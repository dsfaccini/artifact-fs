#!/usr/bin/env bash
# Installs the slimtree wheel for this machine with uv, as a user does, and
# checks the installed slimtree. It changes nothing outside a temporary
# directory. On Linux with FUSE it mounts a worktree there, and unmounts it.
#
#   smoke.sh <wheel directory> <version> <tag>
set -euo pipefail

dist=$(cd "$1" && pwd)
version=$2
tag=$3
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
export UV_TOOL_DIR="$work/tools" UV_TOOL_BIN_DIR="$work/bin" SLIMTREE_HOME="$work/home"

fail() {
	echo "::error::$*" >&2
	exit 1
}

uv tool install --no-index --find-links "$dist" "slimtree==$version"
slimtree="$UV_TOOL_BIN_DIR/slimtree"
binary=$(readlink -f "$slimtree")
echo "installed $binary"

got=$("$slimtree" --version)
echo "$got"
case $got in
"slimtree $version ($tag, "*) ;;
*) fail "slimtree --version printed '$got', want 'slimtree $version ($tag, ...)'" ;;
esac

repo="$work/src/app"
git init -q -b main "$repo"
echo hello >"$repo/README.md"
git -C "$repo" add README.md
git -C "$repo" -c user.name=smoke -c user.email=smoke@example.invalid commit -qm base
# doctor probes the current directory.
cd "$repo"

# doctor exits 1 when a check fails, as on a runner without FUSE.
status=0
"$slimtree" doctor --json >"$work/doctor.json" || status=$?
cat "$work/doctor.json"
python3 - "$work/doctor.json" "$status" <<'EOF'
import json, sys
report = json.load(open(sys.argv[1]))
assert report["version"] == 1 and report["checks"], report
assert sys.argv[2] in ("0", "1") and report["ok"] == (sys.argv[2] == "0"), f"doctor --json exited {sys.argv[2]}"
EOF

case $(uname -s) in
Darwin)
	if xattr -p com.apple.quarantine "$binary" 2>/dev/null; then
		fail "$binary carries a quarantine attribute"
	fi
	codesign -v "$binary"
	# The wheel's platform tag names the oldest macOS the binary runs on.
	minos=$(vtool -show-build "$binary" | awk '$1 == "minos" { print $2 }')
	wheel=$(basename "$dist"/slimtree-"$version"-py3-none-macosx_*_arm64.whl)
	tagged=$(echo "$wheel" | sed -E 's/.*-macosx_([0-9]+)_([0-9]+)_arm64\.whl$/\1.\2/')
	[ "$minos" = "$tagged" ] || fail "the binary needs macOS $minos, and $wheel claims macOS $tagged"

	"$slimtree" --backend artifactfs doctor --json >"$work/doctor-artifactfs.json" || true
	python3 - "$work/doctor-artifactfs.json" <<'EOF'
import json, sys
checks = {c["id"]: c for c in json.load(open(sys.argv[1]))["checks"]}
assert checks["macos-version"]["status"] == "ok", checks["macos-version"]
print("macos-version:", checks["macos-version"]["detail"])
EOF

	path=$("$slimtree" new feat)
	[ "$(git -C "$path" branch --show-current)" = feat ] || fail "$path is not on the branch feat"
	[ -z "$(git -C "$path" status --porcelain)" ] || fail "git status lists changes in $path"
	cmp "$repo/README.md" "$path/README.md"
	"$slimtree" list
	"$slimtree" list --json | python3 -c 'import json, sys; assert [w["name"] for w in json.load(sys.stdin)["worktrees"]] == ["app-feat"]'
	"$slimtree" rm app-feat
	[ ! -e "$path" ] || fail "slimtree rm left $path"
	"$slimtree" list --json | python3 -c 'import json, sys; assert json.load(sys.stdin)["worktrees"] == []'
	git rev-parse --verify --quiet refs/heads/feat >/dev/null || fail "slimtree rm deleted the branch feat"
	;;
Linux)
	# This release removes no artifactfs worktree, Linux's default backend.
	python3 - "$work/doctor.json" <<'EOF'
import json, sys
report = json.load(open(sys.argv[1]))
backend = report["checks"][0]
assert backend["id"] == "backend" and backend["status"] == "warn", report
assert backend["detail"] == "slimtree rm of artifactfs worktrees is not in this release yet", backend
EOF
	states() {
		"$slimtree" list --json | python3 -c 'import json, sys; print(" ".join(w["name"] + "=" + w["state"] for w in json.load(sys.stdin)["worktrees"]))'
	}
	if python3 -c 'import json, sys; sys.exit(not json.load(open(sys.argv[1]))["ok"])' "$work/doctor.json"; then
		path=$("$slimtree" new feat)
		[ "$(git -C "$path" branch --show-current)" = feat ] || fail "$path is not on the branch feat"
		[ -z "$(git -C "$path" status --porcelain)" ] || fail "git status lists changes in $path"
		cmp "$repo/README.md" "$path/README.md"
		[ "$(states)" = app-feat=mounted ] || fail "slimtree list after new: $(states)"
		"$slimtree" stop app-feat
		[ "$(states)" = app-feat=stopped ] || fail "slimtree list after stop: $(states)"
		status=0
		"$slimtree" rm app-feat 2>"$work/rm.err" || status=$?
		cat "$work/rm.err"
		[ "$status" -eq 1 ] || fail "slimtree rm of an artifactfs worktree exited $status, want 1"
		grep -q 'removing artifactfs worktrees is not in this release yet' "$work/rm.err" || fail "slimtree rm did not say removing artifactfs worktrees is not in this release"
	else
		# Without FUSE, new refuses before it changes anything.
		status=0
		"$slimtree" new feat 2>"$work/new.err" || status=$?
		cat "$work/new.err"
		[ "$status" -eq 1 ] || fail "slimtree new without FUSE exited $status, want 1"
		grep -q 'slimtree new --backend artifactfs needs:' "$work/new.err" || fail "slimtree new did not name the failed checks"
		[ ! -e "$work/src/app-feat" ] || fail "slimtree new made $work/src/app-feat"
		if git rev-parse --verify --quiet refs/heads/feat; then
			fail "slimtree new made the branch feat"
		fi
		[ "$(git worktree list | wc -l)" -eq 1 ] || fail "git lists a new worktree"
		[ -z "$(ls -A "$SLIMTREE_HOME/w" 2>/dev/null)" ] || fail "slimtree new wrote a record"
	fi
	;;
*) fail "no smoke test for $(uname -s)" ;;
esac
echo "smoke test passed: slimtree $version on $(uname -sm)"
