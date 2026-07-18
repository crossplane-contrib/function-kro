# Collection Limits Example

A `forEach` subnet collection guarded by `--rgd-max-collection-size=5`: expanding
past five items is a fatal error. `xr.yaml` stays under the limit with three
zones; `xr-exceed.yaml` crosses it with six. See the
[Collection Limits Example](../README.md#collection-limits-example) in the
top-level README for the full walkthrough on a live cluster.

## Render it locally

The limit is a function flag, so start function-kro with it set from the
repository root:

```shell
go run . --insecure --debug --rgd-max-collection-size=5
```

Render the under-limit XR:

```shell
cd example/collection-limits
crossplane render -r -x xr.yaml composition.yaml functions.yaml --required-schemas=schemas/
```

This renders the composite and the VPC. The subnet collection reads the VPC's ID,
so render defers it until the VPC has one. Add `--observed-resources=observed.yaml`
to supply that ID and expand the three subnets:

```shell
crossplane render -r -x xr.yaml composition.yaml functions.yaml --required-schemas=schemas/ --observed-resources=observed.yaml
```

Rendering `xr-exceed.yaml` with the same observed VPC expands the collection to
six subnets, past the limit, and fails:

```shell
crossplane render -r -x xr-exceed.yaml composition.yaml functions.yaml --required-schemas=schemas/ --observed-resources=observed.yaml
```

```
collection size of 6 is over the maximum collection size of 5
```
