// Source Gateway <https://sourcegateway.dev>
// Copyright The Source Gateway Authors
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5" // #nosec G501 -- S3 Content-MD5 is an integrity checksum, not a password hash.
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/storage"
	"github.com/aws/smithy-go"
	"github.com/sourcegateway/sourcegateway/internal/auth"
	"github.com/sourcegateway/sourcegateway/internal/objectcache"
	"github.com/sourcegateway/sourcegateway/internal/objectstore"
	"github.com/sourcegateway/sourcegateway/internal/policy"
	"github.com/sourcegateway/sourcegateway/internal/routing"
	"google.golang.org/api/googleapi"
)

var keyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

// s3Handler serves the supported S3-compatible object API.
type s3Handler struct {
	registry      *routing.Registry
	logger        *slog.Logger
	tmpDir        string
	cache         *objectcache.Cache
	maxObjectSize int64
	multipart     *multipartStore
	locks         *objectLocks
}

// objectLocks serializes operations that address the same authoritative object.
type objectLocks struct {
	mu    sync.Mutex
	locks map[string]*objectLock
}

// objectLock tracks one keyed mutex while callers are acquiring or holding it.
type objectLock struct {
	mu    sync.Mutex
	users int
}

// newObjectLocks creates an empty keyed lock set.
func newObjectLocks() *objectLocks {
	return &objectLocks{locks: make(map[string]*objectLock)}
}

// lockMany acquires unique object keys in stable order to avoid deadlocks.
func (l *objectLocks) lockMany(bucket string, keys []string) func() {
	identifiers := make([]string, 0, len(keys))
	seen := make(map[string]bool, len(keys))
	for _, key := range keys {
		identifier := bucket + "\x00" + key
		if !seen[identifier] {
			seen[identifier] = true
			identifiers = append(identifiers, identifier)
		}
	}
	sort.Strings(identifiers)
	releases := make([]func(), 0, len(identifiers))
	for _, identifier := range identifiers {
		releases = append(releases, l.lockIdentifier(identifier))
	}
	return func() {
		for index := len(releases) - 1; index >= 0; index-- {
			releases[index]()
		}
	}
}

// lockIdentifier acquires one internal lock and removes it after the last user.
func (l *objectLocks) lockIdentifier(identifier string) func() {
	l.mu.Lock()
	keyLock := l.locks[identifier]
	if keyLock == nil {
		keyLock = &objectLock{}
		l.locks[identifier] = keyLock
	}
	keyLock.users++
	l.mu.Unlock()
	keyLock.mu.Lock()
	return func() {
		keyLock.mu.Unlock()
		l.mu.Lock()
		keyLock.users--
		if keyLock.users == 0 {
			delete(l.locks, identifier)
		}
		l.mu.Unlock()
	}
}

// awsChunkedBody decodes S3's content encoding and captures its checksum trailers.
type awsChunkedBody struct {
	reader   io.Reader
	buffered *bufio.Reader
	trailers http.Header
	finished bool
}

// listAllMyBucketsResult is the S3 ListBuckets response envelope.
type listAllMyBucketsResult struct {
	XMLName xml.Name `xml:"ListAllMyBucketsResult"`
	Xmlns   string   `xml:"xmlns,attr"`
	Buckets struct {
		Buckets []bucketXML `xml:"Bucket"`
	} `xml:"Buckets"`
}

// bucketXML is one bucket in a ListBuckets response.
type bucketXML struct {
	Name         string `xml:"Name"`
	CreationDate string `xml:"CreationDate"`
}

// listObjectsResult is the S3 ListObjectsV2 response envelope.
type listObjectsResult struct {
	XMLName               xml.Name    `xml:"ListBucketResult"`
	Xmlns                 string      `xml:"xmlns,attr"`
	Name                  string      `xml:"Name"`
	Prefix                string      `xml:"Prefix"`
	Delimiter             string      `xml:"Delimiter,omitempty"`
	MaxKeys               int32       `xml:"MaxKeys"`
	KeyCount              int         `xml:"KeyCount"`
	IsTruncated           bool        `xml:"IsTruncated"`
	NextContinuationToken string      `xml:"NextContinuationToken,omitempty"`
	Contents              []objectXML `xml:"Contents"`
	CommonPrefixes        []prefixXML `xml:"CommonPrefixes"`
}

// objectXML is one object in a listing response.
type objectXML struct {
	Key          string `xml:"Key"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
	StorageClass string `xml:"StorageClass"`
}

// prefixXML is one common prefix in a delimited listing.
type prefixXML struct {
	Prefix string `xml:"Prefix"`
}

// deleteRequest is the bounded S3 multi-object delete payload.
type deleteRequest struct {
	Objects []struct {
		Key string `xml:"Key"`
	} `xml:"Object"`
	Quiet bool `xml:"Quiet"`
}

// deleteResult reports per-object outcomes from a multi-object delete.
type deleteResult struct {
	XMLName xml.Name `xml:"DeleteResult"`
	Xmlns   string   `xml:"xmlns,attr"`
	Deleted []struct {
		Key string `xml:"Key"`
	} `xml:"Deleted"`
	Errors []struct {
		Key     string `xml:"Key"`
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	} `xml:"Error"`
}

// ServeHTTP validates and dispatches an S3-compatible request.
func (h *s3Handler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	setTransferDeadline(response, request, 2*time.Minute)
	if request.URL.Path == "/" {
		if request.Method != http.MethodGet {
			writeS3Error(response, http.StatusMethodNotAllowed, "MethodNotAllowed")
			return
		}
		h.listBuckets(response, request)
		return
	}
	trimmed := strings.TrimPrefix(request.URL.EscapedPath(), "/")
	parts := strings.SplitN(trimmed, "/", 2)
	bucket, err := url.PathUnescape(parts[0])
	if err != nil {
		writeS3Error(response, http.StatusBadRequest, "InvalidURI")
		return
	}
	source := h.registry.Load().Objects[bucket]
	if source == nil {
		writeS3Error(response, http.StatusNotFound, "NoSuchBucket")
		return
	}
	release, acquired := source.Requests.Acquire()
	if !acquired {
		writeS3Error(response, http.StatusServiceUnavailable, "ServiceUnavailable")
		return
	}
	defer release()
	key := ""
	if len(parts) == 2 {
		key, err = url.PathUnescape(parts[1])
		if err != nil || !validKey(key) {
			writeS3Error(response, http.StatusBadRequest, "InvalidObjectName")
			return
		}
	}
	if key == "" {
		h.bucket(response, request, source)
		return
	}
	h.object(response, request, source, key)
}

// listBuckets returns only buckets visible to the caller.
func (h *s3Handler) listBuckets(response http.ResponseWriter, request *http.Request) {
	identity, _ := auth.IdentityFromContext(request.Context())
	names := make([]string, 0)
	for name, source := range h.registry.Load().Objects {
		release, acquired := source.Requests.Acquire()
		if !acquired {
			continue
		}
		allowed := h.authorize(source, identity, policy.Request{Operation: "bucket.list", Bucket: name})
		release()
		if allowed {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	result := listAllMyBucketsResult{Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/"}
	for _, name := range names {
		result.Buckets.Buckets = append(result.Buckets.Buckets, bucketXML{Name: name, CreationDate: time.Unix(0, 0).UTC().Format(time.RFC3339)})
	}
	writeXML(response, http.StatusOK, result)
}

// bucket dispatches operations that do not address an object key.
func (h *s3Handler) bucket(response http.ResponseWriter, request *http.Request, source *routing.ObjectSource) {
	switch {
	case request.Method == http.MethodHead:
		if !h.requestAllowed(response, request, source, policy.Request{Operation: "bucket.head", Bucket: source.Bucket}) {
			return
		}
		ctx, cancel := providerContext(request)
		defer cancel()
		if err := source.Provider.HeadBucket(ctx); err != nil {
			h.readProviderError(response, err)
			return
		}
		response.WriteHeader(http.StatusOK)
	case request.Method == http.MethodGet && request.URL.Query().Get("list-type") == "2":
		prefix, delimiter := request.URL.Query().Get("prefix"), request.URL.Query().Get("delimiter")
		if !validPrefix(prefix) || delimiter != "" && delimiter != "/" {
			writeS3Error(response, http.StatusBadRequest, "InvalidArgument")
			return
		}
		if !h.requestAllowed(response, request, source, policy.Request{Operation: "object.list", Bucket: source.Bucket, Key: request.URL.Query().Get("prefix")}) {
			return
		}
		h.listObjects(response, request, source)
	case request.Method == http.MethodPost && request.URL.Query().Has("delete"):
		h.deleteObjects(response, request, source)
	default:
		writeS3Error(response, http.StatusMethodNotAllowed, "MethodNotAllowed")
	}
}

// listObjects returns one bounded page of objects and common prefixes.
func (h *s3Handler) listObjects(response http.ResponseWriter, request *http.Request, source *routing.ObjectSource) {
	max := int64(1000)
	if raw := request.URL.Query().Get("max-keys"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || parsed < 0 || parsed > 1000 {
			writeS3Error(response, http.StatusBadRequest, "InvalidArgument")
			return
		}
		max = parsed
	}
	ctx, cancel := providerContext(request)
	defer cancel()
	result, err := source.Provider.List(ctx, request.URL.Query().Get("prefix"), request.URL.Query().Get("delimiter"), request.URL.Query().Get("continuation-token"), int32(max))
	if err != nil {
		h.readProviderError(response, err)
		return
	}
	output := listObjectsResult{
		Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/", Name: source.Bucket,
		Prefix: request.URL.Query().Get("prefix"), Delimiter: request.URL.Query().Get("delimiter"), MaxKeys: int32(max),
		IsTruncated: result.NextToken != "", NextContinuationToken: result.NextToken,
	}
	for _, object := range result.Objects {
		output.Contents = append(output.Contents, objectXML{Key: object.Key, LastModified: object.LastModified.UTC().Format(time.RFC3339), ETag: quoteETag(object.ETag), Size: object.Size, StorageClass: "STANDARD"})
	}
	for _, prefix := range result.Prefixes {
		output.CommonPrefixes = append(output.CommonPrefixes, prefixXML{Prefix: prefix})
	}
	output.KeyCount = len(output.Contents) + len(output.CommonPrefixes)
	writeXML(response, http.StatusOK, output)
}

// object authorizes and dispatches an operation on one key.
func (h *s3Handler) object(response http.ResponseWriter, request *http.Request, source *routing.ObjectSource, key string) {
	keys := []string{key}
	if request.Method == http.MethodPut {
		// A copy reads one object while writing another, so serialize both ends.
		if decoded, err := url.PathUnescape(request.Header.Get("X-Amz-Copy-Source")); err == nil {
			parts := strings.SplitN(strings.TrimPrefix(decoded, "/"), "/", 2)
			if len(parts) == 2 && parts[0] == source.Bucket && validKey(parts[1]) {
				keys = append(keys, parts[1])
			}
		}
	}
	release := h.locks.lockMany(source.Bucket, keys)
	defer release()
	query := request.URL.Query()
	switch {
	case request.Method == http.MethodPost && query.Has("uploads"):
		h.createMultipartUpload(response, request, source, key)
		return
	case request.Method == http.MethodPut && query.Has("uploadId") && query.Has("partNumber"):
		h.uploadPart(response, request, source, key)
		return
	case request.Method == http.MethodPost && query.Has("uploadId"):
		h.completeMultipartUpload(response, request, source, key)
		return
	case request.Method == http.MethodDelete && query.Has("uploadId"):
		h.abortMultipartUpload(response, request, source, key)
		return
	}
	operation := map[string]string{http.MethodHead: "object.read", http.MethodGet: "object.read", http.MethodPut: "object.write", http.MethodDelete: "object.delete"}[request.Method]
	if operation == "" {
		writeS3Error(response, http.StatusMethodNotAllowed, "MethodNotAllowed")
		return
	}
	if request.Method == http.MethodPut && request.Header.Get("X-Amz-Copy-Source") != "" {
		h.copyObject(response, request, source, key, request.Header.Get("X-Amz-Copy-Source"))
		return
	}
	if request.Method != http.MethodPut && !h.requestAllowed(response, request, source, policy.Request{Operation: operation, Bucket: source.Bucket, Key: key, Size: request.ContentLength}) {
		return
	}
	switch request.Method {
	case http.MethodHead:
		h.headObject(response, request, source, key)
	case http.MethodGet:
		h.getObject(response, request, source, key)
	case http.MethodPut:
		h.putObject(response, request, source, key)
	case http.MethodDelete:
		ctx, cancel := providerContext(request)
		defer cancel()
		if err := source.Provider.Delete(ctx, key); err != nil && !providerNotFound(err) {
			h.cache.Remove(source.Bucket, key)
			h.mutationProviderError(response, err)
			return
		}
		h.cache.Remove(source.Bucket, key)
		response.WriteHeader(http.StatusNoContent)
	}
}

// headObject returns metadata for an object without its body.
func (h *s3Handler) headObject(response http.ResponseWriter, request *http.Request, source *routing.ObjectSource, key string) {
	ctx, cancel := providerContext(request)
	defer cancel()
	info, err := source.Provider.Head(ctx, key)
	if err != nil {
		h.readProviderError(response, err)
		return
	}
	setObjectHeaders(response, info)
	response.WriteHeader(http.StatusOK)
}

// getObject serves a complete object or a normalized single byte range.
func (h *s3Handler) getObject(response http.ResponseWriter, request *http.Request, source *routing.ObjectSource, key string) {
	byteRange := request.Header.Get("Range")
	contentRange := ""
	if byteRange != "" {
		ctx, cancel := providerContext(request)
		info, err := source.Provider.Head(ctx, key)
		cancel()
		if err != nil {
			h.readProviderError(response, err)
			return
		}
		var rangeErr error
		byteRange, contentRange, rangeErr = normalizeByteRange(byteRange, info.Size)
		if rangeErr != nil {
			response.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(info.Size, 10))
			writeS3Error(response, http.StatusRequestedRangeNotSatisfiable, "InvalidRange")
			return
		}
	}
	if byteRange == "" {
		// Complete objects may be served directly from the local verified cache.
		if cachedPath, info, ok := h.cache.Get(source.Bucket, key, source.TTL); ok {
			file, err := os.Open(cachedPath) // #nosec G304,G703 -- cache returns an internally derived path.
			if err == nil {
				defer func() { _ = file.Close() }()
				setObjectHeaders(response, info)
				_, _ = io.Copy(response, file)
				return
			}
		}
	}
	ctx, cancel := transferContext(request)
	defer cancel()
	body, info, err := source.Provider.Get(ctx, key, byteRange)
	if err != nil {
		h.readProviderError(response, err)
		return
	}
	defer func() { _ = body.Close() }()
	if contentRange != "" && info.ContentRange == "" {
		info.ContentRange = contentRange
	}
	// Stage provider output before committing headers or admitting it to the cache.
	temporary, err := os.CreateTemp(h.tmpDir, "get-*")
	if err != nil {
		writeS3Error(response, http.StatusInsufficientStorage, "InsufficientStorage")
		return
	}
	temporaryName := temporary.Name()
	defer func() { _ = os.Remove(temporaryName) }()
	written, copyErr := io.Copy(temporary, body)
	closeErr := temporary.Close()
	if copyErr != nil {
		h.readProviderError(response, copyErr)
		return
	}
	if closeErr != nil || written != info.Size {
		writeS3Error(response, http.StatusServiceUnavailable, "ServiceUnavailable")
		return
	}
	if byteRange == "" {
		if release, reserveErr := h.cache.Reserve(source.Bucket, info.Size, source.Capacity); reserveErr == nil {
			_ = h.cache.Store(source.Bucket, key, temporaryName, info)
			release()
		}
	}
	file, err := os.Open(temporaryName) // #nosec G304 -- path was returned by os.CreateTemp above.
	if err != nil {
		writeS3Error(response, http.StatusInternalServerError, "InternalError")
		return
	}
	defer func() { _ = file.Close() }()
	setObjectHeaders(response, info)
	if byteRange != "" {
		response.WriteHeader(http.StatusPartialContent)
	}
	_, _ = io.Copy(response, file)
}

// putObject validates and writes one request to its authoritative provider.
func (h *s3Handler) putObject(response http.ResponseWriter, request *http.Request, source *routing.ObjectSource, key string) {
	var body io.Reader
	var size int64
	var stagedPath string
	if h.mustStageUpload(request) {
		// Complete all size, framing, and checksum checks before contacting upstream.
		path, written, _, ok := h.stageUpload(response, request)
		if !ok {
			return
		}
		stagedPath, size = path, written
		defer func() { _ = os.Remove(stagedPath) }() // #nosec G703 -- stageUpload returns an internally created temporary path.
		file, err := os.Open(stagedPath)             // #nosec G304,G703 -- stageUpload returns an internally created temporary path.
		if err != nil {
			writeS3Error(response, http.StatusInternalServerError, "InternalError")
			return
		}
		defer func() { _ = file.Close() }()
		body = file
	} else {
		// net/http framing makes an ordinary Content-Length authoritative for this request body.
		stream, expected, _, ok := h.uploadBody(response, request)
		if !ok {
			return
		}
		body, size = stream, expected
	}
	if !h.requestAllowed(response, request, source, policy.Request{Operation: "object.write", Bucket: source.Bucket, Key: key, Size: size}) {
		return
	}
	// A direct stream may also fill a temporary cache candidate without delaying admission failures.
	var cacheFile *os.File
	var cacheRelease func()
	if stagedPath == "" {
		if release, reserveErr := h.cache.Reserve(source.Bucket, size, source.Capacity); reserveErr == nil {
			file, createErr := os.CreateTemp(h.tmpDir, "put-cache-*")
			if createErr == nil {
				cacheFile, cacheRelease = file, release
				body = io.TeeReader(body, cacheFile)
				defer func() { _ = os.Remove(cacheFile.Name()) }()
				defer cacheRelease()
			} else {
				release()
			}
		}
	}
	ctx, cancel := transferContext(request)
	defer cancel()
	stopClose := context.AfterFunc(ctx, func() { _ = request.Body.Close() })
	defer stopClose()
	info, err := source.Provider.Put(ctx, key, body, size, preconditions(request))
	if err != nil {
		if cacheFile != nil {
			_ = cacheFile.Close()
		}
		h.cache.Remove(source.Bucket, key)
		h.mutationProviderError(response, err)
		return
	}
	info.Size = size
	// Cache admission remains best-effort after the authoritative write succeeds.
	if cacheFile != nil {
		if cacheFile.Close() != nil || h.cache.Store(source.Bucket, key, cacheFile.Name(), info) != nil {
			h.cache.Remove(source.Bucket, key)
		}
	} else if stagedPath == "" {
		h.cache.Remove(source.Bucket, key)
	} else if release, reserveErr := h.cache.Reserve(source.Bucket, size, source.Capacity); reserveErr != nil {
		h.cache.Remove(source.Bucket, key)
	} else {
		if storeErr := h.cache.Store(source.Bucket, key, stagedPath, info); storeErr != nil {
			h.cache.Remove(source.Bucket, key)
		}
		release()
	}
	response.Header().Set("ETag", quoteETag(info.ETag))
	response.Header().Set("Last-Modified", info.LastModified.UTC().Format(http.TimeFormat))
	response.WriteHeader(http.StatusOK)
}

// mustStageUpload reports whether validation requires consuming the complete body before the upstream write.
func (h *s3Handler) mustStageUpload(request *http.Request) bool {
	hasher, _ := requestChecksum(request)
	if h.maxObjectSize > 0 || request.ContentLength < 0 || hasher != nil {
		return true
	}
	// The decoded length is an untrusted application header rather than HTTP framing.
	return strings.EqualFold(request.Header.Get("Content-Encoding"), "aws-chunked")
}

// stageUpload validates and spools one bounded streaming request.
func (h *s3Handler) stageUpload(response http.ResponseWriter, request *http.Request) (string, int64, string, bool) {
	body, expectedLength, encodedBody, ok := h.uploadBody(response, request)
	if !ok {
		return "", 0, "", false
	}
	hasher, checksumHeader := requestChecksum(request)
	if hasher == nil {
		hasher = sha256.New()
	}
	etagHasher := md5.New() // #nosec G401 -- S3 multipart ETags use MD5.
	ctx, cancel := transferContext(request)
	defer cancel()
	stopClose := context.AfterFunc(ctx, func() { _ = request.Body.Close() })
	defer stopClose()
	file, err := os.CreateTemp(h.tmpDir, "put-*")
	if err != nil {
		writeS3Error(response, http.StatusInsufficientStorage, "InsufficientStorage")
		return "", 0, "", false
	}
	name := file.Name()
	limitedBody := body
	if h.maxObjectSize > 0 {
		limitedBody = io.LimitReader(body, h.maxObjectSize+1)
	}
	// Multipart parts remain staged so completion can assemble them atomically.
	written, copyErr := io.Copy(io.MultiWriter(file, hasher, etagHasher), limitedBody)
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil || expectedLength >= 0 && written != expectedLength {
		_ = os.Remove(name)
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			writeS3Error(response, http.StatusGatewayTimeout, "RequestTimeout")
			return "", 0, "", false
		}
		writeS3Error(response, http.StatusBadRequest, "IncompleteBody")
		return "", 0, "", false
	}
	if h.maxObjectSize > 0 && written > h.maxObjectSize {
		_ = os.Remove(name)
		writeS3Error(response, http.StatusRequestEntityTooLarge, "EntityTooLarge")
		return "", 0, "", false
	}
	checksum := checksumValue(request, checksumHeader)
	if encodedBody != nil && checksum == "" {
		checksum = encodedBody.trailers.Get(checksumHeader)
	}
	if checksumHeader != "" && !validChecksum(hasher, checksum) {
		_ = os.Remove(name)
		writeS3Error(response, http.StatusBadRequest, "BadDigest")
		return "", 0, "", false
	}
	return name, written, hex.EncodeToString(etagHasher.Sum(nil)), true
}

// uploadBody validates request framing and returns its decoded object stream.
func (h *s3Handler) uploadBody(response http.ResponseWriter, request *http.Request) (io.Reader, int64, *awsChunkedBody, bool) {
	chunked := len(request.TransferEncoding) == 1 && strings.EqualFold(request.TransferEncoding[0], "chunked")
	expectedLength := request.ContentLength
	body := io.Reader(request.Body)
	var encodedBody *awsChunkedBody
	contentEncoding := request.Header.Get("Content-Encoding")
	if strings.EqualFold(contentEncoding, "aws-chunked") {
		decodedLength, err := strconv.ParseInt(request.Header.Get("X-Amz-Decoded-Content-Length"), 10, 64)
		if err != nil || decodedLength < 0 {
			writeS3Error(response, http.StatusBadRequest, "InvalidRequest")
			return nil, 0, nil, false
		}
		expectedLength = decodedLength
		encodedBody = newAWSChunkedBody(request.Body)
		body = encodedBody
	} else if contentEncoding != "" {
		writeS3Error(response, http.StatusBadRequest, "InvalidRequest")
		return nil, 0, nil, false
	}
	if h.maxObjectSize > 0 && expectedLength > h.maxObjectSize || len(request.TransferEncoding) != 0 && !chunked || request.Header.Get("Expect") != "" && !strings.EqualFold(request.Header.Get("Expect"), "100-continue") || !validChecksumTrailers(request) {
		writeS3Error(response, http.StatusBadRequest, "InvalidRequest")
		return nil, 0, nil, false
	}
	return body, expectedLength, encodedBody, true
}

// newAWSChunkedBody prepares a decoder over S3's HTTP-like content encoding.
func newAWSChunkedBody(body io.Reader) *awsChunkedBody {
	buffered := bufio.NewReader(body)
	return &awsChunkedBody{reader: httputil.NewChunkedReader(buffered), buffered: buffered, trailers: make(http.Header)}
}

// Read returns decoded payload bytes and parses trailers at the final chunk.
func (r *awsChunkedBody) Read(buffer []byte) (int, error) {
	read, err := r.reader.Read(buffer)
	if !errors.Is(err, io.EOF) || r.finished {
		return read, err
	}
	r.finished = true
	if trailerErr := r.readTrailers(); trailerErr != nil {
		return read, trailerErr
	}
	return read, io.EOF
}

// readTrailers accepts a small set of bounded S3 checksum trailers.
func (r *awsChunkedBody) readTrailers() error {
	consumed := 0
	for {
		line, err := r.buffered.ReadString('\n')
		consumed += len(line)
		if err != nil || consumed > 16<<10 || !strings.HasSuffix(line, "\r\n") {
			return errors.New("invalid aws-chunked trailer")
		}
		line = strings.TrimSuffix(line, "\r\n")
		if line == "" {
			return nil
		}
		name, value, found := strings.Cut(line, ":")
		name = http.CanonicalHeaderKey(strings.TrimSpace(name))
		if !found || checksumHasher(name) == nil || r.trailers.Get(name) != "" {
			return errors.New("unsupported aws-chunked trailer")
		}
		r.trailers.Set(name, strings.TrimSpace(value))
	}
}

// copyObject authorizes both sides before asking the provider for a server-side copy.
func (h *s3Handler) copyObject(response http.ResponseWriter, request *http.Request, source *routing.ObjectSource, destination, rawSource string) {
	// Parse and authorize the source before exposing its metadata to destination policy.
	decoded, err := url.PathUnescape(rawSource)
	if err != nil {
		writeS3Error(response, http.StatusBadRequest, "InvalidArgument")
		return
	}
	parts := strings.SplitN(strings.TrimPrefix(decoded, "/"), "/", 2)
	if len(parts) != 2 || parts[0] != source.Bucket || !validKey(parts[1]) {
		writeS3Error(response, http.StatusBadRequest, "InvalidRequest")
		return
	}
	identity, _ := auth.IdentityFromContext(request.Context())
	if !h.authorize(source, identity, policy.Request{Operation: "object.read", Bucket: source.Bucket, Key: parts[1]}) {
		writeS3Error(response, http.StatusForbidden, "AccessDenied")
		return
	}
	headContext, headCancel := providerContext(request)
	info, err := source.Provider.Head(headContext, parts[1])
	headCancel()
	if err != nil {
		h.readProviderError(response, err)
		return
	}
	if h.maxObjectSize > 0 && info.Size > h.maxObjectSize {
		writeS3Error(response, http.StatusBadRequest, "EntityTooLarge")
		return
	}
	if !h.authorize(source, identity, policy.Request{Operation: "object.write", Bucket: source.Bucket, Key: destination, Size: info.Size}) {
		writeS3Error(response, http.StatusForbidden, "AccessDenied")
		return
	}
	ctx, cancel := transferContext(request)
	defer cancel()
	info, err = source.Provider.Copy(ctx, parts[1], destination, preconditions(request))
	if err != nil {
		h.cache.Remove(source.Bucket, destination)
		h.mutationProviderError(response, err)
		return
	}
	// A server-side copy has no local body to cache, so evict the old destination.
	h.cache.Remove(source.Bucket, destination)
	writeXML(response, http.StatusOK, struct {
		XMLName      xml.Name `xml:"CopyObjectResult"`
		LastModified string   `xml:"LastModified"`
		ETag         string   `xml:"ETag"`
	}{LastModified: info.LastModified.UTC().Format(time.RFC3339), ETag: quoteETag(info.ETag)})
}

// deleteObjects validates and authorizes a complete multi-object delete before mutation.
func (h *s3Handler) deleteObjects(response http.ResponseWriter, request *http.Request, source *routing.ObjectSource) {
	if request.ContentLength < 0 || request.ContentLength > 1<<20 {
		writeS3Error(response, http.StatusBadRequest, "InvalidRequest")
		return
	}
	hasher, checksumHeader := requestChecksum(request)
	if hasher == nil {
		writeS3Error(response, http.StatusBadRequest, "InvalidDigest")
		return
	}
	ctx, cancel := providerContext(request)
	stopClose := context.AfterFunc(ctx, func() { _ = request.Body.Close() })
	body, err := io.ReadAll(io.TeeReader(io.LimitReader(request.Body, request.ContentLength+1), hasher))
	stopClose()
	cancel()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		writeS3Error(response, http.StatusGatewayTimeout, "RequestTimeout")
		return
	}
	if err != nil || int64(len(body)) != request.ContentLength {
		writeS3Error(response, http.StatusBadRequest, "IncompleteBody")
		return
	}
	if !validChecksum(hasher, checksumValue(request, checksumHeader)) {
		writeS3Error(response, http.StatusBadRequest, "BadDigest")
		return
	}
	var input deleteRequest
	decoder := xml.NewDecoder(bytes.NewReader(body))
	decodeErr := decoder.Decode(&input)
	var trailing any
	trailingErr := decoder.Decode(&trailing)
	if decodeErr != nil || !errors.Is(trailingErr, io.EOF) || len(input.Objects) > 1000 {
		writeS3Error(response, http.StatusBadRequest, "MalformedXML")
		return
	}
	identity, _ := auth.IdentityFromContext(request.Context())
	// Reject the entire batch unless every requested key is authorized.
	keys := make([]string, 0, len(input.Objects))
	for _, object := range input.Objects {
		if !validKey(object.Key) || !h.authorize(source, identity, policy.Request{Operation: "object.delete", Bucket: source.Bucket, Key: object.Key}) {
			writeS3Error(response, http.StatusForbidden, "AccessDenied")
			return
		}
		keys = append(keys, object.Key)
	}
	release := h.locks.lockMany(source.Bucket, keys)
	defer release()
	result := deleteResult{Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/"}
	committed := false
	for _, object := range input.Objects {
		ctx, cancel := providerContext(request)
		err := source.Provider.Delete(ctx, object.Key)
		cancel()
		if err != nil && !providerNotFound(err) {
			h.cache.Remove(source.Bucket, object.Key)
			if mutationOutcomeUnknown(err) {
				h.logger.Warn("provider request failed", "class", "OperationOutcomeUnknown")
				result.Errors = append(result.Errors, struct {
					Key     string `xml:"Key"`
					Code    string `xml:"Code"`
					Message string `xml:"Message"`
				}{Key: object.Key, Code: "OperationOutcomeUnknown", Message: "The upstream may have applied this mutation."})
				continue
			}
			if !committed {
				h.providerError(response, err)
				return
			}
			h.logger.Warn("provider request failed", "class", "ServiceUnavailable")
			result.Errors = append(result.Errors, struct {
				Key     string `xml:"Key"`
				Code    string `xml:"Code"`
				Message string `xml:"Message"`
			}{Key: object.Key, Code: "ServiceUnavailable", Message: http.StatusText(http.StatusServiceUnavailable)})
			continue
		}
		committed = true
		h.cache.Remove(source.Bucket, object.Key)
		if !input.Quiet {
			result.Deleted = append(result.Deleted, struct {
				Key string `xml:"Key"`
			}{Key: object.Key})
		}
	}
	writeXML(response, http.StatusOK, result)
}

// requestChecksum selects the first supported S3 integrity checksum.
func requestChecksum(request *http.Request) (hash.Hash, string) {
	for _, header := range []string{"X-Amz-Checksum-Sha256", "Content-Md5", "X-Amz-Checksum-Crc32", "X-Amz-Checksum-Crc32c"} {
		if request.Header.Get(header) != "" {
			return checksumHasher(header), header
		}
		if _, advertised := request.Trailer[http.CanonicalHeaderKey(header)]; advertised {
			return checksumHasher(header), header
		}
		for _, advertised := range strings.Split(request.Header.Get("X-Amz-Trailer"), ",") {
			if strings.EqualFold(strings.TrimSpace(advertised), header) {
				return checksumHasher(header), header
			}
		}
	}
	return nil, ""
}

// checksumHasher returns the digest implementation for a supported S3 header.
func checksumHasher(header string) hash.Hash {
	switch http.CanonicalHeaderKey(header) {
	case "X-Amz-Checksum-Sha256":
		return sha256.New()
	case "Content-Md5":
		return md5.New() // #nosec G401 -- S3 protocol integrity checksum.
	case "X-Amz-Checksum-Crc32":
		return crc32.NewIEEE()
	case "X-Amz-Checksum-Crc32c":
		return crc32.New(crc32.MakeTable(crc32.Castagnoli))
	default:
		return nil
	}
}

// checksumValue returns a checksum supplied in headers or HTTP trailers.
func checksumValue(request *http.Request, header string) string {
	if value := request.Header.Get(header); value != "" {
		return value
	}
	return request.Trailer.Get(header)
}

// validChecksumTrailers rejects trailers unrelated to supported integrity checks.
func validChecksumTrailers(request *http.Request) bool {
	for header := range request.Trailer {
		if checksumHasher(header) == nil {
			return false
		}
	}
	for _, header := range strings.Split(request.Header.Get("X-Amz-Trailer"), ",") {
		if header != "" && checksumHasher(strings.TrimSpace(header)) == nil {
			return false
		}
	}
	return true
}

// validChecksum compares a computed digest with its base64 wire value.
func validChecksum(hasher hash.Hash, encoded string) bool {
	want, err := base64.StdEncoding.DecodeString(encoded)
	return err == nil && bytes.Equal(want, hasher.Sum(nil))
}

// requestAllowed authorizes a request and writes the S3 denial response.
func (h *s3Handler) requestAllowed(response http.ResponseWriter, request *http.Request, source *routing.ObjectSource, policyRequest policy.Request) bool {
	identity, _ := auth.IdentityFromContext(request.Context())
	if h.authorize(source, identity, policyRequest) {
		return true
	}
	writeS3Error(response, http.StatusForbidden, "AccessDenied")
	return false
}

// authorize evaluates and audits an object policy without logging object topology.
func (h *s3Handler) authorize(source *routing.ObjectSource, identity auth.Identity, request policy.Request) bool {
	allowed, matched, err := source.Policy.Authorize(policy.ServiceAccount{Namespace: identity.Namespace, Name: identity.Name, UID: identity.UID}, request)
	reason := "no matching rule"
	if err != nil {
		reason = "evaluation failure"
	} else if allowed {
		reason = "matched"
	}
	h.logger.Info("authorization decision", "service_account_namespace", identity.Namespace, "service_account_name", identity.Name,
		"service_account_uid", identity.UID, "source_namespace", source.Namespace, "source_name", source.Name, "operation", request.Operation,
		"policy_revision", source.Policy.Revision(), "allowed", allowed, "reason", reason, "matched_rules", matched)
	return allowed && err == nil
}

// readProviderError preserves read timeouts while redacting other provider errors.
func (h *s3Handler) readProviderError(response http.ResponseWriter, err error) {
	if errors.Is(err, context.DeadlineExceeded) {
		writeS3Error(response, http.StatusGatewayTimeout, "RequestTimeout")
		return
	}
	h.providerError(response, err)
}

// providerError maps provider failures to a small redacted S3 error vocabulary.
func (h *s3Handler) providerError(response http.ResponseWriter, err error) {
	status, code := http.StatusServiceUnavailable, "ServiceUnavailable"
	if providerNotFound(err) {
		status, code = http.StatusNotFound, "NoSuchKey"
	} else if errors.Is(err, objectstore.ErrPreconditionFailed) {
		status, code = http.StatusPreconditionFailed, "PreconditionFailed"
	}
	var apiError smithy.APIError
	if errors.As(err, &apiError) {
		switch apiError.ErrorCode() {
		case "NoSuchKey", "NotFound", "404":
			status, code = http.StatusNotFound, "NoSuchKey"
		case "PreconditionFailed", "412":
			status, code = http.StatusPreconditionFailed, "PreconditionFailed"
		}
	}
	var googleError *googleapi.Error
	if errors.As(err, &googleError) {
		switch googleError.Code {
		case http.StatusNotFound:
			status, code = http.StatusNotFound, "NoSuchKey"
		case http.StatusPreconditionFailed:
			status, code = http.StatusPreconditionFailed, "PreconditionFailed"
		}
	}
	h.logger.Warn("provider request failed", "class", code)
	writeS3Error(response, status, code)
}

// mutationProviderError distinguishes definitive rejection from an indeterminate commit.
func (h *s3Handler) mutationProviderError(response http.ResponseWriter, err error) {
	if !mutationOutcomeUnknown(err) {
		h.providerError(response, err)
		return
	}
	h.logger.Warn("provider request failed", "class", "OperationOutcomeUnknown")
	writeXML(response, http.StatusConflict, struct {
		XMLName xml.Name `xml:"Error"`
		Code    string   `xml:"Code"`
		Message string   `xml:"Message"`
	}{Code: "OperationOutcomeUnknown", Message: "The upstream may have applied this mutation; inspect authoritative state before retrying."})
}

// mutationOutcomeUnknown reports whether an error occurred after mutation dispatch without a definitive rejection.
func mutationOutcomeUnknown(err error) bool {
	if errors.Is(err, objectstore.ErrMutationOutcomeUnknown) {
		return true
	}
	if errors.Is(err, objectstore.ErrPreconditionFailed) || providerNotFound(err) {
		return false
	}
	var apiError smithy.APIError
	if errors.As(err, &apiError) && (apiError.ErrorCode() == "PreconditionFailed" || apiError.ErrorCode() == "412" || apiError.ErrorCode() == "NoSuchKey" || apiError.ErrorCode() == "NotFound" || apiError.ErrorCode() == "404") {
		return false
	}
	var googleError *googleapi.Error
	return !errors.As(err, &googleError) || googleError.Code != http.StatusPreconditionFailed && googleError.Code != http.StatusNotFound
}

// providerNotFound recognizes object absence across supported providers.
func providerNotFound(err error) bool {
	if errors.Is(err, storage.ErrObjectNotExist) {
		return true
	}
	var apiError smithy.APIError
	if errors.As(err, &apiError) && (apiError.ErrorCode() == "NoSuchKey" || apiError.ErrorCode() == "NotFound" || apiError.ErrorCode() == "404") {
		return true
	}
	var googleError *googleapi.Error
	return errors.As(err, &googleError) && googleError.Code == http.StatusNotFound
}

// validKey accepts portable object keys without empty or relative path segments.
func validKey(key string) bool {
	if len(key) == 0 || len(key) > 1024 || !keyPattern.MatchString(key) {
		return false
	}
	for _, segment := range strings.Split(key, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

// validPrefix accepts an empty prefix or a valid key prefix with an optional slash.
func validPrefix(prefix string) bool {
	return prefix == "" || validKey(strings.TrimSuffix(prefix, "/"))
}

// preconditions translates supported conditional headers into provider options.
func preconditions(request *http.Request) objectstore.PutOptions {
	return objectstore.PutOptions{
		ContentType: request.Header.Get("Content-Type"), IfMatch: request.Header.Get("If-Match"), IfNoneMatch: request.Header.Get("If-None-Match"),
		CopySourceIfMatch: request.Header.Get("X-Amz-Copy-Source-If-Match"), CopySourceIfNoneMatch: request.Header.Get("X-Amz-Copy-Source-If-None-Match"),
	}
}

// setObjectHeaders writes provider-neutral S3 object metadata.
func setObjectHeaders(response http.ResponseWriter, info objectstore.ObjectInfo) {
	response.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
	response.Header().Set("ETag", quoteETag(info.ETag))
	response.Header().Set("Last-Modified", info.LastModified.UTC().Format(http.TimeFormat))
	response.Header().Set("Accept-Ranges", "bytes")
	if info.ContentRange != "" {
		response.Header().Set("Content-Range", info.ContentRange)
	}
}

// normalizeByteRange resolves one HTTP byte range against an object size.
func normalizeByteRange(value string, size int64) (string, string, error) {
	if size <= 0 || !strings.HasPrefix(value, "bytes=") || strings.Contains(value, ",") {
		return "", "", errors.New("invalid byte range")
	}
	parts := strings.Split(strings.TrimPrefix(value, "bytes="), "-")
	if len(parts) != 2 || parts[0] == "" && parts[1] == "" {
		return "", "", errors.New("invalid byte range")
	}
	var start, end int64
	var err error
	if parts[0] == "" {
		length, parseErr := strconv.ParseInt(parts[1], 10, 64)
		if parseErr != nil || length <= 0 {
			return "", "", errors.New("invalid byte range")
		}
		if length > size {
			length = size
		}
		start, end = size-length, size-1
	} else {
		start, err = strconv.ParseInt(parts[0], 10, 64)
		if err != nil || start < 0 || start >= size {
			return "", "", errors.New("invalid byte range")
		}
		if parts[1] == "" {
			end = size - 1
		} else {
			end, err = strconv.ParseInt(parts[1], 10, 64)
			if err != nil || end < start {
				return "", "", errors.New("invalid byte range")
			}
			if end >= size {
				end = size - 1
			}
		}
	}
	normalized := fmt.Sprintf("bytes=%d-%d", start, end)
	contentRange := fmt.Sprintf("bytes %d-%d/%d", start, end, size)
	return normalized, contentRange, nil
}

// quoteETag returns an ETag with exactly one surrounding pair of quotes.
func quoteETag(etag string) string {
	return `"` + strings.Trim(etag, `"`) + `"`
}

// writeXML writes an S3 XML response with its declaration.
func writeXML(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/xml")
	response.WriteHeader(status)
	_, _ = response.Write([]byte(xml.Header))
	_ = xml.NewEncoder(response).Encode(value)
}

// writeS3Error writes a redacted S3 error response.
func writeS3Error(response http.ResponseWriter, status int, code string) {
	writeXML(response, status, struct {
		XMLName xml.Name `xml:"Error"`
		Code    string   `xml:"Code"`
		Message string   `xml:"Message"`
	}{Code: code, Message: http.StatusText(status)})
}

// providerContext bounds short provider metadata operations.
func providerContext(request *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(request.Context(), 30*time.Second)
}

// transferContext bounds provider operations that move object data.
func transferContext(request *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(request.Context(), 2*time.Minute)
}
