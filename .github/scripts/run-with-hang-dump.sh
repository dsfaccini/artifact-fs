#!/usr/bin/env bash
# Usage: run-with-hang-dump.sh SECONDS TEST_BINARY [ARG]...
#
# Runs a Go test binary with GOTRACEBACK=system. When it is still running after
# SECONDS, prints every process under it with the kernel stack of each thread,
# then sends the binary SIGQUIT, which makes the Go runtime print every
# goroutine and exit. When that exit stalls on the binary's own FUSE mount,
# aborts the artifact-fs FUSE connections. The binary's -test.timeout cannot
# stand in for the dump: its panic runs from a timer, and no timer runs while
# a stop-the-world waits on a thread that never stops.
set -u
shopt -s nullglob

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

# abort_fuse_connections aborts each FUSE connection whose mount is an
# artifact-fs mount, and each connection with no mount: a lazily detached
# mount leaves the mount table but keeps its connection while a request holds
# it. A connection's directory is named after its device number.
abort_fuse_connections() {
  local conn dev fstype
  for conn in /sys/fs/fuse/connections/*; do
    dev=${conn##*/}
    dev="$((dev >> 20)):$((dev & 0xfffff))"
    # The fstype is the first field after the " - " separator. The optional
    # fields before the separator vary in number.
    fstype=$(awk -v dev="$dev" '$3 == dev { for (i = 7; i < NF; i++) if ($i == "-") { print $(i + 1); exit } }' /proc/self/mountinfo)
    if [ -n "$fstype" ] && [ "$fstype" != fuse.artifact-fs ]; then
      continue
    fi
    echo "aborting FUSE connection ${conn##*/} (${fstype:-no mount})"
    echo 1 | sudo -n tee "$conn"/abort >/dev/null
  done
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
# kill sends the signal to the thread it names, unless that thread cannot take
# it. A thread that waits on a FUSE request the stopped server already read
# never handles a signal, and the main thread can be one. So SIGQUIT goes to a
# thread that waits on a futex, as an idle Go thread does.
quit_tid=
for p in $pids; do
  for task in /proc/"$p"/task/*; do
    echo "--- kernel stack of pid $p, thread ${task##*/} ($(cat "$task"/comm 2>/dev/null))"
    stack=$(sudo -n cat "$task"/stack)
    echo "$stack"
    if [ "$p" = "$pid" ] && [ -z "$quit_tid" ] && [[ $stack == *futex_wait* ]]; then
      quit_tid=${task##*/}
    fi
  done
done
kill -QUIT "${quit_tid:-$pid}"
if ! wait_for_exit 60; then
  # The binary cannot exit while one of its threads waits on a FUSE request
  # that the binary's own server read, until the connection aborts.
  echo "$1 outlived SIGQUIT by 60s; aborting its FUSE connections"
  abort_fuse_connections
  if ! wait_for_exit 30; then
    echo "$1 outlived the FUSE abort by 30s; killing it"
    kill -KILL "$pid"
  fi
fi
wait "$pid"
exit 124
