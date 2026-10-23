// Source Gateway <https://sourcegateway.dev>
// Copyright The Source Gateway Authors
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GitAuthorization holds policy rules for a Git repository.
type GitAuthorization struct {
	// +kubebuilder:validation:MaxItems=32
	// +listType=map
	// +listMapKey=name
	Rules []GitAuthorizationRule `json:"rules,omitempty"`
}

// GitAuthorizationRule grants Git operations when its CEL expression allows them.
type GitAuthorizationRule struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:items:Enum=git.read;git.push
	// +listType=set
	Operations []string `json:"operations"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=4096
	Allow string `json:"allow"`
}

// ObjectAuthorization holds policy rules for an object-storage bucket.
type ObjectAuthorization struct {
	// +kubebuilder:validation:MaxItems=32
	// +listType=map
	// +listMapKey=name
	Rules []ObjectAuthorizationRule `json:"rules,omitempty"`
}

// ObjectAuthorizationRule grants bucket or object operations when its CEL expression allows them.
type ObjectAuthorizationRule struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:items:Enum=bucket.list;bucket.head;object.list;object.read;object.write;object.delete
	// +listType=set
	Operations []string `json:"operations"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=4096
	Allow string `json:"allow"`
}

// SourceStatus records reconciliation progress shared by source kinds.
type SourceStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +listType=map
	// +listMapKey=type
	Conditions     []metav1.Condition `json:"conditions,omitempty"`
	PolicyRevision string             `json:"policyRevision,omitempty"`
}

// GitRepositorySpec describes an upstream Git repository and its local mirror.
type GitRepositorySpec struct {
	Upstream      GitUpstream      `json:"upstream"`
	Mirror        GitMirror        `json:"mirror"`
	Authorization GitAuthorization `json:"authorization"`
}

// GitUpstream identifies the repository and optional credential used to reach it.
type GitUpstream struct {
	// +kubebuilder:validation:Pattern=`^https?://.+`
	URL string `json:"url"`
	// Credential names an operator-configured credential. Omit it for anonymous
	// upstream access; clients still require authentication and authorization.
	// +optional
	// +kubebuilder:validation:MinLength=1
	Credential string `json:"credential,omitempty"`
}

// GitMirror controls mirror freshness.
type GitMirror struct {
	RefreshInterval metav1.Duration `json:"refreshInterval"`
	MaxStaleness    metav1.Duration `json:"maxStaleness"`
}

// GitRepositoryStatus reports the state and local path of a Git mirror.
type GitRepositoryStatus struct {
	SourceStatus `json:",inline"`
	Path         string `json:"path,omitempty"`
}

// GitRepository is a Git source managed by Source Gateway.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=sggit
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Path",type=string,JSONPath=`.status.path`
type GitRepository struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              GitRepositorySpec   `json:"spec,omitempty"`
	Status            GitRepositoryStatus `json:"status,omitempty"`
}

// GitRepositoryList contains GitRepository resources.
// +kubebuilder:object:root=true
type GitRepositoryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GitRepository `json:"items"`
}

// ObjectStorageBucketSpec describes an object-storage backend and local cache.
type ObjectStorageBucketSpec struct {
	Backend       ObjectBackend       `json:"backend"`
	Cache         ObjectCache         `json:"cache"`
	Authorization ObjectAuthorization `json:"authorization"`
}

// ObjectBackend identifies a bucket and the credential used to reach it.
type ObjectBackend struct {
	// +kubebuilder:validation:Enum=s3;gcs
	Provider string `json:"provider"`
	// Endpoint is the object storage API base URL. Its origin must match the
	// selected credential's effective origin. The provider default is used when omitted.
	Endpoint string `json:"endpoint,omitempty"`
	Region   string `json:"region,omitempty"`
	// +kubebuilder:validation:MinLength=1
	Bucket string `json:"bucket"`
	// +kubebuilder:validation:MinLength=1
	Credential string `json:"credential"`
}

// ObjectCache controls local object retention.
type ObjectCache struct {
	Capacity resource.Quantity `json:"capacity"`
	TTL      metav1.Duration   `json:"ttl"`
}

// ObjectStorageBucketStatus reports the state and exposed bucket name.
type ObjectStorageBucketStatus struct {
	SourceStatus `json:",inline"`
	BucketName   string `json:"bucketName,omitempty"`
}

// ObjectStorageBucket is an object source managed by Source Gateway.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=sgbucket
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Bucket",type=string,JSONPath=`.status.bucketName`
type ObjectStorageBucket struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ObjectStorageBucketSpec   `json:"spec,omitempty"`
	Status            ObjectStorageBucketStatus `json:"status,omitempty"`
}

// ObjectStorageBucketList contains ObjectStorageBucket resources.
// +kubebuilder:object:root=true
type ObjectStorageBucketList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ObjectStorageBucket `json:"items"`
}
