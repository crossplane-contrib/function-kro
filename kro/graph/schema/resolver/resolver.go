// Copyright 2025 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package resolver

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

// NewCombinedResolver creates a new schema resolver that can resolve both core
// and CRD types. This version is adapted for Crossplane functions which receive
// CRDs directly rather than using API server discovery.
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
			if v.Schema == nil || v.Schema.OpenAPIV3Schema == nil {
				continue
			}

			// Derived from https://github.com/kubernetes/apiextensions-apiserver/blob/v0.32.1/pkg/controller/openapi/builder/builder.go#L112-L116
			internal := &apiextensions.CustomResourceValidation{}
			if err := extv1.Convert_v1_CustomResourceValidation_To_apiextensions_CustomResourceValidation(v.Schema, internal, nil); err != nil {
				continue
			}

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
