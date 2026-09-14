#!/usr/bin/env bash
# Offline command contracts. No real Docker, database, /opt writes or passwords.
set -Eeuo pipefail
TEST_REPO=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
mkdir -p "$TEST_REPO/.cache"
TEST_WORK=$(mktemp -d "$TEST_REPO/.cache/admin-password-test.XXXXXXXX")
trap '[[ $TEST_WORK == "$TEST_REPO/.cache/admin-password-test."* && ! -L $TEST_WORK ]] && rm -rf -- "$TEST_WORK"' EXIT
source "$TEST_REPO/deploy/manage.sh"
INSTALL_ROOT="$TEST_WORK/site"
mkdir -p "$INSTALL_ROOT"
printf '%s\n' "$MARKER" > "$INSTALL_ROOT/.managed-by-msboost"
printf 'MSBOOST_IMAGE=ghcr.io/mozziexwz/node@sha256:%064d\nMSBOOST_IMAGE_ID=sha256:%064d\nMSBOOST_DATABASE_NAME=msboost_restore_test\n' 1 1 > "$INSTALL_ROOT/.env"
TRACE="$TEST_WORK/trace"
OUTPUT="$TEST_WORK/output"
TEST_SECRET='New!synthetic$Pass12'
MOCK_EMAIL=admin@example.com
MOCK_FIRST=$TEST_SECRET
MOCK_SECOND=$TEST_SECRET
MOCK_CONFIRM='RESET admin@example.com'
MOCK_READ_FAILURE=
MOCK_TOOL_FAILURE=0
MOCK_IMAGE_FAILURE=0
MOCK_DB_STATE='msboost|database|running|healthy'
MOCK_UID=0
MOCK_SYSTEM=Linux
fail() { printf 'FAIL: %s\n' "$1" >&2; exit 1; }
id() { printf '%s' "$MOCK_UID"; }
uname() { if [[ ${1:-} == -m ]]; then printf x86_64; else printf '%s' "$MOCK_SYSTEM"; fi; }
stat() { case "$2" in %u) printf 0 ;; %a) printf 600 ;; *) fail 'unexpected stat' ;; esac; }
assert_managed() { [[ $INSTALL_ROOT == "$TEST_WORK/site" ]]; }
server_identity() { [[ $MOCK_IMAGE_FAILURE == 0 ]] && printf 'sha256:%064d' 1; }
admin_password_open_tty() { exec {tty_fd}</dev/null; }
admin_password_read() {
  [[ $MOCK_READ_FAILURE != "$2" ]] || return 1
  case "$2" in
    admin_email_input) printf -v "$2" '%s' "$MOCK_EMAIL" ;;
    admin_password_first) printf -v "$2" '%s' "$MOCK_FIRST" ;;
    admin_password_second) printf -v "$2" '%s' "$MOCK_SECOND" ;;
    admin_confirm_input) printf -v "$2" '%s' "$MOCK_CONFIRM" ;;
    *) fail 'unexpected terminal field' ;;
  esac
}
docker() {
  # Arguments must never contain either entered password, including failures.
  [[ "$*" != *"$TEST_SECRET"* ]] || fail 'password leaked into process arguments'
  printf '%s\n' "$*" >> "$TRACE"
  case "$1" in
    ps) printf '%064d' 4 ;;
    inspect) printf '%s' "$MOCK_DB_STATE" ;;
    run)
      [[ " $* " == *' --rm -i --pull never --read-only --user 0:0 '* ]] || fail 'tool privilege or download policy changed'
      [[ " $* " == *' --cap-drop ALL --security-opt no-new-privileges:true --log-driver none '* ]] || fail 'tool isolation missing'
      [[ " $* " == *' --ulimit core=0 '* ]] || fail 'tool core dumps not disabled'
      [[ " $* " == *' --env DATABASE_URL= --env DATABASE_HOST=127.0.0.1 --env DATABASE_PORT=5432 '* ]] || fail 'database connection could be redirected'
      [[ " $* " == *' --env DATABASE_NAME=msboost_restore_test '* ]] || fail 'restored database selection lost'
      [[ " $* " == *" --network container:$(printf '%064d' 4) "* ]] || fail 'not confined to existing local database'
      [[ " $* " == *" --entrypoint /usr/local/bin/msboost-restore sha256:$(printf '%064d' 1) admin-password " ]] || fail 'untrusted tool/image chosen'
      local email first second confirm extra
      IFS= read -r email
      IFS= read -r first
      IFS= read -r second
      IFS= read -r confirm
      if IFS= read -r extra; then fail 'extra secret frame data'; fi
      [[ $email == "$MOCK_EMAIL" && $first == "$TEST_SECRET" && $second == "$TEST_SECRET" && $confirm == "$MOCK_CONFIRM" ]] || fail 'invalid stdin frame'
      [[ $MOCK_TOOL_FAILURE == 0 ]] || return 63
      printf '%s\n' 'synthetic-admin-updated' ;;
    *) fail 'administrator command attempted service lifecycle mutation' ;;
  esac
}
check_no_leak() {
  if grep -Fq "$TEST_SECRET" "$TRACE" "$OUTPUT" "$INSTALL_ROOT/.env"; then fail 'plaintext synthetic password leaked'; fi
  if grep -Eq '(^| )(stop|start|restart|up|down|kill|rm|pull|build|exec)( |$)' "$TRACE"; then fail 'existing service lifecycle changed'; fi
}
: > "$TRACE"
original_env=$(sha256sum "$INSTALL_ROOT/.env")
# Xtrace must switch off BEFORE reading/expanding any new secret.
(set -x; admin_password_site) > "$OUTPUT" 2>&1 || fail 'valid administrator flow failed'
grep -qx synthetic-admin-updated "$OUTPUT" || fail 'helper success not reported'
[[ $(sha256sum "$INSTALL_ROOT/.env") == "$original_env" ]] || fail '.env rewritten'
check_no_leak
for field in admin_email_input admin_password_first admin_password_second admin_confirm_input; do
  : > "$TRACE"
  MOCK_READ_FAILURE=$field
  if admin_password_site > "$OUTPUT" 2>&1; then fail 'EOF accepted'; fi
  if grep -q '^run ' "$TRACE"; then fail 'EOF sent a mutation'; fi
  check_no_leak
done
MOCK_READ_FAILURE=
for reason in cancel mismatch short long invalid_email image unhealthy nonroot nonlinux; do
  : > "$TRACE"
  (
    case "$reason" in
      cancel) MOCK_CONFIRM=CANCEL ;;
      mismatch) MOCK_SECOND=different-password ;;
      short) MOCK_FIRST=short; MOCK_SECOND=short ;;
      long) MOCK_FIRST=$(printf '%073d' 1); MOCK_SECOND=$MOCK_FIRST ;;
      invalid_email) MOCK_EMAIL=$'bad\033@example.com' ;;
      image) MOCK_IMAGE_FAILURE=1 ;;
      unhealthy) MOCK_DB_STATE='msboost|database|running|unhealthy' ;;
      nonroot) MOCK_UID=1000 ;;
      nonlinux) MOCK_SYSTEM=Windows ;;
    esac
    if admin_password_site; then fail 'rejected administrator flow succeeded'; fi
  ) > "$OUTPUT" 2>&1 || fail 'rejection assertion failed'
  if grep -q '^run ' "$TRACE"; then fail 'rejected input started helper'; fi
  check_no_leak
done
: > "$TRACE"
MOCK_TOOL_FAILURE=1
if admin_password_site > "$OUTPUT" 2>&1; then fail 'tool failure reported success'; fi
check_no_leak
MOCK_TOOL_FAILURE=0
# Reject accidental CLI secrets before a generic unknown-argument error can
# reflect them. Source bootstrap too; these guards must run before root paths.
if manage_main admin-password --password "$TEST_SECRET" > "$OUTPUT" 2>&1; then fail 'manager accepted password argument'; fi
check_no_leak
source "$TEST_REPO/install.sh"
if bootstrap_main admin-password --password "$TEST_SECRET" > "$OUTPUT" 2>&1; then fail 'bootstrap accepted password argument'; fi
check_no_leak
read_tty() { printf 12; }
[[ $(menu) == admin-password ]] || fail 'installed menu item missing'
grep -q '12) printf admin-password' "$TEST_REPO/install.sh" || fail 'release bootstrap menu item missing'
printf '%s\n' 'PASS: root-only administrator stdin recovery, cancellation/EOF, immutable image, no service restart, no CLI/log/.env secret leakage'
