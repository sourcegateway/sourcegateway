// Source Gateway <https://sourcegateway.dev>
// Copyright The Source Gateway Authors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/tailscale/hujson"
	"k8s.io/apimachinery/pkg/api/resource"
)

const (
	DefaultAudience             = "sourcegateway.dev"
	DefaultCredentialsDirectory = "/var/run/sourcegateway/secrets" // #nosec G101 -- this is a directory, not a credential.
	DefaultGCSEndpoint          = "https://storage.googleapis.com/storage/v1/"
	DefaultGCSOrigin            = "https://storage.googleapis.com"
	DefaultGitHubUserAnnotation = "sourcegateway.dev/github-user"
	DefaultMaxObjectSize        = "15Mi"
)

// Config contains validated runtime settings.
type Config struct {
	TokenAudience        string      `json:"tokenAudience"`
	ObjectCacheCapacity  string      `json:"objectCacheCapacity"`
	MaxObjectSize        *string     `json:"maxObjectSize"`
	CredentialsDirectory string      `json:"credentialsDirectory"`
	GitHubUserAnnotation string      `json:"githubUserAnnotation"`
	ListenAddress        string      `json:"listenAddress,omitempty"`
	MetricsAddress       string      `json:"metricsAddress,omitempty"`
	StorageDirectory     string      `json:"storageDirectory,omitempty"`
	TLSCertificateFile   string      `json:"tlsCertificateFile,omitempty"`
	TLSPrivateKeyFile    string      `json:"tlsPrivateKeyFile,omitempty"`
	Credentials          Credentials `json:"credentials"`

	CacheCapacity      resource.Quantity `json:"-"`
	MaxObjectSizeBytes int64             `json:"-"`
	EncryptionKey      []byte            `json:"-"`
}

// Credentials groups source credentials by resource kind.
type Credentials struct {
	GitRepository       map[string]Credential `json:"gitRepository,omitempty"`
	ObjectStorageBucket map[string]Credential `json:"objectStorageBucket,omitempty"`
}

// Credential describes a credential provider and its namespace scope.
type Credential struct {
	Kind                string   `json:"kind"`
	Namespaces          []string `json:"namespaces"`
	Origin              string   `json:"origin,omitempty"`
	SecretProviderClass string   `json:"-"`
}

// Load reads, strictly decodes, defaults, and validates a JSONC configuration.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- path is the explicit administrator-supplied --config value.
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	standard, err := hujson.Standardize(raw)
	if err != nil {
		return nil, fmt.Errorf("parse JSONC: %w", err)
	}
	defaultMaxObjectSize := DefaultMaxObjectSize
	cfg := &Config{
		TokenAudience:        DefaultAudience,
		MaxObjectSize:        &defaultMaxObjectSize,
		CredentialsDirectory: DefaultCredentialsDirectory,
		GitHubUserAnnotation: DefaultGitHubUserAnnotation,
		ListenAddress:        ":8443",
		MetricsAddress:       ":9443",
		StorageDirectory:     "/var/lib/sourcegateway",
	}
	decoder := json.NewDecoder(bytes.NewReader(standard))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(cfg); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("decode config: trailing JSON value")
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// validate derives runtime values and rejects unsafe configuration.
func (c *Config) validate() error {
	if c.TokenAudience == "" {
		return errors.New("tokenAudience must not be empty")
	}
	if c.ObjectCacheCapacity == "" {
		return errors.New("objectCacheCapacity must be set")
	}
	capacity, err := resource.ParseQuantity(c.ObjectCacheCapacity)
	if err != nil || capacity.Sign() <= 0 {
		return errors.New("objectCacheCapacity must be a positive quantity")
	}
	c.CacheCapacity = capacity
	if c.MaxObjectSize != nil {
		maximumObjectSize, err := resource.ParseQuantity(*c.MaxObjectSize)
		if err != nil || maximumObjectSize.Sign() <= 0 {
			return errors.New("maxObjectSize must be a positive quantity or null")
		}
		c.MaxObjectSizeBytes = maximumObjectSize.Value()
	}
	if !filepath.IsAbs(c.CredentialsDirectory) || !filepath.IsAbs(c.StorageDirectory) {
		return errors.New("credentialsDirectory and storageDirectory must be absolute")
	}
	credentialNames := make(map[string]string)
	// Validate both credential groups while enforcing globally unique names.
	for group, credentials := range map[string]map[string]Credential{
		"gitRepository":       c.Credentials.GitRepository,
		"objectStorageBucket": c.Credentials.ObjectStorageBucket,
	} {
		for name, credential := range credentials {
			if name == "encryption-key" {
				return fmt.Errorf("%s credential name encryption-key is reserved", group)
			}
			if len(credential.Namespaces) == 0 {
				return fmt.Errorf("%s credential %q has no namespaces", group, name)
			}
			if err := validateKind(group, credential.Kind); err != nil {
				return fmt.Errorf("%s credential %q: %w", group, name, err)
			}
			if err := validateCredentialOrigin(group, &credential); err != nil {
				return fmt.Errorf("%s credential %q: %w", group, name, err)
			}
			credentials[name] = credential
			if otherGroup, duplicate := credentialNames[name]; duplicate {
				return fmt.Errorf("credential %q is configured in both %s and %s", name, otherGroup, group)
			}
			credentialNames[name] = group
		}
	}
	// Load the encryption key only after all structural validation succeeds.
	files, err := secureFiles(c.CredentialsDirectory, []string{"encryption-key"})
	if err != nil {
		return fmt.Errorf("encryption-key: %w", err)
	}
	encoded := files["encryption-key"]
	key, err := base64.StdEncoding.Strict().DecodeString(string(bytes.TrimSpace(encoded)))
	if err != nil || len(key) != 32 {
		return errors.New("encryption-key must contain standard base64 encoding of exactly 32 bytes")
	}
	c.EncryptionKey = key
	return nil
}

// validateCredentialOrigin defaults and validates a credential's trusted network boundary.
func validateCredentialOrigin(group string, credential *Credential) error {
	if credential.Origin == "" {
		switch credential.Kind {
		case "githubApp", "githubUser":
			credential.Origin = "https://github.com"
		case "gcsDefault", "gcsServiceAccount":
			credential.Origin = DefaultGCSOrigin
		}
	}
	// AWS origins depend on the bucket region and are resolved when a resource
	// selects the credential. All other kinds have a complete origin here.
	if credential.Origin == "" {
		return nil
	}
	origin, err := NormalizeOrigin(credential.Origin)
	if err != nil {
		return errors.New("origin must be an HTTP or HTTPS origin")
	}
	credential.Origin = origin
	return nil
}

// NormalizeOrigin validates and canonicalizes an HTTP origin without a path.
func NormalizeOrigin(value string) (string, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Opaque != "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.EscapedPath() != "" && parsed.EscapedPath() != "/" {
		return "", errors.New("invalid HTTP origin")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", errors.New("invalid HTTP origin")
	}
	hostname := strings.ToLower(parsed.Hostname())
	if hostname == "" {
		return "", errors.New("invalid HTTP origin")
	}
	port := parsed.Port()
	if scheme == "http" && port == "80" || scheme == "https" && port == "443" {
		port = ""
	}
	host := hostname
	if port != "" {
		host = net.JoinHostPort(hostname, port)
	} else if strings.Contains(hostname, ":") {
		host = "[" + hostname + "]"
	}
	return scheme + "://" + host, nil
}

// NormalizeEndpoint validates an HTTP base URL and returns it with its origin.
func NormalizeEndpoint(value string) (string, string, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Opaque != "" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", "", errors.New("invalid HTTP endpoint")
	}
	origin, err := NormalizeOrigin(parsed.Scheme + "://" + parsed.Host)
	if err != nil {
		return "", "", errors.New("invalid HTTP endpoint")
	}
	path := parsed.EscapedPath()
	if path == "" || path == "/" {
		return origin, origin, nil
	}
	return origin + path, origin, nil
}

// validateKind checks that a credential kind belongs to its resource group.
func validateKind(group, kind string) error {
	allowed := map[string]map[string]bool{
		"gitRepository":       {"githubApp": true, "githubUser": true},
		"objectStorageBucket": {"awsAccessKey": true, "awsDefault": true, "gcsDefault": true, "gcsServiceAccount": true},
	}
	if !allowed[group][kind] {
		return fmt.Errorf("unsupported kind %q", kind)
	}
	return nil
}

// secureFiles reads regular files confined to one mounted-secret generation.
func secureFiles(directory string, names []string) (map[string][]byte, error) {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	filesRoot := root
	// Pin Kubernetes atomic-writer's immutable generation rather than following
	// the mutable ..data link separately for each credential file.
	generationName, generationErr := os.Readlink(filepath.Join(directory, "..data"))
	if generationErr == nil {
		if filepath.Base(generationName) != generationName || generationName == "." || generationName == ".." {
			return nil, errors.New("invalid mounted-secret generation")
		}
		generation, err := root.OpenRoot(generationName)
		if err != nil {
			return nil, err
		}
		defer func() { _ = generation.Close() }()
		filesRoot = generation
	} else if !errors.Is(generationErr, os.ErrNotExist) {
		return nil, generationErr
	}
	result := make(map[string][]byte, len(names))
	for _, name := range names {
		if filepath.Base(name) != name {
			return nil, errors.New("invalid credential path")
		}
		file, openErr := filesRoot.Open(name)
		if openErr != nil {
			return nil, openErr
		}
		info, statErr := file.Stat()
		if statErr != nil || !info.Mode().IsRegular() {
			_ = file.Close()
			if statErr != nil {
				return nil, statErr
			}
			return nil, errors.New("credential must be a regular file")
		}
		contents, readErr := io.ReadAll(file)
		closeErr := file.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			return nil, err
		}
		result[name] = contents
	}
	return result, nil
}

// Credential resolves a named credential when the namespace is permitted.
func (c *Config) Credential(group, name, namespace string) (Credential, error) {
	var entries map[string]Credential
	if group == "gitRepository" {
		entries = c.Credentials.GitRepository
	} else {
		entries = c.Credentials.ObjectStorageBucket
	}
	credential, ok := entries[name]
	if !ok {
		return Credential{}, errors.New("credential not configured")
	}
	for _, permitted := range credential.Namespaces {
		if permitted == "*" || permitted == namespace {
			return credential, nil
		}
	}
	return Credential{}, errors.New("credential is not permitted in this namespace")
}

// ReadCredentialFiles safely reads files from one credential mount generation.
func (c *Config) ReadCredentialFiles(name string, files ...string) (map[string][]byte, error) {
	if filepath.Base(name) != name {
		return nil, errors.New("invalid credential path")
	}
	return secureFiles(filepath.Join(c.CredentialsDirectory, name), files)
}
