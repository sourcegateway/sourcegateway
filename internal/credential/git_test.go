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
