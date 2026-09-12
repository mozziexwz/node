#!/usr/bin/env bash
set -Eeuo pipefail
version=${1:?Usage: package-release.sh vX.Y.Z}
[[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || { printf 'Expected a stable semantic version.\n' >&2; exit 1; }
root=$(git rev-parse --show-toplevel)
cd "$root"
out="$root/.runtime/release"
mkdir -p "$out"
# Only reviewed, committed sources enter the bundle; no local .env, data or tools.
git archive --format=tar HEAD | gzip -n > "$out/msboost-deploy-${version}.tar.gz"
cp deploy/install-agent.sh "$out/install-agent.sh"
for arch in amd64 arm64; do
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -ldflags="-s -w" -o "$out/msboost-agent-linux-${arch}" ./cmd/agent
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -ldflags="-s -w" -o "$out/msboost-restore-linux-${arch}" ./cmd/restore
done
(
  cd "$out"
  sha256sum "msboost-deploy-${version}.tar.gz" install-agent.sh msboost-agent-linux-amd64 msboost-agent-linux-arm64 msboost-restore-linux-amd64 msboost-restore-linux-arm64 msboost-image-linux-amd64.tar.gz msboost-image-linux-arm64.tar.gz > SHA256SUMS
)
