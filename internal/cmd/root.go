// Source Gateway <https://sourcegateway.dev>
// Copyright The Source Gateway Authors
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sourcegateway/sourcegateway/api/v1alpha1"
	"github.com/sourcegateway/sourcegateway/internal/auth"
	"github.com/sourcegateway/sourcegateway/internal/buildvars"
	"github.com/sourcegateway/sourcegateway/internal/config"
	"github.com/sourcegateway/sourcegateway/internal/controller"
	"github.com/sourcegateway/sourcegateway/internal/routing"
	"github.com/sourcegateway/sourcegateway/internal/server"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

// +kubebuilder:rbac:urls=/readyz,verbs=get

// Run dispatches helper modes or starts the Kubernetes controllers and Git/object storage proxy.
func Run(ctx context.Context, arguments []string) error {
	if filepath.Base(os.Args[0]) == "proc-receive" || len(arguments) > 0 && arguments[0] == "git-proc-receive" {
		if err := server.RunProcReceiveHook(ctx, os.Stdin, os.Stdout); err != nil {
			return fmt.Errorf("run Git proc-receive hook: %w", err)
		}
		return nil
	}
	flags := flag.NewFlagSet("sourcegateway", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "/etc/sourcegateway/config.jsonc", "path to runtime JSONC configuration")
	var allowedInsecureUpstreams []string
	flags.Func("allow-insecure-upstream", "allow one exact plaintext HTTP upstream origin (repeatable, development only)", func(value string) error {
		origin, err := insecureOrigin(value)
		if err != nil {
			return err
		}
		allowedInsecureUpstreams = append(allowedInsecureUpstreams, origin)
		return nil
	})
	version := flags.Bool("version", false, "print build version")
	if err := flags.Parse(arguments); err != nil {
		return fmt.Errorf("parse arguments: %w", err)
	}
	if *version {
		if _, err := fmt.Fprintf(os.Stdout, "sourcegateway %s (%s, %s)\n", buildvars.Version(), buildvars.CommitHash(), buildvars.BuildDate()); err != nil {
			return fmt.Errorf("write version: %w", err)
		}
		return nil
	}
	// Wait briefly for the mounted configuration file to appear.
	deadline := time.Now().Add(30 * time.Second)
	var cfg *config.Config
	var err error
	for {
		cfg, err = config.Load(*configPath)
		if err == nil || !errors.Is(err, os.ErrNotExist) || time.Now().After(deadline) {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	return runServer(ctx, cfg, allowedInsecureUpstreams)
}

// insecureOrigin validates and normalizes one development-only HTTP origin.
func insecureOrigin(value string) (string, error) {
	origin, err := config.NormalizeOrigin(value)
	if err != nil || !strings.HasPrefix(origin, "http://") {
		return "", errors.New("insecure upstream must be an HTTP origin without a path")
	}
	return origin, nil
}

// runServer constructs and runs the Kubernetes control plane and HTTP data plane.
func runServer(parent context.Context, cfg *config.Config, allowedInsecureUpstreams []string) error {
	// Build the Kubernetes manager and register Source Gateway controllers.
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if len(allowedInsecureUpstreams) != 0 {
		logger.Warn("plaintext HTTP upstreams are enabled", "origins", allowedInsecureUpstreams)
	}
	controllerruntime.SetLogger(zap.New())
	restConfig, err := controllerruntime.GetConfig()
	if err != nil {
		return fmt.Errorf("load Kubernetes configuration: %w", err)
	}
	runtimeScheme := runtime.NewScheme()
	if err := scheme.AddToScheme(runtimeScheme); err != nil {
		return fmt.Errorf("register Kubernetes API scheme: %w", err)
	}
	if err := v1alpha1.AddToScheme(runtimeScheme); err != nil {
		return fmt.Errorf("register Source Gateway API scheme: %w", err)
	}
	manager, err := controllerruntime.NewManager(restConfig, controllerruntime.Options{
		Scheme: runtimeScheme, Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0",
		LeaderElection: false,
	})
	if err != nil {
		return fmt.Errorf("create Kubernetes manager: %w", err)
	}
	registry := routing.NewRegistry()
	reconciler := &controller.Reconciler{Client: manager.GetClient(), Config: cfg, Registry: registry, AllowedInsecureUpstreams: allowedInsecureUpstreams}
	if err := reconciler.SetupWithManager(manager); err != nil {
		return fmt.Errorf("setup controllers: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("create Kubernetes client: %w", err)
	}
	authenticator := auth.New(auth.NewKubernetesReviewer(clientset), cfg.TokenAudience)
	apiReady := func(ctx context.Context) error {
		return clientset.Discovery().RESTClient().Get().AbsPath("/readyz").Do(ctx).Error()
	}
	proxyServer, err := server.New(cfg, registry, authenticator, logger, apiReady)
	if err != nil {
		return fmt.Errorf("create Git and object storage proxy: %w", err)
	}
	reconciler.RetainObjectBuckets = proxyServer.RetainObjectBuckets
	// Run the Kubernetes controllers and Git/object storage proxy until shutdown or either fails.
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errorsChannel := make(chan error, 2)
	go func() {
		err := manager.Start(ctx)
		if err != nil {
			err = fmt.Errorf("run Kubernetes manager: %w", err)
		}
		errorsChannel <- err
	}()
	go func() {
		err := proxyServer.Start(ctx)
		if err != nil {
			err = fmt.Errorf("run Git and object storage proxy: %w", err)
		}
		errorsChannel <- err
	}()
	select {
	case <-ctx.Done():
		return nil
	case err := <-errorsChannel:
		cancel()
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
}
