// Source Gateway <https://sourcegateway.dev>
// Copyright The Source Gateway Authors
// SPDX-License-Identifier: Apache-2.0

// +kubebuilder:object:generate=true
// +groupName=sourcegateway.dev
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	GroupVersion  = schema.GroupVersion{Group: "sourcegateway.dev", Version: "v1alpha1"}
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)
	AddToScheme   = SchemeBuilder.AddToScheme
)

// addKnownTypes registers Source Gateway resources with a scheme.
func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(GroupVersion,
		&GitRepository{}, &GitRepositoryList{},
		&ObjectStorageBucket{}, &ObjectStorageBucketList{},
	)
	metav1.AddToGroupVersion(scheme, GroupVersion)
	return nil
}
