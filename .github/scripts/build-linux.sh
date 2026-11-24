#!/usr/bin/env bash
set -euo pipefail

: "${TARGET_GOARCH:?TARGET_GOARCH is required}"

export GOROOT=/go-toolchain
export PATH="${GOROOT}/bin:${PATH}"
export GOPATH=/tmp/go
export GOMODCACHE=/go/pkg/mod
export GOCACHE=/tmp/go-cache
export GOTOOLCHAIN=local
export GOPROXY=off
export CGO_ENABLED=1
export GOOS=linux
export GOARCH="${TARGET_GOARCH}"

for enable_script in /opt/rh/devtoolset-*/enable; do
  if [[ -f "${enable_script}" ]]; then
    # shellcheck disable=SC1090
    source "${enable_script}"
  fi
done

go version
go env GOOS GOARCH

mkdir -p dist
go build -trimpath -buildmode=c-shared -ldflags "-s -w" -o dist/opencode-go-omni.so .
rm -f dist/opencode-go-omni.h
