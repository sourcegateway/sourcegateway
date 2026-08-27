// Source Gateway <https://sourcegateway.dev>
// Copyright The Source Gateway Authors
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
)

// fakeReviewer records review calls and returns a fixed result.
type fakeReviewer struct {
	review *authenticationv1.TokenReview
	err    error
	calls  int
}

// Review implements Reviewer for authentication tests.
func (r *fakeReviewer) Review(context.Context, *authenticationv1.TokenReview) (*authenticationv1.TokenReview, error) {
	r.calls++
	return r.review, r.err
}

// validReview returns a successful ServiceAccount review.
func validReview() *authenticationv1.TokenReview {
	return &authenticationv1.TokenReview{Status: authenticationv1.TokenReviewStatus{
		Authenticated: true,
		Audiences:     []string{"sourcegateway.dev"},
		User: authenticationv1.UserInfo{
			Username: "system:serviceaccount:agents:builder",
			UID:      "uid-1",
			Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:agents"},
		},
	}}
}

// TestAuthenticateValidatesAndCaches checks validation and expiry-aware caching.
func TestAuthenticateValidatesAndCaches(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	claims := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, now.Add(10*time.Second).Unix())))
	token := header + "." + claims + "."
	reviewer := &fakeReviewer{review: validReview()}
	authenticator := New(reviewer, "sourcegateway.dev")
	authenticator.now = func() time.Time { return now }

	identity, err := authenticator.Authenticate(t.Context(), token)
	if err != nil || identity != (Identity{Namespace: "agents", Name: "builder", UID: "uid-1"}) {
		t.Fatalf("Authenticate() = %#v, %v", identity, err)
	}
	if _, err := authenticator.Authenticate(t.Context(), token); err != nil || reviewer.calls != 1 {
		t.Fatalf("cached Authenticate() calls = %d, error = %v", reviewer.calls, err)
	}
	now = now.Add(11 * time.Second)
	if _, err := authenticator.Authenticate(t.Context(), token); err != nil || reviewer.calls != 2 {
		t.Fatalf("expired Authenticate() calls = %d, error = %v", reviewer.calls, err)
	}
}

// TestAuthenticateRejectsInvalidTokenReviews checks malformed and unavailable reviews.
func TestAuthenticateRejectsInvalidTokenReviews(t *testing.T) {
	tests := map[string]func(*authenticationv1.TokenReview){
		"unauthenticated": func(review *authenticationv1.TokenReview) { review.Status.Authenticated = false },
		"wrong audience":  func(review *authenticationv1.TokenReview) { review.Status.Audiences = []string{"other"} },
		"empty UID":       func(review *authenticationv1.TokenReview) { review.Status.User.UID = "" },
		"wrong username":  func(review *authenticationv1.TokenReview) { review.Status.User.Username = "person" },
		"missing groups":  func(review *authenticationv1.TokenReview) { review.Status.User.Groups = nil },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			review := validReview()
			mutate(review)
			_, err := New(&fakeReviewer{review: review}, "sourcegateway.dev").Authenticate(t.Context(), "token")
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("error = %v, want ErrInvalid", err)
			}
		})
	}
	reviewError := errors.New("API unavailable")
	_, err := New(&fakeReviewer{err: reviewError}, "sourcegateway.dev").Authenticate(t.Context(), "token")
	if !errors.Is(err, ErrUnavailable) || !errors.Is(err, reviewError) {
		t.Fatalf("error = %v, want wrapped availability errors", err)
	}
}
