# Conditionals Example

Creates the public subnet and security group only when the matching XR flags are
set, using `includeWhen`. See the
[Conditionals Example](../README.md#conditionals-example) in the top-level README
for the full walkthrough on a live cluster.

## Render it locally

Start function-kro from the repository root:

```shell
go run . --insecure --debug
```

Render the example:

```shell
cd example/conditionals
crossplane render -r -x xr.yaml composition.yaml functions.yaml --required-schemas=schemas/
```

`xr.yaml` sets `enablePublicSubnet: false` and `enableSecurityGroup: true`, so
`includeWhen` drops the public subnet from the graph. This first pass renders the
composite and the VPC; the private subnet and security group depend on the VPC's
ID, so render defers them.

Add `--observed-resources=observed.yaml` to supply a VPC ID and see the rest:

```shell
crossplane render -r -x xr.yaml composition.yaml functions.yaml --required-schemas=schemas/ --observed-resources=observed.yaml
```

The private subnet and security group now render (the public subnet stays absent)
and `status.networkingInfo` fills in. Flip the flags in `xr.yaml` to change which
resources appear.
