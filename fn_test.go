package main

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"google.golang.org/protobuf/testing/protocmp"
	"google.golang.org/protobuf/types/known/durationpb"
	"k8s.io/utils/ptr"

	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"

	fnv1 "github.com/crossplane/function-sdk-go/proto/v1"
	"github.com/crossplane/function-sdk-go/resource"
	"github.com/crossplane/function-sdk-go/response"

	"github.com/crossplane-contrib/function-kro/kro/graph"
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
		"spec": {"type": "object", "properties": {"bucketName": {"type": "string"}, "configMapName": {"type": "string"}, "enableLogging": {"type": "boolean"}, "regions": {"type": "array", "items": {"type": "string"}}}},
		"status": {"type": "object", "properties": {"bucketName": {"type": "string"}, "bucketARN": {"type": "string"}, "region": {"type": "string"}}}`)

	schemaBucket = buildSchema(`
		"spec": {"type": "object", "properties": {"forProvider": {"type": "object", "properties": {"region": {"type": "string"}, "objectLockEnabled": {"type": "boolean"}}}, "managementPolicies": {"type": "array", "items": {"type": "string"}}}},
		"status": {"type": "object", "properties": {"atProvider": {"type": "object", "properties": {"arn": {"type": "string"}, "id": {"type": "string"}}}}}`)

	schemaBucketLogging = buildSchema(`
		"spec": {"type": "object", "properties": {"forProvider": {"type": "object", "properties": {"bucket": {"type": "string"}, "targetPrefix": {"type": "string"}}}}}`)

	schemaConfigMap = buildSchema(`
		"data": {"type": "object", "additionalProperties": {"type": "string"}}`)
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
		"DesiredXROnlyContainsDeclaredStatus": {
			reason: "The desired XR should contain resolved status expressions but not observed XR metadata (uid, resourceVersion) or existing status (conditions)",
			args: args{
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test", Capabilities: []fnv1.Capability{fnv1.Capability_CAPABILITY_CAPABILITIES, fnv1.Capability_CAPABILITY_REQUIRED_SCHEMAS}},
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
		"DesiredComposedResourceExcludesObservedFields": {
			reason: "Desired composed resources should only contain template fields, not fields from observed state like provider defaults",
			args: args{
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test", Capabilities: []fnv1.Capability{fnv1.Capability_CAPABILITY_CAPABILITIES, fnv1.Capability_CAPABILITY_REQUIRED_SCHEMAS}},
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
						"apiVersion": "kro.fn.crossplane.io/v1beta1",
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
								Namespace:  ptr.To("xr-ns"),
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
						"apiVersion": "kro.fn.crossplane.io/v1beta1",
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
								Namespace:  ptr.To("xr-ns"),
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
		"IncludeWhenExcludesResource": {
			reason: "Resources with includeWhen conditions that evaluate to false should be excluded from desired output",
			args: args{
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test", Capabilities: []fnv1.Capability{fnv1.Capability_CAPABILITY_CAPABILITIES, fnv1.Capability_CAPABILITY_REQUIRED_SCHEMAS}},
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
		"ReadyWhenTrue": {
			reason: "When readyWhen is defined and the expression evaluates to true, the resource should be marked ReadyTrue",
			args: args{
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test", Capabilities: []fnv1.Capability{fnv1.Capability_CAPABILITY_CAPABILITIES, fnv1.Capability_CAPABILITY_REQUIRED_SCHEMAS}},
					Input: resource.MustStructJSON(`{
						"apiVersion": "kro.fn.crossplane.io/v1beta1",
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
						"apiVersion": "kro.fn.crossplane.io/v1beta1",
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
		"CollectionForEach": {
			reason: "A forEach resource should expand into N composed resources named {id}-{index} with collection-index labels",
			args: args{
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test", Capabilities: []fnv1.Capability{fnv1.Capability_CAPABILITY_CAPABILITIES, fnv1.Capability_CAPABILITY_REQUIRED_SCHEMAS}},
					Input: resource.MustStructJSON(`{
						"apiVersion": "kro.fn.crossplane.io/v1beta1",
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
						"apiVersion": "kro.fn.crossplane.io/v1beta1",
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
		"MultiResourceDependencyChain": {
			reason: "Resource B (BucketLogging) should resolve CEL expressions that reference resource A (Bucket) outputs",
			args: args{
				req: &fnv1.RunFunctionRequest{
					Meta: &fnv1.RequestMeta{Tag: "test", Capabilities: []fnv1.Capability{fnv1.Capability_CAPABILITY_CAPABILITIES, fnv1.Capability_CAPABILITY_REQUIRED_SCHEMAS}},
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
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := &Function{
				log: logging.NewNopLogger(),
				rgdConfig: graph.RGDConfig{
					MaxCollectionSize:          1000,
					MaxCollectionDimensionSize: 10,
				},
			}
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
