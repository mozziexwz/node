#!/usr/bin/env bash
# Pure adapter contract tests; no Docker, network, systemd or host state.
set -Eeuo pipefail
ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
mkdir -p "$ROOT/.cache"
WORK=$(mktemp -d "$ROOT/.cache/disaster-joint-mock.XXXXXXXX")
trap 'rm -rf -- "$WORK"' EXIT
SOURCE="$ROOT/deploy/disaster_joint_validation.sh"
JOINT_PROJECT=msboost-joint-abc12345
PHASE=manual
INSTALL_ROOT="$WORK/site"
PG=postgres@sha256:$(printf 'a%.0s' {1..64})
trace="$WORK/docker.trace"
die_joint() { return 1; }
assert_owner() {
  case "$1:$2" in
    volume:msboost-joint-abc12345_app_data|volume:msboost-joint-abc12345_database_data|volume:msboost-joint-abc12345_caddy_data|volume:msboost-joint-abc12345_caddy_config|container:$(printf 'b%.0s' {1..64})) return 0 ;;
    *) return 1 ;;
  esac
}
env_get() { [[ $2 == POSTGRES_IMAGE ]]; printf '%s\n' "$PG"; }
local_docker() { printf '%s\n' "$@" > "$trace"; }
dc() { printf '%s\n' "$@" > "$trace"; }
gate() { [[ $1 == "${EXPECT_GATE:-true}" ]]; }
# Extract our own test adapter definitions only, never user/config contents.
awk '/^  docker\(\) \{/ {copy=1} copy {print} copy && /^  \}$/ {exit}' "$SOURCE" > "$WORK/docker-functions.sh"
awk '/^  compose_live\(\) \{/ {copy=1} copy {print} copy && /^  \}$/ {exit}' "$SOURCE" > "$WORK/compose-functions.sh"
source "$WORK/docker-functions.sh"
source "$WORK/compose-functions.sh"
for v in app_data database_data caddy_data caddy_config; do
  docker volume inspect --format '{{index .Labels "com.docker.compose.project"}}' "msboost_$v"
  grep -qx "${JOINT_PROJECT}_$v" "$trace"
done
for v in app_data caddy_data caddy_config; do
  docker run --rm --network none --read-only --user 0:0 --entrypoint tar --mount "type=volume,source=msboost_$v,target=/snapshot,readonly" "$PG" -cf - -C /snapshot .
  grep -qx "type=volume,source=${JOINT_PROJECT}_$v,target=/snapshot,readonly" "$trace"
  grep -qx "msboost.joint=$JOINT_PROJECT" "$trace"
done
if docker volume inspect --format any msboost_app_data_other; then exit 1; fi
if docker volume inspect --format any another_app_data; then exit 1; fi
if docker stop agent; then exit 1; fi
if docker run --rm --network none --read-only --user 0:0 --entrypoint tar --mount type=bind,source=/opt/msboost,target=/snapshot,readonly "$PG" -cf - -C /snapshot .; then exit 1; fi
EXPECT_GATE=true compose_live stop --timeout 60 caddy server
EXPECT_GATE=false compose_live start caddy server
EXPECT_GATE=true compose_live exec -T database pg_dump --username=msboost --dbname=msboost --format=custom
EXPECT_GATE=true compose_live run --rm --no-deps -T --user 0:0 --entrypoint /usr/local/bin/msboost-restore server export
if EXPECT_GATE=false compose_live stop --timeout 60 caddy server; then exit 1; fi
if EXPECT_GATE=true compose_live start caddy server; then exit 1; fi
if compose_live stop --timeout 60 database; then exit 1; fi
if compose_live restart agent; then exit 1; fi
if compose_live run --rm --no-deps -T --user 0:0 --entrypoint /usr/local/bin/msboost-restore server backup-pause begin-maintenance; then exit 1; fi
if compose_live exec -T server bash; then exit 1; fi
grep -q '^//go:build linux && msboost_joint_test$' "$ROOT/internal/control/disaster_joint_fixture_linux_test.go"
grep -q 'bin/joint-control.test' "$SOURCE"
! grep -q 'GITHUB_ACTIONS=true\|systemctl.*msboost-disaster-backup\|disaster_restore$\|/usr/local/bin/msboost disaster-backup' "$SOURCE"
awk '/^cleanup_joint\(\) \{/ {copy=1} copy {print} copy && /^\}$/ {exit}' "$SOURCE" > "$WORK/cleanup-functions.sh"
source "$WORK/cleanup-functions.sh"
# Unknown daemon/inventory state and residual owned resources must not be
# reported as successful cleanup. Transient timer ownership is independent.
local_docker() {
  case "$1 ${2:-}" in
    'info ') [[ $CLEANUP_MODE != daemon-fail ]] ;;
    'ps -aq') [[ $CLEANUP_MODE != inventory-fail ]] || return 1; if [[ $CLEANUP_MODE == residual ]]; then printf 'b%.0s' {1..64}; printf '\n'; fi ;;
    'volume ls'|'network ls') return 0 ;;
    'logs '*|'rm -f') return 0 ;;
    *) return 2 ;;
  esac
}
systemctl() {
  if [[ $1 == stop ]]; then printf '%s\n' "$2" >> "$WORK/timer-stops"; return 0; fi
  [[ $1 == show && $3 == -p && $5 == --value ]] || return 2
  case "$4" in
    LoadState) printf 'loaded\n' ;;
    Description) if [[ $2 == *.timer ]]; then printf '%s strict full-backup fixture timer\n' "$JOINT_PROJECT"; else printf '%s strict full-backup fixture\n' "$JOINT_PROJECT"; fi ;;
    Triggers) if [[ $CLEANUP_MODE == foreign-timer ]]; then printf 'another.service\n'; else printf '%s.service\n' "$TIMER"; fi ;;
    *) return 2 ;;
  esac
}
for mode in success daemon-fail inventory-fail residual foreign-timer; do
  result=0
  (CLEANUP_MODE=$mode RESOURCES=1 TIMER_CREATED=0 ALIAS= TIMER="$JOINT_PROJECT-backup"; [[ $mode != foreign-timer ]] || TIMER_CREATED=1; cleanup_joint) > "$WORK/cleanup-$mode.log" 2>&1 || result=$?
  if [[ $mode == success ]]; then [[ $result == 0 ]]; else [[ $result != 0 ]]; fi
done
if [[ -f $WORK/timer-stops ]]; then ! grep -qx "$JOINT_PROJECT-backup.timer" "$WORK/timer-stops"; fi
printf 'disaster joint adapter contracts: PASS (no real Docker/network/systemd)\n'
