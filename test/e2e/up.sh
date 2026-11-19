#!/usr/bin/env bash
# Create the e2e cluster (Talos on Yandex Cloud: cp + w1 in ZONE, w2 in ZONE2) and install the driver chart.
set -euo pipefail
# shellcheck source=lib.sh
. "$(dirname "$0")/lib.sh"
[ -f "$STATE" ] && die "$STATE exists: a cluster is already up (run down.sh first)"
state_set ZONE "$ZONE"
state_set ZONE2 "$ZONE2"
state_set E2E_VERSION "$E2E_VERSION"
FOLDER_ID=$(yc config get folder-id) # yc commands always use the profile's folder
[ -n "$FOLDER_ID" ] || die "the yc profile has no folder-id (yc config set folder-id …)"
SUBNET=default-$ZONE

addr() { yc vpc address create --name "$1" --external-ipv4 zone="$ZONE" --format json | jq -r .external_ipv4_address.address; }

sg_allow() { # IP: let a node's public IP reach Talos API, trustd and kube-apiserver
  yc vpc security-group update-rules "$P" \
    --add-rule "direction=ingress,port=50000,protocol=tcp,v4-cidrs=[$1/32]" \
    --add-rule "direction=ingress,port=50001,protocol=tcp,v4-cidrs=[$1/32]" \
    --add-rule "direction=ingress,port=6443,protocol=tcp,v4-cidrs=[$1/32]" >/dev/null
}

gen_config() { # the control plane IP goes into the API certificate, so regenerate after it changes
  talosctl gen config "$P" "https://$CP:6443" --additional-sans "$CP" -o "$WORK" --force \
    --install-disk /dev/vda --install-image "$TALOS_INSTALLER:$TALOS_VERSION" \
    --config-patch-control-plane @"$E2E_DIR/talos/single-node.yaml" \
    --config-patch @"$E2E_DIR/talos/ntp.yaml" \
    --config-patch-control-plane @"$E2E_DIR/talos/cilium.yaml" \
    --with-docs=false --with-examples=false
}

vm() { # NAME ZONE SUBNET CONFIG [STATIC_IP]
  local nat=nat-ip-version=ipv4
  if [ -n "${5:-}" ]; then nat="$nat,nat-address=$5"; fi
  yc compute instance create --name "$1" --zone "$2" --platform standard-v3 --cores 2 --memory 4 \
    --create-boot-disk "image-name=$TALOS_IMAGE,size=10,type=network-hdd,auto-delete=true" \
    --network-interface "subnet-name=$3,$nat,security-group-ids=$SG" \
    --metadata-from-file "user-data=$4" \
    --metadata-options aws-v1-http-endpoint=enabled >/dev/null
}

vm_ip() { yc compute instance get "$1" --format json | jq -r '.network_interfaces[0].primary_v4_address.one_to_one_nat.address'; }

# Some YC public IPs pass no TCP at all; Talos then logs "dial tcp ...: i/o timeout" from boot.
dead() { [ "$(yc compute instance get-serial-port-output "$1" | grep -c 'dial tcp.*i/o timeout' || true)" -gt 0 ]; }

new_cp() {
  yc vpc address delete "$P-cp" >/dev/null
  state_set CP "$(addr "$P-cp")"
  sg_allow "$CP"
  gen_config
  vm "$P-cp" "$ZONE" "$SUBNET" "$WORK/controlplane.yaml" "$CP"
}
new_w1() {
  yc vpc address delete "$P-w1" >/dev/null
  state_set W1 "$(addr "$P-w1")"
  sg_allow "$W1"
  vm "$P-w1" "$ZONE" "$SUBNET" "$WORK/worker.yaml" "$W1"
}
new_w2() { # ephemeral IP: known only after creation
  vm "$P-w2" "$ZONE2" "$SUBNET2" "$WORK/worker.yaml"
  state_set W2 "$(vm_ip "$P-w2")"
  sg_allow "$W2"
}

ensure_tcp() { # NAME RECREATE_FN: recreate the VM while its public IP is dead, at most 3 times
  local i
  for i in 1 2 3; do
    sleep 60
    dead "$1" || return 0
    log "$1: public IP passes no TCP, recreating ($i/3)"
    yc compute instance delete "$1" >/dev/null
    "$2"
  done
  sleep 60
  if dead "$1"; then die "$1: public IP still passes no TCP after 3 recreations"; fi
}

nodes_registered() { # all 3 nodes carry the zone label set by the CSI node plugin
  [ "$(kubectl get nodes -l failure-domain.beta.kubernetes.io/zone -o name 2>/dev/null | grep -c . || true)" = 3 ]
}
node_count_is_3() { [ "$(kubectl get nodes -o name 2>/dev/null | grep -c . || true)" = 3 ]; }

log "up: prefix $P, driver $E2E_VERSION, folder $FOLDER_ID"
MYIP=$(curl -fsS https://ipinfo.io/ip)

# 1. addresses and security group
state_set CP "$(addr "$P-cp")"
state_set W1 "$(addr "$P-w1")"
state_set SG "$(yc vpc security-group create --name "$P" --network-name default --format json \
  --rule "direction=ingress,port=50000,protocol=tcp,v4-cidrs=[$MYIP/32,$CP/32,$W1/32]" \
  --rule "direction=ingress,port=50001,protocol=tcp,v4-cidrs=[$CP/32,$W1/32]" \
  --rule "direction=ingress,port=6443,protocol=tcp,v4-cidrs=[$MYIP/32,$CP/32,$W1/32]" \
  --rule "direction=ingress,from-port=0,to-port=65535,protocol=any,predefined=self_security_group" \
  --rule "direction=egress,from-port=0,to-port=65535,protocol=any,v4-cidrs=[0.0.0.0/0]" | jq -r .id)"
if yc vpc subnet get "default-$ZONE2" >/dev/null 2>&1; then
  state_set SUBNET2 "default-$ZONE2"
else
  yc vpc subnet create --name "$P-$ZONE2" --zone "$ZONE2" --network-name default --range 10.132.0.0/24 >/dev/null
  state_set SUBNET2 "$P-$ZONE2"
fi

# service accounts first: folder roles take time to propagate before the chart starts using them
state_set SA "$(yc iam service-account create --name "$P-csi" --format json | jq -r .id)"
yc resource-manager folder add-access-binding "$FOLDER_ID" --role compute.editor --subject "serviceAccount:$SA" >/dev/null
yc iam key create --service-account-id "$SA" --output "$WORK/sa-key.json" >/dev/null
state_set SA_NODE "$(yc iam service-account create --name "$P-csi-node" --format json | jq -r .id)"
yc resource-manager folder add-access-binding "$FOLDER_ID" --role compute.viewer --subject "serviceAccount:$SA_NODE" >/dev/null
yc iam key create --service-account-id "$SA_NODE" --output "$WORK/sa-node-key.json" >/dev/null

# 2-4. Talos config and VMs; workers are created after cp has a live IP (their config points at it)
gen_config
vm "$P-cp" "$ZONE" "$SUBNET" "$WORK/controlplane.yaml" "$CP"
ensure_tcp "$P-cp" new_cp
vm "$P-w1" "$ZONE" "$SUBNET" "$WORK/worker.yaml" "$W1"
new_w2
ensure_tcp "$P-w1" new_w1
ensure_tcp "$P-w2" new_w2

# 5. cluster and Cilium
talosctl config endpoint "$CP"
talosctl config node "$CP"
retry 60 talosctl bootstrap
retry 60 talosctl kubeconfig "$KUBECONFIG" --force
wait_until 900 node_count_is_3 || die "3 nodes did not join"
retry 3 helm upgrade --install cilium cilium --repo https://helm.cilium.io --version "$CILIUM_VERSION" \
  -n kube-system -f "$E2E_DIR/talos/cilium-values.yaml"
k -n kube-system rollout status ds/cilium --timeout=10m
k apply -f "$E2E_DIR/talos/deny-cloud-metadata.yaml"
k wait --for=condition=Ready nodes --all --timeout=10m

# 6a. snapshot CRDs and controller (before the chart, or the VolumeSnapshotClass is not created)
k apply -k "github.com/kubernetes-csi/external-snapshotter/client/config/crd?ref=$SNAP_VERSION"
k apply -k "github.com/kubernetes-csi/external-snapshotter/deploy/kubernetes/snapshot-controller?ref=$SNAP_VERSION"
k -n kube-system rollout status deploy/snapshot-controller --timeout=10m

# 6. the driver chart
retry 3 helm upgrade --install yandex-csi "$CHART" --version "$CHART_VER" -n kube-system \
  --set serviceAccountJSON="$(base64 < "$WORK/sa-key.json" | tr -d '\n')" \
  --set nodeServiceAccountJSON="$(base64 < "$WORK/sa-node-key.json" | tr -d '\n')" \
  --set folderID="$(printf %s "$FOLDER_ID" | base64)"
k -n kube-system rollout status deploy/yandex-csi-controller --timeout=10m
k -n kube-system rollout status ds/yandex-csi-node --timeout=10m
wait_until 300 nodes_registered || die "nodes did not get zone labels from the CSI node plugin"

state_set KS_UID "$(k get ns kube-system -o jsonpath='{.metadata.uid}')"
zone_nodes() { k get nodes -l "failure-domain.beta.kubernetes.io/zone=$1" -o jsonpath='{.items[*].metadata.name}'; }
read -r na1 na2 _ <<<"$(zone_nodes "$ZONE")"
state_set NA1 "$na1"
state_set NA2 "$na2"
state_set N3 "$(zone_nodes "$ZONE2")"
log "up: done (NA1=$NA1 NA2=$NA2 N3=$N3)"
