#!/usr/bin/env bash
# Offline lifecycle contracts: no real Docker, apt, network, systemd or /opt.
set -Eeuo pipefail
TEST_REPO=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
mkdir -p "$TEST_REPO/.cache"
TEST_WORK=$(mktemp -d "$TEST_REPO/.cache/disaster-test.XXXXXXXX")
trap '[[ -d $TEST_WORK && ! -L $TEST_WORK && $(realpath -m "$TEST_WORK") == "$TEST_WORK" ]] && command rm -rf -- "$TEST_WORK"' EXIT
source "$TEST_REPO/deploy/manage.sh"
source "$TEST_REPO/deploy/disaster.sh"

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
trace() { printf '%s\n' "$*" >> "$TRACE"; }
assert_has() { grep -Fqx -- "$2" "$1" || fail "missing trace: $2"; }
assert_absent() { if grep -Fq -- "$2" "$1"; then fail "unexpected trace: $2"; fi; }
expect_failure() { if "$@"; then fail 'expected operation to reject the unsafe/incomplete state'; fi; }

# The production cleanup checks a fixed /root prefix. Test staging deliberately
# lives outside it; the suite's own exact-path trap removes this private fixture.
# This exercises the real EXIT resume branch without ever touching /root.
mktemp() {
  if [[ $* == '-d /root/msboost-disaster-work.XXXXXXXX' ]]; then command mktemp -d "$CASE_ROOT/staging.XXXXXXXX"
  else
    [[ ${*: -1} == "$CASE_ROOT"/* ]] || fail 'unexpected temporary path'
    command mktemp "$@"
  fi
}
rm() {
  local item
  for item in "$@"; do case "$item" in -*) ;; "$TEST_WORK"|"$TEST_WORK"/*) ;; *) fail 'refusing removal outside test fixture' ;; esac; done
  command rm "$@"
}
if [[ $(uname -s) == MINGW* || $(uname -s) == MSYS* ]]; then
  chmod() { :; }
  install() {
    local directory=0
    while [[ ${1:-} == -* ]]; do case "$1" in -d) directory=1; shift ;; -m) shift 2 ;; *) return 2 ;; esac; done
    if [[ $directory == 1 ]]; then mkdir -p -- "$@"; else cp -- "$1" "$2"; fi
  }
fi
assert_root_path() { [[ $INSTALL_ROOT == "$CASE_ROOT"/installation && ! -L $INSTALL_ROOT ]]; }
assert_managed() { assert_root_path && [[ -f $INSTALL_ROOT/.managed-by-msboost && $(<"$INSTALL_ROOT/.managed-by-msboost") == "$MARKER" && -f $INSTALL_ROOT/.env && ! -L $INSTALL_ROOT/.env && -d $INSTALL_ROOT/deploy ]]; }
require_platform() { :; }
ensure_docker() { trace ensure-docker; }
install_launcher() { trace install-launcher; }
check_frontend() { trace frontend-check; [[ $MOCK_FAIL != frontend ]]; }
server_identity() { printf 'sha256:%064d' 1; }
read_tty() { trace confirmation-request; printf '%s' "$MOCK_CONFIRM"; }
ss() { :; }
curl() { fail 'contract attempted a network request'; }
apt-get() { fail 'contract attempted package installation'; }
systemctl() { fail 'contract attempted real systemd management'; }
disaster_tool() { DISASTER_TOOL=mock_disaster_tool; }
load_release_image() { trace fallback-image; [[ $1 == "$(env_get "$STAGE/.env" MSBOOST_IMAGE_ID)" && $MOCK_FAIL != fallback ]] || return 1; }
disaster_resume_clock() { printf '%s' "$MOCK_RESUME_ELAPSED"; }
sleep() {
  [[ $1 == 1 || $1 == 2 ]] || fail 'resume sleep exceeded deadline granularity'
  MOCK_RESUME_ELAPSED=$((MOCK_RESUME_ELAPSED + $1))
  trace "resume-elapsed $MOCK_RESUME_ELAPSED"
}

docker() {
  trace "docker $*"
  case "$1:$2" in
    inspect:--type)
      local container=${*: -1} service=server
      [[ $container == "$(printf '%064d' 1)" || $container == "$(printf '%064d' 2)" ]] || fail 'inspected a replacement/unexpected container'
      [[ $container != "$(printf '%064d' 1)" ]] || service=caddy
      if [[ $* == *'com.docker.compose.project'* ]]; then
        if [[ $MOCK_RESUME == foreign ]]; then printf 'other|%s' "$service"; else printf 'msboost|%s' "$service"; fi
      else
        case "$MOCK_RESUME" in
          vanished) return 1 ;;
          exited|dead) printf '%s|none|required' "$MOCK_RESUME" ;;
          unhealthy) printf 'running|unhealthy|required' ;;
          invalid) printf 'unknown|none|none' ;;
          timeout|missing-health) printf 'running|none|required' ;;
          shared-deadline)
            if [[ $service == caddy && $MOCK_RESUME_ELAPSED -ge 176 ]]; then printf 'running|healthy|required'; else printf 'running|starting|required'; fi ;;
          *)
            if [[ $service == caddy ]]; then printf 'running|none|none'
            elif [[ $MOCK_RESUME == delayed && $MOCK_RESUME_ELAPSED -lt 4 ]]; then printf 'running|starting|required'
            else printf 'running|healthy|required'; fi ;;
        esac
      fi ;;
    ps:-aq) [[ $MOCK_COLLISION == 0 ]] || printf occupied ;;
    network:inspect) return 1 ;;
    volume:inspect)
      local name=${*: -1}
      [[ $name =~ ^msboost_(app_data|database_data|caddy_data|caddy_config)$ ]] || fail 'unexpected volume'
      [[ $CASE_KIND == backup || -f $CASE_ROOT/$name ]] || return 1
      [[ $MOCK_FAIL != volume-owner ]] || { printf other-project; return; }
      [[ $3 != --format ]] || printf msboost ;;
    volume:create)
      local name=${*: -1}
      [[ $name =~ ^msboost_(app_data|database_data|caddy_data|caddy_config)$ && $* == *'--label com.docker.compose.project=msboost'* ]] || fail 'unowned volume creation'
      : > "$CASE_ROOT/$name" ;;
    pull:*) [[ $MOCK_FAIL != pull && $MOCK_FAIL != fallback ]] ;;
    run:*)
      [[ " $* " == *' --network none '* && " $* " == *' --read-only '* && " $* " == *' --entrypoint tar '* ]] || fail 'unexpected image execution'
      if [[ " $* " == *'target=/snapshot,readonly'* ]]; then
        [[ $MOCK_FAIL != volume-export ]] || return 1
        printf isolated-volume-archive
      else
        [[ " $* " == *'target=/restore'* && " $* " == *' --numeric-owner'* ]] || fail 'unexpected restore mount'
        [[ -f $INSTALL_ROOT/.disaster-incomplete ]] || fail 'volume imported without restore isolation marker'
        [[ $MOCK_FAIL != volume-import ]] || return 1
        : > "$CASE_ROOT/volumes-imported"
      fi ;;
    *) fail 'unexpected Docker operation' ;;
  esac
}

compose_live() {
  trace "compose $*"
  case "$1" in
    ps)
      if [[ $* == 'ps --status running --services' ]]; then printf '%s\n' "$MOCK_RUNNING"
      else
        [[ $* == 'ps --all --quiet caddy' || $* == 'ps --all --quiet server' ]] || fail 'unexpected resume discovery'
        [[ $MOCK_RESUME != missing ]] || return 0
        [[ $MOCK_RESUME != duplicate ]] || { printf '%064d\n%064d\n' 1 2; return; }
        if [[ ${*: -1} == caddy ]]; then printf '%064d' 1; else printf '%064d' 2; fi
      fi ;;
    stop)
      [[ $* == 'stop --timeout 60 caddy server' || $* == 'stop --timeout 60 server' || $* == 'stop --timeout 60 caddy' ]] || fail 'stopped unexpected services'
      : > "$CASE_ROOT/paused"
      [[ $MOCK_FAIL != stop ]] ;;
    start)
      [[ $* == 'start caddy server' || $* == 'start server' || $* == 'start caddy' ]] || fail 'resume uses unsupported flags or includes a previously stopped service'
      [[ $MOCK_FAIL != resume ]] || return 1
      rm -f -- "$CASE_ROOT/paused" ;;
    exec)
      [[ " $* " == *' database pg_dump '* && " $* " == *' --format=custom '* ]] || fail 'backup does not export PostgreSQL read-only'
      [[ $MOCK_FAIL != dump ]] || return 1
      [[ $MOCK_FAIL != empty-dump ]] || return 0
      printf isolated-pg-dump ;;
    run)
      [[ " $* " == *' --rm --no-deps -T '* && " $* " == *' --entrypoint /usr/local/bin/msboost-restore server '* ]] || fail 'application entrypoint or dependency started by helper'
      if [[ " $* " == *' server export '* ]]; then
        [[ $MOCK_FAIL != state-export ]] || return 1
        printf isolated-encrypted-state
      else
        [[ " $* " == *' --backup /recovery/state.msb '* && " $* " == *' --postgres-new-database msboost_restore_'* && " $* " == *' --confirm-disaster-restore '* ]] || fail 'restore did not use isolated database helper'
        [[ -f $INSTALL_ROOT/.disaster-incomplete && -f $CASE_ROOT/volumes-imported ]] || fail 'state import ran out of order'
        [[ $MOCK_FAIL != state-import ]] || return 1
        : > "$CASE_ROOT/state-imported"
      fi ;;
    config) [[ $MOCK_FAIL != config ]] ;;
    pull) [[ $MOCK_FAIL != dependencies ]] ;;
    up)
      if [[ ! -f $CASE_ROOT/state-imported && $CASE_KIND == restore ]]; then
        [[ $* == 'up -d --no-deps --no-build --pull never --wait --wait-timeout 180 database' ]] || fail 'application/proxy created before state import'
      else
        [[ ! -e $INSTALL_ROOT/.disaster-incomplete && ! -L $INSTALL_ROOT/.disaster-incomplete ]] || fail 'startup bypassed incomplete recovery'
      fi
      [[ $MOCK_FAIL != database-start ]] ;;
    create) fail 'must not create app/proxy before offline import' ;;
    down) [[ $* == 'down --timeout 30' ]] || fail 'uninstall requested volume deletion' ;;
    *) fail 'unexpected Compose operation' ;;
  esac
}

mock_disaster_tool() {
  [[ $1 == disaster ]] || fail 'unknown helper command'
  local action=$2 output='' directory='' archive='' field=''; shift 2
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --output) output=$2; shift 2 ;; --dir) directory=$2; shift 2 ;;
      --archive) archive=$2; shift 2 ;; --field) field=$2; shift 2 ;;
      --file|--config) shift 2 ;; *) fail 'unknown helper argument' ;;
    esac
  done
  trace "helper $action"
  case "$action" in
    config-get) [[ $field == localDir ]] || fail 'unexpected config field'; printf '%s' "$CASE_ROOT/root-backups" ;;
    prepare-dir) mkdir -p -- "$directory" ;;
    validate-volume) [[ $MOCK_FAIL != archive-member ]] ;;
    pack)
      [[ ! -f $CASE_ROOT/paused ]] || fail 'packing before service recovery'
      [[ -s $directory/state.msb && -s $directory/database.dump && -s $directory/deployment.tar ]] || fail 'snapshot missing data component'
      [[ $MOCK_FAIL != pack ]] || return 1
      printf verified-local-bundle > "$output" ;;
    upload) [[ -s $archive ]] || fail 'upload before local archive exists'; [[ $MOCK_FAIL != upload ]] ;;
    retain) [[ $MOCK_FAIL != retention ]] ;;
    verify) [[ $MOCK_FAIL != archive-verify ]] ;;
    unpack)
      [[ $MOCK_FAIL != unpack ]] || return 1
      mkdir -p -- "$directory"
      cp -- "$CASE_ROOT/recovery.env" "$directory/site.env"
      for field in app_data caddy_data caddy_config; do printf isolated-volume > "$directory/$field.tar"; done
      printf isolated-encrypted-state > "$directory/state.msb"
      printf isolated-pg-dump > "$directory/database.dump"
      # An archived script is data only; restore must install SOURCE_DIR files.
      printf '#!/bin/sh\ntouch %s\n' "$CASE_ROOT/archived-code-ran" > "$directory/deployment.tar" ;;
    *) fail 'unexpected helper command' ;;
  esac
}

fixture() {
  local name=$1
  CASE_ROOT="$TEST_WORK/$name"; mkdir -p "$CASE_ROOT"
  TRACE="$CASE_ROOT/trace"; : > "$TRACE"
  INSTALL_ROOT="$CASE_ROOT/installation"
  SOURCE_DIR="$TEST_REPO"
  VERSION=v0.2.3
  CASE_KIND=backup
  MOCK_RUNNING=$'caddy\nserver\ndatabase'
  MOCK_FAIL= MOCK_COLLISION=0 MOCK_CONFIRM=RESTORE_NEW_MSBOOST MOCK_RESUME=healthy MOCK_RESUME_ELAPSED=0
  DISASTER_TOOL= DISASTER_TOOL_DIR= DISASTER_TOOL_CONTAINER= DISASTER_WORK= DISASTER_RESUME=0
  DISASTER_RUNNING=() STAGE= SNAPSHOT= BUILD=0
  DISASTER_ARCHIVE="$CASE_ROOT/source.tar.gz"; printf fixture-archive > "$DISASTER_ARCHIVE"
  {
    printf '%s\n' MSBOOST_DOMAIN=panel.example.com MSBOOST_SITE_ADDRESS=panel.example.com PUBLIC_URL=https://panel.example.com COOKIE_SECURE=true "MSBOOST_VERSION=$VERSION"
    printf 'MSBOOST_IMAGE=ghcr.io/mozziexwz/node@sha256:%064d\nMSBOOST_IMAGE_ID=sha256:%064d\nPOSTGRES_IMAGE=postgres@sha256:%064d\nCADDY_IMAGE=caddy@sha256:%064d\n' 1 1 2 3
    printf '%s\n' ADMIN_EMAIL=123456789@qq.com ADMIN_PASSWORD=isolated-admin-password POSTGRES_PASSWORD=isolated-postgres-password
    printf 'MASTER_KEY=%064d\n' 9
  } > "$CASE_ROOT/recovery.env"
}
installed_fixture() {
  fixture "$1"
  mkdir -p "$INSTALL_ROOT/deploy"
  printf '%s\n' "$MARKER" > "$INSTALL_ROOT/.managed-by-msboost"
  cp -- "$CASE_ROOT/recovery.env" "$INSTALL_ROOT/.env"
  cp -- "$TEST_REPO/install.sh" "$INSTALL_ROOT/install.sh"
  printf '{}\n' > "$INSTALL_ROOT/disaster.json"
}
run_backup() { (trap disaster_cleanup EXIT; disaster_backup); }
run_restore() { (trap disaster_cleanup EXIT; disaster_restore); }

for fault in none stop dump empty-dump state-export volume-export archive-member resume pack upload retention; do
  installed_fixture "backup-$fault"
  [[ $fault == none ]] || MOCK_FAIL=$fault
  if [[ $fault == none ]]; then run_backup; else expect_failure run_backup; fi
  assert_has "$TRACE" 'compose stop --timeout 60 caddy server'
  assert_has "$TRACE" 'compose start caddy server'
  assert_absent "$TRACE" 'compose start database'
  if [[ $fault == resume ]]; then
    [[ $(grep -c '^compose start ' "$TRACE") == 2 ]] || fail 'resume failure was not retried by real EXIT cleanup'
    assert_absent "$TRACE" 'helper pack'
  else [[ ! -f $CASE_ROOT/paused ]] || fail "service remained paused after $fault"; fi
  if [[ $fault == upload ]]; then
    [[ $(find "$CASE_ROOT/root-backups" -maxdepth 1 -type f | wc -l) == 1 ]] || fail 'remote failure lost local archive'
    assert_absent "$TRACE" 'helper retain'
  fi
done
installed_fixture backup-server-only
MOCK_RUNNING=$'server\ndatabase'
run_backup
assert_has "$TRACE" 'compose start server'
assert_absent "$TRACE" 'compose start caddy'
installed_fixture backup-database-only
MOCK_RUNNING=database
run_backup
assert_absent "$TRACE" 'compose stop'
assert_absent "$TRACE" 'compose start'
for fault in database-missing volume-owner; do
  installed_fixture "backup-$fault"
  if [[ $fault == database-missing ]]; then MOCK_RUNNING=$'caddy\nserver'; else MOCK_FAIL=volume-owner; fi
  expect_failure run_backup
  assert_absent "$TRACE" 'compose stop'
done

# Run the real compatibility helper, not a mocked health wrapper. The Compose
# stub above rejects every start flag, modelling the older installed CLI.
installed_fixture resume-delayed
MOCK_RESUME=delayed
disaster_resume_services caddy server
[[ $MOCK_RESUME_ELAPSED == 4 ]] || fail 'returned before checked service became healthy'
assert_has "$TRACE" 'compose start caddy server'
assert_absent "$TRACE" 'compose up'
for fault in missing duplicate foreign vanished exited dead unhealthy invalid timeout missing-health shared-deadline; do
  installed_fixture "resume-$fault"
  MOCK_RESUME=$fault
  expect_failure disaster_resume_services caddy server
  case "$fault" in
    missing|duplicate|foreign) assert_absent "$TRACE" 'compose start' ;;
    timeout|missing-health|shared-deadline) [[ $MOCK_RESUME_ELAPSED == 180 ]] || fail 'resume deadline is not one shared 180-second bound' ;;
    *) [[ $MOCK_RESUME_ELAPSED == 0 ]] || fail 'terminal service failure did not fail immediately' ;;
  esac
  assert_absent "$TRACE" 'compose up'
done
installed_fixture resume-invalid-service
expect_failure disaster_resume_services database
expect_failure disaster_resume_services server server
assert_absent "$TRACE" 'compose start'
installed_fixture backup-unhealthy-retry
MOCK_RESUME=unhealthy
expect_failure run_backup
[[ $(grep -c '^compose start ' "$TRACE") == 2 ]] || fail 'health failure was not retried by EXIT cleanup'
assert_absent "$TRACE" 'helper pack'

for fault in none archive-verify unpack archive-member config dependencies volume-import database-start state-import frontend; do
  fixture "restore-$fault"; CASE_KIND=restore
  [[ $fault == none ]] || MOCK_FAIL=$fault
  if [[ $fault == none ]]; then run_restore; else expect_failure run_restore; fi
  [[ ! -e $CASE_ROOT/archived-code-ran ]] || fail 'archived deployment scripts executed'
  if [[ $fault == none || $fault == frontend ]]; then
    [[ -f $CASE_ROOT/state-imported && ! -e $INSTALL_ROOT/.disaster-incomplete ]] || fail 'successful import did not clear isolation marker'
    [[ $(env_get "$INSTALL_ROOT/.env" MSBOOST_DATABASE_NAME) == msboost_restore_* ]] || fail 'restore did not select new DB'
    [[ $(env_get "$INSTALL_ROOT/.env" MASTER_KEY) == "$(env_get "$CASE_ROOT/recovery.env" MASTER_KEY)" ]] || fail 'restore regenerated master key'
    cmp "$INSTALL_ROOT/deploy/manage.sh" "$TEST_REPO/deploy/manage.sh" || fail 'restore used archived manager'
    assert_has "$TRACE" 'compose up -d --no-build --pull never --wait --wait-timeout 180 database server'
  elif [[ -e $INSTALL_ROOT/.env ]]; then
    [[ -f $INSTALL_ROOT/.disaster-incomplete ]] || fail "failed $fault lost isolation marker"
    before=$(wc -l < "$TRACE")
    expect_failure start_live
    expect_failure repair_site
    [[ $(wc -l < "$TRACE") == "$before" ]] || fail 'incomplete restore attempted Docker startup/repair'
  else
    [[ ! -e $INSTALL_ROOT ]] || fail 'unverified archive created installed site'
  fi
done

fixture restore-existing; CASE_KIND=restore; mkdir "$INSTALL_ROOT"
expect_failure run_restore
[[ ! -s $TRACE ]] || fail 'existing installation touched before cold-restore rejection'
fixture restore-collision; CASE_KIND=restore; MOCK_COLLISION=1
expect_failure run_restore
[[ ! -e $INSTALL_ROOT ]] || fail 'colliding Docker installation was claimed'
fixture restore-cancelled; CASE_KIND=restore; MOCK_CONFIRM=CANCEL
expect_failure run_restore
assert_absent "$TRACE" 'ensure-docker'
fixture restore-incomplete-symlink
mkdir -p "$INSTALL_ROOT/deploy"; cp "$CASE_ROOT/recovery.env" "$INSTALL_ROOT/.env"; printf '%s\n' "$MARKER" > "$INSTALL_ROOT/.managed-by-msboost"
if ln -s "$CASE_ROOT/missing-marker" "$INSTALL_ROOT/.disaster-incomplete" 2>/dev/null && [[ -L $INSTALL_ROOT/.disaster-incomplete ]]; then
  expect_failure start_live
  expect_failure repair_site
  [[ ! -s $TRACE ]] || fail 'dangling marker symlink bypassed recovery guard'
else printf 'SKIP: marker symlink needs Linux symlink support\n'; fi

for invalid in duplicate interpolation unknown-key inconsistent-https malformed-image; do
  fixture "environment-$invalid"
  case "$invalid" in
    duplicate) printf '%s\n' PUBLIC_URL=https://other.example >> "$CASE_ROOT/recovery.env" ;;
    interpolation) printf '%s\n' 'MSBOOST_DATABASE_NAME=$(touch SHOULD_NOT_EXECUTE)' >> "$CASE_ROOT/recovery.env" ;;
    unknown-key) printf '%s\n' LD_PRELOAD=/tmp/untrusted >> "$CASE_ROOT/recovery.env" ;;
    inconsistent-https) env_set "$CASE_ROOT/recovery.env" PUBLIC_URL https://other.example ;;
    malformed-image) env_set "$CASE_ROOT/recovery.env" MSBOOST_IMAGE ghcr.io/other/image:latest ;;
  esac
  expect_failure disaster_validate_environment "$CASE_ROOT/recovery.env"
  CASE_KIND=restore
  expect_failure run_restore
  [[ ! -e $INSTALL_ROOT ]] || fail 'unsafe archive environment created installation'
  assert_absent "$TRACE" 'docker pull'
done

installed_fixture uninstall-retains-backups
mkdir -p "$CASE_ROOT/root-backups" "$INSTALL_ROOT/backups"
printf retained > "$CASE_ROOT/root-backups/cold-backup.tar.gz"
printf retained > "$INSTALL_ROOT/backups/local.msb"
disaster_timer() { [[ $1 == off ]] || fail 'uninstall enabled a timer'; trace timer-off; }
uninstall_site
assert_has "$TRACE" timer-off
assert_has "$TRACE" 'compose down --timeout 30'
[[ $(head -n 1 "$TRACE") == timer-off ]] || fail 'timer was not disabled before uninstall'
[[ -f $CASE_ROOT/root-backups/cold-backup.tar.gz && -f $INSTALL_ROOT/backups/local.msb && -f $INSTALL_ROOT/.env && -f $INSTALL_ROOT/disaster.json ]] || fail 'uninstall removed retained recovery material'
assert_absent "$TRACE" 'docker volume rm'
printf 'PASS: isolated disaster backup/resume, cold restore guards, environment provenance and uninstall retention contracts\n'
