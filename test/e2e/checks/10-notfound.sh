#!/usr/bin/env bash
# 10: idempotent deletes when the cloud resource is already gone (DeleteVolume / DeleteSnapshot get NotFound).
set -euo pipefail
# shellcheck source=../lib.sh
. "$(dirname "$0")/../lib.sh"
state_load

# volume: detach, delete the disk in the cloud, then delete the PVC
pvc_pod csi-nf yc-network-ssd 4Gi "$NA1"
pod_ready csi-nf
NF=$(pv_disk csi-nf)
PV=$(k get pvc csi-nf -o jsonpath='{.spec.volumeName}')
k delete pod csi-nf
detached() { [ "$(disk_json "$NF" | jq -r '.instance_ids // [] | length')" = 0 ]; }
wait_until 180 detached || die "disk $NF still attached 3 min after the pod was deleted"
yc compute disk delete "$NF" >/dev/null
k delete pvc csi-nf --wait=false
pv_gone() { ! kubectl get pv "$PV" >/dev/null 2>&1; }
if wait_until 180 pv_gone; then
  pass 10 "PV удалён, хотя диска уже нет" "PV нет за 3 мин" "PV нет"
else
  fail 10 "PV удалён, хотя диска уже нет" "PV нет за 3 мин" "$(k get pv "$PV" -o jsonpath='{.status.phase}')"
fi
check_ge 10 "лог: assuming volume is deleted" 1 "$(ctrl_log | grep -c 'assuming volume is deleted' || true)"

# snapshot: delete it in the cloud, then delete the VolumeSnapshot
vsnap csi-nf-snap csi-test
wait_until 300 snap_ready csi-nf-snap || die "csi-nf-snap not ready in 5 min"
VSC=$(snap_content csi-nf-snap)
yc compute snapshot delete "$(snap_handle csi-nf-snap)" >/dev/null
k delete volumesnapshot csi-nf-snap --wait=false
vsc_gone() { ! kubectl get volumesnapshotcontent "$VSC" >/dev/null 2>&1; }
if wait_until 180 vsc_gone; then
  pass 10 "VolumeSnapshotContent удалён, хотя снапшота уже нет" "нет за 3 мин" "нет"
else
  fail 10 "VolumeSnapshotContent удалён, хотя снапшота уже нет" "нет за 3 мин" "$VSC есть"
fi
check_ge 10 "лог: snapshot is already deleted" 1 "$(ctrl_log | grep -c 'snapshot is already deleted' || true)"
