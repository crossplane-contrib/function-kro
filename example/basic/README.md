# Basic Example

Composes a VPC, three subnets, and a security group, where the subnets and
security group depend on the VPC's ID. See the
[Basic Example](../README.md#basic-example) in the top-level README for the full
walkthrough on a live cluster.

## Render it locally

Start function-kro from the repository root:

```shell
go run . --insecure --debug
```

Render the example:

```shell
cd example/basic
crossplane render -r -x xr.yaml composition.yaml functions.yaml --required-schemas=schemas/
```

This renders the composite and the VPC. The subnets and security group read
`${vpc.status.atProvider.id}`, which is empty until a provider creates the VPC,
so render defers them.

Add `--observed-resources=observed.yaml` to supply provider-assigned IDs, as a
cluster would:

```shell
crossplane render -r -x xr.yaml composition.yaml functions.yaml --required-schemas=schemas/ --observed-resources=observed.yaml
```

Now the whole graph renders and `status.networkingInfo` fills in with the VPC,
subnet, and security group IDs.
