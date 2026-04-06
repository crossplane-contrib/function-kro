package main

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"maps"
	"strings"

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
	"github.com/crossplane/crossplane-runtime/v2/pkg/fieldpath"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"

	fnv1 "github.com/crossplane/function-sdk-go/proto/v1"
	"github.com/crossplane/function-sdk-go/request"
	"github.com/crossplane/function-sdk-go/resource"
	"github.com/crossplane/function-sdk-go/resource/composed"
	"github.com/crossplane/function-sdk-go/resource/composite"
	"github.com/crossplane/function-sdk-go/response"

	"github.com/crossplane-contrib/function-kro/input/v1beta1"
	"github.com/crossplane-contrib/function-kro/kro/graph"
	schemaresolver "github.com/crossplane-contrib/function-kro/kro/graph/schema/resolver"
	"github.com/crossplane-contrib/function-kro/kro/metadata"
	"github.com/crossplane-contrib/function-kro/kro/runtime"
)

// Function returns whatever response you ask it to.
type Function struct {
	fnv1.UnimplementedFunctionRunnerServiceServer

	log       logging.Logger
	rgdConfig graph.RGDConfig
}

// NewFunction returns a Function with the supplied configuration. A nil logger
// defaults to a no-op logger.
func NewFunction(log logging.Logger, rgdConfig graph.RGDConfig) *Function {
	if log == nil {
		log = logging.NewNopLogger()
	}
	return &Function{log: log, rgdConfig: rgdConfig}
}

// RunFunction runs the Function.
func (f *Function) RunFunction(_ context.Context, req *fnv1.RunFunctionRequest) (*fnv1.RunFunctionResponse, error) {
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

	// Build the KRO graph using the schema resolver.
	gb := graph.NewBuilder(resolver)
	g, err := gb.NewResourceGraphDefinition(rg, xrSchema, f.rgdConfig)
	if err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot create resource graph"))
		return rsp, nil
	}

	// Create the KRO runtime from the graph and XR
	rt, err := runtime.FromGraph(g, &oxr.Resource.Unstructured, f.rgdConfig)
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

		// get the required resource(s) that Crossplane provided to us for this external reference
		resources, ok, err := request.GetRequiredResource(req, r.ID)
		if err != nil {
			response.Fatal(rsp, errors.Wrapf(err, "cannot get external resource %q", r.ID))
			return rsp, nil
		}
		if !ok || len(resources) == 0 {
			f.log.Debug("External resource not available yet", "id", r.ID)
			continue
		}

		// set the resource(s) observed state on the runtime node, so KRO has access to it for later evaluations etc.
		if node, ok := nodesByID[r.ID]; ok {
			observed := make([]*unstructured.Unstructured, len(resources))
			for i, res := range resources {
				observed[i] = res.Resource
			}
			f.log.Debug("SetObserved external ref", "id", r.ID, "externalRef", r.ExternalRef, "count", len(observed))
			node.SetObserved(observed)
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
	observedByNodeID := groupObservedByNodeID(f.log, ocds, nodesByID)

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
	if err := buildDesiredComposed(f.log, rt, dcds); err != nil {
		response.Fatal(rsp, err)
		return rsp, nil
	}

	if err := response.SetDesiredComposedResources(rsp, dcds); err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot set desired composed resources"))
		return rsp, nil
	}

	// Build a minimal desired XR containing only the status paths declared in
	// the ResourceGraph. This is critical for SSA - we must only include fields
	// we want to own.
	dxr, err := buildDesiredXRStatus(rt, g, oxr)
	if err != nil {
		response.Fatal(rsp, err)
		return rsp, nil
	}

	if err := response.SetDesiredCompositeResource(rsp, &resource.Composite{Resource: dxr}); err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot set desired composite resource"))
		return rsp, nil
	}

	return rsp, nil
}

// collectGVKs returns the GVKs for the XR and every resource in the graph.
func collectGVKs(xrGVK schema.GroupVersionKind, resources []*v1beta1.Resource) ([]schema.GroupVersionKind, error) {
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
		if node.Spec.Meta.Type != graph.NodeTypeExternal && node.Spec.Meta.Type != graph.NodeTypeExternalCollection {
			// not an external ref, skip it
			continue
		}

		if node.Spec.Meta.Type == graph.NodeTypeExternalCollection {
			// attempt to build selectors for this external collection
			selector, err := externalCollectionSelector(node)
			if err != nil {
				if runtime.IsDataPending(err) {
					f.log.Debug("External collection not resolvable yet, skipping", "id", node.Spec.Meta.ID)
					continue
				}
				return nil, errors.Wrapf(err, "cannot build selector for external collection %q", node.Spec.Meta.ID)
			}
			if selector != nil {
				// we got a fully formed selector for this external collection, add it the list
				selectors[node.Spec.Meta.ID] = selector
			}
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

		template := desired[0]
		namespace := template.GetNamespace()
		if namespace == "" {
			namespace = xrNamespace
		}

		selectors[node.Spec.Meta.ID] = &fnv1.ResourceSelector{
			ApiVersion: template.GetAPIVersion(),
			Kind:       template.GetKind(),
			Match: &fnv1.ResourceSelector_MatchName{
				MatchName: template.GetName(),
			},
			Namespace: &namespace,
		}
	}

	return selectors, nil
}

// externalCollectionSelector resolves an external collection node's template and
// extracts the label selector to build a Crossplane ResourceSelector with MatchLabels.
func externalCollectionSelector(node *runtime.Node) (*fnv1.ResourceSelector, error) {
	// compute/resolve all the expressions in this external collection's template
	desired, err := node.GetDesired()
	if err != nil {
		return nil, err
	}
	if len(desired) == 0 {
		return nil, nil
	}
	template := desired[0]

	// extract the selector from the unstructured data
	selectorRaw, found, err := unstructured.NestedMap(template.Object, "metadata", "selector")
	if err != nil {
		return nil, errors.Wrap(err, "cannot extract selector from resolved template")
	}
	if !found {
		return nil, errors.New("resolved template has no selector")
	}

	ls := &metav1.LabelSelector{}
	if err := k8sruntime.DefaultUnstructuredConverter.FromUnstructured(selectorRaw, ls); err != nil {
		return nil, errors.Wrapf(err, "cannot convert selector")
	}

	labels := make(map[string]string)
	if ls.MatchLabels != nil {
		maps.Copy(labels, ls.MatchLabels)
	}

	selector := &fnv1.ResourceSelector{
		ApiVersion: template.GetAPIVersion(),
		Kind:       template.GetKind(),
		Match: &fnv1.ResourceSelector_MatchLabels{
			MatchLabels: &fnv1.MatchLabels{
				Labels: labels,
			},
		},
	}

	// Namespace: if explicitly set, use it. If empty, omit entirely
	// to match all namespaces (per Crossplane's ResourceSelector semantics).
	if ns := template.GetNamespace(); ns != "" {
		selector.Namespace = &ns
	}

	return selector, nil
}

// groupObservedByNodeID groups observed composed resources by their runtime
// node ID. For single resources, the composed resource name equals the node ID.
// For collections, the composed resource name uses the pattern
// "collectionNodeID-metadataName" (e.g., "subnets-my-app-us-east-1") and has
// the kro.run/collection-index label set.
func groupObservedByNodeID(log logging.Logger, ocds map[resource.Name]resource.ObservedComposed, nodesByID map[string]*runtime.Node) map[string][]*unstructured.Unstructured {
	observedByNodeID := make(map[string][]*unstructured.Unstructured)
	for name, r := range ocds {
		id := string(name) // ID is the same as the composed resource name
		if _, ok := nodesByID[id]; ok {
			// found a direct match node for this ID
			observedByNodeID[id] = append(observedByNodeID[id], &r.Resource.Unstructured)
			continue
		}
		if _, isCollectionItem := r.Resource.GetLabels()[metadata.CollectionIndexLabel]; isCollectionItem {
			// this is a collection item, try to find its parent collection node
			if parentNodeID := findCollectionNodeID(id, nodesByID); parentNodeID != "" {
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
func findCollectionNodeID(id string, nodesByID map[string]*runtime.Node) string {
	// try all segments of the ID from longest to shortest
	for remaining := id; ; {
		// find the next longest segment in the resource name
		idx := strings.LastIndex(remaining, "-")
		if idx <= 0 {
			// no more segments to check, return empty
			return ""
		}
		prefix := id[:idx]
		if node, ok := nodesByID[prefix]; ok && (node.Spec.Meta.Type == graph.NodeTypeCollection || node.Spec.Meta.Type == graph.NodeTypeExternalCollection) {
			// we found a collection node parent that matches the name prefix of this collection item
			return prefix
		}
		remaining = prefix
	}
}

// buildDesiredComposed processes all runtime nodes in topological order,
// building the desired composed resources map. Collection resources are named
// "{id}-{metadata.name}" for stable identity across reconciles.
func buildDesiredComposed(log logging.Logger, rt *runtime.Runtime, dcds map[resource.Name]*resource.DesiredComposed) error { //nolint:gocognit // Readability is better with the logic inline.
	for _, node := range rt.Nodes() {
		id := node.Spec.Meta.ID

		// External refs are read-only and not managed by this function or Crossplane.
		// Skip them from desired output.
		if node.Spec.Meta.Type == graph.NodeTypeExternal || node.Spec.Meta.Type == graph.NodeTypeExternalCollection {
			log.Debug("Not including external ref in desired resources", "id", id)
			continue
		}

		// Check if this node should be ignored (includeWhen evaluated to false).
		ignored, err := node.IsIgnored()
		if err != nil {
			log.Info("Error checking if resource is ignored", "id", id, "err", err)
			continue
		}
		if ignored {
			log.Debug("Not including ignored resource in desired resources", "id", id)
			continue
		}

		// Get the desired state with CEL expressions resolved.
		// This is critical for SSA - desired state must only contain fields
		// we want to own, not provider-defaulted fields from observed state.
		desired, err := node.GetDesired()
		if err != nil {
			if runtime.IsDataPending(err) {
				log.Debug("Not including resource with pending data in desired resources", "id", id)
				continue
			}
			return errors.Wrapf(err, "cannot get desired state for resource %q", id)
		}

		// For single resources, desired has one element.
		// For collections, desired has multiple elements (one per forEach expansion).
		isCollection := node.Spec.Meta.Type == graph.NodeTypeCollection
		for _, r := range desired {
			resourceName := id
			if isCollection {
				// This resource is part of a collection: append the resource's metadata.name
				// to produce a stable composed resource name that doesn't depend on list order.
				resourceName = id + "-" + r.GetName()
			}

			cd, err := composed.From(r)
			if err != nil {
				return errors.Wrapf(err, "cannot create composed resource from template id %s", id)
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
				if err := node.CheckReadiness(); err != nil {
					if !stderrors.Is(err, runtime.ErrWaitingForReadiness) {
						log.Info("Error checking resource readiness", "id", id, "err", err)
					}
				} else {
					readyState = resource.ReadyTrue
				}
			}
			log.Debug("Resource ready state", "id", id, "ready", readyState)
			dcds[resource.Name(resourceName)] = &resource.DesiredComposed{Resource: cd, Ready: readyState}
		}
	}

	return nil
}

// buildDesiredXRStatus builds a minimal desired XR containing only the resolved
// status paths declared in the ResourceGraph. This is critical for SSA - we
// must only include fields we want to own.
func buildDesiredXRStatus(rt *runtime.Runtime, g *graph.Graph, oxr *resource.Composite) (*composite.Unstructured, error) {
	dxr := &composite.Unstructured{Unstructured: unstructured.Unstructured{Object: map[string]any{}}}
	dxr.SetAPIVersion(oxr.Resource.GetAPIVersion())
	dxr.SetKind(oxr.Resource.GetKind())

	instanceDesired, err := rt.Instance().GetDesired()
	if err != nil {
		return nil, errors.Wrap(err, "cannot get desired instance status")
	}

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
				return nil, errors.Wrapf(err, "cannot set desired XR status field %q", v.Path)
			}
		}
	}

	return dxr, nil
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
