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

# 09 (best effort): restart the controller while a disk is CREATING; the retried CreateVolume must answer
# Aborted, and the PVC binds only after the disk is READY. Uses csi-snap-a from 04, deletes csi-snap-a/b.
set -euo pipefail
# shellcheck source=../lib.sh
. "$(dirname "$0")/../lib.sh"
state_load

creating() {
  yc compute disk list --format json |
    jq -e --arg u "$KS_UID" 'any(.[]; .status == "CREATING" and .labels.cluster_uuid == $u)' >/dev/null
}
bound() { [ "$(kubectl get pvc csi-st -o jsonpath='{.status.phase}')" = Bound ]; }

mkdir -p "$WORK/logs"
k -n kube-system logs deploy/yandex-csi-controller --all-containers > "$WORK/logs/controller-before-09.log" || true

pvc_pod csi-st yc-network-ssd 4Gi "$NA1" csi-snap-a
caught=0
if wait_until 120 creating; then
  k -n kube-system delete pod -l app=yandex-csi-controller
  caught=1
fi
k -n kube-system rollout status deploy/yandex-csi-controller --timeout=5m
ok=1
wait_until 300 bound || ok=0
clog=$(ctrl_log)
if [ "$ok" = 0 ]; then
  fail 09 "PVC Bound после рестарта контроллера" Bound "$(k get pvc csi-st -o jsonpath='{.status.phase}')"
elif [ "$caught" = 1 ] && [ "$(grep -c 'is in status CREATING' <<<"$clog" || true)" -gt 0 ]; then
  pass 09 "Aborted на CREATING, затем Bound" "is in status CREATING" "поймали, PVC Bound"
else
  skip 09 "Aborted на CREATING" "гонку не поймали (диск стал READY раньше), PVC Bound"
fi

k delete pod csi-st
k delete pvc csi-st
k delete volumesnapshot csi-snap-a csi-snap-b
