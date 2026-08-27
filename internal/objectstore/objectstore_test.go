// Source Gateway <https://sourcegateway.dev>
// Copyright The Source Gateway Authors
// SPDX-License-Identifier: Apache-2.0

package objectstore

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"cloud.google.com/go/storage"
	"github.com/sourcegateway/sourcegateway/api/v1alpha1"
	"github.com/sourcegateway/sourcegateway/internal/config"
)

// TestCredentialRevisionTracksRotationAndOptionalSessionToken verifies credential hashing.
func TestCredentialRevisionTracksRotationAndOptionalSessionToken(t *testing.T) {
	root := t.TempDir()
	credentialDirectory := filepath.Join(root, "object")
	if err := os.Mkdir(credentialDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	writeCredential := func(name, value string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(credentialDirectory, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeCredential("access-key-id", "first")
	writeCredential("secret-access-key", "secret")
	cfg := &config.Config{
		CredentialsDirectory: root,
		Credentials: config.Credentials{ObjectStorageBucket: map[string]config.Credential{
			"object": {Kind: "awsAccessKey", Namespaces: []string{"agents"}, Origin: "https://s3.us-east-1.amazonaws.com"},
		}},
	}
	backend := v1alpha1.ObjectBackend{Credential: "object"}
	first, err := CredentialRevision(cfg, "agents", backend)
	if err != nil {
		t.Fatal(err)
	}
	writeCredential("access-key-id", "rotated")
	second, err := CredentialRevision(cfg, "agents", backend)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("credential rotation did not change the provider revision")
	}
}

// TestResolveOriginDefaultsAWSAndGCS checks provider defaults and exact overrides.
func TestResolveOriginDefaultsAWSAndGCS(t *testing.T) {
	tests := []struct {
		name       string
		backend    v1alpha1.ObjectBackend
		credential config.Credential
		want       string
		valid      bool
	}{
		{name: "AWS dual stack", backend: v1alpha1.ObjectBackend{Provider: "s3", Region: "us-east-1"}, credential: config.Credential{Kind: "awsDefault"}, want: "https://s3.dualstack.us-east-1.amazonaws.com", valid: true},
		{name: "AWS China partition", backend: v1alpha1.ObjectBackend{Provider: "s3", Region: "cn-north-1"}, credential: config.Credential{Kind: "awsDefault"}, want: "https://s3.dualstack.cn-north-1.amazonaws.com.cn", valid: true},
		{name: "custom S3", backend: v1alpha1.ObjectBackend{Provider: "s3", Region: "us-east-1"}, credential: config.Credential{Kind: "awsAccessKey", Origin: "http://127.0.0.1:8333"}, want: "http://127.0.0.1:8333", valid: true},
		{name: "GCS", backend: v1alpha1.ObjectBackend{Provider: "gcs"}, credential: config.Credential{Kind: "gcsDefault", Origin: "https://storage.googleapis.com"}, want: "https://storage.googleapis.com", valid: true},
		{name: "AWS credential with GCS", backend: v1alpha1.ObjectBackend{Provider: "gcs"}, credential: config.Credential{Kind: "awsDefault"}},
		{name: "GCS credential with S3", backend: v1alpha1.ObjectBackend{Provider: "s3", Region: "us-east-1"}, credential: config.Credential{Kind: "gcsDefault", Origin: "https://storage.googleapis.com"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ResolveOrigin(t.Context(), test.backend, test.credential)
			if (err == nil) != test.valid || got != test.want {
				t.Fatalf("ResolveOrigin() = %q, %v; want %q, valid %t", got, err, test.want, test.valid)
			}
		})
	}
}

// TestS3ProviderPreservesEndpointPath checks the base URL passed to the AWS SDK.
func TestS3ProviderPreservesEndpointPath(t *testing.T) {
	paths := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		paths <- request.URL.Path
		response.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	root := t.TempDir()
	directory := filepath.Join(root, "object")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"access-key-id": "access", "secret-access-key": "secret"} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{
		CredentialsDirectory: root,
		Credentials: config.Credentials{ObjectStorageBucket: map[string]config.Credential{
			"object": {Kind: "awsAccessKey", Namespaces: []string{"agents"}, Origin: server.URL},
		}},
	}
	backend := v1alpha1.ObjectBackend{Provider: "s3", Endpoint: server.URL + "/api", Region: "us-east-1", Bucket: "bucket", Credential: "object"}
	provider, err := NewS3(t.Context(), cfg, "agents", backend)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.HeadBucket(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got, want := <-paths, "/api/bucket"; got != want {
		t.Fatalf("request path = %q; want %q", got, want)
	}
}

// TestGCSConditionsRejectNonAtomicOrInvalidCombinations verifies safe preconditions.
func TestGCSConditionsRejectNonAtomicOrInvalidCombinations(t *testing.T) {
	object := (&storage.Client{}).Bucket("bucket").Object("object")
	if _, err := conditionedObject(object, PutOptions{IfMatch: "1", IfNoneMatch: "2"}); !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("conflicting destination conditions returned %v", err)
	}
	if _, err := conditionedObject(object, PutOptions{IfMatch: "not-a-generation"}); !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("invalid destination generation returned %v", err)
	}
	if _, err := conditionedCopySource(object, PutOptions{CopySourceIfNoneMatch: "*"}); !errors.Is(err, ErrPreconditionFailed) {
		t.Fatalf("copy source If-None-Match * returned %v", err)
	}
}

// TestCrossOriginRedirectIsRejected verifies credential-safe redirect handling.
func TestCrossOriginRedirectIsRejected(t *testing.T) {
	original := &http.Request{URL: &url.URL{Scheme: "https", Host: "storage.example"}}
	same := &http.Request{URL: &url.URL{Scheme: "https", Host: "storage.example", Path: "/next"}}
	other := &http.Request{URL: &url.URL{Scheme: "https", Host: "credentials.example"}}
	if err := sameOriginRedirect(same, []*http.Request{original}); err != nil {
		t.Fatalf("same-origin redirect rejected: %v", err)
	}
	if err := sameOriginRedirect(other, []*http.Request{original}); err == nil {
		t.Fatal("cross-origin redirect unexpectedly allowed")
	}
}
