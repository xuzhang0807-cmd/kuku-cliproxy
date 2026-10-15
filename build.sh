#!/usr/bin/env bash
# Build the kuku plugin for one target platform.
#
# The host derives a plugin id from the file name, so the deployed file must be
# named kuku.so (the plugin registers the provider id "kuku" itself).
#
# Native CGO cross-compilation does not work on macOS, so run this inside the
# golang image for anything other than the host platform:
#   docker run --rm --platform linux/amd64 -v "$PWD":/src -w /src \
#     golang:1.26-bookworm bash -c 'export PATH=$PATH:/usr/local/go/bin && ./build.sh'
set -euo pipefail

GOOS_EFF="${GOOS:-$(go env GOOS)}"
GOARCH_EFF="${GOARCH:-$(go env GOARCH)}"
export GOOS="$GOOS_EFF" GOARCH="$GOARCH_EFF"
export CGO_ENABLED=1

case "${GOOS_EFF}-${GOARCH_EFF}" in
  linux-amd64) CC_DEFAULT="gcc" ;;
  linux-arm64) CC_DEFAULT="aarch64-linux-gnu-gcc" ;;
  linux-386)   CC_DEFAULT="i686-linux-gnu-gcc" ;;
  linux-arm)   CC_DEFAULT="arm-linux-gnueabihf-gcc" ;;
  *)           CC_DEFAULT="$(go env CC)" ;;
esac
export CC="${CC:-$CC_DEFAULT}"

if ! command -v "$CC" >/dev/null 2>&1; then
  echo "error: cross compiler '$CC' not found." >&2
  echo "       Debian/Ubuntu: sudo apt-get install -y gcc-aarch64-linux-gnu" >&2
  exit 1
fi

SUFFIX="${GOOS_EFF}-${GOARCH_EFF}"
OUTDIR="${OUTDIR:-dist}"
mkdir -p "$OUTDIR"

go build -buildmode=c-shared -o "$OUTDIR/kuku-$SUFFIX.so" .
echo "built $OUTDIR/kuku-$SUFFIX.so (provider=kuku)"
