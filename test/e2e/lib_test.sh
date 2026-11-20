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

# Offline self-check of lib.sh helpers (no cloud, no cluster): bash test/e2e/lib_test.sh
set -euo pipefail
export E2E_PREFIX=libtest-$$
# shellcheck source=lib.sh
. "$(dirname "$0")/lib.sh"
cleanup() { local rc=$?; rm -rf "$WORK"; exit "$rc"; } # keep the exit code: bash 3.2 traps may reset it
trap cleanup EXIT

expect() { # name actual expected
  if [ "$2" = "$3" ]; then printf 'ok   %s\n' "$1"; else printf 'FAIL %s\n  got:  %s\n  want: %s\n' "$1" "$2" "$3"; exit 1; fi
}

state_set FOO 'a b'
state_set FOO 'c d|e'
expect "state_set keeps one line per key" "$(grep -c '^FOO=' "$STATE")" 1
FOO=
state_load
expect "state roundtrip" "$FOO" 'c d|e'
if (state_set EMPTY '' 2>/dev/null); then expect "state_set rejects empty" 0 1; else expect "state_set rejects empty" 1 1; fi

: > "$RESULTS"
{
  check 1 eq x x
  check 2 ne x y
  check_ge 3 ge 5 7
  check_ge 4 empty 5 ''
  info 5 'pipe|cell' 'multi
line'
} 2>/dev/null
expect "check equal → PASS" "$(sed -n 1p "$RESULTS")" '| 1 | eq | x | x | PASS |'
expect "check differ → FAIL" "$(sed -n 2p "$RESULTS")" '| 2 | ne | x | y | FAIL |'
expect "check_ge" "$(sed -n 3p "$RESULTS")" '| 3 | ge | ≥ 5 | 7 | PASS |'
expect "check_ge empty → FAIL" "$(sed -n 4p "$RESULTS")" '| 4 | empty | ≥ 5 |  | FAIL |'
expect "cells escape | and newlines" "$(sed -n 5p "$RESULTS")" '| 5 | pipe\|cell |  | multi line | INFO |'

if wait_until 10 true; then expect "wait_until success" 0 0; else expect "wait_until success" 1 0; fi
if wait_until 1 false; then expect "wait_until timeout" 0 1; else expect "wait_until timeout" 1 1; fi
if retry 2 false; then expect "retry gives up" 0 1; else expect "retry gives up" 1 1; fi
