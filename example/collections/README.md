# Collections Example

Uses `forEach` to create one subnet per availability zone in the XR spec, then
collects their IDs into status with `map()`. See the
[Collections Example](../README.md#collections-example) in the top-level README
for the full walkthrough on a live cluster.

## Render it locally

Start function-kro from the repository root:

```shell
go run . --insecure --debug
```

Render the example:

```shell
cd example/collections
crossplane render -r -x xr.yaml composition.yaml functions.yaml --required-schemas=schemas/
```

This renders the composite and the VPC. The subnet collection reads the VPC's ID
for each subnet's `vpcId`, so render defers the whole collection until the VPC
has an ID.

Add `--observed-resources=observed.yaml` to supply that ID:

```shell
crossplane render -r -x xr.yaml composition.yaml functions.yaml --required-schemas=schemas/ --observed-resources=observed.yaml
```

The collection now expands into one subnet per availability zone, and
`status.networkingInfo.subnetIDs` collects every subnet's ID.
