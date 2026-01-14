# Desired State and Server-Side Apply

This document describes issues with how function-kro produces desired state for
Crossplane's Server-Side Apply (SSA) model.

## Background

Crossplane composition functions return "desired state" that Crossplane applies
to the API server using SSA. The desired state should contain **only the fields
the function wants to own**. This is critical because:

1. SSA uses field managers to track ownership - including a field claims it
2. To remove a field, you omit it from desired state - if you copy from observed
   state, you can never remove fields
3. Including fields set by the API server (e.g. `metadata.generation`) or other
   controllers causes ownership conflicts

The kro library was designed for a standalone controller context where it
directly manages resources. function-kro integrates kro into Crossplane's
function pipeline, which requires adapting kro's state model to SSA semantics.

## XR (Composite Resource)

### Problem

The function passes the observed XR to `NewGraphRuntime()`, which stores it as
the instance's `originalObject`. The runtime's `evaluateInstanceStatuses()`
mutates this object in-place, setting status fields from resolved CEL
expressions. Finally, `GetInstance()` returns this mutated observed XR as the
desired state.

```
fn.go:44        oxr = request.GetObservedCompositeResource(req)
fn.go:111       g.NewGraphRuntime(&oxr.Resource.Unstructured)
                    ↓
graph.go:45     instance.originalObject = newInstance  // observed XR
                    ↓
runtime.go:408  evaluateInstanceStatuses() mutates instance in-place
                    ↓
fn.go:179       SetDesiredCompositeResource(rsp, rt.GetInstance())  // full observed XR!
```

This causes the function to claim SSA ownership of every field in the observed
XR, including:

- `metadata.resourceVersion`, `metadata.generation`, `metadata.uid`
- `status.conditions` (owned by Crossplane)
- Any other fields set by the API server or other controllers

### Fix

Build a minimal desired XR containing only the status paths declared in
`ResourceGraph.Status`. kro validates that all status fields must contain CEL
expressions referencing resources (static values are rejected), so we only need
to copy paths from `GetVariables()`:

```go
import "github.com/crossplane/crossplane-runtime/pkg/fieldpath"

// Build minimal desired XR with only declared status fields
src := fieldpath.Pave(rt.GetInstance().Object)
dst := fieldpath.Pave(map[string]any{})

for _, v := range g.Instance.GetVariables() {
    if val, err := src.GetValue(v.Path); err == nil {
        dst.SetValue(v.Path, val)
    }
}

desired := &composite.Unstructured{Unstructured: unstructured.Unstructured{Object: dst.UnstructuredContent()}}
desired.SetAPIVersion(oxr.Resource.GetAPIVersion())
desired.SetKind(oxr.Resource.GetKind())

response.SetDesiredCompositeResource(rsp, &resource.Composite{Resource: desired})
```

Unresolved CEL expressions are automatically handled - `evaluateInstanceStatuses()`
only writes resolved values, so `GetValue()` returns an error for unresolved
paths and they're skipped.

## Composed Resources

### Problem

The runtime's `GetResource()` method returns the observed resource if one was
set via `SetResource()`:

```go
// runtime.go:189-203
func (rt *ResourceGraphDefinitionRuntime) GetResource(id string) (*unstructured.Unstructured, ResourceState) {
    r, ok := rt.resolvedResources[id]
    if ok {
        return r, ResourceStateResolved  // Returns observed!
    }
    // Only returns rendered template if not in resolvedResources
    return rt.resources[id].Unstructured(), ResourceStateResolved
}
```

The function calls `SetResource()` with observed composed resources for CEL
evaluation context, then calls `GetResource()` to produce desired state:

```
fn.go:128       rt.SetResource(id, &r.Resource.Unstructured)  // Store observed
fn.go:151       r, state := rt.GetResource(id)                 // Returns observed!
fn.go:157       cd, err := composed.From(r)                    // Desired = observed
```

On the first reconcile this works (no observed resource exists, so the rendered
template is returned). On subsequent reconciles, the observed resource is
returned, causing the function to claim ownership of:

- Provider-defaulted spec fields
- Status fields (though Crossplane filters these)
- Labels/annotations set by other controllers
- Any field that exists in observed but wasn't in the template

### Fix

Add a new `GetRenderedResource()` method that always returns the rendered
template, leaving `GetResource()` unchanged for backward compatibility with
upstream kro:

```go
// runtime.go - new method
func (rt *ResourceGraphDefinitionRuntime) GetRenderedResource(id string) (*unstructured.Unstructured, ResourceState) {
    if !rt.canProcessResource(id) {
        return nil, ResourceStateWaitingOnDependencies
    }
    // Always return the rendered template, never observed
    return rt.resources[id].Unstructured(), ResourceStateResolved
}
```

Then update `fn.go` to use `GetRenderedResource()` instead of `GetResource()`
when producing desired state.

The `SetResource()` method continues to populate `resolvedResources` for:

- CEL evaluation in `evaluateDynamicVariables()` (e.g. `${vpc.status.atProvider.id}`)
- Readiness checks in `IsResourceReady()`

This separates the read path (observed resources for CEL context) from the write
path (rendered templates for desired state), while preserving the original
`GetResource()` behavior for any code that expects it.

## Upstream kro

Upstream kro has the same `GetResource()` behavior but hasn't surfaced as a
problem because:

1. kro is typically the sole controller managing these resources (no SSA
   conflicts with other field managers)
2. They use a consistent field manager (`kro.run/applyset`)
3. Resource specs tend to be stable after creation
4. No interoperability testing with other tools fighting for field ownership

For the XR/instance, upstream kro uses the `/status` subresource directly via
`UpdateStatus()`, which replaces the entire status rather than using SSA. This
sidesteps the field ownership issue but isn't applicable to function-kro since
Crossplane applies the entire XR via SSA.

A search of kro-run/kro GitHub issues found no reports of this problem.
