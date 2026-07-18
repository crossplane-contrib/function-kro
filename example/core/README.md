# Core Example

Composes a core Kubernetes `Secret`, showing function-kro handles built-in
Kubernetes types, not just provider managed resources. See the
[Core example](../README.md#core-example) in the top-level README for the full
walkthrough on a live cluster.

## Render it locally

Start function-kro from the repository root:

```shell
go run . --insecure --debug
```

Render the example:

```shell
cd example/core
crossplane render -r -x xr.yaml composition.yaml functions.yaml --required-schemas=schemas/
```

The Secret has no dependencies, so this renders the complete result: the
composite plus the Secret, with `secretField` taken from the XR spec.
