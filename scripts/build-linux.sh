#!/usr/bin/env bash
# Builds the Linux plugin library inside manylinux2014 (glibc 2.17), the same
# baseline CLIProxyAPI uses for its own glibc release binaries, so the plugin
# loads on every host the CLIProxyAPI binary runs on. The build must run on a
# host whose architecture matches GOARCH (no emulation).
#
# Usage: scripts/build-linux.sh <amd64|arm64> <version> <output.so>
set -euo pipefail

goarch=${1:?goarch}
version=${2:?version}
output=${3:?output path}
go_version=${GO_VERSION:-1.26.8}

case "$goarch" in
  amd64) image=quay.io/pypa/manylinux2014_x86_64 ;;
  arm64) image=quay.io/pypa/manylinux2014_aarch64 ;;
  *) echo "unsupported GOARCH $goarch" >&2; exit 1 ;;
esac

repo=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cache="$repo/.cache/linux-$goarch"
mkdir -p "$cache" "$(dirname "$repo/$output")"

docker run --rm \
  --user "$(id -u):$(id -g)" \
  -e HOME=/cache/home \
  -e GOCACHE=/cache/go-build \
  -e GOMODCACHE=/cache/go-mod \
  -e GOTOOLCHAIN=local \
  -e GO_VERSION="$go_version" \
  -e GOARCH="$goarch" \
  -e VERSION="$version" \
  -e OUTPUT="$output" \
  -v "$repo:/src" \
  -v "$cache:/cache" \
  -w /src \
  "$image" \
  bash -euo pipefail -c '
    mkdir -p /cache/home /cache/dl
    archive="go${GO_VERSION}.linux-${GOARCH}.tar.gz"
    if [ ! -x "/cache/go-${GO_VERSION}/bin/go" ]; then
      curl -fsSL "https://go.dev/dl/${archive}" -o "/cache/dl/${archive}"
      rm -rf "/cache/go-${GO_VERSION}" /cache/go-unpack
      mkdir -p /cache/go-unpack
      tar -C /cache/go-unpack -xzf "/cache/dl/${archive}"
      mv /cache/go-unpack/go "/cache/go-${GO_VERSION}"
    fi
    export PATH="/cache/go-${GO_VERSION}/bin:${PATH}"
    go version
    CGO_ENABLED=1 GOOS=linux GOARCH="${GOARCH}" go build -buildvcs=false -trimpath \
      -ldflags "-s -w -X main.pluginVersion=${VERSION}" \
      -buildmode=c-shared -o "${OUTPUT}" ./cmd/cliproxyapi-ollama
    rm -f "${OUTPUT%.so}.h"

    glibc=$(readelf --version-info "${OUTPUT}" | sed -n "s/.*Name: GLIBC_\([0-9.]*\).*/\1/p" | sort -Vu | tail -n 1)
    echo "maximum required GLIBC: ${glibc:-none}"
    if [ -n "${glibc}" ] && [ "$(printf "2.17\n%s\n" "${glibc}" | sort -V | tail -n 1)" != "2.17" ]; then
      echo "plugin requires GLIBC_${glibc}; expected GLIBC_2.17 or older" >&2
      exit 1
    fi
  '
sha256sum "$repo/$output"
