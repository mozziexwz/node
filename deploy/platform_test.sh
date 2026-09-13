#!/usr/bin/env bash
# Run inside the official Debian 12 container with the repository mounted read-only.
# This intentionally uses the real /etc/os-release and the real require_platform.
# No apt, network request, Docker daemon or deployment is used by this test.
set -Eeuo pipefail

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
[[ $(id -u) == 0 && $(uname -s) == Linux && -r /etc/os-release ]] || {
  printf '%s\n' 'Run this test as root inside: docker run --rm --network none -v "$PWD:/src:ro" debian:12 bash /src/deploy/platform_test.sh' >&2
  exit 2
}

# Prove this is the real Debian fixture containing the colliding VERSION key.
(
  . /etc/os-release
  [[ ${ID:-} == debian && ${VERSION_ID:-} == 12 && -n ${VERSION:-} && -n ${NAME:-} && -n ${PRETTY_NAME:-} ]] || fail 'real Debian 12 os-release fixture is required'
  [[ $VERSION != v0.2.2 ]] || fail 'OS fixture does not exercise release-version collision'
)

TEST_REPO=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
readonly EXPECTED_VERSION=v0.2.2
source "$TEST_REPO/deploy/manage.sh"
[[ $VERSION == "$EXPECTED_VERSION" ]] || fail 'default release version does not match this regression fixture'

VERSION=$EXPECTED_VERSION
PRETTY_NAME=caller-pretty-sentinel
NAME=caller-name-sentinel
ID=caller-id-sentinel
VERSION_ID=caller-version-id-sentinel
VERSION_CODENAME=caller-codename-sentinel
HOME_URL=caller-home-url-sentinel
before=$(declare -p VERSION PRETTY_NAME NAME ID VERSION_ID VERSION_CODENAME HOME_URL)
require_platform
after=$(declare -p VERSION PRETTY_NAME NAME ID VERSION_ID VERSION_CODENAME HOME_URL)
[[ $before == "$after" ]] || fail 'require_platform leaked os-release values into its caller'
printf '%s\n' 'PASS: real Debian 12 platform check preserves caller VERSION and OS-name sentinels'

check_install_entry() (
  local mode=$1
  source "$TEST_REPO/deploy/manage.sh"
  PRETTY_NAME=entry-pretty-sentinel
  NAME=entry-name-sentinel
  local entered=0
  # Only the action that would install software or mutate /opt is replaced.
  # The CLI parser, release validation, platform check, root-path guard and
  # lock all run unchanged. The lock exists only in this disposable container.
  install_site() {
    [[ $VERSION == "$EXPECTED_VERSION" ]] || fail "$mode CLI installation received OS version instead of release version"
    [[ $PRETTY_NAME == entry-pretty-sentinel && $NAME == entry-name-sentinel ]] || fail "$mode CLI platform check leaked OS names"
    entered=1
  }
  docker() { fail 'test attempted to invoke Docker'; }
  apt-get() { fail 'test attempted to install packages'; }
  curl() { fail 'test attempted a network request'; }
  case "$mode" in
    default)
      manage_main install --domain panel.example.com --email 12345678@qq.com ;;
    explicit)
      VERSION=v9.9.9
      manage_main install --version "$EXPECTED_VERSION" --domain panel.example.com --email 12345678@qq.com ;;
    *) fail 'invalid test mode' ;;
  esac
  [[ $entered == 1 && $VERSION == "$EXPECTED_VERSION" ]] || fail "$mode CLI did not reach the safe installation stub"
  printf 'PASS: real Debian 12 manage_main install (%s version)\n' "$mode"
)

check_install_entry default
check_install_entry explicit
