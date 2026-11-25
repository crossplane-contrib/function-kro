# Conditions Example

Composes a native Kubernetes `Deployment` and `Service`, and sets custom
conditions on the XR that describe them, with no cloud provider needed. Each
entry in the ResourceGraph's `status.conditions` is a `runtime.newCondition`
expression that returns a condition's type, status, reason, and message. See the
[Conditions Example](../README.md#conditions-example) in the top-level README
for the full walkthrough on a live cluster.

## Render it locally

Start function-kro from the repository root:

```shell
go run . --insecure --debug
```

Render the example:

```shell
cd example/conditions
crossplane render -r -x xr.yaml composition.yaml functions.yaml --required-schemas=schemas/
```

The workloads have not reported any observed state yet, so the XR's
`DeploymentAvailable` condition is `False` with the message `0 of 2 replicas
available`, and `AllResourcesReady` is `False`. `ServiceAddressAssigned` reads
the Service's cluster IP, which doesn't exist yet, so it's left off the XR.

Add `--observed-resources=observed.yaml`, which gives the Deployment an
`Available` condition and the Service a `clusterIP`:

```shell
crossplane render -r -x xr.yaml composition.yaml functions.yaml --required-schemas=schemas/ --observed-resources=observed.yaml
```

All three conditions are now `True`, and `ServiceAddressAssigned` has the
message `reachable at 10.96.0.42:8080`.
