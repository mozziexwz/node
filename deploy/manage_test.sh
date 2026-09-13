#!/usr/bin/env bash
# Offline contract tests. No real Docker daemon, apt, /opt or public network.
set -Eeuo pipefail
TEST_REPO=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
mkdir -p "$TEST_REPO/.cache"
TEST_WORK=$(mktemp -d "$TEST_REPO/.cache/manage-test.XXXXXXXX")
trap '[[ -n $TEST_WORK && -d $TEST_WORK && ! -L $TEST_WORK ]] && rm -rf -- "$TEST_WORK"' EXIT
source "$TEST_REPO/deploy/manage.sh"

# Git Bash cannot apply Unix ownership/modes to this managed Windows workspace.
# Linux CI exercises real install/chmod; Windows still tests lifecycle decisions.
if [[ $(uname -s) == MINGW* || $(uname -s) == MSYS* ]]; then
  chmod() { :; }
  install() {
    local directory=0
    while [[ ${1:-} == -* ]]; do case "$1" in -d) directory=1; shift ;; -m) shift 2 ;; *) return 2 ;; esac; done
    if [[ $directory == 1 ]]; then mkdir -p -- "$@"; else cp -- "$1" "$2"; fi
  }
fi

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
expect_failure() { if "$@"; then fail "expected failure: $*"; fi; }
require_platform() { :; }
ensure_docker() { :; }
assert_root_path() { [[ $INSTALL_ROOT == "$TEST_WORK"/* && ! -L $INSTALL_ROOT ]]; }
assert_managed() { assert_root_path && [[ $(<"$INSTALL_ROOT/.managed-by-msboost") == "$MARKER" && -f $INSTALL_ROOT/.env ]]; }
install_launcher() { :; }
check_frontend() { [[ ${MOCK_FRONT_FAIL:-0} == 0 ]]; }
read_tty() { case "${MOCK_CONFIRM:-cancel}:$1" in yes:*DELETE_MSBOOST*) printf DELETE_MSBOOST ;; yes:*) printf /opt/msboost ;; *) printf CANCEL ;; esac; }
remove_managed_installation() {
  [[ $INSTALL_ROOT == "$TEST_WORK"/* && -d $INSTALL_ROOT && ! -L $INSTALL_ROOT ]] || return 1
  printf 'remove-owned-directory %s\n' "$INSTALL_ROOT" >> "$TRACE"
  rm -rf -- "$INSTALL_ROOT"
}

docker() {
  printf '%s\n' "$*" >> "$TRACE"
  local command=${1:-}; shift || true
  case "$command" in
    compose)
      while [[ $# -gt 0 ]]; do
        case "$1" in --project-name|--env-file|-f) shift 2 ;; *) break ;; esac
      done
      case "${1:-}" in
        config) return 0 ;;
        pull)
          [[ ${MOCK_PULL_FAIL:-0} == 0 ]] || return 1
          [[ ${2:-} != server || ${MOCK_SERVER_PULL_FAIL:-0} == 0 ]] ;;
        build) printf built >> "$TEST_WORK/builds" ;;
        exec) printf 'mock-pg-custom-dump' ;;
        up)
          if [[ -f $TEST_WORK/fail-next-up ]]; then rm -- "$TEST_WORK/fail-next-up"; return 1; fi
          return 0 ;;
        down|ps|logs) return 0 ;;
        *) fail "unexpected compose command: $*" ;;
      esac ;;
    ps) [[ ${MOCK_PS_FAIL:-0} == 0 ]] || return 1; [[ ${MOCK_CONTAINERS:-0} == 0 ]] || printf 'existing-container\n'; return 0 ;;
    network)
      if [[ ${1:-} == ls ]]; then [[ ${MOCK_NETWORK:-0} == 0 ]] || printf 'msboost_control\n'; return 0; fi
      return 1 ;;
    image)
      if [[ ${MOCK_MISSING_RELEASE_IMAGE:-0} == 1 && ${2:-} == msboost-release:* && $# == 2 && ! -f $TEST_WORK/image-loaded ]]; then return 1; fi
      if [[ ${1:-} == inspect && ${3:-} == --format ]]; then
        if [[ $4 == *'.RepoDigests'* ]]; then
          case "$2" in ghcr.io/mozziexwz/node*) printf 'ghcr.io/mozziexwz/node@sha256:%064d\n' 1 ;; postgres*) printf 'postgres@sha256:%064d\n' 2 ;; caddy*) printf 'caddy@sha256:%064d\n' 3 ;; esac
        else printf 'linux|%s|sha256:%064d|https://github.com/mozziexwz/node\n' "${MOCK_IMAGE_ARCH:-amd64}" "${MOCK_IMAGE_NUMBER:-1}"; fi
      fi ;;
    load)
      touch "$TEST_WORK/image-loaded"
      if [[ ${MOCK_WRONG_TAG:-0} == 1 ]]; then printf 'Loaded image: unrelated:wrong\n'; else printf 'Loaded image: ghcr.io/mozziexwz/node:%s\n' "$VERSION"; fi ;;
    tag) return 0 ;;
    volume)
      case "${1:-}" in
        ls)
          [[ ${MOCK_VOLUME_LIST_FAIL:-0} == 0 ]] || return 1
          [[ ${MOCK_VOLUMES:-0} == 0 ]] || printf 'msboost_database_data\n'
          return 0 ;;
        inspect)
          [[ ${MOCK_VOLUMES:-0} == 1 ]] || return 1
          if [[ ${2:-} == --format ]]; then printf '%s' "${MOCK_VOLUME_OWNER:-msboost}"; fi ;;
        rm) [[ $# == 2 && $2 =~ ^msboost_(app_data|database_data|caddy_data|caddy_config)$ ]] || fail 'unsafe volume removal' ;;
        *) fail "unexpected volume command: $*" ;;
      esac ;;
    *) fail "unexpected Docker command: $command $*" ;;
  esac
}
curl() {
  printf 'mock-download %s\n' "$*" >> "$TRACE"
  [[ ${MOCK_ARCHIVE_FETCH_FAIL:-0} == 0 ]] || return 22
  local output=''
  while [[ $# -gt 0 ]]; do case "$1" in -o) output=$2; shift 2 ;; *) shift ;; esac; done
  [[ -n $output ]] || fail 'unexpected real curl call'
  if [[ $output == */SHA256SUMS ]]; then
    local digest
    digest=$(printf 'mock-ci-prebuilt-image-archive' | sha256sum); digest=${digest%% *}
    [[ ${MOCK_BAD_CHECKSUM:-0} == 0 ]] || digest=$(printf '%064d' 0)
    printf '%s  msboost-image-linux-amd64.tar.gz\n' "$digest" > "$output"
  else printf 'mock-ci-prebuilt-image-archive' > "$output"; fi
}

fixture() {
  INSTALL_ROOT="$TEST_WORK/install-$1"
  TRACE="$TEST_WORK/trace-$1"
  : > "$TRACE"
  SOURCE_DIR=$TEST_REPO
  VERSION=v0.2.1
  DOMAIN=panel.example.com
  IP_ADDRESS=
  ADMIN_EMAIL=12345678@qq.com
  ALLOW_HTTP=0
  BUILD=0
  RECOVER_INCOMPLETE=0
  STAGE=
  SNAPSHOT=
  MOCK_PULL_FAIL=0
  MOCK_FRONT_FAIL=0
  MOCK_VOLUMES=0
  MOCK_VOLUME_OWNER=msboost
  MOCK_CONFIRM=cancel
  MOCK_SERVER_PULL_FAIL=0
  MOCK_ARCHIVE_FETCH_FAIL=0
  MOCK_BAD_CHECKSUM=0
  MOCK_WRONG_TAG=0
  MOCK_MISSING_RELEASE_IMAGE=0
  MOCK_IMAGE_ARCH=amd64
  MOCK_IMAGE_NUMBER=1
  MOCK_CONTAINERS=0
  MOCK_PS_FAIL=0
  MOCK_VOLUME_LIST_FAIL=0
  MOCK_NETWORK=0
  [[ ! -f $TEST_WORK/image-loaded ]] || rm -- "$TEST_WORK/image-loaded"
}

# Keep dynamically assigned database/server addresses away from Caddy's pinned
# trusted-proxy address even when Caddy starts after the server becomes healthy.
grep -Fq 'ip_range: 172.30.86.128/25' "$TEST_REPO/deploy/compose.yml" || fail 'dynamic address pool overlaps reserved proxy address'
grep -Fq 'gateway: 172.30.86.1' "$TEST_REPO/deploy/compose.yml" || fail 'network gateway contract changed'
grep -Fq 'ipv4_address: 172.30.86.2' "$TEST_REPO/deploy/compose.yml" || fail 'Caddy fixed address missing'
grep -Fq 'TRUSTED_PROXY_CIDRS: 172.30.86.2/32' "$TEST_REPO/deploy/compose.yml" || fail 'trusted proxy does not match Caddy'

fixture normal
install_site
[[ $(env_get "$INSTALL_ROOT/.env" MSBOOST_IMAGE) == ghcr.io/mozziexwz/node@sha256:* ]] || fail 'installed image was not pinned to its pulled digest'
[[ $(env_get "$INSTALL_ROOT/.env" COOKIE_SECURE) == true ]] || fail 'domain install lacks secure cookie'
[[ $(env_get "$INSTALL_ROOT/.env" MASTER_KEY) =~ ^[a-f0-9]{64}$ ]] || fail 'missing random master key'
[[ $(env_get "$INSTALL_ROOT/.env" POSTGRES_PASSWORD) =~ ^[a-f0-9]{64}$ ]] || fail 'missing independent DB password'
[[ $(env_get "$INSTALL_ROOT/.env" MASTER_KEY) != "$(env_get "$INSTALL_ROOT/.env" POSTGRES_PASSWORD)" ]] || fail 'secrets reused'
[[ ! -f $TEST_WORK/builds ]] || fail 'default install built locally'
cp "$INSTALL_ROOT/.env" "$TEST_WORK/original.env"
expect_failure replace_live_environment "$TEST_WORK/nonexistent.env"
cmp -s "$INSTALL_ROOT/.env" "$TEST_WORK/original.env" || fail 'failed atomic environment copy damaged live keys'
repair_site
cmp -s "$INSTALL_ROOT/.env" "$TEST_WORK/original.env" || fail 'repair changed keys/config'
uninstall_site
[[ -f $INSTALL_ROOT/.env ]] || fail 'uninstall deleted environment'
! grep -Eq '(^| )(rm|prune|--volumes|-v)( |$)' "$TRACE" || fail 'uninstall deletes data'
repair_site

VERSION=v0.2.2
MOCK_PULL_FAIL=1
expect_failure upgrade_site
cmp -s "$INSTALL_ROOT/.env" "$TEST_WORK/original.env" || fail 'failed pull modified config'
[[ ! -f $TEST_WORK/builds ]] || fail 'failed pull silently built'
MOCK_PULL_FAIL=0
touch "$TEST_WORK/fail-next-up"
expect_failure upgrade_site
cmp -s "$INSTALL_ROOT/.env" "$TEST_WORK/original.env" || fail 'failed upgrade did not restore prior environment'
[[ -s $SNAPSHOT/database.dump ]] || fail 'upgrade skipped backup'
upgrade_site
[[ $(env_get "$INSTALL_ROOT/.env" MSBOOST_VERSION) == v0.2.2 ]] || fail 'successful upgrade wrong version'
[[ $(env_get "$INSTALL_ROOT/.env" MASTER_KEY) == "$(env_get "$TEST_WORK/original.env" MASTER_KEY)" ]] || fail 'upgrade changed master key'

expect_failure purge_site
[[ -f $INSTALL_ROOT/.env ]] || fail 'cancelled purge deleted environment'
MOCK_CONFIRM=yes
MOCK_VOLUMES=1
MOCK_VOLUME_OWNER=other-project
expect_failure purge_site
[[ -f $INSTALL_ROOT/.env ]] || fail 'foreign volume safeguard failed'
MOCK_VOLUME_OWNER=msboost
purge_site
[[ ! -e $INSTALL_ROOT ]] || fail 'purge did not remove owned fixture'
[[ $(grep -c '^volume rm msboost_' "$TRACE") == 4 ]] || fail 'purge did not target exactly four fixed volumes'
! grep -q 'prune' "$TRACE" || fail 'global Docker prune invoked'

fixture failed-install
MOCK_PULL_FAIL=1
expect_failure install_site
[[ -f $INSTALL_ROOT/.env ]] || fail 'failed install lost initial keys'
[[ ! -f $TEST_WORK/builds ]] || fail 'failed install silently built'
MOCK_PULL_FAIL=0
repair_site

# Reproduce the exact persisted v0.1.1 failure without executing an old unsafe
# installer. This path must never create new secrets or skip a live DB backup.
fixture os-release-recovery
MOCK_PULL_FAIL=1
expect_failure install_site
env_set "$INSTALL_ROOT/.env" MSBOOST_VERSION '12 (bookworm)'
env_set "$INSTALL_ROOT/.env" MSBOOST_IMAGE 'ghcr.io/mozziexwz/node:12 (bookworm)'
cp "$INSTALL_ROOT/.env" "$TEST_WORK/polluted.env"
MOCK_PULL_FAIL=0
RECOVER_INCOMPLETE=1
env_set "$INSTALL_ROOT/.env" MASTER_KEY ''
expect_failure upgrade_site
cp "$TEST_WORK/polluted.env" "$INSTALL_ROOT/.env"
env_set "$INSTALL_ROOT/.env" MSBOOST_IMAGE_ID "sha256:$(printf '%064d' 1)"
expect_failure upgrade_site
cp "$TEST_WORK/polluted.env" "$INSTALL_ROOT/.env"
MOCK_CONTAINERS=1
expect_failure upgrade_site
MOCK_CONTAINERS=0
MOCK_PS_FAIL=1
expect_failure upgrade_site
MOCK_PS_FAIL=0
MOCK_VOLUMES=1
expect_failure upgrade_site
MOCK_VOLUMES=0
MOCK_VOLUME_LIST_FAIL=1
expect_failure upgrade_site
MOCK_VOLUME_LIST_FAIL=0
MOCK_NETWORK=1
expect_failure upgrade_site
MOCK_NETWORK=0
MOCK_PULL_FAIL=1
expect_failure upgrade_site
MOCK_PULL_FAIL=0
cmp -s "$INSTALL_ROOT/.env" "$TEST_WORK/polluted.env" || fail 'rejected recovery modified original environment'
MOCK_FRONT_FAIL=1
expect_failure upgrade_site
[[ $(env_get "$INSTALL_ROOT/.env" MSBOOST_VERSION) == "$VERSION" ]] || fail 'recovery did not replace corrupt version'
cmp -s "$SNAPSHOT/.env" "$TEST_WORK/polluted.env" || fail 'recovery did not back up original environment'
[[ -f $SNAPSHOT/deploy/manage.sh && -f $SNAPSHOT/install.sh ]] || fail 'recovery did not back up deployment scripts'
for key in MSBOOST_DOMAIN MSBOOST_SITE_ADDRESS PUBLIC_URL COOKIE_SECURE ADMIN_EMAIL ADMIN_PASSWORD POSTGRES_PASSWORD MASTER_KEY; do
  [[ $(env_get "$INSTALL_ROOT/.env" "$key") == "$(env_get "$TEST_WORK/polluted.env" "$key")" ]] || fail "recovery changed $key"
done
! grep -q 'pg_dump' "$TRACE" || fail 'incomplete recovery tried to back up nonexistent database'
! grep -Eq '(^| )(rm|prune|--volumes|-v)( |$)' "$TRACE" || fail 'recovery deleted resources'
MOCK_FRONT_FAIL=0
repair_site
expect_failure upgrade_site

fixture invalid-release
VERSION='12 (bookworm)'
expect_failure install_site
[[ ! -e $INSTALL_ROOT/.env ]] || fail 'invalid release reached environment creation'

fixture selected-database
install_site
env_set "$INSTALL_ROOT/.env" MSBOOST_DATABASE_NAME msboost_restore_reviewed
snapshot_deployment
grep -q -- '--dbname=msboost_restore_reviewed' "$TRACE" || fail 'upgrade backed up the original DB after a recovery cutover'
before_dump_count=$(grep -c -- 'pg_dump' "$TRACE")
env_set "$INSTALL_ROOT/.env" MSBOOST_DATABASE_NAME 'invalid;database'
expect_failure snapshot_deployment
[[ $(grep -c -- 'pg_dump' "$TRACE") == "$before_dump_count" ]] || fail 'invalid database name reached pg_dump'

fixture http
DOMAIN=
IP_ADDRESS=203.0.113.10
expect_failure collect_install_settings
ALLOW_HTTP=1
install_site
[[ $(env_get "$INSTALL_ROOT/.env" PUBLIC_URL) == http://203.0.113.10 ]] || fail 'HTTP origin incorrect'
[[ $(env_get "$INSTALL_ROOT/.env" COOKIE_SECURE) == false ]] || fail 'HTTP debug cookie configuration incorrect'
expect_failure valid_domain 'example.com;touch /tmp/injection'
expect_failure valid_domain 'http://example.com'
expect_failure valid_ipv4 '1.2.3.999'
expect_failure valid_ipv4 '1.2.3.4$(id)'

fixture release-fallback
MOCK_SERVER_PULL_FAIL=1
install_site
[[ $(env_get "$INSTALL_ROOT/.env" MSBOOST_IMAGE) == msboost-release:v0.2.1-amd64-* ]] || fail 'release archive was not used after GHCR denial'
[[ $(env_get "$INSTALL_ROOT/.env" MSBOOST_IMAGE_ID) =~ ^sha256:[0-9a-f]{64}$ ]] || fail 'archive imageID was not fixed'
[[ ! -f $TEST_WORK/builds ]] || fail 'archive fallback compiled source'
MOCK_MISSING_RELEASE_IMAGE=1
rm -- "$TEST_WORK/image-loaded"
repair_site
[[ -f $TEST_WORK/image-loaded ]] || fail 'repair did not restore missing archived image'
cp "$INSTALL_ROOT/.env" "$TEST_WORK/archive.env"
MOCK_IMAGE_NUMBER=9
expect_failure repair_site
cmp -s "$INSTALL_ROOT/.env" "$TEST_WORK/archive.env" || fail 'repair accepted changed imageID'

fixture bad-archive
MOCK_SERVER_PULL_FAIL=1
MOCK_BAD_CHECKSUM=1
expect_failure install_site
! grep -q '^load ' "$TRACE" || fail 'unchecked image archive reached Docker load'
fixture wrong-archive-tag
MOCK_SERVER_PULL_FAIL=1
MOCK_WRONG_TAG=1
expect_failure install_site
! grep -q '^compose .* up ' "$TRACE" || fail 'wrong archive tag reached service startup'
fixture wrong-architecture
MOCK_SERVER_PULL_FAIL=1
MOCK_IMAGE_ARCH=arm64
expect_failure install_site
! grep -q '^compose .* up ' "$TRACE" || fail 'wrong image architecture reached service startup'
fixture both-prebuilt-failed
MOCK_SERVER_PULL_FAIL=1
MOCK_ARCHIVE_FETCH_FAIL=1
expect_failure install_site
[[ ! -f $TEST_WORK/builds ]] || fail 'both prebuilt sources failing triggered source build'
fixture dependencies-failed
MOCK_PULL_FAIL=1
expect_failure install_site
! grep -q '^mock-download ' "$TRACE" || fail 'dependency pull failure incorrectly triggered app archive fallback'
printf '%s\n' 'PASS: offline lifecycle, incomplete recovery, atomic config, GHCR/Release fallback, checksum/tag/architecture/imageID, HTTP and input contracts'
