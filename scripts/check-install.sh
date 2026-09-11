#!/bin/sh

set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_dir=$(CDPATH= cd -- "$script_dir/.." && pwd)
tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/bedrock-proxy-install.XXXXXX")
cleanup() {
	chmod -R u+w "$tmp_dir" 2>/dev/null || true
	rm -rf -- "$tmp_dir"
}
trap cleanup EXIT HUP INT TERM

fake_bin=$tmp_dir/bin
fake_home=$tmp_dir/home
blocked_cache=$tmp_dir/blocked-cache
test_tmp=$tmp_dir/tmp
go_log=$tmp_dir/go-environment
mkdir -p "$fake_bin" "$fake_home" "$blocked_cache" "$test_tmp"
chmod 500 "$blocked_cache"

cat >"$fake_bin/go" <<'EOF'
#!/bin/sh
set -eu

if [ "$1" = "env" ]; then
	case "$2" in
		GOOS) printf '%s\n' darwin ;;
		GOARCH) printf '%s\n' arm64 ;;
		*) exit 2 ;;
	esac
	exit 0
fi

if [ "$1" != "build" ]; then
	exit 2
fi

: "${GOCACHE:?installer must set GOCACHE}"
: "${GOMODCACHE:?installer must set GOMODCACHE}"
mkdir -p "$GOCACHE" "$GOMODCACHE"
probe=$GOCACHE/.write-test
: >"$probe"
rm -f -- "$probe"
probe=$GOMODCACHE/.write-test
: >"$probe"
rm -f -- "$probe"
mkdir -p "$GOMODCACHE/example.invalid/module@v1.0.0"
: >"$GOMODCACHE/example.invalid/module@v1.0.0/go.mod"
chmod 444 "$GOMODCACHE/example.invalid/module@v1.0.0/go.mod"
chmod 555 "$GOMODCACHE/example.invalid/module@v1.0.0"
printf '%s\n%s\n' "$GOCACHE" "$GOMODCACHE" >"${INSTALL_TEST_GO_LOG:?}"

output=
shift
while [ "$#" -gt 0 ]; do
	if [ "$1" = "-o" ]; then
		shift
		output=$1
	fi
	shift
done
: "${output:?missing build output}"
cat >"$output" <<'EOF_BINARY'
#!/bin/sh
if [ "${1-}" = "--version" ]; then
	printf '%s\n' 'bedrock-proxy test'
	exit 0
fi
exit 2
EOF_BINARY
chmod 755 "$output"
EOF
chmod 755 "$fake_bin/go"

PATH="$fake_bin:$PATH" \
	HOME="$fake_home" \
	TMPDIR="$test_tmp" \
	XDG_CACHE_HOME="$blocked_cache" \
	GOCACHE="$blocked_cache/default-build" \
	GOMODCACHE="$blocked_cache/default-mod" \
	INSTALL_TEST_GO_LOG="$go_log" \
	"$repo_dir/install.sh" >/dev/null

test -x "$fake_home/.local/bin/bedrock-proxy"
test -f "$fake_home/.config/bedrock-proxy/config.yaml"
test -s "$go_log"
if grep -F "$blocked_cache" "$go_log" >/dev/null; then
	printf '%s\n' "installer reused an inaccessible Go cache" >&2
	exit 1
fi

printf '%s\n' "Installer permission fallback passed."
