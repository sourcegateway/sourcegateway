// Source Gateway <https://sourcegateway.dev>
// Copyright The Source Gateway Authors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/sourcegateway/sourcegateway/api/v1alpha1"
	"github.com/sourcegateway/sourcegateway/internal/config"
	"github.com/sourcegateway/sourcegateway/internal/routing"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestBucketName verifies stable, unique, DNS-compatible names.
func TestBucketName(t *testing.T) {
	first := BucketName("team.example", "artifacts.with.a.very.long.resource.name")
	second := BucketName("team.example", "artifacts.with.a.very.long.resource.name")
	if first != second || len(first) > 63 || !regexp.MustCompile(`^[a-z0-9][a-z0-9-]*[a-z0-9]$`).MatchString(first) {
		t.Fatalf("BucketName() = %q", first)
	}
	if first == BucketName("team.example", "other") {
		t.Fatal("different resources have the same derived bucket name")
	}
}

// TestBindBackendRequiresCredentialOrigin checks origin binding and insecure opt-in.
func TestBindBackendRequiresCredentialOrigin(t *testing.T) {
	tests := []struct {
		endpoint string
		trusted  string
		allowed  []string
		valid    bool
	}{
		{trusted: "https://storage.example", valid: true},
		{endpoint: "https://storage.example", trusted: "https://storage.example", valid: true},
		{endpoint: "https://attacker.example", trusted: "https://storage.example", valid: false},
		{trusted: "http://127.0.0.1:8333", valid: false},
		{trusted: "http://127.0.0.1:8333", allowed: []string{"http://127.0.0.1:8333"}, valid: true},
		{trusted: "http://storage.example", allowed: []string{"http://other.example"}, valid: false},
	}
	for _, test := range tests {
		backend := v1alpha1.ObjectBackend{Provider: "s3", Endpoint: test.endpoint, Region: "us-east-1"}
		_, err := bindBackend(t.Context(), backend, config.Credential{Kind: "awsAccessKey", Origin: test.trusted}, test.allowed)
		if (err == nil) != test.valid {
			t.Errorf("bindBackend(%q, %q, %v) returned %v", test.endpoint, test.trusted, test.allowed, err)
		}
	}
}

// TestBindBackendPreservesTrustedEndpointPath checks origin-scoped endpoint binding.
func TestBindBackendPreservesTrustedEndpointPath(t *testing.T) {
	backend := v1alpha1.ObjectBackend{Provider: "s3", Endpoint: "HTTPS://Storage.Example:443/s3/team-a", Region: "us-east-1"}
	bound, err := bindBackend(t.Context(), backend, config.Credential{Kind: "awsAccessKey", Origin: "https://storage.example"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := "https://storage.example/s3/team-a"; bound.Endpoint != want {
		t.Fatalf("bound endpoint = %q; want %q", bound.Endpoint, want)
	}
	backend.Endpoint = "https://attacker.example/s3/team-a"
	if _, err := bindBackend(t.Context(), backend, config.Credential{Kind: "awsAccessKey", Origin: "https://storage.example"}, nil); err == nil {
		t.Fatal("cross-origin endpoint unexpectedly accepted")
	}
	backend.Endpoint = "http://storage.example/s3/team-a"
	credential := config.Credential{Kind: "awsAccessKey", Origin: "http://storage.example"}
	if _, err := bindBackend(t.Context(), backend, credential, []string{"http://storage.example"}); err != nil {
		t.Fatalf("origin-authorized HTTP endpoint rejected: %v", err)
	}
}

// TestBindBackendUsesAWSDefaultOrigin checks regional dual-stack binding.
func TestBindBackendUsesAWSDefaultOrigin(t *testing.T) {
	backend := v1alpha1.ObjectBackend{Provider: "s3", Region: "us-east-1"}
	bound, err := bindBackend(t.Context(), backend, config.Credential{Kind: "awsDefault"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := "https://s3.dualstack.us-east-1.amazonaws.com"; bound.Endpoint != want {
		t.Fatalf("default endpoint = %q; want %q", bound.Endpoint, want)
	}
	backend.Endpoint = "https://s3.us-east-1.amazonaws.com"
	if _, err := bindBackend(t.Context(), backend, config.Credential{Kind: "awsDefault"}, nil); err == nil {
		t.Fatal("non-default endpoint unexpectedly accepted")
	}
}

// TestBindBackendUsesGCSDefaultEndpoint checks the provider's complete base URL.
func TestBindBackendUsesGCSDefaultEndpoint(t *testing.T) {
	backend := v1alpha1.ObjectBackend{Provider: "gcs"}
	credential := config.Credential{Kind: "gcsDefault", Origin: config.DefaultGCSOrigin}
	bound, err := bindBackend(t.Context(), backend, credential, nil)
	if err != nil {
		t.Fatal(err)
	}
	if bound.Endpoint != config.DefaultGCSEndpoint {
		t.Fatalf("default endpoint = %q; want %q", bound.Endpoint, config.DefaultGCSEndpoint)
	}
}

// TestSetConditionPreservesTransitionTime verifies condition update semantics.
func TestSetConditionPreservesTransitionTime(t *testing.T) {
	var conditions []metav1.Condition
	setCondition(&conditions, 1, "Ready", true, "Serving")
	if want := "The source is ready to serve requests."; conditions[0].Message != want {
		t.Fatalf("condition message = %q; want %q", conditions[0].Message, want)
	}
	first := conditions[0].LastTransitionTime
	setCondition(&conditions, 1, "Ready", true, "Serving")
	if !conditions[0].LastTransitionTime.Equal(&first) {
		t.Fatalf("transition time changed from %v to %v", first, conditions[0].LastTransitionTime)
	}
	setCondition(&conditions, 2, "Ready", false, "Unavailable")
	if conditions[0].Status != metav1.ConditionFalse || conditions[0].ObservedGeneration != 2 {
		t.Fatalf("condition = %#v", conditions[0])
	}
}

// TestReconcileBucketExcludesInvalidAndCollidingSources checks fail-closed routing decisions.
func TestReconcileBucketExcludesInvalidAndCollidingSources(t *testing.T) {
	tests := []struct {
		name      string
		collision bool
		rule      v1alpha1.ObjectAuthorizationRule
		reason    string
	}{
		{name: "invalid policy", rule: v1alpha1.ObjectAuthorizationRule{Name: "invalid", Operations: []string{"git.read"}, Allow: "true"}, reason: "PolicyInvalid"},
		{name: "bucket name collision", collision: true, rule: v1alpha1.ObjectAuthorizationRule{Name: "read", Operations: []string{"object.read"}, Allow: "true"}, reason: "BucketNameCollision"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bucket := &v1alpha1.ObjectStorageBucket{
				ObjectMeta: metav1.ObjectMeta{Namespace: "agents", Name: "artifacts", Generation: 2},
				Spec:       v1alpha1.ObjectStorageBucketSpec{Authorization: v1alpha1.ObjectAuthorization{Rules: []v1alpha1.ObjectAuthorizationRule{test.rule}}},
			}
			name := BucketName(bucket.Namespace, bucket.Name)
			current := &routing.Snapshot{Git: map[string]*routing.GitSource{}, Objects: map[string]*routing.ObjectSource{name: {Namespace: bucket.Namespace, Name: bucket.Name}}}
			next := &routing.Snapshot{Git: map[string]*routing.GitSource{}, Objects: map[string]*routing.ObjectSource{}}
			reconciler := &Reconciler{Registry: routing.NewRegistry()}
			status, update, err := reconciler.reconcileBucket(t.Context(), bucket, test.collision, current, next)
			if err != nil || !update {
				t.Fatalf("reconcileBucket() update = %t, error = %v", update, err)
			}
			ready := meta.FindStatusCondition(status.Conditions, "Ready")
			if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != test.reason {
				t.Fatalf("Ready condition = %#v", ready)
			}
			if len(next.Objects) != 0 {
				t.Fatalf("invalid source remained routed: %#v", next.Objects)
			}
		})
	}
}

// TestUnchangedStatusSkipsClientWrites checks both status update paths without a Kubernetes client.
func TestUnchangedStatusSkipsClientWrites(t *testing.T) {
	reconciler := &Reconciler{}
	repository := &v1alpha1.GitRepository{Status: v1alpha1.GitRepositoryStatus{Path: "/git/agents/application.git"}}
	if err := reconciler.updateGitStatus(context.Background(), repository, repository.Status); err != nil {
		t.Fatalf("unchanged Git status returned %v", err)
	}
	bucket := &v1alpha1.ObjectStorageBucket{Status: v1alpha1.ObjectStorageBucketStatus{BucketName: "agents-artifacts-hash"}}
	if err := reconciler.updateBucketStatus(context.Background(), bucket, bucket.Status); err != nil {
		t.Fatalf("unchanged bucket status returned %v", err)
	}
}

// TestMirrorUsableHonorsOriginAndStaleness checks safe fallback after refresh failure.
func TestMirrorUsableHonorsOriginAndStaleness(t *testing.T) {
	source := &routing.GitSource{UpstreamURL: "https://github.com/example/repository.git", RefreshedAt: time.Now().Add(-time.Minute)}
	if !mirrorUsable(source, source.UpstreamURL, 2*time.Minute) {
		t.Fatal("fresh unchanged mirror was not reusable")
	}
	if mirrorUsable(source, "https://github.com/example/other.git", 2*time.Minute) {
		t.Fatal("mirror was reused for another upstream")
	}
	if mirrorUsable(source, source.UpstreamURL, 30*time.Second) {
		t.Fatal("stale mirror was reused")
	}
}

// TestRecoverMirrorRestoresBackup checks recovery at the first rename crash boundary.
func TestRecoverMirrorRestoresBackup(t *testing.T) {
	root := t.TempDir()
	destination := filepath.Join(root, "repository.git")
	backup := destination + ".old"
	command := exec.Command("git", "init", "--bare", backup) // #nosec G204 -- backup is created within this test's temporary directory.
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("initialize mirror: %v: %s", err, output)
	}
	upstream := "https://github.com/example/repository.git"
	want := mirrorMetadata{UpstreamURL: upstream, RefreshedAt: time.Now().Add(-time.Minute).Round(0)}
	if err := writeMirrorMetadata(backup, want); err != nil {
		t.Fatal(err)
	}
	got, err := (&Reconciler{}).recoverMirror(destination, upstream, &routing.GitState{})
	if err != nil || !got.RefreshedAt.Equal(want.RefreshedAt) {
		t.Fatalf("recoverMirror() = %#v, %v", got, err)
	}
	if _, err := validateMirror(destination, upstream); err != nil {
		t.Fatalf("restored mirror is invalid: %v", err)
	}
}

// TestCleanupGitStorageRetainsOnlyRoutedMirrors checks restart cleanup without finalizers.
func TestCleanupGitStorageRetainsOnlyRoutedMirrors(t *testing.T) {
	root := t.TempDir()
	reconciler := &Reconciler{Config: &config.Config{StorageDirectory: root}}
	wanted := filepath.Join(root, "git", "agents", "wanted.git")
	orphan := filepath.Join(root, "git", "agents", "orphan.git")
	for _, path := range []string{wanted, orphan, filepath.Join(root, "git", "agents", ".mirror-interrupted")} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	next := &routing.Snapshot{Git: map[string]*routing.GitSource{"/git/agents/wanted.git": {MirrorPath: wanted}}, Objects: map[string]*routing.ObjectSource{}}
	if err := reconciler.cleanupGitStorage(next); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wanted); err != nil {
		t.Fatalf("routed mirror removed: %v", err)
	}
	for _, path := range []string{orphan, filepath.Join(root, "git", "agents", ".mirror-interrupted")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("orphan %q remains: %v", path, err)
		}
	}
}
