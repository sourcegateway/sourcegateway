// Source Gateway <https://sourcegateway.dev>
// Copyright The Source Gateway Authors
// SPDX-License-Identifier: Apache-2.0

package objectcache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sourcegateway/sourcegateway/internal/objectstore"
)

// ErrNoSpace reports that a cache reservation cannot be satisfied.
var ErrNoSpace = errors.New("object cache has insufficient space")

// entry records the on-disk metadata for a cached object.
type entry struct {
	Bucket       string    `json:"bucket"`
	Key          string    `json:"key"`
	Size         int64     `json:"size"`
	ETag         string    `json:"etag"`
	Digest       string    `json:"digest"`
	LastModified time.Time `json:"lastModified"`
	Stored       time.Time `json:"stored"`
	Accessed     time.Time `json:"accessed"`
}

// Cache stores object data and metadata within a bounded local directory.
type Cache struct {
	root           string
	maximum        int64
	mu             sync.Mutex
	entries        map[string]*entry
	used           int64
	reserved       int64
	reservations   map[string]int64
	spaceAvailable func(int64) bool
}

// BucketStats summarizes cached data for one bucket.
type BucketStats struct {
	Bucket       string
	Entries      int
	Bytes        int64
	OldestStored time.Time
}

// Open opens or creates a cache rooted at root.
func Open(root string, maximum int64) (*Cache, error) {
	return open(root, maximum, nil)
}

// open opens a cache with an optional disk-space probe for tests.
func open(root string, maximum int64, spaceAvailable func(int64) bool) (*Cache, error) {
	if maximum <= 0 {
		return nil, ErrNoSpace
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	cache := &Cache{root: root, maximum: maximum, entries: make(map[string]*entry), reservations: make(map[string]int64)}
	if spaceAvailable == nil {
		cache.spaceAvailable = cache.diskAvailableLocked
	} else {
		cache.spaceAvailable = spaceAvailable
	}
	files, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	// Recover valid entries and discard remnants of interrupted writes.
	for _, file := range files {
		if filepath.Ext(file.Name()) == ".tmp" {
			_ = os.Remove(filepath.Join(root, file.Name()))
			continue
		}
		if filepath.Ext(file.Name()) == ".data" {
			identifier := file.Name()[:len(file.Name())-5]
			if _, statErr := os.Stat(filepath.Join(root, identifier+".json")); errors.Is(statErr, os.ErrNotExist) {
				_ = os.Remove(filepath.Join(root, file.Name()))
			}
			continue
		}
		if filepath.Ext(file.Name()) != ".json" {
			continue
		}
		identifier := file.Name()[:len(file.Name())-5]
		metadata, readErr := os.ReadFile(filepath.Join(root, file.Name())) // #nosec G304 -- name comes from this directory's ReadDir result.
		var loaded entry
		dataPath := filepath.Join(root, identifier+".data")
		dataInfo, statErr := os.Stat(dataPath)
		digest, digestErr := dataDigest(dataPath)
		if readErr != nil || json.Unmarshal(metadata, &loaded) != nil || statErr != nil || dataInfo.Size() != loaded.Size || identifier != cacheID(loaded.Bucket, loaded.Key) || loaded.Digest == "" || digestErr != nil || !strings.EqualFold(digest, loaded.Digest) {
			_ = os.Remove(filepath.Join(root, identifier+".json"))
			_ = os.Remove(filepath.Join(root, identifier+".data"))
			continue
		}
		cache.entries[identifier] = &loaded
		cache.used += loaded.Size
	}
	// Bring an oversized recovered cache back within its limits.
	for cache.used > cache.maximum || !cache.spaceAvailable(0) {
		identifier := cache.oldestLocked("", false)
		if identifier == "" {
			break
		}
		cache.removeLocked(identifier)
	}
	return cache, nil
}

// Get returns a fresh cached object and updates its access time.
func (c *Cache) Get(bucket, key string, ttl time.Duration) (string, objectstore.ObjectInfo, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	identifier := cacheID(bucket, key)
	item := c.entries[identifier]
	if item == nil || time.Since(item.Stored) > ttl {
		if item != nil {
			c.removeLocked(identifier)
		}
		return "", objectstore.ObjectInfo{}, false
	}
	item.Accessed = time.Now()
	_ = c.writeMetadata(identifier, item)
	return filepath.Join(c.root, identifier+".data"), objectstore.ObjectInfo{Key: key, Size: item.Size, ETag: item.ETag, LastModified: item.LastModified}, true
}

// Reserve secures cache capacity and returns an idempotent release function.
func (c *Cache) Reserve(bucket string, size, perResourceMaximum int64) (func(), error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if size < 0 || size > c.maximum || size > perResourceMaximum {
		return nil, ErrNoSpace
	}
	for c.used+c.reserved+size > c.maximum || c.bucketUsageLocked(bucket)+c.reservations[bucket]+size > perResourceMaximum || !c.spaceAvailable(size) {
		identifier := c.oldestLocked(bucket, c.bucketUsageLocked(bucket)+c.reservations[bucket]+size > perResourceMaximum)
		if identifier == "" {
			return nil, ErrNoSpace
		}
		c.removeLocked(identifier)
	}
	c.reserved += size
	c.reservations[bucket] += size
	released := false
	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if !released {
			c.reserved -= size
			c.reservations[bucket] -= size
			released = true
		}
	}, nil
}

// Store atomically copies an object into the cache.
func (c *Cache) Store(bucket, key, sourcePath string, info objectstore.ObjectInfo) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	identifier := cacheID(bucket, key)
	temporary, err := os.CreateTemp(c.root, identifier+"-*.tmp")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer func() { _ = os.Remove(temporaryName) }()
	source, err := os.Open(sourcePath) // #nosec G304 -- caller supplies an internal spool path, never request input.
	if err != nil {
		_ = temporary.Close()
		return err
	}
	digest := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(temporary, digest), source)
	closeSourceErr := source.Close()
	closeErr := temporary.Close()
	if copyErr != nil || closeSourceErr != nil || closeErr != nil || written != info.Size {
		return errors.New("cache object copy incomplete")
	}
	// Publish data before metadata so recovery never exposes a partial entry.
	c.removeLocked(identifier)
	if err := os.Rename(temporaryName, filepath.Join(c.root, identifier+".data")); err != nil {
		return err
	}
	now := time.Now()
	item := &entry{Bucket: bucket, Key: key, Size: info.Size, ETag: info.ETag, Digest: hex.EncodeToString(digest.Sum(nil)), LastModified: info.LastModified, Stored: now, Accessed: now}
	if err := c.writeMetadata(identifier, item); err != nil {
		_ = os.Remove(filepath.Join(c.root, identifier+".data"))
		return err
	}
	c.entries[identifier] = item
	c.used += item.Size
	return nil
}

// dataDigest computes the SHA-256 digest used to validate a recovered entry.
func dataDigest(path string) (string, error) {
	file, err := os.Open(path) // #nosec G304 -- recovery derives this path from its private cache directory.
	if err != nil {
		return "", err
	}
	digest := sha256.New()
	_, copyErr := io.Copy(digest, file)
	closeErr := file.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// Remove deletes an object from the cache.
func (c *Cache) Remove(bucket, key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.removeLocked(cacheID(bucket, key))
}

// RetainBuckets deletes cached objects whose logical bucket is no longer configured.
func (c *Cache) RetainBuckets(buckets map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for identifier, item := range c.entries {
		if !buckets[item.Bucket] {
			c.removeLocked(identifier)
		}
	}
}

// Stats returns cache usage grouped and sorted by bucket.
func (c *Cache) Stats() []BucketStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	byBucket := make(map[string]*BucketStats)
	for _, item := range c.entries {
		stats := byBucket[item.Bucket]
		if stats == nil {
			stats = &BucketStats{Bucket: item.Bucket}
			byBucket[item.Bucket] = stats
		}
		stats.Entries++
		stats.Bytes += item.Size
		if stats.OldestStored.IsZero() || item.Stored.Before(stats.OldestStored) {
			stats.OldestStored = item.Stored
		}
	}
	result := make([]BucketStats, 0, len(byBucket))
	for _, stats := range byBucket {
		result = append(result, *stats)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Bucket < result[j].Bucket })
	return result
}

// writeMetadata atomically replaces an entry's metadata file.
func (c *Cache) writeMetadata(identifier string, item *entry) error {
	encoded, err := json.Marshal(item)
	if err != nil {
		return err
	}
	temporary := filepath.Join(c.root, identifier+".json.tmp")
	if err := os.WriteFile(temporary, encoded, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, filepath.Join(c.root, identifier+".json"))
}

// removeLocked removes an entry while c.mu is held.
func (c *Cache) removeLocked(identifier string) {
	if item := c.entries[identifier]; item != nil {
		c.used -= item.Size
		delete(c.entries, identifier)
	}
	_ = os.Remove(filepath.Join(c.root, identifier+".json"))
	_ = os.Remove(filepath.Join(c.root, identifier+".data"))
}

// bucketUsageLocked totals a bucket's stored bytes while c.mu is held.
func (c *Cache) bucketUsageLocked(bucket string) int64 {
	var result int64
	for _, item := range c.entries {
		if item.Bucket == bucket {
			result += item.Size
		}
	}
	return result
}

// oldestLocked selects the least recently used eligible entry.
func (c *Cache) oldestLocked(bucket string, sameBucket bool) string {
	var oldest string
	var accessed time.Time
	for identifier, item := range c.entries {
		if (!sameBucket || item.Bucket == bucket) && (oldest == "" || item.Accessed.Before(accessed)) {
			oldest, accessed = identifier, item.Accessed
		}
	}
	return oldest
}

// diskAvailableLocked checks that a write preserves ten percent free space.
func (c *Cache) diskAvailableLocked(size int64) bool {
	var statistics syscall.Statfs_t
	if err := syscall.Statfs(c.root, &statistics); err != nil {
		return false
	}
	if statistics.Bsize <= 0 {
		return false
	}
	return hasDiskCapacity(statistics.Blocks, statistics.Bavail, uint64(statistics.Bsize), size)
}

// hasDiskCapacity reports whether a write leaves at least ten percent of a filesystem free.
func hasDiskCapacity(totalBlocks, availableBlocks, blockSize uint64, requestedBytes int64) bool {
	if blockSize == 0 || requestedBytes < 0 {
		return false
	}
	requestedBlocks := uint64(requestedBytes) / blockSize
	if uint64(requestedBytes)%blockSize != 0 {
		requestedBlocks++
	}
	if requestedBlocks > availableBlocks {
		return false
	}
	reservedBlocks := totalBlocks / 10
	if totalBlocks%10 != 0 {
		reservedBlocks++
	}
	return availableBlocks-requestedBlocks >= reservedBlocks
}

// cacheID derives a filesystem-safe identifier from an object location.
func cacheID(bucket, key string) string {
	digest := sha256.Sum256([]byte(bucket + "\x00" + key))
	return hex.EncodeToString(digest[:])
}
