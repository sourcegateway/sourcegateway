// Source Gateway <https://sourcegateway.dev>
// Copyright The Source Gateway Authors
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"github.com/aws/smithy-go"
	"github.com/sourcegateway/sourcegateway/api/v1alpha1"
	"github.com/sourcegateway/sourcegateway/internal/auth"
	"github.com/sourcegateway/sourcegateway/internal/objectstore"
	"github.com/sourcegateway/sourcegateway/internal/policy"
	"github.com/sourcegateway/sourcegateway/internal/routing"
)

// TestProviderErrorMapsAndRedacts checks stable errors without provider detail leakage.
func TestProviderErrorMapsAndRedacts(t *testing.T) {
	var logs bytes.Buffer
	handler := &s3Handler{logger: slog.New(slog.NewJSONHandler(&logs, nil))}
	response := httptest.NewRecorder()
	handler.providerError(response, &smithy.GenericAPIError{Code: "NoSuchKey", Message: "sensitive-provider-message"})
	if response.Code != http.StatusNotFound || !strings.Contains(response.Body.String(), "NoSuchKey") {
		t.Fatalf("response = %d %q", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "sensitive") || strings.Contains(logs.String(), "sensitive-provider-message") {
		t.Fatalf("provider detail leaked in response or logs: %q %q", response.Body.String(), logs.String())
	}
}

// TestMutationProviderErrorReportsUnknownOutcome checks that clients are not encouraged to retry a possible commit.
func TestMutationProviderErrorReportsUnknownOutcome(t *testing.T) {
	var logs bytes.Buffer
	handler := &s3Handler{logger: slog.New(slog.NewJSONHandler(&logs, nil))}
	response := httptest.NewRecorder()
	handler.mutationProviderError(response, fmt.Errorf("lost acknowledgement: %w", objectstore.ErrMutationOutcomeUnknown))
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "OperationOutcomeUnknown") || response.Header().Get("Retry-After") != "" {
		t.Fatalf("response = %d %q, Retry-After %q", response.Code, response.Body.String(), response.Header().Get("Retry-After"))
	}
}

// TestObjectLocksSerializeOverlappingKeys checks ordering without reverse-copy deadlocks.
func TestObjectLocksSerializeOverlappingKeys(t *testing.T) {
	locks := newObjectLocks()
	release := locks.lockMany("bucket", []string{"source", "destination"})
	var acquired atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		secondRelease := locks.lockMany("bucket", []string{"destination", "source"})
		acquired.Store(true)
		secondRelease()
	}()
	time.Sleep(10 * time.Millisecond)
	if acquired.Load() {
		t.Fatal("second operation acquired the same key concurrently")
	}
	release()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("second operation did not acquire the released key")
	}
}

// TestProviderNotFoundRecognizesGCSSentinel checks wrapped GCS absence errors.
func TestProviderNotFoundRecognizesGCSSentinel(t *testing.T) {
	if !providerNotFound(storage.ErrObjectNotExist) || !providerNotFound(fmt.Errorf("wrapper: %w", storage.ErrObjectNotExist)) {
		t.Fatal("GCS object absence was not recognized")
	}
}

// TestAuthorizationAuditRedactsRequestAndTopology checks that object keys stay out of audits.
func TestAuthorizationAuditRedactsRequestAndTopology(t *testing.T) {
	compiled, err := policy.CompileObject([]v1alpha1.ObjectAuthorizationRule{{Name: "allow", Operations: []string{"object.read"}, Allow: "true"}})
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	handler := &s3Handler{logger: slog.New(slog.NewJSONHandler(&logs, nil))}
	source := &routing.ObjectSource{Namespace: "agents", Name: "artifacts", Bucket: "public-derived-name", Policy: compiled}
	if !handler.authorize(source, auth.Identity{Namespace: "agents", Name: "builder", UID: "uid"}, policy.Request{Operation: "object.read", Key: "sensitive/object/key"}) {
		t.Fatal("authorization unexpectedly denied")
	}
	if strings.Contains(logs.String(), "sensitive/object/key") {
		t.Fatalf("request key leaked in audit: %s", logs.String())
	}
}

// TestNormalizeByteRange covers supported and malformed single byte ranges.
func TestNormalizeByteRange(t *testing.T) {
	tests := []struct {
		input, normalized, contentRange string
	}{
		{input: "bytes=2-5", normalized: "bytes=2-5", contentRange: "bytes 2-5/10"},
		{input: "bytes=7-", normalized: "bytes=7-9", contentRange: "bytes 7-9/10"},
		{input: "bytes=-4", normalized: "bytes=6-9", contentRange: "bytes 6-9/10"},
		{input: "bytes=0-99", normalized: "bytes=0-9", contentRange: "bytes 0-9/10"},
	}
	for _, test := range tests {
		normalized, contentRange, err := normalizeByteRange(test.input, 10)
		if err != nil || normalized != test.normalized || contentRange != test.contentRange {
			t.Errorf("normalizeByteRange(%q) = %q, %q, %v", test.input, normalized, contentRange, err)
		}
	}
	for _, invalid := range []string{"items=0-1", "bytes=", "bytes=1-2,4-5", "bytes=10-", "bytes=4-2", "bytes=--1"} {
		if _, _, err := normalizeByteRange(invalid, 10); err == nil {
			t.Errorf("normalizeByteRange(%q) unexpectedly succeeded", invalid)
		}
	}
}

// TestValidPrefixAllowsOnlyKeyPrefixes checks portable, traversal-free listing prefixes.
func TestValidPrefixAllowsOnlyKeyPrefixes(t *testing.T) {
	for _, valid := range []string{"", "workspaces", "workspaces/agent/", "object.partial"} {
		if !validPrefix(valid) {
			t.Errorf("validPrefix(%q) returned false", valid)
		}
	}
	for _, invalid := range []string{"/", "workspaces//agent", "workspaces/../other", "sensitive-♥"} {
		if validPrefix(invalid) {
			t.Errorf("validPrefix(%q) returned true", invalid)
		}
	}
}

// TestAWSChunkedBodyDecodesPayloadAndTrailers checks modern S3 SDK framing.
func TestAWSChunkedBodyDecodesPayloadAndTrailers(t *testing.T) {
	encoded := "3\r\nabc\r\n4\r\ndefg\r\n0\r\nx-amz-checksum-crc32: MMMMMM==\r\n\r\n"
	body := newAWSChunkedBody(strings.NewReader(encoded))
	decoded, err := io.ReadAll(body)
	if err != nil || string(decoded) != "abcdefg" {
		t.Fatalf("decoded = %q, %v", decoded, err)
	}
	if got := body.trailers.Get("X-Amz-Checksum-Crc32"); got != "MMMMMM==" {
		t.Fatalf("trailer = %q", got)
	}
}

// TestUploadStagingDecision protects the validation barrier before upstream writes.
func TestUploadStagingDecision(t *testing.T) {
	tests := []struct {
		name            string
		maximum         int64
		contentLength   int64
		contentEncoding string
		checksum        string
		want            bool
	}{
		{name: "finite fixed length", maximum: 15 << 20, contentLength: 10, want: true},
		{name: "unlimited fixed length", contentLength: 10, want: false},
		{name: "unlimited unknown length", contentLength: -1, want: true},
		{name: "unlimited AWS chunked", contentLength: 10, contentEncoding: "aws-chunked", want: true},
		{name: "unlimited checksummed", contentLength: 10, checksum: "digest", want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPut, "/bucket/key", strings.NewReader("payload"))
			request.ContentLength = test.contentLength
			request.Header.Set("Content-Encoding", test.contentEncoding)
			request.Header.Set("X-Amz-Checksum-Sha256", test.checksum)
			handler := &s3Handler{maxObjectSize: test.maximum}
			if got := handler.mustStageUpload(request); got != test.want {
				t.Fatalf("mustStageUpload() = %t, want %t", got, test.want)
			}
		})
	}
}
