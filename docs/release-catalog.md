# Release catalog

The release catalog lists the releases a cluster could be moved to. It is a separate controller
(`cmd/catalog`, image built from `Dockerfile.catalog`, chart `charts/elemental-release-catalog`) meant
for a management cluster. It needs nothing but Kubernetes and its two custom resource definitions; it does
not need Rancher, Cluster API or the Lifecycle Manager itself.

```sh
helm install release-catalog charts/elemental-release-catalog -n elemental-catalog --create-namespace \
  --set image.repository=<registry>/elemental-release-catalog
kubectl apply -f config/samples/lifecycle_v1alpha1_releasesource.yaml
kubectl get releasesources,availablereleases
```

## Objects (`lifecycle.suse.com/v1alpha1`, both cluster-scoped)

| Kind | Written by | Content |
|---|---|---|
| `ReleaseSource` | an administrator | `spec.registry`: one OCI repository of release manifests, one tag per release (the same value as `spec.registry` of a `Release`). Optional: `interval` (default 5m, minimum 30s), `pullSecretRef` (a `kubernetes.io/dockerconfigjson` Secret in the controller's namespace), `insecure`, `caBundle`. Status: condition `Ready`, `stale`, `releases`, `lastSyncTime`, `lastSuccessfulSyncTime`, `skipped` (tags that are not published, with the reason) |
| `AvailableRelease` | the controller only | One per release found, named `<source>-<version>` and owned by its source. `spec.registry` and `spec.version` are the two values a `Release` takes. Also: `digest`, `manifest` (reference pinned by digest), `lineage` (the manifest's `metadata.name`), `creationDate`, `operatingSystem` (image, version), `kubernetes` (version, image), `corePlatform` for a solution release. Status: `discoveredAt`, `stale`. Labels `lifecycle.suse.com/source` and `lifecycle.suse.com/lineage` |

## Behaviour

- Every interval the controller lists the tags of the repository. A tag that names a version is a release;
  its manifest is downloaded only when the digest of the tag is not the one already published.
- A new tag appears as an `AvailableRelease` at the next query. A tag removed from the repository is removed
  from the catalog. A tag moved to other content is updated.
- Not published, and listed under `status.skipped` of the source: tags that are not versions (`latest`,
  signatures), artifacts without a `release_manifest.yaml`, invalid manifests, and releases without
  Kubernetes (operating system only: not a cluster version).
- A solution release is resolved to the core platform release it pins: its lineage is the solution's name,
  its operating system and Kubernetes are the core platform's.
- **Source not reachable:** nothing is deleted. The source gets `Ready=False` (`SourceUnreachable`) and
  `status.stale: true`, every release of the source gets `status.stale: true`, and
  `lastSuccessfulSyncTime` keeps the time of the last good answer. The marks are cleared by the next
  query that succeeds. When only some tags cannot be read, only those entries are marked stale
  (`PartiallySynced`).

## Which releases can a cluster reach

The catalog states facts; it does not know the clusters. The rule is applied by whoever offers a target
(`internal/catalog/reach.go`, `Reachable`): the same lineage, and a version higher than the one the
cluster runs, compared the way the admission webhook of the Lifecycle Manager compares versions (it
refuses an equal or lower version). The lineage of the cluster's current release is looked up in the
catalog; when the catalog does not hold that release, the last path element of the two repositories must
be equal instead.

```sh
release-catalog --reachable-from 1.37.1-17.1 --registry registry.suse.com/beta/uc/rke2/rke2-manifest
```

prints the catalog with a verdict and a reason per entry, using the current kubeconfig.

Not checked: Kubernetes minor version skips, signatures.

## Access

The chart creates a ClusterRole `<release>-reader` (get, list, watch on both kinds) and binds it to nobody
unless `reader.authenticated=true`. The controller itself may write only its own two kinds, and reads
Secrets by name in its own namespace only.

Plain HTTP: the registry client uses HTTP for registries on private addresses and `localhost` even when
`insecure` is not set (behaviour of go-containerregistry, the same as for a `Release`).
