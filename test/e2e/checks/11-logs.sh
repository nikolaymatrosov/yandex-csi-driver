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

# 11 (last): driver logs are JSON with distinct keys, no known-bad messages, no restarts during the run.
set -euo pipefail
# shellcheck source=../lib.sh
. "$(dirname "$0")/../lib.sh"
state_load

logs=$(ctrl_log)
node_pods=$(k -n kube-system get pods -l app=yandex-csi-node -o name)
check 11 "node-поды для логов" 3 "$(wc -w <<<"$node_pods" | tr -d ' ')"
for p in $node_pods; do
  logs="$logs"$'\n'"$(k -n kube-system logs "$p" -c yandex-csi-node)"
done
lines() { printf '%s\n' "$logs"; }

total=$(lines | grep -c . || true)
json=$(lines | jq -Rc 'fromjson? | objects' | grep -c . || true)
check 11 "строки логов драйвера — JSON-объекты" "$total" "$json"
if [ "$total" != "$json" ]; then
  info 11 "первая не-JSON строка" "$(lines | jq -Rr 'select((try fromjson catch null | type) != "object")' | sed -n 1p)"
fi

read -r n_cv dup miss <<<"$(lines | awk '/"msg":"create volume called"/ {
  n++
  if (split($0, parts, "\"zone\":") - 1 != 1) dup++
  if ($0 !~ /"volume_region":/ || $0 !~ /"volume_zone":/) miss++
} END { printf "%d %d %d", n, dup, miss }')"
check_ge 11 "строк create volume called" 1 "$n_cv"
check 11 "create volume: строк с дублем ключа zone" 0 "$dup"
check 11 "create volume: строк без volume_region/volume_zone" 0 "$miss"

for pat in 'expected operation response to be' too_many_pings Unauthenticated PermissionDenied; do
  check 11 "нет «${pat}» в логах" 0 "$(lines | grep -c "$pat" || true)"
done

pods=$(k -n kube-system get pods -l 'app in (yandex-csi-controller,yandex-csi-node)' -o json)
drv='.name == "csi-controller" or .name == "yandex-csi-node"'
check 11 "рестарты контейнеров драйвера" 0 \
  "$(jq "[.items[] | (.status.containerStatuses // [])[] | select($drv) | .restartCount] | add // 0" <<<"$pods")"
side=$(jq "[.items[] | (.status.containerStatuses // [])[] | select(($drv) | not) | .restartCount] | max // 0" <<<"$pods")
if [ "$side" -le 1 ]; then pass 11 "рестарты сайдкаров" "≤ 1" "$side"; else fail 11 "рестарты сайдкаров" "≤ 1" "$side"; fi
info 11 "строк level=error" "$(lines | grep -c '"level":"error"' || true)"
