# `function-kro` Examples

These examples demonstrate the key capabilities of `function-kro`, from basic resource
dependencies through conditional creation, readiness checks, external references, and
collections.

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

## Core example

This example demonstrates support for core Kubernetes resources. A Secret is always
created both in Crossplane <2.2.0 without required_schemas capability and in Crossplane >2.2.0
where that capability is supported.

Create the `CoolSecret` XRD and composition:
```shell
kubectl apply -f core/xrd.yaml
kubectl apply -f core/composition.yaml
```

Create a `CoolSecret` instance:
```shell
kubectl apply -f core/xr.yaml
```

Watch the composed resources being created and observe how readiness propagates as each
resource satisfies its `readyWhen` conditions:
```shell
crossplane beta trace -w coolsecret.core.example.crossplane.io/cool-secret
```

### Clean-up

```shell
kubectl delete -f kubectl apply -f core/xr.yaml
kubectl get managed
```

```shell
kubectl delete -f core/composition.yaml
kubectl delete -f core/xrd.yaml
```
