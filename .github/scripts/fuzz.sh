#!/usr/bin/env bash
# Usage: fuzz.sh <package> <fuzz target> <fuzz time>
#
# Fuzzes one target and prints only the fuzzer's progress and the verdict. A
# failure message shows the input that failed, and a CI log is public.
set -euo pipefail

package=$1
target=$2
fuzztime=$3
log=$(mktemp)

status=0
go test -run '^$' -fuzz "^$target\$" -fuzztime "$fuzztime" "$package" >"$log" 2>&1 || status=$?
grep -E '^(fuzz: |--- FAIL|FAIL|ok |PASS$)' "$log" || true
if [ "$status" -ne 0 ]; then
  echo "::error::Fuzzing failed. Its input stays out of this public log: rerun go test -fuzz '^$target\$' $package locally."
  exit "$status"
fi
# go test passes when -fuzz matches no target, as after a rename.
if ! grep -qE '^fuzz: elapsed: .*execs:' "$log"; then
  echo "::error::$target did not fuzz. Check that -fuzz names a fuzz test in $package."
  exit 1
fi
