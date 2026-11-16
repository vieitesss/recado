#!/bin/sh
# Install recado: curl -fsSL https://recado.vieitesss.eu/install | sh
#
# RECADO_VERSION       release tag to install (default: the latest release)
# RECADO_INSTALL_DIR   where the binary goes (default: ~/.local/bin)
# RECADO_DOWNLOAD_URL  directory holding the release assets (default: GitHub)
#
# Everything runs from main, called on the last line, so a truncated download
# of this script does nothing.

set -eu

repo="https://github.com/vieitesss/recado"

fail() {
	echo "recado install: $*" >&2
	exit 1
}

platform() {
	case "$(uname -s)" in
	Darwin) os=darwin ;;
	Linux) os=linux ;;
	*) fail "unsupported OS $(uname -s): recado runs on macOS and Linux (Windows via WSL)" ;;
	esac
	case "$(uname -m)" in
	x86_64 | amd64) arch=amd64 ;;
	arm64 | aarch64) arch=arm64 ;;
	*) fail "unsupported architecture $(uname -m): recado ships amd64 and arm64" ;;
	esac
	echo "${os}_${arch}"
}

download() { # URL FILE
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL "$1" -o "$2"
	elif command -v wget >/dev/null 2>&1; then
		wget -q "$1" -O "$2"
	else
		fail "need curl or wget"
	fi
}

sha256() { # FILE
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | cut -d' ' -f1
	else
		fail "need sha256sum or shasum to verify the download"
	fi
}

main() {
	if [ -n "${RECADO_DOWNLOAD_URL:-}" ]; then
		base="$RECADO_DOWNLOAD_URL"
	elif [ -n "${RECADO_VERSION:-}" ]; then
		base="$repo/releases/download/$RECADO_VERSION"
	else
		base="$repo/releases/latest/download"
	fi
	dir="${RECADO_INSTALL_DIR:-$HOME/.local/bin}"
	archive="recado_$(platform).tar.gz"

	tmp="$(mktemp -d)"
	trap 'rm -rf "$tmp"' EXIT

	download "$base/$archive" "$tmp/$archive" || fail "download failed: $base/$archive"
	download "$base/checksums.txt" "$tmp/checksums.txt" || fail "download failed: $base/checksums.txt"

	want="$(awk -v f="$archive" '$2 == f { print $1 }' "$tmp/checksums.txt")"
	[ -n "$want" ] || fail "$archive is not listed in checksums.txt"
	[ "$(sha256 "$tmp/$archive")" = "$want" ] || fail "checksum mismatch for $archive"

	tar -xzf "$tmp/$archive" -C "$tmp" recado
	mkdir -p "$dir"
	# Move a copy into place: writing over a running binary fails on Linux and
	# can get the process killed on macOS.
	cp "$tmp/recado" "$dir/.recado.new"
	chmod 755 "$dir/.recado.new"
	mv -f "$dir/.recado.new" "$dir/recado"

	echo "installed $("$dir/recado" --version) to $dir/recado"
	case ":$PATH:" in
	*":$dir:"*) ;;
	*) echo "note: $dir is not on PATH; add it to your shell profile" ;;
	esac
	# recado update sets RECADO_UPDATING and runs setup itself.
	[ -n "${RECADO_UPDATING:-}" ] || echo "next: run 'recado skill setup' to install the skill and harness shims"
}

main "$@"
