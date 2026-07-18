# External Reference Example

Reads existing cluster resources with `externalRef`: one ConfigMap by name for
platform configuration, and a set of ConfigMaps by label selector whose data is
aggregated into status with CEL list functions. See the
[External Reference Example](../README.md#external-reference-example) in the
top-level README for the full walkthrough on a live cluster.

## Render it locally

External references resolve from real cluster resources, which `crossplane render`
mocks with `--required-resources`. The example includes the same ConfigMaps
(`configmap.yaml`, `subscribers.yaml`) it applies on a cluster.

Start function-kro from the repository root:

```shell
go run . --insecure --debug
```

Render the example, passing the external ConfigMaps:

```shell
cd example/externalref
crossplane render -r -x xr.yaml composition.yaml functions.yaml \
  --required-schemas=schemas/ \
  --required-resources=configmap.yaml \
  --required-resources=subscribers.yaml
```

Because the ConfigMaps are supplied, the status aggregates fully: `platformConfig`
carries the region, environment, and CIDR from the single ConfigMap, and
`notifications` reports `subscriberCount`, the sorted `teams`, and `hasOncall`
computed across all subscriber ConfigMaps. The VPC renders with configuration
sourced from the ConfigMap; the subnet and security group depend on the VPC's ID
and reconcile on a cluster.
