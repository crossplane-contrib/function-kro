# function-kro

A [Crossplane Composition Function][functions] for YAML+[CEL][cel] resource
composition that implements the same syntax and experience provided by
[KRO][kro] (Kubernetes Resource Orchestrator). Define complex, interdependent
Kubernetes resources using CEL expressions — all inline in your Crossplane
Composition's function pipeline.

## Overview

`function-kro` is a Crossplane composition function for declarative resource
orchestration. It adapts [KRO][kro]'s approach to run inside Crossplane, letting
you combine CEL-based resource definitions with other Crossplane functions in a
single pipeline.

There is no need to install KRO itself — `function-kro` natively embeds KRO's
graph builder, CEL evaluator, and runtime engine with full feature parity. See
the [KRO documentation][kro-docs] for details on all available capabilities.

## Usage

Use `function-kro` as a step in a Crossplane Composition pipeline. The function
takes a `ResourceGraph` input that defines your resources and their
relationships using `${...}` CEL expressions.

If you already have KRO resource definitions, the `resources` and `status`
blocks are identical to what you'd write in a standalone KRO
`ResourceGraphDefinition` — they drop into the pipeline input without changes.

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
      apiVersion: kro.fn.crossplane.io/v1alpha1
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

## Why Crossplane?

Running resource definitions inside Crossplane's architecture adds capabilities
that come with the platform:

- **Pipeline composability.** Add functions from a broad ecosystem of pipeline
  steps alongside your resource definitions. Enforce policy, inject tags, pull
  secrets, or call external APIs — each as an independent pipeline step that
  adds functionality while leaving your resource definitions untouched.
- **Safe rollouts.** CompositionRevisions let you pin existing instances to a
  known-good revision, roll forward incrementally, and roll back without a
  rewrite.
- **Multi-implementation APIs.** Define one API backed by multiple Compositions.
  The same API schema can have separate AWS, GCP, and Azure implementations
  behind it.
- **Operational controls.** Pause reconciliation, set management policies, and
  control resource lifecycle per-instance without touching the Composition.
- **Developer experience.** Test and validate locally with `crossplane render`,
  [diff][crossplane-diff] against a live environment, and run unit and
  integration tests before deploying.

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
| [omit](example/omit/) | Conditional field removal using the `omit()` CEL function |
| [collection-limits](example/collection-limits/) | Enforcing maximum collection size limits |

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

## Contributing

We welcome contributions of all kinds — opening issues, improving documentation,
fixing bugs, or adding new features. If you don't know where to start or have
any questions, please reach out to us on the `#function-kro` channel on
Crossplane's [Slack][slack].

## License

Apache 2.0. See [LICENSE](LICENSE) for details.

[functions]: https://docs.crossplane.io/latest/packages/functions/
[kro]: https://kro.run
[kro-docs]: https://kro.run/docs/overview
[cel]: https://github.com/google/cel-go
[crossplane-diff]: https://github.com/crossplane-contrib/crossplane-diff
[slack]: https://slack.crossplane.io
