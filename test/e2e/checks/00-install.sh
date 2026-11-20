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

# 00: driver installed from the chart: pods Ready, version, cluster UUID, provisioner flags.
set -euo pipefail
# shellcheck source=../lib.sh
. "$(dirname "$0")/../lib.sh"
state_load

containers_ready() { # app label -> "ready/total" containers over all its pods
  k -n kube-system get pods -l "app=$1" -o json |
    jq -r '[.items[] | (.status.containerStatuses // [])[]] | "\(map(select(.ready)) | length)/\(length)"'
}
check 00 "controller: контейнеры Ready" "5/5" "$(containers_ready yandex-csi-controller)"
check 00 "node: контейнеры Ready на 3 нодах" "6/6" "$(containers_ready yandex-csi-node)"

ver=$(ctrl_log | jq -Rrn '[inputs | fromjson? | objects | .version // empty] | first // ""')
check 00 "версия в логе csi-controller" "$E2E_VERSION" "$ver"

deploy=$(k -n kube-system get deploy yandex-csi-controller -o json)
arg() { # container flag -> value of --flag=value
  jq -r --arg c "$1" --arg f "--$2=" '.spec.template.spec.containers[] | select(.name == $c) | .args[] | select(startswith($f)) | ltrimstr($f)' <<<"$deploy"
}
check 00 "--cluster-uuid = UID kube-system" "$KS_UID" "$(arg csi-controller cluster-uuid)"
check 00 "provisioner --extra-create-metadata" true "$(arg csi-provisioner extra-create-metadata)"
plog=$(k -n kube-system logs deploy/yandex-csi-controller -c csi-provisioner)
check 00 "GetCapacity в логе csi-provisioner" 0 "$(grep -c GetCapacity <<<"$plog" || true)"
