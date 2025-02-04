package main

import (
	"context"

	"github.com/upbound/function-kro/input/v1beta1"
	"github.com/upbound/function-kro/kro/graph"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"github.com/crossplane/crossplane-runtime/pkg/errors"
	"github.com/crossplane/crossplane-runtime/pkg/logging"
	fnv1 "github.com/crossplane/function-sdk-go/proto/v1"
	"github.com/crossplane/function-sdk-go/request"
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

	// TODO(negz): This won't work yet. It needs a schema resolver that can get
	// CEL schemas for composed resources. See schema/resolver.go.
	gb, err := graph.NewBuilder()
	if err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot create resource graph builder"))
		return rsp, nil
	}

	// TODO(negz): Use extra resources to get the XR CRD and pass it in.
	// TODO(negz): Does the CRD need anything special from crd.SynthesizeCRD?
	xrCRD := &extv1.CustomResourceDefinition{}
	g, err := gb.NewResourceGraphDefinition(rg, xrCRD)
	if err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot create resource graph"))
		return rsp, nil
	}

	oxr, err := request.GetDesiredCompositeResource(req)
	if err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot get observed composite resource"))
		return rsp, nil
	}

	// TODO(negz): Does NewGraphRuntime make assumptions about the shape of the
	// resource - e.g. its schema is from crd.SynthesizeCRD?
	rt, err := g.NewGraphRuntime(&oxr.Resource.Unstructured)
	if err != nil {
		response.Fatal(rsp, errors.Wrap(err, "cannot get graph runtime"))
		return rsp, nil
	}

	// TODO(negz): Pickup from here: https://github.com/kro-run/kro/blob/87a9b1c460854170e9bceac001ff870933d6a084/pkg/controller/instance/controller_reconcile.go#L63
	_ = rt.GetInstance()

	return rsp, nil
}
