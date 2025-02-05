// Copyright 2025 The Kube Resource Orchestrator Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package schema

import (
	"sync"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/generated/openapi"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/cel/openapi/resolver"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/kube-openapi/pkg/validation/spec"
)

// NewCombinedResolver creates a new schema resolver that can resolve both core and client types.
func NewCombinedResolver(crds ...*extv1.CustomResourceDefinition) (resolver.SchemaResolver, error) {
	// CoreResolver is a resolver that uses the OpenAPI definitions to resolve
	// core types. It is used to resolve types that are known at compile time.
	coreResolver := resolver.NewDefinitionsSchemaResolver(
		openapi.GetOpenAPIDefinitions,
		scheme.Scheme,
	)

	crdResolver, err := NewCRDSchemaResolver(crds...)
	if err != nil {
		return nil, err
	}

	return coreResolver.Combine(crdResolver), nil
}

// NewCRDSchemaResolver returns a resolver.SchemaResolver backed by CRDs.
func NewCRDSchemaResolver(crds ...*extv1.CustomResourceDefinition) (*CRDSchemaResolver, error) {
	schemas := make(map[schema.GroupVersionKind]*spec.Schema)

	for _, crd := range crds {
		for _, v := range crd.Spec.Versions {
			// Derived from https://github.com/kubernetes/apiextensions-apiserver/blob/v0.32.1/pkg/controller/openapi/builder/builder.go#L112-L116
			internal := &apiextensions.CustomResourceValidation{}
			if err := extv1.Convert_v1_CustomResourceValidation_To_apiextensions_CustomResourceValidation(v.Schema, internal, nil); err != nil {
				continue
			}
			// TODO(negz): Should we validate the schema before passing it to
			// NewStructural? Is it safe to assume that they're valid because we
			// read them from the API server?
			ss, err := structuralschema.NewStructural(internal.OpenAPIV3Schema)
			if err != nil {
				continue
			}

			schemas[schema.GroupVersionKind{
				Group:   crd.Spec.Group,
				Version: v.Name,
				Kind:    crd.Spec.Names.Kind,
			}] = ss.ToKubeOpenAPI()
		}
	}

	return &CRDSchemaResolver{schemas: schemas}, nil
}

// CRDSchemaResolver is resolver.SchemaResolver backed by CRDs.
type CRDSchemaResolver struct {
	schemas map[schema.GroupVersionKind]*spec.Schema
	mx      sync.RWMutex // Protects schemas.
}

// ResolveSchema takes a GroupVersionKind (GVK) and returns the OpenAPI schema
// identified by the GVK.
func (r *CRDSchemaResolver) ResolveSchema(gvk schema.GroupVersionKind) (*spec.Schema, error) {
	r.mx.RLock()
	defer r.mx.RUnlock()
	return r.schemas[gvk], nil
}
