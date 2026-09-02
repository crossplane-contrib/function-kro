# Context Example

Composes a core Kubernetes `ConfigMap`, showing function-kro handles context.

## Render it locally 

Start function-kro from the repository root:

```shell
go run . --insecure --debug
```

Render the example:

```shell
cd example/context
crossplane render -r -x xr.yaml composition.yaml functions.yaml --required-schemas=schemas/ --context-files=apiextensions.crossplane.io/environment=environment-context.json
```

The ConfigMap has no dependencies, so this renders the complete result: the
composite plus the ConfigMap, with `foo` taken from the context.
