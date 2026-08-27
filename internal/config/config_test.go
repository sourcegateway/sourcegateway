// Source Gateway <https://sourcegateway.dev>
// Copyright The Source Gateway Authors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfigFixture creates a minimal valid config and secret tree.
func writeConfigFixture(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	secrets := filepath.Join(root, "secrets")
	if err := os.Mkdir(secrets, 0o700); err != nil {
		t.Fatal(err)
	}
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	if err := os.WriteFile(filepath.Join(secrets, "encryption-key"), []byte(key), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "config.jsonc")
	contents := fmt.Sprintf(`{
		// JSONC is accepted.
		"objectCacheCapacity": "10Mi",
		"credentialsDirectory": %q,
		"storageDirectory": %q,
		"credentials": {"gitRepository": {"git": {"kind": "githubUser", "namespaces": ["agents"]}}}
	}`, secrets, filepath.Join(root, "storage"))
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, secrets
}

// TestLoadStrictJSONCAndCredentialScope checks decoding and namespace authorization.
func TestLoadStrictJSONCAndCredentialScope(t *testing.T) {
	path, _ := writeConfigFixture(t)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TokenAudience != DefaultAudience || len(cfg.EncryptionKey) != 32 || cfg.CacheCapacity.Value() != 10<<20 || cfg.MaxObjectSizeBytes != 15<<20 {
		t.Fatalf("unexpected config: %#v", cfg)
	}
	if _, err := cfg.Credential("gitRepository", "git", "agents"); err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.Credential("gitRepository", "git", "other"); err == nil {
		t.Fatal("out-of-scope credential unexpectedly resolved")
	}
	if credential, err := cfg.Credential("gitRepository", "git", "agents"); err != nil || credential.Origin != "https://github.com" {
		t.Fatalf("default GitHub origin = %q, %v", credential.Origin, err)
	}

	raw, err := os.ReadFile(path) // #nosec G304 -- path is created by this test.
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(raw), `"credentials":`, `"unknown": true, "credentials":`, 1)), 0o600); err != nil { // #nosec G703 -- path is created by this test.
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("unknown config field unexpectedly accepted")
	}
	if err := os.WriteFile(path, append(raw, []byte(` {}`)...), 0o600); err != nil { // #nosec G703 -- path is created by this test.
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("trailing JSON value unexpectedly accepted")
	}
}

// TestLoadValidatesMaximumObjectSize checks custom and malformed size limits.
func TestLoadValidatesMaximumObjectSize(t *testing.T) {
	path, _ := writeConfigFixture(t)
	raw, err := os.ReadFile(path) // #nosec G304 -- path is created by this test.
	if err != nil {
		t.Fatal(err)
	}
	custom := strings.Replace(string(raw), `"objectCacheCapacity": "10Mi",`, `"objectCacheCapacity": "10Mi", "maxObjectSize": "32Mi",`, 1)
	if err := os.WriteFile(path, []byte(custom), 0o600); err != nil { // #nosec G703 -- path is created by this test.
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil || cfg.MaxObjectSizeBytes != 32<<20 {
		t.Fatalf("custom maximum = %d, %v", cfg.MaxObjectSizeBytes, err)
	}
	invalid := strings.Replace(custom, `"maxObjectSize": "32Mi"`, `"maxObjectSize": "0"`, 1)
	if err := os.WriteFile(path, []byte(invalid), 0o600); err != nil { // #nosec G703 -- path is created by this test.
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "maxObjectSize") {
		t.Fatalf("invalid maximum returned %v", err)
	}
	unlimited := strings.Replace(custom, `"maxObjectSize": "32Mi"`, `"maxObjectSize": null`, 1)
	if err := os.WriteFile(path, []byte(unlimited), 0o600); err != nil { // #nosec G703 -- path is created by this test.
		t.Fatal(err)
	}
	cfg, err = Load(path)
	if err != nil || cfg.MaxObjectSize != nil || cfg.MaxObjectSizeBytes != 0 {
		t.Fatalf("unlimited maximum = %#v, %v", cfg.MaxObjectSize, err)
	}
}

// TestLoadRejectsUnsafeSecretFiles checks permissions and symlink confinement.
func TestLoadRejectsUnsafeSecretFiles(t *testing.T) {
	path, secrets := writeConfigFixture(t)
	keyPath := filepath.Join(secrets, "encryption-key")
	if err := os.Chmod(keyPath, 0o644); err != nil { // #nosec G302 -- deliberately verifies rejection of an unsafe mode.
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("group-readable key unexpectedly accepted")
	}
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(filepath.Dir(secrets), "outside")
	if err := os.WriteFile(outside, []byte(base64.StdEncoding.EncodeToString(make([]byte, 32))), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, keyPath); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("escaping key symlink unexpectedly accepted")
	}
}

// TestLoadAllowsContainedSecureSymlink checks safe secret rotation links.
func TestLoadAllowsContainedSecureSymlink(t *testing.T) {
	path, secrets := writeConfigFixture(t)
	keyPath := filepath.Join(secrets, "encryption-key")
	target := filepath.Join(secrets, "rotated-key")
	if err := os.Rename(keyPath, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Base(target), keyPath); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("contained secure symlink was rejected: %v", err)
	}
}

// TestSecureFilesPinsAtomicWriterGeneration checks that rotation cannot mix credential files.
func TestSecureFilesPinsAtomicWriterGeneration(t *testing.T) {
	root := t.TempDir()
	for _, generation := range []string{"..one", "..two"} {
		if err := os.Mkdir(filepath.Join(root, generation), 0o700); err != nil {
			t.Fatal(err)
		}
		value := strings.TrimPrefix(generation, "..")
		for _, name := range []string{"username", "token"} {
			if err := os.WriteFile(filepath.Join(root, generation, name), []byte(value), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.Symlink("..one", filepath.Join(root, "..data")); err != nil {
		t.Fatal(err)
	}
	// Public links are intentionally inconsistent; the reader must ignore them
	// and resolve every requested file through the pinned ..data generation.
	if err := os.Symlink("..data/username", filepath.Join(root, "username")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("..two/token", filepath.Join(root, "token")); err != nil {
		t.Fatal(err)
	}
	files, err := secureFiles(root, []string{"username", "token"})
	if err != nil {
		t.Fatal(err)
	}
	if string(files["username"]) != "one" || string(files["token"]) != "one" {
		t.Fatalf("mixed credential generations: %q and %q", files["username"], files["token"])
	}
}

// TestLoadRejectsCredentialNameSharedByGroups checks cross-group uniqueness.
func TestLoadRejectsCredentialNameSharedByGroups(t *testing.T) {
	path, _ := writeConfigFixture(t)
	raw, err := os.ReadFile(path) // #nosec G304 -- path is created by this test.
	if err != nil {
		t.Fatal(err)
	}
	updated := strings.Replace(string(raw),
		`"gitRepository": {"git": {"kind": "githubUser", "namespaces": ["agents"]}}`,
		`"gitRepository": {"shared": {"kind": "githubUser", "namespaces": ["agents"]}}, "objectStorageBucket": {"shared": {"kind": "awsDefault", "namespaces": ["agents"]}}`, 1)
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil { // #nosec G703 -- path is created by this test.
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "configured in both") {
		t.Fatalf("duplicate cross-group credential returned %v", err)
	}
}

// TestNormalizeOriginCanonicalizesEffectivePorts checks exact credential boundaries.
func TestNormalizeOriginCanonicalizesEffectivePorts(t *testing.T) {
	for value, want := range map[string]string{
		"HTTPS://GitHub.COM:443/": "https://github.com",
		"http://127.0.0.1:80":     "http://127.0.0.1",
		"https://[::1]:9443":      "https://[::1]:9443",
	} {
		got, err := NormalizeOrigin(value)
		if err != nil || got != want {
			t.Errorf("NormalizeOrigin(%q) = %q, %v; want %q", value, got, err, want)
		}
	}
	for _, value := range []string{"", "ftp://storage.example", "https://user@storage.example", "https://storage.example/path", "https://storage.example?query"} {
		if _, err := NormalizeOrigin(value); err == nil {
			t.Errorf("NormalizeOrigin(%q) unexpectedly succeeded", value)
		}
	}
}

// TestNormalizeEndpointPreservesPathsAndExtractsOrigins checks base URL handling.
func TestNormalizeEndpointPreservesPathsAndExtractsOrigins(t *testing.T) {
	for value, want := range map[string]struct {
		endpoint string
		origin   string
	}{
		"HTTPS://Storage.Example:443/s3/team-a": {endpoint: "https://storage.example/s3/team-a", origin: "https://storage.example"},
		"http://127.0.0.1:80/api%20root":        {endpoint: "http://127.0.0.1/api%20root", origin: "http://127.0.0.1"},
		"https://[::1]:9443/":                   {endpoint: "https://[::1]:9443", origin: "https://[::1]:9443"},
	} {
		endpoint, origin, err := NormalizeEndpoint(value)
		if err != nil || endpoint != want.endpoint || origin != want.origin {
			t.Errorf("NormalizeEndpoint(%q) = %q, %q, %v; want %q, %q", value, endpoint, origin, err, want.endpoint, want.origin)
		}
	}
	for _, value := range []string{"", "ftp://storage.example/path", "https://user@storage.example/path", "https://storage.example/path?query", "https://storage.example/path#fragment"} {
		if _, _, err := NormalizeEndpoint(value); err == nil {
			t.Errorf("NormalizeEndpoint(%q) unexpectedly succeeded", value)
		}
	}
}

// TestValidateKindAcceptsObjectCredentials verifies object credential names.
func TestValidateKindAcceptsObjectCredentials(t *testing.T) {
	for _, kind := range []string{"awsAccessKey", "awsDefault", "gcsDefault", "gcsServiceAccount"} {
		if err := validateKind("objectStorageBucket", kind); err != nil {
			t.Errorf("validateKind(%q) returned %v", kind, err)
		}
	}
}

// TestValidateCredentialOriginDefaultsKnownServices checks kind-specific origins.
func TestValidateCredentialOriginDefaultsKnownServices(t *testing.T) {
	for kind, want := range map[string]string{
		"githubApp":         "https://github.com",
		"githubUser":        "https://github.com",
		"gcsDefault":        "https://storage.googleapis.com",
		"gcsServiceAccount": "https://storage.googleapis.com",
		"awsAccessKey":      "",
		"awsDefault":        "",
	} {
		group := "objectStorageBucket"
		if strings.HasPrefix(kind, "github") {
			group = "gitRepository"
		}
		credential := Credential{Kind: kind}
		if err := validateCredentialOrigin(group, &credential); err != nil || credential.Origin != want {
			t.Errorf("validateCredentialOrigin(%q) = %q, %v; want %q", kind, credential.Origin, err, want)
		}
	}
}
