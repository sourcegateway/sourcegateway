// Source Gateway <https://sourcegateway.dev>
// Copyright The Source Gateway Authors
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sourcegateway/sourcegateway/internal/config"
	"github.com/sourcegateway/sourcegateway/internal/routing"
	"k8s.io/apimachinery/pkg/api/resource"
)

// TestIsLoopback checks loopback classification against bound-address representations.
func TestIsLoopback(t *testing.T) {
	tests := []struct {
		address net.Addr
		want    bool
	}{
		{address: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8443}, want: true},
		{address: &net.TCPAddr{IP: net.IPv6loopback, Port: 8443}, want: true},
		{address: &net.TCPAddr{IP: net.IPv4zero, Port: 8443}, want: false},
		{address: &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 8443}, want: false},
	}
	for _, test := range tests {
		if got := isLoopback(test.address); got != test.want {
			t.Errorf("isLoopback(%s) = %t, want %t", test.address, got, test.want)
		}
	}
}

// TestWritableChecksAndCleansDirectory verifies the readiness probe leaves no artifact behind.
func TestWritableChecksAndCleansDirectory(t *testing.T) {
	directory := t.TempDir()
	if err := writable(directory); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("writable left %d temporary files", len(entries))
	}
}

// TestCertificateWatcherRefreshesCachedCertificate verifies that TLS callbacks use a rotating in-memory certificate.
func TestCertificateWatcherRefreshesCachedCertificate(t *testing.T) {
	// Construct the server with an initial certificate and confirm it is cached.
	directory := t.TempDir()
	certificatePath := filepath.Join(directory, "tls.crt")
	keyPath := filepath.Join(directory, "tls.key")
	firstCertificate := writeTestCertificatePair(t, certificatePath, keyPath, 1)
	cfg := &config.Config{
		ListenAddress:      "127.0.0.1:0",
		MetricsAddress:     "127.0.0.1:0",
		StorageDirectory:   filepath.Join(directory, "storage"),
		TLSCertificateFile: certificatePath,
		TLSPrivateKeyFile:  keyPath,
		CacheCapacity:      resource.MustParse("1Mi"),
	}
	server, err := New(cfg, routing.NewRegistry(), nil, slog.New(slog.DiscardHandler), nil)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := server.http.TLSConfig.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(loaded.Certificate[0], firstCertificate) {
		t.Fatal("initial certificate was not cached")
	}

	// Start the watcher and replace the pair as a mounted CSI volume would during rotation.
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- server.certificateWatcher.WithWatchInterval(10 * time.Millisecond).Start(ctx)
	}()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("stop certificate watcher: %v", err)
		}
	}()
	secondCertificate := writeTestCertificatePair(t, certificatePath, keyPath, 2)
	deadline := time.Now().Add(2 * time.Second)
	for {
		loaded, err = server.http.TLSConfig.GetCertificate(nil)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(loaded.Certificate[0], secondCertificate) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("certificate was not refreshed")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// A transiently missing mount must not interrupt handshakes using the last known-good certificate.
	if err := os.Remove(certificatePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	loaded, err = server.http.TLSConfig.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(loaded.Certificate[0], secondCertificate) {
		t.Fatal("cached certificate changed after source files disappeared")
	}
}

// writeTestCertificatePair writes a self-signed ECDSA certificate and key for a rotation test.
func writeTestCertificatePair(t *testing.T, certificatePath, keyPath string, serial int64) []byte {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
	}
	certificate, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	key, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certificatePath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certificate
}
