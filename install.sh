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

mkdir -p "$bin_dir"

# Build in the destination directory so the final rename is atomic and a
# failed build cannot replace a working installation.
tmp_binary=$(mktemp "$bin_dir/.bedrock-proxy.XXXXXX")
cleanup() {
	rm -f -- "$tmp_binary"
}
trap cleanup EXIT HUP INT TERM

printf '%s\n' "Building bedrock-proxy for $(go env GOOS)/$(go env GOARCH)..."
if ! (cd "$script_dir" && go build -o "$tmp_binary" ./cmd/bedrock-proxy); then
	printf '%s\n' "install.sh: build failed; existing installation was left unchanged" >&2
	exit 1
fi

if ! version_output=$("$tmp_binary" --version 2>&1); then
	printf '%s\n' "install.sh: built binary failed --version; existing installation was left unchanged" >&2
	printf '%s\n' "$version_output" >&2
	exit 1
fi

chmod 755 "$tmp_binary"
mv -f -- "$tmp_binary" "$install_path"
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
	printf '%s\n' "  1. Edit $config_path and replace YOUR_AWS_PROFILE and REPLACE_WITH_YOUR_BEDROCK_MODEL_ID."
	printf '%s\n' "  2. Sign in with: aws sso login --profile YOUR_AWS_PROFILE"
else
	printf '%s\n' "  1. Check $config_path for its configured model and AWS profile."
	printf '%s\n' "  2. Sign in with: aws sso login --profile YOUR_AWS_PROFILE"
fi
printf '%s\n' "  3. Run: bedrock-proxy"
