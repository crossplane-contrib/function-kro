# function-kro

A [Crossplane Composition Function][functions] that brings the [KRO][kro]
(Kubernetes Resource Orchestrator) experience to Crossplane. Define complex,
interdependent Kubernetes resources using [CEL][cel] expressions — all inline in
your Crossplane Composition's function pipeline.

## Overview

[KRO][kro] is a standalone Kubernetes controller for declarative resource
orchestration. `function-kro` adapts KRO's approach to run as a Crossplane
composition function, letting you combine KRO-style resource orchestration with
other Crossplane functions in a single pipeline.

`function-kro` supports the full set of upstream KRO features. See the
[KRO documentation][kro-docs] for details on all available capabilities.

## Usage

Use `function-kro` as a step in a Crossplane Composition pipeline. The function
takes a `ResourceGraph` input that defines your resources and their
relationships using `${...}` CEL expressions:

```yaml
apiVersion: apiextensions.crossplane.io/v1
kind: Composition
spec:
  compositeTypeRef:
    apiVersion: example.crossplane.io/v1alpha1
    kind: MyResource
  mode: Pipeline
  pipeline:
  - step: kro
    functionRef:
      name: function-kro
    input:
      apiVersion: kro.fn.crossplane.io/v1beta1
      kind: ResourceGraph
      status:
        vpcId: ${vpc.status.atProvider.id}
      resources:
      - id: vpc
        template:
          apiVersion: ec2.aws.upbound.io/v1beta1
          kind: VPC
          spec:
            forProvider:
              region: ${schema.spec.region}
              cidrBlock: "10.0.0.0/16"
      - id: subnet
        template:
          apiVersion: ec2.aws.upbound.io/v1beta1
          kind: Subnet
          spec:
            forProvider:
              region: ${schema.spec.region}
              vpcId: ${vpc.status.atProvider.id}
              cidrBlock: "10.0.1.0/24"
```

### CEL Expressions

Expressions use `${...}` syntax within resource templates:

- Reference the XR spec: `${schema.spec.region}`
- Reference other resources' observed state: `${vpc.status.atProvider.id}`
- Execute logic inline: `${schema.spec.replicas * 2}`, `arn:aws:s3:::${bucket.status.atProvider.id}`

You don't need to manually order your resources or wire up dependencies.
`function-kro` analyzes the expressions in your templates, builds a dependency
graph (DAG), and automatically determines the correct order to create and
reconcile resources. If a resource references another resource's status, it will
wait until that dependency is ready before proceeding.

## Examples

See the [`example/`](example/) directory for complete working examples:

| Example | Description |
|---------|-------------|
| [basic](example/basic/) | Resource dependencies and status aggregation |
| [conditionals](example/conditionals/) | Conditional resource creation with `includeWhen` |
| [readiness](example/readiness/) | Custom readiness conditions with `readyWhen` |
| [externalref](example/externalref/) | Referencing existing cluster resources outside of the XR/composition |
| [collections](example/collections/) | Dynamic resource expansion with `forEach` |

See the [examples README](example/README.md) for setup instructions and walkthroughs.

## Development

```shell
# Run code generation - see input/generate.go
$ go generate ./...

# Run tests - see fn_test.go
$ go test ./...

# Build the function's runtime image - see Dockerfile
$ docker build . --tag=runtime

# Build a function package - see package/crossplane.yaml
$ crossplane xpkg build -f package --embed-runtime-image=runtime
```

## License

Apache 2.0. See [LICENSE](LICENSE) for details.

[functions]: https://docs.crossplane.io/latest/packages/functions/
[kro]: https://kro.run
[kro-docs]: https://kro.run/docs/overview
[cel]: https://github.com/google/cel-go
