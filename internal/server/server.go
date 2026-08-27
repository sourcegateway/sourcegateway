// Source Gateway <https://sourcegateway.dev>
// Copyright The Source Gateway Authors
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/sourcegateway/sourcegateway/internal/auth"
	"github.com/sourcegateway/sourcegateway/internal/config"
	"github.com/sourcegateway/sourcegateway/internal/objectcache"
	"github.com/sourcegateway/sourcegateway/internal/routing"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
)

// Server hosts the authenticated application and operational HTTP endpoints.
type Server struct {
	registry           *routing.Registry
	authenticator      *auth.Authenticator
	logger             *slog.Logger
	apiReady           func(context.Context) error
	objectCache        *objectcache.Cache
	certificateWatcher *certwatcher.CertWatcher
	serving            atomic.Bool
	active             chan struct{}
	waiting            atomic.Int32
	http               *http.Server
	health             *http.Server
}

// New prepares a Server and its persistent object cache.
func New(cfg *config.Config, registry *routing.Registry, authenticator *auth.Authenticator, logger *slog.Logger, apiReady func(context.Context) error) (*Server, error) {
	// Recover reconstructable local state before accepting requests.
	if err := os.MkdirAll(cfg.StorageDirectory, 0o700); err != nil {
		return nil, err
	}
	tmpDir := cfg.StorageDirectory + "/tmp"
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		_ = os.RemoveAll(tmpDir + "/" + entry.Name())
	}
	if err := writable(tmpDir); err != nil {
		return nil, err
	}
	objectCache, err := objectcache.Open(cfg.StorageDirectory+"/object-cache", cfg.CacheCapacity.Value())
	if err != nil {
		return nil, err
	}
	server := &Server{registry: registry, authenticator: authenticator, logger: logger, apiReady: apiReady, objectCache: objectCache, active: make(chan struct{}, 64)}
	if cfg.TLSCertificateFile != "" || cfg.TLSPrivateKeyFile != "" {
		if cfg.TLSCertificateFile == "" || cfg.TLSPrivateKeyFile == "" {
			return nil, errors.New("both TLS certificate and key are required")
		}
		certificateWatcher, err := certwatcher.New(cfg.TLSCertificateFile, cfg.TLSPrivateKeyFile)
		if err != nil {
			return nil, fmt.Errorf("load TLS certificate: %w", err)
		}
		server.certificateWatcher = certificateWatcher
	}
	// Keep application authentication separate from unauthenticated operations endpoints.
	git := &gitHandler{registry: registry, logger: logger, tmpDir: tmpDir}
	s3 := &s3Handler{registry: registry, logger: logger, tmpDir: tmpDir, cache: objectCache, maxObjectSize: cfg.MaxObjectSizeBytes, multipart: newMultipartStore(), locks: newObjectLocks()}
	application := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if strings.HasPrefix(request.URL.Path, "/git/") {
			git.ServeHTTP(response, request)
		} else {
			s3.ServeHTTP(response, request)
		}
	})
	server.http = &http.Server{
		Addr: cfg.ListenAddress, Handler: server.limit(authenticator.Middleware(application)),
		ReadTimeout: 10 * time.Minute, ReadHeaderTimeout: 10 * time.Second, WriteTimeout: 10 * time.Minute, IdleTimeout: 30 * time.Second,
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	}
	if server.certificateWatcher != nil {
		server.http.TLSConfig.GetCertificate = server.certificateWatcher.GetCertificate
	}
	healthMux := http.NewServeMux()
	healthMux.HandleFunc("GET /healthz", func(response http.ResponseWriter, _ *http.Request) { response.WriteHeader(http.StatusOK) })
	healthMux.HandleFunc("GET /readyz", func(response http.ResponseWriter, request *http.Request) {
		readyContext, cancel := context.WithTimeout(request.Context(), 2*time.Second)
		defer cancel()
		if !server.serving.Load() || !server.registry.Published() || writable(tmpDir) != nil || server.apiReady == nil || server.apiReady(readyContext) != nil {
			http.Error(response, "not ready", http.StatusServiceUnavailable)
			return
		}
		response.WriteHeader(http.StatusOK)
	})
	healthMux.HandleFunc("GET /metrics", server.metrics(objectCache))
	server.health = &http.Server{Addr: cfg.MetricsAddress, Handler: healthMux, ReadHeaderTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second}
	return server, nil
}

// setTransferDeadline bounds both halves of an HTTP transfer.
func setTransferDeadline(response http.ResponseWriter, request *http.Request, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	controller := http.NewResponseController(response)
	_ = controller.SetReadDeadline(deadline)
	_ = controller.SetWriteDeadline(deadline)
}

// RetainObjectBuckets evicts cached objects whose source is no longer configured.
func (s *Server) RetainObjectBuckets(buckets map[string]bool) {
	s.objectCache.RetainBuckets(buckets)
}

// Start serves requests until cancellation or a listener failure.
func (s *Server) Start(ctx context.Context) error {
	// Bind both listeners before starting either server.
	healthListener, err := net.Listen("tcp", s.health.Addr)
	if err != nil {
		return err
	}
	applicationListener, err := net.Listen("tcp", s.http.Addr)
	if err != nil {
		_ = healthListener.Close()
		return err
	}
	applicationIsLoopback := isLoopback(applicationListener.Addr())
	healthIsLoopback := isLoopback(healthListener.Addr())
	if s.certificateWatcher == nil && (!applicationIsLoopback || !healthIsLoopback) {
		_ = applicationListener.Close()
		_ = healthListener.Close()
		return errors.New("TLS certificate and key are required for non-loopback listeners")
	}
	if s.certificateWatcher != nil {
		tlsConfig := s.http.TLSConfig.Clone()
		applicationListener = tls.NewListener(applicationListener, tlsConfig)
		if !healthIsLoopback {
			healthListener = tls.NewListener(healthListener, tlsConfig)
		}
	}
	// Serve both planes as one lifecycle and drain them together.
	errorChannel := make(chan error, 3)
	go func() { errorChannel <- s.http.Serve(applicationListener) }()
	go func() { errorChannel <- s.health.Serve(healthListener) }()
	if s.certificateWatcher != nil {
		go func() {
			err := s.certificateWatcher.Start(ctx)
			if err != nil {
				err = fmt.Errorf("watch TLS certificate: %w", err)
			}
			errorChannel <- err
		}()
	}
	s.serving.Store(true)
	var serveError error
	select {
	case <-ctx.Done():
	case serveError = <-errorChannel:
	}
	s.serving.Store(false)
	shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = s.http.Shutdown(shutdown)
	_ = s.health.Shutdown(shutdown)
	if serveError == nil || errors.Is(serveError, http.ErrServerClosed) {
		return nil
	}
	return serveError
}

// limit bounds active and queued application requests.
func (s *Server) limit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		select {
		case s.active <- struct{}{}:
			defer func() { <-s.active }()
			next.ServeHTTP(response, request)
			return
		default:
		}
		waiting := s.waiting.Add(1)
		if waiting > 128 {
			s.waiting.Add(-1)
			response.Header().Set("Retry-After", "1")
			http.Error(response, "busy", http.StatusServiceUnavailable)
			return
		}
		defer s.waiting.Add(-1)
		select {
		case s.active <- struct{}{}:
			defer func() { <-s.active }()
			next.ServeHTTP(response, request)
		case <-request.Context().Done():
		}
	})
}

// isLoopback reports whether address is a TCP loopback address.
func isLoopback(address net.Addr) bool {
	tcpAddress, ok := address.(*net.TCPAddr)
	return ok && tcpAddress.AddrPort().Addr().IsLoopback()
}

// writable verifies that a directory permits creating and removing a file.
func writable(directory string) error {
	file, err := os.CreateTemp(directory, ".ready-*")
	if err != nil {
		return err
	}
	name := file.Name()
	if err := file.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	return os.Remove(name)
}

// metrics returns a handler for current mirror and cache gauges.
func (s *Server) metrics(objectCache *objectcache.Cache) http.HandlerFunc {
	return func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		snapshot := s.registry.Load()
		routes := make([]string, 0, len(snapshot.Git))
		for route := range snapshot.Git {
			routes = append(routes, route)
		}
		sort.Strings(routes)
		_, _ = fmt.Fprintln(response, "# HELP sourcegateway_git_mirror_refresh_timestamp_seconds Unix timestamp of the last complete mirror refresh.")
		_, _ = fmt.Fprintln(response, "# TYPE sourcegateway_git_mirror_refresh_timestamp_seconds gauge")
		for _, route := range routes {
			source := snapshot.Git[route]
			_, _ = fmt.Fprintf(response, "sourcegateway_git_mirror_refresh_timestamp_seconds{namespace=%s,name=%s} %d\n", strconv.Quote(source.Namespace), strconv.Quote(source.Name), source.RefreshedAt.Unix())
		}
		_, _ = fmt.Fprintln(response, "# HELP sourcegateway_object_cache_oldest_age_seconds Age of the oldest complete cached object.")
		_, _ = fmt.Fprintln(response, "# TYPE sourcegateway_object_cache_oldest_age_seconds gauge")
		_, _ = fmt.Fprintln(response, "# HELP sourcegateway_object_cache_entries Complete cached objects.")
		_, _ = fmt.Fprintln(response, "# TYPE sourcegateway_object_cache_entries gauge")
		_, _ = fmt.Fprintln(response, "# HELP sourcegateway_object_cache_bytes Bytes occupied by complete cached objects.")
		_, _ = fmt.Fprintln(response, "# TYPE sourcegateway_object_cache_bytes gauge")
		now := time.Now()
		cacheStats := make(map[string]objectcache.BucketStats)
		for _, stats := range objectCache.Stats() {
			cacheStats[stats.Bucket] = stats
		}
		buckets := make([]string, 0, len(snapshot.Objects))
		for bucket := range snapshot.Objects {
			buckets = append(buckets, bucket)
		}
		sort.Strings(buckets)
		for _, bucket := range buckets {
			stats := cacheStats[bucket]
			age := 0.0
			if !stats.OldestStored.IsZero() {
				age = now.Sub(stats.OldestStored).Seconds()
			}
			label := strconv.Quote(bucket)
			_, _ = fmt.Fprintf(response, "sourcegateway_object_cache_oldest_age_seconds{bucket=%s} %.3f\n", label, age)
			_, _ = fmt.Fprintf(response, "sourcegateway_object_cache_entries{bucket=%s} %d\n", label, stats.Entries)
			_, _ = fmt.Fprintf(response, "sourcegateway_object_cache_bytes{bucket=%s} %d\n", label, stats.Bytes)
		}
	}
}
