# Gardenlet

Gardener is implemented using the [operator pattern](https://kubernetes.io/docs/concepts/extend-kubernetes/operator/):
It uses custom controllers that act on our own custom resources,
and apply Kubernetes principles to manage clusters instead of containers.
Following this analogy, you can recognize components of the Gardener architecture
as well-known Kubernetes components, for example, shoot clusters can be compared with pods,
and seed clusters can be seen as worker nodes.

The following Gardener components play a similar role as the corresponding components
in the Kubernetes architecture:

| Gardener Component | Kubernetes Component |
|:---|:---|
| `gardener-apiserver` | `kube-apiserver` |
| `gardener-controller-manager` | `kube-controller-manager` |
| `gardener-scheduler` | `kube-scheduler` |
| `gardenlet` | `kubelet` |

Similar to how the `kube-scheduler` of Kubernetes finds an appropriate node
for newly created pods, the `gardener-scheduler` of Gardener finds an appropriate seed cluster
to host the control plane for newly ordered clusters.
By providing multiple seed clusters for a region or provider, and distributing the workload,
Gardener also reduces the blast radius of potential issues.

Kubernetes runs a primary "agent" on every node, the kubelet,
which is responsible for managing pods and containers on its particular node.
Decentralizing the responsibility to the kubelet has the advantage that the overall system
is scalable. Gardener achieves the same for cluster management by using a **gardenlet**
as primary "agent" on every seed cluster, and is only responsible for shoot clusters
located in its particular seed cluster:

![Counterparts in the Gardener Architecture and the Kubernetes Architecture](images/gardenlet-architecture-similarities.png)

The `gardener-controller-manager` has control loops to manage resources of the Gardener API. However, instead of letting the `gardener-controller-manager` talk directly to seed clusters or shoot clusters, the responsibility isn’t only delegated to the gardenlet, but also managed using a reversed control flow: It's up to the gardenlet to contact the Gardener API server, for example, to share a status for its managed seed clusters.

Reversing the control flow allows placing seed clusters or shoot clusters behind firewalls without the necessity of direct access via VPN tunnels anymore.

![Reversed Control Flow Using a Gardenlet](images/gardenlet-architecture-detailed.png)

## TLS Bootstrapping

Kubernetes doesn’t manage worker nodes itself, and it’s also not
responsible for the lifecycle of the kubelet running on the workers.
Similarly, Gardener doesn’t manage seed clusters itself,
so Gardener is also not responsible for the lifecycle of the gardenlet running on the seeds.
As a consequence, both the gardenlet and the kubelet need to prepare
a trusted connection to the Gardener API server
and the Kubernetes API server correspondingly.

To prepare a trusted connection between the gardenlet
and the Gardener API server, the gardenlet initializes
a bootstrapping process after you deployed it into your seed clusters:

1. The gardenlet starts up with a bootstrap `kubeconfig`
   having a bootstrap token that allows to create `CertificateSigningRequest` (CSR) resources.

2. After the CSR is signed, the gardenlet downloads
   the created client certificate, creates a new `kubeconfig` with it,
   and stores it inside a `Secret` in the seed cluster.

3. The gardenlet deletes the bootstrap `kubeconfig` secret,
    and starts up with its new `kubeconfig`.

4. The gardenlet starts normal operation.

The `gardener-controller-manager` runs a control loop
that automatically signs CSRs created by gardenlets.

> The gardenlet bootstrapping process is based on the
> kubelet bootstrapping process. More information:
> [Kubelet's TLS bootstrapping](https://kubernetes.io/docs/reference/access-authn-authz/kubelet-tls-bootstrapping/).

If you don't want to run this bootstrap process you can create
a `kubeconfig` pointing to the garden cluster for the gardenlet yourself,
and use field `gardenClientConnection.kubeconfig` in the
gardenlet configuration to share it with the gardenlet.

## Gardenlet Certificate Rotation

The certificate used to authenticate the gardenlet against the API server
has a certain validity based on the configuration of the garden cluster
(`--cluster-signing-duration` flag of the `kube-controller-manager` (default `1y`)). 

> If your garden cluster is of at least Kubernetes v1.22 then you can also configure the validity for the client certificate by specifying `.gardenClientConnection.kubeconfigValidity.validity` in the gardenlet's component configuration.
> Note that changing this value will only take effect when the kubeconfig is rotated again (it is not picked up immediately).
> The minimum validity is `10m` (that's what is enforced by the `CertificateSigningRequest` API in Kubernetes which is used by gardenlet).

By default, after about 70-90% of the validity expired, the gardenlet tries to automatically replace
the current certificate with a new one (certificate rotation).

> You can change these boundaries by specifying `.gardenClientConnection.kubeconfigValidity.autoRotationJitterPercentage{Min,Max}` in the gardenlet's component configuration.

To use certificate rotation, you need to specify the secret to store
the `kubeconfig` with the rotated certificate in field
`.gardenClientConnection.kubeconfigSecret` of the
gardenlet [component configuration](#component-configuration).

### Rotate certificates using bootstrap `kubeconfig`

If the gardenlet created the certificate during the initial TLS Bootstrapping
using the Bootstrap `kubeconfig`, certificates can be rotated automatically.
The same control loop in the `gardener-controller-manager` that signs
the CSRs during the initial TLS Bootstrapping also automatically signs
the CSR during a certificate rotation.

ℹ️ You can trigger an immediate renewal by annotating the `Secret` in the seed
cluster stated in the `.gardenClientConnection.kubeconfigSecret` field with
`gardener.cloud/operation=renew` and restarting the gardenlet. After it booted
up again, gardenlet will issue a new certificate independent of the remaining
validity  of the existing one.

### Rotate Certificate Using Custom `kubeconfig`

When trying to rotate a custom certificate that wasn’t created by gardenlet
as part of the TLS Bootstrap, the x509 certificate's `Subject` field
needs to conform to the following:
  - the Common Name (CN) is prefixed with `gardener.cloud:system:seed:`
  - the Organization (O) equals `gardener.cloud:system:seeds`

Otherwise, the `gardener-controller-manager` doesn’t automatically
sign the CSR.
In this case, an external component or user needs to approve the CSR manually,
for example, using command  `kubectl certificate approve  seed-csr-<...>`).
If that doesn’t happen within 15 minutes,
the gardenlet repeats the process and creates another CSR.

## Configuring the Seed to work with

The Gardenlet works with a single seed, which must be configured in the
`GardenletConfiguration` under `.seedConfig`. This must be a copy of the
`Seed` resource, for example (see `example/20-componentconfig-gardenlet.yaml`
for a more complete example):

```yaml
apiVersion: gardenlet.config.gardener.cloud/v1alpha1
kind: GardenletConfiguration
seedConfig:
  metadata:
    name: my-seed
  spec:
    provider:
      type: aws
    # ...
    secretRef:
      name: my-seed-secret
      namespace: garden
```

When using `make start-gardenlet`, the corresponding script will automatically
fetch the seed cluster's `kubeconfig` based on the `seedConfig.spec.secretRef`
and set the environment accordingly.

On startup, gardenlet registers a `Seed` resource using the given template
in `seedConfig` if it's not present already.

## Component Configuration

In the component configuration for the gardenlet, it’s possible to define:

* settings for the Kubernetes clients interacting with the various clusters
* settings for the control loops inside the gardenlet
* settings for leader election and log levels, feature gates, and seed selection or seed configuration.

More information: [Example Gardenlet Component Configuration](../../example/20-componentconfig-gardenlet.yaml).

## Heartbeats

Similar to how Kubernetes uses `Lease` objects for node heart beats
(see [KEP](https://github.com/kubernetes/enhancements/blob/master/keps/sig-node/589-efficient-node-heartbeats/README.md)),
the gardenlet is using `Lease` objects for heart beats of the seed cluster.
Every two seconds, the gardenlet checks that the seed cluster's `/healthz`
endpoint returns HTTP status code 200.
If that is the case, the gardenlet renews the lease in the Garden cluster in the `gardener-system-seed-lease` namespace and updates
the `GardenletReady` condition in the `status.conditions` field of the `Seed` resource, see also [this section](#lease-reconciler).

Similarly to the `node-lifecycle-controller` inside the `kube-controller-manager`,
the `gardener-controller-manager` features a `seed-lifecycle-controller` that sets
the `GardenletReady` condition to `Unknown` in case the gardenlet fails to renew the lease.
As a consequence, the `gardener-scheduler` doesn’t consider this seed cluster for newly created shoot clusters anymore.

### `/healthz` Endpoint

The gardenlet includes an HTTP server that serves a `/healthz` endpoint.
It’s used as a liveness probe in the `Deployment` of the gardenlet.
If the gardenlet fails to renew its lease
then the endpoint returns `500 Internal Server Error`, otherwise it returns `200 OK`.

Please note that the `/healthz` only indicates whether the gardenlet
could successfully probe the Seed's API server and renew the lease with
the Garden cluster.
It does *not* show that the Gardener extension API server (with the Gardener resource groups)
is available.
However, the Gardenlet is designed to withstand such connection outages and
retries until the connection is reestablished.

## Control Loops

The gardenlet consists out of several controllers which are now described in more detail.

### `BackupEntry` Controller

The `BackupEntry` controller reconciles those `core.gardener.cloud/v1beta1.BackupEntry` resources whose `.spec.seedName` value is equal to the name of a `Seed` the respective gardenlet is responsible for.
Those resources are created by the `Shoot` controller (only if backup is enabled for the respective `Seed`) and there is exactly one `BackupEntry` per `Shoot`.

The controller creates an `extensions.gardener.cloud/v1alpha1.BackupEntry` resource (non-namespaced) in the seed cluster and waits until the responsible extension controller reconciled it (see [this](../extensions/backupentry.md) for more details).
The status is populated in the `.status.lastOperation` field.

The `core.gardener.cloud/v1beta1.BackupEntry` resource has an owner reference pointing to the corresponding `Shoot`.
Hence, if the `Shoot` is deleted, also the `BackupEntry` resource gets deleted.
In this case, the controller deletes the `extensions.gardener.cloud/v1alpha1.BackupEntry` resource in the seed cluster and waits until the responsible extension controller has deleted it.
Afterwards, the finalizer of the `core.gardener.cloud/v1beta1.BackupEntry` resource is released so that it finally disappears from the system.

#### Keep Backup for Deleted Shoots

In some scenarios it might be beneficial to not immediately delete the `BackupEntry`s (and with them, the etcd backup) for deleted `Shoot`s.

In this case you can configure the `.controllers.backupEntry.deletionGracePeriodHours` field in the component configuration of the gardenlet.
For example, if you set it to `48`, then the `BackupEntry`s for deleted `Shoot`s will only be deleted `48` hours after the `Shoot` was deleted.

Additionally, you can limit the [shoot purposes](../usage/shoot_purposes.md) for which this applies by setting `.controllers.backupEntry.deletionGracePeriodShootPurposes[]`.
For example, if you set it to `[production]` then only the `BackupEntry`s for `Shoot`s with `.spec.purpose=production` will be deleted after the configured grace period. All others will be deleted immediately after the `Shoot` deletion.

### [`ControllerInstallation` Controller](../../pkg/gardenlet/controller/controllerinstallation)

The `ControllerInstallation` controller in the `gardenlet` reconciles `ControllerInstallation` objects with the help of the following reconcilers.

#### "Main" Reconciler

This reconciler is responsible for `ControllerInstallation`s referencing a `ControllerDeployment` whose `type=helm`.
It is responsible for unpacking the Helm chart tarball in the `ControllerDeployment`s `.providerConfig.chart` field and deploying the rendered resources to the seed cluster.
The Helm chart values in `.providerConfig.values` will be used and extended with some information about the Gardener environment and the seed cluster:

```yaml
gardener:
  version: <gardenlet-version>
  garden:
    clusterIdentity: <identity-of-garden-cluster>
  seed:
    identity: <seed-name>
    clusterIdentity: <identity-of-seed-cluster>
    annotations: <seed-annotations>
    labels: <seed-labels>
    spec: <seed-specification>
```

As of today, there are a few more fields in `.gardener.seed`, but it is recommended to use the `.gardener.seed.spec` if the Helm chart needs more information about the seed configuration.

The rendered chart will be deployed via a `ManagedResource` created in the `garden` namespace of the seed cluster.
It is labeled with `controllerinstallation-name=<name>` so that one can easily find the owning `ControllerInstallation` for an existing `ManagedResource`.

The reconciler maintains the `Installed` condition of the `ControllerInstallation` and sets it to `False` if the rendering or deployment fails.

#### "Care" Reconciler

This reconciler reconciles `ControllerInstallation` objects and checks whether they are in a healthy state.
It checks the `.status.conditions` of the backing `ManagedResource` created in the `garden` namespace of the seed cluster.

- If the `ResourcesApplied` condition of the `ManagedResource` is `True` then the `Installed` condition of the `ControllerInstallation` will be set  to `True`.
- If the `ResourcesHealthy` condition of the `ManagedResource` is `True` then the `Healthy` condition of the `ControllerInstallation` will be set  to `True`.
- If the `ResourcesProgressing` condition of the `ManagedResource` is `True` then the `Progressing` condition of the `ControllerInstallation` will be set  to `True`.

A `ControllerInstallation` is considered "healthy" if `Applied=Healthy=True` and `Progressing=False`.

#### "Required" Reconciler

This reconciler watches all resources in the `extensions.gardener.cloud` API group in the seed cluster.
It is responsible for maintaining the `Required` condition on `ControllerInstallation`s.
Concretely, when there is at least one extension resource in the seed cluster a `ControllerInstallation` is responsible for, then the status of the `Required` condition will be `True`.
If there are no extension resources anymore, its status will be `False`.

This condition is taken into account by the `ControllerRegistration` controller part of `gardener-controller-manager` when it computes which extensions have to deployed to which seed cluster, see [this document](controller-manager.md#controllerregistration-controller) for more details.

### [`Seed` Controller](../../pkg/gardenlet/controller/seed)

The `Seed` controller in the `gardenlet` reconciles `Seed` objects with the help of the following reconcilers.

#### "Main Reconciler"

This reconciler is responsible for managing the seed's system components.
Those comprise CA certificates, the various `CustomResourceDefinition`s, the logging and monitoring stacks, and few central components like `gardener-resource-manager`, `etcd-druid`, `istio`, etc.

The reconciler also deploys a `BackupBucket` resource in the garden cluster in case the `Seed'`s `.spec.backup` is set.
It also checks whether the seed cluster's Kubernetes version is at least the [minimum supported version](../usage/supported_k8s_versions.md#seed-cluster-versions) and errors in case this constraint is not met.

This reconciler maintains the `Bootstrapped` condition, i.e. it sets it

- to `Progressing` before it executes its reconciliation flow,
- to `False` in case an error occurs,
- to `True` in case the reconciliation succeeded.

#### "Care" Reconciler

This reconciler checks whether the seed system components (deployed by the "main" reconciler) are healthy.
It checks the `.status.conditions` of the backing `ManagedResource` created in the `garden` namespace of the seed cluster.
A `ManagedResource` is considered "healthy" if the conditions `ResourcesApplied=ResourcesHealthy=True` and `ResourcesProgressing=False`.

If all `ManagedResource`s are healthy then the `SeedSystemComponentsHealthy` condition of the `Seed` will be set to `True`.
Otherwise, it will be set to `False`.

If at least one `ManagedResource` is unhealthy and there is threshold configuration for the conditions (in `.controllers.seedCare.conditionThresholds`) then the status of the `SeedSystemComponentsHealthy` condition will be set

- to `Progressing` if it was `True` before.
- to `Progressing` if it was `Progressing` before and the `lastUpdateTime` of the condition does not exceed the configured threshold duration yet.
- to `False` if it was `Progressing` before and the `lastUpdateTime` of the condition does exceed the configured threshold duration.

The condition thresholds can be used to prevent reporting issues too early just because there is a rollout or a short disruption.
Only if the unhealthiness persists for at least the configured threshold duration then the issues will be reported (by setting the status to `False`).

#### "Lease" Reconciler

This reconciler checks whether the connection to the seed cluster's `/healthz` endpoint works.
If this succeeds then it renews a `Lease` resource in the garden cluster's `gardener-system-seed-lease` namespace.
This indicates a heartbeat to the external world, and internally the `gardenlet` sets its health status to `true`.
In addition, the `GardenletReady` condition in the `status` of the `Seed` is set to `True`.
The whole process is similar to what the `kubelet` does to report heartbeats for its `Node` resource and its `KubeletReady` condition, see also [this section](#heartbeats).

If the connection to the `/healthz` endpoint or the update of the `Lease` fails then the internal health status of `gardenlet` is set to `false`.
Also, this internal health status is set to `false` automatically after some time in case the controller gets stuck for whatever reason.
This internal health status is available via the `gardenlet`'s `/healthz` endpoint and is used for the `livenessProbe` in the `gardenlet` pod.

### [`ShootState` Controller](../../pkg/gardenlet/controller/shootstate)

The `ShootState` controller in the `gardenlet` reconciles resources containing information that have to be synced to the `ShootState`.
This information is used when a [control plane migration](../usage/control_plane_migration.md) is performed.

#### "Extensions" Reconciler

This reconciler watches resources in the `extensions.gardener.cloud` API group in the seed cluster which contain `Shoot`-specific state or data.
Those are `BackupEntry`s, `ContainerRuntime`s, `ControlPlane`s, `DNSRecord`s, `Extension`s, `Infrastructure`s, `Network`s, `OperatingSystemConfig`s, and `Worker`s.

When there is a change in the `.status.state` or `.status.resources[]` fields of these resources then this information is synced into the `ShootState` resource in the garden cluster.

#### "Secret" Reconciler

This reconciler reconciles `Secret`s having labels `managed-by=secrets-manager` and `persist=true` in the shoot namespaces in the seed cluster.
It syncs them to the `ShootState` so that the secrets can be restored from there in case a shoot control plane has to be restored to another seed cluster (in case of migration).

## Managed Seeds

Gardener users can use shoot clusters as seed clusters, so-called "managed seeds" (aka "shooted seeds"),
by creating `ManagedSeed` resources.
By default, the gardenlet that manages this shoot cluster then automatically
creates a clone of itself with the same version and the same configuration
that it currently has.
Then it deploys the gardenlet clone into the managed seed cluster.

If you want to prevent the automatic gardenlet deployment,
specify the `seedTemplate` section in the `ManagedSeed` resource, and don't specify
the `gardenlet` section.
In this case, you have to deploy the gardenlet on your own into the seed cluster.

More information: [Register Shoot as Seed](../usage/managed_seed.md)

## Migrating from Previous Gardener Versions

If your Gardener version doesn’t support gardenlets yet,
no special migration is required, but the following prerequisites must be met:

* Your Gardener version is at least 0.31 before upgrading to v1.
* You have to make sure that your garden cluster is exposed in a way
  that it’s reachable from all your seed clusters.

With previous Gardener versions, you had deployed the Gardener Helm chart
(incorporating the API server, `controller-manager`, and scheduler).
With v1, this stays the same, but you now have to deploy the gardenlet Helm chart as well
into all of your seeds (if they aren’t managed, as mentioned earlier).

More information: [Deploy a Gardenlet](../deployment/deploy_gardenlet.md) for all instructions.

## Related Links

- [Gardener Architecture](architecture.md)
- [#356: Implement Gardener Scheduler](https://github.com/gardener/gardener/issues/356)
- [#2309: Add /healthz endpoint for Gardenlet](https://github.com/gardener/gardener/pull/2309)
