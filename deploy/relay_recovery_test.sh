#!/usr/bin/env bash
# Offline private-file/TTY/Docker transport contracts. No real service/network.
set -Eeuo pipefail
TEST_REPO=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
mkdir -p "$TEST_REPO/.cache"
TEST_WORK=$(mktemp -d "$TEST_REPO/.cache/relay-recovery-test.XXXXXXXX")
trap '[[ $TEST_WORK == "$TEST_REPO/.cache/relay-recovery-test."* && ! -L $TEST_WORK && $(realpath -m "$TEST_WORK") == "$TEST_WORK" ]] && command rm -rf -- "$TEST_WORK"' EXIT
source "$TEST_REPO/deploy/manage.sh"
source "$TEST_REPO/deploy/backup_activity_recovery.sh"
source "$TEST_REPO/deploy/relay_recovery.sh"
TEST_REQUEST_SECRET=synthetic-private-request-token
TEST_RESULT_SECRET=synthetic-private-plan-token
TEST_ERROR_SECRET=synthetic-private-helper-error
TEST_ENV_SECRET=$(printf '%064d' 8)
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
trace() { printf '%s\n' "$*" >> "$TRACE"; }
id() { [[ $MOCK_FAIL != nonroot ]] && printf 0 || printf 1000; }
uname() { if [[ ${1:-} == -m ]]; then printf x86_64; elif [[ $MOCK_FAIL == nonlinux ]]; then printf Windows; else printf Linux; fi; }
assert_managed() { [[ $INSTALL_ROOT == "$CASE_ROOT/site" && -f $INSTALL_ROOT/.env ]]; }
stat() {
  local format=$2 path=${*: -1}
  case "$format" in
    %u) if [[ $MOCK_FAIL == parent-owner && $path == "$CASE_ROOT/private" ]]; then printf 1000; else printf 0; fi ;;
    %a) if [[ $MOCK_FAIL == config-write && $path == "$INSTALL_ROOT/.env" ]]; then printf 666; else printf 700; fi ;;
    %u:%a) if [[ $MOCK_FAIL == output-mode && $path == "$CASE_ROOT/private" ]]; then printf 0:755; else printf 0:700; fi ;;
    %u:%a:%h)
      if [[ $path == "$MOCK_INPUT" && $MOCK_FAIL == input-mode ]]; then printf 0:644:1
      elif [[ $path == "$MOCK_INPUT" && $MOCK_FAIL == input-hardlink ]]; then printf 0:600:2
      else printf 0:600:1; fi ;;
    %s) command stat -c %s -- "$path" ;;
    %d:%i)
      if [[ $path == /proc/*/fd/* ]]; then command stat -c '%d:%i' -- "$MOCK_INPUT"
      else command stat -c '%d:%i' -- "$path"; fi ;;
    *) fail 'unexpected file metadata query' ;;
  esac
}
sync() {
  [[ $# == 2 && $1 == -f && $2 == "$CASE_ROOT/private"* ]] || fail 'unexpected durability target'
  MOCK_SYNC_COUNT=$((MOCK_SYNC_COUNT+1)); trace "sync $MOCK_SYNC_COUNT"
  [[ $MOCK_SYNC_COUNT != "$MOCK_SYNC_FAIL" ]]
}
ln() {
  [[ $1 == -T && $2 == -- && $3 == "$CASE_ROOT/private"/.msboost-relay-recovery.*/result.partial && $4 == "$MOCK_OUTPUT" ]] || fail 'unsafe publication arguments'
  if [[ $MOCK_FAIL == publish-race ]]; then printf other-root-file > "$MOCK_OUTPUT"; fi
  command ln "$@"
}
relay_recovery_open_tty() { [[ $MOCK_FAIL != tty ]] || return 1; exec {recovery_tty_fd}</dev/null; }
relay_recovery_read() {
  [[ $MOCK_FAIL != "eof-$1" ]] || return 1
  case "$1" in
    recovery_action) recovery_action=$MOCK_ACTION ;;
    recovery_input) recovery_input=$MOCK_INPUT ;;
    recovery_output) recovery_output=$MOCK_OUTPUT ;;
    recovery_confirmation)
      if [[ $MOCK_FAIL == cancel ]]; then recovery_confirmation=CANCEL; else recovery_confirmation="RELAY_RECOVERY $MOCK_ACTION"; fi ;;
    *) fail 'unexpected terminal input' ;;
  esac
}
docker() {
  [[ $1 == --host && $2 == unix:///var/run/docker.sock && -z ${DOCKER_HOST+x} && -z ${DOCKER_CONTEXT+x} ]] || fail 'remote Docker context accepted'
  shift 2
  [[ "$*" != *"$TEST_REQUEST_SECRET"* && "$*" != *"$TEST_RESULT_SECRET"* && "$*" != *"$TEST_ERROR_SECRET"* && "$*" != *"$TEST_ENV_SECRET"* ]] || fail 'private JSON/key in command argv'
  trace "$*"
  case "$1" in
    image)
      if [[ $MOCK_FAIL == image ]]; then printf 'linux|amd64|sha256:%064d|https://other.invalid\n' 1
      else printf 'linux|amd64|sha256:%064d|https://github.com/mozziexwz/node\n' 1; fi ;;
    ps)
      if [[ " $* " == *' label=com.docker.compose.service=server '* ]]; then
        [[ $MOCK_FAIL != server-missing ]] || return 0
        printf '%064d' 5
        [[ $MOCK_FAIL != server-duplicate ]] || printf '\n%064d' 6
      else printf '%064d' 4; fi ;;
    inspect)
      if [[ ${*: -1} == "$(printf '%064d' 4)" ]]; then
        if [[ $MOCK_FAIL == database ]]; then printf 'msboost|database|running|unhealthy'; else printf 'msboost|database|running|healthy'; fi
      elif [[ "$*" == *'$comma := false'* ]]; then
        case $MOCK_FAIL in
          master-key) printf '["MASTER_KEY=%064d","PUBLIC_URL=https://panel.example.test"]' 9 ;;
          public-url) printf '["MASTER_KEY=%s","PUBLIC_URL=https://wrong.example.test"]' "$TEST_ENV_SECRET" ;;
          duplicate-env) printf '["MASTER_KEY=%s","MASTER_KEY=%s"]' "$TEST_ENV_SECRET" "$TEST_ENV_SECRET" ;;
          *) printf '["MASTER_KEY=%s","PUBLIC_URL=https://panel.example.test"]' "$TEST_ENV_SECRET" ;;
        esac
        [[ $MOCK_FAIL != env-inspect-failure ]] || return 29
      elif [[ "$*" == *'{{and'* ]]; then
        if [[ $MOCK_FAIL == server-env ]]; then printf false; else printf true; fi
      else
        if [[ $MOCK_FAIL == server-image ]]; then printf 'sha256:%064d|msboost|server' 2; else printf 'sha256:%064d|msboost|server' 1; fi
      fi ;;
    run)
      [[ " $* " == *' --rm -i --pull never --read-only --user 0:0 --cap-drop ALL --security-opt no-new-privileges:true --log-driver none --ulimit core=0 '* ]] || fail 'helper isolation missing'
      [[ " $* " != *' --mount '* && " $* " != *' --cap-add '* ]] || fail 'unneeded data mount/privilege requested'
      if [[ ${*: -1} == verify-environment ]]; then
        [[ " $* " == *" --network none --env-file $INSTALL_ROOT/.env --entrypoint /usr/local/bin/msboost-restore sha256:$(printf '%064d' 1) relay-recovery verify-environment "* ]] || fail 'environment verifier missing isolation or pinned image'
        local actual_environment
        actual_environment=$(cat)
        [[ $actual_environment == "[\"MASTER_KEY=$TEST_ENV_SECRET\",\"PUBLIC_URL=https://panel.example.test\"]" && $MOCK_FAIL != env-helper-failure ]] || return 31
        if [[ $MOCK_FAIL == env-extra-output ]]; then printf 'true\nextra\n'; else printf 'true\n'; fi
        return 0
      fi
      [[ " $* " == *" --network container:$(printf '%064d' 4) --env-file $INSTALL_ROOT/.env "* ]] || fail 'wrong local database namespace/config'
      [[ " $* " == *' --env DATABASE_URL= --env DATABASE_HOST=127.0.0.1 --env DATABASE_PORT=5432 --env DATABASE_USER=msboost --env DATABASE_NAME=msboost_restore_test --env DATABASE_SSLMODE=disable '* ]] || fail 'database could be redirected'
      [[ " $* " == *" --entrypoint /usr/local/bin/msboost-restore sha256:$(printf '%064d' 1) relay-recovery $MOCK_ACTION "* ]] || fail 'wrong pinned image or helper action'
      local frame
      frame=$(cat)
      if [[ $MOCK_ACTION == status ]]; then [[ -z $frame ]] || fail 'status read a request'
      else [[ $frame == "{\"request\":\"$TEST_REQUEST_SECRET\"}" ]] || fail 'request stdin content changed'; fi
      printf '%s\n' "$TEST_ERROR_SECRET" >&2
      if [[ $MOCK_FAIL != empty ]]; then printf '{"token":"%s"}\n' "$TEST_RESULT_SECRET"; fi
      if [[ $MOCK_FAIL == output-race ]]; then printf other-root-file > "$MOCK_OUTPUT"; fi
      [[ $MOCK_FAIL != helper ]] || return 53 ;;
    *) fail 'recovery attempted a service/network lifecycle operation' ;;
  esac
}
fixture() {
  CASE_ROOT="$TEST_WORK/$1"; mkdir -p "$CASE_ROOT/site" "$CASE_ROOT/private"
  INSTALL_ROOT="$CASE_ROOT/site"; TRACE="$CASE_ROOT/trace"; OUTPUT="$CASE_ROOT/console"; : > "$TRACE"
  printf '%s\n' "$MARKER" > "$INSTALL_ROOT/.managed-by-msboost"
  printf 'MSBOOST_IMAGE=ghcr.io/mozziexwz/node@sha256:%064d\nMSBOOST_IMAGE_ID=sha256:%064d\nMASTER_KEY=%064d\nMSBOOST_DATABASE_NAME=msboost_restore_test\nPUBLIC_URL=https://panel.example.test\n' 1 1 8 > "$INSTALL_ROOT/.env"
  MOCK_INPUT="$CASE_ROOT/private/request.json"; MOCK_OUTPUT="$CASE_ROOT/private/result.json"
  printf '{"request":"%s"}\n' "$TEST_REQUEST_SECRET" > "$MOCK_INPUT"
  MOCK_ACTION=prepare MOCK_FAIL= MOCK_SYNC_FAIL=0 MOCK_SYNC_COUNT=0
}
assert_private_console() {
  if grep -Fq -e "$TEST_REQUEST_SECRET" -e "$TEST_RESULT_SECRET" -e "$TEST_ERROR_SECRET" -e "$TEST_ENV_SECRET" "$OUTPUT" "$TRACE"; then fail 'private helper data leaked to console or command trace'; fi
  if grep -Eq '(^| )(stop|start|restart|up|down|kill|pull|build|exec|compose)( |$)' "$TRACE"; then fail 'existing service lifecycle changed'; fi
}
assert_no_request() { if grep '^run ' "$TRACE" | grep -qv ' relay-recovery verify-environment$'; then fail 'rejected request reached recovery helper'; fi; }
export DOCKER_HOST=tcp://untrusted.invalid DOCKER_CONTEXT=untrusted
for action in inspect prepare status finalize tls-inspect tls-rotate; do
  fixture "success-$action"; MOCK_ACTION=$action
  (set -x; relay_recovery_site) > "$OUTPUT" 2>&1 || fail "valid $action failed"
  [[ -f $MOCK_OUTPUT && $(<"$MOCK_OUTPUT") == "{\"token\":\"$TEST_RESULT_SECRET\"}" ]] || fail 'result not privately published'
  [[ -z $(find "$CASE_ROOT/private" -maxdepth 1 -type d -name '.msboost-relay-recovery.*') ]] || fail 'successful publication left temporary data'
  assert_private_console
done
for fault in tty eof-recovery_action eof-recovery_input eof-recovery_output eof-recovery_confirmation cancel nonroot nonlinux config-write image database server-missing server-duplicate server-image server-env master-key public-url duplicate-env env-inspect-failure env-helper-failure env-extra-output input-mode input-hardlink output-mode parent-owner; do
  fixture "$fault"; MOCK_FAIL=$fault
  if relay_recovery_site > "$OUTPUT" 2>&1; then fail "accepted $fault"; fi
  assert_no_request; assert_private_console
  [[ ! -e $MOCK_OUTPUT ]] || fail 'rejected request created output'
done
for fault in bad-action existing-output input-empty input-relative output-relative systemd; do
  fixture "$fault"
  (
    case "$fault" in
      bad-action) MOCK_ACTION=unknown ;;
      existing-output) printf preserved > "$MOCK_OUTPUT" ;;
      input-empty) : > "$MOCK_INPUT" ;;
      input-relative) MOCK_INPUT=relative.json ;;
      output-relative) MOCK_OUTPUT=relative.json ;;
      systemd) INVOCATION_ID=synthetic-service ;;
    esac
    if relay_recovery_site > "$OUTPUT" 2>&1; then fail "accepted $fault"; fi
  ) || fail 'rejection assertion failed'
  assert_no_request; assert_private_console
done
for fault in helper empty output-race publish-race flush-before flush-after; do
  fixture "$fault"; MOCK_FAIL=$fault
  [[ $fault != flush-before ]] || MOCK_SYNC_FAIL=1
  [[ $fault != flush-after ]] || MOCK_SYNC_FAIL=2
  if relay_recovery_site > "$OUTPUT" 2>&1; then fail "reported success after $fault"; fi
  assert_private_console
  [[ -n $(find "$CASE_ROOT/private" -maxdepth 1 -type d -name '.msboost-relay-recovery.*') ]] || fail 'failure lost private result/diagnostic directory'
  if [[ $fault == output-race || $fault == publish-race ]]; then [[ $(<"$MOCK_OUTPUT") == other-root-file ]] || fail 'publication overwrote existing file'; fi
  [[ $fault != flush-before ]] || assert_no_request
done
for fault in input-symlink output-symlink parent-symlink; do
  fixture "$fault"
  case "$fault" in
    input-symlink)
      link="$CASE_ROOT/private/input-link.json"
      command ln -s "$MOCK_INPUT" "$link" 2>/dev/null || true
      MOCK_INPUT=$link ;;
    output-symlink)
      link=$MOCK_OUTPUT
      command ln -s "$CASE_ROOT/private/missing.json" "$link" 2>/dev/null || true ;;
    parent-symlink)
      link="$CASE_ROOT/linked-parent"
      command ln -s "$CASE_ROOT/private" "$link" 2>/dev/null || true
      MOCK_OUTPUT="$link/result.json" ;;
  esac
  if [[ -L $link ]]; then
    if relay_recovery_site > "$OUTPUT" 2>&1; then fail "accepted $fault"; fi
    assert_no_request; assert_private_console
  else printf 'SKIP: %s requires Linux symlink support\n' "$fault"; fi
done
fixture arguments
if manage_main relay-recovery --json "$TEST_REQUEST_SECRET" > "$OUTPUT" 2>&1; then fail 'manager accepted JSON argv'; fi
assert_private_console
source "$TEST_REPO/install.sh"
if bootstrap_main relay-recovery --json "$TEST_REQUEST_SECRET" > "$OUTPUT" 2>&1; then fail 'bootstrap accepted JSON argv'; fi
assert_private_console
read_tty() { printf 15; }
[[ $(menu) == relay-recovery ]] || fail 'installed menu15 missing'
grep -q '15) printf relay-recovery' "$TEST_REPO/install.sh" || fail 'bootstrap menu15 missing'
printf '%s\n' 'PASS: private relay recovery file transport, pinned local DB/server, no-overwrite publication, fsync failures, TTY/no-args guards, no secrets in console or Docker logs'
