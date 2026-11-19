#!/usr/bin/env bash
# 05: online PVC expansion 4Gi -> 8Gi on the running csi-test pod.
set -euo pipefail
# shellcheck source=../lib.sh
. "$(dirname "$0")/../lib.sh"
state_load

uid=$(k get pod csi-test -o jsonpath='{.metadata.uid}')
k patch pvc csi-test -p '{"spec":{"resources":{"requests":{"storage":"8Gi"}}}}'
wait_until 300 cap_is csi-test 8Gi || true
check 05 "capacity PVC" 8Gi "$(cap csi-test)"
check_ge 05 "размер ФС, КиБ (7.5 ГиБ)" $((15 * 512 * 1024)) "$(fs_kib csi-test)"
check 05 "UID пода не изменился" "$uid" "$(k get pod csi-test -o jsonpath='{.metadata.uid}')"
check 05 "рестарты пода" 0 "$(k get pod csi-test -o jsonpath='{.status.containerStatuses[0].restartCount}')"
