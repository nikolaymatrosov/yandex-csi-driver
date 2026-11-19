#!/usr/bin/env bash
# 07: non-replicated disks are rounded up to multiples of 93 GiB on create and on resize.
set -euo pipefail
# shellcheck source=../lib.sh
. "$(dirname "$0")/../lib.sh"
state_load

pvc_pod csi-nrd yc-network-ssd-nonreplicated 10Gi "$N3"
pod_ready csi-nrd
check 07 "capacity PVC 10Gi" 93Gi "$(cap csi-nrd)"
ND=$(pv_disk csi-nrd)
d=$(disk_json "$ND")
check 07 "размер диска" 99857989632 "$(jq -r .size <<<"$d")"
check 07 "тип диска" network-ssd-nonreplicated "$(jq -r .type_id <<<"$d")"
check 07 "зона диска" "$ZONE2" "$(jq -r .zone_id <<<"$d")"

k patch pvc csi-nrd -p '{"spec":{"resources":{"requests":{"storage":"100Gi"}}}}'
wait_until 300 cap_is csi-nrd 186Gi || true
check 07 "capacity PVC после ресайза до 100Gi" 186Gi "$(cap csi-nrd)"
check 07 "размер диска после ресайза" 199715979264 "$(disk_json "$ND" | jq -r .size)"
check_ge 07 "размер ФС, КиБ (180 ГиБ)" $((180 * 1024 * 1024)) "$(fs_kib csi-nrd)"

k delete pod csi-nrd
k delete pvc csi-nrd
if wait_until 180 gone_disk "$ND"; then
  pass 07 "диск удалён вместе с PVC" "нет" "нет"
else
  fail 07 "диск удалён вместе с PVC" "нет" "$ND есть через 3 мин"
fi
