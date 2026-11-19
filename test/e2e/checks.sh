#!/usr/bin/env bash
# Run the acceptance checks against the cluster from up.sh: all of them, or only the given numbers (checks.sh 02 08).
# Writes $WORK/results.md, saves driver logs to $WORK/logs/, exits 1 if any check failed.
set -euo pipefail
# shellcheck source=lib.sh
. "$(dirname "$0")/lib.sh"
[ -f "$STATE" ] || die "no $STATE: run up.sh first"

nums="$*"
if [ -z "$nums" ]; then
  for f in "$E2E_DIR"/checks/*.sh; do
    n=$(basename "$f")
    nums="$nums ${n%%-*}"
  done
fi

[ ! -f "$RESULTS" ] || mv "$RESULTS" "$RESULTS.$(date +%Y%m%d-%H%M%S)"
{
  printf '# e2e %s: %s, %s\n\n' "$P" "$E2E_VERSION" "$(date '+%Y-%m-%d %H:%M')"
  printf '| id | проверка | ожидание | факт | статус |\n|---|---|---|---|---|\n'
} > "$RESULTS"

for n in $nums; do
  f=$(find "$E2E_DIR/checks" -name "$n-*.sh" | sort | sed -n 1p)
  [ -n "$f" ] || die "no check $n"
  log "=== $(basename "$f")"
  rc=0
  bash -euo pipefail "$f" 2>&1 | tee "$WORK/check-$n.log" || rc=$?
  if [ "$rc" != 0 ]; then
    fail "$n" "$(basename "$f" .sh) прервана" "exit 0" "exit $rc: $(tail -n 1 "$WORK/check-$n.log")"
  fi
done

mkdir -p "$WORK/logs"
kubectl -n kube-system logs deploy/yandex-csi-controller --all-containers > "$WORK/logs/controller.log" 2>&1 || true
for pod in $(kubectl -n kube-system get pods -l app=yandex-csi-node -o name 2>/dev/null); do
  kubectl -n kube-system logs "$pod" --all-containers > "$WORK/logs/${pod#pod/}.log" 2>&1 || true
done

count() { grep -c "| $1 |\$" "$RESULTS" || true; }
failed=$(count FAIL)
printf '\nPASS %s, FAIL %s, SKIP %s, INFO %s\n' "$(count PASS)" "$failed" "$(count SKIP)" "$(count INFO)" >> "$RESULTS"
cat "$RESULTS"
[ "$failed" = 0 ]
