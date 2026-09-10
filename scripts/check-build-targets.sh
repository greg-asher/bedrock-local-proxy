#!/bin/sh

set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_dir=$(CDPATH= cd -- "$script_dir/.." && pwd)
tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/bedrock-proxy-targets.XXXXXX")
cleanup() {
	rm -rf -- "$tmp_dir"
}
trap cleanup EXIT HUP INT TERM

targets="darwin/arm64 darwin/amd64 linux/amd64 linux/arm64"
for target in $targets; do
	goos=${target%/*}
	goarch=${target#*/}
	out_dir=$tmp_dir/$goos-$goarch
	mkdir -p "$out_dir"
	printf '%s\n' "Checking $target (compile only; runtime check is unavailable on this host)..."
	(
		cd "$repo_dir"
		GOOS=$goos GOARCH=$goarch go build -o "$out_dir/bedrock-proxy" ./cmd/bedrock-proxy
	)
done
printf '%s\n' "All supported targets compiled successfully."
