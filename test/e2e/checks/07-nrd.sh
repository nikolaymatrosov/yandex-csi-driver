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

# 07: non-replicated disks take only multiples of 93 GiB, on create and on resize; other sizes are rejected.
set -euo pipefail
# shellcheck source=../lib.sh
. "$(dirname "$0")/../lib.sh"
state_load

ev() { k get events --field-selector "involvedObject.name=$1,reason=$2" -o json |
  jq -r '.items | sort_by(.lastTimestamp) | last | .message // ""'; }
has_ev() { ev "$1" "$2" | grep -q 'must be a multiple of 93Gi'; }
ev_check() { # desc pvc reason: PASS when the PVC gets the size-step error within 3 min
  if wait_until 180 has_ev "$2" "$3"; then pass 07 "$1" "кратно 93Gi" "$(ev "$2" "$3")"
  else fail 07 "$1" "кратно 93Gi" "$(ev "$2" "$3")"; fi
}

pvc_pod csi-nrd-bad yc-network-ssd-nonreplicated 10Gi "$N3"
ev_check "PVC 10Gi отклонён" csi-nrd-bad ProvisioningFailed
check 07 "фаза PVC 10Gi" Pending "$(k get pvc csi-nrd-bad -o jsonpath='{.status.phase}')"
k delete pod csi-nrd-bad
k delete pvc csi-nrd-bad

pvc_pod csi-nrd yc-network-ssd-nonreplicated 93Gi "$N3"
pod_ready csi-nrd
check 07 "capacity PVC 93Gi" 93Gi "$(cap csi-nrd)"
ND=$(pv_disk csi-nrd)
d=$(disk_json "$ND")
check 07 "размер диска" 99857989632 "$(jq -r .size <<<"$d")"
check 07 "тип диска" network-ssd-nonreplicated "$(jq -r .type_id <<<"$d")"
check 07 "зона диска" "$ZONE2" "$(jq -r .zone_id <<<"$d")"

k patch pvc csi-nrd -p '{"spec":{"resources":{"requests":{"storage":"100Gi"}}}}'
ev_check "ресайз до 100Gi отклонён" csi-nrd VolumeResizeFailed
check 07 "размер диска после отказа" 99857989632 "$(disk_json "$ND" | jq -r .size)"

k patch pvc csi-nrd -p '{"spec":{"resources":{"requests":{"storage":"186Gi"}}}}'
wait_until 300 cap_is csi-nrd 186Gi || true
check 07 "capacity PVC после ресайза до 186Gi" 186Gi "$(cap csi-nrd)"
check 07 "размер диска после ресайза" 199715979264 "$(disk_json "$ND" | jq -r .size)"
check_ge 07 "размер ФС, КиБ (180 ГиБ)" $((180 * 1024 * 1024)) "$(fs_kib csi-nrd)"

k delete pod csi-nrd
k delete pvc csi-nrd
if wait_until 180 gone_disk "$ND"; then
  pass 07 "диск удалён вместе с PVC" "нет" "нет"
else
  fail 07 "диск удалён вместе с PVC" "нет" "$ND есть через 3 мин"
fi
