#!/bin/sh
# Cross-compile release archives and checksums.
# Usage: scripts/build-release.sh <version> [outdir]
set -eu

VERSION=${1:?usage: build-release.sh <version> [outdir]}
OUT=${2:-dist}
NAME=merge-conflict-loop
TARGETS="linux/amd64 linux/arm64 darwin/amd64 darwin/arm64"

ROOT=$(cd "$(dirname "$0")/.." && pwd)
mkdir -p "$OUT"
OUT=$(cd "$OUT" && pwd)
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

for t in $TARGETS; do
	os=${t%/*}
	arch=${t#*/}
	dir="$TMP/${NAME}_${os}_${arch}"
	mkdir -p "$dir"
	echo "building $os/$arch"
	(cd "$ROOT" && CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath \
		-ldflags "-s -w -X main.version=$VERSION" -o "$dir/$NAME" ./cmd/$NAME)
	cp "$ROOT/README.md" "$ROOT/LICENSE" "$dir/"
	# Asset names carry no version so releases/latest/download/<asset> works.
	tar -C "$dir" -czf "$OUT/${NAME}_${os}_${arch}.tar.gz" .
done

cd "$OUT"
if command -v sha256sum >/dev/null 2>&1; then
	sha256sum ${NAME}_*.tar.gz >checksums.txt
else
	shasum -a 256 ${NAME}_*.tar.gz >checksums.txt
fi
echo "wrote $(ls "$OUT" | wc -l | tr -d ' ') files to $OUT"
