package main

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"maps"
	"strings"

	xpv1 "github.com/crossplane/crossplane/apis/v2/apiextensions/v1"
	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	"github.com/gobuffalo/flect"
	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/cel"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/rgdadapter"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/runtime"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8sjson "k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apiserver/pkg/cel/openapi/resolver"
	"k8s.io/kube-openapi/pkg/validation/spec"

	"github.com/crossplane/crossplane-runtime/v2/pkg/errors"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"

	fnv1 "github.com/crossplane/function-sdk-go/proto/v1"
	"github.com/crossplane/function-sdk-go/request"
	"github.com/crossplane/function-sdk-go/resource"
	"github.com/crossplane/function-sdk-go/resource/composite"
	"github.com/crossplane/function-sdk-go/response"

	input "github.com/crossplane-contrib/function-kro/input/v1alpha1"
	schemaresolver "github.com/crossplane-contrib/function-kro/internal/schemaresolver"
	"github.com/crossplane-contrib/function-kro/kro/graphengine/executor"
)

// Function runs a KRO ResourceGraph as a Crossplane composition function.
type Function struct {
	fnv1.UnimplementedFunctionRunnerServiceServer

	log       logging.Logger
	rgdConfig graph.Config
}

// NewFunction returns a Function with the supplied configuration. A nil logger
// defaults to a no-op logger.
func NewFunction(log logging.Logger, rgdConfig graph.Config) *Function {
	if log == nil {
		log = logging.NewNopLogger()
	}
	return &Function{log: log, rgdConfig: rgdConfig}
}

// RunFunction runs the Function.
func (f *Function) RunFunction(ctx context.Context, req *fnv1.RunFunctionRequest) (*fnv1.RunFunctionResponse, error) {
	f.log.Debug("Running function", "tag", req.GetMeta().GetTag(), "advertisesCapabilities", request.AdvertisesCapabilities(req), "capabilities", req.GetMeta().GetCapabilities())
	rsp := response.To(req, response.DefaultTTL)

	// Get the input resource graph
	rg := &input.ResourceGraph{}
	if err := request.GetInput(req, rg); err != nil {
		response.Fatal(rsp, errors.Wrapf(err, "cannot get Function input from %T", req))
		return rsp, nil
	}

	// Get the observed XR and decode it in a format KRO expects
	oxr, err := request.GetObservedCompositeResource(req)
	if err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot get observed composite resource"))
		return rsp, nil
	}

	if err := decodeAsK8sAPI(&oxr.Resource.Unstructured); err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot decode observed composite resource as Kubernetes API object"))
		return rsp, nil
	}

	// Collect all GVKs we need schemas for, which is the XR and every resource
	// template and external ref.
	xrGVK := schema.FromAPIVersionAndKind(oxr.Resource.GetAPIVersion(), oxr.Resource.GetKind())
	gvks, err := collectGVKs(xrGVK, rg.Resources)
	if err != nil {
		response.Fatal(rsp, err)
		return rsp, nil
	}

	// Request the schemas we need in the function response so Crossplane will
	// send them to us as part of the next request. Do this on every function
	// run so our requirements are stable.
	requireSchemas(req, rsp, gvks)

	// Build the schema resolver from the schemas that Crossplane has provided to us.
	resolver, xrSchema, err := f.buildResolver(req, gvks, xrGVK)
	if err != nil {
		response.Fatal(rsp, errors.Wrapf(err, "cannot process schemas"))
		return rsp, nil
	} else if resolver == nil {
		f.log.Debug("Waiting for Crossplane to provide schemas")
		return rsp, nil
	}

	// Expose the function context to CEL as schema.context. Context keys and
	// values are whatever earlier pipeline steps wrote, so we can't give them
	// a schema. Values are nullable, so if a template reads a field under a
	// context key whose value is null, its resource waits instead of failing.
	xrSchema.Properties["context"] = spec.Schema{
		Type: []string{"object"},
		AdditionalProperties: &spec.SchemaOrBool{
			Schema: &spec.Schema{
				Nullable:   true,
				Extensions: spec.Extensions{cel.XKubernetesPreserveUnknownFields: true},
			},
		},
	}

	// KRO limits how many dimensions a forEach can have when it validates an
	// RGD, which we don't run, so we check it here.
	for _, r := range rg.Resources {
		if len(r.ForEach) > f.rgdConfig.MaxCollectionDimensionSize {
			response.Fatal(rsp, errors.Errorf("resource %q: forEach cannot have more than %d dimensions, got %d", r.ID, f.rgdConfig.MaxCollectionDimensionSize, len(r.ForEach)))
			return rsp, nil
		}
	}

	// Wrap the input as a KRO ResourceGraphDefinition, and tell KRO which kinds
	// are namespaced.
	xr := &oxr.Resource.Unstructured
	rgd := resourceGraphDefinition(rg, xr)
	rm, err := restMapperFor(xr, rg.Resources)
	if err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot get REST mapper"))
		return rsp, nil
	}

	// The context isn't part of the XR, so we set it on a copy that only the
	// runtime sees. It's decoded like the XR so integer math on context values
	// works in CEL.
	fnContext := &unstructured.Unstructured{}
	if err := resource.AsObject(req.GetContext(), fnContext); err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot create context object"))
		return rsp, nil
	}
	if err := decodeAsK8sAPI(fnContext); err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot decode context object"))
		return rsp, nil
	}
	xrc := xr.DeepCopy()
	xrc.Object["context"] = fnContext.Object

	// Create the KRO runtime from the graph and XR. This will perform a type
	// check on every CEL expression against the XR's schema, including the
	// context property we added above.
	c, err := newXRSchemaCompiler(compiler.NewCompilerWithDependencies(&schemalessStatusResolver{SchemaResolver: resolver, gvk: xrGVK}, rm), rgd, xrSchema)
	if err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot create compiler"))
		return rsp, nil
	}
	rt, _, err := rgdadapter.BuildRuntimeForInstance(rgd, xr, c,
		runtime.WithMaxCollectionSize(f.rgdConfig.MaxCollectionSize),
		// Read schema.* from the copy of the XR with the context. Otherwise KRO
		// reads only the XR's standard fields like apiVersion, kind, metadata,
		// spec and status, and leaves out the context we added.
		runtime.WithNodeObjectOverride(rgdadapter.SchemaNodeID, xrc))
	if err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot create graph runtime"))
		return rsp, nil
	}

	// KRO needs the API server to look up an apiVersion or kind computed by a
	// CEL expression, so we don't support them.
	for _, n := range rt.Nodes() {
		if n.DynamicGVK() {
			response.Fatal(rsp, errors.Errorf("resource %q: CEL expressions in apiVersion or kind are not supported", n.ID()))
			return rsp, nil
		}
	}

	// Gather the external resources Crossplane fetched for our external refs.
	required, err := requiredExternalResources(req, rg.Resources)
	if err != nil {
		response.Fatal(rsp, err)
		return rsp, nil
	}

	// Group observed composed resources by their runtime node ID.
	ocds, err := request.GetObservedComposedResources(req)
	if err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot get observed composed resources"))
		return rsp, nil
	}
	observed := groupObservedByNodeID(f.log, ocds, rt)
	for id, objs := range observed {
		if err := decodeAsK8sAPI(objs...); err != nil {
			response.Fatal(rsp, errors.Wrapf(err, "cannot decode observed composed resources for %q as Kubernetes API objects", id))
			return rsp, nil
		}
	}

	// Process all resources in dependency order, generating the desired
	// composed resources.
	e := executor.NewCrossplane(observed, required)
	err = e.Apply(ctx, rt)

	// Ask Crossplane to fetch the external resources this run referenced. A
	// reference whose name or selector reads data that doesn't exist yet isn't
	// asked for yet.
	if rsp.GetRequirements().GetResources() == nil {
		rsp.Requirements.Resources = make(map[string]*fnv1.ResourceSelector)
	}
	maps.Copy(rsp.GetRequirements().GetResources(), e.ExternalRefSelectors)

	// A resource that can't be rendered would be left out of desired state and
	// deleted by Crossplane. Fail instead, so Crossplane leaves everything as it
	// is.
	//
	// If we're still waiting on Crossplane to fetch an external ref though, we
	// don't fail yet, because Crossplane won't fetch anything for us after a
	// fatal result. The resource we couldn't render isn't in e.Desired, so this
	// response leaves it out. That's fine, because Crossplane will fetch the ref,
	// call us again, and only use the response from that last call.
	if err != nil && !stderrors.Is(err, executor.ErrNotReady) && !fetching(e.ExternalRefSelectors, required) {
		response.Fatal(rsp, err)
		return rsp, nil
	}
	allReady := err == nil
	if !allReady {
		f.log.Debug("Resources not ready yet", "reason", err)
	}

	dcds, err := request.GetDesiredComposedResources(req)
	if err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot get desired composed resources"))
		return rsp, nil
	}
	maps.Copy(dcds, e.Desired)
	if err := response.SetDesiredComposedResources(rsp, dcds); err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot set desired composed resources"))
		return rsp, nil
	}

	// Build a minimal desired XR containing only the status paths declared in
	// the ResourceGraph. This is critical for SSA - we must only include fields
	// we want to own.
	dxr := &composite.Unstructured{Object: map[string]any{}}
	dxr.SetAPIVersion(xr.GetAPIVersion())
	dxr.SetKind(xr.GetKind())
	if len(e.Status) > 0 {
		dxr.Object["status"] = e.Status
	}
	if err := response.SetDesiredCompositeResource(rsp, &resource.Composite{Resource: dxr}); err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot set desired composite resource"))
		return rsp, nil
	}

	// evaluate custom conditions and include them in the response
	setAuthorConditions(f.log, rsp, rt, rgd, allReady)

	return rsp, nil
}

// decodeAsK8sAPI re-decodes the supplied objects in place using the same JSON decoder the API
// server uses, so KRO sees the data exactly as it would reading straight from the API server.
//
// Standalone KRO reads observed state from the API server. We instead receive it from Crossplane
// over gRPC, where every JSON number is a double and function-sdk-go leaves them as float64. The
// most visible consequence is integers: KRO is happy with what it gets from the API server (int64),
// but is not happy with the float64 type we get from gRPC/function-sdk-go if we were to pass that
// directly to KRO.
//
// Round-tripping through k8sjson.Unmarshal re-decodes the data more like how reading from the API
// server would, so the data format KRO gets matches what it expects.
func decodeAsK8sAPI(objs ...*unstructured.Unstructured) error {
	for _, u := range objs {
		if u == nil || u.Object == nil {
			continue
		}
		b, err := json.Marshal(u.Object)
		if err != nil {
			return errors.Wrapf(err, "cannot marshal %s/%s", u.GetKind(), u.GetName())
		}
		out := map[string]any{}
		if err := k8sjson.Unmarshal(b, &out); err != nil {
			return errors.Wrapf(err, "cannot unmarshal %s/%s", u.GetKind(), u.GetName())
		}
		u.Object = out
	}
	return nil
}

// collectGVKs returns the GVKs for the XR and every resource in the graph.
func collectGVKs(xrGVK schema.GroupVersionKind, resources []*v1alpha1.Resource) ([]schema.GroupVersionKind, error) {
	gvks := make([]schema.GroupVersionKind, 0, len(resources)+1)
	gvks = append(gvks, xrGVK)
	for _, r := range resources {
		if r.ExternalRef != nil {
			gvks = append(gvks, schema.FromAPIVersionAndKind(r.ExternalRef.APIVersion, r.ExternalRef.Kind))
			continue
		}

		u := &unstructured.Unstructured{}
		if err := k8sjson.Unmarshal(r.Template.Raw, u); err != nil {
			return nil, errors.Wrapf(err, "cannot unmarshal resource id %q", r.ID)
		}
		gvks = append(gvks, schema.FromAPIVersionAndKind(u.GetAPIVersion(), u.GetKind()))
	}
	return gvks, nil
}

func requireSchemas(req *fnv1.RunFunctionRequest, rsp *fnv1.RunFunctionResponse, gvks []schema.GroupVersionKind) {
	// If Crossplane supports required_schemas (v2.2+), use those exclusively.
	if request.HasCapability(req, fnv1.Capability_CAPABILITY_REQUIRED_SCHEMAS) {
		for _, gvk := range gvks {
			response.RequireSchema(rsp, gvk.String(), gvk.GroupVersion().String(), gvk.Kind)
		}
		return
	}

	// Crossplane doesn't support required_schemas, fall back to requesting CRDs
	// via required_resources.
	if rsp.GetRequirements() == nil {
		rsp.Requirements = &fnv1.Requirements{}
	}

	resources := map[string]*fnv1.ResourceSelector{}
	for _, gvk := range gvks {
		resources[gvk.String()] = &fnv1.ResourceSelector{
			ApiVersion: "apiextensions.k8s.io/v1",
			Kind:       "CustomResourceDefinition",
			Match: &fnv1.ResourceSelector_MatchName{
				MatchName: strings.ToLower(flect.Pluralize(gvk.Kind) + "." + gvk.Group),
			},
		}
	}

	rsp.Requirements.Resources = resources
}

func (f *Function) buildResolver(req *fnv1.RunFunctionRequest, gvks []schema.GroupVersionKind, xrGVK schema.GroupVersionKind) (resolver.SchemaResolver, *spec.Schema, error) {
	// If Crossplane supports required_schemas (v2.2+), use those exclusively.
	if request.HasCapability(req, fnv1.Capability_CAPABILITY_REQUIRED_SCHEMAS) {
		return f.buildResolverFromSchemas(req, gvks, xrGVK)
	}

	return f.buildResolverFromCRDs(req, gvks, xrGVK)
}

// buildResolverFromSchemas attempts to build a resolver from Crossplane's required_schemas.
// Returns (nil, nil, nil) if schemas aren't available yet (not an error).
// Returns (resolver, xrSchema, nil) on success.
// Returns (nil, nil, error) on fatal errors.
func (f *Function) buildResolverFromSchemas(req *fnv1.RunFunctionRequest, gvks []schema.GroupVersionKind, xrGVK schema.GroupVersionKind) (resolver.SchemaResolver, *spec.Schema, error) {
	reqSchemas := request.GetRequiredSchemas(req)
	if len(reqSchemas) == 0 {
		// Crossplane hasn't sent any schemas yet
		return nil, nil, nil
	}

	schemas := make(map[schema.GroupVersionKind]*spec.Schema)
	for _, gvk := range gvks {
		s, ok := reqSchemas[gvk.String()]
		if !ok {
			// This GVK wasn't in the response, log it but continue
			f.log.Debug("Schema not in required_schemas response", "gvk", gvk.String())
			continue
		}

		if s == nil {
			// Schema exists but has no content, log it but continue
			f.log.Debug("Schema has no OpenAPI v3 content", "gvk", gvk.String())
			continue
		}

		// convert the schema protobuf struct to the schema type KRO expects
		specSchema, err := structToSpecSchema(s)
		if err != nil {
			return nil, nil, errors.Wrapf(err, "cannot convert schema for %q", gvk)
		}

		schemas[gvk] = specSchema
	}

	// Crossplane responded with required schemas, but we have no usable schemas from the response.
	if len(schemas) == 0 {
		return nil, nil, errors.New("no usable schemas found in response")
	}

	// Create the schema map resolver
	schemaMapResolver := schemaresolver.NewSchemaMapResolver(schemas)
	combinedResolver := schemaresolver.NewCombinedResolverFromSchemas(schemaMapResolver)

	// Get XR schema from the combined resolver.
	xrSchema, err := combinedResolver.ResolveSchema(xrGVK)
	if err != nil {
		return nil, nil, errors.Wrapf(err, "cannot resolve schema for XR %q", xrGVK)
	}
	if xrSchema == nil {
		return nil, nil, errors.Errorf("schema for XR %q not found", xrGVK)
	}

	f.log.Debug("Using required_schemas path")
	return combinedResolver, xrSchema, nil
}

// buildResolverFromCRDs attempts to build a resolver by extracting schemas from CRDs.
// Returns (nil, nil, nil) if CRDs aren't available yet (not an error).
// Returns (resolver, xrSchema, nil) on success.
// Returns (nil, nil, error) on fatal errors.
func (f *Function) buildResolverFromCRDs(req *fnv1.RunFunctionRequest, gvks []schema.GroupVersionKind, xrGVK schema.GroupVersionKind) (resolver.SchemaResolver, *spec.Schema, error) {
	requiredResources, err := request.GetRequiredResources(req)
	if err != nil {
		return nil, nil, errors.Wrap(err, "cannot get required resources")
	}
	if len(requiredResources) == 0 {
		// Crossplane hasn't sent any required resources yet.
		return nil, nil, nil
	}

	crds := make([]*extv1.CustomResourceDefinition, 0, len(gvks))
	for _, gvk := range gvks {
		resources, ok := requiredResources[gvk.String()]
		if !ok {
			// This GVK wasn't in the response - Crossplane might not have
			// processed our requirements yet.
			f.log.Debug("CRD not in required_resources response", "gvk", gvk.String())
			return nil, nil, nil
		}

		if len(resources) == 0 {
			// Crossplane is telling us the CRD doesn't exist.
			// This might be a built-in type without a CRD.
			f.log.Debug("CRD unavailable", "gvk", gvk.String())
			continue
		}

		// convert from the unstructured CRD to a strongly typed CRD. Note that
		// we should only have at most one CRD for each GVK.
		ucrd := resources[0].Resource.Object
		crd := &extv1.CustomResourceDefinition{}
		if err := k8sruntime.DefaultUnstructuredConverter.FromUnstructured(ucrd, crd); err != nil {
			return nil, nil, errors.Wrapf(err, "cannot convert CRD for %q", gvk)
		}

		crds = append(crds, crd)
	}

	// Crossplane responded to our request for CRDs, but we have no usable CRDS from the response.
	if len(crds) == 0 {
		return nil, nil, errors.New("no usable CRDs found in response")
	}

	// Create combined resolver from CRDs.
	crdResolver, err := schemaresolver.NewCRDSchemaResolver(crds)
	if err != nil {
		return nil, nil, errors.Wrap(err, "cannot create schema resolver from CRDs")
	}
	combinedResolver := schemaresolver.NewCombinedResolverFromCRDs(crdResolver)

	// Get XR schema from the combined resolver.
	xrSchema, err := combinedResolver.ResolveSchema(xrGVK)
	if err != nil {
		return nil, nil, errors.Wrapf(err, "cannot resolve schema for XR %q", xrGVK)
	}
	if xrSchema == nil {
		return nil, nil, errors.Errorf("schema for XR %q not found", xrGVK)
	}

	f.log.Debug("Using required_resources CRD path")
	return combinedResolver, xrSchema, nil
}

// requiredExternalResources returns the resources Crossplane fetched for each
// external ref, by resource ID. A ref Crossplane hasn't answered yet has no
// entry.
func requiredExternalResources(req *fnv1.RunFunctionRequest, resources []*v1alpha1.Resource) (map[string][]*unstructured.Unstructured, error) {
	all, err := request.GetRequiredResources(req)
	if err != nil {
		return nil, errors.Wrap(err, "cannot get required resources")
	}
	out := map[string][]*unstructured.Unstructured{}
	for _, r := range resources {
		if r.ExternalRef == nil {
			continue
		}
		found, ok := all[r.ID]
		if !ok {
			continue
		}
		objs := make([]*unstructured.Unstructured, len(found))
		for i := range found {
			objs[i] = found[i].Resource
		}
		if err := decodeAsK8sAPI(objs...); err != nil {
			return nil, errors.Wrapf(err, "cannot decode external resources for %q as Kubernetes API objects", r.ID)
		}
		out[r.ID] = objs
	}
	return out, nil
}

// fetching returns true if we've asked Crossplane for an external ref that it
// hasn't fetched for us yet. A ref that Crossplane looked for but didn't find
// still comes back to us as an empty entry, so we don't count that one.
func fetching(selectors map[string]*fnv1.ResourceSelector, required map[string][]*unstructured.Unstructured) bool {
	for id := range selectors {
		if _, ok := required[id]; !ok {
			return true
		}
	}
	return false
}

// groupObservedByNodeID groups observed composed resources by their runtime
// node ID. For single resources, the composed resource name equals the node ID.
// For collections, the composed resource name uses the pattern
// "collectionNodeID-metadataName" (e.g., "subnets-my-app-us-east-1") and has
// the kro.run/collection-index label set.
func groupObservedByNodeID(log logging.Logger, ocds map[resource.Name]resource.ObservedComposed, rt *runtime.Runtime) map[string][]*unstructured.Unstructured {
	observedByNodeID := make(map[string][]*unstructured.Unstructured)
	for name, r := range ocds {
		id := string(name) // ID is the same as the composed resource name
		if n := rt.Node(id); n != nil && n.Kind() == compiler.NodeKindTemplate {
			// found a direct match node for this ID
			observedByNodeID[id] = append(observedByNodeID[id], &r.Resource.Unstructured)
			continue
		}
		if _, isCollectionItem := r.Resource.GetLabels()[metadata.CollectionIndexLabel]; isCollectionItem {
			// this is a collection item, try to find its parent collection node
			if parentNodeID := findCollectionNodeID(id, rt); parentNodeID != "" {
				// we found a matching collection parent, add this observed resource to its parent's list
				observedByNodeID[parentNodeID] = append(observedByNodeID[parentNodeID], &r.Resource.Unstructured)
				continue
			}
		}
		// This resource doesn't match any node - might be from a different function
		// or a stale resource. Skip it.
		log.Debug("Observed resource has no matching node", "name", name)
	}

	return observedByNodeID
}

// findCollectionNodeID finds the collection node that owns a composed resource
// by trying progressively shorter "-" delimited prefixes of the resource name.
// This naturally finds the longest match and avoids ambiguity when one node ID
// is a prefix of another (e.g., "bucket" vs "bucket-log").
func findCollectionNodeID(id string, rt *runtime.Runtime) string {
	// try all segments of the ID from longest to shortest
	for remaining := id; ; {
		// find the next longest segment in the resource name
		idx := strings.LastIndex(remaining, "-")
		if idx <= 0 {
			// no more segments to check, return empty
			return ""
		}
		prefix := id[:idx]
		if n := rt.Node(prefix); n != nil && n.Kind() == compiler.NodeKindTemplate && n.IsCollection() {
			// we found a collection node parent that matches the name prefix of this collection item
			return prefix
		}
		remaining = prefix
	}
}

// setAuthorConditions evaluates the conditions block of the ResourceGraph's
// status (runtime.newCondition(...) expressions) and sets each resulting
// condition on the XR.
func setAuthorConditions(log logging.Logger, rsp *fnv1.RunFunctionResponse, rt *runtime.Runtime, rgd *v1alpha1.ResourceGraphDefinition, allReady bool) {
	status := metav1.ConditionFalse
	if allReady {
		status = metav1.ConditionTrue
	}

	// runtime.condition(schema, ...) reads KRO's built-in instance conditions,
	// which a function doesn't compute, so we approximate them from this run.
	// GraphResolved is always True, because we only get here once the graph has
	// compiled and run without a hard error. ResourcesReady and Ready are True
	// once every resource is ready. InstanceManaged is about KRO's own
	// finalizers and labels, so we leave it out.
	builtins := []v1alpha1.Condition{
		{Type: v1alpha1.InstanceConditionTypeGraphResolved, Status: metav1.ConditionTrue},
		{Type: v1alpha1.InstanceConditionTypeResourcesReady, Status: status},
		{Type: v1alpha1.InstanceConditionTypeReady, Status: status},
	}

	// A condition that's waiting for data, or whose expression fails, is left
	// out. Crossplane only updates the conditions we return, so it keeps its
	// last value on the XR.
	conds, incomplete, err := rgdadapter.ProjectInstanceConditions(rt, rgd, builtins, 0)
	if err != nil {
		response.Warning(rsp, errors.Wrap(err, "cannot evaluate status conditions"))
	}
	if incomplete {
		log.Debug("Some status conditions are waiting for data")
	}
	for _, c := range conds {
		if xpv1.IsSystemConditionType(xpv2.ConditionType(c.ConditionType)) {
			// Crossplane sets these itself, and ignores them from a function.
			response.Warning(rsp, errors.Errorf("cannot set condition %q: Crossplane reserves this condition type", c.ConditionType))
			continue
		}

		var opt *response.ConditionOption
		switch metav1.ConditionStatus(c.Status) { //nolint:exhaustive // anything else is Unknown
		case metav1.ConditionTrue:
			opt = response.ConditionTrue(rsp, c.ConditionType, c.Reason)
		case metav1.ConditionFalse:
			opt = response.ConditionFalse(rsp, c.ConditionType, c.Reason)
		default:
			opt = response.ConditionUnknown(rsp, c.ConditionType, c.Reason)
		}
		if c.Message != "" {
			opt = opt.WithMessage(c.Message)
		}
		opt.TargetComposite()
	}
}

// structToSpecSchema converts a protobuf Struct (as returned by Crossplane's
// required_schemas) to a kube-openapi spec.Schema.
func structToSpecSchema(s *structpb.Struct) (*spec.Schema, error) {
	if s == nil {
		return nil, errors.New("schema struct is nil")
	}

	// Convert protobuf Struct to JSON bytes
	jsonBytes, err := protojson.Marshal(s)
	if err != nil {
		return nil, errors.Wrap(err, "cannot marshal struct to JSON")
	}

	// Unmarshal JSON into spec.Schema
	schema := &spec.Schema{}
	if err := json.Unmarshal(jsonBytes, schema); err != nil {
		return nil, errors.Wrap(err, "cannot unmarshal JSON to spec.Schema")
	}

	return schema, nil
}
