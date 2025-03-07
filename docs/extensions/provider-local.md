# Local Provider Extension

The "local provider" extension is used to allow the usage of seed and shoot clusters which run entirely locally without any real infrastructure or cloud provider involved.
It implements Gardener's extension contract ([GEP-1](../proposals/01-extensibility.md)) and thus comprises several controllers and webhooks acting on resources in seed and shoot clusters.

The code is maintained in [`pkg/provider-local`](../../pkg/provider-local).

## Motivation

The motivation for maintaining such extension is the following:

- 🛡 Output Qualification: Run fast and cost-efficient end-to-end tests, locally and in CI systems (increased confidence ⛑ before merging pull requests)
- ⚙️ Development Experience: Develop Gardener entirely on a local machine without any external resources involved (improved costs 💰 and productivity 🚀)
- 🤝 Open Source: Quick and easy setup for a first evaluation of Gardener and a good basis for first contributions

## Current Limitations

The following enlists the current limitations of the implementation.
Please note that all of them are no technical limitations/blockers but simply advanced scenarios that we haven't had invested yet into.

1. Shoot clusters can only have multiple nodes, but inter-pod communication for pods on different nodes does not work.

   _We are using the [`networking-calico`](https://github.com/gardener/gardener-extension-networking-calico/) extension for the CNI plugin in shoot clusters, however, it doesn't seem to be configured correctly yet to support this scenario._

2. No owner TXT `DNSRecord`s (hence, no ["bad-case" control plane migration](../proposals/17-shoot-control-plane-migration-bad-case.md)).

   _In order to realize DNS (see [Implementation Details](#implementation-details) section below), the `/etc/hosts` file is manipulated. This does not work for TXT records. In the future, we could look into using [CoreDNS](https://coredns.io/) instead._

3. No load balancers for Shoot clusters.

   _We have not yet developed a `cloud-controller-manager` which could reconcile load balancer `Service`s in the shoot cluster. Hence, when the gardenlet's `ReversedVPN` feature gate is disabled then the `kube-system/vpn-shoot` `Service` must be manually patched (with `{"status": {"loadBalancer": {"ingress": [{"hostname": "vpn-shoot"}]}}}`) to make the reconciliation work._

4. Only one shoot cluster possible when gardenlet's `APIServerSNI` feature gate is disabled.

   _When [`APIServerSNI`](../proposals/08-shoot-apiserver-via-sni.md) is disabled then gardenlet uses load balancer `Service`s in order to expose the shoot clusters' `kube-apiserver`s. Typically, local Kubernetes clusters don't support this. In this case, the local extension uses the host IP to expose the `kube-apiserver`, however, this can only be done once._\
   _However, given that the `APIServerSNI` feature gate is deprecated and will be removed in the future (see [gardener/gardener#6007](https://github.com/gardener/gardener/pull/6007)), we will probably not invest into this._

## `ManagedSeed`s

It is possible to deploy [`ManagedSeed`s](../usage/managed_seed.md) with `provider-local` by first creating a [`Shoot` in the `garden` namespace](../../example/provider-local/managedseeds/shoot-managedseed.yaml) and then creating a referencing [`ManagedSeed` object](../../example/provider-local/managedseeds/managedseed.yaml).

> Please note that this is only supported by the [`Skaffold`-based setup](../deployment/getting_started_locally.md).

The corresponding e2e test can be run via

```bash
./hack/test-e2e-local.sh --label-filter "ManagedSeed"
```

### Implementation Details

The images locally built by `Skaffold` for the Gardener components which are deployed to this shoot cluster are managed by a container registry in the `registry` namespace in the kind cluster.
`provider-local` configures this registry as mirror for the shoot by mutating the `OperatingSystemConfig` and using the [default contract for extending the `containerd` configuration](../usage/custom-containerd-config.md).

In order to bootstrap a seed cluster, the `gardenlet` deploys `PersistentVolumeClaim`s and `Service`s of type `LoadBalancer`.
While storage is supported in shoot clusters by using the [`local-path-provisioner`](https://github.com/rancher/local-path-provisioner), load balancers are not supported yet.
However, `provider-local` runs a `Service` controller which specifically reconciles the seed-related `Service`s of type `LoadBalancer`.
This way, they get an IP and `gardenlet` can finish its bootstrapping process.
Note that these IPs are not reachable, however for the sake of developing `ManagedSeed`s this is sufficient for now.

Also, please note that the `provider-local` extension only gets deployed because of the `Always` deployment policy in its corresponding `ControllerRegistration` and because the DNS provider type of the seed is set to `local`.

## Implementation Details

This section contains information about how the respective controllers and webhooks in `provider-local` are implemented and what their purpose is.

### Bootstrapping

The Helm chart of the `provider-local` extension defined in its [`ControllerDeployment`](controllerregistration.md) contains a special deployment for a [CoreDNS](https://coredns.io/) instance in a `gardener-extension-provider-local-coredns` namespace in the seed cluster.

This CoreDNS instance is responsible for enabling the components running in the shoot clusters to be able to resolve the DNS names when they communicate with their `kube-apiserver`s.

It contains static configuration to resolve the DNS names based on `local.gardener.cloud` to either the `istio-ingressgateway.istio-ingress.svc` or the `kube-apiserver.<shoot-namespace>.svc` addresses (depending on whether the `--apiserver-sni-enabled` flag is set to `true` or `false`).

### Controllers

There are controllers for all resources in the `extensions.gardener.cloud/v1alpha1` API group except for `BackupBucket` and `BackupEntry`s.

#### `ControlPlane`

This controller is deploying the [local-path-provisioner](https://github.com/rancher/local-path-provisioner) as well as a related `StorageClass` in order to support `PersistentVolumeClaim`s in the local shoot cluster.
Additionally, it creates a few (currently unused) dummy secrets (CA, server and client certificate, basic auth credentials) for the sake of testing the secrets manager integration in the extensions library.

#### `DNSRecord`

This controller manipulates the `/etc/hosts` file and adds a new line for each `DNSRecord` it observes.
This enables accessing the shoot clusters from the respective machine, however, it also requires to run the extension with elevated privileges (`sudo`).

The `/etc/hosts` would be extended as follows:

```text
# Begin of gardener-extension-provider-local section
10.84.23.24 api.local.local.external.local.gardener.cloud
10.84.23.24 api.local.local.internal.local.gardener.cloud
...
# End of gardener-extension-provider-local section
```

#### `Infrastructure`

This controller generates a `NetworkPolicy` which allows the control plane pods (like `kube-apiserver`) to communicate with the worker machine pods (see [`Worker` section](#worker))).

In addition, it creates a `Service` named `vpn-shoot` which is only used in case the gardenlet's `ReversedVPN` feature gate is disabled.
This `Service` enables the `vpn-seed` containers in the `kube-apiserver` pods in the seed cluster to communicate with the `vpn-shoot` pod running in the shoot cluster.

#### `Network`

This controller is not implemented anymore. In the initial version of `provider-local`, there was a `Network` controller deploying [kindnetd](https://github.com/kubernetes-sigs/kind/blob/main/images/kindnetd/README.md) (see https://github.com/gardener/gardener/tree/v1.44.1/pkg/provider-local/controller/network).
However, we decided to drop it because this setup prevented us from using `NetworkPolicy`s (kindnetd does not ship a `NetworkPolicy` controller).
In addition, we had issues with shoot clusters having more than one node (hence, we couldn't support rolling updates, see https://github.com/gardener/gardener/pull/5666/commits/491b3cd16e40e5c20ef02367fda93a34ff9465eb).

#### `OperatingSystemConfig`

This controller leverages the standard [`oscommon` library](../../extensions/pkg/controller/operatingsystemconfig/oscommon) in order to render a simple cloud-init template which can later be executed by the shoot worker nodes.

The shoot worker nodes are `Pod`s with a container based on the `kindest/node` image. This is maintained in https://github.com/gardener/machine-controller-manager-provider-local/tree/master/node and has a special `run-userdata` systemd service which executes the cloud-init generated earlier by the `OperatingSystemConfig` controller.

#### `Worker`

This controller leverages the standard [generic `Worker` actuator](../../extensions/pkg/controller/worker/genericactuator) in order to deploy the [`machine-controller-manager`](https://github.com/gardener/machine-controller-manager) as well as the [`machine-controller-manager-provider-local`](https://github.com/gardener/machine-controller-manager-provider-local).

Additionally, it generates the [`MachineClass`es](https://github.com/gardener/machine-controller-manager-provider-local/blob/master/kubernetes/machine-class.yaml) and the `MachineDeployment`s based on the specification of the `Worker` resources.

#### `Ingress`

Gardenlet creates a wildcard DNS record for the Seed's ingress domain pointing to the `nginx-ingress-controller`'s LoadBalancer.
This domain is commonly used by all `Ingress` objects created in the Seed for Seed and Shoot components.
However, provider-local implements the `DNSRecord` extension API by writing the DNS record to `/etc/hosts`, which doesn't support wildcard entries.
To make `Ingress` domains resolvable on the host, this controller reconciles all `Ingresses` and creates `DNSRecords` of type `local` for each host included in `spec.rules`.

#### `Service`

This controller reconciles `Services` of type `LoadBalancer` in the local `Seed` cluster.
Since the local Kubernetes clusters used as Seed clusters typically don't support such services, this controller sets the `.status.ingress.loadBalancer.ip[0]` to the IP of the host.
It makes important LoadBalancer Services (e.g. `istio-ingress/istio-ingressgateway` and `garden/nginx-ingress-controller`) available to the host by setting `spec.ports[].nodePort` to well-known ports that are mapped to `hostPorts` in the kind cluster configuration.

If the `--apiserver-sni-enabled` flag is set to `true` (default), `istio-ingress/istio-ingressgateway` is set to be exposed on `nodePort` `30433` by this controller. Otherwise, the `kube-apiserver` `Service` in the shoot namespaces in the seed cluster needs to be patched to be exposed on `30443` by the [Control Plane Exposure Webhook](#control-plane-exposure).

#### ETCD Backups
This controller reconciles the `BackuBucket` and `BackupEntry` of the shoot allowing the `etcd-backup-restore` to create and copy backups using the `local` provider functionality. The backups are stored on the host file system. This is achieved by mounting that directory to the `etcd-backup-restore` container.

#### Health Checks

The health check controller leverages the [health check library](healthcheck-library.md) in order to

- check the health of the `ManagedResource/extension-controlplane-shoot-webhooks` and populate the `SystemComponentsHealthy` condition in the `ControlPlane` resource.
- check the health of the `ManagedResource/extension-networking-local` and populate the `SystemComponentsHealthy` condition in the `Network` resource.
- check the health of the `ManagedResource/extension-worker-mcm-shoot` and populate the `SystemComponentsHealthy` condition in the `Worker` resource.
- check the health of the `Deployment/machine-controller-manager` and populate the `ControlPlaneHealthy` condition in the `Worker` resource.
- check the health of the `Node`s and populate the `EveryNodeReady` condition in the `Worker` resource.

### Webhooks

#### Control Plane

This webhook reacts on the `OperatingSystemConfig` containing the configuration of the kubelet and sets the `failSwapOn` to `false` (independent of what is configured in the `Shoot` spec) ([ref](https://github.com/kubernetes-sigs/kind/blob/b6bc112522651d98c81823df56b7afa511459a3b/site/content/docs/design/node-image.md#design)).

#### Control Plane Exposure

This webhook reacts on the `kube-apiserver` `Service` in shoot namespaces in the seed in case the gardenlet's `APIServerSNI` feature gate is disabled.
It sets the `nodePort` to `30443` to enable communication from the host (this requires a port mapping to work when creating the local cluster).

#### DNS Config

This webhook reacts on events for the `dependency-watchdog-probe` `Deployment`, the `prometheus` `StatefulSet` as well as on events for `Pod`s created when the `machine-controller-manager` reconciles `Machine`s.
All these pods need to be able to resolve the DNS names for shoot clusters.
It sets the `.spec.dnsPolicy=None` and `.spec.dnsConfig.nameServers` to the cluster IP of the `coredns` `Service` created in the `gardener-extension-provider-local-coredns` namespaces so that these pods can resolve the DNS records for shoot clusters (see the [Bootstrapping section](#bootstrapping) for more details).

#### Node

This webhook reacts on updates to `nodes/status` in both seed and shoot clusters and sets the `.status.{allocatable,capacity}.cpu="100"` and `.status.{allocatable,capacity}.memory="100Gi"` fields.

Background: Typically, the `.status.{capacity,allocatable}` values are determined by the resources configured for the Docker daemon (see for example [this](https://docs.docker.com/desktop/mac/#resources) for Mac).
Since many of the `Pod`s deployed by Gardener have quite high `.spec.resources.requests`, the `Node`s easily get filled up and only a few `Pod`s can be scheduled (even if they barely consume any of their reserved resources).
In order to improve the user experience, on startup/leader election the provider-local extension submits an empty patch which triggers the "node webhook" (see below section) for the seed cluster.
The webhook will increase the capacity of the `Node`s to allow all `Pod`s to be scheduled.
For the shoot clusters, this empty patch trigger is not needed since the `MutatingWebhookConfiguration` is reconciled by the `ControlPlane` controller and exists before the `Node` object gets registered.

#### Shoot

This webhook reacts on the `ConfigMap` used by the `kube-proxy` and sets the `maxPerCore` field to `0` since other values don't work well in conjunction with the `kindest/node` image which is used as base for the shoot worker machine pods ([ref](https://github.com/kubernetes-sigs/kind/blob/fa7d86470f4c0e924fc4c2e767ec8491c45f4304/pkg/cluster/internal/kubeadm/config.go#L283-L285)).

## Future Work

Future work could mostly focus on resolving above listed [limitations](#limitations), i.e.,

- Implement a `cloud-controller-manager` and deploy it via the [`ControlPlane` controller](#controlplane).
- Properly implement `.spec.machineTypes` in the `CloudProfile`s (i.e., configure `.spec.resources` properly for the created shoot worker machine pods).
