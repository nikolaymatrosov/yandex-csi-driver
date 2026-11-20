# shellcheck shell=bash disable=SC2034
# Shared settings and helpers for test/e2e. Sourced, never executed.
# Must run on bash 3.2 (macOS /bin/bash).

E2E_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "$E2E_DIR/../.." && pwd)

P=${E2E_PREFIX:-e2e}
ZONE=${ZONE:-ru-central1-a}
ZONE2=${ZONE2:-ru-central1-e}
E2E_VERSION=${E2E_VERSION:-$(cat "$REPO_ROOT/VERSION")}
CHART_VER=${E2E_VERSION#v}
CHART=oci://ghcr.io/nikolaymatrosov/charts/yandex-csi-controller
TALOS_VERSION=${TALOS_VERSION:-v1.14.6}
TALOS_IMAGE=${TALOS_IMAGE:-talos-metal-yc-v1-14-6}
TALOS_INSTALLER=${TALOS_INSTALLER:-factory.talos.dev/metal-installer/b68af66f6cc572d8d192786b4c0f38890ca5a03150160aa6efc5462ba4d55503}
CILIUM_VERSION=1.20.4
SNAP_VERSION=v8.7.0
DRIVER=yandex.csi.nikolaymatrosov.ru

WORK=$E2E_DIR/_work/$P
STATE=$WORK/state.env
RESULTS=$WORK/results.md
mkdir -p "$WORK"
export KUBECONFIG=$WORK/kubeconfig TALOSCONFIG=$WORK/talosconfig

# Written by up.sh into state.env; defaults keep `set -u` and shellcheck quiet.
CP=${CP:-} W1=${W1:-} W2=${W2:-} SG=${SG:-} SUBNET2=${SUBNET2:-}
SA=${SA:-} SA_NODE=${SA_NODE:-} KS_UID=${KS_UID:-} NA1=${NA1:-} NA2=${NA2:-} N3=${N3:-}
PG_SPREAD=${PG_SPREAD:-} PG_PART=${PG_PART:-} # written by check 03

log() { printf '%s %s\n' "$(date +%H:%M:%S)" "$*" >&2; }
die() { log "FATAL: $*"; exit 1; }

# Name the failing command when a script stops on an error (set -e) or a step fails (down.sh).
set -E
trap 'log "ERR ${BASH_SOURCE[0]##*/}:$LINENO: $BASH_COMMAND${FUNCNAME:+ (via ${FUNCNAME[*]}, lines ${BASH_LINENO[*]})}"' ERR

# state_set KEY VALUE: set the variable here and persist it for later scripts.
state_set() {
  case $2 in '' | null) die "state_set $1: empty value" ;; esac
  touch "$STATE"
  grep -v "^$1=" "$STATE" > "$STATE.tmp" || true
  printf '%s=%q\n' "$1" "$2" >> "$STATE.tmp"
  mv "$STATE.tmp" "$STATE"
  eval "$1=\$2"
}

state_load() {
  if [ -f "$STATE" ]; then
    # shellcheck source=/dev/null
    . "$STATE"
  fi
}

# --- results.md rows: | id | check | expected | actual | status |
_cell() { printf '%s' "$1" | tr '\n' ' ' | sed 's/|/\\|/g'; }
_row() {
  printf '| %s | %s | %s | %s | %s |\n' "$1" "$(_cell "$2")" "$(_cell "$3")" "$(_cell "$4")" "$5" >> "$RESULTS"
  log "$5 $1 $2: $4"
}
check() { if [ "$3" = "$4" ]; then _row "$1" "$2" "$3" "$4" PASS; else _row "$1" "$2" "$3" "$4" FAIL; fi; }
check_ge() { # id desc min actual: PASS when actual >= min (integers)
  if [ "${4:-0}" -ge "$3" ] 2>/dev/null; then _row "$1" "$2" "≥ $3" "$4" PASS; else _row "$1" "$2" "≥ $3" "$4" FAIL; fi
}
pass() { _row "$1" "$2" "$3" "$4" PASS; }
fail() { _row "$1" "$2" "$3" "$4" FAIL; }
info() { _row "$1" "$2" "" "$3" INFO; }
skip() { _row "$1" "$2" "" "$3" SKIP; }

# wait_until SECONDS CMD...: poll every 5 s until CMD succeeds; 1 on timeout.
wait_until() {
  local deadline
  deadline=$(($(date +%s) + $1))
  shift
  until "$@" >/dev/null 2>&1; do
    [ "$(date +%s)" -ge "$deadline" ] && return 1
    sleep 5
  done
}

# retry N CMD...: up to N attempts, 5 s apart.
retry() {
  local n=$1 i=1
  shift
  until "$@"; do
    [ "$i" -ge "$n" ] && return 1
    i=$((i + 1))
    sleep 5
  done
}

# --- kubernetes
k() { retry 3 kubectl "$@"; }
apply() { # manifest on stdin; saved to a file so retries can re-read it
  local f
  f=$(mktemp "$WORK/manifest.XXXXXX")
  cat > "$f"
  k apply -f "$f"
}
pod_ready() { k wait "pod/$1" --for=condition=Ready --timeout=5m >/dev/null; }
pv_disk() { k get pv "$(k get pvc "$1" -o jsonpath='{.spec.volumeName}')" -o jsonpath='{.spec.csi.volumeHandle}'; }
cap() { k get pvc "$1" -o jsonpath='{.status.capacity.storage}'; }
cap_is() { [ "$(cap "$1")" = "$2" ]; }
fs_kib() { k exec "$1" -- df -kP /data | awk 'NR==2 {print $2}'; }
ctrl_log() { k -n kube-system logs deploy/yandex-csi-controller -c csi-controller; }
ctrl_restarts() {
  k -n kube-system get pods -l app=yandex-csi-controller -o json |
    jq '[.items[] | (.status.containerStatuses // [])[] | select(.name=="csi-controller") | .restartCount] | add // 0'
}

# pvc_pod NAME STORAGECLASS SIZE NODE [SNAPSHOT]: PVC (optionally from a VolumeSnapshot) and a busybox pod on NODE.
pvc_pod() {
  local src=""
  if [ -n "${5:-}" ]; then
    src="  dataSource: {apiGroup: snapshot.storage.k8s.io, kind: VolumeSnapshot, name: $5}"
  fi
  apply <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: $1}
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: $2
  resources: {requests: {storage: $3}}
$src
---
apiVersion: v1
kind: Pod
metadata: {name: $1}
spec:
  nodeSelector: {kubernetes.io/hostname: $4}
  terminationGracePeriodSeconds: 1
  containers:
  - {name: c, image: busybox:1.37, command: [sleep, "3600"], volumeMounts: [{name: d, mountPath: /data}]}
  volumes: [{name: d, persistentVolumeClaim: {claimName: $1}}]
EOF
}

vsnap() { # NAME PVC: VolumeSnapshot NAME of PVC
  apply <<EOF
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata: {name: $1}
spec: {volumeSnapshotClassName: yc-csi-snapclass, source: {persistentVolumeClaimName: $2}}
EOF
}
snap_ready() { [ "$(kubectl get volumesnapshot "$1" -o jsonpath='{.status.readyToUse}')" = true ]; }
snap_content() { k get volumesnapshot "$1" -o jsonpath='{.status.boundVolumeSnapshotContentName}'; }
snap_handle() { k get volumesnapshotcontent "$(snap_content "$1")" -o jsonpath='{.status.snapshotHandle}'; }

# --- yandex cloud
disk_json() { yc compute disk get "$1" --format json; }
gone_disk() { ! yc compute disk get "$1" >/dev/null 2>&1; }
gone_snapshot() { ! yc compute snapshot get "$1" >/dev/null 2>&1; }
