#!/usr/bin/env bash
# Offline root/TTY/container contracts. No real Docker, PG, /opt or network.
set -Eeuo pipefail
TEST_REPO=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
mkdir -p "$TEST_REPO/.cache"
TEST_WORK=$(mktemp -d "$TEST_REPO/.cache/backup-activity-test.XXXXXXXX")
trap '[[ $TEST_WORK == "$TEST_REPO/.cache/backup-activity-test."* && ! -L $TEST_WORK && $(realpath -m "$TEST_WORK") == "$TEST_WORK" ]] && rm -rf -- "$TEST_WORK"' EXIT
source "$TEST_REPO/deploy/manage.sh"
source "$TEST_REPO/deploy/backup_activity_recovery.sh"
INSTALL_ROOT="$TEST_WORK/site"
mkdir -p "$INSTALL_ROOT"
printf '%s\n' "$MARKER" > "$INSTALL_ROOT/.managed-by-msboost"
printf 'MSBOOST_IMAGE=ghcr.io/mozziexwz/node@sha256:%064d\nMSBOOST_IMAGE_ID=sha256:%064d\nMSBOOST_DATABASE_NAME=msboost_restore_test\n' 1 1 > "$INSTALL_ROOT/.env"
TRACE="$TEST_WORK/trace" OUTPUT="$TEST_WORK/output"
TEST_ID=backup-123_TEST
TEST_FINGERPRINT=$(printf '%064d' 9)
MOCK_FAIL= MOCK_UID=0 MOCK_SYSTEM=Linux MOCK_MODE=600
MOCK_CONFIRM="RECONCILE_BACKUP $TEST_ID"
MOCK_REPORT=$(printf 'MSBOOST_BACKUP_ACTIVITY_INSPECT_V1\n1\n1\n%s\n%s\n1789400000000\n' "$TEST_ID" "$TEST_FINGERPRINT")
VALID_REPORT=$MOCK_REPORT
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
trace() { printf '%s\n' "$*" >> "$TRACE"; }
# Exercise the real manager wrapper before replacing Docker with the fixtures
# below. Reconcile/recovery must pass exactly one local --host even if the
# caller's environment points at a remote daemon.
mkdir -p "$TEST_WORK/mock-bin"
printf '%s\n' '#!/usr/bin/env bash' 'printf "%s|%s|%s|%s|%s|%s\n" "$1" "$2" "$3" "${DOCKER_HOST-unset}" "${DOCKER_CONTEXT-unset}" "${DOCKER_DEFAULT_PLATFORM-unset}"' > "$TEST_WORK/mock-bin/docker"
chmod 0700 "$TEST_WORK/mock-bin/docker"
pin_result=$(PATH="$TEST_WORK/mock-bin:$PATH" DOCKER_HOST=tcp://untrusted.invalid DOCKER_CONTEXT=untrusted DOCKER_DEFAULT_PLATFORM=linux/arm64 backup_activity_docker ps)
[[ $pin_result == '--host|unix:///var/run/docker.sock|ps|unset|unset|unset' ]] || fail 'recovery helper did not invoke the real manager wrapper with one pinned local Docker host'
id() { printf '%s' "$MOCK_UID"; }
uname() { if [[ ${1:-} == -m ]]; then printf x86_64; else printf '%s' "$MOCK_SYSTEM"; fi; }
stat() { case "$2" in %u) printf 0 ;; %a) printf '%s' "$MOCK_MODE" ;; *) fail 'unexpected stat' ;; esac; }
assert_managed() { [[ $INSTALL_ROOT == "$TEST_WORK/site" ]]; }
backup_activity_open_tty() {
  [[ $MOCK_FAIL != tty ]] || return 1
  trace tty-open
  exec {tty_fd}</dev/null
}
backup_activity_read_confirmation() {
  trace tty-confirm
  [[ $MOCK_FAIL != eof ]] || return 1
  confirmation=$MOCK_CONFIRM
}
docker() {
  [[ $1 != --host && -z ${DOCKER_HOST+x} && -z ${DOCKER_CONTEXT+x} && -z ${DOCKER_DEFAULT_PLATFORM+x} ]] || fail 'helper bypassed the pinned manager Docker wrapper or allowed a remote context'
  [[ "$*" != *"$TEST_FINGERPRINT"* && "$*" != *RECONCILE_BACKUP* ]] || fail 'confirmation proof leaked into process arguments'
  trace "$*"
  case "$1" in
    image)
      [[ $2 == inspect && $MOCK_FAIL != image-unavailable ]] || return 1
      if [[ $MOCK_FAIL == image ]]; then printf 'linux|amd64|sha256:%064d|https://example.invalid/other\n' 1
      else printf 'linux|amd64|sha256:%064d|https://github.com/mozziexwz/node\n' 1; fi ;;
    ps)
      if [[ " $* " == *' label=com.docker.compose.service=server '* ]]; then
        [[ $MOCK_FAIL != server-missing ]] || return 0
        printf '%064d' 5
        [[ $MOCK_FAIL != server-duplicate ]] || printf '\n%064d' 6
      else
        [[ $MOCK_FAIL != database-missing ]] || return 0
        printf '%064d' 4
        [[ $MOCK_FAIL != database-duplicate ]] || printf '\n%064d' 6
      fi ;;
    inspect)
      if [[ ${*: -1} == "$(printf '%064d' 4)" ]]; then
        if [[ $MOCK_FAIL == database ]]; then printf 'msboost|database|running|unhealthy'; else printf 'msboost|database|running|healthy'; fi
      else
        [[ ${*: -1} == "$(printf '%064d' 5)" ]] || fail 'unexpected container'
        if [[ "$*" == *'{{and'* ]]; then
          [[ "$*" == *'DATABASE_NAME=msboost_restore_test'* && "$*" == *'DATABASE_URL='* && "$*" == *'DATA_DIR=/app/data'* && "$*" == *'DATABASE_PORT=5432'* ]] || fail 'server configuration omitted identity fields'
          case "$MOCK_FAIL" in server-database|server-url|server-env-duplicate) printf false ;; *) printf true ;; esac
          return
        fi
        case "$MOCK_FAIL" in
          server-bind) printf 'sha256:%064d|msboost|server|bind|' 1 ;;
          server-volume) printf 'sha256:%064d|msboost|server|volume|other_data' 1 ;;
          server-image) printf 'sha256:%064d|msboost|server|volume|msboost_app_data' 2 ;;
          *) printf 'sha256:%064d|msboost|server|volume|msboost_app_data' 1 ;;
        esac
      fi ;;
    volume)
      [[ $2 == inspect && ${*: -1} == msboost_app_data ]] || fail 'unexpected volume access'
      if [[ $MOCK_FAIL == volume ]]; then printf 'another|app_data'; else printf 'msboost|app_data'; fi ;;
    run)
      [[ " $* " == *' --rm -i --pull never --read-only --user 0:0 '* && " $* " == *' --cap-drop ALL --cap-add DAC_READ_SEARCH --security-opt no-new-privileges:true --log-driver none --ulimit core=0 '* ]] || fail 'helper isolation missing'
      [[ " $* " == *" --network container:$(printf '%064d' 4) "* && " $* " == *" --env-file $INSTALL_ROOT/.env "* ]] || fail 'database namespace/configuration changed'
      [[ " $* " == *' --env DATABASE_URL= --env DATABASE_HOST=127.0.0.1 --env DATABASE_PORT=5432 --env DATABASE_USER=msboost --env DATABASE_NAME=msboost_restore_test --env DATABASE_SSLMODE=disable '* ]] || fail 'database redirected or restored DB selection lost'
      [[ " $* " == *' --mount type=volume,source=msboost_app_data,target=/app/data,readonly --env DATA_DIR=/app/data '* ]] || fail 'original lock volume missing or writable'
      [[ " $* " == *" --entrypoint /usr/local/bin/msboost-restore sha256:$(printf '%064d' 1) backup-activity "* ]] || fail 'wrong image/tool'
      if [[ ${*: -1} == inspect-lines ]]; then
        [[ $MOCK_FAIL != inspect ]] || return 1
        local extra
        if IFS= read -r extra; then fail 'read-only inspect consumed input'; fi
        printf '%s\n' "$MOCK_REPORT"
      else
        [[ ${*: -1} == reconcile ]] || fail 'unexpected helper action'
        local frame_id frame_fingerprint frame_confirm extra
        IFS= read -r frame_id
        IFS= read -r frame_fingerprint
        IFS= read -r frame_confirm
        if IFS= read -r extra; then fail 'extra reconciliation input'; fi
        [[ $frame_id == "$TEST_ID" && $frame_fingerprint == "$TEST_FINGERPRINT" && $frame_confirm == "RECONCILE_BACKUP $TEST_ID" && -z ${extra:-} ]] || fail 'invalid stdin reconciliation frame'
        [[ $MOCK_FAIL != reconcile ]] || return 63
        printf '%s\n' synthetic-interrupted-unknown
      fi ;;
    *) fail 'recovery attempted lifecycle or unapproved Docker operation' ;;
  esac
}
assert_no_mutation() { if grep -Eq '^run .* backup-activity reconcile$' "$TRACE"; then fail 'rejected operation sent mutation'; fi; }
check_isolation() {
  if grep -Fq "$TEST_FINGERPRINT" "$TRACE" "$OUTPUT"; then fail 'fingerprint leaked to command or output'; fi
  if grep -Eq '(^| )(stop|start|restart|up|down|kill|pull|build|exec|compose)( |$)' "$TRACE"; then fail 'recovery changed service lifecycle'; fi
  [[ $(sha256sum "$INSTALL_ROOT/.env") == "$original_env" ]] || fail '.env changed'
}
: > "$TRACE"
original_env=$(sha256sum "$INSTALL_ROOT/.env")
export DOCKER_HOST=tcp://untrusted.invalid DOCKER_CONTEXT=untrusted DOCKER_DEFAULT_PLATFORM=linux/arm64
(set -x; backup_activity_reconcile_site) > "$OUTPUT" 2>&1 || fail 'valid reconciliation failed'
grep -qx synthetic-interrupted-unknown "$OUTPUT" || fail 'helper result missing'
check_isolation
for fault in tty eof image image-unavailable database database-missing database-duplicate volume server-missing server-duplicate server-bind server-volume server-image server-database server-url server-env-duplicate inspect; do
  : > "$TRACE"; MOCK_FAIL=$fault
  if backup_activity_reconcile_site > "$OUTPUT" 2>&1; then fail "accepted failed $fault"; fi
  assert_no_mutation
  check_isolation
done
MOCK_FAIL=
for malformed in invalid-version extra-line missing-line invalid-id invalid-fingerprint invalid-time inconsistent-empty forbidden-old-json; do
  : > "$TRACE"
  case "$malformed" in
    invalid-version) MOCK_REPORT=${VALID_REPORT/MSBOOST_BACKUP_ACTIVITY_INSPECT_V1/OTHER} ;;
    extra-line) MOCK_REPORT="$VALID_REPORT"$'\nextra' ;;
    missing-line) MOCK_REPORT=${VALID_REPORT%$'\n'*} ;;
    invalid-id) MOCK_REPORT=${VALID_REPORT/$TEST_ID/'$(touch invalid)'} ;;
    invalid-fingerprint) MOCK_REPORT=${VALID_REPORT/$TEST_FINGERPRINT/invalid} ;;
    invalid-time) MOCK_REPORT=${VALID_REPORT/1789400000000/-1} ;;
    inconsistent-empty) MOCK_REPORT=$(printf 'MSBOOST_BACKUP_ACTIVITY_INSPECT_V1\n0\n1\n-\n-\n0') ;;
    forbidden-old-json) MOCK_REPORT='{"pending":true,"eligible":true}' ;;
  esac
  if backup_activity_reconcile_site > "$OUTPUT" 2>&1; then fail "accepted $malformed"; fi
  assert_no_mutation
  check_isolation
done
MOCK_REPORT=$VALID_REPORT
for fault in cancel nonroot nonlinux permissions; do
  : > "$TRACE"
  (
    case "$fault" in cancel) MOCK_CONFIRM=CANCEL ;; nonroot) MOCK_UID=1000 ;; nonlinux) MOCK_SYSTEM=Windows ;; permissions) MOCK_MODE=666 ;; esac
    if backup_activity_reconcile_site > "$OUTPUT" 2>&1; then fail "accepted $fault"; fi
  ) || fail 'rejection assertion failed'
  assert_no_mutation
  check_isolation
done
: > "$TRACE"
MOCK_REPORT=$(printf 'MSBOOST_BACKUP_ACTIVITY_INSPECT_V1\n1\n0\n%s\n%s\n1789400000000' "$TEST_ID" "$TEST_FINGERPRINT")
if backup_activity_reconcile_site > "$OUTPUT" 2>&1; then fail 'ineligible live/old/mismatched marker accepted'; fi
assert_no_mutation
if grep -qx tty-confirm "$TRACE"; then fail 'ineligible evidence requested confirmation'; fi
: > "$TRACE"
MOCK_REPORT=$(printf 'MSBOOST_BACKUP_ACTIVITY_INSPECT_V1\n0\n0\n-\n-\n0')
backup_activity_reconcile_site > "$OUTPUT" 2>&1 || fail 'empty marker rejected'
assert_no_mutation
: > "$TRACE"; MOCK_REPORT=$VALID_REPORT; MOCK_FAIL=reconcile
if backup_activity_reconcile_site > "$OUTPUT" 2>&1; then fail 'lost lock/changed marker failure reported success'; fi
if grep -qx synthetic-interrupted-unknown "$OUTPUT"; then fail 'failed helper claimed success'; fi
check_isolation
MOCK_FAIL=
: > "$TRACE"
if manage_main backup-reconcile > "$OUTPUT" 2>&1; then fail 'manager exposed removed reconciliation entrypoint'; fi
check_isolation
source "$TEST_REPO/install.sh"
if bootstrap_main backup-reconcile > "$OUTPUT" 2>&1; then fail 'bootstrap exposed removed reconciliation entrypoint'; fi
check_isolation
read_tty() { printf 13; }
if menu > "$OUTPUT" 2>&1; then fail 'installed menu still accepts removed choice 13'; fi
if grep -q '13) printf backup-reconcile' "$TEST_REPO/install.sh"; then fail 'bootstrap menu still exposes choice 13'; fi
grep -q 'backup_activity_recovery.sh' "$TEST_REPO/deploy/manage.sh" || fail 'private recovery module copy integration missing'
printf '%s\n' 'PASS: local root/TTY backup reconciliation, pinned server/database/volume identity, readonly original lock mount, strict inspection/stdin proof, no service lifecycle or secret leakage'
