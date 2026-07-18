# Kubernetes App Example

Composes a native Kubernetes `Deployment` and `Service` and aggregates their
observed state back into the XR status, with no cloud provider needed. See the
[Kubernetes App Example](../README.md#kubernetes-app-example) in the top-level
README for the full walkthrough on a live cluster.

## Render it locally

Start function-kro from the repository root:

```shell
go run . --insecure --debug
```

Render the example:

```shell
cd example/app
crossplane render -r -x xr.yaml composition.yaml functions.yaml --required-schemas=schemas/
```

The Deployment and Service depend only on the XR spec, so both render right away.
The aggregated `status.replicas` and `status.address` stay empty because the
workloads have not reported any observed state yet.

Add `--observed-resources=observed.yaml`, which gives the Deployment an
`availableReplicas` count and the Service a `clusterIP`:

```shell
crossplane render -r -x xr.yaml composition.yaml functions.yaml --required-schemas=schemas/ --observed-resources=observed.yaml
```

`status.replicas` now reads `2` and `status.address` holds the cluster IP. The
replica count also exercises function-kro's integer handling. Crossplane delivers
observed numbers as float64, and function-kro normalizes whole numbers back to
integers.
