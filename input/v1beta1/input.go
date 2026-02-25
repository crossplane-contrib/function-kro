// Package v1beta1 contains the input type for this Function
// +kubebuilder:object:generate=true
// +groupName=kro.fn.crossplane.io
// +versionName=v1beta1
package v1beta1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtime "k8s.io/apimachinery/pkg/runtime"
)

// KRODomainName is the base domain name for KRO resources.
// This constant is used across the codebase for labels, finalizers, etc.
const KRODomainName = "kro.run"

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
	Resources []*Resource `json:"resources,omitempty"`
}

// ExternalRefMetadata contains metadata for referencing an external resource.
type ExternalRefMetadata struct {
	// Name is the name of the external resource to reference.
	// +kubebuilder:validation:Required
	Name string `json:"name,omitempty"`
	// Namespace is the namespace of the external resource.
	// If empty, the instance's namespace will be used.
	// +kubebuilder:validation:Optional
	Namespace string `json:"namespace,omitempty"`
}

// ExternalRef is a reference to an external resource that already exists in the cluster.
// It allows you to read and use existing resources in your ResourceGraph
// without creating them.
type ExternalRef struct {
	// APIVersion is the API version of the external resource.
	// +kubebuilder:validation:Required
	APIVersion string `json:"apiVersion"`
	// Kind is the kind of the external resource.
	// +kubebuilder:validation:Required
	Kind string `json:"kind"`
	// Metadata contains the name and optional namespace of the external resource.
	// +kubebuilder:validation:Required
	Metadata ExternalRefMetadata `json:"metadata"`
}

// ForEachDimension is a map with exactly one entry where the key is the iterator
// variable name and the value is a CEL expression that returns a list.
type ForEachDimension map[string]string

// Resource represents a Kubernetes resource that is part of the ResourceGraph.
// Each resource can either be created using a template or reference an existing resource.
type Resource struct {
	// ID is a unique identifier for this resource within the ResourceGraph.
	// It is used to reference this resource in CEL expressions from other resources.
	// +kubebuilder:validation:Required
	ID string `json:"id,omitempty"`

	// Template is the Kubernetes resource manifest to create.
	// It can contain CEL expressions (using ${...} syntax) that reference other resources.
	// Exactly one of template or externalRef must be provided.
	// +kubebuilder:validation:Optional
	Template runtime.RawExtension `json:"template,omitempty"`

	// ExternalRef references an existing resource in the cluster instead of creating one.
	// This is useful for reading existing resources and using their values in other resources.
	// Exactly one of template or externalRef must be provided.
	// +kubebuilder:validation:Optional
	ExternalRef *ExternalRef `json:"externalRef,omitempty"`

	// ReadyWhen is a list of CEL expressions that determine when this resource is considered ready.
	// All expressions must evaluate to true for the resource to be ready.
	// If not specified, the resource is considered ready when it exists.
	// +kubebuilder:validation:Optional
	ReadyWhen []string `json:"readyWhen,omitempty"`

	// IncludeWhen is a list of CEL expressions that determine whether this resource should be created.
	// All expressions must evaluate to true for the resource to be included.
	// If not specified, the resource is always included.
	// +kubebuilder:validation:Optional
	IncludeWhen []string `json:"includeWhen,omitempty"`

	// ForEach expands this resource into a collection of resources.
	// Each entry is a map with exactly one key-value pair where the key is the
	// iterator variable name and the value is a CEL expression returning a list.
	// +kubebuilder:validation:Optional
	ForEach []ForEachDimension `json:"forEach,omitempty"`
}
