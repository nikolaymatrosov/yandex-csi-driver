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
