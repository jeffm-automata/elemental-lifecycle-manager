# Release API

The `Release` resource is the single point of entry for environment upgrades. It defines the desired state of an environment and provides upgrade-specific configuration. Elemental Lifecycle Manager (LCM) reconciles the cluster to match the state described by this resource.


## Constraints

When defining a `Release` resource, LCM enforces the following constraints:

* Only one `Release` resource can exist on a cluster.
* Updates of an existing `Release` resource are only allowed when **no** upgrade is currently in progress.
* Downgrades of an existing `Release` resource `spec.version` field are not allowed.

## Spec

```yaml
# Release resource example
apiVersion: lifecycle.suse.com/v1alpha1
kind: Release
metadata:
  name: release-example
spec:
  version: "1.0.0"
  registry: "registry.example.org/project/release-manifest"
  disableDrain: true
  workerConcurrency: 2
  componentConfiguration:
    helm:
    - chart: example
      values:
        replicaCount: 1
        resources:
          requests:
            cpu: 250m
            memory: 250Mi
      valuesFrom:
        secretRef:
          name: example-values-secret
          keys:
          - "values-example-chart"
```

| Field               | Description                                                              | Required |
|---------------------|--------------------------------------------------------------------------|:--------:|
| `spec.version`      | Target version of the release. Use semantic versioning, when possible.  | `true`   |
| `spec.registry`     | OCI registry from which LCM will fetch the release metadata. Metadata must be defined in the form of a [release manifest](https://github.com/SUSE/elemental/blob/main/docs/release-manifest.md).   | `true`   |
| `spec.disableDrain` | Disables node drains during upgrade phases. Defaults to `false`.        | `false`  |
| `spec.workerConcurrency` | Maximum number of worker nodes upgraded at the same time during the operating system and Kubernetes upgrade phases. Must be `1` or greater. Defaults to `1`. See [Worker Concurrency](#worker-concurrency). | `false`  |
| `spec.componentConfiguration.helm` | Provide configurations to Helm chart components specified in the release. | `false` |

### Worker Concurrency

By default LCM upgrades the nodes of a cluster one at a time. `spec.workerConcurrency` raises the number of **worker** nodes that are upgraded at the same time. Control plane nodes are always upgraded one at a time, and worker nodes are only upgraded after all control plane nodes.

When setting this field, consider the following:

* All worker nodes that are upgraded at the same time are cordoned and, unless draining is disabled, drained together. The remaining nodes must have the capacity to run the evicted workloads, and `PodDisruptionBudgets` must allow the evictions. A drain that cannot complete blocks the upgrade of that node.
* The value is applied when LCM creates the [SUC Plans](https://github.com/rancher/system-upgrade-controller/tree/master#api-documentation) for an operating system or Kubernetes version. Plans that already exist for the same version are not changed.

### Component Configurations

> IMPORTANT: Component configuration can only be done **during a release version upgrade**. Configuring components outside of a release version upgrade is not **yet** supported.

#### Helm

> NOTE: The `[*]` prefix refers to an entry in the `spec.componentConfiguration.helm` configuration list.

| Field                           | Description                                                              | Required |
|---------------------------------|--------------------------------------------------------------------------|:--------:|
| `[*].chart`                     | Reference to the chart in the release that will be configured.           | `true`   |
| `[*].priorityUpgrade`           | Overrides the upgrade priority of the referenced chart.                  | `false`  |
| `[*].values`                    | Custom Helm values configuration provided inline.                        | `false`  |
| `[*].valuesFrom.secretRef`      | Apply Helm values configuration provided from a Kubernetes Secret.       | `false`  |
| `[*].valuesFrom.secretRef.name` | Name of the secret from which to retrieve custom helm chart values. **The secret must be located in the `kube-system` namespace.** | `true` |
| `[*].valuesFrom.secretRef.keys` | Data entries containing the desired values as seen in the Secret.        | `true`   |

> NOTE: `[*].valuesFrom` takes precedence over `[*].values` definitions.

## Status

| Field               | Description                                                                             |
|---------------------|-----------------------------------------------------------------------------------------|
| `status.version`    | The last release version to which the environment state was successfully upgraded.      | 
| `spec.conditions`   | Information about the state of the currently running upgrade process.                   | 
| `spec.disableDrain` | The latest resource generation observed by the controller. Meant for internal use only. |
