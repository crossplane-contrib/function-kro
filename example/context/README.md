# Context Example

Composes an app's `Deployment` and a `ConfigMap` of its settings, with the
replica count, environment name, and log level taken from platform-wide
defaults in the function context. An earlier pipeline step,
function-environment-configs, reads the `platform-defaults` EnvironmentConfig
and writes its data to the context key `apiextensions.crossplane.io/environment`.
The templates read it as
`${schema.context["apiextensions.crossplane.io/environment"].replicas}`; keys
that contain dots or slashes need this bracket syntax. See the
[Context example](../README.md#context-example) in the top-level README for the
full walkthrough on a live cluster.

If the context key's whole value is null, the resources that read it are held
back until the key has a value. A null value nested under the key, such as
`logLevel: null`, is rendered as `null` into the composed resource.

## Render it locally

Start function-kro from the repository root:

```shell
go run . --insecure --debug
```

Render the example. `crossplane render` pulls function-environment-configs and
passes it the EnvironmentConfig with `--required-resources`:

```shell
cd example/context
crossplane render -r -x xr.yaml composition.yaml functions.yaml --required-schemas=schemas/ --required-resources=environmentconfig.yaml
```

Both resources depend only on the XR and the context, so this renders the
complete result: the Deployment with `replicas: 2`, and the ConfigMap with
`ENVIRONMENT: production` and `LOG_LEVEL: info`, all from the EnvironmentConfig.
