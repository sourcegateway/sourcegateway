// Source Gateway <https://sourcegateway.dev>
// Copyright The Source Gateway Authors
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// TestMultipartReaderStreamsAndSeeks checks assembly across file boundaries.
func TestMultipartReaderStreamsAndSeeks(t *testing.T) {
	root := t.TempDir()
	parts := make([]multipartPart, 0, 2)
	for index, contents := range []string{"abc", "defg"} {
		path := filepath.Join(root, string(rune('a'+index)))
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		parts = append(parts, multipartPart{path: path, size: int64(len(contents))})
	}
	reader := &multipartReader{parts: parts, total: 7}
	assembled, err := io.ReadAll(reader)
	if err != nil || string(assembled) != "abcdefg" {
		t.Fatalf("assembled = %q, %v", assembled, err)
	}
	if _, err := reader.Seek(2, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	remainder, err := io.ReadAll(reader)
	if err != nil || string(remainder) != "cdefg" {
		t.Fatalf("seek remainder = %q, %v", remainder, err)
	}
}

// TestMultipartStoreValidatesCompletion checks destination, order, and ETag binding.
func TestMultipartStoreValidatesCompletion(t *testing.T) {
	store := newMultipartStore()
	identifier, err := store.create("bucket", "key", "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	part := multipartPart{path: filepath.Join(t.TempDir(), "part"), size: 4, etag: "etag"}
	if err := store.storePart(identifier, "bucket", "key", 1, part); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.beginCompletion(identifier, "bucket", "other", []completedMultipartPart{{PartNumber: 1, ETag: `"etag"`}}); !errors.Is(err, errNoSuchUpload) {
		t.Fatalf("wrong destination returned %v", err)
	}
	if _, _, err := store.beginCompletion(identifier, "bucket", "key", []completedMultipartPart{{PartNumber: 1, ETag: `"wrong"`}}); !errors.Is(err, errInvalidPart) {
		t.Fatalf("wrong ETag returned %v", err)
	}
	parts, contentType, err := store.beginCompletion(identifier, "bucket", "key", []completedMultipartPart{{PartNumber: 1, ETag: `"etag"`}})
	if err != nil || len(parts) != 1 || contentType != "text/plain" {
		t.Fatalf("completion = %#v, %q, %v", parts, contentType, err)
	}
	upload, err := store.remove(identifier, "bucket", "key", true)
	if err != nil {
		t.Fatal(err)
	}
	cleanupMultipartUploads([]*multipartUpload{upload})
}
