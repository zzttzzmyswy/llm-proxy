#!/bin/sh
# Build the two release binaries and their checksums.
#
# CGO_ENABLED=0 is what makes them statically linked. Without it the amd64 build
# links against the host's glibc and fails to start on a machine with a different
# one (a NAS, for instance), which is not obvious from a successful build.
# -buildvcs=false keeps the VCS state out of the artifact, so rebuilding the same
# commit twice produces the same metadata instead of picking up "+dirty" from an
# unrelated working-tree change.
#
# Run this from a clean tree, after the version bump has been committed.
set -eu
cd "$(dirname "$0")"

for arch in amd64 arm64; do
	CGO_ENABLED=0 GOOS=linux GOARCH="$arch" \
		go build -trimpath -buildvcs=false -ldflags="-s -w" \
		-o "llm-proxy-linux-$arch" .
done

sha256sum llm-proxy-linux-amd64 llm-proxy-linux-arm64 > SHA256SUMS
sha256sum -c SHA256SUMS
