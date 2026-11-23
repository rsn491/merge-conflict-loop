#!/bin/sh
# Installer for merge-conflict-loop.
#
#   curl -fsSL https://raw.githubusercontent.com/rsn491/merge-conflict-loop/main/install.sh | sh
#   curl -fsSL https://raw.githubusercontent.com/rsn491/merge-conflict-loop/main/install.sh | VERSION=v0.0.1 sh
#
# Environment:
#   VERSION      release tag to install (default: latest)
#   INSTALL_DIR  where to put the binary (default: /usr/local/bin if writable, else ~/.local/bin)
#   BASE_URL     override the download location (for testing / mirrors)
set -eu

REPO=rsn491/merge-conflict-loop
NAME=merge-conflict-loop
VERSION=${VERSION:-latest}

say() { printf '%s\n' "$*" >&2; }
die() { say "error: $*"; exit 1; }

case "$(uname -s)" in
Linux) os=linux ;;
Darwin) os=darwin ;;
*) die "unsupported OS: $(uname -s) (supported: Linux, macOS)" ;;
esac
case "$(uname -m)" in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) die "unsupported architecture: $(uname -m) (supported: amd64, arm64)" ;;
esac

if [ -z "${BASE_URL:-}" ]; then
	if [ "$VERSION" = latest ]; then
		BASE_URL="https://github.com/$REPO/releases/latest/download"
	else
		BASE_URL="https://github.com/$REPO/releases/download/$VERSION"
	fi
fi

if command -v curl >/dev/null 2>&1; then
	fetch() { curl -fsSL "$1" -o "$2"; }
elif command -v wget >/dev/null 2>&1; then
	fetch() { wget -qO "$2" "$1"; }
else
	die "curl or wget is required"
fi
if command -v sha256sum >/dev/null 2>&1; then
	sha256() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
	sha256() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
	die "sha256sum or shasum is required"
fi

if [ -z "${INSTALL_DIR:-}" ]; then
	if [ -d /usr/local/bin ] && [ -w /usr/local/bin ]; then
		INSTALL_DIR=/usr/local/bin
	else
		INSTALL_DIR="$HOME/.local/bin"
	fi
fi

asset="${NAME}_${os}_${arch}.tar.gz"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

say "downloading $NAME $VERSION ($os/$arch)"
fetch "$BASE_URL/$asset" "$tmp/$asset" || die "download failed: $BASE_URL/$asset"
fetch "$BASE_URL/checksums.txt" "$tmp/checksums.txt" || die "download failed: $BASE_URL/checksums.txt"

want=$(awk -v f="$asset" '$2 == f || $2 == "*" f { print $1 }' "$tmp/checksums.txt")
[ -n "$want" ] || die "no checksum for $asset in checksums.txt"
got=$(sha256 "$tmp/$asset")
[ "$want" = "$got" ] || die "checksum mismatch for $asset (expected $want, got $got)"

tar -xzf "$tmp/$asset" -C "$tmp" "./$NAME" 2>/dev/null || tar -xzf "$tmp/$asset" -C "$tmp" "$NAME"
mkdir -p "$INSTALL_DIR"
install -m 0755 "$tmp/$NAME" "$INSTALL_DIR/$NAME" 2>/dev/null || {
	cp "$tmp/$NAME" "$INSTALL_DIR/$NAME" && chmod 0755 "$INSTALL_DIR/$NAME"
}

say "installed $("$INSTALL_DIR/$NAME" --version) to $INSTALL_DIR/$NAME"
case ":$PATH:" in
*":$INSTALL_DIR:"*) ;;
*) say "note: $INSTALL_DIR is not on your PATH; add it with: export PATH=\"$INSTALL_DIR:\$PATH\"" ;;
esac
say ""
say "next steps (runtime dependencies):"
say "  npm install -g --ignore-scripts @earendil-works/pi-coding-agent   # the agent (Node 22+)"
say "  curl -fsSL https://ollama.com/install.sh | sh && ollama serve      # local open-weight LLM"
say "  GH_TOKEN=\$(gh auth token) $NAME --dry-run   # run from a clone of your repo"
