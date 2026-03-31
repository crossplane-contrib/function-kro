# `function-kro` Examples

These examples demonstrate the key capabilities of `function-kro`, from basic resource
dependencies through conditional creation, readiness checks, external references,
collections, field omission, and collection size limits.

## Pre-Requisites

Create a Kubernetes cluster, e.g. with `kind`:
```shell
kind create cluster
```

Install Crossplane:
```shell
helm repo add crossplane-stable https://charts.crossplane.io/stable
helm repo update

helm install crossplane --namespace crossplane-system --create-namespace crossplane-stable/crossplane --set args='{"--debug","--circuit-breaker-burst=500.0", "--circuit-breaker-refill-rate=5.0", "--circuit-breaker-cooldown=1m"}'
```

### Install Extensions

Install the required functions and providers:
```shell
kubectl apply -f drc.yaml
kubectl apply -f extensions.yaml
```

The DeploymentRuntimeConfig (`drc.yaml`) enables debug logging, the alpha `omit()` CEL
function (`--feature-gates=CELOmitFunction=true`), and a low collection size limit
(`--rgd-max-collection-size=5`) for the collection limits example.

Wait for the functions and providers to be installed and healthy:
```shell
kubectl get pkg
```

### Configure AWS Credentials
```shell
AWS_PROFILE=default && echo -e "[default]\naws_access_key_id = $(aws configure get aws_access_key_id --profile $AWS_PROFILE)\naws_secret_access_key = $(aws configure get aws_secret_access_key --profile $AWS_PROFILE)" > aws-credentials.txt

kubectl create secret generic aws-secret -n crossplane-system --from-file=creds=./aws-credentials.txt
kubectl apply -f providerconfig.yaml
```

## Basic Example

This example demonstrates the fundamental dependency pattern and DAG approach of
function-kro. It creates a VPC with three subnets across availability zones and
a security group. Each resource depends on the VPC ID, showing how
`function-kro` resolves static expressions (from the XR spec) and dynamic
expressions (from composed resource status).

Create the `NetworkingStack` XRD and composition:
```shell
kubectl apply -f basic/xrd.yaml
kubectl apply -f basic/composition.yaml
```

Create a `NetworkingStack` instance:
```shell
kubectl apply -f basic/xr.yaml
```

Watch the composed resources being created and the status being updated:
```shell
crossplane beta trace -w networkingstack.example.crossplane.io/cool-network
```

We can see the aggregated networking info in the XR status, which includes the VPC ID,
subnet IDs, and security group ID:
```shell
kubectl get NetworkingStack/cool-network -o json | jq '.status.networkingInfo'
```

### Clean-up

Clean up all the resources when you are done:
```shell
kubectl delete -f basic/xr.yaml
kubectl get managed
```

```shell
kubectl delete -f basic/composition.yaml
kubectl delete -f basic/xrd.yaml
```

## Conditionals Example

This example demonstrates conditional resource creation using `includeWhen`. A VPC and
private subnet are always created, but a public subnet and security group are only included
when their respective boolean flags are enabled in the XR spec (`enablePublicSubnet`,
`enableSecurityGroup`).

Create the `NetworkingStack` XRD and composition:
```shell
kubectl apply -f conditionals/xrd.yaml
kubectl apply -f conditionals/composition.yaml
```

Create a `NetworkingStack` instance:
```shell
kubectl apply -f conditionals/xr.yaml
```

Watch the composed resources being created and note that conditional resources are only
included when their flags are set to `true`:
```shell
crossplane beta trace -w networkingstack.conditionals.example.crossplane.io/cool-network
```

We can see the status reflects which resources were actually created:
```shell
kubectl get networkingstack.conditionals.example.crossplane.io/cool-network -o json | jq '.status.networkingInfo'
```

### Clean-up

```shell
kubectl delete -f conditionals/xr.yaml
kubectl get managed
```

```shell
kubectl delete -f conditionals/composition.yaml
kubectl delete -f conditionals/xrd.yaml
```

## Readiness Example

This example demonstrates custom readiness conditions using `readyWhen`. Each resource
defines CEL expressions that determine when it is considered ready. For example, the VPC uses
safe field access (`?.` operator) to check that `status.atProvider.id` has a value, and the
security group checks for a `Ready=True` condition.

This example does not use `function-auto-ready` in its pipeline because we are
exclusively using `readyWhen` statements for each resource. Crossplane will
automatically detect that the parent XR is ready when all composed resources
have their ready state set to true in the function pipeline via the `readyWhen`
statements.

Create the `NetworkingStack` XRD and composition:
```shell
kubectl apply -f readiness/xrd.yaml
kubectl apply -f readiness/composition.yaml
```

Create a `NetworkingStack` instance:
```shell
kubectl apply -f readiness/xr.yaml
```

Watch the composed resources being created and observe how readiness propagates as each
resource satisfies its `readyWhen` conditions:
```shell
crossplane beta trace -w networkingstack.readiness.example.crossplane.io/cool-network
```

We can see the aggregated status once all resources are ready:
```shell
kubectl get networkingstack.readiness.example.crossplane.io/cool-network -o json | jq '.status.networkingInfo'
```

### Clean-up

```shell
kubectl delete -f readiness/xr.yaml
kubectl get managed
```

```shell
kubectl delete -f readiness/composition.yaml
kubectl delete -f readiness/xrd.yaml
```

## External Reference Example

This example demonstrates referencing existing resources using `externalRef`. Instead of
creating all resources from scratch, the composition references an existing ConfigMap that
provides platform configuration (region, CIDR block, environment). The VPC, subnet, and
security group all source their configuration from this external ConfigMap rather than
directly from the XR spec.

Create the `NetworkingStack` XRD, composition, and the external ConfigMap:
```shell
kubectl apply -f externalref/xrd.yaml
kubectl apply -f externalref/composition.yaml
kubectl apply -f externalref/configmap.yaml
```

Create a `NetworkingStack` instance:
```shell
kubectl apply -f externalref/xr.yaml
```

Watch the composed resources being created with configuration sourced from the external
ConfigMap:
```shell
crossplane beta trace -w networkingstack.externalref.example.crossplane.io/cool-network
```

We can see the networking info along with the platform configuration pulled from the
external ConfigMap:
```shell
kubectl get networkingstack.externalref.example.crossplane.io/cool-network -o json | jq '.status.networkingInfo,.status.platformConfig'
```

### Clean-up

```shell
kubectl delete -f externalref/xr.yaml
kubectl get managed
```

```shell
kubectl delete -f externalref/configmap.yaml
kubectl delete -f externalref/composition.yaml
kubectl delete -f externalref/xrd.yaml
```

## Collections Example

This example demonstrates iterating over array inputs using `forEach` to dynamically create
multiple resources. A single "subnets" resource definition with `forEach` iterates over an
array of availability zones from the XR spec, creating one subnet per entry. The status
uses `map()` to collect all subnet IDs into an array.

Create the `NetworkingStack` XRD and composition:
```shell
kubectl apply -f collections/xrd.yaml
kubectl apply -f collections/composition.yaml
```

Create a `NetworkingStack` instance:
```shell
kubectl apply -f collections/xr.yaml
```

Watch the composed resources being created and note that a subnet is created for each
availability zone in the input array:
```shell
crossplane beta trace -w networkingstack.collections.example.crossplane.io/cool-network
```

We can see the array of subnet IDs collected from all dynamically created subnets:
```shell
kubectl get networkingstack.collections.example.crossplane.io/cool-network -o json | jq '.status.networkingInfo'
```

### Clean-up

```shell
kubectl delete -f collections/xr.yaml
kubectl get managed
```

```shell
kubectl delete -f collections/composition.yaml
kubectl delete -f collections/xrd.yaml
```

## Omit Example

This example demonstrates conditional field removal using `omit()`, an alpha CEL function
that removes a field from the desired state entirely rather than setting it to a zero value.
This distinction matters for Server-Side Apply (SSA) — when a field is absent from the
desired state, Crossplane does not claim ownership of it, allowing other controllers or
provider defaults to manage it independently.

The composition creates a VPC where `enableDnsHostnames` is controlled by the XR spec. When
the flag is false, `omit()` removes the field from the desired VPC instead of setting it to
`false`. When the flag is true, the field is present and set to `true`.

Create the `NetworkingStack` XRD and composition:
```shell
kubectl apply -f omit/xrd.yaml
kubectl apply -f omit/composition.yaml
```

Create a `NetworkingStack` instance with DNS hostnames disabled:
```shell
kubectl apply -f omit/xr.yaml
```

Watch the composed resources being created:
```shell
crossplane beta trace -w networkingstack.omit.example.crossplane.io/cool-network
```

We can inspect the composed VPC's `forProvider` spec and confirm that `enableDnsHostnames`
is absent — `omit()` removed it entirely rather than setting it to `false`:
```shell
kubectl get vpc.ec2.aws.m.upbound.io -l crossplane.io/composite=cool-network -o json | jq '.items[0].spec.forProvider'
```

Now patch the XR to enable DNS hostnames and stop the `omit()` function from
omitting this value on the composed VPC:
```shell
kubectl patch networkingstack.omit.example.crossplane.io/cool-network --type merge -p '{"spec":{"enableDnsHostnames":true}}'
```

Inspect the VPC again to see `enableDnsHostnames` appear in `forProvider`:
```shell
kubectl get vpc.ec2.aws.m.upbound.io -l crossplane.io/composite=cool-network -o json | jq '.items[0].spec.forProvider'
```

### Clean-up

```shell
kubectl delete -f omit/xr.yaml
kubectl get managed
```

```shell
kubectl delete -f omit/composition.yaml
kubectl delete -f omit/xrd.yaml
```

## Collection Limits Example

This example demonstrates the collection size limit guardrail that prevents `forEach`
expansions from creating too many resources. The function is configured with
`--rgd-max-collection-size=5` via the shared DeploymentRuntimeConfig, so any collection
that expands beyond 5 items produces a fatal error.

The composition creates subnets dynamically via `forEach` over an array of availability
zones from the XR spec. We start with 3 availability zones (under the limit), then expand
to 6 to trigger the guardrail.

Create the `NetworkingStack` XRD and composition:
```shell
kubectl apply -f collection-limits/xrd.yaml
kubectl apply -f collection-limits/composition.yaml
```

Create a `NetworkingStack` instance with 3 availability zones:
```shell
kubectl apply -f collection-limits/xr.yaml
```

Watch the composed resources being created — 3 subnets are within the limit of 5:
```shell
crossplane beta trace -w networkingstack.collectionlimits.example.crossplane.io/cool-network
```

We can see the array of subnet IDs collected from all dynamically created subnets:
```shell
kubectl get networkingstack.collectionlimits.example.crossplane.io/cool-network -o json | jq '.status.networkingInfo'
```

Now apply the XR with 6 availability zones, exceeding the collection size limit:
```shell
kubectl apply -f collection-limits/xr-exceed.yaml
```

Watch the trace to see the fatal error indicating the collection size was exceeded:
```shell
crossplane beta trace networkingstack.collectionlimits.example.crossplane.io/cool-network -o wide
```

### Clean-up

```shell
kubectl delete -f collection-limits/xr.yaml
kubectl get managed
```

```shell
kubectl delete -f collection-limits/composition.yaml
kubectl delete -f collection-limits/xrd.yaml
```
