# Monitoring and Troubleshooting the Upgrade Process

LCM reports the current state of an in-progress upgrade in the `status.conditions` list of the `Release` resource. Each entry represents a phase that LCM tracks to determine whether the upgrade has completed successfully.

Conditions follow the Kubernetes [condition structure](https://kubernetes.io/docs/concepts/workloads/pods/pod-condition/#structure-of-a-pod-condition) and use the `reason` field to describe their current state:

| Reason       | Meaning |
|--------------|---------|
| `Pending`    | The phase has not started yet. |
| `InProgress` | The phase is currently running. |
| `Skipped`    | The phase is not required for this upgrade. |
| `Failed`     | The phase failed. This causes the overall upgrade to fail. |
| `Succeeded`  | The phase completed successfully. |

The table below summarizes the conditions LCM reports during an upgrade:

| Condition Type       | Description |
|----------------------|-------------|
| `LCMUpgraded`        | Tracks the LCM upgrade phase which upgrades the Helm charts for LCM itself and its CRDs if found in the manifest under upgrade |
| `ManifestResolved`   | Indicates whether LCM retrieved and resolved the [release manifest](https://github.com/SUSE/elemental/blob/main/docs/release-manifest.md) for the requested version. |
| `PriorityHelmChartsUpgraded`   | Tracks the upgrade of Helm charts marked with the [priorityUpgrade](./release-api.md#helm) field. |
| `OSUpgraded`         | Tracks the operating system upgrade phase, including the related System Upgrade Controller Plans for `control-plane` and `worker` nodes. |
| `KubernetesUpgraded` | Tracks the Kubernetes upgrade phase, including related System Upgrade Controller Plans and the availability of packaged Kubernetes components after node upgrades complete. |
| `HelmChartsUpgraded` | Tracks the Helm chart upgrade phase for additional chart components defined in the release manifest. |
| `Applied`            | Summarizes the overall upgrade state. It becomes `True` only after the manifest is resolved and all upgrade phases have either succeeded or been skipped. |

Whenever a condition changes to `Failed`, or fails for a different reason, LCM also emits a `Warning` event with the reason `UpgradeFailed` on the `Release` resource. The event carries the type and the message of the failed condition.

## Troubleshooting Failed Conditions

This section provides troubleshooting steps for each `Release` condition when it reports as `Failed` or stays `InProgress` longer than expected.

### Lifecycle Manager Upgrade

Condition type: `LCMUpgraded`

If this phase fails or stops progressing, inspect the following resources:


| Resource                | Namespace                 | Name        | Description |
|-------------------------|---------------------------|-------------|------------- |
| Helm Chart Pod | `kube-system` | `helm-install-<chart-name>` | Inspect the Pod logs for Helm chart errors. |
| LCM Pod | `elemental-system` | LCM Pod name | Inspect LCM's logs for LCM charts upgrade reconciliation errors. |

### Release Manifest Retrieval

Condition type: `ManifestResolved`

If this phase fails or stops progressing, inspect the following resources:

| Resource                           | Namespace     | Name                     | Description |
|------------------------------------|---------------|--------------------------|-------------|
| Manifest Cache ConfigMap           | LCM namespace | `release-manifest-cache` | Use it to confirm whether the release manifest was retrieved and cached. |
| LCM Pod                            | LCM namespace | LCM Pod name             | Inspect LCM's logs for errors while retrieving, parsing, or caching the release manifest. |

### Priority Helm Chart Upgrade

Condition type: `PriorityHelmChartsUpgraded`

If this phase fails or stops progressing, inspect the following resources:


| Resource                | Namespace                 | Name        | Description |
|-------------------------|---------------------------|-------------|------------- |
| Helm Chart Pod          | `kube-system`             | `helm-install-<chart-name>` | Inspect the Pod logs for Helm chart errors. |
| LCM Pod                 | `elemental-system`        | LCM Pod name | Inspect LCM's logs for any reconciliation errors. |

### Operating System Upgrade

Condition type: `OSUpgraded`

If this phase fails or stops progressing, inspect the following resources:

| Resource                | Namespace                 | Name        | Description |
|-------------------------|---------------------------|-------------|-------------|
| SUC Plan                | `cattle-system`           | The `OSUpgraded` condition message will indicate the exact SUC Plan name | Inspect the Plan status and events for errors. | 
| SUC Plan Job            | `cattle-system`           | `apply-<suc-plan-name>-on-<node-name>` | Inspect the Job status and events for errors. |
| SUC Plan Pod            | `cattle-system`           | `apply-<suc-plan-name>-on-<node-name>` | Inspect the Pod logs for errors from the [SUSE/elemental](https://github.com/SUSE/elemental) tool set. |
| LCM Pod                 | LCM namespace             | LCM Pod name | Inspect LCM's logs for operating system upgrade reconciliation errors. |

### Failed Node Upgrades

Applies to the `OSUpgraded` and `KubernetesUpgraded` conditions.

When the upgrade workload of a node fails, the System Upgrade Controller marks the SUC Plan as failed and does not start another workload for that node. The condition of the phase then reports `Failed`, and its message contains what the SUC Plan reports: the failed Job, the node and the reason, for example `DeadlineExceeded` when the workload did not finish in [time](./workflow.md#operating-system-upgrade-phase). No other node of the same SUC Plan is upgraded in the meantime.

The node of a failed workload was cordoned before its upgrade and **remains cordoned**. The condition message lists these nodes after `Still cordoned:`. LCM does not make them schedulable again, since it cannot tell in which state the failed upgrade has left them. Nodes that have been removed from the cluster since are listed after `No longer part of the cluster:`.

The Job of the failed workload and its Pods are kept for a limited time (15 minutes by default) by the System Upgrade Controller, inspect them before they are removed.

### Kubernetes Upgrade

Condition type: `KubernetesUpgraded`

If this phase fails or stops progressing, inspect the following resources:

| Resource                          | Namespace                 | Name        | Description | 
|-----------------------------------|---------------------------|-------------|-------------|
| SUC Plan                          | `cattle-system`           | The `KubernetesUpgraded` condition message will indicate the exact SUC Plan name | Inspect the Plan status and events for errors. |
| SUC Plan Job                      | `cattle-system`           | `apply-<suc-plan-name>-on-<node-name>` | Inspect the Job status and events for errors. |
| SUC Plan Pod                      | `cattle-system`           | `apply-<suc-plan-name>-on-<node-name>` | Inspect the Pod logs for errors from the [rancher/rke2-upgrade](https://github.com/rancher/rke2-upgrade/tree/master) tool set. |
| Kubernetes Packaged Component Pod | `kube-system`             | `helm-install-rke2-<component>` | Inspect the Pod logs for packaged component errors. |
| LCM Pod                           | LCM namespace             | LCM Pod name | Inspect LCM's logs for Kubernetes upgrade reconciliation errors. |

### Additional Helm Charts Upgrade

Condition type: `HelmChartsUpgraded`

If this phase fails or stops progressing, inspect the following resources:

| Resource                      | Namespace                 | Name        | Description | 
|-------------------------------|---------------------------|-------------|-------------|
| Helm Chart Pod                | `kube-system`             | `helm-install-<chart-name>` | Inspect the Pod logs for Helm chart errors. |
| LCM Pod                       | LCM namespace             | LCM Pod name | Inspect LCM's logs for Helm chart upgrade reconciliation errors. |
