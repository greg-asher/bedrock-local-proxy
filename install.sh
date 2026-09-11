#!/bin/sh

set -eu

usage() {
	cat <<'EOF'
Usage: ./install.sh

Build and install bedrock-proxy for the current host, then create the starter
configuration when it does not already exist.
EOF
}

if [ "$#" -gt 0 ]; then
	if [ "$#" -eq 1 ] && [ "$1" = "--help" ]; then
		usage
		exit 0
	fi
	printf '%s\n' "install.sh: no arguments are supported (use --help for usage)" >&2
	exit 2
fi

if ! command -v go >/dev/null 2>&1; then
	printf '%s\n' "install.sh: Go is required; install Go and run ./install.sh again" >&2
	exit 1
fi

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
bin_dir=${HOME:?HOME must be set}/.local/bin
config_dir=${HOME:?HOME must be set}/.config/bedrock-proxy
config_path=$config_dir/config.yaml
install_path=$bin_dir/bedrock-proxy
case "${XDG_CACHE_HOME-}" in
	/*) installer_cache_root=$XDG_CACHE_HOME/bedrock-proxy/go ;;
	*) installer_cache_root=$HOME/.cache/bedrock-proxy/go ;;
esac
case "${XDG_STATE_HOME-}" in
	/*) report_dir=$XDG_STATE_HOME/bedrock-proxy/sessions ;;
	*) report_dir=$HOME/.local/state/bedrock-proxy/sessions ;;
esac

tmp_binary=
temporary_cache_root=
remove_temporary_cache() {
	if [ -z "$temporary_cache_root" ]; then
		return
	fi
	# Go deliberately marks downloaded module directories read-only. Restore
	# owner write permission before removing an installer-owned temporary cache.
	chmod -R u+w "$temporary_cache_root" 2>/dev/null || true
	rm -rf -- "$temporary_cache_root" 2>/dev/null || true
}
cleanup() {
	if [ -n "$tmp_binary" ]; then
		rm -f -- "$tmp_binary"
	fi
	remove_temporary_cache
}
trap cleanup EXIT HUP INT TERM

prepare_go_cache() {
	cache_root=$1
	if ! mkdir -p "$cache_root/build" "$cache_root/mod" 2>/dev/null; then
		return 1
	fi
	for cache_dir in "$cache_root/build" "$cache_root/mod"; do
		cache_probe=$cache_dir/.bedrock-proxy-write-test.$$
		if ! (umask 077 && : >"$cache_probe") 2>/dev/null; then
			rm -f -- "$cache_probe" 2>/dev/null || true
			return 1
		fi
		rm -f -- "$cache_probe"
	done
}

if ! prepare_go_cache "$installer_cache_root"; then
	temporary_cache_root=$(mktemp -d "${TMPDIR:-/tmp}/bedrock-proxy-go-cache.XXXXXX")
	installer_cache_root=$temporary_cache_root
	if ! prepare_go_cache "$installer_cache_root"; then
		printf '%s\n' "install.sh: cannot create a writable Go build cache" >&2
		exit 1
	fi
	printf '%s\n' "Using a temporary Go cache because the per-user cache is not writable."
fi
go_build_cache=$installer_cache_root/build
go_module_cache=$installer_cache_root/mod

mkdir -p "$bin_dir"

# Build in the destination directory so the final rename is atomic and a
# failed build cannot replace a working installation.
tmp_binary=$(mktemp "$bin_dir/.bedrock-proxy.XXXXXX")

printf '%s\n' "Building bedrock-proxy for $(GOCACHE="$go_build_cache" GOMODCACHE="$go_module_cache" go env GOOS)/$(GOCACHE="$go_build_cache" GOMODCACHE="$go_module_cache" go env GOARCH)..."
if ! (cd "$script_dir" && GOCACHE="$go_build_cache" GOMODCACHE="$go_module_cache" go build -o "$tmp_binary" ./cmd/bedrock-proxy); then
	printf '%s\n' "install.sh: build failed; existing installation was left unchanged" >&2
	exit 1
fi

if [ -n "$temporary_cache_root" ]; then
	remove_temporary_cache
	temporary_cache_root=
fi

if ! version_output=$("$tmp_binary" --version 2>&1); then
	printf '%s\n' "install.sh: built binary failed --version; existing installation was left unchanged" >&2
	printf '%s\n' "$version_output" >&2
	exit 1
fi

chmod 755 "$tmp_binary"
mv -f -- "$tmp_binary" "$install_path"
tmp_binary=
trap - EXIT HUP INT TERM

# Run the installed path as the final installation check.
if ! installed_version=$("$install_path" --version 2>&1); then
	printf '%s\n' "install.sh: installed binary failed --version" >&2
	printf '%s\n' "$installed_version" >&2
	exit 1
fi

config_created=0
if [ ! -e "$config_path" ] && [ ! -L "$config_path" ]; then
	mkdir -p "$config_dir"
	umask 077
	cp "$script_dir/config.example.yaml" "$config_path"
	config_created=1
fi

printf '%s\n' "Installed: $install_path"
printf '%s\n' "Version:   $installed_version"
printf '%s\n' "Config:    $config_path"
printf '%s\n' "Reports:   $report_dir"
printf '%s\n' "AWS connectivity: not checked"

case ":${PATH-}:" in
	*":$bin_dir:"*)
		;;
	*)
		printf '%s\n' "Add the installed command to PATH for future shells:"
		printf '  export PATH="%s:$PATH"\n' "$bin_dir"
		;;
esac

printf '%s\n' "Next steps:"
if [ "$config_created" -eq 1 ]; then
	printf '%s\n' "  1. Edit $config_path and set the AWS profile, target ID, and documented capabilities."
	printf '%s\n' "  2. For Codex, run: bedrock-proxy configure codex"
else
	printf '%s\n' "  1. Check $config_path for its configured model, capabilities, and AWS profile."
	printf '%s\n' "  2. For Codex, regenerate metadata: bedrock-proxy configure codex"
fi
	printf '%s\n' "  3. For Claude Code 2.1.242+, run: bedrock-proxy configure claude"
	printf '%s\n' "  4. Check locally: bedrock-proxy doctor --client codex"
	printf '%s\n' "  5. Check Claude locally: bedrock-proxy doctor --client claude"
	printf '%s\n' "  6. Sign in with: aws sso login --profile YOUR_AWS_PROFILE"
	printf '%s\n' "  7. Run: bedrock-proxy"
