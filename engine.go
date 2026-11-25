package main

import (
	"encoding/json"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/cel"
	kroschema "github.com/kubernetes-sigs/kro/pkg/graph/schema"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/rgdadapter"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8sjson "k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apiserver/pkg/cel/openapi/resolver"
	"k8s.io/kube-openapi/pkg/validation/spec"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"

	input "github.com/crossplane-contrib/function-kro/input/v1alpha1"
)

// resourceGraphDefinition wraps our ResourceGraph input in a KRO
// ResourceGraphDefinition, so KRO's RGD adapter can use it as is. The
// schema block only has the XR's group, version, kind and scope. It has no
// SimpleSchema spec because we get the XR's OpenAPI schema from Crossplane
// instead.
func resourceGraphDefinition(rg *input.ResourceGraph, xr *unstructured.Unstructured) *v1alpha1.ResourceGraphDefinition {
	gvk := xr.GroupVersionKind()
	scope := v1alpha1.ResourceScopeNamespaced
	if xr.GetNamespace() == "" {
		scope = v1alpha1.ResourceScopeCluster
	}
	return &v1alpha1.ResourceGraphDefinition{
		Name: rg.GetName(),
		Spec: v1alpha1.ResourceGraphDefinitionSpec{
			Schema: &v1alpha1.Schema{
				Group:      gvk.Group,
				APIVersion: gvk.Version,
				Kind:       gvk.Kind,
				Scope:      scope,
				Status:     rg.Status,
			},
			Resources: rg.Resources,
		},
	}
}

// restMapperFor returns a REST mapper for every GVK the graph uses. A function
// can't ask the API server which kinds are namespaced, so we make some educated
// guesses here to include them in the REST mapper we'll give to KRO that it
// uses to fill in namespaces and to check templates later on.
//
// 1. Crossplane only lets a namespaced XR compose resources in its own
// namespace, so for a namespaced XR every kind is namespaced.
//
// 2. For a cluster-scoped XR, a kind is namespaced only if some template or
// external ref sets a namespace on it.
//
// Because we just infer the scope from the templates, and we don't know if the
// author has made a real mistake, KRO won't have the information it needs to
// do a real check. This loses some strict validation capabilities of KRO, but
// is a trade-off we accept for functions not having API server access.
func restMapperFor(xr *unstructured.Unstructured, resources []*v1alpha1.Resource) (meta.RESTMapper, error) {
	rm := meta.NewDefaultRESTMapper(nil)

	xrNamespaced := xr.GetNamespace() != ""
	xrScope := meta.RESTScopeRoot
	if xrNamespaced {
		xrScope = meta.RESTScopeNamespace
	}
	rm.Add(xr.GroupVersionKind(), xrScope)

	namespaced := map[schema.GroupVersionKind]bool{}
	for _, r := range resources {
		var u unstructured.Unstructured
		switch {
		case r.ExternalRef != nil:
			u.SetAPIVersion(r.ExternalRef.APIVersion)
			u.SetKind(r.ExternalRef.Kind)
			u.SetNamespace(r.ExternalRef.Metadata.Namespace)
		default:
			if err := k8sjson.Unmarshal(r.Template.Raw, &u.Object); err != nil {
				return nil, errors.Wrapf(err, "cannot unmarshal resource id %q", r.ID)
			}
		}
		gvk := u.GroupVersionKind()
		namespaced[gvk] = namespaced[gvk] || xrNamespaced || u.GetNamespace() != ""
	}
	for gvk, ns := range namespaced {
		scope := meta.RESTScopeRoot
		if ns {
			scope = meta.RESTScopeNamespace
		}
		rm.Add(gvk, scope)
	}
	return rm, nil
}

// newXRSchemaCompiler wraps c so CEL that reads the `schema` variable is
// checked against the XR's OpenAPI schema. Standalone KRO uses the RGD's
// SimpleSchema, which a composition function doesn't have; Crossplane sends
// the XR's OpenAPI schema instead. Pass the result to
// rgdadapter.BuildRuntimeForInstance.
//
// BuildRuntimeForInstance sets its own type for the `schema` node with
// compiler.WithNodeSchemaOverride. We append ours after its options, and the
// compiler keeps the last override for a node.
func newXRSchemaCompiler(c rgdadapter.Compiler, rgd *v1alpha1.ResourceGraphDefinition, xrSchema *spec.Schema) (rgdadapter.Compiler, error) {
	s, err := instanceSchemaForCEL(rgd, xrSchema)
	if err != nil {
		return nil, err
	}
	return &xrSchemaCompiler{Compiler: c, schema: s}, nil
}

type xrSchemaCompiler struct {
	rgdadapter.Compiler

	schema *spec.Schema
}

func (c *xrSchemaCompiler) CompileWithOptions(g *v1alpha1.Graph, opts ...compiler.CompileOption) (*compiler.Program, error) {
	// Use the XR's schema for schema.*, including the context property
	// RunFunction adds. Without it, CEL that reads schema.spec or
	// schema.context fails to compile.
	opts = append(opts, compiler.WithNodeSchemaOverride(rgdadapter.SchemaNodeID, c.schema))
	return c.Compiler.CompileWithOptions(g, opts...)
}

// instanceSchemaForCEL builds the type of the `schema` variable from the XR's
// schema, the way KRO's graph.InstanceSchemaForCEL builds it from the RGD's
// SimpleSchema. It drops status, because KRO doesn't let resources read the
// XR's status, and replaces metadata with KRO's standard ObjectMeta schema,
// without namespace for a cluster-scoped XR.
func instanceSchemaForCEL(rgd *v1alpha1.ResourceGraphDefinition, xrSchema *spec.Schema) (*spec.Schema, error) {
	out, err := copySchema(xrSchema)
	if err != nil {
		return nil, errors.Wrap(err, "cannot copy XR schema")
	}
	delete(out.Properties, "status")
	if out.Properties == nil {
		out.Properties = map[string]spec.Schema{}
	}
	out.Properties["metadata"] = kroschema.ObjectMetaSchema
	if rgd.Spec.Schema != nil && rgd.Spec.Schema.Scope == v1alpha1.ResourceScopeCluster {
		out.Properties["metadata"] = kroschema.NamespacelessObjectMetaSchema
	}
	return out, nil
}

// copySchema deep copies a schema. spec.Schema has no DeepCopy, so we
// round-trip it through JSON.
func copySchema(s *spec.Schema) (*spec.Schema, error) {
	data, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	out := &spec.Schema{}
	if err := json.Unmarshal(data, out); err != nil {
		return nil, err
	}
	return out, nil
}

// schemalessStatusResolver returns the XR's schema with status as an open
// object. KRO checks the ResourceGraph's status fields against the XR's
// schema. Standalone KRO generates the status schema from those same fields,
// so they always match, but an XRD's status schema is written by hand and
// might not. So we don't check status against the XRD.
type schemalessStatusResolver struct {
	resolver.SchemaResolver

	gvk schema.GroupVersionKind
}

func (r *schemalessStatusResolver) ResolveSchema(gvk schema.GroupVersionKind) (*spec.Schema, error) {
	s, err := r.SchemaResolver.ResolveSchema(gvk)
	if err != nil || s == nil || gvk != r.gvk {
		return s, err
	}
	out, err := copySchema(s)
	if err != nil {
		return nil, errors.Wrap(err, "cannot copy XR schema")
	}
	if out.Properties == nil {
		out.Properties = map[string]spec.Schema{}
	}
	status := spec.Schema{Type: []string{"object"}}
	status.AddExtension(cel.XKubernetesPreserveUnknownFields, true)
	out.Properties["status"] = status
	return out, nil
}
