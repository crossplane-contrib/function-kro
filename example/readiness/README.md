# Readiness Example

Each resource sets its own `readyWhen` CEL conditions, so there is no
`function-auto-ready` step. See the
[Readiness Example](../README.md#readiness-example) in the top-level README for
the full walkthrough on a live cluster.

## Render it locally

Start function-kro from the repository root:

```shell
go run . --insecure --debug
```

Render the example:

```shell
cd example/readiness
crossplane render -r -x xr.yaml composition.yaml functions.yaml --required-schemas=schemas/
```

This renders the composite and the VPC. The subnet and security group depend on
the VPC's ID, so render defers them, and because nothing reports a status yet, no
`readyWhen` is satisfied and the composite stays not-ready.

Add `--observed-resources=observed.yaml`, which gives each resource a Ready
condition and an ID:

```shell
crossplane render -r -x xr.yaml composition.yaml functions.yaml --required-schemas=schemas/ --observed-resources=observed.yaml
```

Every `readyWhen` is now satisfied, so the whole graph renders and the composite
reports `Ready=True`.
