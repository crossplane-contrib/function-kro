package executor

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/selection"
	"sigs.k8s.io/controller-runtime/pkg/client"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/rgdadapter"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/runtime"
	"github.com/kubernetes-sigs/kro/pkg/metadata"

	fnv1 "github.com/crossplane/function-sdk-go/proto/v1"
	"github.com/crossplane/function-sdk-go/resource"
	"github.com/crossplane/function-sdk-go/resource/composed"
)

// Crossplane stands in for the API server when Simple runs inside a
// composition function. Simple reads observed composed resources and the
// external resources Crossplane fetched from it, and writes desired composed
// resources, the external resources to fetch, and the XR status to it.
// Crossplane applies, watches and garbage collects composed resources itself.
type Crossplane struct {
	// Observed composed resources, grouped by node ID.
	Observed map[string][]*unstructured.Unstructured

	// Required resources Crossplane fetched for external refs, by node ID. A
	// missing key means Crossplane hasn't answered the requirement yet; a
	// present but empty slice means nothing matched.
	Required map[string][]*unstructured.Unstructured

	// Desired composed resources, keyed by composed resource name.
	Desired map[resource.Name]*resource.DesiredComposed

	// Selectors for the external refs Crossplane should fetch, by node ID.
	ExternalRefSelectors map[string]*fnv1.ResourceSelector

	// Status is the XR status rendered from the ResourceGraph's status block,
	// with fields whose data is still pending left out.
	Status map[string]any

	// What emitTemplate returned for each node, by node ID. markReady uses it
	// to set readiness on desired composed resources after the walk.
	emitted map[string]emission

	// Hard errors from preserve, which Apply returns after the walk.
	errs []error
}

// emission records what emitTemplate returned as desired state for a node.
type emission struct {
	names []resource.Name

	// observed is true when Crossplane has created every resource the node
	// rendered.
	observed bool
}

// NewCrossplane returns a Crossplane holding the observed composed resources
// and fetched external resources for one function run.
func NewCrossplane(observed, required map[string][]*unstructured.Unstructured) *Crossplane {
	return &Crossplane{
		Observed:             observed,
		Required:             required,
		Desired:              map[resource.Name]*resource.DesiredComposed{},
		ExternalRefSelectors: map[string]*fnv1.ResourceSelector{},
		emitted:              map[string]emission{},
	}
}

// Apply walks rt with Simple, with readiness gating on, as upstream's
// instance controller does. It returns Simple's error: one wrapping
// ErrNotReady when some node isn't ready yet, or a hard error when a node
// can't be processed at all.
func (c *Crossplane) Apply(ctx context.Context, rt *runtime.Runtime) error {
	s := &Simple{
		GateReadiness: true,
		Crossplane:    c,
	}
	// Run Simple's Apply, but note that we ignore the ApplyResult here.
	// Upstream uses it to work out what to prune, but Crossplane prunes
	// composed resources itself, and preserve has already kept anything Simple
	// couldn't process in Desired.
	_, err := s.Apply(ctx, rt, nil)
	c.markReady(rt)

	// Errors from preserve are hard. A hard error dominates a soft one, as in
	// Simple.Apply, so we drop err if it only says a node isn't ready.
	if len(c.errs) > 0 {
		if err != nil && !errors.Is(err, ErrNotReady) {
			c.errs = append([]error{err}, c.errs...)
		}
		return errors.Join(c.errs...)
	}
	return err
}

// Get takes the place of the client read in applyRef. It asks Crossplane to
// fetch the external ref, and answers from what Crossplane fetched. It returns
// NotFound until Crossplane has fetched it, which applyRef treats as not ready.
func (c *Crossplane) Get(id string, key client.ObjectKey, obj *unstructured.Unstructured) error {
	gvk := obj.GroupVersionKind()
	sel := &fnv1.ResourceSelector{
		ApiVersion: gvk.GroupVersion().String(),
		Kind:       gvk.Kind,
		Match:      &fnv1.ResourceSelector_MatchName{MatchName: key.Name},
	}
	if key.Namespace != "" {
		sel.Namespace = &key.Namespace
	}
	c.ExternalRefSelectors[id] = sel

	items := c.Required[id]
	if len(items) == 0 {
		return apierrors.NewNotFound(schema.GroupResource{Group: gvk.Group, Resource: gvk.Kind}, key.Name)
	}
	items[0].DeepCopyInto(obj)
	return nil
}

// List takes the place of the client list in applyRefCollection. It asks
// Crossplane to fetch the external collection, and answers from what
// Crossplane fetched.
//
// Crossplane's required resources can only match exact label values, not
// matchExpressions like `tier In (gold, silver)`
// (https://github.com/crossplane/crossplane/issues/6744). So we ask Crossplane
// for everything that has the selector's exact labels, which may be more than
// the selector matches, and filter that down with the full selector here.
func (c *Crossplane) List(id string, list *unstructured.UnstructuredList, opts ...client.ListOption) error {
	lo := (&client.ListOptions{}).ApplyOptions(opts)
	selector := lo.LabelSelector
	if selector == nil {
		selector = labels.Everything()
	}

	// applyRefCollection gives us the selector already compiled, so we read the
	// exact label matches back out of it: each matchLabels entry compiles to an
	// Equals requirement. With no exact matches, we ask Crossplane for every
	// object of the kind.
	match := map[string]string{}
	reqs, _ := selector.Requirements()
	for _, r := range reqs {
		if r.Operator() == selection.Equals || r.Operator() == selection.DoubleEquals {
			match[r.Key()] = r.ValuesUnsorted()[0]
		}
	}

	gvk := list.GroupVersionKind()
	sel := &fnv1.ResourceSelector{
		ApiVersion: gvk.GroupVersion().String(),
		Kind:       gvk.Kind,
		Match:      &fnv1.ResourceSelector_MatchLabels{MatchLabels: &fnv1.MatchLabels{Labels: match}},
	}
	// An empty namespace means all namespaces.
	if lo.Namespace != "" {
		sel.Namespace = &lo.Namespace
	}
	c.ExternalRefSelectors[id] = sel

	items, answered := c.Required[id]
	if !answered {
		return fmt.Errorf("resource %q: waiting for Crossplane to fetch %s resources: %w", id, gvk.Kind, ErrNotReady)
	}
	// Crossplane may have sent more than the selector matches, so apply the full
	// selector, including any matchExpressions.
	for _, item := range items {
		if selector.Matches(labels.Set(item.GetLabels())) {
			list.Items = append(list.Items, *item.DeepCopy())
		}
	}
	return nil
}

// emitTemplate takes the place of applyTemplate. Instead of applying each
// rendered object, it adds it to Crossplane's desired state.
//
// When applyTemplate applies an object, the API server sends back the object
// as it now exists, including fields the server sets, and that replaces the
// object in desired. The caller then publishes desired for dependents to read.
// We can't apply, so we put the observed resource in desired instead, or leave
// what we rendered if Crossplane hasn't created it yet. Dependents can use
// fields we set, like metadata.name, in the same pass, while fields only the
// server sets stay pending until they're observed.
func (s *Simple) emitTemplate(rt *runtime.Runtime, n *runtime.Node, desired []*unstructured.Unstructured) ([]expv1alpha1.ManagedResource, error) {
	c := s.Crossplane
	id := n.ID()
	names := make([]resource.Name, 0, len(desired))
	for i, obj := range desired {
		_, namespaced, err := s.mappingFor(n, obj)
		if err != nil {
			return nil, err
		}

		// We match observed collection items back to their collection by
		// index. Upstream's stampKROMeta also stamps labels and annotations
		// that only its own watches and pruning use.
		if n.IsCollection() {
			l := obj.GetLabels()
			if l == nil {
				l = map[string]string{}
			}
			l[metadata.CollectionIndexLabel] = strconv.Itoa(i)
			obj.SetLabels(l)
		}

		// We leave the namespace unset in what we return, because Crossplane
		// sets it to the XR's namespace when it applies the resource.
		cd, err := composed.From(obj.DeepCopy())
		if err != nil {
			return nil, fmt.Errorf("cannot create composed resource from resource %q: %w", id, err)
		}

		// The runtime's copy gets upstream's namespace defaulting, so observed
		// resources line up with desired ones.
		s.defaultNamespace(rt, namespaced, obj)

		// A resource with no name gets one generated by Crossplane, so it
		// can't collide with another.
		if obj.GetName() != "" {
			if err := s.identityClaims.claim(identityKey(obj), s.qualifiedPath(id)); err != nil {
				return nil, err
			}
		}

		// Collection items are named after their metadata.name so composed
		// resource names don't depend on list order.
		name := resource.Name(id)
		if n.IsCollection() {
			name = resource.Name(id + "-" + obj.GetName())
		}
		c.Desired[name] = &resource.DesiredComposed{Resource: cd}
		names = append(names, name)
	}

	// Swap the observed resources into desired, so dependents read each
	// resource as it exists rather than as we rendered it. What we return to
	// Crossplane was copied above, so it keeps only the fields we rendered.
	// We record what we returned so markReady can set readiness after the walk.
	observed := c.overlayObserved(n, desired)
	c.emitted[id] = emission{names: names, observed: observed}

	// Upstream knows everything it applied exists. Crossplane may not have
	// created ours yet, so a node that declares readiness isn't ready until it
	// has.
	if len(n.Spec().ReadyWhen) > 0 && !observed {
		publishScope(rt, n, desired)
		return nil, fmt.Errorf("resource %q: waiting for Crossplane to create it: %w", id, ErrNotReady)
	}
	return nil, nil
}

// overlayObserved replaces each rendered object in desired with its observed
// counterpart, where one exists. It reports whether every rendered object was
// observed.
func (c *Crossplane) overlayObserved(n *runtime.Node, desired []*unstructured.Unstructured) bool {
	observed := c.Observed[n.ID()]
	if !n.IsCollection() {
		// Crossplane matches a single composed resource by its composed
		// resource name, not its metadata.name, which Crossplane may have
		// generated. So the observed resource is ours whatever its name.
		if len(observed) == 0 || len(desired) == 0 {
			return false
		}
		desired[0] = observed[0]
		return true
	}

	byKey := make(map[string]*unstructured.Unstructured, len(observed))
	for _, o := range observed {
		byKey[identityKey(o)] = o
	}
	all := true
	for i, d := range desired {
		o, ok := byKey[identityKey(d)]
		if !ok {
			all = false
			continue
		}
		desired[i] = o
	}
	return all
}

// capturePatch takes the place of applyPatch. The only patch node an RGD
// produces is the synthesized XR status node, and its rendered status becomes
// the desired XR's status.
func (s *Simple) capturePatch(n *runtime.Node, desired []*unstructured.Unstructured) ([]Contribution, error) {
	if n.ID() == rgdadapter.StatusPatchNodeID && len(desired) == 1 {
		s.Crossplane.Status, _ = desired[0].Object["status"].(map[string]any)
	}
	return nil, nil
}

// preserve keeps a node's existing composed resources in the desired state
// when Apply can't process the node this walk. Upstream holds back pruning for
// such nodes. Crossplane deletes any composed resource the function stops
// returning, so we have to render it again to keep it. If we can't render it,
// we record a hard error, which makes the function return a fatal result so
// Crossplane leaves every resource as it is.
func (s *Simple) preserve(ctx context.Context, rt *runtime.Runtime, n *runtime.Node) {
	c := s.Crossplane
	id := n.ID()
	if n.Kind() == compiler.NodeKindRef {
		// An external ref has nothing to keep, but an existing resource that
		// reads it can't be rendered again without it. So we read it as Apply
		// would, which also keeps asking Crossplane to fetch it. If we can't,
		// any resource that needs it fails to render and reports why.
		if desired, err := n.Resolve(); err == nil {
			_, _ = s.applyNodeByKind(ctx, rt, nil, n, desired, &ApplyResult{})
		}
		return
	}
	if n.Kind() != compiler.NodeKindTemplate || len(c.Observed[id]) == 0 {
		// nothing exists yet, so nothing to keep
		return
	}

	desired, err := n.Resolve()
	if err != nil {
		c.errs = append(c.errs, fmt.Errorf("resource %q already exists but cannot be rendered, refusing to delete it: %w", id, err))
		return
	}
	if _, err := s.emitTemplate(rt, n, desired); err != nil {
		if !errors.Is(err, ErrNotReady) {
			c.errs = append(c.errs, err)
		}
		return
	}
	n.SetObserved(desired, desired)
	publishScope(rt, n, n.Observed())
}

// markReady records readiness on desired composed resources. With no readyWhen
// we leave readiness unspecified, so later functions in the pipeline (like
// function-auto-ready) can decide.
func (c *Crossplane) markReady(rt *runtime.Runtime) {
	for _, n := range rt.Nodes() {
		e, ok := c.emitted[n.ID()]
		if !ok || len(n.Spec().ReadyWhen) == 0 {
			continue
		}
		state := resource.ReadyFalse
		if e.observed && n.CheckReadiness() == nil {
			state = resource.ReadyTrue
		}
		for _, name := range e.names {
			c.Desired[name].Ready = state
		}
	}
}

// identityKey identifies an object in the cluster by its apiVersion, kind,
// namespace and name, in the same format upstream's prepareItem uses for
// identity claims.
func identityKey(u *unstructured.Unstructured) string {
	gvk := u.GroupVersionKind()
	return fmt.Sprintf("%s/%s/%s/%s", gvk.GroupVersion().String(), gvk.Kind, u.GetNamespace(), u.GetName())
}
