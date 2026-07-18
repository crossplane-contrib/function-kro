# Omit Example

When the XR flag is false, `omit()` removes `enableDnsHostnames` from the desired
VPC entirely rather than setting it to `false`, so Crossplane does not claim
ownership of it under Server-Side Apply. See the
[Omit Example](../README.md#omit-example) in the top-level README for the full
walkthrough on a live cluster.

## Render it locally

`omit()` is gated behind the `CELOmitFunction` feature gate, so start function-kro
with it enabled from the repository root:

```shell
go run . --insecure --debug --feature-gates=CELOmitFunction=true
```

Render the example:

```shell
cd example/omit
crossplane render -r -x xr.yaml composition.yaml functions.yaml --required-schemas=schemas/
```

`xr.yaml` sets `enableDnsHostnames: false`, so the rendered VPC's `forProvider`
has no `enableDnsHostnames` field at all. Set it to `true` in `xr.yaml` and the
field appears, set to `true`.
