// Source Gateway <https://sourcegateway.dev>
// Copyright The Source Gateway Authors
// SPDX-License-Identifier: Apache-2.0

package policy

import (
	"strings"
	"testing"

	"github.com/sourcegateway/sourcegateway/api/v1alpha1"
)

// TestAuthorize verifies matching and nonmatching policy evaluations.
func TestAuthorize(t *testing.T) {
	compiled, err := CompileObject([]v1alpha1.ObjectAuthorizationRule{
		{Name: "namespace", Operations: []string{"object.read"}, Allow: `sa.namespace == "agents" && request.key.startsWith("workspaces/" + sa.name + "/")`},
	})
	if err != nil {
		t.Fatal(err)
	}
	allowed, matched, err := compiled.Authorize(ServiceAccount{Namespace: "agents", Name: "builder", UID: "uid"}, Request{Operation: "object.read", Key: "workspaces/builder/result"})
	if err != nil || !allowed || len(matched) != 1 || matched[0] != "namespace" {
		t.Fatalf("Authorize() = %v, %v, %v", allowed, matched, err)
	}
	allowed, matched, err = compiled.Authorize(ServiceAccount{Namespace: "other", Name: "builder"}, Request{Operation: "object.read", Key: "workspaces/builder/result"})
	if err != nil || allowed || len(matched) != 0 {
		t.Fatalf("denied Authorize() = %v, %v, %v", allowed, matched, err)
	}
}

// TestCompileRejectsInvalidPolicies covers policy validation boundaries.
func TestCompileRejectsInvalidPolicies(t *testing.T) {
	tests := map[string][]v1alpha1.GitAuthorizationRule{
		"duplicate names": {
			{Name: "same", Operations: []string{"git.read"}, Allow: "true"},
			{Name: "same", Operations: []string{"git.read"}, Allow: "true"},
		},
		"wrong operation": {{Name: "bad", Operations: []string{"object.read"}, Allow: "true"}},
		"non boolean":     {{Name: "bad", Operations: []string{"git.read"}, Allow: `"yes"`}},
		"unknown field":   {{Name: "bad", Operations: []string{"git.read"}, Allow: `request.pod == "secret"`}},
		"object field":    {{Name: "bad", Operations: []string{"git.read"}, Allow: `request.key == "secret"`}},
		"oversize":        {{Name: "bad", Operations: []string{"git.read"}, Allow: strings.Repeat(" ", maxExpression) + "true"}},
		"regex":           {{Name: "bad", Operations: []string{"git.read"}, Allow: `request.branch.matches("` + strings.Repeat("a", maxRegexText+1) + `")`}},
	}
	for name, rules := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := CompileGit(rules); err == nil {
				t.Fatal("CompileGit() unexpectedly succeeded")
			}
		})
	}
}

// TestCompileObjectRejectsOtherVocabularies verifies object policies use only current bucket and object operations.
func TestCompileObjectRejectsOtherVocabularies(t *testing.T) {
	for _, operation := range []string{"git.read", "object.list-buckets", "object.head-bucket"} {
		rules := []v1alpha1.ObjectAuthorizationRule{{Name: "bad", Operations: []string{operation}, Allow: "true"}}
		if _, err := CompileObject(rules); err == nil {
			t.Fatalf("CompileObject() accepted %q", operation)
		}
	}
	rules := []v1alpha1.ObjectAuthorizationRule{{Name: "bad", Operations: []string{"object.read"}, Allow: `request.branch == "main"`}}
	if _, err := CompileObject(rules); err == nil {
		t.Fatal("CompileObject() accepted a Git request field")
	}
}

// TestAuthorizeRejectsCrossDomainOperations verifies each compiled policy retains its operation vocabulary.
func TestAuthorizeRejectsCrossDomainOperations(t *testing.T) {
	gitPolicy, err := CompileGit([]v1alpha1.GitAuthorizationRule{{Name: "allow", Operations: []string{"git.read"}, Allow: "true"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := gitPolicy.Authorize(ServiceAccount{}, Request{Operation: "object.read"}); err == nil {
		t.Fatal("Git policy accepted an object operation")
	}
	objectPolicy, err := CompileObject([]v1alpha1.ObjectAuthorizationRule{{Name: "allow", Operations: []string{"object.read"}, Allow: "true"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := objectPolicy.Authorize(ServiceAccount{}, Request{Operation: "git.read"}); err == nil {
		t.Fatal("object policy accepted a Git operation")
	}
}
