# yandex-csi-driver

A Container Storage Interface ([CSI](https://github.com/container-storage-interface/spec)) Driver for [Yandex Compute Disks](https://cloud.yandex.ru/docs/compute/concepts/disk).

Since v1.0.0 the driver is named `yandex.csi.nikolaymatrosov.ru` (before: `yandex.csi.flant.com`, the name of the
Deckhouse driver it was forked from) and its pods listen on ports 12312 (controller) and 12412 (node) instead of
12302/12402, so it can run in the same cluster as the Deckhouse driver. The PV annotation that skips formatting is now
`yandex.csi.nikolaymatrosov.ru/noformat`. PVs and VolumeSnapshotContents created by earlier versions keep the old name
and are not served by v1.0.0.

## Releases

The Yandex CSI driver follows [semantic versioning](https://semver.org/).
The version will be bumped following the rules below:

* Bug fixes will be released as a `PATCH` update.
* New features (such as CSI spec bumps with no breaking changes) will be released as a `MINOR` update.
* Significant breaking changes makes a `MAJOR` update.

## Features

Below is a list of functionality implemented by the plugin.

### Volume Expansion

Volumes can be expanded by updating the storage request value of the corresponding PVC:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: csi-pvc
  namespace: default
spec:
  [...]
  resources:
    requests:
      # The field below can be increased.
      storage: 10Gi
      [...]
```

After successful expansion, the _status_ section of the PVC object will reflect the actual volume capacity.

Important notes:

* Expansion works on a running Pod: the disk is resized in place and the filesystem is expanded on the node, there is no need to delete the Pod.
* Volumes can only be increased in size, not decreased; attempts to do so will lead to an error.
* Expanding a volume that is larger than the target size will have no effect. The PVC object status section will continue to represent the actual volume capacity.
* Resizing volumes other than through the PVC object (e.g., the DigitalOcean cloud control panel) is not recommended as this can potentially cause conflicts. Additionally, size updates will not be reflected in the PVC object status section immediately, and the section will eventually show the actual volume capacity.

### Disk Block Size

The block size of a disk can optionally be set with the `blockSize` StorageClass parameter. The value is either a plain number of bytes (`8192`) or a number with a binary kilobyte suffix (`8K`, `8Ki`, `8KiB`). Allowed values are powers of two from 4 KiB to 128 KiB, as described in the [Yandex.Cloud documentation](https://yandex.cloud/en/docs/compute/operations/disk-create/empty-disk-blocksize). When the parameter is omitted, the Yandex.Cloud default of 4 KiB is used.

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: yc-network-ssd-64k
provisioner: yandex.csi.nikolaymatrosov.ru
parameters:
  typeID: network-ssd
  blockSize: 64Ki
allowVolumeExpansion: true
volumeBindingMode: WaitForFirstConsumer
```

The block size defines the maximum size of a network disk: 8 TiB for the default 4 KiB block, and twice as much for every next block size, up to 256 TiB for a 128 KiB block. The block size cannot be changed after a disk is created.

### Snapshots

Snapshots are supported through the standard `VolumeSnapshot` API. The `snapshot.storage.k8s.io` CRDs and the snapshot-controller must be installed **before** the chart, otherwise the `yc-csi-snapclass` `VolumeSnapshotClass` is not created (it appears on the next `helm upgrade`).

```yaml
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata:
  name: data-snap
spec:
  volumeSnapshotClassName: yc-csi-snapclass
  source:
    persistentVolumeClaimName: data
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: data-restored
spec:
  storageClassName: yc-network-ssd
  dataSource:
    apiGroup: snapshot.storage.k8s.io
    kind: VolumeSnapshot
    name: data-snap
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 4Gi
```

With `volumeBindingMode: WaitForFirstConsumer` the new disk is created in the zone of the Pod that uses the PVC, so a snapshot can be restored in another zone.

Limitations:

* Snapshots are crash-consistent: data still in application or OS caches is not included. For databases, freeze writes at the application level or use the database's own backups.
* Snapshot storage is billed separately by Yandex.Cloud.
* The cloud has a snapshot quota (32 snapshots and 400 GB per cloud according to the docs, check your own cloud).
* The `blockSize` of the new volume's StorageClass must match the source disk's block size.
* `ListSnapshots` is not supported: snapshots that already exist in the cloud cannot be imported.
* Volume cloning (PVC as `dataSource`) is not supported.

### Nonreplicated and io-m3 disks

For `network-ssd-nonreplicated` and `network-ssd-io-m3` the cloud requires the size to be a multiple of 93 GiB (93Gi, 186Gi, 279Gi, …). The driver does not round other sizes up, it rejects them with `OutOfRange`: a 10Gi PVC stays Pending with a `ProvisioningFailed` event, a resize to 100Gi fails with a `VolumeResizeFailed` event and the disk keeps its size (request 186Gi instead). So the PVC request, the PV and the disk always have the same size. Before v0.20.0 the driver rounded the size up; existing volumes keep working, but resizing them now needs a multiple of 93Gi. Nonreplicated disks cannot have snapshots (a cloud limitation).

### Placement groups

A disk can be put into an existing [disk placement group](https://yandex.cloud/en/docs/compute/concepts/disk-placement-group) with the `diskPlacementGroupId` StorageClass parameter; for a group with the `PARTITION` strategy also set `diskPlacementGroupPartition` (the partition number, from 1):

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: yc-nrd-partition
provisioner: yandex.csi.nikolaymatrosov.ru
parameters:
  typeID: network-ssd-nonreplicated
  diskPlacementGroupId: <placement-group-id>
  diskPlacementGroupPartition: "2"
allowVolumeExpansion: true
volumeBindingMode: WaitForFirstConsumer
allowedTopologies:
- matchLabelExpressions:
  - key: failure-domain.beta.kubernetes.io/zone
    values: [ru-central1-e]
```

* The driver neither creates nor deletes groups: create the group yourself (`yc compute disk-placement-group create`, Terraform) and put its ID into the StorageClass. The group is only applied when a disk is created.
* A disk and its group must be in the same zone: set `allowedTopologies` to the zone of the group, otherwise the cloud refuses to create the disk and the PVC stays Pending with the cloud error in a `ProvisioningFailed` event.
* A `PARTITION` group needs `diskPlacementGroupPartition`: the cloud does not pick a partition itself and answers `InvalidArgument` (`partition index must be between 1 and N`). `diskPlacementGroupPartition` without `diskPlacementGroupId`, or a value that is not a positive integer, is rejected by the driver with `InvalidArgument`.
* The driver does not check the disk type: according to the documentation only `network-ssd-nonreplicated` disks can be in a group, the rest is up to the cloud.
* Cloud limits: 5 placement groups per cloud (`compute.diskPlacementGroups.count`), up to 8 non-replicated disks per group, up to 5 partitions per group, a disk belongs to one group.
* A repeated `CreateVolume` for an existing disk compares the group (and the partition, if set) with the StorageClass and answers `AlreadyExists` on a mismatch; a StorageClass without a group does not check it.

### Encryption

A disk can be encrypted with a Yandex Key Management Service key set in the `kmsKeyID` StorageClass parameter:

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: yc-network-ssd-encrypted
provisioner: yandex.csi.nikolaymatrosov.ru
parameters:
  typeID: network-ssd
  kmsKeyID: abj0123456789abcdefg
allowVolumeExpansion: true
volumeBindingMode: WaitForFirstConsumer
```

The controller service account needs the `kms.keys.encrypterDecrypter` role on the key. The key is applied only when a disk is created.

A snapshot of an encrypted disk is encrypted with the same key. To restore it, use a StorageClass with the same `kmsKeyID`: the cloud refuses to create a disk from an encrypted snapshot with another key or without one (`KekID should be the same for request and encrypted source`), and the PVC stays Pending.

### Disk labels

Every new disk gets the labels `cluster_uuid`, `csi-pvc-name` and `csi-pvc-namespace` (the values are cut to 63 characters, the label value limit in Yandex.Cloud). The PVC labels are filled in by external-provisioner, the chart enables this with `--extra-create-metadata`. When upgrading an existing release use `helm upgrade ... --reset-then-reuse-values` (Helm >= 3.14), otherwise `--reuse-values` keeps the old values and new chart defaults such as `--extra-create-metadata` are not applied.

### RWX

The driver supports only `ReadWriteOnce`. A Filestore cannot be used through CSI: [attaching a file storage requires stopping the VM](https://yandex.cloud/ru/docs/compute/operations/filesystem/attach-to-vm). For `ReadWriteMany` use [csi-driver-nfs](https://github.com/kubernetes-csi/csi-driver-nfs) on top of your own NFS server or [k8s-csi-s3](https://github.com/yandex-cloud/k8s-csi-s3).

### Comparison with the official driver

Compared with the official [yc-csi-driver](https://github.com/yandex-cloud/yc-csi-driver) (v1.3.0):

| | This driver | Official |
|---|---|---|
| Snapshots | yes | no |
| Volume expansion | ONLINE (on a running Pod) | OFFLINE |
| Volumes per node | from the VM topology | 5 |
| Installation | Helm chart in OCI registry | manifests |
| `kmsKeyID` | yes | yes |
| PVC name and namespace labels | yes | yes |
| csi-sanity | yes | yes |
| Placement groups | yes | yes |

### Volumes per node

The node reports how many CSI volumes it can attach (`CSINode` `allocatable.count`). On VMs with `PCI_TOPOLOGY_V2` the limit is 62 devices for up to 4 vCPU and 80 for 6+ vCPU, boot disk included, so a 2 vCPU node reports 61. Yandex.Cloud docs also say that a VM with more than 14 devices attached may fail to start after a restart; this has not been verified for `PCI_TOPOLOGY_V2` yet. VMs without `PCI_TOPOLOGY_V2` (including those that report no PCI topology) keep the old limits (8 or 14).

### Volume Statistics

Volume statistics are exposed through the CSI-conformant endpoints. Monitoring systems such as Prometheus can scrape metrics and provide insights into volume usage.

### Logs

The driver writes JSON logs to stderr, one object per line, level `info` and above. Driver messages carry `region`,
`zone`, `instance_id` and `version` (the VM the pod runs on); Yandex Cloud SDK errors, such as authentication failures,
go to the same stream. Fatal startup errors are printed as plain text. Before v0.19.0 the logs were logrus `key=value`
text: update log parsers and queries when upgrading.

## Installing to Kubernetes

### Kubernetes Compatibility

Tested on Kubernetes v1.37 (Talos Linux).

### Installing driver

Install the Helm chart from the OCI registry (the Yandex.Cloud service account key and folder ID are passed base64-encoded):

```bash
FOLDER=$(yc config get folder-id)   # folder where disks are created

# snapshot CRDs and snapshot-controller (external-snapshotter v8.7.0), before the chart
SNAP=v8.7.0
kubectl apply -k "github.com/kubernetes-csi/external-snapshotter/client/config/crd?ref=$SNAP"
kubectl apply -k "github.com/kubernetes-csi/external-snapshotter/deploy/kubernetes/snapshot-controller?ref=$SNAP"

# controller service account (compute.editor)
SA=$(yc iam service-account create --name csi-controller --format json | jq -r .id)
yc resource-manager folder add-access-binding $FOLDER --role compute.editor --subject serviceAccount:$SA
yc iam key create --service-account-id $SA --output sa-key.json

# optional: separate read-only key for the node pods (compute.viewer)
SA_NODE=$(yc iam service-account create --name csi-node --format json | jq -r .id)
yc resource-manager folder add-access-binding $FOLDER --role compute.viewer --subject serviceAccount:$SA_NODE
yc iam key create --service-account-id $SA_NODE --output sa-node-key.json

# omit the nodeServiceAccountJSON line to use the controller key on nodes
helm install yandex-csi oci://ghcr.io/nikolaymatrosov/charts/yandex-csi-controller --version 1.0.0 -n kube-system \
  --set serviceAccountJSON=$(base64 < sa-key.json | tr -d '\n') \
  --set nodeServiceAccountJSON=$(base64 < sa-node-key.json | tr -d '\n') \
  --set folderID=$(printf %s $FOLDER | base64)
```

Install into `kube-system`, or into any namespace labeled `pod-security.kubernetes.io/enforce=privileged`
(`kubectl label ns <ns> pod-security.kubernetes.io/enforce=privileged`). Otherwise Pod Security (`baseline`, the Talos default)
rejects the privileged node pod (hostPath) and the controller pod (`hostNetwork`).
