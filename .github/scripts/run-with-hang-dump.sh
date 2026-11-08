#!/usr/bin/env bash
# Usage: run-with-hang-dump.sh SECONDS TEST_BINARY [ARG]...
#
# Runs a Go test binary with GOTRACEBACK=system. When it is still running after
# SECONDS, prints every process under it with the kernel stack of each thread,
# then sends the binary SIGQUIT, which makes the Go runtime print every
# goroutine and exit. The binary's own -test.timeout cannot stand in for this:
# its panic runs from a timer, and no timer runs while a stop-the-world waits
# on a thread that never stops.
set -u

deadline=$1
shift
GOTRACEBACK=system "$@" &
pid=$!

# wait_for_exit returns 0 once the binary exits within $1 seconds.
wait_for_exit() {
  local waited
  for ((waited = 0; waited < $1; waited++)); do
    kill -0 "$pid" 2>/dev/null || return 0
    sleep 1
  done
  return 1
}

descendants() {
  local child
  for child in $(ps -o pid= --ppid "$1"); do
    echo "$child"
    descendants "$child"
  done
}

if wait_for_exit "$deadline"; then
  wait "$pid"
  exit
fi

echo "$1 still runs after ${deadline}s; dumping its processes, then sending it SIGQUIT"
pids=$(echo "$pid" $(descendants "$pid"))
ps -o pid,ppid,stat,wchan:32,args -p "${pids// /,}"
for p in $pids; do
  for task in /proc/"$p"/task/*; do
    echo "--- kernel stack of pid $p, thread ${task##*/} ($(cat "$task"/comm 2>/dev/null))"
    sudo -n cat "$task"/stack
  done
done
kill -QUIT "$pid"
if ! wait_for_exit 60; then
  echo "$1 outlived SIGQUIT by 60s; killing it"
  kill -KILL "$pid"
fi
wait "$pid"
exit 124
