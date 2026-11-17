# Image build operator

A `NodeImage` object asks for the node image of one Elemental release to exist on one target. The operator
resolves the release to a digest, computes a cache key, and either finds the image on the target (cache hit) or
has it built and delivers it. It uses the Kubernetes API of the cluster it runs on and the API of the target,
nothing else; in particular no Rancher API and no Rancher resource.

Status: a working minimum for one target type (`harvester`), **x86-64 only**. It builds with an embedded copy of
a lab recipe (`internal/recipe/files/SOURCE`), which today needs an unreleased `elemental3`.

```yaml
apiVersion: image.lifecycle.suse.com/v1alpha1
kind: NodeImage
metadata: {name: rke2-1-37-2, namespace: image-operator-system}
spec:
  release: {manifest: registry.suse.com/beta/uc/rke2/rke2-manifest:1.37.2-20.3}
  target:
    type: harvester
    credentialsSecretRef: {name: harvester}        # Secret with key "kubeconfig"; a namespace tenant is enough
    harvester: {namespace: default}
```

`kubectl wait nodeimage/rke2-1-37-2 --for=condition=Ready`; then `status.image.name` is the Harvester image and
`status.image.kubernetesVersion` the version a control plane object must state.

## How it works

| Step | What happens |
|---|---|
| Resolve | Registry `HEAD` of the manifest reference; the digest is stored in status. A release that does not exist ends the request (`Failed`, reason `ReleaseNotFound`) |
| Key | `sha256` of: key schema version, manifest digest, target type, architecture, disk size, chart values, recipe digest, `elemental3` source repository and commit (`internal/cachekey`) |
| Lookup | Harvester image `<prefix>img-<key[:12]>` whose annotation holds the whole key. Imported: `Ready`, `cacheHit: true`, nothing built |
| Build (`vm`; for `pod` see Builders) | A short-lived VM in the target namespace (stock cloud image; cloud-init installs podman and runs the recipe) behind a ClusterIP Service. The operator reads `status.json` from it through the Service proxy of the target API. The VM holds no credential |
| Deliver | A `VirtualMachineImage` with the Service URL and the SHA-512; Harvester downloads and verifies. Then the VM, its disk, Secret and Service are deleted |
| Failure | A failed or overdue build is retried with a new VM up to `--max-attempts` (2), then `Failed` with the end of the build log. An object the target refuses as invalid ends the request at once (`TargetRejected`). Nothing retries a `Failed` request; delete and recreate it |

The state machine is one pure function, `internal/controller/ladder.go`. Targets and builders are the two
interfaces in `internal/target`.

## Privileges

- Operator pod: unprivileged, non-root, read-only root file system. Namespaced RBAC only (`config/deploy/operator.yaml`):
  its own resource, `get` on Secrets, events. Cluster-scoped: the CRD.
- Target credentials, in the target namespace: VirtualMachine, PersistentVolumeClaim, Secret, Service,
  `services/proxy`, VirtualMachineImage.
- VM builder: the build runs as root with `podman run --privileged` **inside the builder VM**. Nothing privileged
  runs on the management cluster or on the Harvester host.
- Pod builder: see below. Never `privileged`, no device, no host mount.

## Builders

Two builders sit behind one interface (`internal/target`: `Builder`). `--builder=vm|pod` sets the operator's
default; a request may name one in `spec.builder`. A request for a builder the operator is not configured with
fails (`UnsupportedBuilder`); it is never served by the other one. The builder is not part of the cache key: both
build the same recipe, and an image one of them built is a cache hit for the other.

| | `vm` (`internal/target/harvester`) | `pod` (`internal/builder/pod`) |
|---|---|---|
| Runs | A short-lived VM in the target's Harvester namespace | A short-lived pod in `--pod-builder-namespace` of the operator's cluster, or of the cluster `--pod-builder-kubeconfig` names |
| Needs | The target credentials only | A builder image (`builder-image/Containerfile`: the `elemental3` image plus the recipe's tools) the cluster can pull; RBAC of `config/deploy/pod-builder.yaml`; a namespace that admits the pod (Pod Security Standard `baseline`); an address the target can fetch from (`--pod-builder-service-type=NodePort --pod-builder-download-host=...`, unless the target is the same cluster) |
| Privileges of the build | root and `--privileged` inside the VM; an ordinary VM to Harvester | Root of the pod's **own user namespace** (`hostUsers: false`), capabilities `CHOWN` and `DAC_OVERRIDE` only, no privilege escalation, seccomp `RuntimeDefault`, read-only root file system, no device, no host path, no host network, no service account token. The second container, which serves the result, is not root and has no capability. `TestPodPrivileges` fails if any of this widens |
| Tool | Built from source in the VM (`--elemental-repo`, `--elemental-commit`) | In the builder image; the pod refuses to build if the image's `elemental3` is not `--elemental-commit` |
| Work space | VM disk (`--builder-disk`) | `--pod-builder-work-size`: an emptyDir with that limit, or an ephemeral claim of `--pod-builder-work-storage-class` |

Why the pod needs no more: without a loop device `systemd-repart` builds the two file systems of the image
without mounting anything, so `elemental3 customize` needs neither `SYS_ADMIN` nor a device. It does unpack
container images keeping the owners of their files, which is what the two capabilities are for; as a non-root user
it stops at the first `lchown`. `--pod-builder-host-users=true` is for clusters without user namespaces: the same
two capabilities, held by the node's root.

Not handled: two requests for one cache key that name different builders each start their own; both deliver the
same image name, and the second delivery is a no-op. Changing `--builder` while a build runs leaves the first
builder to its deadline.

## Build and test

```sh
make test        # unit tests: cache key, state machine, controller against a fake target, Harvester objects
make test-crd    # the CRD schema (defaults, patterns, immutable spec) against a real API server (envtest)
make build       # bin/image-operator
make container   # container image (podman)
make generate    # after changing api/
```

`hack/apiserver` starts a bare API server with only this CRD in it, to run the operator against a management
API that has nothing else installed.

Not implemented (see the design note of the parity project, `design/T1.5-image-build-operator.md`): the prebuilt
image path, pull secrets for private releases, time-to-live collection of unused images, targets other than
Harvester, builders other than the VM.
