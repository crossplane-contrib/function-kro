package schemaresolver

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/kube-openapi/pkg/validation/spec"
)

// SchemaMapResolver is a resolver.SchemaResolver backed by a map of GVK to
// OpenAPI schemas. This is used when schemas are provided directly (e.g., from
// Crossplane's required_schemas) rather than extracted from CRDs.
// The map is read-only after construction.
type SchemaMapResolver struct {
	schemas map[schema.GroupVersionKind]*spec.Schema
}

// NewSchemaMapResolver creates a new SchemaMapResolver from a map of GVK to
// spec.Schema.
func NewSchemaMapResolver(schemas map[schema.GroupVersionKind]*spec.Schema) *SchemaMapResolver {
	return &SchemaMapResolver{schemas: schemas}
}

// ResolveSchema returns the OpenAPI schema for the given GVK.
func (r *SchemaMapResolver) ResolveSchema(gvk schema.GroupVersionKind) (*spec.Schema, error) {
	return r.schemas[gvk], nil
}
