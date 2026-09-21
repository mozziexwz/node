#!/usr/bin/env bash
# Read-only and source-safe candidate/release version consistency check.
# Usage: bash deploy/version_test.sh [EXPECTED_TAG]
# CI must pass its actual tag when validating a release; omission checks the
# repository's current install default without claiming that tag is published.
version_check_line() {
  local expected=$1 file=$2 count
  count=$(grep -Fxc -- "$expected" "$file") || return 1
  [[ $count == 1 ]]
}
version_check_tree() {
  local repo=$1 tag=$2 value url base
  local -a admin_urls=()
  [[ $tag =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$ ]] || return 1
  base=${tag#v}
  version_check_line "readonly INITIAL_VERSION=$tag" "$repo/install.sh" || return 1
  version_check_line "version=$tag" "$repo/agent.sh" || return 1
  grep -Fq -- "[--version $tag]" "$repo/install.sh" || return 1
  grep -Fq -- "[--version $tag]" "$repo/agent.sh" || return 1
  version_check_line "VERSION=$tag" "$repo/deploy/manage.sh" || return 1
  version_check_line "MSBOOST_VERSION=$tag" "$repo/.env.example" || return 1
  version_check_line "MSBOOST_IMAGE=ghcr.io/mozziexwz/node:$tag" "$repo/.env.example" || return 1
  version_check_line '    image: ${MSBOOST_IMAGE:-ghcr.io/mozziexwz/node:'"$tag"'}' "$repo/deploy/compose.yml" || return 1
  version_check_line '        VERSION: ${MSBOOST_VERSION:-'"$tag"'}' "$repo/deploy/compose.build.yml" || return 1
  version_check_line "ARG VERSION=$base-dev" "$repo/Dockerfile" || return 1
  version_check_line "var version = \"$base-dev\"" "$repo/cmd/server/main.go" || return 1
  value=$(sed -n 's/^[[:space:]]*"version": "\([^"]*\)",$/\1/p' "$repo/apps/web/package.json") || return 1
  [[ $value == "$base" ]] || return 1
  # Only the top-level package and packages[""] precede the first dependencies
  # object. Dependency package versions are deliberately not release defaults.
  value=$(awk '/"dependencies":/ {exit} /"version":/ {gsub(/[",]/, ""); print $2}' "$repo/apps/web/package-lock.json") || return 1
  [[ $value == "$base"$'\n'"$base" ]] || return 1
  value=$(grep -Eo 'https://raw\.githubusercontent\.com/mozziexwz/node/[^ /`]+/agent\.sh' "$repo/apps/web/src/admin.tsx") || return 1
  mapfile -t admin_urls <<< "$value"
  # Bind both the visible installation command and the clipboard command.
  [[ ${#admin_urls[@]} == 2 ]] || return 1
  for url in "${admin_urls[@]}"; do
    [[ $url == "https://raw.githubusercontent.com/mozziexwz/node/$tag/agent.sh" ]] || return 1
  done
}
version_test_main() {
  [[ $# -le 1 ]] || return 2
  local repo tag
  repo=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P) || return 1
  tag=${1:-}
  if [[ -z $tag ]]; then
    tag=$(sed -n 's/^readonly INITIAL_VERSION=\(v[^[:space:]]*\)$/\1/p' "$repo/install.sh") || return 1
  fi
  if ! version_check_tree "$repo" "$tag"; then
    printf '%s\n' 'FAIL: candidate/tag version differs across install, Agent, Compose, env, server, web package or admin clipboard defaults' >&2
    return 1
  fi
  printf 'PASS: %s defaults and both admin Agent commands agree (publication not asserted)\n' "$tag"
}
if [[ ${BASH_SOURCE[0]} == "$0" ]]; then
  set -Eeuo pipefail
  version_test_main "$@"
fi
