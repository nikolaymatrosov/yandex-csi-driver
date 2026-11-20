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

# 02: data survives moving the pod to the other node of the zone; PVC labels on the cloud disk.
set -euo pipefail
# shellcheck source=../lib.sh
. "$(dirname "$0")/../lib.sh"
state_load

test_pod() { sed "s/__ZONE__/$ZONE/" "$E2E_DIR/manifests/test.yaml" | apply; }

test_pod
pod_ready csi-test
N1=$(k get pod csi-test -o jsonpath='{.spec.nodeName}')
DISK=$(pv_disk csi-test)
d=$(disk_json "$DISK")
check 02 "метка диска cluster_uuid" "$KS_UID" "$(jq -r '.labels.cluster_uuid // ""' <<<"$d")"
check 02 "метка диска csi-pvc-name" csi-test "$(jq -r '.labels["csi-pvc-name"] // ""' <<<"$d")"
check 02 "метка диска csi-pvc-namespace" default "$(jq -r '.labels["csi-pvc-namespace"] // ""' <<<"$d")"

marker=$(k exec csi-test -- cat /data/marker)
k exec csi-test -- sh -c 'echo hello > /data/check; sync'
k delete pod csi-test
k cordon "$N1"
trap 'kubectl uncordon "$N1" >/dev/null 2>&1 || true' EXIT
start=$(date +%s)
test_pod
pod_ready csi-test
info 02 "секунд до Ready на новой ноде" "$(($(date +%s) - start))"
N2=$(k get pod csi-test -o jsonpath='{.spec.nodeName}')
if [ "$N2" != "$N1" ]; then pass 02 "под переехал" "N2 ≠ N1" "$N1 → $N2"; else fail 02 "под переехал" "N2 ≠ N1" "$N2"; fi
check 02 "marker после переезда" "$marker" "$(k exec csi-test -- cat /data/marker)"
check 02 "check после переезда" hello "$(k exec csi-test -- cat /data/check)"
check 02 "диск подключён к N2" "$N2" "$(disk_json "$DISK" | jq -r '.instance_ids // [] | join(",")')"
info 02 "событий Multi-Attach" "$(k get events --field-selector involvedObject.name=csi-test -o json |
  jq '[.items[] | select(.message | test("Multi-Attach"))] | length')"
