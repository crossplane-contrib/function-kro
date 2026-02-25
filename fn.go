package main

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strconv"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8sjson "k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apiserver/pkg/cel/openapi/resolver"
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

	"github.com/upbound/function-kro/input/v1beta1"
	"github.com/upbound/function-kro/kro/graph"
	schemaresolver "github.com/upbound/function-kro/kro/graph/schema/resolver"
	"github.com/upbound/function-kro/kro/metadata"
	"github.com/upbound/function-kro/kro/runtime"
)

// Function returns whatever response you ask it to.
type Function struct {
	fnv1.UnimplementedFunctionRunnerServiceServer

	log logging.Logger
}

// RunFunction runs the Function.
func (f *Function) RunFunction(_ context.Context, req *fnv1.RunFunctionRequest) (*fnv1.RunFunctionResponse, error) { //nolint:gocognit // See below.
	// This loop is fairly complex, but more readable with less abstraction.

	f.log.Debug("Running function", "tag", req.GetMeta().GetTag(), "advertisesCapabilities", request.AdvertisesCapabilities(req), "capabilities", req.GetMeta().GetCapabilities())
	rsp := response.To(req, response.DefaultTTL)

	// Get the input resource graph
	rg := &v1beta1.ResourceGraph{}
	if err := request.GetInput(req, rg); err != nil {
		response.Fatal(rsp, errors.Wrapf(err, "cannot get Function input from %T", req))
		return rsp, nil
	}

	// Get the observed XR
	oxr, err := request.GetObservedCompositeResource(req)
	if err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot get observed composite resource"))
		return rsp, nil
	}

	// Collect all GVKs we need schemas for, which is the XR and all resource templates.
	gvks := make([]schema.GroupVersionKind, 0, len(rg.Resources)+1)
	xrGVK := schema.FromAPIVersionAndKind(oxr.Resource.GetAPIVersion(), oxr.Resource.GetKind())
	gvks = append(gvks, xrGVK)
	for _, r := range rg.Resources {
		if r.ExternalRef != nil {
			// this is an external ref, we have access to the GVK directly
			gvks = append(gvks, schema.FromAPIVersionAndKind(r.ExternalRef.APIVersion, r.ExternalRef.Kind))
			continue
		}

		// it's a template, unmarshal it into an unstructured so we can access the GVK from that
		u := &unstructured.Unstructured{}
		if err := k8sjson.Unmarshal(r.Template.Raw, u); err != nil {
			response.Fatal(rsp, errors.Wrapf(err, "cannot unmarshal resource id %q", r.ID))
			return rsp, nil
		}
		gvks = append(gvks, schema.FromAPIVersionAndKind(u.GetAPIVersion(), u.GetKind()))
	}

	// Request the schemas we need in the function response so Crossplane will
	// send them to us as part of the next request. Do this on every function
	// run so our requirements are stable.
	f.requireSchemas(req, rsp, gvks)

	// Build the schema resolver from the schemas that Crossplane has provided to us.
	resolver, xrSchema, err := f.buildResolver(req, gvks, xrGVK)
	if err != nil {
		response.Fatal(rsp, errors.Wrapf(err, "cannot process schemas"))
		return rsp, nil
	} else if resolver == nil {
		f.log.Debug("Waiting for Crossplane to provide schemas")
		return rsp, nil
	}

	// Build the KRO graph using the schema resolver.
	gb := graph.NewBuilder(resolver, nil)
	g, err := gb.NewResourceGraphDefinition(rg, xrSchema)
	if err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot create resource graph"))
		return rsp, nil
	}

	// Create the KRO runtime from the graph and XR
	rt, err := runtime.FromGraph(g, &oxr.Resource.Unstructured)
	if err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot create graph runtime"))
		return rsp, nil
	}

	// Build a map from node ID to node for easy lookups later on.
	nodesByID := make(map[string]*runtime.Node)
	for _, node := range rt.Nodes() {
		nodesByID[node.Spec.Meta.ID] = node
	}

	// Process all external references in the input, matching them to required
	// resources that Crossplane provided to us and setting that observed state
	// on their corresponding nodes in the runtime so KRO can use their data later on.
	for _, r := range rg.Resources {
		if r.ExternalRef == nil {
			// not an external reference, skip it
			continue
		}

		// get the required resource that Crossplane provided to us for this external reference
		resources, ok, err := request.GetRequiredResource(req, r.ID)
		if err != nil {
			response.Fatal(rsp, errors.Wrapf(err, "cannot get external resource %q", r.ID))
			return rsp, nil
		}
		if !ok || len(resources) == 0 {
			f.log.Debug("External resource not available yet", "id", r.ID)
			continue
		}

		// we always expect exactly one external resource since we ask for a specific one
		u := resources[0].Resource

		// set the resource's observed state on the runtime node, so KRO has access to it for later evaluations etc.
		if node, ok := nodesByID[r.ID]; ok {
			f.log.Debug("SetObserved external ref resource", "id", r.ID, "name", u.GetName(), "namespace", u.GetNamespace())
			node.SetObserved([]*unstructured.Unstructured{u})
		}
	}

	// Find all external references from the runtime so we can include them in the
	// response's required resources. Basically, we'll get Crossplane to look up
	// external references in the control plane for us.
	externalRefSelectors, err := f.externalRefSelectorsFromRuntime(rt, oxr.Resource.GetNamespace())
	if err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot build external resource selectors"))
		return rsp, nil
	}
	if rsp.Requirements.Resources == nil {
		rsp.Requirements.Resources = make(map[string]*fnv1.ResourceSelector)
	}
	maps.Copy(rsp.GetRequirements().GetResources(), externalRefSelectors)

	// get the observed composed resources
	ocds, err := request.GetObservedComposedResources(req)
	if err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot get observed composed resources"))
		return rsp, nil
	}

	// Group observed composed resources by their runtime node ID.
	// For single resources, the composed resource name equals the node ID.
	// For collections, the composed resource name uses the pattern "nodeID-N" (e.g., "subnets-0")
	// and has the kro.run/collection-index label set.
	observedByNodeID := make(map[string][]*unstructured.Unstructured)
	for name, r := range ocds {
		id := string(name)
		// Try direct match first (single resources).
		if _, ok := nodesByID[id]; ok {
			observedByNodeID[id] = append(observedByNodeID[id], &r.Resource.Unstructured)
			continue
		}
		// Check if this is a collection item (has collection-index label).
		// If so, extract the node ID by stripping the "-N" suffix from the name.
		if _, isCollectionItem := r.Resource.GetLabels()[metadata.CollectionIndexLabel]; isCollectionItem {
			if idx := strings.LastIndex(id, "-"); idx > 0 {
				baseID := id[:idx]
				if node, ok := nodesByID[baseID]; ok && node.Spec.Meta.Type == graph.NodeTypeCollection {
					// this resource is part of a collection, add it to the list of resources for its parent/base node
					observedByNodeID[baseID] = append(observedByNodeID[baseID], &r.Resource.Unstructured)
					continue
				}
			}
		}
		// This resource doesn't match any node - might be from a different function
		// or a stale resource. Skip it.
		f.log.Debug("Observed resource has no matching node", "name", name)
	}

	// Set observed state on each node so the KRO runtime has access to all its
	// observed fields/values to use when evaluating expressions.
	for id, observed := range observedByNodeID {
		nodesByID[id].SetObserved(observed)
	}

	dcds, err := request.GetDesiredComposedResources(req)
	if err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot get desired composed resources"))
		return rsp, nil
	}

	// Process all runtime nodes in topological order, generating the entire set of desired composed resources.
	for _, node := range rt.Nodes() {
		id := node.Spec.Meta.ID

		// External refs are read-only and not managed by this function or Crossplane.
		// Skip them from desired output.
		if node.Spec.Meta.Type == graph.NodeTypeExternal {
			f.log.Debug("Not including external ref in desired resources", "id", id)
			continue
		}

		// Check if this node should be ignored (includeWhen evaluated to false).
		ignored, err := node.IsIgnored()
		if err != nil {
			f.log.Info("Error checking if resource is ignored", "id", id, "err", err)
			continue
		}
		if ignored {
			f.log.Debug("Not including ignored resource in desired resources", "id", id)
			continue
		}

		// Get the desired state with CEL expressions resolved.
		// This is critical for SSA - desired state must only contain fields
		// we want to own, not provider-defaulted fields from observed state.
		desired, err := node.GetDesired()
		if err != nil {
			if runtime.IsDataPending(err) {
				f.log.Debug("Not including resource with pending data in desired resources", "id", id)
				continue
			}
			response.Fatal(rsp, errors.Wrapf(err, "cannot get desired state for resource %q", id))
			return rsp, nil
		}

		// For single resources, desired has one element.
		// For collections, desired has multiple elements (one per forEach expansion).
		isCollection := node.Spec.Meta.Type == graph.NodeTypeCollection
		for i, r := range desired {
			resourceName := id
			if isCollection {
				// This resource is part of a collection: append index to make a unique composed resource name
				resourceName = id + "-" + strconv.Itoa(i)
			}

			cd, err := composed.From(r)
			if err != nil {
				response.Fatal(rsp, errors.Wrapf(err, "cannot create composed resource from template id %s", id))
				return rsp, nil
			}

			// add the resource to the desired composed resources and set its
			// ready state. If readyWhen expressions are defined, we explicitly
			// set ReadyTrue/ReadyFalse based on their evaluation. If no
			// readyWhen is defined, we leave readiness as ReadyUnspecified so
			// that later functions in the pipeline (like function-auto-ready)
			// can determine readiness using their own logic.
			readyState := resource.ReadyUnspecified
			if len(node.Spec.ReadyWhen) > 0 {
				readyState = resource.ReadyFalse
				if isReady, err := node.IsReady(); err != nil {
					f.log.Info("Error checking resource readiness", "id", id, "err", err)
				} else if isReady {
					readyState = resource.ReadyTrue
				}
			}
			f.log.Debug("Resource ready state", "id", id, "ready", readyState)
			dcds[resource.Name(resourceName)] = &resource.DesiredComposed{Resource: cd, Ready: readyState}
		}
	}

	if err := response.SetDesiredComposedResources(rsp, dcds); err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot set desired composed resources"))
		return rsp, nil
	}

	// Build a minimal desired XR containing only the status paths declared in
	// the ResourceGraph. This is critical for SSA - we must only include fields
	// we want to own. The runtime uses soft resolution for instance status,
	// returning only fields where all CEL expressions were successfully resolved.
	dxr := &composite.Unstructured{Unstructured: unstructured.Unstructured{Object: map[string]any{}}}
	dxr.SetAPIVersion(oxr.Resource.GetAPIVersion())
	dxr.SetKind(oxr.Resource.GetKind())

	// Get the resolved status fields from the instance node (KRO runtime node corresponding to the XR).
	instanceDesired, err := rt.Instance().GetDesired()
	if err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot get desired instance status"))
		return rsp, nil
	}

	// Copy resolved status fields to the desired XR.
	if len(instanceDesired) > 0 && instanceDesired[0] != nil {
		src := fieldpath.Pave(instanceDesired[0].Object)
		dst := fieldpath.Pave(dxr.Object)
		for _, v := range g.Instance.Variables {
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
	}

	if err := response.SetDesiredCompositeResource(rsp, &resource.Composite{Resource: dxr}); err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot set desired composite resource"))
		return rsp, nil
	}

	return rsp, nil
}

func (f *Function) requireSchemas(req *fnv1.RunFunctionRequest, rsp *fnv1.RunFunctionResponse, gvks []schema.GroupVersionKind) {
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
				MatchName: strings.ToLower(gvk.Kind + "s." + gvk.Group),
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

	// There are no schemas we care about yet
	if len(schemas) == 0 {
		return nil, nil, nil
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

	if len(crds) == 0 {
		// No CRDs available yet.
		return nil, nil, nil
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

// externalRefSelectorsFromRuntime builds resource selectors for external references
// by using the KRO runtime to evaluate CEL expressions in metadata fields.
// This allows external ref names/namespaces to use expressions like ${schema.spec.configMapName}.
// The namespace defaults to the XR namespace if not specified, following KRO semantics.
//
// If an external ref's identity cannot be resolved yet (e.g., it depends on another
// resource that isn't observed), it's skipped and will be resolved on a subsequent
// invocation. This handles the multi-phase execution model of Crossplane functions.
func (f *Function) externalRefSelectorsFromRuntime(rt *runtime.Runtime, xrNamespace string) (map[string]*fnv1.ResourceSelector, error) {
	selectors := make(map[string]*fnv1.ResourceSelector)

	for _, node := range rt.Nodes() {
		if node.Spec.Meta.Type != graph.NodeTypeExternal {
			// not an external ref, skip it
			continue
		}

		// Use GetDesiredIdentity to evaluate CEL expressions in metadata.name/namespace.
		// This only resolves identity fields and doesn't require other dependencies to be ready.
		desired, err := node.GetDesiredIdentity()
		if err != nil {
			// If data is pending (dependency not yet observed), skip this external ref.
			// It will be resolved on a subsequent invocation when the dependency is available.
			// This is expected during multi-phase function execution.
			if runtime.IsDataPending(err) {
				f.log.Debug("External ref identity not resolvable yet, skipping", "id", node.Spec.Meta.ID)
				continue
			}
			// Other errors (e.g., invalid CEL expression) are fatal.
			return nil, errors.Wrapf(err, "cannot resolve identity for external ref %q", node.Spec.Meta.ID)
		}

		if len(desired) == 0 {
			continue
		}

		u := desired[0]
		namespace := u.GetNamespace()
		if namespace == "" {
			namespace = xrNamespace
		}

		selectors[node.Spec.Meta.ID] = &fnv1.ResourceSelector{
			ApiVersion: u.GetAPIVersion(),
			Kind:       u.GetKind(),
			Match: &fnv1.ResourceSelector_MatchName{
				MatchName: u.GetName(),
			},
			Namespace: &namespace,
		}
	}

	return selectors, nil
}

// StructToSpecSchema converts a protobuf Struct (as returned by Crossplane's
// required_schemas) to a kube-openapi spec.Schema.
func structToSpecSchema(s *structpb.Struct) (*spec.Schema, error) {
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
