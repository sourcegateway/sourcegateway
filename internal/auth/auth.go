// Source Gateway <https://sourcegateway.dev>
// Copyright The Source Gateway Authors
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/lestrrat-go/jwx/v4/jwt"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

var (
	ErrInvalid     = errors.New("invalid credentials")
	ErrUnavailable = errors.New("authentication unavailable")
)

const maximumCachedTokens = 4096

// Identity names an authenticated Kubernetes ServiceAccount.
type Identity struct {
	Namespace string
	Name      string
	UID       string
}

// Reviewer submits Kubernetes TokenReview requests.
type Reviewer interface {
	Review(context.Context, *authenticationv1.TokenReview) (*authenticationv1.TokenReview, error)
}

// kubeReviewer adapts a Kubernetes client to Reviewer.
type kubeReviewer struct{ client kubernetes.Interface }

// +kubebuilder:rbac:groups=authentication.k8s.io,resources=tokenreviews,verbs=create

// NewKubernetesReviewer returns a reviewer backed by the Kubernetes API.
func NewKubernetesReviewer(client kubernetes.Interface) Reviewer {
	return &kubeReviewer{client: client}
}

// Review submits a TokenReview to Kubernetes.
func (r *kubeReviewer) Review(ctx context.Context, review *authenticationv1.TokenReview) (*authenticationv1.TokenReview, error) {
	return r.client.AuthenticationV1().TokenReviews().Create(ctx, review, metav1.CreateOptions{})
}

// cacheEntry retains a verified identity until its safe expiry.
type cacheEntry struct {
	identity Identity
	expires  time.Time
}

// Authenticator validates and briefly caches ServiceAccount tokens.
type Authenticator struct {
	reviewer Reviewer
	audience string
	now      func() time.Time
	mu       sync.Mutex
	cache    map[[32]byte]cacheEntry
}

// New constructs an Authenticator for the required token audience.
func New(reviewer Reviewer, audience string) *Authenticator {
	return &Authenticator{reviewer: reviewer, audience: audience, now: time.Now, cache: make(map[[32]byte]cacheEntry)}
}

// Authenticate validates a bearer token and returns its ServiceAccount identity.
func (a *Authenticator) Authenticate(ctx context.Context, token string) (Identity, error) {
	if token == "" {
		return Identity{}, ErrInvalid
	}
	hash := sha256.Sum256([]byte(token))
	now := a.now()
	a.mu.Lock()
	if cached, ok := a.cache[hash]; ok && now.Before(cached.expires) {
		a.mu.Unlock()
		return cached.identity, nil
	}
	delete(a.cache, hash)
	a.mu.Unlock()

	// Ask Kubernetes only after a cache miss.
	review, err := a.reviewer.Review(ctx, &authenticationv1.TokenReview{Spec: authenticationv1.TokenReviewSpec{
		Token: token, Audiences: []string{a.audience},
	}})
	if err != nil {
		return Identity{}, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	identity, err := a.validate(review)
	if err != nil {
		return Identity{}, err
	}
	expires := now.Add(30 * time.Second)
	if tokenExpiry, ok := jwtExpiry(token); ok && tokenExpiry.Before(expires) {
		expires = tokenExpiry
	}
	if expires.After(now) {
		// Prune stale entries and bound the cache before inserting.
		a.mu.Lock()
		var oldestHash [32]byte
		oldestExpiry := time.Time{}
		for cachedHash, cached := range a.cache {
			if !now.Before(cached.expires) {
				delete(a.cache, cachedHash)
				continue
			}
			if oldestExpiry.IsZero() || cached.expires.Before(oldestExpiry) {
				oldestHash, oldestExpiry = cachedHash, cached.expires
			}
		}
		if len(a.cache) >= maximumCachedTokens {
			delete(a.cache, oldestHash)
		}
		a.cache[hash] = cacheEntry{identity: identity, expires: expires}
		a.mu.Unlock()
	}
	return identity, nil
}

// validate extracts a complete ServiceAccount identity from a TokenReview.
func (a *Authenticator) validate(review *authenticationv1.TokenReview) (Identity, error) {
	if review == nil || !review.Status.Authenticated || review.Status.User.UID == "" {
		return Identity{}, ErrInvalid
	}
	audienceOK := false
	for _, audience := range review.Status.Audiences {
		if audience == a.audience {
			audienceOK = true
		}
	}
	if !audienceOK {
		return Identity{}, ErrInvalid
	}
	const prefix = "system:serviceaccount:"
	parts := strings.Split(strings.TrimPrefix(review.Status.User.Username, prefix), ":")
	if !strings.HasPrefix(review.Status.User.Username, prefix) || len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return Identity{}, ErrInvalid
	}
	requiredGroups := map[string]bool{
		"system:serviceaccounts":             false,
		"system:serviceaccounts:" + parts[0]: false,
	}
	for _, group := range review.Status.User.Groups {
		if _, required := requiredGroups[group]; required {
			requiredGroups[group] = true
		}
	}
	for _, present := range requiredGroups {
		if !present {
			return Identity{}, ErrInvalid
		}
	}
	return Identity{Namespace: parts[0], Name: parts[1], UID: review.Status.User.UID}, nil
}

// jwtExpiry reads an unverified JWT expiry solely to cap cache lifetime.
func jwtExpiry(token string) (time.Time, bool) {
	// TokenReview is authoritative. The unverified expiry can only shorten the
	// 30-second positive cache; it is never used to authenticate the token.
	parsed, err := jwt.ParseInsecure([]byte(token))
	if err != nil {
		return time.Time{}, false
	}
	return parsed.Expiration()
}

// contextKey keeps authenticated identities private to this package.
type contextKey struct{}

// IdentityFromContext returns the identity attached by Middleware.
func IdentityFromContext(ctx context.Context) (Identity, bool) {
	identity, ok := ctx.Value(contextKey{}).(Identity)
	return identity, ok
}

// Middleware authenticates bearer tokens before invoking the next handler.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		header := request.Header.Get("Authorization")
		if !strings.HasPrefix(header, "Bearer ") || strings.Contains(strings.TrimPrefix(header, "Bearer "), " ") {
			writeAuthError(response, http.StatusUnauthorized)
			return
		}
		identity, err := a.Authenticate(request.Context(), strings.TrimPrefix(header, "Bearer "))
		if err != nil {
			if errors.Is(err, ErrUnavailable) {
				writeAuthError(response, http.StatusServiceUnavailable)
			} else {
				writeAuthError(response, http.StatusUnauthorized)
			}
			return
		}
		next.ServeHTTP(response, request.WithContext(context.WithValue(request.Context(), contextKey{}, identity)))
	})
}

// writeAuthError emits a non-cacheable authentication failure.
func writeAuthError(response http.ResponseWriter, status int) {
	response.Header().Set("Content-Type", "text/plain; charset=utf-8")
	response.Header().Set("Cache-Control", "no-store")
	response.WriteHeader(status)
	_, _ = response.Write([]byte(http.StatusText(status) + "\n"))
}
