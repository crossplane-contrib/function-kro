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

### Request Processing Flow

```
Crossplane RunFunctionRequest
    ↓
┌──────────────────────────────────────────────────────────────────┐
│ fn.go: RunFunction()                                             │
│                                                                  │
│  1. Parse ResourceGraph input                                    │
│  2. Request schemas (requireSchemas → buildResolver)             │
│  3. Build resource graph (graph.NewBuilder → NewResourceGraphDef)│
│  4. Create runtime (runtime.FromGraph)                           │
│  5. Process external refs:                                       │
│     - Resolve identity (GetDesiredIdentity with CEL)             │
│     - Request from Crossplane, inject as observed state          │
│  6. Set observed composed resources on runtime nodes             │
│  7. Process nodes in topological order:                          │
│     - Check IsIgnored (includeWhen + contagious dependency skip) │
│     - Evaluate GetDesired (hard resolve for resources/collections│
│       soft resolve for instance)                                 │
│     - Handle collections (forEach → {id}-{name}, ...)            │
│     - Evaluate readiness (CheckReadiness → error sentinel)      │
│  8. Build XR status from instance node's soft-resolved fields    │
└──────────────────────────────────────────────────────────────────┘
    ↓
Crossplane RunFunctionResponse (desired resources + XR status)
```

### Key Components

```
fn.go                        Main function entry point
    ↓
kro/graph/builder.go         Builds validated resource graphs
    ├── node.go              Node types (Resource, Collection, External, Instance)
    ├── parser/              Extracts CEL expressions from templates
    ├── fieldpath/           Field path building and parsing
    ├── schema/resolver/     OpenAPI schema resolution strategies
    ├── dag/                 Directed acyclic graph for dependencies
    ├── variable/            CEL variable management
    └── validation.go        Validates resources and expressions
    ↓
kro/runtime/                 Executes the graph, evaluates CEL
    ├── runtime.go           Runtime interface, FromGraph constructor
    ├── node.go              Runtime node (GetDesired, CheckReadiness, IsIgnored, etc.)
    ├── node_resolve.go      Resolution helpers (soft/hard resolve, collections)
    ├── collection.go        Collection identity matching, index labels
    └── resolver/            Template field resolution (path-based value injection)
    ↓
kro/cel/                     CEL environment setup and evaluation
    ├── environment.go       CEL environment construction with options
    ├── ast/inspector.go     AST inspection for dependency extraction
    ├── library/random.go    Custom CEL random functions
    └── schemas.go           Schema-to-CEL type mapping
```

## Codebase Structure

```
function-kro/
├── fn.go                    # Main function implementation (START HERE)
├── main.go                  # CLI entry point with gRPC server setup
├── spec-desired-ssa.md      # SSA design spec (historical — see note in Design Decisions)
│
├── input/v1alpha1/          # Input API definitions (ResourceGraph type)
│
├── kro/                     # Core KRO implementation
│   ├── cel/                 # CEL environment setup and expression evaluation
│   │   ├── ast/             # AST inspection for dependency extraction
│   │   └── library/         # Custom CEL functions (random)
│   ├── features/            # Feature gate definitions
│   ├── graph/               # Graph building and validation
│   │   ├── builder.go       # Main graph builder (key file)
│   │   ├── node.go          # Node types and graph node structure
│   │   ├── validation.go    # Resource and expression validation
│   │   ├── dag/             # Generic DAG with cycle detection
│   │   ├── fieldpath/       # Field path building and parsing
│   │   ├── parser/          # Extracts ${...} expressions from templates
│   │   ├── schema/resolver/ # OpenAPI schema resolution strategies
│   │   └── variable/        # CEL variable management
│   ├── runtime/             # Runtime execution engine
│   │   ├── node.go          # Runtime node (GetDesired, IsReady, IsIgnored, etc.)
│   │   ├── collection.go    # Collection identity matching, index labels
│   │   └── resolver/        # Template field resolution engine
│   ├── metadata/            # Kubernetes metadata utilities (labels, finalizers, GVK)
│   └── testutil/            # Test helpers (generator, fake discovery/resolver)
│
├── patches/                 # Upgrade documentation
│   ├── v0.9.0_PATCHES.md   # Current adaptation reference (v0.9.0 baseline)
│   ├── v0.8.x_PATCHES.md   # Historical v0.8.x adaptation reference
│   └── UPGRADE_PROCESS.md   # Process for upgrading from upstream KRO
├── package/                 # Crossplane package definition
├── example/                 # Usage examples (basic, collections, conditionals, externalref, readiness)
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

### Node Types

The graph builder categorizes each resource into a NodeType that determines runtime behavior:

- **NodeTypeResource**: Standard managed resource. Hard resolution (fails if any CEL expression can't resolve).
- **NodeTypeCollection**: forEach-expanded resource. Hard resolution per expansion item. Named `{id}-{metadata.name}` in composed output (e.g., `subnets-my-app-us-east-1`).
- **NodeTypeExternal**: ExternalRef read-only resource. Identity-only resolution (name/namespace). Excluded from desired output.
- **NodeTypeExternalCollection**: ExternalRef with selector, expands to multiple observed resources. Excluded from desired output.
- **NodeTypeInstance**: The XR itself. Soft resolution (best-effort partial status — skips unresolvable fields).

See `kro/graph/node.go` for type definitions, `kro/runtime/node.go` for evaluation logic.

### CEL Expressions

CEL expressions use `${...}` syntax within templates:

- **Static**: Reference only the XR spec: `${schema.spec.region}`
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

# Run specific package tests
go test -v ./kro/runtime/...
go test -v ./kro/graph/...

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

1. Update `input/v1alpha1/input.go` with new field
2. Run `go generate ./...` to regenerate code
3. Update `kro/graph/builder.go` to handle new field
4. Add validation in `kro/graph/validation.go`
5. Add tests

### Modifying CEL Expression Handling

Key files:
- `kro/graph/parser/parser.go` - Expression extraction
- `kro/cel/environment.go` - CEL environment setup
- `kro/runtime/runtime.go` - Expression evaluation

### Debugging Schema Resolution

The function logs schema resolution attempts. Key code paths:
- `fn.go:requireSchemas()` - Requests schemas from Crossplane (both paths)
- `fn.go:buildResolver()` - Dispatcher: picks schema path vs CRD path based on capability detection
- `fn.go:buildResolverFromSchemas()` - Modern path (Crossplane v2.2+, RequiredSchemas capability)
- `fn.go:buildResolverFromCRDs()` - Fallback path (older Crossplane, extracts schemas from CRDs)
- `kro/graph/schema/resolver/` - Resolver implementations

### Understanding Dependency Order

The DAG package (`kro/graph/dag/`) handles topological sorting:
- `AddVertex()` - Register resources
- `AddDependencies()` - Add edges (fails on cycles)
- `TopologicalSort()` - Get processing order

### Collections (ForEach)

Resources with `forEach` expand into multiple composed resources at runtime:
- Builder sets `NodeTypeCollection` on the graph node (`kro/graph/builder.go`)
- Runtime evaluates forEach dimensions as CEL list expressions, then expands via cartesian product for multi-dimensional cases (`kro/runtime/node.go:hardResolveCollection`)
- Each expanded resource gets a `kro.run/collection-index` label and is named `{id}-{metadata.name}` in composed resources (e.g., `subnets-my-app-us-east-1`)
- Collection `readyWhen` uses the `each` variable to evaluate per-item readiness
- Observed composed resources are matched back to collection nodes by checking the `kro.run/collection-index` label and stripping the `-N` suffix

Key files: `kro/runtime/node.go` (expansion logic), `kro/runtime/collection.go` (identity matching, index labels), `fn.go` (observation grouping)

### External References

ExternalRef resources reference existing cluster resources without creating them:
- Builder sets `NodeTypeExternal` on the graph node (mutually exclusive with Template)
- `fn.go:externalRefSelectorsFromRuntime()` evaluates CEL expressions in external ref metadata (name/namespace can use `${schema.spec.*}`)
- Crossplane fetches the external resource; its observed state is injected on the runtime node via `SetObserved()`
- Multi-phase: if a dependency isn't observed yet, the external ref is skipped and resolved on the next function invocation
- External refs are excluded from desired composed resources (read-only semantics)

Key files: `fn.go` (external ref processing), `input/v1alpha1/input.go` (ExternalRef, ExternalRefMetadata types)

## Key Dependencies

| Package | Purpose |
|---------|---------|
| `github.com/crossplane/function-sdk-go` | Crossplane function SDK |
| `github.com/google/cel-go` | CEL expression evaluation |
| `k8s.io/apimachinery` | Kubernetes types and utilities |
| `k8s.io/kube-openapi` | OpenAPI schema handling |

## Important Design Decisions

### Server-Side Apply (SSA) Compatibility

The runtime returns only rendered template fields via `node.GetDesired()` (returns `[]*unstructured.Unstructured`). This prevents claiming ownership of provider-defaulted fields during SSA. For the instance node (XR status), `softResolve()` returns only fields where all CEL expressions resolved successfully, preventing partial template strings from leaking into status. See `spec-desired-ssa.md` for the original design spec (note: the spec references older API names like `GetRenderedResource`; the implementation evolved during the v0.8.x rewrite).

### Two-Path Schema Resolution

Supporting both `RequiredSchemas` and `RequiredCRDs` ensures compatibility with different Crossplane versions while preferring the more efficient schema-based approach.

### Expression Caching

The runtime deduplicates CEL expression evaluation via a shared `expressionsCache` in `runtime.FromGraph()`. Non-iteration expressions that appear in multiple nodes share the same `expressionEvaluationState`, so they're evaluated once and reused. Iteration expressions (forEach) are NOT cached because they need fresh evaluation with different iterator bindings per item.

## Key Reference Documents

- `patches/v0.9.0_PATCHES.md` — Comprehensive reference for all KRO v0.9.0 adaptations (the definitive source for what changed from upstream)
- `patches/v0.8.x_PATCHES.md` — Historical reference for KRO v0.8.x adaptations (superseded by v0.9.0)
- `patches/UPGRADE_PROCESS.md` — Process for upgrading from upstream KRO releases
- `spec-desired-ssa.md` — Original SSA design spec (references older API names; implementation evolved)
- `example/README.md` — Working examples for all major features (basic, collections, conditionals, externalref, readiness, omit, collection-limits)

## Auditing Our Code Against Upstream KRO

**MANDATORY PROCESS:** When asked to audit, compare, verify, or review function-kro's KRO libraries against upstream KRO, you MUST follow this process exactly. Do NOT guess, speculate, or infer what is or isn't upstream code by reading our files alone.

### Preferred: Use Skills

Two skills automate the audit process end-to-end:

- **`/audit-patches v<tag>`** — Validates that `patches/v*_PATCHES.md` accurately documents all modifications, additions, and exclusions. Use when checking documentation accuracy or before an upgrade. Current baseline: `v0.9.0`.
- **`/review-kro-adaptations v<tag>`** — Principal-level engineering review of every adaptation. Assesses minimality, correctness, maintainability, and whether a senior engineer would make different choices. Use for code quality assessment.

Both skills use the diff script under the hood, clone upstream once, and produce structured reports.

### Manual Fallback

If the skills are unavailable or you need to debug, use the diff script directly:

1. **Run the diff script.** This is non-negotiable. The script clones upstream KRO and performs real file-by-file diffs with import path normalization:
   ```bash
   # Summary of all differences
   ./scripts/diff-upstream-kro.sh -s -r <upstream-tag>

   # Full diff for a specific file
   ./scripts/diff-upstream-kro.sh -f graph/builder.go -r <upstream-tag>

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

Review resource references to ensure no cycles. The DAG builder will fail fast on cycle detection.

### Type Mismatch Errors

Ensure CEL expression return types match the expected field types from the OpenAPI schema. Check `kro/graph/validation.go` for type checking logic.
