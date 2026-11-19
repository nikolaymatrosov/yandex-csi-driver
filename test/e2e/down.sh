#!/usr/bin/env bash
# Delete everything the e2e run created. Works after a failure at any step and without state.env:
# resources are found by name prefix ($E2E_PREFIX) and by the driver's cluster_uuid label.
set -uo pipefail # no -e: every step runs even if the previous one failed
# shellcheck source=lib.sh
. "$(dirname "$0")/lib.sh"
state_load
FOLDER_ID=${FOLDER_ID:-$(yc config get folder-id)}

named() { # yc list JSON on stdin -> ids of items named $P or $P-*
  jq -r --arg p "$P" '.[] | (.name // "") as $n | select($n == $p or ($n | startswith($p + "-"))) | .id'
}
labeled() { # yc list JSON on stdin -> ids of items labeled cluster_uuid=$KS_UID
  jq -r --arg u "$KS_UID" '.[] | select($u != "" and .labels.cluster_uuid == $u) | .id'
}
driver_leftovers() {
  { yc compute disk list --format json | labeled; yc compute snapshot list --format json | labeled; } | grep -c . || true
}
no_driver_leftovers() { [ "$(driver_leftovers)" = 0 ]; }
no_vms() { [ -z "$(yc compute instance list --format json | named)" ]; }

log "down: prefix $P, folder $FOLDER_ID"

# 1. let the driver delete its own disks and snapshots while the cluster is alive
if kubectl get ns kube-system >/dev/null 2>&1; then
  log "down: deleting pods, VolumeSnapshots and PVCs"
  kubectl delete pod --all --grace-period=1 --wait=false
  kubectl delete volumesnapshot --all --wait=false
  kubectl delete pvc --all --wait=false
  if [ -n "$KS_UID" ] && ! wait_until 300 no_driver_leftovers; then
    log "down: driver did not clean up in 5 min, deleting its disks and snapshots directly"
  fi
fi

# 2. VMs (boot disks go with them; CSI disks get detached)
for id in $(yc compute instance list --format json | named); do
  yc compute instance delete "$id" --async >/dev/null
done
wait_until 600 no_vms || log "down: VMs still present after 10 min"

# 3. disks and snapshots the driver left behind
for id in $( { yc compute disk list --format json | labeled; yc compute disk list --format json | named; } | sort -u); do
  yc compute disk delete "$id" >/dev/null
done
for id in $(yc compute snapshot list --format json | labeled); do
  yc compute snapshot delete "$id" >/dev/null
done

# 4. network, KMS, IAM
for id in $(yc vpc address list --format json | named); do yc vpc address delete "$id" >/dev/null; done
for id in $(yc vpc security-group list --format json | named); do yc vpc security-group delete "$id" >/dev/null; done
for id in $(yc vpc subnet list --format json | named); do yc vpc subnet delete "$id" >/dev/null; done
for id in $(yc kms symmetric-key list --format json | named); do yc kms symmetric-key delete "$id" >/dev/null; done
for id in $(yc iam service-account list --format json | named); do
  for role in compute.editor compute.viewer; do
    yc resource-manager folder remove-access-binding "$FOLDER_ID" --role "$role" --subject "serviceAccount:$id" >/dev/null 2>&1
  done
  yc iam service-account delete "$id" >/dev/null
done

# 5. leftovers
left=$({
  yc compute instance list --format json | named
  yc compute disk list --format json | named
  yc compute disk list --format json | labeled
  yc compute snapshot list --format json | labeled
  yc vpc address list --format json | named
  yc vpc security-group list --format json | named
  yc vpc subnet list --format json | named
  yc kms symmetric-key list --format json | jq '[.[] | select(.status == "ACTIVE" or .status == "INACTIVE")]' | named
  yc iam service-account list --format json | named
} | sort -u | tr '\n' ' ')
if [ -n "${left// /}" ]; then
  log "down: LEFTOVERS: $left"
  exit 1
fi
rm -f "$STATE" "$WORK/sa-key.json" "$WORK/sa-node-key.json"
log "down: clean"
