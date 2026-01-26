package main

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"google.golang.org/protobuf/testing/protocmp"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
	fnv1 "github.com/crossplane/function-sdk-go/proto/v1"
	"github.com/crossplane/function-sdk-go/resource"
	"github.com/crossplane/function-sdk-go/response"
)

func TestRunFunction(t *testing.T) {
	type args struct {
		ctx context.Context
		req *fnv1.RunFunctionRequest
	}
	type want struct {
		rsp *fnv1.RunFunctionResponse
		err error
	}

	cases := map[string]struct {
		reason string
		args   args
		want   want
	}{
		"MissingSchemas": {
			reason: "The function should return requirements when schemas are not yet available",
			args: args{
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test"},
					Input: resource.MustStructJSON(`{
						"apiVersion": "kro.fn.crossplane.io/v1beta1",
						"kind": "ResourceGraph",
						"resources": [{
							"id": "bucket",
							"template": {
								"apiVersion": "s3.aws.upbound.io/v1beta1",
								"kind": "Bucket",
								"spec": {
									"forProvider": {
										"region": "us-west-2"
									}
								}
							}
						}],
						"status": {
							"bucketName": "${bucket.status.atProvider.id}"
						}
					}`),
					Observed: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.crossplane.io/v1",
								"kind": "XBucket",
								"metadata": {"name": "test-bucket"},
								"spec": {}
							}`),
						},
					},
				},
			},
			want: want{
				rsp: &fnv1.RunFunctionResponse{
					Meta: &fnv1.ResponseMeta{Tag: "test", Ttl: durationpb.New(response.DefaultTTL)},
					Requirements: &fnv1.Requirements{
						Schemas: map[string]*fnv1.SchemaSelector{
							"example.crossplane.io/v1, Kind=XBucket": {
								ApiVersion: "example.crossplane.io/v1",
								Kind:       "XBucket",
							},
							"s3.aws.upbound.io/v1beta1, Kind=Bucket": {
								ApiVersion: "s3.aws.upbound.io/v1beta1",
								Kind:       "Bucket",
							},
						},
					},
				},
			},
		},
		"DesiredXROnlyContainsDeclaredStatus": {
			reason: "The desired XR should only contain status fields declared in the ResourceGraph, not the full observed XR",
			args: args{
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test"},
					Input: resource.MustStructJSON(`{
						"apiVersion": "kro.fn.crossplane.io/v1beta1",
						"kind": "ResourceGraph",
						"resources": [{
							"id": "bucket",
							"template": {
								"apiVersion": "s3.aws.upbound.io/v1beta1",
								"kind": "Bucket",
								"metadata": {},
								"spec": {
									"forProvider": {
										"region": "us-west-2"
									}
								}
							}
						}],
						"status": {
							"bucketName": "${bucket.status.atProvider.id}"
						}
					}`),
					Observed: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.crossplane.io/v1",
								"kind": "XBucket",
								"metadata": {
									"name": "test-bucket",
									"uid": "abc-123",
									"resourceVersion": "12345",
									"generation": 2
								},
								"spec": {"bucketName": "my-bucket"},
								"status": {
									"conditions": [{"type": "Ready", "status": "True"}]
								}
							}`),
						},
					},
					RequiredSchemas: map[string]*fnv1.Schema{
						"example.crossplane.io/v1, Kind=XBucket": {
							OpenapiV3: resource.MustStructJSON(`{
								"type": "object",
								"properties": {
									"apiVersion": {"type": "string"},
									"kind": {"type": "string"},
									"metadata": {"type": "object"},
									"spec": {
										"type": "object",
										"properties": {
											"bucketName": {"type": "string"}
										}
									},
									"status": {
										"type": "object",
										"properties": {
											"bucketName": {"type": "string"}
										}
									}
								}
							}`),
						},
						"s3.aws.upbound.io/v1beta1, Kind=Bucket": {
							OpenapiV3: resource.MustStructJSON(`{
								"type": "object",
								"properties": {
									"apiVersion": {"type": "string"},
									"kind": {"type": "string"},
									"metadata": {"type": "object"},
									"spec": {
										"type": "object",
										"properties": {
											"forProvider": {
												"type": "object",
												"properties": {
													"region": {"type": "string"}
												}
											}
										}
									},
									"status": {
										"type": "object",
										"properties": {
											"atProvider": {
												"type": "object",
												"properties": {
													"id": {"type": "string"}
												}
											}
										}
									}
								}
							}`),
						},
					},
				},
			},
			want: want{
				rsp: &fnv1.RunFunctionResponse{
					Meta: &fnv1.ResponseMeta{Tag: "test", Ttl: durationpb.New(response.DefaultTTL)},
					Requirements: &fnv1.Requirements{
						Schemas: map[string]*fnv1.SchemaSelector{
							"example.crossplane.io/v1, Kind=XBucket": {
								ApiVersion: "example.crossplane.io/v1",
								Kind:       "XBucket",
							},
							"s3.aws.upbound.io/v1beta1, Kind=Bucket": {
								ApiVersion: "s3.aws.upbound.io/v1beta1",
								Kind:       "Bucket",
							},
						},
					},
					Desired: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.crossplane.io/v1",
								"kind": "XBucket"
							}`),
						},
						Resources: map[string]*fnv1.Resource{
							"bucket": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket",
									"metadata": {},
									"spec": {
										"forProvider": {
											"region": "us-west-2"
										}
									}
								}`),
								Ready: fnv1.Ready_READY_FALSE,
							},
						},
					},
				},
			},
		},
		"DesiredComposedResourceExcludesObservedFields": {
			reason: "Desired composed resources should only contain template fields, not fields from observed state like provider defaults",
			args: args{
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test"},
					Input: resource.MustStructJSON(`{
						"apiVersion": "kro.fn.crossplane.io/v1beta1",
						"kind": "ResourceGraph",
						"resources": [{
							"id": "bucket",
							"template": {
								"apiVersion": "s3.aws.upbound.io/v1beta1",
								"kind": "Bucket",
								"metadata": {},
								"spec": {
									"forProvider": {
										"region": "us-west-2"
									}
								}
							}
						}],
						"status": {
							"bucketARN": "${bucket.status.atProvider.arn}"
						}
					}`),
					Observed: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.crossplane.io/v1",
								"kind": "XBucket",
								"metadata": {"name": "test-bucket"},
								"spec": {}
							}`),
						},
						Resources: map[string]*fnv1.Resource{
							"bucket": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket",
									"metadata": {"name": "test-bucket-abc123"},
									"spec": {
										"forProvider": {
											"region": "us-west-2",
											"objectLockEnabled": false
										},
										"managementPolicies": ["*"]
									},
									"status": {
										"atProvider": {
											"arn": "arn:aws:s3:::test-bucket-abc123",
											"id": "test-bucket-abc123"
										}
									}
								}`),
							},
						},
					},
					RequiredSchemas: map[string]*fnv1.Schema{
						"example.crossplane.io/v1, Kind=XBucket": {
							OpenapiV3: resource.MustStructJSON(`{
								"type": "object",
								"properties": {
									"apiVersion": {"type": "string"},
									"kind": {"type": "string"},
									"metadata": {"type": "object"},
									"spec": {"type": "object"},
									"status": {
										"type": "object",
										"properties": {
											"bucketARN": {"type": "string"}
										}
									}
								}
							}`),
						},
						"s3.aws.upbound.io/v1beta1, Kind=Bucket": {
							OpenapiV3: resource.MustStructJSON(`{
								"type": "object",
								"properties": {
									"apiVersion": {"type": "string"},
									"kind": {"type": "string"},
									"metadata": {"type": "object"},
									"spec": {
										"type": "object",
										"properties": {
											"forProvider": {
												"type": "object",
												"properties": {
													"region": {"type": "string"},
													"objectLockEnabled": {"type": "boolean"}
												}
											},
											"managementPolicies": {
												"type": "array",
												"items": {"type": "string"}
											}
										}
									},
									"status": {
										"type": "object",
										"properties": {
											"atProvider": {
												"type": "object",
												"properties": {
													"arn": {"type": "string"},
													"id": {"type": "string"}
												}
											}
										}
									}
								}
							}`),
						},
					},
				},
			},
			want: want{
				rsp: &fnv1.RunFunctionResponse{
					Meta: &fnv1.ResponseMeta{Tag: "test", Ttl: durationpb.New(response.DefaultTTL)},
					Requirements: &fnv1.Requirements{
						Schemas: map[string]*fnv1.SchemaSelector{
							"example.crossplane.io/v1, Kind=XBucket": {
								ApiVersion: "example.crossplane.io/v1",
								Kind:       "XBucket",
							},
							"s3.aws.upbound.io/v1beta1, Kind=Bucket": {
								ApiVersion: "s3.aws.upbound.io/v1beta1",
								Kind:       "Bucket",
							},
						},
					},
					Desired: &fnv1.State{
						Composite: &fnv1.Resource{
							// Only declared status field, CEL resolved from observed bucket
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.crossplane.io/v1",
								"kind": "XBucket",
								"status": {
									"bucketARN": "arn:aws:s3:::test-bucket-abc123"
								}
							}`),
						},
						Resources: map[string]*fnv1.Resource{
							"bucket": {
								// Only template fields - excludes observed objectLockEnabled and managementPolicies
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket",
									"metadata": {},
									"spec": {
										"forProvider": {
											"region": "us-west-2"
										}
									}
								}`),
								Ready: fnv1.Ready_READY_TRUE, // Ready because observed resource exists with status
							},
						},
					},
				},
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := &Function{log: logging.NewNopLogger()}
			rsp, err := f.RunFunction(tc.args.ctx, tc.args.req)

			if diff := cmp.Diff(tc.want.rsp, rsp, protocmp.Transform()); diff != "" {
				t.Errorf("%s\nf.RunFunction(...): -want rsp, +got rsp:\n%s", tc.reason, diff)
			}

			if diff := cmp.Diff(tc.want.err, err, cmpopts.EquateErrors()); diff != "" {
				t.Errorf("%s\nf.RunFunction(...): -want err, +got err:\n%s", tc.reason, diff)
			}
		})
	}
}
