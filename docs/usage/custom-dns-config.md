---
title: Custom DNS Configuration
---

# Custom DNS Configuration

Gardener provides Kubernetes-Clusters-As-A-Service where all the system components (e.g., kube-proxy, networking, dns, ...) are managed.
As a result, Gardener needs to ensure and auto-correct additional configuration to those system components to avoid unnecessary down-time.

In some cases, auto-correcting system components can prevent users from deploying applications on top of the cluster that requires bits of customization, DNS configuration can be a good example.

To allow for customizations for DNS configuration (that could potentially lead to downtime) while having the option to "undo", we utilize the `import` plugin from CoreDNS [1].
which enables in-line configuration changes.

## How to use

To customize your CoreDNS cluster config, you can simply edit a `ConfigMap` named `coredns-custom` in the `kube-system` namespace.
By editing, this `ConfigMap`, you are modifying CoreDNS configuration, therefore care is advised.

For example, to apply new config to CoreDNS that would point all `.global` DNS requests to another DNS pod, simply edit the configuration as follows:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: coredns-custom
  namespace: kube-system
data:
  istio.server: |
    global:8053 {
            errors
            cache 30
            forward . 1.2.3.4
        }
  corefile.override: |
         # <some-plugin> <some-plugin-config>
         debug
         whoami
```

It is important to have the `ConfigMap` keys ending with `*.server` (if you would like to add a new server) or `*.override`
if you want to customize the current server configuration (it is optional setting both).

## [Optional] Reload CoreDNS

As Gardener is configuring the `reload` [plugin](https://coredns.io/plugins/reload/) of CoreDNS a restart of the CoreDNS components is typically not necessary to propagate `ConfigMap` changes. However, if you don't want to wait for the default (30s) to kick in, you can roll-out your CoreDNS deployment using:

```bash
kubectl -n kube-system rollout restart deploy coredns
```

This will reload the config into CoreDNS.

The approach we follow here was inspired by AKS's approach [2].

## Anti-Pattern

Applying a configuration that is in-compatible with the running version of CoreDNS is an anti-pattern (sometimes plugin configuration changes,
simply applying a configuration can break DNS).

If incompatible changes are applied by mistake, simply delete the content of the `ConfigMap` and re-apply.
This should bring the cluster DNS back to functioning state.

## Known Issues

The custom DNS configuration may not work as expected in conjunction with [`NodeLocalDNS`](node-local-dns.md).
With `NodeLocalDNS`, ordinary dns queries targetted at the upstream DNS servers, i.e. non-kubernetes domains,
will not end up at CoreDNS, but will instead be directly sent to the upstream DNS server. Therefore, configuration
applying to non-kubernetes entities, e.g. the `istio.server` block in the example above, may not have any effect
with `NodeLocalDNS` enabled. If this kind of custom configuration is required `NodeLocalDNS` needs to be disabled.

## References

[1] [Import plugin](https://github.com/coredns/coredns/tree/master/plugin/import)
[2] [AKS Custom DNS](https://docs.microsoft.com/en-us/azure/aks/coredns-custom)
