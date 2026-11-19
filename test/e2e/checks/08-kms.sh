#!/usr/bin/env bash
# 08: kmsKeyID encryption; restore of an encrypted snapshot with and without the key.
set -euo pipefail
# shellcheck source=../lib.sh
. "$(dirname "$0")/../lib.sh"
state_load

KEY=$(yc kms symmetric-key create --name "$P-csi" --default-algorithm aes-256 --format json | jq -r .id)
yc kms symmetric-key add-access-binding "$KEY" --role kms.keys.encrypterDecrypter --subject "serviceAccount:$SA" >/dev/null
sleep 30 # the key binding takes a few seconds to propagate
apply <<YAML
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata: {name: yc-network-ssd-kms}
provisioner: $DRIVER
parameters: {typeID: network-ssd, kmsKeyID: $KEY}
allowVolumeExpansion: true
volumeBindingMode: WaitForFirstConsumer
YAML
key_of() { disk_json "$(pv_disk "$1")" | jq -r '.kms_key.key_id // ""'; }

pvc_pod csi-kms yc-network-ssd-kms 4Gi "$NA1"
pod_ready csi-kms
check 08 "kms_key диска" "$KEY" "$(key_of csi-kms)"
k exec csi-kms -- sh -c 'echo secret > /data/check; sync'
check 08 "запись и чтение" secret "$(k exec csi-kms -- cat /data/check)"

vsnap csi-kms-snap csi-kms
wait_until 300 snap_ready csi-kms-snap || true
check 08 "csi-kms-snap readyToUse" true "$(k get volumesnapshot csi-kms-snap -o jsonpath='{.status.readyToUse}')"
info 08 "kms_key снапшота" "$(yc compute snapshot get "$(snap_handle csi-kms-snap)" --format json | jq -c '.kms_key // "нет"')"

pvc_pod csi-kms-restore yc-network-ssd-kms 4Gi "$NA1" csi-kms-snap
pod_ready csi-kms-restore
check 08 "restore с тем же ключом: данные" secret "$(k exec csi-kms-restore -- cat /data/check)"
check 08 "restore с тем же ключом: kms_key" "$KEY" "$(key_of csi-kms-restore)"

# Known cloud behaviour (v0.18.0 run): restore into an SC without the key fails with InvalidArgument.
pvc_pod csi-kms-nokey yc-network-ssd 4Gi "$NA1" csi-kms-snap
sleep 120
info 08 "restore без ключа: фаза PVC" "$(k get pvc csi-kms-nokey -o jsonpath='{.status.phase}')"
info 08 "restore без ключа: событие" "$(k get events --field-selector involvedObject.name=csi-kms-nokey,reason=ProvisioningFailed -o json |
  jq -r '.items | sort_by(.lastTimestamp) | last | .message // "нет"')"

k delete pod csi-kms csi-kms-restore csi-kms-nokey
k delete pvc csi-kms csi-kms-restore csi-kms-nokey
k delete volumesnapshot csi-kms-snap
k delete sc yc-network-ssd-kms
