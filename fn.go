package main

import (
	"context"
	"strings"

	"github.com/upbound/function-kro/input/v1beta1"
	"github.com/upbound/function-kro/kro/graph"
	"github.com/upbound/function-kro/kro/runtime"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/json"

	"github.com/crossplane/crossplane-runtime/pkg/errors"
	"github.com/crossplane/crossplane-runtime/pkg/logging"
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

	gvks := make([]schema.GroupVersionKind, 0, len(rg.Resources)+1)
	gvks = append(gvks, schema.FromAPIVersionAndKind(oxr.Resource.GetAPIVersion(), oxr.Resource.GetKind()))
	for _, r := range rg.Resources {
		u := &unstructured.Unstructured{}
		if err := json.Unmarshal(r.Template.Raw, u); err != nil {
			response.Fatal(rsp, errors.Wrapf(err, "cannot unmarshal resource id %q", r.ID))
			return rsp, nil
		}
		gvks = append(gvks, schema.FromAPIVersionAndKind(u.GetAPIVersion(), u.GetKind()))
	}

	// Tell Crossplane we need the CRDs for our XR and resource templates.
	// TODO(negz): In v2 we'll need to handle resource templates for built-in
	// types that don't have CRDs - e.g. Deployment.
	rsp.Requirements = RequiredCRDs(gvks...)

	// Process the extra CRDs we required.
	crds := make([]*extv1.CustomResourceDefinition, len(gvks))
	for i := range gvks {
		e, ok := req.GetExtraResources()[gvks[i].String()]
		if !ok {
			// Crossplane hasn't sent us this required CRD. Let it know.
			return rsp, nil
		}

		crd := &extv1.CustomResourceDefinition{}
		if err := resource.AsObject(e.GetItems()[0].GetResource(), crd); err != nil {
			response.Fatal(rsp, errors.Wrapf(err, "cannot unmarshal CRD for %s", gvks[i]))
			return rsp, nil
		}

		crds[i] = crd
	}

	gb, err := graph.NewBuilder(crds...)
	if err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot create resource graph builder"))
		return rsp, nil
	}

	// TODO(negz): Does the CRD need anything special from crd.SynthesizeCRD?
	g, err := gb.NewResourceGraphDefinition(rg, crds[0])
	if err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot create resource graph"))
		return rsp, nil
	}

	// TODO(negz): Does NewGraphRuntime make assumptions about the shape of the
	// resource - e.g. its schema is from crd.SynthesizeCRD?
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

	// TODO(negz): Is it okay to do this before create/update?
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
		if want, err := rt.WantToCreateResource(id); err != nil || !want {
			f.log.Info("Skipping resource", "id", id, "err", err)
			rt.IgnoreResource(id)
			continue
		}

		r, state := rt.GetResource(id)
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

		// TODO(negz): Do we need to do this even when we return/continue above?
		if _, err := rt.Synchronize(); err != nil {
			response.Fatal(rsp, errors.Wrap(err, "cannot synchronize instance"))
			return rsp, nil
		}
	}

	if err := response.SetDesiredComposedResources(rsp, dcds); err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot set desired composed resources"))
		return rsp, nil
	}

	if err := response.SetDesiredCompositeResource(rsp, &resource.Composite{Resource: &composite.Unstructured{Unstructured: *rt.GetInstance()}}); err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot set desired composite resource"))
		return rsp, nil
	}

	return rsp, nil
}

// RequiredCRDs returns the extra CRDs this function requires to run.
func RequiredCRDs(gvks ...schema.GroupVersionKind) *fnv1.Requirements {
	rq := &fnv1.Requirements{ExtraResources: map[string]*fnv1.ResourceSelector{}}

	for _, gvk := range gvks {
		rq.ExtraResources[gvk.String()] = &fnv1.ResourceSelector{
			ApiVersion: "apiextensions.k8s.io/v1",
			Kind:       "CustomResourceDefinition",
			Match: &fnv1.ResourceSelector_MatchName{
				MatchName: strings.ToLower(gvk.Kind + "s." + gvk.Group),
			},
		}
	}

	return rq
}
