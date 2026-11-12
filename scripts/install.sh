#!/bin/sh
# Install the latest AgentBox release. Usage:
#   curl -fsSL https://raw.githubusercontent.com/jeeqe/agentbox/main/scripts/install.sh | sh
set -eu

repo="${AGENTBOX_REPO:-jeeqe/agentbox}"
binary="agentbox"

download() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL "$1"
	elif command -v wget >/dev/null 2>&1; then
		wget -qO- "$1"
	else
		echo "agentbox installer requires curl or wget" >&2
		exit 1
	fi
}

case "$(uname -s)" in
	Linux) release_os="Linux" ;;
	Darwin) release_os="Darwin" ;;
	*)
		echo "unsupported operating system: $(uname -s)" >&2
		exit 1
		;;
esac

case "$(uname -m)" in
	x86_64|amd64) release_arch="amd64" ;;
	aarch64|arm64) release_arch="arm64" ;;
	*)
		echo "unsupported architecture: $(uname -m)" >&2
		exit 1
		;;
esac

tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT HUP INT TERM

release_json="$(download "https://api.github.com/repos/$repo/releases/latest")"
tag="$(printf '%s\n' "$release_json" | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)"
archive="$(printf '%s\n' "$release_json" | sed -n 's/.*"name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | grep -E "^${binary}_.+_${release_os}_${release_arch}\\.tar\\.gz$" | head -n 1 || true)"

if [ -z "$tag" ] || [ -z "$archive" ]; then
	echo "could not find a $release_os/$release_arch AgentBox release for $repo" >&2
	exit 1
fi

base_url="https://github.com/$repo/releases/download/$tag"
archive_path="$tmpdir/$archive"
checksums_path="$tmpdir/${binary}_checksums.txt"
download "$base_url/$archive" >"$archive_path"
download "$base_url/${binary}_checksums.txt" >"$checksums_path"

expected="$(awk -v file="$archive" '$2 == file { print $1; exit }' "$checksums_path")"
if command -v sha256sum >/dev/null 2>&1; then
	actual="$(sha256sum "$archive_path" | awk '{print $1}')"
elif command -v shasum >/dev/null 2>&1; then
	actual="$(shasum -a 256 "$archive_path" | awk '{print $1}')"
else
	echo "agentbox installer requires sha256sum or shasum" >&2
	exit 1
fi

if [ -z "$expected" ] || [ "$actual" != "$expected" ]; then
	echo "checksum verification failed for $archive" >&2
	exit 1
fi

tar -xzf "$archive_path" -C "$tmpdir"
if [ ! -f "$tmpdir/$binary" ]; then
	echo "release archive did not contain $binary" >&2
	exit 1
fi

if [ -n "${PREFIX:-}" ]; then
	destination="$PREFIX/bin"
elif [ -w /usr/local/bin ]; then
	destination="/usr/local/bin"
else
	destination="${HOME}/.local/bin"
fi

mkdir -p "$destination"
install -m 0755 "$tmpdir/$binary" "$destination/$binary"
echo "Installed $binary to $destination/$binary"

case ":${PATH}:" in
	*":${destination}:"*) ;;
	*) echo "Add $destination to your PATH to run $binary from any shell." ;;
esac
