// Source Gateway <https://sourcegateway.dev>
// Copyright The Source Gateway Authors
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sourcegateway/sourcegateway/api/v1alpha1"
	"github.com/sourcegateway/sourcegateway/internal/config"
	"github.com/sourcegateway/sourcegateway/internal/credential"
	"github.com/sourcegateway/sourcegateway/internal/objectstore"
	"github.com/sourcegateway/sourcegateway/internal/policy"
	"github.com/sourcegateway/sourcegateway/internal/routing"
	"github.com/sourcegateway/sourcegateway/internal/server"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

const conditionMessage = "See Source Gateway logs for details."

const mirrorMetadataFile = ".sourcegateway-mirror.json"

// mirrorMetadata records the authoritative identity and age of an installed mirror.
type mirrorMetadata struct {
	UpstreamURL string    `json:"upstreamURL"`
	RefreshedAt time.Time `json:"refreshedAt"`
}

// Reconciler builds routing configuration from Source Gateway resources.
type Reconciler struct {
	client.Client
	Config                   *config.Config
	Registry                 *routing.Registry
	RetainObjectBuckets      func(map[string]bool)
	AllowedInsecureUpstreams []string
	mu                       sync.Mutex
}

// gitStatusUpdate defers a Git status write until after publication.
type gitStatusUpdate struct {
	repository *v1alpha1.GitRepository
	status     v1alpha1.GitRepositoryStatus
}

// bucketStatusUpdate defers a bucket status write until after publication.
type bucketStatusUpdate struct {
	bucket *v1alpha1.ObjectStorageBucket
	status v1alpha1.ObjectStorageBucketStatus
}

// +kubebuilder:rbac:groups=sourcegateway.dev,resources=gitrepositories;objectstoragebuckets,verbs=list;watch
// +kubebuilder:rbac:groups=sourcegateway.dev,resources=gitrepositories/status;objectstoragebuckets/status,verbs=update

// SetupWithManager registers resource controllers and refresh handling.
func (r *Reconciler) SetupWithManager(controllerManager controllerruntime.Manager) error {
	if err := controllerruntime.NewControllerManagedBy(controllerManager).For(&v1alpha1.GitRepository{}).Complete(r); err != nil {
		return err
	}
	if err := controllerruntime.NewControllerManagedBy(controllerManager).For(&v1alpha1.ObjectStorageBucket{}).Complete(r); err != nil {
		return err
	}
	return controllerManager.Add(manager.RunnableFunc(func(ctx context.Context) error {
		if !controllerManager.GetCache().WaitForCacheSync(ctx) {
			return nil
		}
		r.mu.Lock()
		err := r.rebuild(ctx)
		r.mu.Unlock()
		if err != nil {
			return err
		}
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-r.Registry.GitRefreshRequests():
				r.mu.Lock()
				err := r.rebuild(ctx)
				r.mu.Unlock()
				if err != nil {
					controllerruntime.LoggerFrom(ctx).Error(err, "requested Git mirror refresh failed")
				}
			}
		}
	}))
}

// Reconcile rebuilds the complete routing snapshot.
func (r *Reconciler) Reconcile(ctx context.Context, _ controllerruntime.Request) (controllerruntime.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return controllerruntime.Result{RequeueAfter: 30 * time.Second}, r.rebuild(ctx)
}

// rebuild reconciles all resources and atomically publishes their runtime view.
func (r *Reconciler) rebuild(ctx context.Context) error {
	var repositories v1alpha1.GitRepositoryList
	if err := r.List(ctx, &repositories); err != nil {
		return err
	}
	var buckets v1alpha1.ObjectStorageBucketList
	if err := r.List(ctx, &buckets); err != nil {
		return err
	}
	current := r.Registry.Load()
	next := &routing.Snapshot{Git: make(map[string]*routing.GitSource), Objects: make(map[string]*routing.ObjectSource)}
	var gitStatuses []gitStatusUpdate
	var bucketStatuses []bucketStatusUpdate

	// Detect derived bucket-name collisions before reconciling either owner.
	collisions := make(map[string]int)
	for i := range buckets.Items {
		if buckets.Items[i].DeletionTimestamp.IsZero() {
			collisions[BucketName(buckets.Items[i].Namespace, buckets.Items[i].Name)]++
		}
	}
	// Build the next snapshot while retaining status updates for later.
	for i := range repositories.Items {
		repository := &repositories.Items[i]
		status, updateStatus, err := r.reconcileGit(ctx, repository, current, next)
		if err != nil && !apierrors.IsConflict(err) {
			controllerruntime.LoggerFrom(ctx).Error(err, "GitRepository reconciliation failed", "namespace", repository.Namespace, "name", repository.Name)
		} else if updateStatus {
			gitStatuses = append(gitStatuses, gitStatusUpdate{repository: repository, status: status})
		}
	}
	for i := range buckets.Items {
		bucket := &buckets.Items[i]
		collision := collisions[BucketName(bucket.Namespace, bucket.Name)] > 1
		status, updateStatus, err := r.reconcileBucket(ctx, bucket, collision, current, next)
		if err != nil && !apierrors.IsConflict(err) {
			controllerruntime.LoggerFrom(ctx).Error(err, "ObjectStorageBucket reconciliation failed", "namespace", bucket.Namespace, "name", bucket.Name)
		} else if updateStatus {
			bucketStatuses = append(bucketStatuses, bucketStatusUpdate{bucket: bucket, status: status})
		}
	}
	if err := r.retireSources(ctx, current, next); err != nil {
		return err
	}
	// Publish serving state before advertising resource status.
	r.Registry.Publish(next)
	for _, update := range gitStatuses {
		if err := r.updateGitStatus(ctx, update.repository, update.status); err != nil && !apierrors.IsConflict(err) {
			controllerruntime.LoggerFrom(ctx).Error(err, "GitRepository status update failed", "namespace", update.repository.Namespace, "name", update.repository.Name)
		}
	}
	for _, update := range bucketStatuses {
		if err := r.updateBucketStatus(ctx, update.bucket, update.status); err != nil && !apierrors.IsConflict(err) {
			controllerruntime.LoggerFrom(ctx).Error(err, "ObjectStorageBucket status update failed", "namespace", update.bucket.Namespace, "name", update.bucket.Name)
		}
	}
	return nil
}

// reconcileGit adds one repository to next and computes its status.
func (r *Reconciler) reconcileGit(ctx context.Context, repository *v1alpha1.GitRepository, current, next *routing.Snapshot) (v1alpha1.GitRepositoryStatus, bool, error) {
	sourceKey := "git/" + repository.Namespace + "/" + repository.Name
	if !repository.DeletionTimestamp.IsZero() {
		return v1alpha1.GitRepositoryStatus{}, false, nil
	}
	// Validate policy and credentials before touching upstream state.
	status := v1alpha1.GitRepositoryStatus{SourceStatus: v1alpha1.SourceStatus{ObservedGeneration: repository.Generation, Conditions: append([]metav1.Condition(nil), repository.Status.Conditions...)}, Path: fmt.Sprintf("/git/%s/%s.git", repository.Namespace, repository.Name)}
	compiled, err := policy.CompileGit(repository.Spec.Authorization.Rules)
	if err != nil {
		setCondition(&status.Conditions, repository.Generation, "PolicyReady", false, "PolicyInvalid")
		setCondition(&status.Conditions, repository.Generation, "CredentialsReady", false, "Blocked")
		setCondition(&status.Conditions, repository.Generation, "UpstreamReady", false, "Blocked")
		setCondition(&status.Conditions, repository.Generation, "Ready", false, "PolicyInvalid")
		return status, true, nil
	}
	setCondition(&status.Conditions, repository.Generation, "PolicyReady", true, "Compiled")
	status.PolicyRevision = compiled.Revision()
	authHeader, err := credential.GitAuthorization(ctx, r.Config, repository.Spec.Upstream.Credential, repository.Namespace, repository.Spec.Upstream.URL)
	if err != nil {
		setCondition(&status.Conditions, repository.Generation, "CredentialsReady", false, "CredentialsInvalid")
		setCondition(&status.Conditions, repository.Generation, "UpstreamReady", false, "Blocked")
		setCondition(&status.Conditions, repository.Generation, "Ready", false, "CredentialsInvalid")
		return status, true, nil
	}
	setCondition(&status.Conditions, repository.Generation, "CredentialsReady", true, "Loaded")
	// Reuse a fresh mirror when possible; otherwise replace it atomically.
	state := r.Registry.GitState(sourceKey)
	refreshRequired := false
	destination := r.gitPath(repository.Namespace, repository.Name)
	if _, statErr := os.Stat(filepath.Join(destination, routing.GitRefreshMarker)); statErr == nil {
		refreshRequired = true
	}
	old := current.Git[status.Path]
	if old == nil && !refreshRequired {
		if recovered, recoverErr := r.recoverMirror(destination, repository.Spec.Upstream.URL, state); recoverErr == nil {
			old = r.gitSource(repository, status.Path, authHeader, compiled, state, recovered.RefreshedAt)
		}
	}
	if repository.Spec.Mirror.RefreshInterval.Duration <= 0 || repository.Spec.Mirror.MaxStaleness.Duration <= 0 {
		err = errors.New("mirror durations must be positive")
	} else {
		if !refreshRequired && old != nil && old.UpstreamURL == repository.Spec.Upstream.URL && time.Since(old.RefreshedAt) < repository.Spec.Mirror.RefreshInterval.Duration {
			next.Git[status.Path] = r.gitSource(repository, status.Path, authHeader, compiled, state, old.RefreshedAt)
		} else {
			var refreshedAt time.Time
			refreshedAt, err = r.refreshMirror(ctx, repository, authHeader, state)
			if err == nil {
				next.Git[status.Path] = r.gitSource(repository, status.Path, authHeader, compiled, state, refreshedAt)
			} else if !refreshRequired && mirrorUsable(old, repository.Spec.Upstream.URL, repository.Spec.Mirror.MaxStaleness.Duration) {
				next.Git[status.Path] = r.gitSource(repository, status.Path, authHeader, compiled, state, old.RefreshedAt)
			}
		}
	}
	if err != nil {
		setCondition(&status.Conditions, repository.Generation, "UpstreamReady", false, "UpstreamUnavailable")
	} else {
		setCondition(&status.Conditions, repository.Generation, "UpstreamReady", true, "MirrorCurrent")
	}
	if next.Git[status.Path] == nil {
		setCondition(&status.Conditions, repository.Generation, "Ready", false, "UpstreamUnavailable")
	} else if err != nil {
		setCondition(&status.Conditions, repository.Generation, "Ready", true, "ServingStale")
	} else {
		setCondition(&status.Conditions, repository.Generation, "Ready", true, "Serving")
	}
	return status, true, nil
}

// refreshMirror clones and atomically installs a repository mirror.
func (r *Reconciler) refreshMirror(ctx context.Context, repository *v1alpha1.GitRepository, authHeader string, state *routing.GitState) (time.Time, error) {
	destination := r.gitPath(repository.Namespace, repository.Name)
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return time.Time{}, err
	}
	temporary, err := os.MkdirTemp(filepath.Dir(destination), ".mirror-*")
	if err != nil {
		return time.Time{}, err
	}
	_ = os.Remove(temporary)
	defer func() { _ = os.RemoveAll(temporary) }()
	state.Update.Lock()
	defer state.Update.Unlock()
	// Clone with ambient credentials and unsafe redirects disabled.
	arguments := []string{"-c", "http.followRedirects=false", "-c", "http.sslVerify=true", "-c", "credential.helper="}
	if authHeader != "" {
		arguments = append(arguments, "-c", "http.extraHeader=Authorization: "+authHeader)
	}
	arguments = append(arguments, "clone", "--mirror", "--", repository.Spec.Upstream.URL, temporary)
	commandContext, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	command := exec.CommandContext(commandContext, "git", arguments...) // #nosec G204 -- arguments are validated controller fields and are not interpreted by a shell.
	server.ConfigureGitCommand(command)
	if output, err := command.CombinedOutput(); err != nil {
		_ = output
		return time.Time{}, errors.New("upstream mirror refresh failed")
	}
	if err := server.InstallProcReceiveHook(temporary); err != nil {
		return time.Time{}, err
	}
	refreshedAt := time.Now()
	if err := writeMirrorMetadata(temporary, mirrorMetadata{UpstreamURL: repository.Spec.Upstream.URL, RefreshedAt: refreshedAt}); err != nil {
		return time.Time{}, err
	}
	// Gate readers only for the short final directory swap.
	backup := destination + ".old"
	state.Gate.Lock()
	defer state.Gate.Unlock()
	if err := os.RemoveAll(backup); err != nil {
		return time.Time{}, err
	}
	if _, err := os.Stat(destination); err == nil {
		if err := os.Rename(destination, backup); err != nil {
			return time.Time{}, err
		}
	}
	if err := os.Rename(temporary, destination); err != nil {
		_ = os.Rename(backup, destination)
		return time.Time{}, err
	}
	if _, err := validateMirror(destination, repository.Spec.Upstream.URL); err != nil {
		_ = os.RemoveAll(destination)
		_ = os.Rename(backup, destination)
		return time.Time{}, err
	}
	if err := syncDirectory(filepath.Dir(destination)); err != nil {
		return time.Time{}, err
	}
	if err := os.RemoveAll(backup); err != nil {
		return time.Time{}, err
	}
	return refreshedAt, syncDirectory(filepath.Dir(destination))
}

// gitSource builds one immutable route from current policy and credential state.
func (r *Reconciler) gitSource(repository *v1alpha1.GitRepository, path, authHeader string, compiled *policy.Policy, state *routing.GitState, refreshedAt time.Time) *routing.GitSource {
	sourceKey := "git/" + repository.Namespace + "/" + repository.Name
	return &routing.GitSource{
		Namespace: repository.Namespace, Name: repository.Name, Path: path, MirrorPath: r.gitPath(repository.Namespace, repository.Name),
		UpstreamURL: repository.Spec.Upstream.URL, AuthHeader: authHeader, Policy: compiled, Rules: repository.Spec.Authorization.Rules,
		RefreshedAt: refreshedAt, MaxStaleness: repository.Spec.Mirror.MaxStaleness.Duration,
		Requests: r.Registry.RequestGate(sourceKey), State: state,
	}
}

// mirrorUsable reports whether source still represents the same upstream and
// is fresh enough to serve after a refresh failure.
func mirrorUsable(source *routing.GitSource, upstream string, maximum time.Duration) bool {
	return source != nil && source.UpstreamURL == upstream && time.Since(source.RefreshedAt) <= maximum
}

// recoverMirror restores the best complete local candidate left by a restart.
func (r *Reconciler) recoverMirror(destination, upstream string, state *routing.GitState) (mirrorMetadata, error) {
	candidates := []string{destination, destination + ".old"}
	temporary, _ := filepath.Glob(filepath.Join(filepath.Dir(destination), ".mirror-*"))
	candidates = append(candidates, temporary...)
	selected := ""
	var selectedMetadata mirrorMetadata
	for _, candidate := range candidates {
		metadata, err := validateMirror(candidate, upstream)
		if err == nil && (selected == "" || candidate == destination || selected != destination && metadata.RefreshedAt.After(selectedMetadata.RefreshedAt)) {
			selected, selectedMetadata = candidate, metadata
		}
	}
	if selected == "" {
		return mirrorMetadata{}, os.ErrNotExist
	}
	state.Gate.Lock()
	defer state.Gate.Unlock()
	if selected != destination {
		if err := os.RemoveAll(destination); err != nil {
			return mirrorMetadata{}, err
		}
		if err := os.Rename(selected, destination); err != nil {
			return mirrorMetadata{}, err
		}
	}
	if err := server.InstallProcReceiveHook(destination); err != nil {
		return mirrorMetadata{}, err
	}
	for _, candidate := range candidates {
		if candidate != destination && candidate != selected {
			_ = os.RemoveAll(candidate)
		}
	}
	return selectedMetadata, syncDirectory(filepath.Dir(destination))
}

// validateMirror verifies local metadata and Git's bare-repository structure.
func validateMirror(path, upstream string) (mirrorMetadata, error) {
	encoded, err := os.ReadFile(filepath.Join(path, mirrorMetadataFile)) // #nosec G304 -- path is controller-owned local state.
	var metadata mirrorMetadata
	if err != nil || json.Unmarshal(encoded, &metadata) != nil || metadata.UpstreamURL != upstream || metadata.RefreshedAt.IsZero() {
		return mirrorMetadata{}, errors.New("invalid mirror metadata")
	}
	command := exec.Command("git", "-C", path, "rev-parse", "--is-bare-repository") // #nosec G204 -- path is controller-owned local state.
	server.ConfigureGitCommand(command)
	output, err := command.Output()
	if err != nil || strings.TrimSpace(string(output)) != "true" {
		return mirrorMetadata{}, errors.New("invalid Git mirror")
	}
	return metadata, nil
}

// writeMirrorMetadata durably publishes the identity and age of a complete mirror.
func writeMirrorMetadata(path string, metadata mirrorMetadata) error {
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	temporary := filepath.Join(path, mirrorMetadataFile+".tmp")
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600) // #nosec G304 -- path is controller-owned local state.
	if err != nil {
		return err
	}
	_, writeErr := file.Write(encoded)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	if err := os.Rename(temporary, filepath.Join(path, mirrorMetadataFile)); err != nil {
		return err
	}
	return syncDirectory(path)
}

// syncDirectory commits directory entry changes before a mirror is acknowledged locally.
func syncDirectory(path string) error {
	directory, err := os.Open(path) // #nosec G304 -- path is controller-owned local state.
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	return errors.Join(syncErr, closeErr)
}

// reconcileBucket adds one object bucket to next and computes its status.
func (r *Reconciler) reconcileBucket(ctx context.Context, bucket *v1alpha1.ObjectStorageBucket, collision bool, current, next *routing.Snapshot) (v1alpha1.ObjectStorageBucketStatus, bool, error) {
	sourceKey := "object/" + bucket.Namespace + "/" + bucket.Name
	if !bucket.DeletionTimestamp.IsZero() {
		return v1alpha1.ObjectStorageBucketStatus{}, false, nil
	}
	// Validate local configuration before constructing an upstream client.
	name := BucketName(bucket.Namespace, bucket.Name)
	status := v1alpha1.ObjectStorageBucketStatus{SourceStatus: v1alpha1.SourceStatus{ObservedGeneration: bucket.Generation, Conditions: append([]metav1.Condition(nil), bucket.Status.Conditions...)}, BucketName: name}
	compiled, err := policy.CompileObject(bucket.Spec.Authorization.Rules)
	if err != nil {
		setCondition(&status.Conditions, bucket.Generation, "PolicyReady", false, "PolicyInvalid")
	} else {
		setCondition(&status.Conditions, bucket.Generation, "PolicyReady", true, "Compiled")
	}
	if err != nil || collision {
		setCondition(&status.Conditions, bucket.Generation, "CredentialsReady", false, "Blocked")
		setCondition(&status.Conditions, bucket.Generation, "UpstreamReady", false, "Blocked")
		reason := "PolicyInvalid"
		if collision {
			reason = "BucketNameCollision"
		}
		setCondition(&status.Conditions, bucket.Generation, "Ready", false, reason)
		return status, true, nil
	}
	status.PolicyRevision = compiled.Revision()
	credentialConfig, err := r.Config.Credential("objectStorageBucket", bucket.Spec.Backend.Credential, bucket.Namespace)
	if err != nil {
		setCondition(&status.Conditions, bucket.Generation, "CredentialsReady", false, "CredentialsInvalid")
		setCondition(&status.Conditions, bucket.Generation, "UpstreamReady", false, "Blocked")
		setCondition(&status.Conditions, bucket.Generation, "Ready", false, "CredentialsInvalid")
		return status, true, nil
	}
	backend, err := bindBackend(ctx, bucket.Spec.Backend, credentialConfig, r.AllowedInsecureUpstreams)
	if err != nil {
		setCondition(&status.Conditions, bucket.Generation, "CredentialsReady", false, "ConfigurationInvalid")
		setCondition(&status.Conditions, bucket.Generation, "UpstreamReady", false, "Blocked")
		setCondition(&status.Conditions, bucket.Generation, "Ready", false, "ConfigurationInvalid")
		return status, true, nil
	}
	setCondition(&status.Conditions, bucket.Generation, "CredentialsReady", true, "Loaded")
	credentialRevision, err := objectstore.CredentialRevision(r.Config, bucket.Namespace, backend)
	if err != nil {
		setCondition(&status.Conditions, bucket.Generation, "CredentialsReady", false, "CredentialsInvalid")
		setCondition(&status.Conditions, bucket.Generation, "UpstreamReady", false, "Blocked")
		setCondition(&status.Conditions, bucket.Generation, "Ready", false, "CredentialsInvalid")
		return status, true, nil
	}
	// Preserve clients while their backend and credential material are unchanged.
	var objectProvider objectstore.ObjectProvider
	if old := current.Objects[name]; old != nil && old.Namespace == bucket.Namespace && old.Name == bucket.Name && reflect.DeepEqual(old.Backend, backend) && old.CredentialRevision == credentialRevision {
		objectProvider = old.Provider
	} else if backend.Provider == "s3" {
		objectProvider, err = objectstore.NewS3(ctx, r.Config, bucket.Namespace, backend)
	} else {
		objectProvider, err = objectstore.NewGCS(ctx, r.Config, bucket.Namespace, backend)
	}
	if err == nil {
		upstreamContext, cancel := context.WithTimeout(ctx, 30*time.Second)
		err = objectProvider.HeadBucket(upstreamContext)
		cancel()
	}
	if err != nil {
		setCondition(&status.Conditions, bucket.Generation, "UpstreamReady", false, "UpstreamUnavailable")
	} else {
		setCondition(&status.Conditions, bucket.Generation, "UpstreamReady", true, "Available")
	}
	// Publish only providers whose upstream and cache settings are ready.
	capacity := bucket.Spec.Cache.Capacity.Value()
	readyReason := "Serving"
	if bucket.Spec.Cache.Capacity.Sign() <= 0 || capacity > r.Config.CacheCapacity.Value() || bucket.Spec.Cache.TTL.Duration <= 0 {
		err = errors.New("invalid cache settings")
		readyReason = "CacheInvalid"
	} else if err != nil {
		readyReason = "UpstreamUnavailable"
	}
	if err == nil {
		next.Objects[name] = &routing.ObjectSource{
			Namespace: bucket.Namespace, Name: bucket.Name, Bucket: name, Backend: backend, Policy: compiled, Provider: objectProvider,
			Capacity: capacity, TTL: bucket.Spec.Cache.TTL.Duration, Requests: r.Registry.RequestGate(sourceKey), CredentialRevision: credentialRevision,
		}
	}
	setCondition(&status.Conditions, bucket.Generation, "Ready", err == nil, readyReason)
	return status, true, nil
}

// retireSources drains routes omitted from the next snapshot and deletes only reconstructable local state.
func (r *Reconciler) retireSources(ctx context.Context, current, next *routing.Snapshot) error {
	for path, source := range current.Git {
		if next.Git[path] != nil {
			continue
		}
		if err := r.Registry.Retire(ctx, source.Requests); err != nil {
			return err
		}
		if err := os.RemoveAll(source.MirrorPath); err != nil {
			return err
		}
	}
	for bucket, source := range current.Objects {
		if next.Objects[bucket] != nil {
			continue
		}
		if err := r.Registry.Retire(ctx, source.Requests); err != nil {
			return err
		}
	}
	if err := r.cleanupGitStorage(next); err != nil {
		return err
	}
	if r.RetainObjectBuckets != nil {
		buckets := make(map[string]bool, len(next.Objects))
		for bucket := range next.Objects {
			buckets[bucket] = true
		}
		r.RetainObjectBuckets(buckets)
	}
	return nil
}

// cleanupGitStorage removes mirrors and interrupted refreshes without a configured route.
func (r *Reconciler) cleanupGitStorage(next *routing.Snapshot) error {
	root := filepath.Join(r.Config.StorageDirectory, "git")
	namespaces, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	expected := make(map[string]bool, len(next.Git))
	for _, source := range next.Git {
		expected[source.MirrorPath] = true
	}
	for _, namespace := range namespaces {
		namespacePath := filepath.Join(root, namespace.Name())
		if !namespace.IsDir() {
			if err := os.RemoveAll(namespacePath); err != nil {
				return err
			}
			continue
		}
		entries, readErr := os.ReadDir(namespacePath)
		if readErr != nil {
			return readErr
		}
		for _, entry := range entries {
			path := filepath.Join(namespacePath, entry.Name())
			if !expected[path] {
				if err := os.RemoveAll(path); err != nil {
					return err
				}
			}
		}
		_ = os.Remove(namespacePath)
	}
	return nil
}

// bindBackend requires a source to use the origin trusted by its credential.
func bindBackend(ctx context.Context, backend v1alpha1.ObjectBackend, credential config.Credential, allowedInsecureUpstreams []string) (v1alpha1.ObjectBackend, error) {
	origin, err := objectstore.ResolveOrigin(ctx, backend, credential)
	if err != nil {
		return v1alpha1.ObjectBackend{}, err
	}
	if backend.Endpoint == "" {
		backend.Endpoint = origin
		if backend.Provider == "gcs" && origin == config.DefaultGCSOrigin {
			backend.Endpoint = config.DefaultGCSEndpoint
		}
	} else {
		endpoint, endpointOrigin, normalizeErr := config.NormalizeEndpoint(backend.Endpoint)
		if normalizeErr != nil || endpointOrigin != origin {
			return v1alpha1.ObjectBackend{}, errors.New("backend endpoint is not permitted by the selected credential")
		}
		backend.Endpoint = endpoint
	}
	if err := validateBackend(backend, allowedInsecureUpstreams); err != nil {
		return v1alpha1.ObjectBackend{}, err
	}
	return backend, nil
}

// validateBackend enforces provider-specific endpoint and region constraints.
func validateBackend(backend v1alpha1.ObjectBackend, allowedInsecureUpstreams []string) error {
	endpoint, origin, err := config.NormalizeEndpoint(backend.Endpoint)
	if err != nil || endpoint != backend.Endpoint {
		return errors.New("invalid backend endpoint")
	}
	if !strings.HasPrefix(endpoint, "https://") && !slices.Contains(allowedInsecureUpstreams, origin) {
		return errors.New("endpoint must use HTTPS")
	}
	if backend.Provider == "s3" && backend.Region == "" || backend.Provider == "gcs" && backend.Region != "" {
		return errors.New("invalid backend region")
	}
	return nil
}

// BucketName derives a stable DNS-compatible upstream bucket name.
func BucketName(namespace, name string) string {
	normalize := func(value string) string {
		value = strings.ReplaceAll(value, ".", "-")
		if len(value) > 24 {
			return value[:24]
		}
		return value
	}
	digest := sha256.Sum256([]byte(namespace + "/" + name))
	hash := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(digest[:]))[:12]
	return normalize(namespace) + "-" + normalize(name) + "-" + hash
}

// gitPath returns the local mirror path for a repository.
func (r *Reconciler) gitPath(namespace, name string) string {
	return filepath.Join(r.Config.StorageDirectory, "git", namespace, name+".git")
}

// setCondition records a standard readiness condition.
func setCondition(conditions *[]metav1.Condition, generation int64, conditionType string, ready bool, reason string) {
	status := metav1.ConditionFalse
	if ready {
		status = metav1.ConditionTrue
	}
	meta.SetStatusCondition(conditions, metav1.Condition{Type: conditionType, Status: status, ObservedGeneration: generation, Reason: reason, Message: conditionMessage})
}

// updateGitStatus writes status only when it has changed.
func (r *Reconciler) updateGitStatus(ctx context.Context, repository *v1alpha1.GitRepository, status v1alpha1.GitRepositoryStatus) error {
	if reflect.DeepEqual(repository.Status, status) {
		return nil
	}
	repository.Status = status
	return r.Status().Update(ctx, repository)
}

// updateBucketStatus writes status only when it has changed.
func (r *Reconciler) updateBucketStatus(ctx context.Context, bucket *v1alpha1.ObjectStorageBucket, status v1alpha1.ObjectStorageBucketStatus) error {
	if reflect.DeepEqual(bucket.Status, status) {
		return nil
	}
	bucket.Status = status
	return r.Status().Update(ctx, bucket)
}
