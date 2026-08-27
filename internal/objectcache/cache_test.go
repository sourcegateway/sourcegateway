// Source Gateway <https://sourcegateway.dev>
// Copyright The Source Gateway Authors
// SPDX-License-Identifier: Apache-2.0

package objectcache

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sourcegateway/sourcegateway/internal/objectstore"
)

// TestCacheStoreGetAndRecover exercises persistence and startup cleanup.
func TestCacheStoreGetAndRecover(t *testing.T) {
	root := t.TempDir()
	cache, err := open(root, 1024, func(int64) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(source, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	release, err := cache.Reserve("bucket", 5, 10)
	if err != nil {
		t.Fatal(err)
	}
	info := objectstore.ObjectInfo{Key: "key", Size: 5, ETag: "etag", LastModified: time.Now()}
	if err := cache.Store("bucket", "key", source, info); err != nil {
		t.Fatal(err)
	}
	release()
	path, got, ok := cache.Get("bucket", "key", time.Minute)
	if !ok || got.ETag != "etag" {
		t.Fatalf("Get() = %q, %#v, %v", path, got, ok)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "hello" { // #nosec G304 -- path is returned by the cache under test.
		t.Fatalf("cached data = %q, %v", data, err)
	}

	if err := os.WriteFile(filepath.Join(root, "orphan.data"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	reopened, err := open(root, 1024, func(int64) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "orphan.data")); !os.IsNotExist(err) {
		t.Fatalf("orphan remains: %v", err)
	}
	if len(reopened.Stats()) != 1 || reopened.Stats()[0].Bytes != 5 {
		t.Fatalf("Stats() = %#v", reopened.Stats())
	}
	reopened.RetainBuckets(map[string]bool{})
	if len(reopened.Stats()) != 0 {
		t.Fatalf("Stats() after RetainBuckets = %#v", reopened.Stats())
	}
}

// TestCacheEnforcesPerResourceCapacity verifies bucket-specific limits.
func TestCacheEnforcesPerResourceCapacity(t *testing.T) {
	cache, err := open(t.TempDir(), 100, func(int64) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Reserve("bucket", 11, 10); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("Reserve() error = %v", err)
	}
}

// TestHasDiskCapacityChecksRangesAndReserve verifies block-based disk accounting.
func TestHasDiskCapacityChecksRangesAndReserve(t *testing.T) {
	tests := []struct {
		name                    string
		blocks, availableBlocks uint64
		blockSize               uint64
		requested               int64
		want                    bool
	}{
		{name: "preserves reserve", blocks: 100, availableBlocks: 20, blockSize: 10, requested: 100, want: true},
		{name: "uses reserve", blocks: 100, availableBlocks: 20, blockSize: 10, requested: 101},
		{name: "exceeds available", blocks: 100, availableBlocks: 20, blockSize: 10, requested: 201},
		{name: "negative request", blocks: 100, availableBlocks: 20, blockSize: 10, requested: -1},
		{name: "invalid block size", blocks: 100, availableBlocks: 20, blockSize: 0},
		{name: "rounds reserve up", blocks: 11, availableBlocks: 1, blockSize: 10},
		{name: "rounds request up", blocks: 10, availableBlocks: 2, blockSize: 10, requested: 1, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := hasDiskCapacity(test.blocks, test.availableBlocks, test.blockSize, test.requested); got != test.want {
				t.Fatalf("hasDiskCapacity() = %t; want %t", got, test.want)
			}
		})
	}
}

// TestCacheRejectsSameLengthCorruption verifies recovery hashes cached bytes.
func TestCacheRejectsSameLengthCorruption(t *testing.T) {
	root := t.TempDir()
	cache, err := open(root, 100, func(int64) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(source, []byte("good"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cache.Store("bucket", "key", source, objectstore.ObjectInfo{Size: 4}); err != nil {
		t.Fatal(err)
	}
	path, _, ok := cache.Get("bucket", "key", time.Minute)
	if !ok {
		t.Fatal("stored cache entry is missing")
	}
	if err := os.WriteFile(path, []byte("evil"), 0o600); err != nil {
		t.Fatal(err)
	}
	reopened, err := open(root, 100, func(int64) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := reopened.Get("bucket", "key", time.Minute); ok {
		t.Fatal("corrupt cache entry survived recovery")
	}
}
