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
	"fmt"
	"sync"

	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/kube-openapi/pkg/validation/spec"

	kroschema "github.com/upbound/function-kro/kro/graph/schema"
)

// CRDSchemaResolver is a resolver.SchemaResolver backed by a set of CRDs.
// It extracts OpenAPI schemas from CRD validation schemas.
type CRDSchemaResolver struct {
	// schemas maps GVK to the extracted spec.Schema
	schemas map[schema.GroupVersionKind]*spec.Schema
	mx      sync.RWMutex
}

// NewCRDSchemaResolver creates a new CRDSchemaResolver from a set of CRDs.
// It extracts the OpenAPI schema from each CRD's validation schema and
// injects the ObjectMeta schema for the metadata field.
func NewCRDSchemaResolver(crds []*extv1.CustomResourceDefinition) (*CRDSchemaResolver, error) {
	schemas := make(map[schema.GroupVersionKind]*spec.Schema)

	for _, crd := range crds {
		if crd == nil {
			continue
		}

		group := crd.Spec.Group
		kind := crd.Spec.Names.Kind

		for _, version := range crd.Spec.Versions {
			if version.Schema == nil || version.Schema.OpenAPIV3Schema == nil {
				continue
			}

			gvk := schema.GroupVersionKind{
				Group:   group,
				Version: version.Name,
				Kind:    kind,
			}

			// Convert CRD's JSONSchemaProps to spec.Schema
			specSchema, err := kroschema.ConvertJSONSchemaPropsToSpecSchema(version.Schema.OpenAPIV3Schema)
			if err != nil {
				return nil, fmt.Errorf("failed to convert schema for %s: %w", gvk, err)
			}

			// CRD schemas typically have a metadata field, but it's usually just
			// {type: object} without the full ObjectMeta schema. Replace it with
			// our fully-resolved ObjectMeta schema so CEL expressions like
			// ${resource.metadata.name} work correctly.
			if specSchema.Properties == nil {
				specSchema.Properties = make(map[string]spec.Schema)
			}
			specSchema.Properties["metadata"] = kroschema.ObjectMetaSchema

			schemas[gvk] = specSchema
		}
	}

	return &CRDSchemaResolver{schemas: schemas}, nil
}

// ResolveSchema returns the OpenAPI schema for the given GVK.
func (r *CRDSchemaResolver) ResolveSchema(gvk schema.GroupVersionKind) (*spec.Schema, error) {
	r.mx.RLock()
	defer r.mx.RUnlock()
	return r.schemas[gvk], nil
}

// GetSchemaForGVK returns the schema for a specific GVK, or nil if not found.
// This is a convenience method for callers who need direct access to a schema.
func (r *CRDSchemaResolver) GetSchemaForGVK(gvk schema.GroupVersionKind) *spec.Schema {
	r.mx.RLock()
	defer r.mx.RUnlock()
	return r.schemas[gvk]
}
