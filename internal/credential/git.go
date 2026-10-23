// Source Gateway <https://sourcegateway.dev>
// Copyright The Source Gateway Authors
// SPDX-License-Identifier: Apache-2.0

package credential

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/lestrrat-go/jwx/v4/jwa"
	"github.com/lestrrat-go/jwx/v4/jwt"
	"github.com/sourcegateway/sourcegateway/internal/config"
)

// GitAuthorization builds an HTTP authorization value for a Git upstream.
// An omitted credential permits anonymous upstream access, not anonymous clients.
func GitAuthorization(ctx context.Context, cfg *config.Config, name, namespace, upstream string, allowedInsecureUpstreams []string) (string, error) {
	parsed, err := url.Parse(upstream)
	if err != nil || parsed.Scheme != "http" && parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("upstream must be an HTTP or HTTPS URL without user information")
	}
	origin, err := config.NormalizeOrigin(parsed.Scheme + "://" + parsed.Host)
	if err != nil {
		return "", errors.New("invalid upstream origin")
	}
	if parsed.Scheme == "http" && !slices.Contains(allowedInsecureUpstreams, origin) {
		return "", errors.New("upstream must use HTTPS")
	}
	if name == "" {
		return "", nil
	}
	credential, err := cfg.Credential("gitRepository", name, namespace)
	if err != nil {
		return "", err
	}
	if origin != credential.Origin {
		return "", errors.New("upstream origin is not permitted by the selected credential")
	}
	switch credential.Kind {
	case "githubUser":
		files, err := cfg.ReadCredentialFiles(name, "username", "token")
		if err != nil {
			return "", err
		}
		encoded := base64.StdEncoding.EncodeToString([]byte(strings.TrimSpace(string(files["username"])) + ":" + strings.TrimSpace(string(files["token"]))))
		return "Basic " + encoded, nil
	case "githubApp":
		return githubAppAuthorization(ctx, cfg, name, credential.Origin)
	default:
		return "", errors.New("unsupported Git credential")
	}
}

// githubAppCredential contains validated material needed to mint an installation token.
type githubAppCredential struct {
	appID          int64
	installationID int64
	privateKey     *rsa.PrivateKey
}

// githubAppAuthorization exchanges a signed app JWT for an installation token.
func githubAppAuthorization(ctx context.Context, cfg *config.Config, name, origin string) (string, error) {
	credential, err := loadGitHubAppCredential(cfg, name)
	if err != nil {
		return "", err
	}
	assertion, err := signGitHubAppJWT(credential.appID, credential.privateKey)
	if err != nil {
		return "", err
	}
	return exchangeGitHubAppJWT(ctx, origin, credential.installationID, assertion)
}

// loadGitHubAppCredential reads and validates a GitHub App identity and signing key.
func loadGitHubAppCredential(cfg *config.Config, name string) (githubAppCredential, error) {
	files, err := cfg.ReadCredentialFiles(name, "app-id", "installation-id", "private-key.pem")
	if err != nil {
		return githubAppCredential{}, err
	}
	appID, err := strconv.ParseInt(strings.TrimSpace(string(files["app-id"])), 10, 64)
	if err != nil || appID <= 0 {
		return githubAppCredential{}, errors.New("invalid GitHub App ID")
	}
	installationID, err := strconv.ParseInt(strings.TrimSpace(string(files["installation-id"])), 10, 64)
	if err != nil || installationID <= 0 {
		return githubAppCredential{}, errors.New("invalid GitHub installation ID")
	}
	block, _ := pem.Decode(files["private-key.pem"])
	if block == nil {
		return githubAppCredential{}, errors.New("invalid GitHub App private key")
	}
	var privateKey *rsa.PrivateKey
	if parsed, parseErr := x509.ParsePKCS1PrivateKey(block.Bytes); parseErr == nil {
		privateKey = parsed
	} else if generic, parseErr := x509.ParsePKCS8PrivateKey(block.Bytes); parseErr == nil {
		privateKey, _ = generic.(*rsa.PrivateKey)
	}
	if privateKey == nil {
		return githubAppCredential{}, errors.New("GitHub App private key is not RSA")
	}
	return githubAppCredential{appID: appID, installationID: installationID, privateKey: privateKey}, nil
}

// signGitHubAppJWT creates the short-lived assertion used for token exchange.
func signGitHubAppJWT(appID int64, privateKey *rsa.PrivateKey) (string, error) {
	now := time.Now()
	token, err := jwt.NewBuilder().
		Issuer(strconv.FormatInt(appID, 10)).
		IssuedAt(now.Add(-time.Minute)).
		Expiration(now.Add(9 * time.Minute)).
		Build()
	if err != nil {
		return "", fmt.Errorf("build GitHub App JWT: %w", err)
	}
	signed, err := jwt.Sign(token, jwt.WithKey(jwa.RS256(), privateKey))
	if err != nil {
		return "", fmt.Errorf("sign GitHub App JWT: %w", err)
	}
	return string(signed), nil
}

// exchangeGitHubAppJWT requests an installation token from the matching GitHub API origin.
func exchangeGitHubAppJWT(ctx context.Context, origin string, installationID int64, jwt string) (string, error) {
	apiBase := "https://api.github.com"
	if origin != "https://github.com" {
		apiBase = origin + "/api/v3"
	}
	endpoint := fmt.Sprintf("%s/app/installations/%d/access_tokens", apiBase, installationID)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Authorization", "Bearer "+jwt)
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: sameOriginRedirect}
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusCreated {
		_, _ = io.Copy(io.Discard, response.Body)
		return "", errors.New("GitHub rejected installation token request")
	}
	var body struct {
		Token string `json:"token"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	if err := decoder.Decode(&body); err != nil || body.Token == "" {
		return "", errors.New("invalid GitHub installation token response")
	}
	encoded := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + body.Token))
	return "Basic " + encoded, nil
}

// sameOriginRedirect prevents credential-bearing requests crossing origins.
func sameOriginRedirect(request *http.Request, via []*http.Request) error {
	if len(via) != 0 && !strings.EqualFold(request.URL.Scheme+"://"+request.URL.Host, via[0].URL.Scheme+"://"+via[0].URL.Host) {
		return errors.New("cross-origin redirect rejected")
	}
	if len(via) >= 3 {
		return errors.New("too many redirects")
	}
	return nil
}
