#!/usr/bin/env bash
# Delete everything the e2e run created. Works after a failure at any step and without state.env:
# resources are found by name prefix ($E2E_PREFIX) and by the driver's cluster_uuid label.
set -uo pipefail # no -e: every step runs even if the previous one failed
# shellcheck source=lib.sh
. "$(dirname "$0")/lib.sh"
state_load
FOLDER_ID=$(yc config get folder-id) # yc commands always use the profile's folder
[ -n "$FOLDER_ID" ] || die "the yc profile has no folder-id (yc config set folder-id …)"
LIST_FAILED=$WORK/down.list-failed

named() { # yc list JSON on stdin -> ids of items named $P or $P-*
  jq -r --arg p "$P" '.[] | (.name // "") as $n | select($n == $p or ($n | startswith($p + "-"))) | .id'
}
labeled() { # yc list JSON on stdin -> ids of items labeled cluster_uuid=$KS_UID
  jq -r --arg u "$KS_UID" '.[] | select($u != "" and .labels.cluster_uuid == $u) | .id'
}
yc_list() { # yc args...: list JSON; on failure print [] and leave a mark (works from subshells and pipes)
  local out
  if out=$(yc "$@" --format json); then printf '%s\n' "$out"; else log "down: yc $* failed"; touch "$LIST_FAILED"; echo '[]'; fi
}
driver_leftovers() {
  { yc_list compute disk list | labeled; yc_list compute snapshot list | labeled; } | grep -c . || true
}
no_driver_leftovers() { [ "$(driver_leftovers)" = 0 ]; }
no_vms() { [ -z "$(yc_list compute instance list | named)" ]; }

log "down: prefix $P, folder $FOLDER_ID"

# 1. let the driver delete its own disks and snapshots while the cluster is alive
if kubectl get ns kube-system >/dev/null 2>&1; then
  if [ -z "$KS_UID" ]; then KS_UID=$(kubectl get ns kube-system -o jsonpath='{.metadata.uid}'); fi
  log "down: deleting pods, VolumeSnapshots and PVCs"
  kubectl delete pod --all --grace-period=1 --wait=false
  kubectl delete volumesnapshot --all --wait=false
  kubectl delete pvc --all --wait=false
  if [ -n "$KS_UID" ] && ! wait_until 300 no_driver_leftovers; then
    log "down: driver did not clean up in 5 min, deleting its disks and snapshots directly"
  fi
fi

# 2. VMs (boot disks go with them; CSI disks get detached)
for id in $(yc_list compute instance list | named); do
  yc compute instance delete "$id" --async >/dev/null
done
wait_until 600 no_vms || log "down: VMs still present after 10 min"

# 3. disks and snapshots the driver left behind
for id in $( { yc_list compute disk list | labeled; yc_list compute disk list | named; } | sort -u); do
  yc compute disk delete "$id" >/dev/null
done
for id in $(yc_list compute snapshot list | labeled); do
  yc compute snapshot delete "$id" >/dev/null
done
# a placement group with disks cannot be deleted, so this comes after the disks
for id in $(yc_list compute disk-placement-group list | named); do yc compute disk-placement-group delete "$id" >/dev/null; done

# 4. network, KMS, IAM
for id in $(yc_list vpc address list | named); do yc vpc address delete "$id" >/dev/null; done
for id in $(yc_list vpc security-group list | named); do yc vpc security-group delete "$id" >/dev/null; done
for id in $(yc_list vpc subnet list | named); do yc vpc subnet delete "$id" >/dev/null; done
for id in $(yc_list kms symmetric-key list | named); do yc kms symmetric-key delete "$id" >/dev/null; done
for id in $(yc_list iam service-account list | named); do
  for role in compute.editor compute.viewer; do
    yc resource-manager folder remove-access-binding "$FOLDER_ID" --role "$role" --subject "serviceAccount:$id" >/dev/null 2>&1
  done
  yc iam service-account delete "$id" >/dev/null
done

# 5. leftovers
rm -f "$LIST_FAILED" # step 5 relists everything, so an earlier skipped deletion still shows up
left=$({
  yc_list compute instance list | named
  yc_list compute disk list | named
  yc_list compute disk list | labeled
  yc_list compute snapshot list | labeled
  yc_list compute disk-placement-group list | named
  yc_list vpc address list | named
  yc_list vpc security-group list | named
  yc_list vpc subnet list | named
  yc_list kms symmetric-key list | jq '[.[] | select(.status == "ACTIVE" or .status == "INACTIVE")]' | named
  yc_list iam service-account list | named
} | sort -u | tr '\n' ' ')
if [ -f "$LIST_FAILED" ]; then log "down: some yc list calls failed, cleanup not confirmed"; exit 1; fi
if [ -n "${left// /}" ]; then
  log "down: LEFTOVERS: $left"
  exit 1
fi
rm -f "$STATE" "$WORK/sa-key.json" "$WORK/sa-node-key.json"
log "down: clean"
