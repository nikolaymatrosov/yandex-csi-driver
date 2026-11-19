#!/usr/bin/env bash
# 01: CSINode topology keys and volume limit, node zone labels.
set -euo pipefail
# shellcheck source=../lib.sh
. "$(dirname "$0")/../lib.sh"
state_load

vm_ids=$(for v in cp w1 w2; do yc compute instance get "$P-$v" --format json | jq -r .id; done | sort | tr '\n' ' ')
nodes=$(k get nodes -o name | sed 's|^node/||' | sort | tr '\n' ' ')
check 01 "имена нод = ID ВМ" "$vm_ids" "$nodes"

for n in $nodes; do
  drv=$(k get csinode "$n" -o json | jq -c --arg d "$DRIVER" '[.spec.drivers[] | select(.name == $d)][0] // {}')
  check 01 "$n: драйвер в CSINode" "$DRIVER" "$(jq -r '.name // "нет"' <<<"$drv")"
  check 01 "$n: nodeID" "$n" "$(jq -r '.nodeID // ""' <<<"$drv")"
  check 01 "$n: topologyKeys" "failure-domain.beta.kubernetes.io/region,failure-domain.beta.kubernetes.io/zone" \
    "$(jq -r '.topologyKeys // [] | join(",")' <<<"$drv")"
  check 01 "$n: allocatable.count" 61 "$(jq -r '.allocatable.count // ""' <<<"$drv")"
  labels=$(k get node "$n" -o json | jq -c .metadata.labels)
  zone=$(yc compute instance get "$n" --format json | jq -r .zone_id)
  check 01 "$n: метка region" ru-central1 "$(jq -r '.["failure-domain.beta.kubernetes.io/region"] // ""' <<<"$labels")"
  check 01 "$n: метка zone" "$zone" "$(jq -r '.["failure-domain.beta.kubernetes.io/zone"] // ""' <<<"$labels")"
  info 01 "$n: topology.kubernetes.io/zone" "$(jq -r '.["topology.kubernetes.io/zone"] // "нет (CCM нет, ожидаемо)"' <<<"$labels")"
done
