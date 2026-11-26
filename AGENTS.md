# Function-KRO Agent Guide

This document provides comprehensive orientation for AI agents and developers working with the function-kro codebase.

## What is Function-KRO?

Function-KRO is a **Crossplane Composition Function** that brings the KRO (Kubernetes Resource Orchestration) experience to Crossplane users. It enables declarative definition and management of complex, interdependent Kubernetes resources using CEL (Common Expression Language) expressions.

### Purpose

The main goal is to enable Crossplane users to have the **KRO experience alongside their other Crossplane functions** and choose it when it makes the most sense for their needs. This provides:

- **Declarative Resource Dependencies**: Define resources that depend on outputs from other resources using CEL expressions
- **Conditional Resource Creation**: Include or exclude resources based on CEL conditions (`includeWhen`)
- **Readiness Conditions**: Define when a resource is considered ready (`readyWhen`)
- **Status Aggregation**: Aggregate data from composed resources back to the XR status
- **Type-Safe Templates**: Full CEL type checking against OpenAPI schemas

### Relationship to KRO

KRO is a standalone Kubernetes controller for resource orchestration. Function-KRO adapts KRO's core logic to run as a Crossplane composition function:

| Standalone KRO | Function-KRO |
|----------------|--------------|
| Watches ResourceGraph CRs directly | Receives input via Crossplane function requests |
| Gets schemas from Kubernetes API server | Gets schemas from Crossplane's schema resolution |
| Creates resources directly | Returns desired resources to Crossplane |

## Architecture Overview

Upstream kro reconciles an RGD instance in layers: `pkg/graphengine/rgdadapter` translates the RGD into a `Graph`, `pkg/graphengine/compiler` compiles it, `pkg/graphengine/runtime` evaluates it, and `pkg/graphengine/executor` walks the nodes and applies each one to the API server.

function-kro imports the rgdadapter, compiler and runtime unmodified as the Go module `github.com/kubernetes-sigs/kro`, and vendors the one layer it has to change, the executor, into `kro/graphengine/executor/`. Upstream applies each node to the API server. Crossplane applies, watches and garbage collects composed resources, so our copy returns desired state to Crossplane instead.

Upstream's rgdadapter types the `schema` variable from the RGD's SimpleSchema. We have the XR's OpenAPI schema from Crossplane instead, so `engine.go` wraps the compiler we pass to the rgdadapter and overrides the `schema` node's type with it.

`patches/v0.10.0_PATCHES.md` documents every change we make to upstream code, and `./scripts/diff-upstream-kro.sh` shows them. The "Vendoring Rules" section of `patches/UPGRADE_PROCESS.md` says when to vendor an upstream package, and where code that repeats upstream logic may live.

### Request Processing Flow

```
Crossplane RunFunctionRequest
    ↓
┌──────────────────────────────────────────────────────────────────┐
│ fn.go: RunFunction()                                             │
│                                                                  │
│  1. Parse ResourceGraph input                                    │
│  2. Request schemas (requireSchemas → buildResolver)             │
│  3. Wrap the input as an upstream ResourceGraphDefinition        │
│     (engine.go: resourceGraphDefinition)                         │
│  4. Translate, compile and build the runtime with upstream's     │
│     rgdadapter.BuildRuntimeForInstance, typed from the XR schema │
│     - XR schema gets a `context` property for schema.context     │
│     - schema node rendered from an XR copy with the context set  │
│  5. Gather external resources Crossplane fetched, and group      │
│     observed composed resources by node                          │
│  6. Walk nodes in topological order with the vendored executor   │
│     (kro/graphengine/executor: Crossplane.Apply → Simple.Apply): │
│     - wait until dependencies are ready                          │
│     - check IsIgnored (includeWhen + contagious skip)            │
│     - Resolve (render), emit desired resources or request        │
│       external refs, publish observed state into scope           │
│     - CheckReadiness                                             │
│     - keep existing resources that can't be processed this run   │
│  7. Build XR status from the status node, and set author         │
│     conditions (runtime.newCondition) on the XR                  │
└──────────────────────────────────────────────────────────────────┘
    ↓
Crossplane RunFunctionResponse (desired resources + XR status)
```

### Key Components

```
fn.go                         Function entry point, schema resolution, observed grouping
engine.go                     Input wrapping, REST mapper, schema resolver, and the
                              compiler wrapper that types `schema` from the XR's schema
kro/graphengine/executor/     Upstream's executor, vendored and adapted
    ├── simple.go                The node walk (upstream, lightly patched)
    ├── executor.go              Types and errors (identical to upstream)
    └── crossplane.go            Our additions: Crossplane stands in for the API server
internal/schemaresolver/      Resolves OpenAPI schemas from what Crossplane sends
    ├── schema_map_resolver.go   From required_schemas (Crossplane v2.2+)
    ├── crd_resolver.go          From CRDs (older Crossplane)
    └── combined_resolver.go     Pairs either with built-in Kubernetes types
```

Upstream packages we vendor and import are listed in `patches/v0.10.0_PATCHES.md`.

## Codebase Structure

```
function-kro/
├── fn.go                    # Main function implementation (START HERE)
├── engine.go                # Builds the upstream graph engine runtime
├── main.go                  # CLI entry point with gRPC server setup
├── spec-desired-ssa.md      # SSA design spec (historical — see note in Design Decisions)
│
├── kro/graphengine/         # Vendored upstream executor, adapted for Crossplane
├── input/v1alpha1/          # Input API definitions (ResourceGraph type)
├── internal/schemaresolver/ # Schema resolution from Crossplane-provided schemas or CRDs
├── schemas/                 # Generated OpenAPI definitions for built-in Kubernetes types
│
├── patches/                 # Upgrade documentation
│   ├── UPGRADE_PROCESS.md   # Process for upgrading to a new upstream kro release
│   ├── UPSTREAM_DIFFERENCES.md # How function-kro behaves differently from kro
│   └── v0.10.0_PATCHES.md   # Current adaptation reference (v0.10.0 baseline)
├── package/                 # Crossplane package definition
├── example/                 # Usage examples (basic, collections, conditionals, externalref, readiness, omit, core, app, context, conditions, collection-limits)
├── scripts/                 # Build scripts (build-local.sh, diff-upstream-kro.sh)
└── Dockerfile               # Production build
```

## Key Concepts

### ResourceGraph Input

The function receives a `ResourceGraph` as input containing:

```go
type ResourceGraph struct {
    Status    runtime.RawExtension  // CEL expressions for XR status
    Resources []*Resource           // Ordered list of resources
}

type Resource struct {
    ID          string               // Unique identifier (valid CEL identifier)
    Template    runtime.RawExtension // Kubernetes manifest with ${...} expressions
    ExternalRef *ExternalRef         // OR reference to existing resource (mutually exclusive with Template)
    ReadyWhen   []string             // CEL conditions for readiness (AND logic)
    IncludeWhen []string             // CEL conditions for inclusion (AND logic)
    ForEach     []ForEachDimension   // Expands resource into a collection
}

type ForEachDimension map[string]string  // iterator variable name → CEL list expression
```

### Node Kinds

Upstream's compiler gives each node a kind that determines how the walk handles it:

- **Def** (`schema`): the XR itself, exposed to CEL as `${schema}`.
- **Template**: a resource we render and return as a desired composed resource. A template with `forEach` is a collection, named `{id}-{metadata.name}` in composed output (e.g., `subnets-my-app-us-east-1`).
- **Ref**: an `externalRef`. We ask Crossplane to fetch it and never return it as desired. A ref with a selector is a collection of whatever matches.
- **Patch** (`instance`): the XR's status, rendered from the ResourceGraph's status block. Fields whose data is pending are left out.

See `applyNodeByKind` in `kro/graphengine/executor/simple.go` for how each kind is handled, and `crossplane.go` beside it for the Crossplane-specific parts.

### CEL Expressions

CEL expressions use `${...}` syntax within templates:

- **Static**: Reference only the XR spec: `${schema.spec.region}`
- **Context**: Read the function context an earlier pipeline step wrote: `${schema.context["apiextensions.crossplane.io/environment"].region}`. Keys containing dots or slashes need bracket syntax. Context is read-only.
- **Dynamic**: Reference other resources: `${vpc.status.atProvider.id}`
- **String templates**: Embedded in strings: `arn:aws:s3:::${bucket.status.atProvider.id}`
- **Standalone**: Single expression for entire field value
- **Iteration**: Within forEach, iterator variables are available: `${item.name}`; readyWhen uses `each` for per-item checks

### Variable Resolution Lifecycle

```
Static Variables (evaluated at init):
  schema.spec.* → Evaluate once → Use in templates

Dynamic Variables (evaluated iteratively):
  resource.status.* → Wait for dependency → Evaluate → Propagate
```

### Schema Resolution

The function tries two paths for maximum compatibility:

1. **RequiredSchemas** (modern Crossplane): Request OpenAPI schemas directly
2. **RequiredCRDs** (fallback): Extract schemas from CRD objects

## Development Guide

### Building

```bash
# Generate code (protobuf, deepcopy)
go generate ./...

# Build Docker image
docker build . --tag=runtime

# Build Crossplane package
crossplane xpkg build -f package --embed-runtime-image=runtime

# Local build with replace statements
./scripts/build-local.sh <xpkg-ref>
```

### Testing

```bash
# Run all tests
go test -v -cover ./...

# Run with race detection
go test -race ./...
```

### Test Patterns

Tests use table-driven patterns with `map[string]struct`:

```go
cases := map[string]struct {
    reason string
    args   args
    want   want
}{
    "TestCaseName": {
        reason: "Description of what this tests",
        args:   args{...},
        want:   want{...},
    },
}
```

### Test Assertion Conventions

All `RunFunction` tests (in `fn_test.go`) compare the **full expected response** using `cmp.Diff` with `protocmp.Transform()`, and check errors with `cmpopts.EquateErrors()`. Follow this pattern even for standalone test functions that live outside the main table:

```go
if diff := cmp.Diff(want, rsp, protocmp.Transform()); diff != "" {
    t.Errorf("...\nf.RunFunction(...): -want rsp, +got rsp:\n%s", reason, diff)
}

if diff := cmp.Diff(nil, err, cmpopts.EquateErrors()); diff != "" {
    t.Errorf("...\nf.RunFunction(...): -want err, +got err:\n%s", reason, diff)
}
```

Do **not** use testify assertions (`require.NoError`, `assert.Contains`, etc.) or manual field-by-field checks in `fn_test.go`. Construct the full expected `*fnv1.RunFunctionResponse` — including `Results` for fatal cases — and diff it. This keeps all tests consistent and makes failures easy to read.

### End-to-End Example Validation

The `/test-examples` skill validates all examples from `example/README.md` end-to-end against a real kind cluster with AWS resources. It creates the cluster, installs Crossplane and extensions, executes each example's commands, makes automated pass/fail assertions based on the README's descriptions, and cleans up. Stops on first failure.

```
/test-examples
```

### Linting

```bash
golangci-lint run
```

## Common Development Tasks

### Adding a New Resource Field

Fields on `Resource`, `ExternalRef` and `ForEachDimension` come from upstream's `api/v1alpha1` and arrive when we bump the kro dependency. For a field only function-kro needs:

1. Update `input/v1alpha1/input.go` with the new field
2. Run `go generate ./...` to regenerate code
3. Handle it in `engine.go` (translating input to upstream types) or `kro/graphengine/executor/crossplane.go` (the walk), and document any change to vendored code in the patches doc
4. Add tests

### Modifying CEL Expression Handling

CEL parsing, type checking and evaluation are upstream's (`pkg/graphengine/compiler`, `pkg/graphengine/runtime`, `pkg/cel`). What we control is what's in scope: `emitTemplate` in `kro/graphengine/executor/crossplane.go` gives the walk each node's observed state, or its rendered state if Crossplane hasn't created it yet, and the walk publishes that.

### Debugging Schema Resolution

The function logs schema resolution attempts. Key code paths:
- `fn.go:requireSchemas()` - Requests schemas from Crossplane (both paths)
- `fn.go:buildResolver()` - Dispatcher: picks schema path vs CRD path based on capability detection
- `fn.go:buildResolverFromSchemas()` - Modern path (Crossplane v2.2+, RequiredSchemas capability)
- `fn.go:buildResolverFromCRDs()` - Fallback path (older Crossplane, extracts schemas from CRDs)
- `internal/schemaresolver/` - Resolver implementations

### Understanding Dependency Order

Upstream's compiler builds the dependency graph and `runtime.Nodes()` returns nodes in topological order. The vendored executor's `Simple.Apply` holds a node back until every node it depends on is ready, because `Crossplane.Apply` turns on readiness gating as upstream's instance controller does.

### Collections (ForEach)

Resources with `forEach` expand into multiple composed resources at runtime:
- Upstream's runtime expands forEach dimensions (cartesian product) when a collection node is resolved
- `emitTemplate` (`kro/graphengine/executor/crossplane.go`) labels each item with `kro.run/collection-index` and names it `{id}-{metadata.name}` in composed resources (e.g., `subnets-my-app-us-east-1`)
- Collection `readyWhen` uses the `each` variable to evaluate per-item readiness. A collection isn't ready until every rendered item is observed.
- `fn.go:groupObservedByNodeID` matches observed composed resources back to collection nodes by the `kro.run/collection-index` label and the composed resource name prefix

### External References

ExternalRef resources reference existing cluster resources without creating them:
- The vendored executor's `applyRef` and `applyRefCollection` render the ExternalRef (name, namespace and selector can use CEL), and `Crossplane.Get` and `Crossplane.List` ask Crossplane to fetch it via required resources
- Crossplane can only select by exact label values (https://github.com/crossplane/crossplane/issues/6744), so for a selector with `matchExpressions` we fetch by `matchLabels` and filter the rest ourselves
- Multi-phase: until Crossplane has fetched a ref, the ref isn't ready and its dependents wait for a later invocation
- An external collection that matches nothing is a valid, empty, ready collection
- External refs are never returned as desired composed resources

## Key Dependencies

| Package | Purpose |
|---------|---------|
| `github.com/kubernetes-sigs/kro` | kro's graph engine, CEL libraries and API types |
| `github.com/crossplane/function-sdk-go` | Crossplane function SDK |
| `github.com/google/cel-go` | CEL expression evaluation |
| `k8s.io/apimachinery` | Kubernetes types and utilities |
| `k8s.io/kube-openapi` | OpenAPI schema handling |

## Important Design Decisions

### Server-Side Apply (SSA) Compatibility

Desired composed resources contain only the fields their templates render, so we never claim ownership of provider-defaulted fields during SSA. The desired XR contains only the status fields the ResourceGraph declares and could resolve. See `spec-desired-ssa.md` for the original design spec (note: the spec references older API names like `GetRenderedResource`; the implementation evolved since).

### Two-Path Schema Resolution

Supporting both `RequiredSchemas` and `RequiredCRDs` ensures compatibility with different Crossplane versions while preferring the more efficient schema-based approach.

### Function Context

`fn.go` exposes the function context as `schema.context` without changes to `kro/`. It adds a `context` property to the XR schema whose values preserve unknown fields and are nullable, and passes the runtime a copy of the observed XR with the context set, using upstream's `runtime.WithNodeObjectOverride` for the `schema` node. A template that reads through a context key whose whole value is null (`{"apiextensions.crossplane.io/environment": null}`) leaves its resource pending, but a null value nested under a key (`{"apiextensions.crossplane.io/environment": {"region": null}}`) renders as `null` into the composed resource. Writing to the context is not supported.

### Keeping Resources That Can't Be Processed

Crossplane deletes any composed resource a function stops returning. Upstream kro instead keeps a resource it can't process this cycle. So when a node is waiting on a dependency or its data, `preserve` (`kro/graphengine/executor/crossplane.go`) renders an existing resource again to keep it, and reads a waiting external ref so resources that read it can be rendered again. If a resource can't be rendered at all, the function returns a fatal result, which makes Crossplane leave every resource as it is.

The exception is when we're still waiting on Crossplane to fetch an external ref. Crossplane won't fetch anything after a fatal result, so the function returns without the resource instead, and Crossplane fetches the ref and calls it again. Crossplane only uses the response from that last call, so it doesn't delete the resource.

## Key Reference Documents

- `patches/v0.10.0_PATCHES.md`: every change we make to upstream kro code, and our code that tracks upstream (the definitive source for what changed from upstream)
- `patches/UPGRADE_PROCESS.md`: process for upgrading to a new upstream kro release
- `patches/UPSTREAM_DIFFERENCES.md`: where function-kro behaves differently from upstream kro, and validation it doesn't run
- `spec-desired-ssa.md` — Original SSA design spec (references older API names; implementation evolved)
- `example/README.md` — Working examples for all major features (basic, collections, conditionals, externalref, readiness, omit, core, app, context, conditions, collection-limits)

## Auditing Our Code Against Upstream KRO

**MANDATORY PROCESS:** When asked to audit, compare, verify, or review function-kro's KRO libraries against upstream KRO, you MUST follow this process exactly. Do NOT guess, speculate, or infer what is or isn't upstream code by reading our files alone.

### Preferred: Use Skills

Two skills automate the audit process end-to-end:

- **`/audit-patches v<tag>`** — Validates that `patches/v*_PATCHES.md` accurately documents all modifications, additions, and exclusions. Use when checking documentation accuracy or before an upgrade. Current baseline: `v0.10.0`.
- **`/review-kro-adaptations v<tag>`** — Principal-level engineering review of every adaptation. Assesses minimality, correctness, maintainability, and whether a senior engineer would make different choices. Use for code quality assessment.

Both skills use the diff script under the hood, clone upstream once, and produce structured reports.

### Manual Fallback

If the skills are unavailable or you need to debug, use the diff script directly:

1. **Run the diff script.** This is non-negotiable. The script clones upstream KRO and performs real file-by-file diffs with import path normalization:
   ```bash
   # Summary of all differences
   ./scripts/diff-upstream-kro.sh -s -r <upstream-tag>

   # Full diff for a specific file
   ./scripts/diff-upstream-kro.sh -f graphengine/executor/simple.go -r <upstream-tag>

   # Full diff of everything (verbose)
   ./scripts/diff-upstream-kro.sh -r <upstream-tag>
   ```

2. **Use the script output as the sole source of truth.** The script categorizes every file as `[IDENTICAL]`, `[MODIFIED]`, `[LOCAL ONLY]`, or `[UPSTREAM ONLY]`. Only files marked `[MODIFIED]` are actual adaptations. Only files marked `[LOCAL ONLY]` are our additions.

3. **For any claim about a specific file being modified or identical, cite the diff.** Run the script with `-f <file>` to get the actual diff. Do not claim a file is modified without seeing the diff output.

### What You Must NOT Do

- Do NOT read our code and guess whether something "looks like" a function-kro addition
- Do NOT claim a function exists only in function-kro without checking upstream
- Do NOT report findings without having run the diff script first
- Do NOT use your training data knowledge of upstream KRO — it may be outdated

### When to Use This Process

- User asks to "audit", "compare", "verify", or "check" our code against upstream
- User asks if the patches documentation is accurate
- User asks what we've changed from upstream KRO
- During upgrade process (Phase 1, Step 1.0 of UPGRADE_PROCESS.md)

## Troubleshooting

### "schema not found" Errors

Check that the GVK is included in the composition's schema requirements or that a CRD exists for the type.

### Circular Dependency Errors

Review resource references to ensure no cycles. Compiling the graph fails fast on a cycle.

### Type Mismatch Errors

Ensure CEL expression return types match the expected field types from the OpenAPI schema. Upstream's compiler type checks every expression against the schema of the field it writes.
