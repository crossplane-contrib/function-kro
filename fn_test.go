package main

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/kubernetes-sigs/kro/pkg/features"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	"google.golang.org/protobuf/testing/protocmp"
	"google.golang.org/protobuf/types/known/durationpb"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"

	fnv1 "github.com/crossplane/function-sdk-go/proto/v1"
	"github.com/crossplane/function-sdk-go/resource"
	"github.com/crossplane/function-sdk-go/response"
)

// buildSchema creates an OpenAPI v3 schema with standard apiVersion, kind, and
// metadata fields. additionalProps is a raw JSON fragment for extra top-level
// properties (spec, status, data, etc.) that gets spliced in alongside the
// standard fields.
func buildSchema(additionalProps string) *fnv1.Schema {
	extra := ""
	if additionalProps != "" {
		extra = ",\n" + additionalProps
	}
	return &fnv1.Schema{
		OpenapiV3: resource.MustStructJSON(`{
			"type": "object",
			"properties": {
				"apiVersion": {"type": "string"},
				"kind": {"type": "string"},
				"metadata": {
					"type": "object",
					"properties": {
						"name": {"type": "string"},
						"namespace": {"type": "string"},
						"labels": {"type": "object", "additionalProperties": {"type": "string"}},
						"annotations": {"type": "object", "additionalProperties": {"type": "string"}}
					}
				}` + extra + `
			}
		}`),
	}
}

// Reusable schemas for test cases. Each distinct OpenAPI schema shape is
// defined once here and referenced by name in the test table.
var (
	schemaXBucket = buildSchema(`
		"spec": {
			"type": "object",
			"properties": {
				"bucketName": {"type": "string"},
				"configMapName": {"type": "string"},
				"enableLogging": {"type": "boolean"},
				"regions": {"type": "array", "items": {"type": "string"}},
				"replicas": {"type": "integer"}
			}
		},
		"status": {
			"type": "object",
			"properties": {
				"bucketName": {"type": "string"},
				"bucketARN": {"type": "string"},
				"region": {"type": "string"},
				"regionCount": {"type": "integer"},
				"port": {"x-kubernetes-int-or-string": true}
			}
		}`)

	schemaBucket = buildSchema(`
		"spec": {
			"type": "object",
			"properties": {
				"forProvider": {
					"type": "object",
					"properties": {
						"region": {"type": "string"},
						"objectLockEnabled": {"type": "boolean"},
						"replicas": {"type": "integer"}
					}
				},
				"managementPolicies": {"type": "array", "items": {"type": "string"}}
			}
		},
		"status": {
			"type": "object",
			"properties": {
				"atProvider": {
					"type": "object",
					"properties": {
						"arn": {"type": "string"},
						"id": {"type": "string"},
						"objectCount": {"type": "integer"},
						"port": {"x-kubernetes-int-or-string": true}
					}
				}
			}
		}`)

	schemaBucketLogging = buildSchema(`
		"spec": {
			"type": "object",
			"properties": {
				"forProvider": {
					"type": "object",
					"properties": {
						"bucket": {"type": "string"},
						"targetPrefix": {"type": "string"}
					}
				}
			}
		}`)

	schemaConfigMap = buildSchema(`
		"data": {
			"type": "object",
			"additionalProperties": {"type": "string"}
		}`)
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
						"apiVersion": "kro.fn.crossplane.io/v1alpha1",
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
						Resources: map[string]*fnv1.ResourceSelector{
							"example.crossplane.io/v1, Kind=XBucket": {
								ApiVersion: "apiextensions.k8s.io/v1",
								Kind:       "CustomResourceDefinition",
								Match:      &fnv1.ResourceSelector_MatchName{MatchName: "xbuckets.example.crossplane.io"},
							},
							"s3.aws.upbound.io/v1beta1, Kind=Bucket": {
								ApiVersion: "apiextensions.k8s.io/v1",
								Kind:       "CustomResourceDefinition",
								Match:      &fnv1.ResourceSelector_MatchName{MatchName: "buckets.s3.aws.upbound.io"},
							},
						},
					},
				},
			},
		},
		"MissingSchemasCRDFallback": {
			reason: "When required schemas are not available, the function should fall back to resolving schemas from CRDs via required_resources",
			args: args{
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test", Capabilities: []fnv1.Capability{}}, // an older crossplane version won't advertise capabilities at all
					Input: resource.MustStructJSON(`{
						"apiVersion": "kro.fn.crossplane.io/v1alpha1",
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
								"metadata": {"name": "test-bucket"},
								"spec": {"bucketName": "my-bucket"}
							}`),
						},
						Resources: map[string]*fnv1.Resource{
							"bucket": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket",
									"metadata": {"name": "test-bucket-xyz"},
									"spec": {"forProvider": {"region": "us-west-2"}},
									"status": {"atProvider": {"id": "test-bucket-xyz"}}
								}`),
							},
						},
					},
					// we include the required resources CRDs in the request also, simulating the 2nd call, so we are testing both:
					// 1. the function always requests CRDs as required resources and that is stable
					// 2. the function can use the CRDs to validate schemas, execute KRO runtime, and return desired composed resources
					RequiredResources: map[string]*fnv1.Resources{
						"example.crossplane.io/v1, Kind=XBucket": {
							Items: []*fnv1.Resource{{
								Resource: resource.MustStructJSON(`{
									"apiVersion": "apiextensions.k8s.io/v1",
									"kind": "CustomResourceDefinition",
									"metadata": {"name": "xbuckets.example.crossplane.io"},
									"spec": {
										"group": "example.crossplane.io",
										"names": {"kind": "XBucket", "plural": "xbuckets"},
										"versions": [{
											"name": "v1",
											"served": true,
											"storage": true,
											"schema": {
												"openAPIV3Schema": {
													"type": "object",
													"properties": {
														"apiVersion": {"type": "string"},
														"kind": {"type": "string"},
														"metadata": {"type": "object"},
														"spec": {"type": "object", "properties": {
															"bucketName": {"type": "string"},
															"configMapName": {"type": "string"},
															"enableLogging": {"type": "boolean"},
															"regions": {"type": "array", "items": {"type": "string"}}
														}},
														"status": {"type": "object", "properties": {
															"bucketName": {"type": "string"},
															"bucketARN": {"type": "string"},
															"region": {"type": "string"}
														}}
													}
												}
											}
										}]
									}
								}`),
							}},
						},
						"s3.aws.upbound.io/v1beta1, Kind=Bucket": {
							Items: []*fnv1.Resource{{
								Resource: resource.MustStructJSON(`{
									"apiVersion": "apiextensions.k8s.io/v1",
									"kind": "CustomResourceDefinition",
									"metadata": {"name": "buckets.s3.aws.upbound.io"},
									"spec": {
										"group": "s3.aws.upbound.io",
										"names": {"kind": "Bucket", "plural": "buckets"},
										"versions": [{
											"name": "v1beta1",
											"served": true,
											"storage": true,
											"schema": {
												"openAPIV3Schema": {
													"type": "object",
													"properties": {
														"apiVersion": {"type": "string"},
														"kind": {"type": "string"},
														"metadata": {"type": "object"},
														"spec": {"type": "object", "properties": {
															"forProvider": {"type": "object", "properties": {
																"region": {"type": "string"},
																"objectLockEnabled": {"type": "boolean"}
															}},
															"managementPolicies": {"type": "array", "items": {"type": "string"}}
														}},
														"status": {"type": "object", "properties": {
															"atProvider": {"type": "object", "properties": {
																"arn": {"type": "string"},
																"id": {"type": "string"}
															}}
														}}
													}
												}
											}
										}]
									}
								}`),
							}},
						},
					},
				},
			},
			want: want{
				rsp: &fnv1.RunFunctionResponse{
					Meta: &fnv1.ResponseMeta{Tag: "test", Ttl: durationpb.New(response.DefaultTTL)},
					Requirements: &fnv1.Requirements{
						Resources: map[string]*fnv1.ResourceSelector{
							"example.crossplane.io/v1, Kind=XBucket": {
								ApiVersion: "apiextensions.k8s.io/v1",
								Kind:       "CustomResourceDefinition",
								Match:      &fnv1.ResourceSelector_MatchName{MatchName: "xbuckets.example.crossplane.io"},
							},
							"s3.aws.upbound.io/v1beta1, Kind=Bucket": {
								ApiVersion: "apiextensions.k8s.io/v1",
								Kind:       "CustomResourceDefinition",
								Match:      &fnv1.ResourceSelector_MatchName{MatchName: "buckets.s3.aws.upbound.io"},
							},
						},
					},
					Desired: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.crossplane.io/v1",
								"kind": "XBucket",
								"status": {
									"bucketName": "test-bucket-xyz"
								}
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
							},
						},
					},
				},
			},
		},
		"EmptySchemasFromRequiredSchemas": {
			reason: "When Crossplane responds via required_schemas but all schemas are nil, the function should return a fatal error rather than silently returning empty desired state",
			args: args{
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test", Capabilities: []fnv1.Capability{fnv1.Capability_CAPABILITY_CAPABILITIES, fnv1.Capability_CAPABILITY_REQUIRED_SCHEMAS}},
					Input: resource.MustStructJSON(`{
						"apiVersion": "kro.fn.crossplane.io/v1alpha1",
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
								"metadata": {"name": "test-bucket"},
								"spec": {}
							}`),
						},
					},
					// Crossplane responded with schema entries but all are nil,
					// simulating a problem causing all schemas to not be found.
					RequiredSchemas: map[string]*fnv1.Schema{
						"example.crossplane.io/v1, Kind=XBucket": nil,
						"s3.aws.upbound.io/v1beta1, Kind=Bucket": nil,
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
					Results: []*fnv1.Result{{
						// we should be returning a fatal result, we don't have usable schemas and we can't compose desired resources
						Severity: fnv1.Severity_SEVERITY_FATAL,
						Message:  "cannot process schemas: no usable schemas found in response",
						Target:   fnv1.Target_TARGET_COMPOSITE.Enum(),
					}},
				},
			},
		},
		"EmptyCRDsFromRequiredResources": {
			reason: "When Crossplane responds via required_resources but all CRD entries are empty, the function should return a fatal error rather than silently returning empty desired state",
			args: args{
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test", Capabilities: []fnv1.Capability{}},
					Input: resource.MustStructJSON(`{
						"apiVersion": "kro.fn.crossplane.io/v1alpha1",
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
								"metadata": {"name": "test-bucket"},
								"spec": {}
							}`),
						},
					},
					// Crossplane responded for all GVKs but with empty items,
					// simulating CRDs that couldn't be found.
					RequiredResources: map[string]*fnv1.Resources{
						"example.crossplane.io/v1, Kind=XBucket": {Items: []*fnv1.Resource{}},
						"s3.aws.upbound.io/v1beta1, Kind=Bucket": {Items: []*fnv1.Resource{}},
					},
				},
			},
			want: want{
				rsp: &fnv1.RunFunctionResponse{
					Meta: &fnv1.ResponseMeta{Tag: "test", Ttl: durationpb.New(response.DefaultTTL)},
					Requirements: &fnv1.Requirements{
						Resources: map[string]*fnv1.ResourceSelector{
							"example.crossplane.io/v1, Kind=XBucket": {
								ApiVersion: "apiextensions.k8s.io/v1",
								Kind:       "CustomResourceDefinition",
								Match:      &fnv1.ResourceSelector_MatchName{MatchName: "xbuckets.example.crossplane.io"},
							},
							"s3.aws.upbound.io/v1beta1, Kind=Bucket": {
								ApiVersion: "apiextensions.k8s.io/v1",
								Kind:       "CustomResourceDefinition",
								Match:      &fnv1.ResourceSelector_MatchName{MatchName: "buckets.s3.aws.upbound.io"},
							},
						},
					},
					Results: []*fnv1.Result{{
						// we should return a fatal result because we have no CRD schemas to use
						// and cannot compose desired resources without them
						Severity: fnv1.Severity_SEVERITY_FATAL,
						Message:  "cannot process schemas: no usable CRDs found in response",
						Target:   fnv1.Target_TARGET_COMPOSITE.Enum(),
					}},
				},
			},
		},
		"DesiredXROnlyContainsDeclaredStatus": {
			reason: "The desired XR should contain resolved status expressions but not observed XR metadata (uid, resourceVersion) or existing status (conditions)",
			args: args{
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test", Capabilities: []fnv1.Capability{fnv1.Capability_CAPABILITY_CAPABILITIES, fnv1.Capability_CAPABILITY_REQUIRED_SCHEMAS}},
					Input: resource.MustStructJSON(`{
						"apiVersion": "kro.fn.crossplane.io/v1alpha1",
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
						Resources: map[string]*fnv1.Resource{
							"bucket": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket",
									"metadata": {"name": "test-bucket-xyz"},
									"spec": {
										"forProvider": {
											"region": "us-west-2"
										}
									},
									"status": {
										"atProvider": {
											"id": "test-bucket-xyz"
										}
									}
								}`),
							},
						},
					},
					RequiredSchemas: map[string]*fnv1.Schema{
						"example.crossplane.io/v1, Kind=XBucket": schemaXBucket,
						"s3.aws.upbound.io/v1beta1, Kind=Bucket": schemaBucket,
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
							// Status contains only the declared field with the resolved CEL expression.
							// Observed XR metadata (uid, resourceVersion, generation) and
							// existing status (conditions) must NOT appear here.
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.crossplane.io/v1",
								"kind": "XBucket",
								"status": {
									"bucketName": "test-bucket-xyz"
								}
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
							},
						},
					},
				},
			},
		},
		"SchemaFieldsResolveIntoStatus": {
			reason: "Status expressions that read the XR's own fields (schema.*) should resolve into the desired XR status",
			args: args{
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test", Capabilities: []fnv1.Capability{fnv1.Capability_CAPABILITY_CAPABILITIES, fnv1.Capability_CAPABILITY_REQUIRED_SCHEMAS}},
					Input: resource.MustStructJSON(`{
						"apiVersion": "kro.fn.crossplane.io/v1alpha1",
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
							"bucketName": "${schema.spec.bucketName}",
							"regionCount": "${size(schema.spec.regions)}"
						}
					}`),
					Observed: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.crossplane.io/v1",
								"kind": "XBucket",
								"metadata": {"name": "test-bucket"},
								"spec": {
									"bucketName": "my-bucket",
									"regions": ["us-east-1", "us-west-2"]
								}
							}`),
						},
					},
					RequiredSchemas: map[string]*fnv1.Schema{
						"example.crossplane.io/v1, Kind=XBucket": schemaXBucket,
						"s3.aws.upbound.io/v1beta1, Kind=Bucket": schemaBucket,
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
								"kind": "XBucket",
								"status": {
									"bucketName": "my-bucket",
									"regionCount": 2
								}
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
							},
						},
					},
				},
			},
		},
		"SchemaSpecIntegerResolvesIntoResource": {
			reason: "An integer read from the XR's spec (schema.spec.*) into a resource template resolves successfully.",
			args: args{
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test", Capabilities: []fnv1.Capability{fnv1.Capability_CAPABILITY_CAPABILITIES, fnv1.Capability_CAPABILITY_REQUIRED_SCHEMAS}},
					Input: resource.MustStructJSON(`{
						"apiVersion": "kro.fn.crossplane.io/v1alpha1",
						"kind": "ResourceGraph",
						"resources": [{
							"id": "bucket",
							"template": {
								"apiVersion": "s3.aws.upbound.io/v1beta1",
								"kind": "Bucket",
								"metadata": {},
								"spec": {
									"forProvider": {
										"region": "us-west-2",
										"replicas": "${schema.spec.replicas}"
									}
								}
							}
						}]
					}`),
					Observed: &fnv1.State{
						Composite: &fnv1.Resource{
							// The integer for spec.replicas should get decoded/processed and be readable by the KRO runtime
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.crossplane.io/v1",
								"kind": "XBucket",
								"metadata": {"name": "test-bucket"},
								"spec": {"bucketName": "my-bucket", "replicas": 3}
							}`),
						},
					},
					RequiredSchemas: map[string]*fnv1.Schema{
						"example.crossplane.io/v1, Kind=XBucket": schemaXBucket,
						"s3.aws.upbound.io/v1beta1, Kind=Bucket": schemaBucket,
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
								// The integer from schema.spec.replicas should land successfully in
								// the rendered template as a number.
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket",
									"metadata": {},
									"spec": {
										"forProvider": {
											"region": "us-west-2",
											"replicas": 3
										}
									}
								}`),
							},
						},
					},
				},
			},
		},
		"ObservedIntegerResolvesIntoStatus": {
			reason: "An integer-typed field read from an observed composed resource resolves successfully into XR status.",
			args: args{
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test", Capabilities: []fnv1.Capability{fnv1.Capability_CAPABILITY_CAPABILITIES, fnv1.Capability_CAPABILITY_REQUIRED_SCHEMAS}},
					Input: resource.MustStructJSON(`{
						"apiVersion": "kro.fn.crossplane.io/v1alpha1",
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
							"bucketName": "${bucket.status.atProvider.id}",
							"regionCount": "${bucket.status.atProvider.objectCount}"
						}
					}`),
					Observed: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.crossplane.io/v1",
								"kind": "XBucket",
								"metadata": {"name": "test-bucket"},
								"spec": {"bucketName": "my-bucket"}
							}`),
						},
						Resources: map[string]*fnv1.Resource{
							"bucket": {
								// objectCount is an integer in the schema, and we should process it
								// correctly and hand it off to KRO in the format it expects
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket",
									"metadata": {"name": "test-bucket-xyz"},
									"spec": {
										"forProvider": {
											"region": "us-west-2"
										}
									},
									"status": {
										"atProvider": {
											"id": "test-bucket-xyz",
											"objectCount": 5
										}
									}
								}`),
							},
						},
					},
					RequiredSchemas: map[string]*fnv1.Schema{
						"example.crossplane.io/v1, Kind=XBucket": schemaXBucket,
						"s3.aws.upbound.io/v1beta1, Kind=Bucket": schemaBucket,
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
							// The integer read from observed state lands in status as a
							// number, alongside the string field.
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.crossplane.io/v1",
								"kind": "XBucket",
								"status": {
									"bucketName": "test-bucket-xyz",
									"regionCount": 5
								}
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
							},
						},
					},
				},
			},
		},
		"ObservedIntOrStringResolvesIntoStatus": {
			reason: "An x-kubernetes-int-or-string field holding an integer, read from an observed composed resource, resolves into XR status.",
			args: args{
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test", Capabilities: []fnv1.Capability{fnv1.Capability_CAPABILITY_CAPABILITIES, fnv1.Capability_CAPABILITY_REQUIRED_SCHEMAS}},
					Input: resource.MustStructJSON(`{
						"apiVersion": "kro.fn.crossplane.io/v1alpha1",
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
							"port": "${bucket.status.atProvider.port}"
						}
					}`),
					Observed: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.crossplane.io/v1",
								"kind": "XBucket",
								"metadata": {"name": "test-bucket"},
								"spec": {"bucketName": "my-bucket"}
							}`),
						},
						Resources: map[string]*fnv1.Resource{
							"bucket": {
								// port is x-kubernetes-int-or-string and we're using the integer
								// variant here. This should be proccessed and handed to KRO runtime
								// successfully in a format it expects.
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket",
									"metadata": {"name": "test-bucket-xyz"},
									"spec": {
										"forProvider": {
											"region": "us-west-2"
										}
									},
									"status": {
										"atProvider": {
											"port": 8080
										}
									}
								}`),
							},
						},
					},
					RequiredSchemas: map[string]*fnv1.Schema{
						"example.crossplane.io/v1, Kind=XBucket": schemaXBucket,
						"s3.aws.upbound.io/v1beta1, Kind=Bucket": schemaBucket,
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
							// the x-kubernetes-int-or-string port value should have successfully
							// made it into the status of the XR
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.crossplane.io/v1",
								"kind": "XBucket",
								"status": {
									"port": 8080
								}
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
					Meta: &fnv1.RequestMeta{Tag: "test", Capabilities: []fnv1.Capability{fnv1.Capability_CAPABILITY_CAPABILITIES, fnv1.Capability_CAPABILITY_REQUIRED_SCHEMAS}},
					Input: resource.MustStructJSON(`{
						"apiVersion": "kro.fn.crossplane.io/v1alpha1",
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
						"example.crossplane.io/v1, Kind=XBucket": schemaXBucket,
						"s3.aws.upbound.io/v1beta1, Kind=Bucket": schemaBucket,
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
								// No readyWhen defined, so Ready is left unspecified for function-auto-ready to handle
							},
						},
					},
				},
			},
		},
		"ExternalRefUsedInTemplate": {
			reason: "External refs should be fetched and their data available in CEL expressions, but not included in desired output",
			args: args{
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test", Capabilities: []fnv1.Capability{fnv1.Capability_CAPABILITY_CAPABILITIES, fnv1.Capability_CAPABILITY_REQUIRED_SCHEMAS}},
					Input: resource.MustStructJSON(`{
						"apiVersion": "kro.fn.crossplane.io/v1alpha1",
						"kind": "ResourceGraph",
						"resources": [{
							"id": "config",
							"externalRef": {
								"apiVersion": "v1",
								"kind": "ConfigMap",
								"metadata": {
									"name": "platform-config"
								}
							}
						}, {
							"id": "bucket",
							"template": {
								"apiVersion": "s3.aws.upbound.io/v1beta1",
								"kind": "Bucket",
								"metadata": {},
								"spec": {
									"forProvider": {
										"region": "${config.data.region}"
									}
								}
							}
						}],
						"status": {
							"region": "${config.data.region}"
						}
					}`),
					Observed: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.crossplane.io/v1",
								"kind": "XBucket",
								"metadata": {"name": "test-bucket", "namespace": "xr-ns"},
								"spec": {}
							}`),
						},
					},
					RequiredSchemas: map[string]*fnv1.Schema{
						"example.crossplane.io/v1, Kind=XBucket": schemaXBucket,
						"s3.aws.upbound.io/v1beta1, Kind=Bucket": schemaBucket,
						"/v1, Kind=ConfigMap":                    schemaConfigMap,
					},
					RequiredResources: map[string]*fnv1.Resources{
						"config": {
							Items: []*fnv1.Resource{{
								Resource: resource.MustStructJSON(`{
									"apiVersion": "v1",
									"kind": "ConfigMap",
									"metadata": {"name": "platform-config", "namespace": "xr-ns"},
									"data": {
										"region": "cool-region-2",
										"environment": "production"
									}
								}`),
							}},
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
							"/v1, Kind=ConfigMap": {
								ApiVersion: "v1",
								Kind:       "ConfigMap",
							},
						},
						Resources: map[string]*fnv1.ResourceSelector{
							"config": {
								ApiVersion: "v1",
								Kind:       "ConfigMap",
								Match:      &fnv1.ResourceSelector_MatchName{MatchName: "platform-config"},
								Namespace:  new("xr-ns"),
							},
						},
					},
					Desired: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.crossplane.io/v1",
								"kind": "XBucket",
								"status": {
									"region": "cool-region-2"
								}
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
											"region": "cool-region-2"
										}
									}
								}`),
							},
						},
					},
				},
			},
		},
		"ExternalRefWithCELExpressionInName": {
			reason: "External refs should support CEL expressions that reference schema.spec fields",
			args: args{
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test", Capabilities: []fnv1.Capability{fnv1.Capability_CAPABILITY_CAPABILITIES, fnv1.Capability_CAPABILITY_REQUIRED_SCHEMAS}},
					Input: resource.MustStructJSON(`{
						"apiVersion": "kro.fn.crossplane.io/v1alpha1",
						"kind": "ResourceGraph",
						"resources": [{
							"id": "config",
							"externalRef": {
								"apiVersion": "v1",
								"kind": "ConfigMap",
								"metadata": {
									"name": "${schema.spec.configMapName}"
								}
							}
						}, {
							"id": "bucket",
							"template": {
								"apiVersion": "s3.aws.upbound.io/v1beta1",
								"kind": "Bucket",
								"metadata": {},
								"spec": {
									"forProvider": {
										"region": "${config.data.region}"
									}
								}
							}
						}],
						"status": {
							"region": "${config.data.region}"
						}
					}`),
					Observed: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.crossplane.io/v1",
								"kind": "XBucket",
								"metadata": {"name": "test-bucket", "namespace": "xr-ns"},
								"spec": {
									"configMapName": "my-platform-config"
								}
							}`),
						},
					},
					RequiredSchemas: map[string]*fnv1.Schema{
						"example.crossplane.io/v1, Kind=XBucket": schemaXBucket,
						"s3.aws.upbound.io/v1beta1, Kind=Bucket": schemaBucket,
						"/v1, Kind=ConfigMap":                    schemaConfigMap,
					},
					RequiredResources: map[string]*fnv1.Resources{
						"config": {
							Items: []*fnv1.Resource{{
								Resource: resource.MustStructJSON(`{
									"apiVersion": "v1",
									"kind": "ConfigMap",
									"metadata": {"name": "my-platform-config", "namespace": "xr-ns"},
									"data": {
										"region": "us-west-2",
										"environment": "production"
									}
								}`),
							}},
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
							"/v1, Kind=ConfigMap": {
								ApiVersion: "v1",
								Kind:       "ConfigMap",
							},
						},
						Resources: map[string]*fnv1.ResourceSelector{
							// Key assertion: the external ref name should be evaluated from CEL expression
							// "${schema.spec.configMapName}" -> "my-platform-config"
							"config": {
								ApiVersion: "v1",
								Kind:       "ConfigMap",
								Match:      &fnv1.ResourceSelector_MatchName{MatchName: "my-platform-config"},
								Namespace:  new("xr-ns"),
							},
						},
					},
					Desired: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.crossplane.io/v1",
								"kind": "XBucket",
								"status": {
									"region": "us-west-2"
								}
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
							},
						},
					},
				},
			},
		},
		"ExternalCollection": {
			reason: "External collections with label selectors should produce MatchLabels selector, pass all matched resources to runtime, and expose the collection as a CEL list",
			args: args{
				ctx: context.Background(),
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test", Capabilities: []fnv1.Capability{fnv1.Capability_CAPABILITY_CAPABILITIES, fnv1.Capability_CAPABILITY_REQUIRED_SCHEMAS}},
					Input: resource.MustStructJSON(`{
						"apiVersion": "kro.fn.crossplane.io/v1alpha1",
						"kind": "ResourceGraph",
						"resources": [{
							"id": "configs",
							"externalRef": {
								"apiVersion": "v1",
								"kind": "ConfigMap",
								"metadata": {
									"selector": {
										"matchLabels": {
											"app": "${schema.metadata.name}"
										}
									}
								}
							}
						}, {
							"id": "bucket",
							"template": {
								"apiVersion": "s3.aws.upbound.io/v1beta1",
								"kind": "Bucket",
								"metadata": {},
								"spec": {
									"forProvider": {
										"region": "${configs[0].data.region}"
									}
								}
							}
						}],
						"status": {
							"region": "${configs[0].data.region}",
							"regionCount": "${size(configs)}"
						}
					}`),
					Observed: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.crossplane.io/v1",
								"kind": "XBucket",
								"metadata": {"name": "test-bucket", "namespace": "xr-ns"},
								"spec": {}
							}`),
						},
					},
					RequiredSchemas: map[string]*fnv1.Schema{
						"example.crossplane.io/v1, Kind=XBucket": schemaXBucket,
						"s3.aws.upbound.io/v1beta1, Kind=Bucket": schemaBucket,
						"/v1, Kind=ConfigMap":                    schemaConfigMap,
					},
					RequiredResources: map[string]*fnv1.Resources{
						// Crossplane returns multiple ConfigMaps matching the label selector
						"configs": {
							Items: []*fnv1.Resource{
								{
									Resource: resource.MustStructJSON(`{
										"apiVersion": "v1",
										"kind": "ConfigMap",
										"metadata": {"name": "config-1", "namespace": "xr-ns", "labels": {"app": "test-bucket"}},
										"data": {"region": "us-west-2"}
									}`),
								},
								{
									Resource: resource.MustStructJSON(`{
										"apiVersion": "v1",
										"kind": "ConfigMap",
										"metadata": {"name": "config-2", "namespace": "xr-ns", "labels": {"app": "test-bucket"}},
										"data": {"region": "eu-west-1"}
									}`),
								},
							},
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
							"/v1, Kind=ConfigMap": {
								ApiVersion: "v1",
								Kind:       "ConfigMap",
							},
						},
						Resources: map[string]*fnv1.ResourceSelector{
							// Key assertion: external collection should produce MatchLabels, not MatchName
							"configs": {
								ApiVersion: "v1",
								Kind:       "ConfigMap",
								Match: &fnv1.ResourceSelector_MatchLabels{
									MatchLabels: &fnv1.MatchLabels{
										Labels: map[string]string{"app": "test-bucket"},
									},
								},
								// No namespace — external collections with empty namespace mean "all namespaces"
							},
						},
					},
					Desired: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.crossplane.io/v1",
								"kind": "XBucket",
								"status": {
									"region": "us-west-2",
									"regionCount": 2
								}
							}`),
						},
						Resources: map[string]*fnv1.Resource{
							"bucket": {
								// The bucket should use the first config's region from the external collection
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
							},
						},
					},
				},
			},
		},
		"ExternalCollectionEmptyMatch": {
			reason: "An external collection that matches nothing should be a ready, empty collection, so resources that depend on it are composed",
			args: args{
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test", Capabilities: []fnv1.Capability{fnv1.Capability_CAPABILITY_CAPABILITIES, fnv1.Capability_CAPABILITY_REQUIRED_SCHEMAS}},
					Input: resource.MustStructJSON(`{
						"apiVersion": "kro.fn.crossplane.io/v1alpha1",
						"kind": "ResourceGraph",
						"resources": [{
							"id": "configs",
							"externalRef": {
								"apiVersion": "v1",
								"kind": "ConfigMap",
								"metadata": {
									"selector": {
										"matchLabels": {
											"app": "${schema.metadata.name}"
										}
									}
								}
							}
						}, {
							"id": "bucket",
							"template": {
								"apiVersion": "s3.aws.upbound.io/v1beta1",
								"kind": "Bucket",
								"metadata": {},
								"spec": {
									"forProvider": {
										"replicas": "${size(configs)}"
									}
								}
							}
						}]
					}`),
					Observed: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.crossplane.io/v1",
								"kind": "XBucket",
								"metadata": {"name": "test-bucket", "namespace": "xr-ns"},
								"spec": {}
							}`),
						},
					},
					RequiredSchemas: map[string]*fnv1.Schema{
						"example.crossplane.io/v1, Kind=XBucket": schemaXBucket,
						"s3.aws.upbound.io/v1beta1, Kind=Bucket": schemaBucket,
						"/v1, Kind=ConfigMap":                    schemaConfigMap,
					},
					RequiredResources: map[string]*fnv1.Resources{
						// Crossplane fetched the collection and nothing matched
						"configs": {Items: []*fnv1.Resource{}},
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
							"/v1, Kind=ConfigMap": {
								ApiVersion: "v1",
								Kind:       "ConfigMap",
							},
						},
						Resources: map[string]*fnv1.ResourceSelector{
							"configs": {
								ApiVersion: "v1",
								Kind:       "ConfigMap",
								Match: &fnv1.ResourceSelector_MatchLabels{
									MatchLabels: &fnv1.MatchLabels{
										Labels: map[string]string{"app": "test-bucket"},
									},
								},
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
											"replicas": 0
										}
									}
								}`),
							},
						},
					},
				},
			},
		},
		"ExternalCollectionMatchExpressions": {
			reason: "Crossplane can only select by labels, so an external collection should request its matchLabels and filter what Crossplane fetched by its matchExpressions",
			args: args{
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test", Capabilities: []fnv1.Capability{fnv1.Capability_CAPABILITY_CAPABILITIES, fnv1.Capability_CAPABILITY_REQUIRED_SCHEMAS}},
					Input: resource.MustStructJSON(`{
						"apiVersion": "kro.fn.crossplane.io/v1alpha1",
						"kind": "ResourceGraph",
						"resources": [{
							"id": "configs",
							"externalRef": {
								"apiVersion": "v1",
								"kind": "ConfigMap",
								"metadata": {
									"selector": {
										"matchLabels": {
											"app": "${schema.metadata.name}"
										},
										"matchExpressions": [{
											"key": "tier",
											"operator": "In",
											"values": ["gold"]
										}]
									}
								}
							}
						}, {
							"id": "bucket",
							"template": {
								"apiVersion": "s3.aws.upbound.io/v1beta1",
								"kind": "Bucket",
								"metadata": {},
								"spec": {
									"forProvider": {
										"replicas": "${size(configs)}"
									}
								}
							}
						}]
					}`),
					Observed: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.crossplane.io/v1",
								"kind": "XBucket",
								"metadata": {"name": "test-bucket", "namespace": "xr-ns"},
								"spec": {}
							}`),
						},
					},
					RequiredSchemas: map[string]*fnv1.Schema{
						"example.crossplane.io/v1, Kind=XBucket": schemaXBucket,
						"s3.aws.upbound.io/v1beta1, Kind=Bucket": schemaBucket,
						"/v1, Kind=ConfigMap":                    schemaConfigMap,
					},
					RequiredResources: map[string]*fnv1.Resources{
						// Crossplane returns every ConfigMap matching the matchLabels
						"configs": {
							Items: []*fnv1.Resource{
								{
									Resource: resource.MustStructJSON(`{
										"apiVersion": "v1",
										"kind": "ConfigMap",
										"metadata": {"name": "config-1", "namespace": "xr-ns", "labels": {"app": "test-bucket", "tier": "gold"}}
									}`),
								},
								{
									Resource: resource.MustStructJSON(`{
										"apiVersion": "v1",
										"kind": "ConfigMap",
										"metadata": {"name": "config-2", "namespace": "xr-ns", "labels": {"app": "test-bucket", "tier": "silver"}}
									}`),
								},
							},
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
							"/v1, Kind=ConfigMap": {
								ApiVersion: "v1",
								Kind:       "ConfigMap",
							},
						},
						Resources: map[string]*fnv1.ResourceSelector{
							// Only the matchLabels are requested
							"configs": {
								ApiVersion: "v1",
								Kind:       "ConfigMap",
								Match: &fnv1.ResourceSelector_MatchLabels{
									MatchLabels: &fnv1.MatchLabels{
										Labels: map[string]string{"app": "test-bucket"},
									},
								},
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
								// Only config-1 has tier=gold
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket",
									"metadata": {},
									"spec": {
										"forProvider": {
											"replicas": 1
										}
									}
								}`),
							},
						},
					},
				},
			},
		},
		"IncludeWhenExcludesResource": {
			reason: "Resources with includeWhen conditions that evaluate to false should be excluded from desired output",
			args: args{
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test", Capabilities: []fnv1.Capability{fnv1.Capability_CAPABILITY_CAPABILITIES, fnv1.Capability_CAPABILITY_REQUIRED_SCHEMAS}},
					Input: resource.MustStructJSON(`{
						"apiVersion": "kro.fn.crossplane.io/v1alpha1",
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
						}, {
							"id": "logging",
							"includeWhen": ["${schema.spec.enableLogging == true}"],
							"template": {
								"apiVersion": "s3.aws.upbound.io/v1beta1",
								"kind": "BucketLogging",
								"metadata": {},
								"spec": {
									"forProvider": {
										"bucket": "${bucket.status.atProvider.id}",
										"targetPrefix": "logs/"
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
								"spec": {
									"enableLogging": false
								}
							}`),
						},
					},
					RequiredSchemas: map[string]*fnv1.Schema{
						"example.crossplane.io/v1, Kind=XBucket":        schemaXBucket,
						"s3.aws.upbound.io/v1beta1, Kind=Bucket":        schemaBucket,
						"s3.aws.upbound.io/v1beta1, Kind=BucketLogging": schemaBucketLogging,
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
							"s3.aws.upbound.io/v1beta1, Kind=BucketLogging": {
								ApiVersion: "s3.aws.upbound.io/v1beta1",
								Kind:       "BucketLogging",
							},
						},
					},
					Desired: &fnv1.State{
						Composite: &fnv1.Resource{
							// No status since bucket isn't observed yet
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.crossplane.io/v1",
								"kind": "XBucket"
							}`),
						},
						Resources: map[string]*fnv1.Resource{
							// Only bucket is included; logging is excluded because enableLogging is false
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
							},
						},
					},
				},
			},
		},
		"IncludeWhenReadsAnotherResource": {
			reason: "includeWhen can read another resource's observed state, and a resource whose includeWhen is false is left out even though it exists",
			args: args{
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test", Capabilities: []fnv1.Capability{fnv1.Capability_CAPABILITY_CAPABILITIES, fnv1.Capability_CAPABILITY_REQUIRED_SCHEMAS}},
					Input: resource.MustStructJSON(`{
						"apiVersion": "kro.fn.crossplane.io/v1alpha1",
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
						}, {
							"id": "logging",
							"includeWhen": ["${bucket.status.atProvider.objectCount > 0}"],
							"template": {
								"apiVersion": "s3.aws.upbound.io/v1beta1",
								"kind": "BucketLogging",
								"metadata": {},
								"spec": {
									"forProvider": {
										"bucket": "${bucket.metadata.name}",
										"targetPrefix": "logs/"
									}
								}
							}
						}]
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
							// bucket has no objects, so logging's includeWhen is false
							"bucket": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket",
									"metadata": {"name": "test-bucket-abc123"},
									"spec": {"forProvider": {"region": "us-west-2"}},
									"status": {"atProvider": {"id": "test-bucket-abc123", "objectCount": 0}}
								}`),
							},
							"logging": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "BucketLogging",
									"metadata": {"name": "test-bucket-logging-xyz"},
									"spec": {"forProvider": {"bucket": "test-bucket-abc123", "targetPrefix": "logs/"}}
								}`),
							},
						},
					},
					RequiredSchemas: map[string]*fnv1.Schema{
						"example.crossplane.io/v1, Kind=XBucket":        schemaXBucket,
						"s3.aws.upbound.io/v1beta1, Kind=Bucket":        schemaBucket,
						"s3.aws.upbound.io/v1beta1, Kind=BucketLogging": schemaBucketLogging,
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
							"s3.aws.upbound.io/v1beta1, Kind=BucketLogging": {
								ApiVersion: "s3.aws.upbound.io/v1beta1",
								Kind:       "BucketLogging",
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
							},
							// logging is left out, so Crossplane deletes it
						},
					},
				},
			},
		},
		"ExistingResourceKeptWhenIncludeWhenWaitsOnData": {
			reason: "A resource that already exists should still be composed while its includeWhen reads data that isn't there yet, or Crossplane would delete it",
			args: args{
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test", Capabilities: []fnv1.Capability{fnv1.Capability_CAPABILITY_CAPABILITIES, fnv1.Capability_CAPABILITY_REQUIRED_SCHEMAS}},
					Input: resource.MustStructJSON(`{
						"apiVersion": "kro.fn.crossplane.io/v1alpha1",
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
						}, {
							"id": "logging",
							"includeWhen": ["${bucket.status.atProvider.objectCount > 0}"],
							"template": {
								"apiVersion": "s3.aws.upbound.io/v1beta1",
								"kind": "BucketLogging",
								"metadata": {},
								"spec": {
									"forProvider": {
										"bucket": "${bucket.metadata.name}",
										"targetPrefix": "logs/"
									}
								}
							}
						}]
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
							// bucket has no status yet, so logging's includeWhen can't be evaluated
							"bucket": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket",
									"metadata": {"name": "test-bucket-abc123"},
									"spec": {"forProvider": {"region": "us-west-2"}}
								}`),
							},
							"logging": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "BucketLogging",
									"metadata": {"name": "test-bucket-logging-xyz"},
									"spec": {"forProvider": {"bucket": "test-bucket-abc123", "targetPrefix": "logs/"}}
								}`),
							},
						},
					},
					RequiredSchemas: map[string]*fnv1.Schema{
						"example.crossplane.io/v1, Kind=XBucket":        schemaXBucket,
						"s3.aws.upbound.io/v1beta1, Kind=Bucket":        schemaBucket,
						"s3.aws.upbound.io/v1beta1, Kind=BucketLogging": schemaBucketLogging,
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
							"s3.aws.upbound.io/v1beta1, Kind=BucketLogging": {
								ApiVersion: "s3.aws.upbound.io/v1beta1",
								Kind:       "BucketLogging",
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
							},
							"logging": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "BucketLogging",
									"metadata": {},
									"spec": {
										"forProvider": {
											"bucket": "test-bucket-abc123",
											"targetPrefix": "logs/"
										}
									}
								}`),
							},
						},
					},
				},
			},
		},
		"ReadyWhenTrue": {
			reason: "When readyWhen is defined and the expression evaluates to true, the resource should be marked ReadyTrue",
			args: args{
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test", Capabilities: []fnv1.Capability{fnv1.Capability_CAPABILITY_CAPABILITIES, fnv1.Capability_CAPABILITY_REQUIRED_SCHEMAS}},
					Input: resource.MustStructJSON(`{
						"apiVersion": "kro.fn.crossplane.io/v1alpha1",
						"kind": "ResourceGraph",
						"resources": [{
							"id": "bucket",
							"readyWhen": ["${bucket.status.?atProvider.?id.hasValue()}"],
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
											"region": "us-west-2"
										}
									},
									"status": {
										"atProvider": {
											"id": "test-bucket-abc123"
										}
									}
								}`),
							},
						},
					},
					RequiredSchemas: map[string]*fnv1.Schema{
						"example.crossplane.io/v1, Kind=XBucket": schemaXBucket,
						"s3.aws.upbound.io/v1beta1, Kind=Bucket": schemaBucket,
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
								"kind": "XBucket",
								"status": {
									"bucketName": "test-bucket-abc123"
								}
							}`),
						},
						Resources: map[string]*fnv1.Resource{
							"bucket": {
								Ready: fnv1.Ready_READY_TRUE, // resource is marked ready
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
							},
						},
					},
				},
			},
		},
		"ReadyWhenFalse": {
			reason: "When readyWhen is defined but the expression evaluates to false, the resource should be marked ReadyFalse",
			args: args{
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test", Capabilities: []fnv1.Capability{fnv1.Capability_CAPABILITY_CAPABILITIES, fnv1.Capability_CAPABILITY_REQUIRED_SCHEMAS}},
					Input: resource.MustStructJSON(`{
						"apiVersion": "kro.fn.crossplane.io/v1alpha1",
						"kind": "ResourceGraph",
						"resources": [{
							"id": "bucket",
							"readyWhen": ["${bucket.status.?atProvider.?id.hasValue()}"],
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
											"region": "us-west-2"
										}
									}
								}`),
							},
						},
					},
					RequiredSchemas: map[string]*fnv1.Schema{
						"example.crossplane.io/v1, Kind=XBucket": schemaXBucket,
						"s3.aws.upbound.io/v1beta1, Kind=Bucket": schemaBucket,
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
							// No status — bucket.status.atProvider.id is not available
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.crossplane.io/v1",
								"kind": "XBucket"
							}`),
						},
						Resources: map[string]*fnv1.Resource{
							"bucket": {
								Ready: fnv1.Ready_READY_FALSE, // marked as not ready
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
							},
						},
					},
				},
			},
		},
		"DependentWaitsForReadyWhen": {
			reason: "A resource should not be composed until the resources it depends on satisfy their readyWhen conditions, even when the data it reads is available",
			args: args{
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test", Capabilities: []fnv1.Capability{fnv1.Capability_CAPABILITY_CAPABILITIES, fnv1.Capability_CAPABILITY_REQUIRED_SCHEMAS}},
					Input: resource.MustStructJSON(`{
						"apiVersion": "kro.fn.crossplane.io/v1alpha1",
						"kind": "ResourceGraph",
						"resources": [{
							"id": "bucket",
							"readyWhen": ["${bucket.status.?atProvider.?id.hasValue()}"],
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
						}, {
							"id": "logging",
							"template": {
								"apiVersion": "s3.aws.upbound.io/v1beta1",
								"kind": "BucketLogging",
								"metadata": {},
								"spec": {
									"forProvider": {
										"bucket": "${bucket.metadata.name}",
										"targetPrefix": "logs/"
									}
								}
							}
						}]
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
							// bucket has a name, but isn't ready
							"bucket": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket",
									"metadata": {"name": "test-bucket-abc123"},
									"spec": {"forProvider": {"region": "us-west-2"}}
								}`),
							},
						},
					},
					RequiredSchemas: map[string]*fnv1.Schema{
						"example.crossplane.io/v1, Kind=XBucket":        schemaXBucket,
						"s3.aws.upbound.io/v1beta1, Kind=Bucket":        schemaBucket,
						"s3.aws.upbound.io/v1beta1, Kind=BucketLogging": schemaBucketLogging,
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
							"s3.aws.upbound.io/v1beta1, Kind=BucketLogging": {
								ApiVersion: "s3.aws.upbound.io/v1beta1",
								Kind:       "BucketLogging",
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
							// logging waits for bucket to be ready
							"bucket": {
								Ready: fnv1.Ready_READY_FALSE,
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
							},
						},
					},
				},
			},
		},
		"ExistingDependentKeptWhenDependencyNotReady": {
			reason: "A resource that already exists should still be composed while a resource it depends on is not ready, or Crossplane would delete it",
			args: args{
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test", Capabilities: []fnv1.Capability{fnv1.Capability_CAPABILITY_CAPABILITIES, fnv1.Capability_CAPABILITY_REQUIRED_SCHEMAS}},
					Input: resource.MustStructJSON(`{
						"apiVersion": "kro.fn.crossplane.io/v1alpha1",
						"kind": "ResourceGraph",
						"resources": [{
							"id": "bucket",
							"readyWhen": ["${bucket.status.?atProvider.?id.hasValue()}"],
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
						}, {
							"id": "logging",
							"template": {
								"apiVersion": "s3.aws.upbound.io/v1beta1",
								"kind": "BucketLogging",
								"metadata": {},
								"spec": {
									"forProvider": {
										"bucket": "${bucket.metadata.name}",
										"targetPrefix": "logs/"
									}
								}
							}
						}]
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
							// bucket was ready when logging was created, but isn't now
							"bucket": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket",
									"metadata": {"name": "test-bucket-abc123"},
									"spec": {"forProvider": {"region": "us-west-2"}}
								}`),
							},
							"logging": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "BucketLogging",
									"metadata": {"name": "test-bucket-logging-xyz"},
									"spec": {"forProvider": {"bucket": "test-bucket-abc123", "targetPrefix": "logs/"}}
								}`),
							},
						},
					},
					RequiredSchemas: map[string]*fnv1.Schema{
						"example.crossplane.io/v1, Kind=XBucket":        schemaXBucket,
						"s3.aws.upbound.io/v1beta1, Kind=Bucket":        schemaBucket,
						"s3.aws.upbound.io/v1beta1, Kind=BucketLogging": schemaBucketLogging,
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
							"s3.aws.upbound.io/v1beta1, Kind=BucketLogging": {
								ApiVersion: "s3.aws.upbound.io/v1beta1",
								Kind:       "BucketLogging",
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
								Ready: fnv1.Ready_READY_FALSE,
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
							},
							"logging": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "BucketLogging",
									"metadata": {},
									"spec": {
										"forProvider": {
											"bucket": "test-bucket-abc123",
											"targetPrefix": "logs/"
										}
									}
								}`),
							},
						},
					},
				},
			},
		},
		"ExistingDependentKeptWhenExternalRefWaitsOnDependency": {
			reason: "A resource that already exists and reads an external ref should still be composed while the resource the ref's name comes from is not ready, and the ref should still be requested",
			args: args{
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test", Capabilities: []fnv1.Capability{fnv1.Capability_CAPABILITY_CAPABILITIES, fnv1.Capability_CAPABILITY_REQUIRED_SCHEMAS}},
					Input: resource.MustStructJSON(`{
						"apiVersion": "kro.fn.crossplane.io/v1alpha1",
						"kind": "ResourceGraph",
						"resources": [{
							"id": "settings",
							"readyWhen": ["${settings.data.approved == 'true'}"],
							"template": {
								"apiVersion": "v1",
								"kind": "ConfigMap",
								"metadata": {"name": "settings"},
								"data": {
									"sharedConfig": "platform-config",
									"approved": "false"
								}
							}
						}, {
							"id": "platform",
							"externalRef": {
								"apiVersion": "v1",
								"kind": "ConfigMap",
								"metadata": {
									"name": "${settings.data.sharedConfig}"
								}
							}
						}, {
							"id": "app",
							"template": {
								"apiVersion": "v1",
								"kind": "ConfigMap",
								"metadata": {"name": "app"},
								"data": {
									"region": "${platform.data.region}"
								}
							}
						}]
					}`),
					Observed: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.crossplane.io/v1",
								"kind": "XBucket",
								"metadata": {"name": "test-bucket", "namespace": "xr-ns"},
								"spec": {}
							}`),
						},
						Resources: map[string]*fnv1.Resource{
							// settings was ready when app was created, but isn't now
							"settings": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "v1",
									"kind": "ConfigMap",
									"metadata": {"name": "settings", "namespace": "xr-ns"},
									"data": {"sharedConfig": "platform-config", "approved": "false"}
								}`),
							},
							"app": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "v1",
									"kind": "ConfigMap",
									"metadata": {"name": "app", "namespace": "xr-ns"},
									"data": {"region": "us-west-2"}
								}`),
							},
						},
					},
					RequiredSchemas: map[string]*fnv1.Schema{
						"example.crossplane.io/v1, Kind=XBucket": schemaXBucket,
						"/v1, Kind=ConfigMap":                    schemaConfigMap,
					},
					RequiredResources: map[string]*fnv1.Resources{
						"platform": {
							Items: []*fnv1.Resource{{
								Resource: resource.MustStructJSON(`{
									"apiVersion": "v1",
									"kind": "ConfigMap",
									"metadata": {"name": "platform-config", "namespace": "xr-ns"},
									"data": {"region": "us-west-2"}
								}`),
							}},
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
							"/v1, Kind=ConfigMap": {
								ApiVersion: "v1",
								Kind:       "ConfigMap",
							},
						},
						Resources: map[string]*fnv1.ResourceSelector{
							"platform": {
								ApiVersion: "v1",
								Kind:       "ConfigMap",
								Match:      &fnv1.ResourceSelector_MatchName{MatchName: "platform-config"},
								Namespace:  new("xr-ns"),
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
							"settings": {
								Ready: fnv1.Ready_READY_FALSE,
								Resource: resource.MustStructJSON(`{
									"apiVersion": "v1",
									"kind": "ConfigMap",
									"metadata": {"name": "settings"},
									"data": {
										"sharedConfig": "platform-config",
										"approved": "false"
									}
								}`),
							},
							"app": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "v1",
									"kind": "ConfigMap",
									"metadata": {"name": "app"},
									"data": {
										"region": "us-west-2"
									}
								}`),
							},
						},
					},
				},
			},
		},
		"ExistingDependentWaitsForExternalRefFetch": {
			reason: "A resource that already exists and reads an external ref Crossplane hasn't fetched yet should not fail the run, so Crossplane can fetch the ref and call the function again",
			args: args{
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test", Capabilities: []fnv1.Capability{fnv1.Capability_CAPABILITY_CAPABILITIES, fnv1.Capability_CAPABILITY_REQUIRED_SCHEMAS}},
					Input: resource.MustStructJSON(`{
						"apiVersion": "kro.fn.crossplane.io/v1alpha1",
						"kind": "ResourceGraph",
						"resources": [{
							"id": "platform",
							"externalRef": {
								"apiVersion": "v1",
								"kind": "ConfigMap",
								"metadata": {
									"name": "platform-config"
								}
							}
						}, {
							"id": "app",
							"template": {
								"apiVersion": "v1",
								"kind": "ConfigMap",
								"metadata": {"name": "app"},
								"data": {
									"region": "${platform.data.region}"
								}
							}
						}]
					}`),
					Observed: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.crossplane.io/v1",
								"kind": "XBucket",
								"metadata": {"name": "test-bucket", "namespace": "xr-ns"},
								"spec": {}
							}`),
						},
						Resources: map[string]*fnv1.Resource{
							"app": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "v1",
									"kind": "ConfigMap",
									"metadata": {"name": "app", "namespace": "xr-ns"},
									"data": {"region": "us-west-2"}
								}`),
							},
						},
					},
					RequiredSchemas: map[string]*fnv1.Schema{
						"example.crossplane.io/v1, Kind=XBucket": schemaXBucket,
						"/v1, Kind=ConfigMap":                    schemaConfigMap,
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
							"/v1, Kind=ConfigMap": {
								ApiVersion: "v1",
								Kind:       "ConfigMap",
							},
						},
						Resources: map[string]*fnv1.ResourceSelector{
							"platform": {
								ApiVersion: "v1",
								Kind:       "ConfigMap",
								Match:      &fnv1.ResourceSelector_MatchName{MatchName: "platform-config"},
								Namespace:  new("xr-ns"),
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
					},
				},
			},
		},
		"ExistingDependentOfMissingExternalRefNotRenderable": {
			reason: "A resource that already exists and reads an external ref Crossplane fetched but didn't find should return a fatal result, rather than being left out of desired state and deleted by Crossplane",
			args: args{
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test", Capabilities: []fnv1.Capability{fnv1.Capability_CAPABILITY_CAPABILITIES, fnv1.Capability_CAPABILITY_REQUIRED_SCHEMAS}},
					Input: resource.MustStructJSON(`{
						"apiVersion": "kro.fn.crossplane.io/v1alpha1",
						"kind": "ResourceGraph",
						"resources": [{
							"id": "platform",
							"externalRef": {
								"apiVersion": "v1",
								"kind": "ConfigMap",
								"metadata": {
									"name": "platform-config"
								}
							}
						}, {
							"id": "app",
							"template": {
								"apiVersion": "v1",
								"kind": "ConfigMap",
								"metadata": {"name": "app"},
								"data": {
									"region": "${platform.data.region}"
								}
							}
						}]
					}`),
					Observed: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.crossplane.io/v1",
								"kind": "XBucket",
								"metadata": {"name": "test-bucket", "namespace": "xr-ns"},
								"spec": {}
							}`),
						},
						Resources: map[string]*fnv1.Resource{
							"app": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "v1",
									"kind": "ConfigMap",
									"metadata": {"name": "app", "namespace": "xr-ns"},
									"data": {"region": "us-west-2"}
								}`),
							},
						},
					},
					RequiredSchemas: map[string]*fnv1.Schema{
						"example.crossplane.io/v1, Kind=XBucket": schemaXBucket,
						"/v1, Kind=ConfigMap":                    schemaConfigMap,
					},
					RequiredResources: map[string]*fnv1.Resources{
						"platform": {},
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
							"/v1, Kind=ConfigMap": {
								ApiVersion: "v1",
								Kind:       "ConfigMap",
							},
						},
						Resources: map[string]*fnv1.ResourceSelector{
							"platform": {
								ApiVersion: "v1",
								Kind:       "ConfigMap",
								Match:      &fnv1.ResourceSelector_MatchName{MatchName: "platform-config"},
								Namespace:  new("xr-ns"),
							},
						},
					},
					Results: []*fnv1.Result{{
						Severity: fnv1.Severity_SEVERITY_FATAL,
						Message:  `resource "app" already exists but cannot be rendered, refusing to delete it: node "app": eval "platform.data.region" at "data.region": eval "platform.data.region": no such attribute(s): platform (data pending)`,
						Target:   fnv1.Target_TARGET_COMPOSITE.Enum(),
					}},
				},
			},
		},
		"ExistingDependentWaitsForExternalCollectionFetch": {
			reason: "A resource that already exists and reads an external collection Crossplane hasn't fetched yet should not fail the run, so Crossplane can fetch the collection and call the function again",
			args: args{
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test", Capabilities: []fnv1.Capability{fnv1.Capability_CAPABILITY_CAPABILITIES, fnv1.Capability_CAPABILITY_REQUIRED_SCHEMAS}},
					Input: resource.MustStructJSON(`{
						"apiVersion": "kro.fn.crossplane.io/v1alpha1",
						"kind": "ResourceGraph",
						"resources": [{
							"id": "configs",
							"externalRef": {
								"apiVersion": "v1",
								"kind": "ConfigMap",
								"metadata": {
									"selector": {
										"matchLabels": {
											"app": "${schema.metadata.name}"
										}
									}
								}
							}
						}, {
							"id": "app",
							"template": {
								"apiVersion": "v1",
								"kind": "ConfigMap",
								"metadata": {"name": "app"},
								"data": {
									"region": "${configs[0].data.region}"
								}
							}
						}]
					}`),
					Observed: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.crossplane.io/v1",
								"kind": "XBucket",
								"metadata": {"name": "test-bucket", "namespace": "xr-ns"},
								"spec": {}
							}`),
						},
						Resources: map[string]*fnv1.Resource{
							"app": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "v1",
									"kind": "ConfigMap",
									"metadata": {"name": "app", "namespace": "xr-ns"},
									"data": {"region": "us-west-2"}
								}`),
							},
						},
					},
					RequiredSchemas: map[string]*fnv1.Schema{
						"example.crossplane.io/v1, Kind=XBucket": schemaXBucket,
						"/v1, Kind=ConfigMap":                    schemaConfigMap,
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
							"/v1, Kind=ConfigMap": {
								ApiVersion: "v1",
								Kind:       "ConfigMap",
							},
						},
						Resources: map[string]*fnv1.ResourceSelector{
							"configs": {
								ApiVersion: "v1",
								Kind:       "ConfigMap",
								Match: &fnv1.ResourceSelector_MatchLabels{
									MatchLabels: &fnv1.MatchLabels{
										Labels: map[string]string{"app": "test-bucket"},
									},
								},
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
					},
				},
			},
		},
		"CollectionForEach": {
			reason: "A forEach resource should expand into N composed resources named {id}-{index} with collection-index labels",
			args: args{
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test", Capabilities: []fnv1.Capability{fnv1.Capability_CAPABILITY_CAPABILITIES, fnv1.Capability_CAPABILITY_REQUIRED_SCHEMAS}},
					Input: resource.MustStructJSON(`{
						"apiVersion": "kro.fn.crossplane.io/v1alpha1",
						"kind": "ResourceGraph",
						"resources": [{
							"id": "bucket",
							"forEach": [{"region": "${schema.spec.regions}"}],
							"template": {
								"apiVersion": "s3.aws.upbound.io/v1beta1",
								"kind": "Bucket",
								"metadata": {
									"name": "${schema.metadata.name + '-' + region}"
								},
								"spec": {
									"forProvider": {
										"region": "${region}"
									}
								}
							}
						}]
					}`),
					Observed: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.crossplane.io/v1",
								"kind": "XBucket",
								"metadata": {"name": "test-bucket"},
								"spec": {
									"regions": ["us-east-1", "us-west-2"]
								}
							}`),
						},
					},
					RequiredSchemas: map[string]*fnv1.Schema{
						"example.crossplane.io/v1, Kind=XBucket": schemaXBucket,
						"s3.aws.upbound.io/v1beta1, Kind=Bucket": schemaBucket,
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
							"bucket-test-bucket-us-east-1": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket",
									"metadata": {
										"name": "test-bucket-us-east-1",
										"labels": {"kro.run/collection-index": "0"}
									},
									"spec": {
										"forProvider": {
											"region": "us-east-1"
										}
									}
								}`),
							},
							"bucket-test-bucket-us-west-2": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket",
									"metadata": {
										"name": "test-bucket-us-west-2",
										"labels": {"kro.run/collection-index": "1"}
									},
									"spec": {
										"forProvider": {
											"region": "us-west-2"
										}
									}
								}`),
							},
						},
					},
				},
			},
		},
		"CollectionObservedResourcesMatching": {
			reason: "Observed collection items with collection-index labels should be matched back to their collection node and used for further evaluations",
			args: args{
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test", Capabilities: []fnv1.Capability{fnv1.Capability_CAPABILITY_CAPABILITIES, fnv1.Capability_CAPABILITY_REQUIRED_SCHEMAS}},
					Input: resource.MustStructJSON(`{
						"apiVersion": "kro.fn.crossplane.io/v1alpha1",
						"kind": "ResourceGraph",
						"resources": [{
							"id": "bucket",
							"forEach": [{"region": "${schema.spec.regions}"}],
							"readyWhen": ["${each.status.atProvider.id != ''}"],
							"template": {
								"apiVersion": "s3.aws.upbound.io/v1beta1",
								"kind": "Bucket",
								"metadata": {
									"name": "${schema.metadata.name + '-' + region}"
								},
								"spec": {
									"forProvider": {
										"region": "${region}"
									}
								}
							}
						}]
					}`),
					Observed: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.crossplane.io/v1",
								"kind": "XBucket",
								"metadata": {"name": "test-bucket"},
								"spec": {
									"regions": ["us-east-1", "us-west-2"]
								}
							}`),
						},
						Resources: map[string]*fnv1.Resource{
							"bucket-test-bucket-us-east-1": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket",
									"metadata": {
										"name": "test-bucket-us-east-1",
										"labels": {"kro.run/collection-index": "0"}
									},
									"spec": {"forProvider": {"region": "us-east-1"}},
									"status": {"atProvider": {"id": "test-bucket-us-east-1"}}
								}`),
							},
							"bucket-test-bucket-us-west-2": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket",
									"metadata": {
										"name": "test-bucket-us-west-2",
										"labels": {"kro.run/collection-index": "1"}
									},
									"spec": {"forProvider": {"region": "us-west-2"}},
									"status": {"atProvider": {"id": "test-bucket-us-west-2"}}
								}`),
							},
						},
					},
					RequiredSchemas: map[string]*fnv1.Schema{
						"example.crossplane.io/v1, Kind=XBucket": schemaXBucket,
						"s3.aws.upbound.io/v1beta1, Kind=Bucket": schemaBucket,
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
							"bucket-test-bucket-us-east-1": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket",
									"metadata": {
										"name": "test-bucket-us-east-1",
										"labels": {"kro.run/collection-index": "0"}
									},
									"spec": {
										"forProvider": {
											"region": "us-east-1"
										}
									}
								}`),
								Ready: fnv1.Ready_READY_TRUE,
							},
							"bucket-test-bucket-us-west-2": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket",
									"metadata": {
										"name": "test-bucket-us-west-2",
										"labels": {"kro.run/collection-index": "1"}
									},
									"spec": {
										"forProvider": {
											"region": "us-west-2"
										}
									}
								}`),
								Ready: fnv1.Ready_READY_TRUE,
							},
						},
					},
				},
			},
		},
		"CollectionPartiallyObservedNotReady": {
			reason: "A collection with readyWhen should not be ready until every item it renders has been observed",
			args: args{
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test", Capabilities: []fnv1.Capability{fnv1.Capability_CAPABILITY_CAPABILITIES, fnv1.Capability_CAPABILITY_REQUIRED_SCHEMAS}},
					Input: resource.MustStructJSON(`{
						"apiVersion": "kro.fn.crossplane.io/v1alpha1",
						"kind": "ResourceGraph",
						"resources": [{
							"id": "bucket",
							"forEach": [{"region": "${schema.spec.regions}"}],
							"readyWhen": ["${each.status.atProvider.id != ''}"],
							"template": {
								"apiVersion": "s3.aws.upbound.io/v1beta1",
								"kind": "Bucket",
								"metadata": {
									"name": "${schema.metadata.name + '-' + region}"
								},
								"spec": {
									"forProvider": {
										"region": "${region}"
									}
								}
							}
						}]
					}`),
					Observed: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.crossplane.io/v1",
								"kind": "XBucket",
								"metadata": {"name": "test-bucket"},
								"spec": {
									"regions": ["us-east-1", "us-west-2"]
								}
							}`),
						},
						Resources: map[string]*fnv1.Resource{
							// us-east-1 exists and would be ready on its own, but us-west-2 doesn't exist yet
							"bucket-test-bucket-us-east-1": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket",
									"metadata": {
										"name": "test-bucket-us-east-1",
										"labels": {"kro.run/collection-index": "0"}
									},
									"spec": {"forProvider": {"region": "us-east-1"}},
									"status": {"atProvider": {"id": "test-bucket-us-east-1"}}
								}`),
							},
						},
					},
					RequiredSchemas: map[string]*fnv1.Schema{
						"example.crossplane.io/v1, Kind=XBucket": schemaXBucket,
						"s3.aws.upbound.io/v1beta1, Kind=Bucket": schemaBucket,
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
							"bucket-test-bucket-us-east-1": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket",
									"metadata": {
										"name": "test-bucket-us-east-1",
										"labels": {"kro.run/collection-index": "0"}
									},
									"spec": {
										"forProvider": {
											"region": "us-east-1"
										}
									}
								}`),
								Ready: fnv1.Ready_READY_FALSE,
							},
							"bucket-test-bucket-us-west-2": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket",
									"metadata": {
										"name": "test-bucket-us-west-2",
										"labels": {"kro.run/collection-index": "1"}
									},
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
		"MultiResourceDependencyChain": {
			reason: "Resource B (BucketLogging) should resolve CEL expressions that reference resource A (Bucket) outputs",
			args: args{
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test", Capabilities: []fnv1.Capability{fnv1.Capability_CAPABILITY_CAPABILITIES, fnv1.Capability_CAPABILITY_REQUIRED_SCHEMAS}},
					Input: resource.MustStructJSON(`{
						"apiVersion": "kro.fn.crossplane.io/v1alpha1",
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
						}, {
							"id": "logging",
							"template": {
								"apiVersion": "s3.aws.upbound.io/v1beta1",
								"kind": "BucketLogging",
								"metadata": {},
								"spec": {
									"forProvider": {
										"bucket": "${bucket.status.atProvider.id}",
										"targetPrefix": "logs/"
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
								"spec": {"bucketName": "my-bucket"}
							}`),
						},
						Resources: map[string]*fnv1.Resource{
							"bucket": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket",
									"metadata": {"name": "test-bucket-xyz"},
									"spec": {"forProvider": {"region": "us-west-2"}},
									"status": {"atProvider": {"id": "test-bucket-xyz"}}
								}`),
							},
						},
					},
					RequiredSchemas: map[string]*fnv1.Schema{
						"example.crossplane.io/v1, Kind=XBucket":        schemaXBucket,
						"s3.aws.upbound.io/v1beta1, Kind=Bucket":        schemaBucket,
						"s3.aws.upbound.io/v1beta1, Kind=BucketLogging": schemaBucketLogging,
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
							"s3.aws.upbound.io/v1beta1, Kind=BucketLogging": {
								ApiVersion: "s3.aws.upbound.io/v1beta1",
								Kind:       "BucketLogging",
							},
						},
					},
					Desired: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.crossplane.io/v1",
								"kind": "XBucket",
								"status": {
									"bucketName": "test-bucket-xyz"
								}
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
							},
							"logging": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "BucketLogging",
									"metadata": {},
									"spec": {
										"forProvider": {
											"bucket": "test-bucket-xyz",
											"targetPrefix": "logs/"
										}
									}
								}`),
							},
						},
					},
				},
			},
		},
		"DependentUsesRenderedFieldsInSamePass": {
			reason: "A resource should be composed in the same pass from fields its dependency's template sets, but wait for fields only the observed dependency has",
			args: args{
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test", Capabilities: []fnv1.Capability{fnv1.Capability_CAPABILITY_CAPABILITIES, fnv1.Capability_CAPABILITY_REQUIRED_SCHEMAS}},
					Input: resource.MustStructJSON(`{
						"apiVersion": "kro.fn.crossplane.io/v1alpha1",
						"kind": "ResourceGraph",
						"resources": [{
							"id": "bucket",
							"template": {
								"apiVersion": "s3.aws.upbound.io/v1beta1",
								"kind": "Bucket",
								"metadata": {
									"name": "${schema.spec.bucketName}"
								},
								"spec": {
									"forProvider": {
										"region": "us-west-2"
									}
								}
							}
						}, {
							"id": "logging",
							"template": {
								"apiVersion": "s3.aws.upbound.io/v1beta1",
								"kind": "BucketLogging",
								"metadata": {},
								"spec": {
									"forProvider": {
										"bucket": "${bucket.metadata.name}",
										"targetPrefix": "logs/"
									}
								}
							}
						}, {
							"id": "cm",
							"template": {
								"apiVersion": "v1",
								"kind": "ConfigMap",
								"metadata": {},
								"data": {
									"arn": "${bucket.status.atProvider.arn}"
								}
							}
						}]
					}`),
					Observed: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.crossplane.io/v1",
								"kind": "XBucket",
								"metadata": {"name": "test-bucket"},
								"spec": {"bucketName": "my-bucket"}
							}`),
						},
					},
					RequiredSchemas: map[string]*fnv1.Schema{
						"example.crossplane.io/v1, Kind=XBucket":        schemaXBucket,
						"s3.aws.upbound.io/v1beta1, Kind=Bucket":        schemaBucket,
						"s3.aws.upbound.io/v1beta1, Kind=BucketLogging": schemaBucketLogging,
						"/v1, Kind=ConfigMap":                           schemaConfigMap,
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
							"s3.aws.upbound.io/v1beta1, Kind=BucketLogging": {
								ApiVersion: "s3.aws.upbound.io/v1beta1",
								Kind:       "BucketLogging",
							},
							"/v1, Kind=ConfigMap": {
								ApiVersion: "v1",
								Kind:       "ConfigMap",
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
									"metadata": {
										"name": "my-bucket"
									},
									"spec": {
										"forProvider": {
											"region": "us-west-2"
										}
									}
								}`),
							},
							// logging reads the name bucket's template sets, before bucket exists
							"logging": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "BucketLogging",
									"metadata": {},
									"spec": {
										"forProvider": {
											"bucket": "my-bucket",
											"targetPrefix": "logs/"
										}
									}
								}`),
							},
							// cm isn't composed, because bucket.status only exists once bucket is observed
						},
					},
				},
			},
		},
		"ContextInComposedResource": {
			reason: "Values read from the function context should resolve into a composed resource, and integers from the context should support CEL integer math",
			args: args{
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test", Capabilities: []fnv1.Capability{fnv1.Capability_CAPABILITY_CAPABILITIES, fnv1.Capability_CAPABILITY_REQUIRED_SCHEMAS}},
					Context: resource.MustStructJSON(`{
						"apiextensions.crossplane.io/environment": {
							"region": "us-west-2"
						},
						"custom": {
							"replicas": 3
						}
					}`),
					// The "+ 1" in the template is deliberate. Context numbers
					// arrive as float64, and CEL can't add an int to one unless
					// decodeAsK8sAPI has run on the context first.
					Input: resource.MustStructJSON(`{
						"apiVersion": "kro.fn.crossplane.io/v1alpha1",
						"kind": "ResourceGraph",
						"resources": [{
							"id": "bucket",
							"template": {
								"apiVersion": "s3.aws.upbound.io/v1beta1",
								"kind": "Bucket",
								"metadata": {
								},
								"spec": {
									"forProvider": {
										"region": "${schema.context[\"apiextensions.crossplane.io/environment\"].region}",
										"replicas": "${schema.context.custom.replicas + 1}"
									}
								}
							}
						}]
					}`),
					Observed: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.crossplane.io/v1",
								"kind": "XBucket",
								"metadata": {"name": "test-bucket"},
								"spec": {"bucketName": "my-bucket"}
							}`),
						},
					},
					RequiredSchemas: map[string]*fnv1.Schema{
						"example.crossplane.io/v1, Kind=XBucket": schemaXBucket,
						"s3.aws.upbound.io/v1beta1, Kind=Bucket": schemaBucket,
					},
				},
			},
			want: want{
				rsp: &fnv1.RunFunctionResponse{
					Meta: &fnv1.ResponseMeta{Tag: "test", Ttl: durationpb.New(response.DefaultTTL)},
					Context: resource.MustStructJSON(`{
						"apiextensions.crossplane.io/environment": {
							"region": "us-west-2"
						},
						"custom": {
							"replicas": 3
						}
					}`),
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
									"metadata": {
									},
									"spec": {
										"forProvider": {
											"region": "us-west-2",
											"replicas": 4
										}
									}
								}`),
							},
						},
					},
				},
			},
		},
		"NullContextKeyNotComposed": {
			reason: "A resource whose template reads through a context key that is null should not be composed",
			args: args{
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test", Capabilities: []fnv1.Capability{fnv1.Capability_CAPABILITY_CAPABILITIES, fnv1.Capability_CAPABILITY_REQUIRED_SCHEMAS}},
					Context: resource.MustStructJSON(`{
						"apiextensions.crossplane.io/environment": null
					}`),
					Input: resource.MustStructJSON(`{
						"apiVersion": "kro.fn.crossplane.io/v1alpha1",
						"kind": "ResourceGraph",
						"resources": [{
							"id": "bucket",
							"template": {
								"apiVersion": "s3.aws.upbound.io/v1beta1",
								"kind": "Bucket",
								"metadata": {},
								"spec": {
									"forProvider": {
										"region": "${schema.context[\"apiextensions.crossplane.io/environment\"].region}"
									}
								}
							}
						}]
					}`),
					Observed: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.crossplane.io/v1",
								"kind": "XBucket",
								"metadata": {"name": "test-bucket"},
								"spec": {"bucketName": "my-bucket", "replicas": 3}
							}`),
						},
					},
					RequiredSchemas: map[string]*fnv1.Schema{
						"example.crossplane.io/v1, Kind=XBucket": schemaXBucket,
						"s3.aws.upbound.io/v1beta1, Kind=Bucket": schemaBucket,
					},
				},
			},
			want: want{
				rsp: &fnv1.RunFunctionResponse{
					Meta: &fnv1.ResponseMeta{Tag: "test", Ttl: durationpb.New(response.DefaultTTL)},
					Context: resource.MustStructJSON(`{
						"apiextensions.crossplane.io/environment": null
					}`),
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
					},
				},
			},
		},
		"NestedNullContextValueComposed": {
			reason: "A null value nested under a context key should render as null in the composed resource rather than holding the resource back",
			args: args{
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test", Capabilities: []fnv1.Capability{fnv1.Capability_CAPABILITY_CAPABILITIES, fnv1.Capability_CAPABILITY_REQUIRED_SCHEMAS}},
					Context: resource.MustStructJSON(`{
						"apiextensions.crossplane.io/environment": {
							"region": null
						}
					}`),
					Input: resource.MustStructJSON(`{
						"apiVersion": "kro.fn.crossplane.io/v1alpha1",
						"kind": "ResourceGraph",
						"resources": [{
							"id": "bucket",
							"template": {
								"apiVersion": "s3.aws.upbound.io/v1beta1",
								"kind": "Bucket",
								"metadata": {},
								"spec": {
									"forProvider": {
										"region": "${schema.context[\"apiextensions.crossplane.io/environment\"].region}"
									}
								}
							}
						}]
					}`),
					Observed: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.crossplane.io/v1",
								"kind": "XBucket",
								"metadata": {"name": "test-bucket"},
								"spec": {"bucketName": "my-bucket", "replicas": 3}
							}`),
						},
					},
					RequiredSchemas: map[string]*fnv1.Schema{
						"example.crossplane.io/v1, Kind=XBucket": schemaXBucket,
						"s3.aws.upbound.io/v1beta1, Kind=Bucket": schemaBucket,
					},
				},
			},
			want: want{
				rsp: &fnv1.RunFunctionResponse{
					Meta: &fnv1.ResponseMeta{Tag: "test", Ttl: durationpb.New(response.DefaultTTL)},
					Context: resource.MustStructJSON(`{
						"apiextensions.crossplane.io/environment": {
							"region": null
						}
					}`),
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
											"region": null
										}
									}
								}`),
							},
						},
					},
				},
			},
		},
		"ExistingResourceNotRenderable": {
			reason: "A resource that already exists but can no longer be rendered should return a fatal result, rather than being left out of desired state and deleted by Crossplane",
			args: args{
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test", Capabilities: []fnv1.Capability{fnv1.Capability_CAPABILITY_CAPABILITIES, fnv1.Capability_CAPABILITY_REQUIRED_SCHEMAS}},
					Input: resource.MustStructJSON(`{
						"apiVersion": "kro.fn.crossplane.io/v1alpha1",
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
						}, {
							"id": "cm",
							"template": {
								"apiVersion": "v1",
								"kind": "ConfigMap",
								"metadata": {},
								"data": {
									"arn": "${bucket.status.atProvider.arn}"
								}
							}
						}]
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
							// bucket no longer has the arn that cm needs, but cm already exists
							"bucket": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "s3.aws.upbound.io/v1beta1",
									"kind": "Bucket",
									"metadata": {"name": "test-bucket-abc123"},
									"spec": {"forProvider": {"region": "us-west-2"}}
								}`),
							},
							"cm": {
								Resource: resource.MustStructJSON(`{
									"apiVersion": "v1",
									"kind": "ConfigMap",
									"metadata": {"name": "test-bucket-cm-xyz"},
									"data": {"arn": "arn:aws:s3:::test-bucket-abc123"}
								}`),
							},
						},
					},
					RequiredSchemas: map[string]*fnv1.Schema{
						"example.crossplane.io/v1, Kind=XBucket": schemaXBucket,
						"s3.aws.upbound.io/v1beta1, Kind=Bucket": schemaBucket,
						"/v1, Kind=ConfigMap":                    schemaConfigMap,
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
							"/v1, Kind=ConfigMap": {
								ApiVersion: "v1",
								Kind:       "ConfigMap",
							},
						},
					},
					Results: []*fnv1.Result{{
						Severity: fnv1.Severity_SEVERITY_FATAL,
						Message:  `resource "cm" already exists but cannot be rendered, refusing to delete it: node "cm": eval "bucket.status.atProvider.arn" at "data.arn": eval "bucket.status.atProvider.arn": no such key: status (data pending)`,
						Target:   fnv1.Target_TARGET_COMPOSITE.Enum(),
					}},
				},
			},
		},
		"CELInKindUnsupported": {
			reason: "A template whose kind is a CEL expression should return a fatal result, since resolving it needs the API server's REST mapper",
			args: args{
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test", Capabilities: []fnv1.Capability{fnv1.Capability_CAPABILITY_CAPABILITIES, fnv1.Capability_CAPABILITY_REQUIRED_SCHEMAS}},
					Input: resource.MustStructJSON(`{
						"apiVersion": "kro.fn.crossplane.io/v1alpha1",
						"kind": "ResourceGraph",
						"resources": [{
							"id": "cm",
							"template": {
								"apiVersion": "v1",
								"kind": "${'Config' + 'Map'}",
								"metadata": {},
								"data": {
									"key": "value"
								}
							}
						}]
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
					RequiredSchemas: map[string]*fnv1.Schema{
						"example.crossplane.io/v1, Kind=XBucket": schemaXBucket,
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
							// Schemas are requested before the graph is compiled, so
							// the unrendered kind is asked for too
							"/v1, Kind=${'Config' + 'Map'}": {
								ApiVersion: "v1",
								Kind:       "${'Config' + 'Map'}",
							},
						},
					},
					Results: []*fnv1.Result{{
						Severity: fnv1.Severity_SEVERITY_FATAL,
						Message:  `resource "cm": CEL expressions in apiVersion or kind are not supported`,
						Target:   fnv1.Target_TARGET_COMPOSITE.Enum(),
					}},
				},
			},
		},
		"CELInExternalRefKindUnsupported": {
			reason: "An external ref whose kind is a CEL expression should return a fatal result, since resolving it needs the API server's REST mapper",
			args: args{
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test", Capabilities: []fnv1.Capability{fnv1.Capability_CAPABILITY_CAPABILITIES, fnv1.Capability_CAPABILITY_REQUIRED_SCHEMAS}},
					Input: resource.MustStructJSON(`{
						"apiVersion": "kro.fn.crossplane.io/v1alpha1",
						"kind": "ResourceGraph",
						"resources": [{
							"id": "config",
							"externalRef": {
								"apiVersion": "v1",
								"kind": "${'Config' + 'Map'}",
								"metadata": {
									"name": "my-platform-config"
								}
							}
						}]
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
					RequiredSchemas: map[string]*fnv1.Schema{
						"example.crossplane.io/v1, Kind=XBucket": schemaXBucket,
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
							// Schemas are requested before the graph is compiled, so
							// the unrendered kind is asked for too
							"/v1, Kind=${'Config' + 'Map'}": {
								ApiVersion: "v1",
								Kind:       "${'Config' + 'Map'}",
							},
						},
					},
					Results: []*fnv1.Result{{
						Severity: fnv1.Severity_SEVERITY_FATAL,
						Message:  `resource "config": CEL expressions in apiVersion or kind are not supported`,
						Target:   fnv1.Target_TARGET_COMPOSITE.Enum(),
					}},
				},
			},
		},
		"TypeMismatch": {
			reason: "A CEL expression whose type doesn't match the field it writes should return a fatal result",
			args: args{
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test", Capabilities: []fnv1.Capability{fnv1.Capability_CAPABILITY_CAPABILITIES, fnv1.Capability_CAPABILITY_REQUIRED_SCHEMAS}},
					Input: resource.MustStructJSON(`{
						"apiVersion": "kro.fn.crossplane.io/v1alpha1",
						"kind": "ResourceGraph",
						"resources": [{
							"id": "bucket",
							"template": {
								"apiVersion": "s3.aws.upbound.io/v1beta1",
								"kind": "Bucket",
								"metadata": {},
								"spec": {
									"forProvider": {
										"replicas": "${schema.spec.bucketName}"
									}
								}
							}
						}]
					}`),
					Observed: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.crossplane.io/v1",
								"kind": "XBucket",
								"metadata": {"name": "test-bucket"},
								"spec": {"bucketName": "my-bucket"}
							}`),
						},
					},
					RequiredSchemas: map[string]*fnv1.Schema{
						"example.crossplane.io/v1, Kind=XBucket": schemaXBucket,
						"s3.aws.upbound.io/v1beta1, Kind=Bucket": schemaBucket,
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
					Results: []*fnv1.Result{{
						Severity: fnv1.Severity_SEVERITY_FATAL,
						Message:  `cannot create graph runtime: rgdadapter: compile: compile node "bucket": type mismatch in node "bucket" at path "spec.forProvider.replicas": expression "schema.spec.bucketName" returns "string" but expected "int": type kind mismatch: got "string", expected "int"`,
						Target:   fnv1.Target_TARGET_COMPOSITE.Enum(),
					}},
				},
			},
		},
		"DuplicateResourceID": {
			reason: "Two resources with the same ID should return a fatal result",
			args: args{
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test", Capabilities: []fnv1.Capability{fnv1.Capability_CAPABILITY_CAPABILITIES, fnv1.Capability_CAPABILITY_REQUIRED_SCHEMAS}},
					Input: resource.MustStructJSON(`{
						"apiVersion": "kro.fn.crossplane.io/v1alpha1",
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
						}, {
							"id": "bucket",
							"template": {
								"apiVersion": "s3.aws.upbound.io/v1beta1",
								"kind": "Bucket",
								"metadata": {},
								"spec": {
									"forProvider": {
										"region": "us-east-1"
									}
								}
							}
						}]
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
					RequiredSchemas: map[string]*fnv1.Schema{
						"example.crossplane.io/v1, Kind=XBucket": schemaXBucket,
						"s3.aws.upbound.io/v1beta1, Kind=Bucket": schemaBucket,
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
					Results: []*fnv1.Result{{
						Severity: fnv1.Severity_SEVERITY_FATAL,
						Message:  `cannot create graph runtime: rgdadapter: compile: invalid graph: duplicate node id "bucket"`,
						Target:   fnv1.Target_TARGET_COMPOSITE.Enum(),
					}},
				},
			},
		},
		"TooManyForEachDimensions": {
			reason: "A forEach with more dimensions than --rgd-max-collection-dim-size allows should return a fatal result",
			args: args{
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test", Capabilities: []fnv1.Capability{fnv1.Capability_CAPABILITY_CAPABILITIES, fnv1.Capability_CAPABILITY_REQUIRED_SCHEMAS}},
					Input: resource.MustStructJSON(`{
						"apiVersion": "kro.fn.crossplane.io/v1alpha1",
						"kind": "ResourceGraph",
						"resources": [{
							"id": "buckets",
							"forEach": [
								{"a": "${schema.spec.regions}"}, {"b": "${schema.spec.regions}"}, {"c": "${schema.spec.regions}"},
								{"d": "${schema.spec.regions}"}, {"e": "${schema.spec.regions}"}, {"f": "${schema.spec.regions}"},
								{"g": "${schema.spec.regions}"}, {"h": "${schema.spec.regions}"}, {"i": "${schema.spec.regions}"},
								{"j": "${schema.spec.regions}"}, {"k": "${schema.spec.regions}"}
							],
							"template": {
								"apiVersion": "s3.aws.upbound.io/v1beta1",
								"kind": "Bucket",
								"metadata": {},
								"spec": {
									"forProvider": {
										"region": "${a}"
									}
								}
							}
						}]
					}`),
					Observed: &fnv1.State{
						Composite: &fnv1.Resource{
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.crossplane.io/v1",
								"kind": "XBucket",
								"metadata": {"name": "test-bucket"},
								"spec": {"regions": ["us-west-2"]}
							}`),
						},
					},
					RequiredSchemas: map[string]*fnv1.Schema{
						"example.crossplane.io/v1, Kind=XBucket": schemaXBucket,
						"s3.aws.upbound.io/v1beta1, Kind=Bucket": schemaBucket,
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
					Results: []*fnv1.Result{{
						Severity: fnv1.Severity_SEVERITY_FATAL,
						Message:  `resource "buckets": forEach cannot have more than 10 dimensions, got 11`,
						Target:   fnv1.Target_TARGET_COMPOSITE.Enum(),
					}},
				},
			},
		},
		"StatusConditionsSetOnXR": {
			reason: "Custom status conditions should be set on the XR, and runtime.condition(schema, ...) should read our approximation of KRO's built-in conditions",
			args: args{
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test", Capabilities: []fnv1.Capability{fnv1.Capability_CAPABILITY_CAPABILITIES, fnv1.Capability_CAPABILITY_REQUIRED_SCHEMAS}},
					Input: resource.MustStructJSON(`{
						"apiVersion": "kro.fn.crossplane.io/v1alpha1",
						"kind": "ResourceGraph",
						"resources": [{
							"id": "bucket",
							"readyWhen": ["${bucket.status.?atProvider.?id.hasValue()}"],
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
							"bucketName": "${bucket.status.atProvider.id}",
							"conditions": [
								"${runtime.newCondition({type: 'BucketCreated', status: 'True', reason: 'HasID', message: 'bucket ' + bucket.status.atProvider.id + ' exists'})}",
								"${runtime.newCondition({type: 'AllResourcesReady', status: runtime.condition(schema, 'ResourcesReady').status, reason: 'Checked', message: ''})}"
							]
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
									"spec": {"forProvider": {"region": "us-west-2"}},
									"status": {"atProvider": {"id": "test-bucket-abc123"}}
								}`),
							},
						},
					},
					RequiredSchemas: map[string]*fnv1.Schema{
						"example.crossplane.io/v1, Kind=XBucket": schemaXBucket,
						"s3.aws.upbound.io/v1beta1, Kind=Bucket": schemaBucket,
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
							// Conditions are returned separately, never in the desired XR's status
							Resource: resource.MustStructJSON(`{
								"apiVersion": "example.crossplane.io/v1",
								"kind": "XBucket",
								"status": {
									"bucketName": "test-bucket-abc123"
								}
							}`),
						},
						Resources: map[string]*fnv1.Resource{
							"bucket": {
								Ready: fnv1.Ready_READY_TRUE,
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
							},
						},
					},
					Conditions: []*fnv1.Condition{
						{
							Type:    "BucketCreated",
							Status:  fnv1.Status_STATUS_CONDITION_TRUE,
							Reason:  "HasID",
							Message: new("bucket test-bucket-abc123 exists"),
							Target:  fnv1.Target_TARGET_COMPOSITE.Enum(),
						},
						{
							// Every resource is ready this run
							Type:   "AllResourcesReady",
							Status: fnv1.Status_STATUS_CONDITION_TRUE,
							Reason: "Checked",
							Target: fnv1.Target_TARGET_COMPOSITE.Enum(),
						},
					},
				},
			},
		},
		"StatusConditionWaitingForDataLeftOut": {
			reason: "A custom status condition whose data isn't available yet should be left out, so Crossplane keeps its last value on the XR",
			args: args{
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test", Capabilities: []fnv1.Capability{fnv1.Capability_CAPABILITY_CAPABILITIES, fnv1.Capability_CAPABILITY_REQUIRED_SCHEMAS}},
					Input: resource.MustStructJSON(`{
						"apiVersion": "kro.fn.crossplane.io/v1alpha1",
						"kind": "ResourceGraph",
						"resources": [{
							"id": "bucket",
							"readyWhen": ["${bucket.status.?atProvider.?id.hasValue()}"],
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
							"conditions": [
								"${runtime.newCondition({type: 'BucketCreated', status: 'True', reason: 'HasID', message: 'bucket ' + bucket.status.atProvider.id + ' exists'})}",
								"${runtime.newCondition({type: 'AllResourcesReady', status: runtime.condition(schema, 'ResourcesReady').status, reason: 'Checked', message: ''})}"
							]
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
					RequiredSchemas: map[string]*fnv1.Schema{
						"example.crossplane.io/v1, Kind=XBucket": schemaXBucket,
						"s3.aws.upbound.io/v1beta1, Kind=Bucket": schemaBucket,
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
								Ready: fnv1.Ready_READY_FALSE,
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
							},
						},
					},
					// BucketCreated is left out, because bucket.status doesn't exist yet
					Conditions: []*fnv1.Condition{
						{
							// bucket isn't ready yet
							Type:   "AllResourcesReady",
							Status: fnv1.Status_STATUS_CONDITION_FALSE,
							Reason: "Checked",
							Target: fnv1.Target_TARGET_COMPOSITE.Enum(),
						},
					},
				},
			},
		},
		"StatusConditionWarnings": {
			reason: "A custom status condition that fails to evaluate (here, one with no type), or uses a condition type Crossplane reserves, should be left out with a warning, while the rest are still set",
			args: args{
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test", Capabilities: []fnv1.Capability{fnv1.Capability_CAPABILITY_CAPABILITIES, fnv1.Capability_CAPABILITY_REQUIRED_SCHEMAS}},
					Input: resource.MustStructJSON(`{
						"apiVersion": "kro.fn.crossplane.io/v1alpha1",
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
							"conditions": [
								"${runtime.newCondition({status: 'True', reason: 'Checked', message: ''})}",
								"${runtime.newCondition({type: 'Ready', status: 'True', reason: 'Checked', message: ''})}",
								"${runtime.newCondition({type: 'Configured', status: 'True', reason: 'Checked', message: ''})}"
							]
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
					RequiredSchemas: map[string]*fnv1.Schema{
						"example.crossplane.io/v1, Kind=XBucket": schemaXBucket,
						"s3.aws.upbound.io/v1beta1, Kind=Bucket": schemaBucket,
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
							},
						},
					},
					Results: []*fnv1.Result{
						{
							Severity: fnv1.Severity_SEVERITY_WARNING,
							Message:  "cannot evaluate status conditions: author condition evaluation degraded: \"${runtime.newCondition({status: 'True', reason: 'Checked', message: ''})}\": parse \"runtime.newCondition({status: 'True', reason: 'Checked', message: ''})\": ERROR: <input>:1:22: runtime.newCondition: 'type' is required\n | runtime.newCondition({status: 'True', reason: 'Checked', message: ''})\n | .....................^",
							Target:   fnv1.Target_TARGET_COMPOSITE.Enum(),
						},
						{
							Severity: fnv1.Severity_SEVERITY_WARNING,
							Message:  `cannot set condition "Ready": Crossplane reserves this condition type`,
							Target:   fnv1.Target_TARGET_COMPOSITE.Enum(),
						},
					},
					Conditions: []*fnv1.Condition{
						{
							Type:   "Configured",
							Status: fnv1.Status_STATUS_CONDITION_TRUE,
							Reason: "Checked",
							Target: fnv1.Target_TARGET_COMPOSITE.Enum(),
						},
					},
				},
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := NewFunction(logging.NewNopLogger(), graph.Config{
				MaxCollectionSize:          1000,
				MaxCollectionDimensionSize: 10,
			})
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

func TestRunFunctionCollectionSizeLimitExceeded(t *testing.T) {
	f := NewFunction(logging.NewNopLogger(), graph.Config{
		MaxCollectionSize:          2,
		MaxCollectionDimensionSize: 10,
	})

	rsp, err := f.RunFunction(context.Background(), &fnv1.RunFunctionRequest{
		Meta: &fnv1.RequestMeta{Tag: "test", Capabilities: []fnv1.Capability{fnv1.Capability_CAPABILITY_CAPABILITIES, fnv1.Capability_CAPABILITY_REQUIRED_SCHEMAS}},
		Input: resource.MustStructJSON(`{
			"apiVersion": "kro.fn.crossplane.io/v1alpha1",
			"kind": "ResourceGraph",
			"resources": [{
				"id": "bucket",
				"forEach": [{"region": "${schema.spec.regions}"}],
				"template": {
					"apiVersion": "s3.aws.upbound.io/v1beta1",
					"kind": "Bucket",
					"metadata": {
						"name": "${schema.metadata.name + '-' + region}"
					},
					"spec": {
						"forProvider": {
							"region": "${region}"
						}
					}
				}
			}]
		}`),
		Observed: &fnv1.State{
			Composite: &fnv1.Resource{
				Resource: resource.MustStructJSON(`{
					"apiVersion": "example.crossplane.io/v1",
					"kind": "XBucket",
					"metadata": {"name": "test-bucket"},
					"spec": {
						"regions": ["us-east-1", "us-west-2", "eu-west-1"]
					}
				}`),
			},
		},
		RequiredSchemas: map[string]*fnv1.Schema{
			"example.crossplane.io/v1, Kind=XBucket": schemaXBucket,
			"s3.aws.upbound.io/v1beta1, Kind=Bucket": schemaBucket,
		},
	})

	// The response should contain a fatal result because the collection
	// expansion (3 items) exceeds the configured MaxCollectionSize (2).
	want := &fnv1.RunFunctionResponse{
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
		Results: []*fnv1.Result{{
			Severity: fnv1.Severity_SEVERITY_FATAL,
			Target:   fnv1.Target_TARGET_COMPOSITE.Enum(),
			Message:  `apply "bucket": resolve: node "bucket": collection size 3 exceeds the maximum of 2`,
		}},
	}

	if diff := cmp.Diff(want, rsp, protocmp.Transform()); diff != "" {
		t.Errorf("Collection exceeding MaxCollectionSize should return a fatal response\nf.RunFunction(...): -want rsp, +got rsp:\n%s", diff)
	}

	if diff := cmp.Diff(nil, err, cmpopts.EquateErrors()); diff != "" {
		t.Errorf("Collection exceeding MaxCollectionSize should not return a Go error\nf.RunFunction(...): -want err, +got err:\n%s", diff)
	}
}

func TestRunFunctionOmitRemovesField(t *testing.T) {
	// Enable the CELOmitFunction feature gate for this test.
	if err := features.FeatureGate.Set("CELOmitFunction=true"); err != nil {
		t.Fatalf("cannot enable CELOmitFunction feature gate: %v", err)
	}
	t.Cleanup(func() {
		_ = features.FeatureGate.Set("CELOmitFunction=false")
	})

	f := NewFunction(logging.NewNopLogger(), graph.Config{
		MaxCollectionSize:          1000,
		MaxCollectionDimensionSize: 10,
	})

	rsp, err := f.RunFunction(context.Background(), &fnv1.RunFunctionRequest{
		Meta: &fnv1.RequestMeta{Tag: "test", Capabilities: []fnv1.Capability{fnv1.Capability_CAPABILITY_CAPABILITIES, fnv1.Capability_CAPABILITY_REQUIRED_SCHEMAS}},
		Input: resource.MustStructJSON(`{
			"apiVersion": "kro.fn.crossplane.io/v1alpha1",
			"kind": "ResourceGraph",
			"resources": [{
				"id": "bucket",
				"template": {
					"apiVersion": "s3.aws.upbound.io/v1beta1",
					"kind": "Bucket",
					"metadata": {},
					"spec": {
						"forProvider": {
							"region": "us-west-2",
							"objectLockEnabled": "${schema.spec.enableLogging ? true : omit()}"
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
					"spec": {
						"enableLogging": false
					}
				}`),
			},
		},
		RequiredSchemas: map[string]*fnv1.Schema{
			"example.crossplane.io/v1, Kind=XBucket": schemaXBucket,
			"s3.aws.upbound.io/v1beta1, Kind=Bucket": schemaBucket,
		},
	})

	want := &fnv1.RunFunctionResponse{
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
				// No status — bucket isn't observed yet so CEL expressions can't resolve
				Resource: resource.MustStructJSON(`{
					"apiVersion": "example.crossplane.io/v1",
					"kind": "XBucket"
				}`),
			},
			Resources: map[string]*fnv1.Resource{
				"bucket": {
					// objectLockEnabled should be absent because omit() removed it
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
				},
			},
		},
	}

	if diff := cmp.Diff(want, rsp, protocmp.Transform()); diff != "" {
		t.Errorf("omit() should remove the field from desired output\nf.RunFunction(...): -want rsp, +got rsp:\n%s", diff)
	}

	if diff := cmp.Diff(nil, err, cmpopts.EquateErrors()); diff != "" {
		t.Errorf("omit() should not return a Go error\nf.RunFunction(...): -want err, +got err:\n%s", diff)
	}
}

func TestDecodeAsK8sAPI(t *testing.T) {
	cases := map[string]struct {
		reason string
		in     []*unstructured.Unstructured
		want   []*unstructured.Unstructured
	}{
		"WholeNumberFloatBecomesInt64": {
			reason: "A whole-number float64 (how structpb delivers integers) is converted to int64.",
			in:     []*unstructured.Unstructured{{Object: map[string]any{"replicas": float64(3)}}},
			want:   []*unstructured.Unstructured{{Object: map[string]any{"replicas": int64(3)}}},
		},
		"FractionalFloatStaysFloat64": {
			reason: "A genuinely fractional float64 is preserved as float64.",
			in:     []*unstructured.Unstructured{{Object: map[string]any{"ratio": float64(2.5)}}},
			want:   []*unstructured.Unstructured{{Object: map[string]any{"ratio": float64(2.5)}}},
		},
		"NestedMapsAndSlices": {
			reason: "Whole-number floats are converted at any depth, inside both maps and slices.",
			in: []*unstructured.Unstructured{{Object: map[string]any{
				"spec": map[string]any{
					"replicas": float64(2),
					"ports":    []any{float64(80), float64(443)},
				},
			}}},
			want: []*unstructured.Unstructured{{Object: map[string]any{
				"spec": map[string]any{
					"replicas": int64(2),
					"ports":    []any{int64(80), int64(443)},
				},
			}}},
		},
		"NonNumbersUntouched": {
			reason: "Strings, bools, and nulls pass through unchanged.",
			in:     []*unstructured.Unstructured{{Object: map[string]any{"name": "app", "enabled": true, "note": nil}}},
			want:   []*unstructured.Unstructured{{Object: map[string]any{"name": "app", "enabled": true, "note": nil}}},
		},
		"NilPointerSkipped": {
			reason: "A nil *Unstructured is skipped rather than panicking.",
			in:     []*unstructured.Unstructured{nil},
			want:   []*unstructured.Unstructured{nil},
		},
		"EmptyObjectSkipped": {
			reason: "An Unstructured with no content is skipped rather than panicking.",
			in:     []*unstructured.Unstructured{{}},
			want:   []*unstructured.Unstructured{{}},
		},
		"NoObjects": {
			reason: "No arguments at all is a no-op.",
			in:     nil,
			want:   nil,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// decodeAsK8sAPI mutates its arguments in place, so we diff the
			// (now-decoded) input slice against want.
			if err := decodeAsK8sAPI(tc.in...); err != nil {
				t.Fatalf("%s\ndecodeAsK8sAPI(...): unexpected error: %v", tc.reason, err)
			}
			if diff := cmp.Diff(tc.want, tc.in); diff != "" {
				t.Errorf("%s\ndecodeAsK8sAPI(...): -want, +got:\n%s", tc.reason, diff)
			}
		})
	}
}

func TestRequireSchemas(t *testing.T) {
	cases := map[string]struct {
		reason string
		req    *fnv1.RunFunctionRequest
		gvks   []schema.GroupVersionKind
		want   *fnv1.Requirements
	}{
		"WithRequiredSchemasCapability": {
			reason: "When Crossplane advertises required_schemas, request schemas directly by GVK instead of guessing a CRD name.",
			req: &fnv1.RunFunctionRequest{
				Meta: &fnv1.RequestMeta{Capabilities: []fnv1.Capability{fnv1.Capability_CAPABILITY_REQUIRED_SCHEMAS}},
			},
			gvks: []schema.GroupVersionKind{{Group: "eks.aws.m.upbound.io", Version: "v1beta1", Kind: "AccessEntry"}},
			want: &fnv1.Requirements{
				Schemas: map[string]*fnv1.SchemaSelector{
					"eks.aws.m.upbound.io/v1beta1, Kind=AccessEntry": {
						ApiVersion: "eks.aws.m.upbound.io/v1beta1",
						Kind:       "AccessEntry",
					},
				},
			},
		},
		"CRDFallbackUsesCorrectEnglishPlural": {
			reason: "Without the required_schemas capability, the CRD requested via required_resources must be named with the real English plural (accessentries), not a naive Kind+\"s\" guess (accessentrys) that no CRD in the cluster actually has.",
			req: &fnv1.RunFunctionRequest{
				Meta: &fnv1.RequestMeta{Capabilities: []fnv1.Capability{}}, // an older Crossplane version won't advertise capabilities at all
			},
			gvks: []schema.GroupVersionKind{{Group: "eks.aws.m.upbound.io", Version: "v1beta1", Kind: "AccessEntry"}},
			want: &fnv1.Requirements{
				Resources: map[string]*fnv1.ResourceSelector{
					"eks.aws.m.upbound.io/v1beta1, Kind=AccessEntry": {
						ApiVersion: "apiextensions.k8s.io/v1",
						Kind:       "CustomResourceDefinition",
						Match:      &fnv1.ResourceSelector_MatchName{MatchName: "accessentries.eks.aws.m.upbound.io"},
					},
				},
			},
		},
		"CRDFallbackRegularPlural": {
			reason: "Kinds with a regular plural still resolve correctly through the fallback path.",
			req: &fnv1.RunFunctionRequest{
				Meta: &fnv1.RequestMeta{Capabilities: []fnv1.Capability{}},
			},
			gvks: []schema.GroupVersionKind{{Group: "s3.aws.upbound.io", Version: "v1beta1", Kind: "Bucket"}},
			want: &fnv1.Requirements{
				Resources: map[string]*fnv1.ResourceSelector{
					"s3.aws.upbound.io/v1beta1, Kind=Bucket": {
						ApiVersion: "apiextensions.k8s.io/v1",
						Kind:       "CustomResourceDefinition",
						Match:      &fnv1.ResourceSelector_MatchName{MatchName: "buckets.s3.aws.upbound.io"},
					},
				},
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rsp := &fnv1.RunFunctionResponse{Requirements: &fnv1.Requirements{}}
			requireSchemas(tc.req, rsp, tc.gvks)
			if diff := cmp.Diff(tc.want, rsp.GetRequirements(), protocmp.Transform()); diff != "" {
				t.Errorf("%s\nrequireSchemas(...): -want, +got:\n%s", tc.reason, diff)
			}
		})
	}
}
