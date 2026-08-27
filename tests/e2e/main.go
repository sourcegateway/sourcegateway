// Source Gateway <https://sourcegateway.dev>
// Copyright The Source Gateway Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/sourcegateway/sourcegateway/api/v1alpha1"
	"github.com/sourcegateway/sourcegateway/internal/controller"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	backendEndpoint = "http://127.0.0.1:8333"
	gatewayEndpoint = "https://127.0.0.1:8443"
	backendBucket   = "sourcegateway-e2e"
)

// main dispatches an end-to-end test harness phase.
func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "e2e:", err)
		os.Exit(1)
	}
}

// run executes one end-to-end test harness phase.
func run(parent context.Context, arguments []string) error {
	if len(arguments) != 1 || (arguments[0] != "bootstrap" && arguments[0] != "trigger" && arguments[0] != "slow-refresh" && arguments[0] != "cleanup") {
		return errors.New("usage: e2e bootstrap|trigger|slow-refresh|cleanup")
	}
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	configuration, err := clientcmd.BuildConfigFromFlags("", os.Getenv("KUBECONFIG"))
	if err != nil {
		return fmt.Errorf("load kubeconfig: %w", err)
	}
	runtimeScheme := runtime.NewScheme()
	if err := scheme.AddToScheme(runtimeScheme); err != nil {
		return fmt.Errorf("register Kubernetes API scheme: %w", err)
	}
	if err := v1alpha1.AddToScheme(runtimeScheme); err != nil {
		return fmt.Errorf("register Source Gateway API scheme: %w", err)
	}
	kubeClient, err := client.New(configuration, client.Options{Scheme: runtimeScheme})
	if err != nil {
		return fmt.Errorf("create Kubernetes client: %w", err)
	}
	if arguments[0] == "trigger" || arguments[0] == "slow-refresh" {
		var repository v1alpha1.GitRepository
		if err := kubeClient.Get(ctx, types.NamespacedName{Namespace: "agents", Name: "application"}, &repository); err != nil {
			return fmt.Errorf("get repository: %w", err)
		}
		if repository.Annotations == nil {
			repository.Annotations = map[string]string{}
		}
		if arguments[0] == "slow-refresh" {
			repository.Spec.Mirror.RefreshInterval = metav1.Duration{Duration: time.Hour}
		}
		repository.Annotations["sourcegateway.dev/e2e-trigger"] = time.Now().Format(time.RFC3339Nano)
		if err := kubeClient.Update(ctx, &repository); err != nil {
			return fmt.Errorf("trigger reconcile: %w", err)
		}
		if arguments[0] == "slow-refresh" {
			return waitGitGeneration(ctx, kubeClient, repository.Generation)
		}
		return nil
	}
	if arguments[0] == "cleanup" {
		return cleanup(ctx, kubeClient)
	}
	clientset, err := kubernetes.NewForConfig(configuration)
	if err != nil {
		return fmt.Errorf("create Kubernetes clientset: %w", err)
	}
	return bootstrap(ctx, kubeClient, clientset)
}

// cleanup deletes test sources and waits for Kubernetes to observe their removal.
func cleanup(ctx context.Context, kubeClient client.Client) error {
	repository := &v1alpha1.GitRepository{ObjectMeta: metav1.ObjectMeta{Namespace: "agents", Name: "application"}}
	bucket := &v1alpha1.ObjectStorageBucket{ObjectMeta: metav1.ObjectMeta{Namespace: "agents", Name: "artifacts"}}
	if err := kubeClient.Delete(ctx, repository); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete Git source: %w", err)
	}
	if err := kubeClient.Delete(ctx, bucket); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete object source: %w", err)
	}
	for {
		gitErr := kubeClient.Get(ctx, types.NamespacedName{Namespace: "agents", Name: "application"}, repository)
		bucketErr := kubeClient.Get(ctx, types.NamespacedName{Namespace: "agents", Name: "artifacts"}, bucket)
		if apierrors.IsNotFound(gitErr) && apierrors.IsNotFound(bucketErr) {
			return nil
		}
		select {
		case <-ctx.Done():
			detail := fmt.Sprintf("git=%v, bucket=%v", gitErr, bucketErr)
			return fmt.Errorf("source deletion did not complete (%s): %w", detail, ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// waitGitGeneration waits until a repository reports a ready generation.
func waitGitGeneration(ctx context.Context, kubeClient client.Client, generation int64) error {
	for {
		var repository v1alpha1.GitRepository
		err := kubeClient.Get(ctx, types.NamespacedName{Namespace: "agents", Name: "application"}, &repository)
		if err == nil && repository.Status.ObservedGeneration >= generation && ready(repository.Status.Conditions) {
			return nil
		}
		select {
		case <-ctx.Done():
			detail := fmt.Sprintf("last error: %v", err)
			return fmt.Errorf("git source did not observe generation %d (%s): %w", generation, detail, ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// bootstrap creates fixtures, waits for reconciliation, and exercises S3 access.
func bootstrap(ctx context.Context, kubeClient client.Client, clientset kubernetes.Interface) error {
	// Establish Kubernetes and backend fixtures.
	if err := createIgnoringExists(ctx, kubeClient, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "agents"}}); err != nil {
		return err
	}
	if err := createIgnoringExists(ctx, kubeClient, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: "agents", Name: "agent"}}); err != nil {
		return err
	}
	if err := createIgnoringExists(ctx, kubeClient, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: "agents", Name: "other"}}); err != nil {
		return err
	}

	backend, err := newS3Client(ctx, backendEndpoint, "", false)
	if err != nil {
		return fmt.Errorf("create backend S3 client: %w", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		_, err := backend.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(backendBucket)})
		if err == nil || strings.Contains(err.Error(), "BucketAlready") {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("create backend bucket: %w", err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("create backend bucket: %w", ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}

	repository := &v1alpha1.GitRepository{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "GitRepository"},
		ObjectMeta: metav1.ObjectMeta{Namespace: "agents", Name: "application"},
		Spec: v1alpha1.GitRepositorySpec{
			Upstream: v1alpha1.GitUpstream{URL: "https://127.0.0.1:9443/application.git", Credential: "dev-git"},
			Mirror:   v1alpha1.GitMirror{RefreshInterval: metav1.Duration{Duration: time.Second}, MaxStaleness: metav1.Duration{Duration: time.Minute}},
			Authorization: v1alpha1.GitAuthorization{Rules: []v1alpha1.GitAuthorizationRule{
				{Name: "read", Operations: []string{"git.read"}, Allow: `sa.namespace == "agents"`},
				{Name: "push", Operations: []string{"git.push"}, Allow: `sa.name == "agent" && request.branch.startsWith("allowed/") && !request.force && !request.delete`},
			}},
		},
	}
	// Verify each CRD rejects operations from the other policy vocabulary.
	invalidRepository := repository.DeepCopy()
	invalidRepository.Name = "invalid-operation"
	invalidRepository.Spec.Authorization.Rules[0].Operations = []string{"object.read"}
	if err := kubeClient.Create(ctx, invalidRepository); err == nil {
		return errors.New("GitRepository accepted an object operation")
	} else if !apierrors.IsInvalid(err) {
		return fmt.Errorf("verify GitRepository operation validation: %w", err)
	}
	if err := createIgnoringExists(ctx, kubeClient, repository); err != nil {
		return err
	}
	bucket := &v1alpha1.ObjectStorageBucket{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "ObjectStorageBucket"},
		ObjectMeta: metav1.ObjectMeta{Namespace: "agents", Name: "artifacts"},
		Spec: v1alpha1.ObjectStorageBucketSpec{
			Backend: v1alpha1.ObjectBackend{Provider: "s3", Endpoint: backendEndpoint, Region: "us-east-1", Bucket: backendBucket, Credential: "dev-s3"},
			Cache:   v1alpha1.ObjectCache{Capacity: resource.MustParse("1Mi"), TTL: metav1.Duration{Duration: time.Minute}},
			Authorization: v1alpha1.ObjectAuthorization{Rules: []v1alpha1.ObjectAuthorizationRule{
				{Name: "buckets", Operations: []string{"bucket.list", "bucket.head"}, Allow: `sa.name == "agent"`},
				{Name: "objects", Operations: []string{"object.list", "object.read", "object.write", "object.delete"}, Allow: `request.key.startsWith("workspaces/" + sa.name + "/")`},
			}},
		},
	}
	invalidBucket := bucket.DeepCopy()
	invalidBucket.Name = "invalid-operation"
	invalidBucket.Spec.Authorization.Rules[0].Operations = []string{"git.read"}
	if err := kubeClient.Create(ctx, invalidBucket); err == nil {
		return errors.New("ObjectStorageBucket accepted a Git operation")
	} else if !apierrors.IsInvalid(err) {
		return fmt.Errorf("verify ObjectStorageBucket operation validation: %w", err)
	}
	if err := createIgnoringExists(ctx, kubeClient, bucket); err != nil {
		return err
	}
	if err := waitReady(ctx, kubeClient); err != nil {
		return err
	}

	// Export runtime values and verify the reconciled gateway.
	token, err := requestToken(ctx, clientset, "agent")
	if err != nil {
		return err
	}
	root := os.Getenv("DEV_ROOT")
	if err := os.WriteFile(filepath.Join(root, "token"), []byte(token), 0o600); err != nil { // #nosec G703 -- root is the test harness-owned DEV_ROOT.
		return fmt.Errorf("write token: %w", err)
	}
	bucketName := controller.BucketName("agents", "artifacts")
	if err := os.WriteFile(filepath.Join(root, "bucket-name"), []byte(bucketName), 0o600); err != nil { // #nosec G703 -- root is the test harness-owned DEV_ROOT.
		return fmt.Errorf("write bucket name: %w", err)
	}
	return testS3(ctx, token, bucketName)
}

// testS3 exercises allowed, ranged, copied, denied, and deleted objects.
func testS3(ctx context.Context, token, bucket string) error {
	client, err := newS3Client(ctx, gatewayEndpoint, token, true)
	if err != nil {
		return fmt.Errorf("create gateway S3 client: %w", err)
	}
	payload := []byte("source gateway e2e\n")
	digest := sha256.Sum256(payload)
	key := "workspaces/agent/artifact.txt"
	if _, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader(payload), ContentLength: aws.Int64(int64(len(payload))),
		ChecksumSHA256: aws.String(base64.StdEncoding.EncodeToString(digest[:])),
	}); err != nil {
		return fmt.Errorf("S3 put: %w", err)
	}
	if _, err := client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)}); err != nil {
		return fmt.Errorf("S3 head bucket: %w", err)
	}
	buckets, err := client.ListBuckets(ctx, &s3.ListBucketsInput{})
	if err != nil {
		return fmt.Errorf("S3 list buckets: %w", err)
	}
	if len(buckets.Buckets) != 1 || aws.ToString(buckets.Buckets[0].Name) != bucket {
		return errors.New("S3 list buckets returned unexpected buckets")
	}
	get, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		return fmt.Errorf("S3 get: %w", err)
	}
	got, readErr := io.ReadAll(get.Body)
	closeErr := get.Body.Close()
	if readErr != nil {
		return fmt.Errorf("read S3 object: %w", readErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close S3 object: %w", closeErr)
	}
	if !bytes.Equal(got, payload) {
		return errors.New("S3 get returned wrong content")
	}
	// Check compatibility beyond the basic SDK read/write path.
	partial, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Range: aws.String("bytes=-4")})
	if err != nil {
		return fmt.Errorf("S3 ranged get: %w", err)
	}
	partialBody, readErr := io.ReadAll(partial.Body)
	closeErr = partial.Body.Close()
	if readErr != nil {
		return fmt.Errorf("read ranged S3 object: %w", readErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close ranged S3 object: %w", closeErr)
	}
	if string(partialBody) != "e2e\n" || aws.ToString(partial.ContentRange) != "bytes 15-18/19" {
		return errors.New("S3 ranged get returned wrong content or range")
	}
	listed, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String("workspaces/agent/")})
	if err != nil {
		return fmt.Errorf("S3 list: %w", err)
	}
	if len(listed.Contents) != 1 {
		return fmt.Errorf("S3 list returned %d objects, want 1", len(listed.Contents))
	}
	rclone := exec.CommandContext(ctx, "rclone", "lsf", ":s3:"+bucket+"/workspaces/agent/", // #nosec G204,G702 -- all values come from this isolated test's API objects and token.
		"--s3-provider=Other", "--s3-endpoint="+gatewayEndpoint, "--s3-env-auth=false", "--s3-no-check-bucket", "--s3-list-version=2",
		"--s3-access-key-id=", "--s3-secret-access-key=", "--header=Authorization: Bearer "+token, "--ca-cert="+filepath.Join(os.Getenv("DEV_ROOT"), "ca.crt"))
	output, err := rclone.CombinedOutput()
	if err != nil {
		return fmt.Errorf("rclone list: %s: %w", strings.TrimSpace(string(output)), err)
	}
	if !strings.Contains(string(output), "artifact.txt") {
		return fmt.Errorf("rclone list did not include artifact.txt: %s", strings.TrimSpace(string(output)))
	}
	copyKey := "workspaces/agent/copied.txt"
	stale := []byte("old destination\n")
	staleDigest := sha256.Sum256(stale)
	if _, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(copyKey), Body: bytes.NewReader(stale), ContentLength: aws.Int64(int64(len(stale))),
		ChecksumSHA256: aws.String(base64.StdEncoding.EncodeToString(staleDigest[:])),
	}); err != nil {
		return fmt.Errorf("S3 seed copy destination: %w", err)
	}
	if _, err := client.CopyObject(ctx, &s3.CopyObjectInput{Bucket: aws.String(bucket), Key: aws.String(copyKey), CopySource: aws.String(bucket + "/" + key)}); err != nil {
		return fmt.Errorf("S3 copy: %w", err)
	}
	copied, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(copyKey)})
	if err != nil {
		return fmt.Errorf("S3 get copied object: %w", err)
	}
	copiedBody, readErr := io.ReadAll(copied.Body)
	closeErr = copied.Body.Close()
	if readErr != nil {
		return fmt.Errorf("read copied S3 object: %w", readErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close copied S3 object: %w", closeErr)
	}
	if !bytes.Equal(copiedBody, payload) {
		return errors.New("S3 copy returned stale destination content")
	}
	if err := testStreamingUploads(ctx, client, token, bucket); err != nil {
		return err
	}
	denied := []byte("denied")
	deniedDigest := sha256.Sum256(denied)
	_, err = client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String("forbidden/object"), Body: bytes.NewReader(denied), ContentLength: aws.Int64(int64(len(denied))),
		ChecksumSHA256: aws.String(base64.StdEncoding.EncodeToString(deniedDigest[:])),
	})
	if err == nil {
		return errors.New("S3 CEL denial was not enforced")
	}
	if !strings.Contains(err.Error(), "AccessDenied") {
		return fmt.Errorf("S3 CEL denial returned an unexpected error: %w", err)
	}
	if _, err := client.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: aws.String(bucket), Delete: &s3types.Delete{Objects: []s3types.ObjectIdentifier{{Key: aws.String(key)}, {Key: aws.String(copyKey)}}}}); err != nil {
		return fmt.Errorf("S3 multi-delete: %w", err)
	}
	return nil
}

// testStreamingUploads covers HTTP chunking and the core S3 multipart lifecycle.
func testStreamingUploads(ctx context.Context, client *s3.Client, token, bucket string) error {
	httpClient, err := newTestHTTPClient(token, true)
	if err != nil {
		return err
	}
	chunkedKey := "workspaces/agent/chunked.txt"
	chunkedPayload := []byte("streamed through HTTP chunking\n")
	digest := sha256.Sum256(chunkedPayload)
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, gatewayEndpoint+"/"+bucket+"/"+chunkedKey, bytes.NewReader(chunkedPayload))
	if err != nil {
		return fmt.Errorf("create chunked S3 request: %w", err)
	}
	request.ContentLength = -1
	request.Header.Set("Expect", "100-continue")
	request.Trailer = http.Header{"X-Amz-Checksum-Sha256": []string{base64.StdEncoding.EncodeToString(digest[:])}}
	response, err := httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("S3 chunked put: %w", err)
	}
	_, readErr := io.Copy(io.Discard, response.Body)
	closeErr := response.Body.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return fmt.Errorf("read S3 chunked put response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("S3 chunked put returned %s", response.Status)
	}
	got, err := readS3Object(ctx, client, bucket, chunkedKey)
	if err != nil {
		return err
	}
	if !bytes.Equal(got, chunkedPayload) {
		return errors.New("S3 chunked upload returned wrong content")
	}

	// Rejected checksums must never replace an existing authoritative object.
	badDigestRequest, err := http.NewRequestWithContext(ctx, http.MethodPut, gatewayEndpoint+"/"+bucket+"/"+chunkedKey, strings.NewReader("invalid replacement\n"))
	if err != nil {
		return fmt.Errorf("create bad-digest S3 request: %w", err)
	}
	badDigestRequest.Header.Set("X-Amz-Checksum-Sha256", base64.StdEncoding.EncodeToString(make([]byte, sha256.Size)))
	badDigestResponse, err := httpClient.Do(badDigestRequest)
	if err != nil {
		return fmt.Errorf("S3 bad-digest put: %w", err)
	}
	_, readErr = io.Copy(io.Discard, badDigestResponse.Body)
	closeErr = badDigestResponse.Body.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return fmt.Errorf("read S3 bad-digest response: %w", err)
	}
	if badDigestResponse.StatusCode != http.StatusBadRequest {
		return fmt.Errorf("S3 bad-digest put returned %s", badDigestResponse.Status)
	}
	got, err = readS3Object(ctx, client, bucket, chunkedKey)
	if err != nil {
		return err
	}
	if !bytes.Equal(got, chunkedPayload) {
		return errors.New("S3 bad-digest upload replaced the authoritative object")
	}

	// An unknown-length upload that crosses the configured limit must also leave the old object intact.
	oversizedRequest, err := http.NewRequestWithContext(ctx, http.MethodPut, gatewayEndpoint+"/"+bucket+"/"+chunkedKey, bytes.NewReader(bytes.Repeat([]byte("x"), 15<<20+1)))
	if err != nil {
		return fmt.Errorf("create oversized S3 request: %w", err)
	}
	oversizedRequest.ContentLength = -1
	oversizedResponse, err := httpClient.Do(oversizedRequest)
	if err != nil {
		return fmt.Errorf("S3 oversized put: %w", err)
	}
	_, readErr = io.Copy(io.Discard, oversizedResponse.Body)
	closeErr = oversizedResponse.Body.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return fmt.Errorf("read S3 oversized response: %w", err)
	}
	if oversizedResponse.StatusCode != http.StatusRequestEntityTooLarge {
		return fmt.Errorf("S3 oversized put returned %s", oversizedResponse.Status)
	}
	got, err = readS3Object(ctx, client, bucket, chunkedKey)
	if err != nil {
		return err
	}
	if !bytes.Equal(got, chunkedPayload) {
		return errors.New("S3 oversized upload replaced the authoritative object")
	}

	// Cache capacity is admission-only: an object larger than the per-source cache still commits upstream.
	uncachedKey := "workspaces/agent/uncached.bin"
	uncachedPayload := bytes.Repeat([]byte("u"), 2<<20)
	uncachedDigest := sha256.Sum256(uncachedPayload)
	if _, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(uncachedKey), Body: bytes.NewReader(uncachedPayload), ContentLength: aws.Int64(int64(len(uncachedPayload))),
		ChecksumSHA256: aws.String(base64.StdEncoding.EncodeToString(uncachedDigest[:])),
	}); err != nil {
		return fmt.Errorf("S3 uncached put: %w", err)
	}
	got, err = readS3Object(ctx, client, bucket, uncachedKey)
	if err != nil {
		return err
	}
	if !bytes.Equal(got, uncachedPayload) {
		return errors.New("S3 uncached upload returned wrong content")
	}

	// Multipart staging accepts S3's five-mebibyte minimum non-final part.
	multipartKey := "workspaces/agent/multipart.bin"
	created, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String(multipartKey), ContentType: aws.String("application/octet-stream")})
	if err != nil {
		return fmt.Errorf("S3 create multipart upload: %w", err)
	}
	first := bytes.Repeat([]byte("a"), 5<<20)
	second := []byte("final part\n")
	partOne, err := client.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String(bucket), Key: aws.String(multipartKey), UploadId: created.UploadId, PartNumber: aws.Int32(1), Body: bytes.NewReader(first), ContentLength: aws.Int64(int64(len(first)))})
	if err != nil {
		return fmt.Errorf("S3 upload first multipart part: %w", err)
	}
	partTwo, err := client.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String(bucket), Key: aws.String(multipartKey), UploadId: created.UploadId, PartNumber: aws.Int32(2), Body: bytes.NewReader(second), ContentLength: aws.Int64(int64(len(second)))})
	if err != nil {
		return fmt.Errorf("S3 upload final multipart part: %w", err)
	}
	_, err = client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: aws.String(bucket), Key: aws.String(multipartKey), UploadId: created.UploadId,
		MultipartUpload: &s3types.CompletedMultipartUpload{Parts: []s3types.CompletedPart{
			{ETag: partOne.ETag, PartNumber: aws.Int32(1)},
			{ETag: partTwo.ETag, PartNumber: aws.Int32(2)},
		}},
	})
	if err != nil {
		return fmt.Errorf("S3 complete multipart upload: %w", err)
	}
	want := append(first, second...)
	got, err = readS3Object(ctx, client, bucket, multipartKey)
	if err != nil {
		return err
	}
	if !bytes.Equal(got, want) {
		return errors.New("S3 multipart upload returned wrong content")
	}

	// Abort must release a staged upload without publishing an object.
	aborted, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String("workspaces/agent/aborted.bin")})
	if err != nil {
		return fmt.Errorf("S3 create aborted upload: %w", err)
	}
	if _, err := client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String("workspaces/agent/aborted.bin"), UploadId: aborted.UploadId}); err != nil {
		return fmt.Errorf("S3 abort multipart upload: %w", err)
	}
	if _, err := client.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: aws.String(bucket), Delete: &s3types.Delete{Objects: []s3types.ObjectIdentifier{{Key: aws.String(chunkedKey)}, {Key: aws.String(uncachedKey)}, {Key: aws.String(multipartKey)}}}}); err != nil {
		return fmt.Errorf("delete streaming test objects: %w", err)
	}
	return nil
}

// readS3Object returns a complete test object and closes its response body.
func readS3Object(ctx context.Context, client *s3.Client, bucket, key string) ([]byte, error) {
	output, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		return nil, fmt.Errorf("S3 get %s: %w", key, err)
	}
	body, readErr := io.ReadAll(output.Body)
	closeErr := output.Body.Close()
	if readErr != nil {
		return nil, fmt.Errorf("read S3 object %s: %w", key, readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close S3 object %s: %w", key, closeErr)
	}
	return body, nil
}

// newS3Client builds a path-style client for a test endpoint.
func newS3Client(ctx context.Context, endpoint, token string, secure bool) (*s3.Client, error) {
	httpClient, err := newTestHTTPClient(token, secure)
	if err != nil {
		return nil, err
	}
	configuration, err := config.LoadDefaultConfig(ctx, config.WithRegion("us-east-1"), config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("dev", "dev", "")), config.WithHTTPClient(httpClient))
	if err != nil {
		return nil, fmt.Errorf("create AWS configuration: %w", err)
	}
	return s3.NewFromConfig(configuration, func(options *s3.Options) { options.BaseEndpoint = aws.String(endpoint); options.UsePathStyle = true }), nil
}

// newTestHTTPClient builds a bearer-authenticated client for a test endpoint.
func newTestHTTPClient(token string, secure bool) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if secure {
		certificate, err := os.ReadFile(filepath.Join(os.Getenv("DEV_ROOT"), "ca.crt")) // #nosec G703 -- DEV_ROOT is created by the isolated test harness.
		if err != nil {
			return nil, fmt.Errorf("read development CA: %w", err)
		}
		pool, err := loadCertPool(certificate)
		if err != nil {
			return nil, fmt.Errorf("load development CA: %w", err)
		}
		transport.TLSClientConfig = pool
	}
	return &http.Client{Transport: bearerTransport{token: token, next: transport}}, nil
}

// createIgnoringExists makes a fixture idempotently.
func createIgnoringExists(ctx context.Context, kubeClient client.Client, object client.Object) error {
	if err := kubeClient.Create(ctx, object); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create %T: %w", object, err)
	}
	return nil
}

// waitReady waits until both test sources report Ready.
func waitReady(ctx context.Context, kubeClient client.Client) error {
	for {
		var repository v1alpha1.GitRepository
		gitErr := kubeClient.Get(ctx, types.NamespacedName{Namespace: "agents", Name: "application"}, &repository)
		var bucket v1alpha1.ObjectStorageBucket
		bucketErr := kubeClient.Get(ctx, types.NamespacedName{Namespace: "agents", Name: "artifacts"}, &bucket)
		if gitErr == nil && bucketErr == nil && ready(repository.Status.Conditions) && ready(bucket.Status.Conditions) {
			return nil
		}
		select {
		case <-ctx.Done():
			detail := fmt.Sprintf("git=%v, bucket=%v", gitErr, bucketErr)
			return fmt.Errorf("sources did not become ready (%s): %w", detail, ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// ready reports whether a condition set declares the source ready.
func ready(conditions []metav1.Condition) bool {
	for _, condition := range conditions {
		if condition.Type == "Ready" {
			return condition.Status == metav1.ConditionTrue
		}
	}
	return false
}

// requestToken creates a short-lived ServiceAccount token for a test client.
func requestToken(ctx context.Context, clientset kubernetes.Interface, name string) (string, error) {
	token, err := clientset.CoreV1().ServiceAccounts("agents").CreateToken(ctx, name, &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{Audiences: []string{"sourcegateway.dev"}, ExpirationSeconds: aws.Int64(600)}}, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("request ServiceAccount token: %w", err)
	}
	return token.Status.Token, nil
}

// bearerTransport adds the test token to outgoing requests.
type bearerTransport struct {
	token string
	next  http.RoundTripper
}

// RoundTrip attaches authorization without mutating the caller's request.
func (t bearerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	copy := request.Clone(request.Context())
	if t.token != "" {
		copy.Header.Set("Authorization", "Bearer "+t.token)
	}
	return t.next.RoundTrip(copy)
}

// loadCertPool builds a TLS configuration for the development CA.
func loadCertPool(certificate []byte) (*tls.Config, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certificate) {
		return nil, errors.New("CA certificate is invalid")
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}, nil
}
