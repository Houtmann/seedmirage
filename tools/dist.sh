#!/bin/sh
# Cross-compile seedmirage for the 5 supported targets into dist/ (archives + SHA256SUMS).
# Usage: tools/dist.sh <version>   (needs go, tar, zip, shasum or sha256sum)
set -eu

VERSION=${1:-dev}
DIST=dist
LDFLAGS="-s -w -X main.version=$VERSION"
TARGETS="darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64"

rm -rf "$DIST"
mkdir -p "$DIST"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

for t in $TARGETS; do
  os=${t%/*}
  arch=${t#*/}
  name="seedmirage_${VERSION}_${os}_${arch}"
  bin=seedmirage
  [ "$os" = windows ] && bin=seedmirage.exe
  mkdir -p "$tmp/$name"
  echo "build $name"
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags "$LDFLAGS" -o "$tmp/$name/$bin" ./cmd/seedmirage
  if [ "$os" = windows ]; then
    (cd "$tmp" && zip -q -r "$OLDPWD/$DIST/$name.zip" "$name")
  else
    tar -C "$tmp" -czf "$DIST/$name.tar.gz" "$name"
  fi
done

cd "$DIST"
if command -v shasum >/dev/null 2>&1; then
  shasum -a 256 -- *.tar.gz *.zip > SHA256SUMS
else
  sha256sum -- *.tar.gz *.zip > SHA256SUMS
fi
