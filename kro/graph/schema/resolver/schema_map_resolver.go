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
	"encoding/json"
	"fmt"
	"sync"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/cel/openapi/resolver"
	"k8s.io/kube-openapi/pkg/validation/spec"
)

// SchemaMapResolver is a resolver.SchemaResolver backed by a map of GVK to
// OpenAPI schemas. This is used when schemas are provided directly (e.g., from
// Crossplane's required_schemas) rather than extracted from CRDs.
type SchemaMapResolver struct {
	schemas map[schema.GroupVersionKind]*spec.Schema
	mx      sync.RWMutex
}

// NewSchemaMapResolver creates a new SchemaMapResolver from a map of GVK to
// spec.Schema.
func NewSchemaMapResolver(schemas map[schema.GroupVersionKind]*spec.Schema) *SchemaMapResolver {
	return &SchemaMapResolver{schemas: schemas}
}

// ResolveSchema returns the OpenAPI schema for the given GVK.
func (r *SchemaMapResolver) ResolveSchema(gvk schema.GroupVersionKind) (*spec.Schema, error) {
	r.mx.RLock()
	defer r.mx.RUnlock()
	return r.schemas[gvk], nil
}

// Combine returns a new SchemaResolver that first tries this resolver,
// then falls back to the provided resolver if not found.
func (r *SchemaMapResolver) Combine(fallback resolver.SchemaResolver) resolver.SchemaResolver {
	return &combinedResolver{
		primary:  r,
		fallback: fallback,
	}
}

// combinedResolver tries resolvers in order until one returns a schema.
type combinedResolver struct {
	primary  resolver.SchemaResolver
	fallback resolver.SchemaResolver
}

func (c *combinedResolver) ResolveSchema(gvk schema.GroupVersionKind) (*spec.Schema, error) {
	// Try primary resolver first
	s, err := c.primary.ResolveSchema(gvk)
	if err != nil {
		return nil, err
	}
	if s != nil {
		return s, nil
	}

	// Fall back to secondary resolver
	return c.fallback.ResolveSchema(gvk)
}

// StructToSpecSchema converts a protobuf Struct (as returned by Crossplane's
// required_schemas) to a kube-openapi spec.Schema.
func StructToSpecSchema(s *structpb.Struct) (*spec.Schema, error) {
	if s == nil {
		return nil, fmt.Errorf("schema struct is nil")
	}

	// Convert protobuf Struct to JSON bytes
	jsonBytes, err := protojson.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal struct to JSON: %w", err)
	}

	// Unmarshal JSON into spec.Schema
	schema := &spec.Schema{}
	if err := json.Unmarshal(jsonBytes, schema); err != nil {
		return nil, fmt.Errorf("failed to unmarshal JSON to spec.Schema: %w", err)
	}

	return schema, nil
}
