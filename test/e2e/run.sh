#!/usr/bin/env bash
# Full acceptance run: up.sh, checks.sh, then down.sh, which always runs unless KEEP=1.
# Exit code: that of up.sh or checks.sh; 1 if they passed but down.sh left resources behind.
set -euo pipefail
dir=$(cd "$(dirname "$0")" && pwd)

finish() {
  local rc=$? # keep the exit code: bash 3.2 traps may reset it
  if [ "${KEEP:-}" = 1 ]; then
    echo "KEEP=1: cluster left running; delete it with $dir/down.sh" >&2
  elif ! "$dir/down.sh"; then
    [ "$rc" != 0 ] || rc=1
  fi
  exit "$rc"
}
trap finish EXIT
trap 'exit 130' INT TERM

"$dir/up.sh"
"$dir/checks.sh"
