#!/usr/bin/env bash
# 04: snapshots: create, restore in the other zone, delete, two at once. Leaves csi-snap-a/b for 09.
set -euo pipefail
# shellcheck source=../lib.sh
. "$(dirname "$0")/../lib.sh"
state_load

ready() { k get volumesnapshot "$1" -o jsonpath='{.status.readyToUse}'; }

# 4.1 snapshot of csi-test (has /data/check = hello from 02)
vsnap csi-snap csi-test
wait_until 300 snap_ready csi-snap || true
check 04.1 "csi-snap readyToUse" true "$(ready csi-snap)"
SNAP=$(snap_handle csi-snap)
s=$(yc compute snapshot get "$SNAP" --format json)
check 04.1 "снапшот в облаке" READY "$(jq -r .status <<<"$s")"
check 04.1 "метка снапшота cluster_uuid" "$KS_UID" "$(jq -r '.labels.cluster_uuid // ""' <<<"$s")"

# 4.2 restore on the node of the second zone
pvc_pod csi-restore yc-network-ssd 4Gi "$N3" csi-snap
pod_ready csi-restore
check 04.2 "данные из снапшота" hello "$(k exec csi-restore -- cat /data/check)"
check 04.2 "зона диска restore" "$ZONE2" "$(disk_json "$(pv_disk csi-restore)" | jq -r .zone_id)"

# 4.3 deleting the VolumeSnapshot deletes the cloud snapshot
k delete pod csi-restore
k delete pvc csi-restore
k delete volumesnapshot csi-snap
if wait_until 180 gone_snapshot "$SNAP"; then
  pass 04.3 "снапшот удалён в облаке" "нет" "нет"
else
  fail 04.3 "снапшот удалён в облаке" "нет" "$SNAP есть через 3 мин"
fi

# 4.4 two snapshots of one disk at once
before=$(ctrl_restarts)
apply <<YAML
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata: {name: csi-snap-a}
spec: {volumeSnapshotClassName: yc-csi-snapclass, source: {persistentVolumeClaimName: csi-test}}
---
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata: {name: csi-snap-b}
spec: {volumeSnapshotClassName: yc-csi-snapclass, source: {persistentVolumeClaimName: csi-test}}
YAML
both_ready() { snap_ready csi-snap-a && snap_ready csi-snap-b; }
wait_until 600 both_ready || true
check 04.4 "csi-snap-a readyToUse" true "$(ready csi-snap-a)"
check 04.4 "csi-snap-b readyToUse" true "$(ready csi-snap-b)"
check 04.4 "рестарты csi-controller" "$before" "$(ctrl_restarts)"
info 04.4 "ошибки облака на CreateSnapshot" "$(ctrl_log |
  jq -Rr 'fromjson? | objects | select(.level == "error" and ((.method // "") | test("CreateSnapshot"))) | .error' |
  sort -u | tr '\n' ' ')"
