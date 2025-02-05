package main

import (
	"context"
	"fmt"
	"strings"

	jsone "encoding/json"

	"github.com/upbound/function-kro/input/v1beta1"
	"github.com/upbound/function-kro/kro/graph"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/json"

	"github.com/crossplane/crossplane-runtime/pkg/errors"
	"github.com/crossplane/crossplane-runtime/pkg/logging"
	fnv1 "github.com/crossplane/function-sdk-go/proto/v1"
	"github.com/crossplane/function-sdk-go/request"
	"github.com/crossplane/function-sdk-go/resource"
	"github.com/crossplane/function-sdk-go/resource/composed"
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
		if len(e.GetItems()) == 0 {
			response.Fatal(rsp, errors.Errorf("cannot find CRD  %s", gvks[i]))
			return rsp, nil
		}
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

	debugPrintCRDs(crds)
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

	dxr, err := request.GetDesiredCompositeResource(req)
	if err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot get desired composite resource"))
		return rsp, nil
	}
	// TODO(negz): Pickup from here: https://github.com/kro-run/kro/blob/87a9b1c460854170e9bceac001ff870933d6a084/pkg/controller/instance/controller_reconcile.go#L63
	dxr.Resource.Object = rt.GetInstance().Object
	if err := dxr.Resource.SetValue("status", rg.Status); err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot write updated status back into composite resource"))
		return rsp, nil
	}
	// Save the updated desired composite resource
	if err := response.SetDesiredCompositeResource(rsp, dxr); err != nil {
		response.Fatal(rsp, errors.Wrapf(err, "cannot set desired composite resource in %T", rsp))
		return rsp, nil
	}

	desired, err := request.GetDesiredComposedResources(req)
	if err != nil {
		response.Fatal(rsp, errors.Wrapf(err, "cannot get desired resources from %T", req))
		return rsp, nil
	}

	for _, resourceFromGraph := range g.Resources {
		cd, err := composed.From(resourceFromGraph.Unstructured())
		if err != nil {
			response.Fatal(rsp, errors.Wrapf(err, "cannot convert %T to %T", resourceFromGraph, &composed.Unstructured{}))
			return rsp, nil
		}
		desired[resource.Name("rg-"+resourceFromGraph.GetID())] = &resource.DesiredComposed{Resource: cd}
	}

	if err := response.SetDesiredComposedResources(rsp, desired); err != nil {
		response.Fatal(rsp, errors.Wrapf(err, "cannot set desired composed resources in %T", rsp))
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

func debugPrintCRDs(crds []*v1.CustomResourceDefinition) {
	for i, crd := range crds {
		data, err := jsone.MarshalIndent(crd, "", "  ")
		if err != nil {
			fmt.Printf("Error marshalling CRD[%d]: %v\n", i, err)
			continue
		}
		fmt.Printf("CRD[%d]: %s\n", i, string(data))
	}
}
