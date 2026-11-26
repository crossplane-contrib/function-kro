# How function-kro Differs from Upstream kro

function-kro aims to match upstream kro in functionality: the resources you can compose together, and the language features that configure them and flow data between them (CEL expressions, `includeWhen`, `readyWhen`, `forEach`, external refs, status, and conditions). It uses upstream kro's own graph engine to do that, so most behavior matches by construction.

This document lists where a ResourceGraph behaves differently than the same RGD would in kro, and the validation function-kro doesn't run. It leaves out jobs Crossplane does in place of kro's controller, like applying, watching and deleting resources, or defining the XR's schema. Update it whenever an upgrade or a change in this repo adds or removes a difference.

## Behavior differences

### An existing resource that can't be rendered fails the run

When a resource already exists but its template can't be rendered this run, function-kro returns a fatal result. Crossplane then applies nothing, so no composed resource is created, changed or deleted, and marks the XR `Synced=False`.

If the data the template is missing comes from something function-kro doesn't create, like a dependency's status field that went empty or an external ref that disappeared, the XR recovers once the data is back. If the data can only come from a resource function-kro still has to create, it never arrives, and the XR stays stuck until you step in, for example by reverting the change. This happens when:

- An existing resource reads `${subnets.map(s, s.status.atProvider.id)}`, and you add a subnet. The new subnet is never created, so it never has an ID.
- You change the composition so an existing resource reads the status of a new resource.
- Someone deletes a composed resource whose status another resource reads.

The XR's status block doesn't have this problem: status fields that are waiting on data are left out.

Upstream kro keeps the resource as it is, holds back pruning, and retries without reporting an error.

We can't do what upstream does, because Crossplane deletes any composed resource a function stops returning, and returning the observed object would claim every field on it under server-side apply. See https://github.com/crossplane-contrib/function-kro/issues/126.

### A resource waiting on a dependency still gets updates

Upstream kro doesn't apply a resource until every resource it depends on is ready. function-kro holds back new resources the same way, but has to keep returning a resource that already exists or Crossplane deletes it. So an existing resource keeps receiving changes to its template while a dependency is not ready.

### A hard error in one resource fails the whole run

If one resource has an error that waiting won't fix, such as a CEL evaluation error, function-kro fails the run. Upstream kro keeps going with the resources that don't depend on it.

### Dependents see rendered values a pass earlier than server-set values

Upstream kro applies each resource and gives dependents what the API server returned. function-kro can't apply anything, so until Crossplane has created a resource, its dependents see the values function-kro rendered for it. Fields the template sets, like `metadata.name`, are usable right away. Fields the API server or a provider sets, like a Service's `spec.clusterIP` or anything under `status`, appear one function run later. An expression that defaults a missing field, such as `service.spec.?clusterIP.orValue("")`, produces the default in between.

### XR status isn't checked against the XRD

Upstream kro generates the instance's status schema from the RGD's status fields, so they always match. With function-kro, the XRD defines the XR's status schema, and function-kro doesn't check the ResourceGraph's status fields against it. A status field the XRD doesn't declare, or one whose type doesn't match, isn't caught when function-kro compiles the ResourceGraph. It only reaches the API server when Crossplane writes the XR's status.

### Resource scope is inferred

Upstream kro asks the API server whether each kind is namespaced. function-kro can't ask, so it infers scope from the ResourceGraph: every kind a namespaced XR composes or references is namespaced, and under a cluster-scoped XR a kind is namespaced only if a template or external ref sets a namespace on it.

Upstream kro rejects two namespace mistakes that function-kro lets through:

- A template that sets a namespace on a cluster-scoped kind, like a ClusterRole. function-kro infers scope from the templates, so it treats that kind as namespaced. Under a cluster-scoped XR, the API server ignores the namespace and creates the resource. Under a namespaced XR, Crossplane rejects any cluster-scoped kind anyway.
- A template under a cluster-scoped XR that leaves the namespace off a namespaced kind, like a ConfigMap. Crossplane can't apply it and reports an error on the XR.

## Conditions

### Authors can't set Crossplane's condition types

Upstream kro lets an RGD author define a condition named `Ready` with `runtime.newCondition`. Crossplane sets `Ready`, `Synced`, `Healthy`, `UpToDate` and `Responsive` itself and ignores them from a function, so function-kro leaves them out and returns a warning. Other author conditions appear on the XR as written.

### `runtime.condition` reads approximations of kro's built-in conditions

function-kro doesn't compute kro's built-in instance conditions, so `runtime.condition(schema, '<type>')` reads approximations instead: `GraphResolved` is always `True`, `ResourcesReady` and `Ready` are `True` when every resource that isn't excluded by `includeWhen` is ready, and `InstanceManaged` is empty.

### Author conditions read `schema.spec` without format types

A `runtime.newCondition` expression reads `schema.spec` fields as plain JSON values. Upstream kro converts a field by its declared `format`, so a `date-time` string becomes a CEL timestamp and a `byte` string becomes bytes. Resource templates, `includeWhen`, `readyWhen` and status fields get those types as they do upstream.

## Validation function-kro doesn't run

Upstream kro validates an RGD when it's created. function-kro skips that step, because it requires upstream code that isn't public. Compiling the graph still catches type errors, unknown references, dependency cycles, duplicate resource IDs, and IDs that aren't valid identifiers. These are accepted by function-kro but rejected by upstream kro:

- Resource IDs kro reserves, such as `instance`. An `instance` resource fails to compile once the ResourceGraph has a status block, with a less obvious error.
- Resource IDs that aren't lowerCamelCase, such as `MyBucket`.
- `kro.run/` annotations in templates.
- A `runtime.newCondition` call missing required keys. function-kro reports it as a warning when it evaluates conditions, rather than rejecting the ResourceGraph.
