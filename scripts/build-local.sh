#!/bin/bash
#
# This script helps to build artifacts locally when using go.mod replace
# statements to modules on local disk. It uses local go to build locally before
# then using Docker to create the images using the locally built go binaries.
# Not useful for CI or real builds, only helpful when using go.mod replace
# statements. Another approach could be to vendor the replaced modules or copy
# them to a docker volume for use by the regular docker build.

set -euo pipefail
set -x

cd "$(dirname "$0")/.."

XPKG_REF="${1:-}"

if [[ -z "$XPKG_REF" ]]; then
    echo "Usage: $0 <package-ref>" >&2
    echo "Example: $0 xpkg.upbound.io/org/repo:tag" >&2
    exit 1
fi

GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o function-amd64 .
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o function-arm64 .

docker build -f Dockerfile.local --platform=linux/amd64 --tag runtime-amd64 .
docker build -f Dockerfile.local --platform=linux/arm64 --tag runtime-arm64 .

crossplane xpkg build \
    --package-root=package \
    --embed-runtime-image=runtime-amd64 \
    --package-file=function-amd64.xpkg

crossplane xpkg build \
    --package-root=package \
    --embed-runtime-image=runtime-arm64 \
    --package-file=function-arm64.xpkg

crossplane xpkg push \
   --package-files=function-amd64.xpkg,function-arm64.xpkg \
   "${XPKG_REF}"

rm -f function-amd64
rm -f function-amd64.xpkg
rm -f function-arm64
rm -f function-arm64.xpkg