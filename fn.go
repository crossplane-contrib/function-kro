package main

import (
	"context"

	"github.com/upbound/function-kro/input/v1beta1"
	"github.com/upbound/function-kro/kro/graph"
	kroschema "github.com/upbound/function-kro/kro/graph/schema"
	schemaresolver "github.com/upbound/function-kro/kro/graph/schema/resolver"
	"github.com/upbound/function-kro/kro/runtime"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/kube-openapi/pkg/validation/spec"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/fieldpath"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
	fnv1 "github.com/crossplane/function-sdk-go/proto/v1"
	"github.com/crossplane/function-sdk-go/request"
	"github.com/crossplane/function-sdk-go/resource"
	"github.com/crossplane/function-sdk-go/resource/composed"
	"github.com/crossplane/function-sdk-go/resource/composite"
	"github.com/crossplane/function-sdk-go/response"
)

// Function returns whatever response you ask it to.
type Function struct {
	fnv1.UnimplementedFunctionRunnerServiceServer

	log logging.Logger
}

// RunFunction runs the Function.
func (f *Function) RunFunction(_ context.Context, req *fnv1.RunFunctionRequest) (*fnv1.RunFunctionResponse, error) {
	f.log.Info("Running function", "tag", req.GetMeta().GetTag())

	rsp := response.To(req, response.DefaultTTL)

	rg := &v1beta1.ResourceGraph{}
	if err := request.GetInput(req, rg); err != nil {
		response.Fatal(rsp, errors.Wrapf(err, "cannot get Function input from %T", req))
		return rsp, nil
	}

	oxr, err := request.GetObservedCompositeResource(req)
	if err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot get observed composite resource"))
		return rsp, nil
	}

	// Collect all GVKs we need schemas for: the XR and all resource templates.
	gvks := make([]schema.GroupVersionKind, 0, len(rg.Resources)+1)
	xrGVK := schema.FromAPIVersionAndKind(oxr.Resource.GetAPIVersion(), oxr.Resource.GetKind())
	gvks = append(gvks, xrGVK)
	for _, r := range rg.Resources {
		u := &unstructured.Unstructured{}
		if err := json.Unmarshal(r.Template.Raw, u); err != nil {
			response.Fatal(rsp, errors.Wrapf(err, "cannot unmarshal resource id %q", r.ID))
			return rsp, nil
		}
		gvks = append(gvks, schema.FromAPIVersionAndKind(u.GetAPIVersion(), u.GetKind()))
	}

	// Tell Crossplane we need the OpenAPI schemas for our XR and resource templates.
	rsp.Requirements = RequiredSchemas(gvks...)

	// Process the schemas Crossplane sent us.
	schemas := make(map[schema.GroupVersionKind]*spec.Schema)
	for _, gvk := range gvks {
		s, ok := req.GetRequiredSchemas()[gvk.String()]
		if !ok {
			// Crossplane hasn't sent us this required schema yet. Return so it can.
			f.log.Debug("Required schema doesn't appear in required_schemas - returning requirements", "gvk", gvk.String())
			return rsp, nil
		}

		if s.GetOpenapiV3() == nil {
			// Crossplane is telling us the required schema doesn't exist or couldn't be found.
			// This might be okay for built-in Kubernetes types which we have compiled-in schemas for.
			f.log.Debug("Required schema has no OpenAPI v3 content, will try built-in resolver", "gvk", gvk.String())
			continue
		}

		specSchema, err := schemaresolver.StructToSpecSchema(s.GetOpenapiV3())
		if err != nil {
			f.log.Debug("Cannot convert schema", "gvk", gvk, "error", err)
			response.Fatal(rsp, errors.Wrapf(err, "cannot convert schema for %q", gvk))
			return rsp, nil
		}

		f.log.Debug("Retrieved required schema", "gvk", gvk.String(), "specSchema", specSchema)

		// CRD schemas from Crossplane may have a metadata field, but it's typically
		// just an unresolved $ref to ObjectMeta rather than the full schema.
		// Always replace it with our fully-resolved ObjectMeta schema so the
		// parser can validate CEL expressions like ${vpc.metadata.name}.
		// Long-term fix: Crossplane should resolve $refs before sending schemas.
		if specSchema.Properties == nil {
			specSchema.Properties = make(map[string]spec.Schema)
		}
		specSchema.Properties["metadata"] = kroschema.ObjectMetaSchema

		schemas[gvk] = specSchema
	}

	// Create a combined schema resolver that uses Crossplane-provided schemas
	// first, falling back to built-in Kubernetes type schemas.
	schemaMapResolver := schemaresolver.NewSchemaMapResolver(schemas)
	combinedResolver := schemaresolver.NewCombinedResolverFromSchemas(schemaMapResolver)

	// Pass nil for REST mapper - we don't need GVR/namespace info since
	// Crossplane handles resource creation directly.
	gb := graph.NewBuilder(combinedResolver, nil)

	// Get the XR schema for the graph definition.
	xrSchema, err := combinedResolver.ResolveSchema(xrGVK)
	if err != nil {
		response.Fatal(rsp, errors.Wrapf(err, "cannot resolve schema for XR %q", xrGVK))
		return rsp, nil
	}
	if xrSchema == nil {
		response.Fatal(rsp, errors.Errorf("schema for XR %q not found", xrGVK))
		return rsp, nil
	}

	g, err := gb.NewResourceGraphDefinition(rg, xrSchema)
	if err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot create resource graph"))
		return rsp, nil
	}

	rt, err := g.NewGraphRuntime(&oxr.Resource.Unstructured)
	if err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot get graph runtime"))
		return rsp, nil
	}

	ocds, err := request.GetObservedComposedResources(req)
	if err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot get observed composed resources"))
		return rsp, nil
	}

	ready := make(map[string]bool)

	for name, r := range ocds {
		id := string(name)
		rt.SetResource(id, &r.Resource.Unstructured)

		if ready, reason, err := rt.IsResourceReady(id); err != nil || !ready {
			f.log.Info("Resource isn't ready yet", "id", id, "reason", reason, "err", err)
			continue
		}

		ready[id] = true
	}

	dcds, err := request.GetDesiredComposedResources(req)
	if err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot get desired composed resources"))
		return rsp, nil
	}

	for _, id := range rt.TopologicalOrder() {
		if want, err := rt.ReadyToProcessResource(id); err != nil || !want {
			f.log.Info("Skipping resource", "id", id, "err", err)
			rt.IgnoreResource(id)
			continue
		}

		// Use GetRenderedResource to get the template with CEL expressions
		// resolved, rather than GetResource which returns observed state.
		// This is critical for SSA - desired state must only contain fields
		// we want to own, not provider-defaulted fields from observed state.
		r, state := rt.GetRenderedResource(id)
		if state != runtime.ResourceStateResolved {
			f.log.Info("Skipping unresolved resource", "id", id, "state", state)
			continue
		}

		cd, err := composed.From(r)
		if err != nil {
			response.Fatal(rsp, errors.Wrapf(err, "cannot create composed resource from template id %s", id))
			return rsp, nil
		}
		dcds[resource.Name(id)] = &resource.DesiredComposed{Resource: cd, Ready: resource.ReadyFalse}
		if ready[id] {
			dcds[resource.Name(id)].Ready = resource.ReadyTrue
		}

		if _, err := rt.Synchronize(); err != nil {
			response.Fatal(rsp, errors.Wrap(err, "cannot synchronize instance"))
			return rsp, nil
		}
	}

	if err := response.SetDesiredComposedResources(rsp, dcds); err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot set desired composed resources"))
		return rsp, nil
	}

	// Build a minimal desired XR containing only the status paths declared in
	// the ResourceGraph. This is critical for SSA - we must only include fields
	// we want to own. The runtime's GetInstance() returns the full observed XR
	// with status fields mutated in-place, but including all of those fields
	// would cause the function to claim SSA ownership of every field.
	dxr := &composite.Unstructured{Unstructured: unstructured.Unstructured{Object: map[string]any{}}}
	dxr.SetAPIVersion(oxr.Resource.GetAPIVersion())
	dxr.SetKind(oxr.Resource.GetKind())

	src := fieldpath.Pave(rt.GetInstance().Object)
	dst := fieldpath.Pave(dxr.Object)
	for _, v := range g.Instance.GetVariables() {
		val, err := src.GetValue(v.Path)
		if err != nil {
			// Value not resolved yet (CEL dependency not satisfied), skip it.
			continue
		}
		if err := dst.SetValue(v.Path, val); err != nil {
			response.Fatal(rsp, errors.Wrapf(err, "cannot set desired XR status field %q", v.Path))
			return rsp, nil
		}
	}

	if err := response.SetDesiredCompositeResource(rsp, &resource.Composite{Resource: dxr}); err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot set desired composite resource"))
		return rsp, nil
	}

	return rsp, nil
}

// RequiredSchemas returns the schema requirements for the given GVKs.
// This tells Crossplane which OpenAPI schemas the function needs.
func RequiredSchemas(gvks ...schema.GroupVersionKind) *fnv1.Requirements {
	rq := &fnv1.Requirements{Schemas: map[string]*fnv1.SchemaSelector{}}

	for _, gvk := range gvks {
		rq.Schemas[gvk.String()] = &fnv1.SchemaSelector{
			ApiVersion: gvk.GroupVersion().String(),
			Kind:       gvk.Kind,
		}
	}

	return rq
}
