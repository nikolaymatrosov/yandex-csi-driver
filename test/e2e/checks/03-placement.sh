#!/usr/bin/env bash
# Copyright 2026 Nikolay Matrosov
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# 03: placement groups (diskPlacementGroupId / diskPlacementGroupPartition in a StorageClass), NRD disks in ZONE2.
# One 93Gi disk at a time. The script creates two groups ($P-pg-spread, $P-pg-part) and deletes them at the end.
set -euo pipefail
# shellcheck source=../lib.sh
. "$(dirname "$0")/../lib.sh"
state_load

ev() { k get events --field-selector "involvedObject.name=$1,reason=$2" -o json |
  jq -r '.items | sort_by(.lastTimestamp) | last | .message // ""'; }
failed_with() { ev "$1" ProvisioningFailed | grep -qF "$2"; } # PVC $1 got a ProvisioningFailed event containing $2
pg_id() { # NAME [create flags...]: id of the group NAME in ZONE2, created if missing (a re-run reuses it)
  local n=$1
  shift
  yc compute disk-placement-group get "$n" >/dev/null 2>&1 ||
    yc compute disk-placement-group create --name "$n" --zone "$ZONE2" "$@" >/dev/null
  yc compute disk-placement-group get "$n" --format json | jq -r .id
}
disks_named() { yc compute disk list --format json | jq --arg n "pvc-$(k get pvc "$1" -o jsonpath='{.metadata.uid}')" '[.[] | select(.name == $n)] | length'; }
sc() { # NAME PARAMETERS (a YAML flow mapping)
  apply <<YAML
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata: {name: $1}
provisioner: $DRIVER
parameters: $2
allowVolumeExpansion: true
volumeBindingMode: WaitForFirstConsumer
YAML
}
drop() { # PVC [DISK]: delete the pod and the PVC, wait for the disk of a bound PVC to go away
  local disk=${2:-}
  k delete pod "$1"
  k delete pvc "$1"
  [ -z "$disk" ] || wait_until 180 gone_disk "$disk" || fail 03 "диск $1 удалён вместе с PVC" "нет" "$disk есть через 3 мин"
}

# an aborted check must not leave its 93Gi NRD disk behind: it would starve check 07 of quota
trap 'for n in csi-pg-spread csi-pg-part csi-pg-nopart csi-pg-bad; do
  kubectl delete pod "$n" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl delete pvc "$n" --ignore-not-found --wait=false >/dev/null 2>&1 || true
done' EXIT

state_set PG_SPREAD "$(pg_id "$P-pg-spread")"
state_set PG_PART "$(pg_id "$P-pg-part" --strategy PARTITION --partition-count 2)"
NRD=network-ssd-nonreplicated
sc yc-nrd-pg-spread "{typeID: $NRD, diskPlacementGroupId: $PG_SPREAD}"
sc yc-nrd-pg-part "{typeID: $NRD, diskPlacementGroupId: $PG_PART, diskPlacementGroupPartition: \"2\"}"
sc yc-nrd-pg-nopart "{typeID: $NRD, diskPlacementGroupId: $PG_PART}"
sc yc-nrd-pg-bad "{typeID: $NRD, diskPlacementGroupId: $PG_PART, diskPlacementGroupPartition: x}"

# spread group
pvc_pod csi-pg-spread yc-nrd-pg-spread 93Gi "$N3"
pod_ready csi-pg-spread
D=$(pv_disk csi-pg-spread)
d=$(disk_json "$D")
check 03 "spread: placement_group_id" "$PG_SPREAD" "$(jq -r '.disk_placement_policy.placement_group_id // ""' <<<"$d")"
check 03 "spread: дисков с именем PVC" 1 "$(disks_named csi-pg-spread)"
drop csi-pg-spread "$D"

# partition group, partition 2
pvc_pod csi-pg-part yc-nrd-pg-part 93Gi "$N3"
pod_ready csi-pg-part
D=$(pv_disk csi-pg-part)
d=$(disk_json "$D")
check 03 "partition: placement_group_id" "$PG_PART" "$(jq -r '.disk_placement_policy.placement_group_id // ""' <<<"$d")"
check 03 "partition: placement_group_partition" 2 "$(jq -r '.disk_placement_policy.placement_group_partition // 0' <<<"$d")"
check 03 "partition: дисков с именем PVC" 1 "$(disks_named csi-pg-part)"
drop csi-pg-part "$D"

# partition group without a partition: the cloud refuses, the error reaches the PVC events
pvc_pod csi-pg-nopart yc-nrd-pg-nopart 93Gi "$N3"
for want in InvalidArgument 'partition index'; do
  if wait_until 180 failed_with csi-pg-nopart "$want"; then
    pass 03 "partition-группа без раздела: событие содержит «${want}»" "есть" "есть"
  else
    fail 03 "partition-группа без раздела: событие содержит «${want}»" "есть" "$(ev csi-pg-nopart ProvisioningFailed)"
  fi
done
# the driver passes the cloud code through: it must not be wrapped into Internal
if failed_with csi-pg-nopart 'code = Internal'; then
  fail 03 "partition-группа без раздела: событие не содержит «code = Internal»" "нет" "есть: $(ev csi-pg-nopart ProvisioningFailed)"
else
  pass 03 "partition-группа без раздела: событие не содержит «code = Internal»" "нет" "нет"
fi
info 03 "partition-группа без раздела: текст события" "$(ev csi-pg-nopart ProvisioningFailed)"
check 03 "partition-группа без раздела: фаза PVC" Pending "$(k get pvc csi-pg-nopart -o jsonpath='{.status.phase}')"
check 03 "partition-группа без раздела: дисков с именем PVC" 0 "$(disks_named csi-pg-nopart)"
drop csi-pg-nopart

# a partition that is not a number is rejected by the driver
pvc_pod csi-pg-bad yc-nrd-pg-bad 93Gi "$N3"
if wait_until 180 failed_with csi-pg-bad InvalidArgument; then
  pass 03 "partition «x»: событие содержит InvalidArgument" "есть" "есть"
else
  fail 03 "partition «x»: событие содержит InvalidArgument" "есть" "$(ev csi-pg-bad ProvisioningFailed)"
fi
check 03 "partition «x»: фаза PVC" Pending "$(k get pvc csi-pg-bad -o jsonpath='{.status.phase}')"
check 03 "partition «x»: дисков с именем PVC" 0 "$(disks_named csi-pg-bad)"
drop csi-pg-bad

k delete sc yc-nrd-pg-spread yc-nrd-pg-part yc-nrd-pg-nopart yc-nrd-pg-bad
yc compute disk-placement-group delete "$PG_SPREAD" "$PG_PART" >/dev/null
