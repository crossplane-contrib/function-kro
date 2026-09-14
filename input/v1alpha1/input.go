// Package v1alpha1 contains the input type for this Function
// +kubebuilder:object:generate=true
// +groupName=kro.fn.crossplane.io
// +versionName=v1alpha1
package v1alpha1

import (
	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// This isn't a custom resource, in the sense that we never install its CRD.
// It is a KRM-like object, so we generate a CRD to describe its schema.

// ResourceGraph can be used to provide input to this Function.
// +kubebuilder:object:root=true
// +kubebuilder:storageversion
// +kubebuilder:resource:categories=crossplane
type ResourceGraph struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// The status schema of the ResourceGraph using CEL expressions.
	// +kubebuilder:validation:Optional
	Status runtime.RawExtension `json:"status,omitempty"`

	// The resources that are part of the ResourceGraph.
	// +kubebuilder:validation:Optional
	Resources []*v1alpha1.Resource `json:"resources,omitempty"`
}
