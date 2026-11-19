#!/usr/bin/env bash
# 11 (last): driver logs are JSON with distinct keys, no known-bad messages, no restarts during the run.
set -euo pipefail
# shellcheck source=../lib.sh
. "$(dirname "$0")/../lib.sh"
state_load

logs=$({
  ctrl_log
  for p in $(k -n kube-system get pods -l app=yandex-csi-node -o name); do
    k -n kube-system logs "$p" -c yandex-csi-node
  done
})
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
  check 11 "нет «$pat» в логах" 0 "$(lines | grep -c "$pat" || true)"
done

pods=$(k -n kube-system get pods -l 'app in (yandex-csi-controller,yandex-csi-node)' -o json)
drv='.name == "csi-controller" or .name == "yandex-csi-node"'
check 11 "рестарты контейнеров драйвера" 0 \
  "$(jq "[.items[].status.containerStatuses[] | select($drv) | .restartCount] | add // 0" <<<"$pods")"
side=$(jq "[.items[].status.containerStatuses[] | select(($drv) | not) | .restartCount] | max // 0" <<<"$pods")
if [ "$side" -le 1 ]; then pass 11 "рестарты сайдкаров" "≤ 1" "$side"; else fail 11 "рестарты сайдкаров" "≤ 1" "$side"; fi
info 11 "строк level=error" "$(lines | grep -c '"level":"error"' || true)"
