#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
mount_script=${1:-$script_dir/container_src/mount-artifact-fs-repo.sh}
tmp_dir=$(mktemp -d)
daemon_pid=''

cleanup() {
  if [ -n "$daemon_pid" ]; then
    kill "$daemon_pid" 2>/dev/null || true
  fi
  rm -rf "$tmp_dir"
}
trap cleanup EXIT

mkdir -p "$tmp_dir/bin" "$tmp_dir/mnt/sandbox-sdk/.git"
touch "$tmp_dir/fuse"
sed "s#/dev/fuse#$tmp_dir/fuse#" "$mount_script" >"$tmp_dir/mount-script.sh"

cat >"$tmp_dir/bin/artifact-fs" <<'EOF'
#!/usr/bin/env bash
if [ "${1:-}" = 'status' ]; then
  exit 1
fi
if [ "${1:-}" = 'daemon' ]; then
  exec sleep 60
fi
EOF
chmod +x "$tmp_dir/bin/artifact-fs"

cat >"$tmp_dir/bin/git" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
chmod +x "$tmp_dir/bin/git"

PATH="$tmp_dir/bin:$PATH" \
  MOUNT_GIT_REMOTE='https://example.test/sandbox-sdk.git' \
  MOUNT_GIT_BRANCH='main' \
  ARTIFACT_FS_ROOT="$tmp_dir/root" \
  MOUNT_ROOT="$tmp_dir/mnt" \
  ARTIFACT_FS_MOUNT_METADATA_FILE="$tmp_dir/metadata" \
  ARTIFACT_FS_DAEMON_PID_FILE="$tmp_dir/daemon.pid" \
  ARTIFACT_FS_DAEMON_LOG="$tmp_dir/daemon.log" \
  ARTIFACT_FS_MOUNT_LOCK_FILE="$tmp_dir/mount.lock" \
  bash "$tmp_dir/mount-script.sh" >/dev/null

daemon_pid=$(cat "$tmp_dir/daemon.pid")
if ! kill -0 "$daemon_pid"; then
  echo 'ArtifactFS daemon did not survive bootstrap' >&2
  exit 1
fi
if ! flock -n "$tmp_dir/mount.lock" true; then
  echo "ArtifactFS daemon $daemon_pid still holds the mount lock" >&2
  exit 1
fi
printf 'daemon %s remains running and the mount lock is available\n' "$daemon_pid"
