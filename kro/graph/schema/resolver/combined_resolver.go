// Package resolver provides schema resolution strategies for function-kro.
//
// This file contains function-kro additions that are not present in upstream KRO.
// These provide combined resolvers that pair Crossplane-provided schemas (or CRD-extracted
// schemas) with a core resolver for built-in Kubernetes types.
package resolver

import (
	"k8s.io/apiextensions-apiserver/pkg/generated/openapi"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/cel/openapi/resolver"
	"k8s.io/client-go/kubernetes/scheme"
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

// newCoreResolver creates a resolver for built-in Kubernetes types.
func newCoreResolver() resolver.SchemaResolver {
	return resolver.NewDefinitionsSchemaResolver(
		openapi.GetOpenAPIDefinitions,
		scheme.Scheme,
	)
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
