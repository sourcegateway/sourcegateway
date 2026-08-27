// Source Gateway <https://sourcegateway.dev>
// Copyright The Source Gateway Authors
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sourcegateway/sourcegateway/internal/policy"
	"github.com/sourcegateway/sourcegateway/internal/routing"
)

const (
	maximumMultipartUploads = 128
	maximumMultipartParts   = 10_000
	minimumMultipartPart    = 5 * 1024 * 1024
	multipartLifetime       = 2 * time.Hour
)

var (
	errInvalidPart      = errors.New("multipart upload contains an invalid part")
	errInvalidPartOrder = errors.New("multipart upload parts are not in ascending order")
	errNoSuchUpload     = errors.New("multipart upload does not exist")
	errUploadBusy       = errors.New("multipart upload is being completed")
	errUploadLimit      = errors.New("too many multipart uploads")
)

// multipartStore owns staged multipart uploads for this process.
type multipartStore struct {
	mu      sync.Mutex
	uploads map[string]*multipartUpload
}

// multipartUpload records the destination and staged parts for one upload.
type multipartUpload struct {
	bucket      string
	key         string
	contentType string
	created     time.Time
	parts       map[int]multipartPart
	completing  bool
}

// multipartPart records one staged part.
type multipartPart struct {
	path string
	size int64
	etag string
}

// completedMultipartUpload is the S3 completion request envelope.
type completedMultipartUpload struct {
	Parts []completedMultipartPart `xml:"Part"`
}

// completedMultipartPart identifies one requested staged part.
type completedMultipartPart struct {
	ETag       string `xml:"ETag"`
	PartNumber int    `xml:"PartNumber"`
}

// initiateMultipartUploadResult is the S3 initiation response.
type initiateMultipartUploadResult struct {
	XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	UploadID string   `xml:"UploadId"`
}

// completeMultipartUploadResult is the S3 completion response.
type completeMultipartUploadResult struct {
	XMLName xml.Name `xml:"CompleteMultipartUploadResult"`
	Bucket  string   `xml:"Bucket"`
	Key     string   `xml:"Key"`
	ETag    string   `xml:"ETag"`
}

// multipartReader streams staged part files without opening them all at once.
type multipartReader struct {
	parts    []multipartPart
	index    int
	current  *os.File
	position int64
	total    int64
}

// newMultipartStore creates an empty in-memory upload registry.
func newMultipartStore() *multipartStore {
	return &multipartStore{uploads: make(map[string]*multipartUpload)}
}

// create registers a new upload and removes abandoned uploads.
func (s *multipartStore) create(bucket, key, contentType string) (string, error) {
	s.mu.Lock()
	expired := s.expiredLocked(time.Now())
	if len(s.uploads) >= maximumMultipartUploads {
		s.mu.Unlock()
		cleanupMultipartUploads(expired)
		return "", errUploadLimit
	}
	identifier, err := multipartUploadID()
	if err != nil {
		s.mu.Unlock()
		cleanupMultipartUploads(expired)
		return "", err
	}
	s.uploads[identifier] = &multipartUpload{bucket: bucket, key: key, contentType: contentType, created: time.Now(), parts: make(map[int]multipartPart)}
	s.mu.Unlock()
	cleanupMultipartUploads(expired)
	return identifier, nil
}

// storePart replaces one staged part while preserving upload consistency.
func (s *multipartStore) storePart(identifier, bucket, key string, number int, part multipartPart) error {
	s.mu.Lock()
	upload := s.uploads[identifier]
	if upload == nil || upload.bucket != bucket || upload.key != key {
		s.mu.Unlock()
		return errNoSuchUpload
	}
	if upload.completing {
		s.mu.Unlock()
		return errUploadBusy
	}
	previous, replaced := upload.parts[number]
	upload.parts[number] = part
	s.mu.Unlock()
	if replaced {
		cleanupMultipartPart(previous)
	}
	return nil
}

// beginCompletion validates the request and freezes its staged parts.
func (s *multipartStore) beginCompletion(identifier, bucket, key string, requested []completedMultipartPart) ([]multipartPart, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	upload := s.uploads[identifier]
	if upload == nil || upload.bucket != bucket || upload.key != key {
		return nil, "", errNoSuchUpload
	}
	if upload.completing {
		return nil, "", errUploadBusy
	}
	parts := make([]multipartPart, 0, len(requested))
	previous := 0
	for index, item := range requested {
		if item.PartNumber <= previous {
			return nil, "", errInvalidPartOrder
		}
		part, found := upload.parts[item.PartNumber]
		if !found || strings.Trim(item.ETag, `"`) != part.etag || index != len(requested)-1 && part.size < minimumMultipartPart {
			return nil, "", errInvalidPart
		}
		previous = item.PartNumber
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return nil, "", errInvalidPart
	}
	upload.completing = true
	return parts, upload.contentType, nil
}

// cancelCompletion makes a failed completion retryable.
func (s *multipartStore) cancelCompletion(identifier string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if upload := s.uploads[identifier]; upload != nil {
		upload.completing = false
	}
}

// remove deletes one upload unless completion currently owns it.
func (s *multipartStore) remove(identifier, bucket, key string, completing bool) (*multipartUpload, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	upload := s.uploads[identifier]
	if upload == nil || upload.bucket != bucket || upload.key != key {
		return nil, errNoSuchUpload
	}
	if upload.completing != completing {
		return nil, errUploadBusy
	}
	delete(s.uploads, identifier)
	return upload, nil
}

// expiredLocked removes and returns uploads older than the local staging lifetime.
func (s *multipartStore) expiredLocked(now time.Time) []*multipartUpload {
	var expired []*multipartUpload
	for identifier, upload := range s.uploads {
		if !upload.completing && now.Sub(upload.created) > multipartLifetime {
			delete(s.uploads, identifier)
			expired = append(expired, upload)
		}
	}
	return expired
}

// multipartUploadID returns an unpredictable URL-safe upload identifier.
func multipartUploadID() (string, error) {
	var random [24]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("generate multipart upload ID: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(random[:]), nil
}

// cleanupMultipartUploads removes all files held by uploads.
func cleanupMultipartUploads(uploads []*multipartUpload) {
	for _, upload := range uploads {
		for _, part := range upload.parts {
			cleanupMultipartPart(part)
		}
	}
}

// cleanupMultipartPart removes one staged file.
func cleanupMultipartPart(part multipartPart) {
	_ = os.Remove(part.path)
}

// Read streams bytes from each staged part in order.
func (r *multipartReader) Read(buffer []byte) (int, error) {
	for {
		if r.current == nil {
			if r.index == len(r.parts) {
				return 0, io.EOF
			}
			file, err := os.Open(r.parts[r.index].path) // #nosec G304 -- paths are internal spool files.
			if err != nil {
				return 0, err
			}
			r.current = file
			r.index++
		}
		read, err := r.current.Read(buffer)
		r.position += int64(read)
		if err != io.EOF {
			return read, err
		}
		if closeErr := r.current.Close(); closeErr != nil {
			return read, closeErr
		}
		r.current = nil
		if read != 0 {
			return read, nil
		}
	}
}

// Seek repositions the assembled stream across its staged part files.
func (r *multipartReader) Seek(offset int64, whence int) (int64, error) {
	target := offset
	switch whence {
	case io.SeekCurrent:
		target += r.position
	case io.SeekEnd:
		target += r.total
	case io.SeekStart:
	default:
		return 0, errors.New("invalid seek origin")
	}
	if target < 0 || target > r.total {
		return 0, errors.New("seek outside multipart upload")
	}
	if err := r.Close(); err != nil {
		return 0, err
	}
	r.index = 0
	r.position = target
	remaining := target
	for index, part := range r.parts {
		if remaining >= part.size {
			remaining -= part.size
			r.index = index + 1
			continue
		}
		file, err := os.Open(part.path) // #nosec G304 -- paths are internal spool files.
		if err != nil {
			return 0, err
		}
		if _, err := file.Seek(remaining, io.SeekStart); err != nil {
			_ = file.Close()
			return 0, err
		}
		r.current = file
		r.index = index + 1
		break
	}
	return target, nil
}

// Close closes the currently open part file.
func (r *multipartReader) Close() error {
	if r.current == nil {
		return nil
	}
	err := r.current.Close()
	r.current = nil
	return err
}

// createMultipartUpload starts one locally staged multipart upload.
func (h *s3Handler) createMultipartUpload(response http.ResponseWriter, request *http.Request, source *routing.ObjectSource, key string) {
	if !h.requestAllowed(response, request, source, policy.Request{Operation: "object.write", Bucket: source.Bucket, Key: key}) {
		return
	}
	identifier, err := h.multipart.create(source.Bucket, key, request.Header.Get("Content-Type"))
	if err != nil {
		writeS3Error(response, http.StatusServiceUnavailable, "ServiceUnavailable")
		return
	}
	writeXML(response, http.StatusOK, initiateMultipartUploadResult{Bucket: source.Bucket, Key: key, UploadID: identifier})
}

// uploadPart validates and stages one multipart part.
func (h *s3Handler) uploadPart(response http.ResponseWriter, request *http.Request, source *routing.ObjectSource, key string) {
	number, err := strconv.Atoi(request.URL.Query().Get("partNumber"))
	if err != nil || number < 1 || number > maximumMultipartParts {
		writeS3Error(response, http.StatusBadRequest, "InvalidArgument")
		return
	}
	path, size, etag, ok := h.stageUpload(response, request)
	if !ok {
		return
	}
	part := multipartPart{path: path, size: size, etag: etag}
	if !h.requestAllowed(response, request, source, policy.Request{Operation: "object.write", Bucket: source.Bucket, Key: key, Size: size}) {
		cleanupMultipartPart(part)
		return
	}
	if err := h.multipart.storePart(request.URL.Query().Get("uploadId"), source.Bucket, key, number, part); err != nil {
		cleanupMultipartPart(part)
		writeMultipartError(response, err)
		return
	}
	response.Header().Set("ETag", `"`+etag+`"`)
	response.WriteHeader(http.StatusOK)
}

// completeMultipartUpload validates and commits all requested parts atomically upstream.
func (h *s3Handler) completeMultipartUpload(response http.ResponseWriter, request *http.Request, source *routing.ObjectSource, key string) {
	body, err := io.ReadAll(io.LimitReader(request.Body, 1<<20+1))
	if err != nil || len(body) > 1<<20 {
		writeS3Error(response, http.StatusBadRequest, "MalformedXML")
		return
	}
	var completion completedMultipartUpload
	decoder := xml.NewDecoder(bytes.NewReader(body))
	decodeErr := decoder.Decode(&completion)
	var trailing any
	trailingErr := decoder.Decode(&trailing)
	if decodeErr != nil || !errors.Is(trailingErr, io.EOF) || len(completion.Parts) > maximumMultipartParts {
		writeS3Error(response, http.StatusBadRequest, "MalformedXML")
		return
	}
	identifier := request.URL.Query().Get("uploadId")
	parts, contentType, err := h.multipart.beginCompletion(identifier, source.Bucket, key, completion.Parts)
	if err != nil {
		writeMultipartError(response, err)
		return
	}
	total := int64(0)
	for _, part := range parts {
		if h.maxObjectSize > 0 && part.size > h.maxObjectSize-total {
			h.multipart.cancelCompletion(identifier)
			writeS3Error(response, http.StatusRequestEntityTooLarge, "EntityTooLarge")
			return
		}
		total += part.size
	}
	if !h.requestAllowed(response, request, source, policy.Request{Operation: "object.write", Bucket: source.Bucket, Key: key, Size: total}) {
		h.multipart.cancelCompletion(identifier)
		return
	}
	reader := &multipartReader{parts: parts, total: total}
	defer func() { _ = reader.Close() }()
	options := preconditions(request)
	options.ContentType = contentType
	ctx, cancel := transferContext(request)
	defer cancel()
	info, err := source.Provider.Put(ctx, key, reader, total, options)
	if err != nil {
		if mutationOutcomeUnknown(err) {
			if upload, removeErr := h.multipart.remove(identifier, source.Bucket, key, true); removeErr == nil {
				cleanupMultipartUploads([]*multipartUpload{upload})
			}
			h.cache.Remove(source.Bucket, key)
		} else {
			h.multipart.cancelCompletion(identifier)
		}
		h.mutationProviderError(response, err)
		return
	}
	upload, err := h.multipart.remove(identifier, source.Bucket, key, true)
	if err != nil {
		h.cache.Remove(source.Bucket, key)
		writeS3Error(response, http.StatusInternalServerError, "InternalError")
		return
	}
	cleanupMultipartUploads([]*multipartUpload{upload})
	// The assembled object only exists upstream, so evict any previous local value.
	h.cache.Remove(source.Bucket, key)
	writeXML(response, http.StatusOK, completeMultipartUploadResult{Bucket: source.Bucket, Key: key, ETag: info.ETag})
}

// abortMultipartUpload releases all local parts without changing the upstream object.
func (h *s3Handler) abortMultipartUpload(response http.ResponseWriter, request *http.Request, source *routing.ObjectSource, key string) {
	if !h.requestAllowed(response, request, source, policy.Request{Operation: "object.write", Bucket: source.Bucket, Key: key}) {
		return
	}
	upload, err := h.multipart.remove(request.URL.Query().Get("uploadId"), source.Bucket, key, false)
	if err != nil {
		writeMultipartError(response, err)
		return
	}
	cleanupMultipartUploads([]*multipartUpload{upload})
	response.WriteHeader(http.StatusNoContent)
}

// writeMultipartError maps local multipart state errors to S3 responses.
func writeMultipartError(response http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errNoSuchUpload):
		writeS3Error(response, http.StatusNotFound, "NoSuchUpload")
	case errors.Is(err, errInvalidPartOrder):
		writeS3Error(response, http.StatusBadRequest, "InvalidPartOrder")
	case errors.Is(err, errInvalidPart):
		writeS3Error(response, http.StatusBadRequest, "InvalidPart")
	default:
		writeS3Error(response, http.StatusConflict, "OperationAborted")
	}
}
