#!/usr/bin/env bash
# Prints the license files of Go and of every module that slimtree links for
# each target, such as linux/amd64.
#
#   third_party_licenses.sh <goos/goarch>...
set -euo pipefail

goroot=$(go env GOROOT)
section() {
	printf '\n================================================================\n%s\n================================================================\n\n' "$1"
}
section "Go $(go env GOVERSION): standard library and runtime"
cat "$goroot/LICENSE" "$goroot/PATENTS"

for target in "$@"; do
	GOOS=${target%/*} GOARCH=${target#*/} CGO_ENABLED=0 go list -deps \
		-f '{{with .Module}}{{if not .Main}}{{.Path}} {{.Version}} {{.Dir}}{{end}}{{end}}' ./cmd/slimtree
done | sort -u | while read -r path version dir; do
	found=
	while IFS= read -r file; do
		section "$path $version: $(basename "$file")"
		cat "$file"
		found=1
	done < <(find "$dir" -maxdepth 1 -type f \( -iname 'licen[cs]e*' -o -iname 'copying*' -o -iname 'notice*' -o -iname 'patents*' \) | sort)
	if [ -z "$found" ]; then
		echo "$path $version has no license file in $dir" >&2
		exit 1
	fi
done
