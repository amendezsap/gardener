# Configuring the Logging stack via Gardenlet configurations

# Enable the Logging

In order to install the Gardener logging stack the `logging.enabled` configuration option has to be enabled in the Gardenlet configuration:
```yaml
logging:
  enabled: true
```

From now on each Seed is going to have a logging stack which will collect logs from all pods and some systemd services. Logs related to Shoots with `testing` purpose are dropped in the `fluent-bit` output plugin. Shoots with a purpose different than `testing` have the same type of log aggregator (but different instance) as the Seed. The logs can be viewed in the Grafana in the `garden` namespace for the Seed components and in the respective shoot control plane namespaces.

# Enable logs from the Shoot's node systemd services.

The logs from the systemd services on each node can be retrieved by enabling the `logging.shootNodeLogging` option in the Gardenlet configuration:
```yaml
logging:
  enabled: true
  shootNodeLogging:
    shootPurposes:
    - "evaluation"
    - "deployment"
```

Under the `shootPurpose` section just list all the shoot purposes for which the Shoot node logging feature will be enabled. Specifying the `testing` purpose has no effect because this purpose prevents the logging stack installation.
Logs can be  viewed in the operator Grafana!
The dedicated labels are `unit`, `syslog_identifier` and `nodename` in the `Explore` menu.

# Configuring the log processor

Under `logging.fluentBit` there is three optional sections.
- `input`: This overwrite the input configuration of the fluent-bit log processor.
 - `output`: This overwrite the output configuration of the fluent-bit log processor.
 - `service`: This overwrite the service configuration of the fluent-bit log processor.

```yaml
logging:
  enabled: true
  fluentBit:
    output: |-
      [Output]
          ...
    input: |-
      [Input]
          ...
    service: |-
      [Service]
          ...
```

# additional egress IPBlock for allow-fluentbit NetworkPolicy

The optional setting under `logging.fluentBit.networkPolicy.additionalEgressIPBlocks` add additional egress IPBlock to `allow-fluentbit` NetworkPolicy to forward logs to a central system.

```yaml
logging:
  enabled: true
  fluentBit:
    additionalEgressIpBlock:
      - 123.123.123.123/32
```

# Configure central logging

For central logging, the output configuration of the fluent-bit log processor can be overwritten (`logging.fluentBit.output`) and the Loki instances deployments in Garden and Shoot namespace can be enabled/disabled (`logging.loki.enabled`), by default Loki is enabled.

```yaml
logging:
  enabled: true
  fluentBit:
    output: |-
      [Output]
          ...
  loki:
    enabled: false
```

# Configuring central Loki storage capacity

By default, the central Loki has `100Gi` of storage capacity.
To overwrite the current central Loki storage capacity, the `logging.loki.garden.storage` setting in the gardenlet's component configuration should be altered.
If you need to increase it you can do so without losing the current data by specifying higher capacity. Doing so, the Loki's `PersistentVolume` capacity will be increased instead of deleting the current PV.
However, if you specify less capacity then the `PersistentVolume` will be deleted and with it the logs, too.

```yaml
logging:
  enabled: true
  fluentBit:
    output: |-
      [Output]
          ...
  loki:
    garden:
      storage: "200Gi"
```
