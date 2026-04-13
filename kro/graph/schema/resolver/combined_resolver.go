// Package resolver provides schema resolution strategies for function-kro.
//
// This file contains function-kro additions that are not present in upstream KRO.
// These provide combined resolvers that pair Crossplane-provided schemas (or CRD-extracted
// schemas) with a core resolver for built-in Kubernetes types.
package resolver

import (
	"maps"

	"github.com/crossplane-contrib/function-kro/schemas"
	"k8s.io/apiextensions-apiserver/pkg/generated/openapi"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/cel/openapi/resolver"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/kube-openapi/pkg/common"
	"k8s.io/kube-openapi/pkg/validation/spec"
)

// NewCombinedResolverFromSchemas creates a schema resolver that combines
// a schema map resolver (for Crossplane-provided schemas) with a core resolver
// (for built-in Kubernetes types).
func NewCombinedResolverFromSchemas(schemaMapResolver *SchemaMapResolver) resolver.SchemaResolver {
	coreResolver := newCoreResolver()
	return &combinedResolver{
		primary:  schemaMapResolver,
		fallback: coreResolver,
	}
}

// NewCombinedResolverFromCRDs creates a schema resolver that combines
// a CRD schema resolver with a core resolver.
func NewCombinedResolverFromCRDs(crdResolver *CRDSchemaResolver) resolver.SchemaResolver {
	coreResolver := newCoreResolver()
	return &combinedResolver{
		primary:  crdResolver,
		fallback: coreResolver,
	}
}

// newCoreResolver creates a resolver for built-in Kubernetes types using
// compiled-in OpenAPI definitions. This handles types like Deployment, Service,
// ConfigMap, etc. that are part of the core Kubernetes API.
//
// It merges two definition sources into a single resolver so that $ref
// resolution works across both:
//   - generated definitions: common API groups (core/v1, apps/v1, batch/v1,
//     rbac/v1, networking/v1, policy/v1, storage/v1, autoscaling/v2, coordination/v1)
//   - apiextensions-apiserver definitions: CRD types, meta/v1 (ObjectMeta,
//     LabelSelector, etc.), and other apimachinery types
func newCoreResolver() resolver.SchemaResolver {
	return resolver.NewDefinitionsSchemaResolver(
		mergedOpenAPIDefinitions,
		scheme.Scheme,
	)
}

// mergedOpenAPIDefinitions returns OpenAPI definitions from both our generated
// definitions and the apiextensions-apiserver definitions. Our generated
// definitions take priority for overlapping types.
func mergedOpenAPIDefinitions(ref common.ReferenceCallback) map[string]common.OpenAPIDefinition {
	// Start with apiextensions-apiserver definitions (meta/v1, CRD types, etc.)
	merged := openapi.GetOpenAPIDefinitions(ref)
	// Layer our generated definitions on top (core/v1, apps/v1, etc.)
	maps.Copy(merged, schemas.GetOpenAPIDefinitions(ref))

	return merged
}

// combinedResolver tries resolvers in order until one returns a schema.
type combinedResolver struct {
	primary  resolver.SchemaResolver
	fallback resolver.SchemaResolver
}

func (c *combinedResolver) ResolveSchema(gvk schema.GroupVersionKind) (*spec.Schema, error) {
	s, err := c.primary.ResolveSchema(gvk)
	if err != nil {
		return nil, err
	}
	if s != nil {
		return s, nil
	}
	return c.fallback.ResolveSchema(gvk)
}
