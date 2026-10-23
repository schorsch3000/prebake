#!/usr/bin/env bash
# Build the prebake CLI for all release platforms into <outdir> (default dist/):
# one archive per platform plus checksums.txt. Usage: scripts/build-release.sh <version> [outdir]
# The archives are reproducible for a given commit (fixed mtimes, sorted, no owner info).
set -euo pipefail

version=${1:?usage: build-release.sh <version> [outdir]}
version=${version#v}
out=${2:-dist}
revision=$(git rev-parse HEAD 2>/dev/null || echo unknown)
epoch=$(git log -1 --format=%ct 2>/dev/null || date +%s)
targets=(linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64)

rm -rf "$out"
mkdir -p "$out"
out=$(cd "$out" && pwd)   # absolute: the zip step runs in another directory
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT

for t in "${targets[@]}"; do
  os=${t%/*}
  arch=${t#*/}
  name=prebake_${version}_${os}_${arch}
  bin=prebake
  [ "$os" = windows ] && bin=prebake.exe
  mkdir -p "$stage/$name"
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath \
    -ldflags "-s -w -X main.version=$version -X main.revision=$revision" \
    -o "$stage/$name/$bin" ./cmd/prebake
  cp README.md "$stage/$name/README.md"
  touch -d "@$epoch" "$stage/$name/$bin" "$stage/$name/README.md" "$stage/$name"
  if [ "$os" = windows ]; then
    (cd "$stage" && zip -q -X -r "$out/$name.zip" "$name")
  else
    tar -C "$stage" --sort=name --owner=0 --group=0 --numeric-owner --mtime="@$epoch" \
      -czf "$out/$name.tar.gz" "$name"
  fi
  echo "built $name"
done

(cd "$out" && sha256sum -- * > checksums.txt)
cat "$out/checksums.txt"
