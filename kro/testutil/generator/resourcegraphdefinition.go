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

package generator

import (
	"encoding/json"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/kube-openapi/pkg/validation/spec"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"

	input "github.com/crossplane-contrib/function-kro/input/v1alpha1"
)

// ResourceGraphDefinitionOption is a functional option for ResourceGraph
type ResourceGraphDefinitionOption func(*input.ResourceGraph)

// SchemaOption is a functional option for Schema configuration.
// In function-kro, schema is handled externally, so this is kept for
// test compatibility but the schema data is stored on the ResourceGraph
// as annotations or status for the test to extract.
type SchemaOption func(map[string]interface{})

// NewResourceGraphDefinition creates a new ResourceGraph with the given name and options.
// This is adapted from upstream KRO's generator to work with function-kro's
// input.ResourceGraph type instead of v1alpha1.ResourceGraphDefinition.
func NewResourceGraphDefinition(name string, opts ...ResourceGraphDefinitionOption) *input.ResourceGraph {
	rg := &input.ResourceGraph{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	}

	for _, opt := range opts {
		opt(rg)
	}
	return rg
}

// WithSchema sets the status schema of the ResourceGraph.
// In function-kro, the XR schema is resolved externally (from Crossplane),
// so this only sets the status expressions. The spec/status maps from
// upstream are adapted: status becomes the ResourceGraph.Status field,
// and spec information is stored as annotations for test extraction.
func WithSchema(kind, version string, spec, status map[string]interface{}, opts ...SchemaOption) ResourceGraphDefinitionOption {
	return func(rg *input.ResourceGraph) {
		if status != nil {
			rawStatus, err := json.Marshal(status)
			if err != nil {
				panic(err)
			}
			rg.Status = runtime.RawExtension{
				Object: &unstructured.Unstructured{Object: status},
				Raw:    rawStatus,
			}
		}

		// Store schema metadata as annotations so tests can extract it
		if rg.Annotations == nil {
			rg.Annotations = make(map[string]string)
		}
		rg.Annotations["test-schema-kind"] = kind
		rg.Annotations["test-schema-version"] = version
		if spec != nil {
			rawSpec, err := json.Marshal(spec)
			if err != nil {
				panic(err)
			}
			rg.Annotations["test-schema-spec"] = string(rawSpec)
		}

		// Reset pendingTypes before applying opts
		pendingTypes = nil

		// Apply schema options (may set pendingTypes via WithTypes)
		for _, opt := range opts {
			opt(status)
		}

		// Store types if set
		if pendingTypes != nil {
			rawTypes, err := json.Marshal(pendingTypes)
			if err != nil {
				panic(err)
			}
			rg.Annotations["test-schema-types"] = string(rawTypes)
			pendingTypes = nil
		}
	}
}

// WithExternalRef adds an external reference to the ResourceGraph.
func WithExternalRef(
	id string,
	externalRef *v1alpha1.ExternalRef,
	readyWhen []string,
	includeWhen []string,
) ResourceGraphDefinitionOption {
	return func(rg *input.ResourceGraph) {
		rg.Resources = append(rg.Resources, &v1alpha1.Resource{
			ID:          id,
			ReadyWhen:   readyWhen,
			IncludeWhen: includeWhen,
			ExternalRef: externalRef,
		})
	}
}

// WithExternalRefAndForEach adds an external reference with forEach iterators.
// This is an invalid combination and should fail validation - used for testing.
func WithExternalRefAndForEach(
	id string,
	externalRef *v1alpha1.ExternalRef,
	forEach []v1alpha1.ForEachDimension,
) ResourceGraphDefinitionOption {
	return func(rg *input.ResourceGraph) {
		rg.Resources = append(rg.Resources, &v1alpha1.Resource{
			ID:          id,
			ExternalRef: externalRef,
			ForEach:     forEach,
		})
	}
}

// WithResource adds a resource to the ResourceGraph with the given name and definition.
func WithResource(
	id string,
	template map[string]interface{},
	readyWhen []string,
	includeWhen []string,
) ResourceGraphDefinitionOption {
	return func(rg *input.ResourceGraph) {
		raw, err := json.Marshal(template)
		if err != nil {
			panic(err)
		}
		rg.Resources = append(rg.Resources, &v1alpha1.Resource{
			ID:          id,
			ReadyWhen:   readyWhen,
			IncludeWhen: includeWhen,
			Template: runtime.RawExtension{
				Object: &unstructured.Unstructured{Object: template},
				Raw:    raw,
			},
		})
	}
}

// WithTypes returns a SchemaOption that stores types metadata in annotations.
// The types map custom type names to their field definitions, which are used
// by BuildTestXRSchema to resolve type references in spec fields.
func WithTypes(types map[string]interface{}) SchemaOption {
	return func(_ map[string]interface{}) {
		// Types will be stored by the WithSchema closure after opts are applied.
		// We use a package-level variable to pass the data through. This is safe
		// because tests run sequentially per package.
		pendingTypes = types
	}
}

// pendingTypes holds types from the last WithTypes call for use by WithSchema.
var pendingTypes map[string]interface{}

// WithScope returns a SchemaOption that stores scope metadata.
// In function-kro, scope is always namespace-scoped from our perspective.
func WithScope(scope string) SchemaOption {
	return func(_ map[string]interface{}) {
		// Scope is always namespace-scoped in function-kro
	}
}

// BuildTestXRSchema constructs a *spec.Schema suitable for passing as the xrSchema
// parameter to Builder.NewResourceGraphDefinition. It reads the spec definition
// stored in the ResourceGraph's annotations by WithSchema and converts SimpleSchema
// type strings ("string", "integer", "boolean", "[]string", etc.) into OpenAPI schemas.
//
// The returned schema has the standard Kubernetes object shape:
// apiVersion, kind, metadata, spec (from the annotation), and status (empty object).
func BuildTestXRSchema(rg *input.ResourceGraph) *spec.Schema {
	specProps := make(map[string]spec.Schema)

	// Load custom types if present
	var customTypes map[string]interface{}
	if rg.Annotations != nil {
		if rawTypes, ok := rg.Annotations["test-schema-types"]; ok && rawTypes != "" {
			_ = json.Unmarshal([]byte(rawTypes), &customTypes)
		}
	}

	if rg.Annotations != nil {
		if rawSpec, ok := rg.Annotations["test-schema-spec"]; ok && rawSpec != "" {
			var specMap map[string]interface{}
			if err := json.Unmarshal([]byte(rawSpec), &specMap); err == nil {
				for k, v := range specMap {
					specProps[k] = simpleSchemaToSpecWithTypes(v, customTypes)
				}
			}
		}
	}

	return &spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type: []string{"object"},
			Properties: map[string]spec.Schema{
				"apiVersion": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
				"kind":       {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
				"metadata": {
					SchemaProps: spec.SchemaProps{
						Type: []string{"object"},
						Properties: map[string]spec.Schema{
							"name":      {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
							"namespace": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
							"labels": {
								SchemaProps: spec.SchemaProps{
									Type: []string{"object"},
									AdditionalProperties: &spec.SchemaOrBool{
										Allows: true,
										Schema: &spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
									},
								},
							},
							"annotations": {
								SchemaProps: spec.SchemaProps{
									Type: []string{"object"},
									AdditionalProperties: &spec.SchemaOrBool{
										Allows: true,
										Schema: &spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
									},
								},
							},
						},
					},
				},
				"spec": {
					SchemaProps: spec.SchemaProps{
						Type:       []string{"object"},
						Properties: specProps,
					},
				},
				"status": {
					SchemaProps: spec.SchemaProps{
						Type:       []string{"object"},
						Properties: map[string]spec.Schema{},
					},
				},
			},
		},
	}
}

// simpleSchemaToSpecWithTypes converts a SimpleSchema type value to an OpenAPI spec.Schema.
// Supported formats: "string", "integer", "boolean", "[]string", "[]integer",
// with optional defaults like "string | default=foo", "integer | default=3",
// and custom type references that are resolved via the customTypes map.
func simpleSchemaToSpecWithTypes(v interface{}, customTypes map[string]interface{}) spec.Schema {
	s, ok := v.(string)
	if !ok {
		// If not a string, treat as an object (nested map)
		if m, ok := v.(map[string]interface{}); ok {
			props := make(map[string]spec.Schema)
			for k, val := range m {
				props[k] = simpleSchemaToSpecWithTypes(val, customTypes)
			}
			return spec.Schema{
				SchemaProps: spec.SchemaProps{
					Type:       []string{"object"},
					Properties: props,
				},
			}
		}
		return spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{"string"}}}
	}

	// Strip default suffix: "string | default=foo" → "string"
	typePart := s
	if idx := strings.Index(s, "|"); idx >= 0 {
		typePart = strings.TrimSpace(s[:idx])
	}

	// Handle array types: "[]string", "[]integer", "[]customType"
	if strings.HasPrefix(typePart, "[]") {
		elemType := typePart[2:]
		// Check if element type is a custom type
		if customTypes != nil {
			if typeDef, ok := customTypes[elemType]; ok {
				elemSchema := simpleSchemaToSpecWithTypes(typeDef, customTypes)
				return spec.Schema{
					SchemaProps: spec.SchemaProps{
						Type: []string{"array"},
						Items: &spec.SchemaOrArray{
							Schema: &elemSchema,
						},
					},
				}
			}
		}
		return spec.Schema{
			SchemaProps: spec.SchemaProps{
				Type: []string{"array"},
				Items: &spec.SchemaOrArray{
					Schema: &spec.Schema{
						SchemaProps: spec.SchemaProps{Type: []string{elemType}},
					},
				},
			},
		}
	}

	// Check if this is a custom type reference
	if customTypes != nil {
		if typeDef, ok := customTypes[typePart]; ok {
			return simpleSchemaToSpecWithTypes(typeDef, customTypes)
		}
	}

	return spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{typePart}}}
}

// WithResourceCollection adds a collection resource with forEach iterators.
func WithResourceCollection(
	id string,
	template map[string]interface{},
	forEach []v1alpha1.ForEachDimension,
	readyWhen []string,
	includeWhen []string,
) ResourceGraphDefinitionOption {
	return func(rg *input.ResourceGraph) {
		raw, err := json.Marshal(template)
		if err != nil {
			panic(err)
		}
		rg.Resources = append(rg.Resources, &v1alpha1.Resource{
			ID:          id,
			ReadyWhen:   readyWhen,
			IncludeWhen: includeWhen,
			ForEach:     forEach,
			Template: runtime.RawExtension{
				Object: &unstructured.Unstructured{Object: template},
				Raw:    raw,
			},
		})
	}
}
