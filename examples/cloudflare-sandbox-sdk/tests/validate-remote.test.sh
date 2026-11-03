#!/usr/bin/env bash
set -euo pipefail

# The mount script runs in a Linux container. macOS ships no flock, so local
# runs there skip; CI must never skip.
if ! command -v flock >/dev/null 2>&1; then
  echo "skip: flock (util-linux) not found; CI runs this test on Linux" >&2
  [ -z "${CI:-}" ] || exit 1
  exit 0
fi

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
mount_script=${1:-$script_dir/container_src/mount-artifact-fs-repo.sh}
tmp_dir=$(mktemp -d)
trap 'rm -rf "$tmp_dir"' EXIT

mkdir -p "$tmp_dir/bin"
touch "$tmp_dir/fuse"
sed "s#/dev/fuse#$tmp_dir/fuse#" "$mount_script" >"$tmp_dir/mount-script.sh"

for tool in artifact-fs git; do
  cat >"$tmp_dir/bin/$tool" <<'EOF'
#!/usr/bin/env bash
: >"$TOOL_INVOKED_MARKER"
exit 0
EOF
  chmod +x "$tmp_dir/bin/$tool"
done

# Git parses a remote that starts with '-' as an option.
status=0
PATH="$tmp_dir/bin:$PATH" \
  TOOL_INVOKED_MARKER="$tmp_dir/tool-invoked" \
  MOUNT_GIT_REMOTE='--upload-pack=a@b:c' \
  MOUNT_GIT_BRANCH='main' \
  ARTIFACT_FS_ROOT="$tmp_dir/root" \
  MOUNT_ROOT="$tmp_dir/mnt" \
  ARTIFACT_FS_MOUNT_METADATA_FILE="$tmp_dir/metadata" \
  ARTIFACT_FS_DAEMON_PID_FILE="$tmp_dir/daemon.pid" \
  ARTIFACT_FS_DAEMON_LOG="$tmp_dir/daemon.log" \
  ARTIFACT_FS_MOUNT_LOCK_FILE="$tmp_dir/mount.lock" \
  bash "$tmp_dir/mount-script.sh" >/dev/null 2>"$tmp_dir/stderr" || status=$?

if [ "$status" -eq 0 ]; then
  echo 'mount script accepted a remote that starts with -' >&2
  exit 1
fi
if ! grep -q "MOUNT_GIT_REMOTE must not start with '-'" "$tmp_dir/stderr"; then
  echo 'mount script did not report the leading-dash rejection:' >&2
  cat "$tmp_dir/stderr" >&2
  exit 1
fi
if [ -e "$tmp_dir/tool-invoked" ]; then
  echo 'mount script ran git or artifact-fs before rejecting the remote' >&2
  exit 1
fi
echo 'mount script rejects a remote that starts with -'
