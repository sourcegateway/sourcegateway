// Source Gateway <https://sourcegateway.dev>
// Copyright The Source Gateway Authors
// SPDX-License-Identifier: Apache-2.0

package credential

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sourcegateway/sourcegateway/internal/config"
)

// TestSignGitHubAppJWT verifies the protected header, claim types, lifetime, and signature.
func TestSignGitHubAppJWT(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	token, err := signGitHubAppJWT(12345, key)
	if err != nil {
		t.Fatal(err)
	}
	segments := strings.Split(token, ".")
	if len(segments) != 3 {
		t.Fatalf("JWT has %d segments", len(segments))
	}

	// Check the protected header before verifying its signature.
	headerJSON, err := base64.RawURLEncoding.DecodeString(segments[0])
	if err != nil {
		t.Fatal(err)
	}
	var header struct {
		Algorithm string `json:"alg"`
		Type      string `json:"typ"`
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		t.Fatal(err)
	}
	if header.Algorithm != "RS256" || header.Type != "JWT" {
		t.Fatalf("unexpected JWT header: %#v", header)
	}

	// Ensure registered claims use GitHub's expected types and lifetime.
	claimsJSON, err := base64.RawURLEncoding.DecodeString(segments[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims struct {
		IssuedAt  int64  `json:"iat"`
		ExpiresAt int64  `json:"exp"`
		Issuer    string `json:"iss"`
	}
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Issuer != "12345" || claims.ExpiresAt-claims.IssuedAt != 10*60 {
		t.Fatalf("unexpected JWT claims: %#v", claims)
	}

	// Verify the compact serialization against the generated public key.
	signature, err := base64.RawURLEncoding.DecodeString(segments[2])
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(segments[0] + "." + segments[1]))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], signature); err != nil {
		t.Fatalf("verify JWT signature: %v", err)
	}
}

// TestGitAuthorizationRejectsCredentialOriginMismatch checks credentials before file access.
func TestGitAuthorizationRejectsCredentialOriginMismatch(t *testing.T) {
	cfg := &config.Config{Credentials: config.Credentials{GitRepository: map[string]config.Credential{
		"github": {Kind: "githubUser", Namespaces: []string{"agents"}, Origin: "https://github.com"},
	}}}
	if _, err := GitAuthorization(context.Background(), cfg, "github", "agents", "https://attacker.example/repository.git", nil); err == nil || !strings.Contains(err.Error(), "not permitted") {
		t.Fatalf("mismatched origin returned %v", err)
	}
}

// TestGitAuthorizationRequiresHTTPSOrExplicitOrigin checks the development-only HTTP opt-in.
func TestGitAuthorizationRequiresHTTPSOrExplicitOrigin(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "github")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"username": "demo", "token": "token"} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{CredentialsDirectory: root, Credentials: config.Credentials{GitRepository: map[string]config.Credential{
		"github": {Kind: "githubUser", Namespaces: []string{"agents"}, Origin: "http://git.example"},
	}}}
	upstream := "http://git.example/repository.git"
	if _, err := GitAuthorization(t.Context(), cfg, "github", "agents", upstream, nil); err == nil || !strings.Contains(err.Error(), "must use HTTPS") {
		t.Fatalf("unapproved HTTP upstream returned %v", err)
	}
	if _, err := GitAuthorization(t.Context(), cfg, "github", "agents", upstream, []string{"http://other.example"}); err == nil || !strings.Contains(err.Error(), "must use HTTPS") {
		t.Fatalf("wrong approved HTTP origin returned %v", err)
	}
	authorization, err := GitAuthorization(t.Context(), cfg, "github", "agents", upstream, []string{"http://git.example"})
	if err != nil || !strings.HasPrefix(authorization, "Basic ") {
		t.Fatalf("approved HTTP upstream authorization = %q, %v", authorization, err)
	}
}

// TestGitAuthorizationAnonymous validates transport even without a credential.
func TestGitAuthorizationAnonymous(t *testing.T) {
	tests := []struct {
		name     string
		upstream string
		allowed  []string
		wantErr  bool
	}{
		{name: "public HTTPS", upstream: "https://github.com/octocat/Hello-World.git"},
		{name: "HTTP denied", upstream: "http://git.example/repository.git", wantErr: true},
		{name: "HTTP allowed", upstream: "http://git.example/repository.git", allowed: []string{"http://git.example"}},
		{name: "different HTTP origin", upstream: "http://git.example/repository.git", allowed: []string{"http://other.example"}, wantErr: true},
		{name: "userinfo", upstream: "https://user@github.com/repository.git", wantErr: true},
		{name: "query", upstream: "https://github.com/repository.git?key=value", wantErr: true},
		{name: "fragment", upstream: "https://github.com/repository.git#main", wantErr: true},
		{name: "local path", upstream: "/tmp/repository.git", wantErr: true},
		{name: "file URL", upstream: "file:///tmp/repository.git", wantErr: true},
		{name: "missing host", upstream: "https:///repository.git", wantErr: true},
		{name: "invalid port", upstream: "https://github.com:invalid/repository.git", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			header, err := GitAuthorization(t.Context(), &config.Config{}, "", "agents", test.upstream, test.allowed)
			if (err != nil) != test.wantErr || header != "" {
				t.Fatalf("authorization = %q, %v; want empty header, error=%t", header, err, test.wantErr)
			}
		})
	}
}

// TestGitAuthorizationDoesNotFallBackToAnonymous keeps named credentials fail-closed.
func TestGitAuthorizationDoesNotFallBackToAnonymous(t *testing.T) {
	cfg := &config.Config{CredentialsDirectory: t.TempDir(), Credentials: config.Credentials{GitRepository: map[string]config.Credential{
		"github": {Kind: "githubUser", Namespaces: []string{"agents"}, Origin: "https://github.com"},
	}}}
	for _, test := range []struct{ name, namespace string }{
		{name: "missing", namespace: "agents"},
		{name: "github", namespace: "other"},
		{name: "github", namespace: "agents"}, // Configured, but its files are missing.
	} {
		header, err := GitAuthorization(t.Context(), cfg, test.name, test.namespace, "https://github.com/octocat/Hello-World.git", nil)
		if err == nil || header != "" {
			t.Fatalf("credential %q in namespace %q returned %q, %v", test.name, test.namespace, header, err)
		}
	}
}
