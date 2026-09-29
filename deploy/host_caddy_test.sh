#!/usr/bin/env bash
# Offline publication transaction tests. No actual /etc, service, package or network.
set -Eeuo pipefail
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
source "$ROOT/deploy/manage.sh"
WORK=$(mktemp -d "${TMPDIR:-/tmp}/msboost-caddy-test.XXXXXXXX")
trap '[[ -d $WORK && ! -L $WORK ]] && command rm -rf -- "$WORK"' EXIT
INSTALL_ROOT="$WORK/install"; CADDY_ROOT="$WORK/caddy"; CADDY_SITE="$CADDY_ROOT/sites-enabled/msboost.caddy"
mkdir -p "$INSTALL_ROOT/deploy" "$CADDY_ROOT/sites-enabled"
cp "$ROOT/deploy/Caddyfile" "$INSTALL_ROOT/deploy/Caddyfile"
printf 'MSBOOST_SITE_ADDRESS=panel.example.com\n' > "$INSTALL_ROOT/.env"
printf 'import sites-enabled/*.caddy\n' > "$CADDY_ROOT/Caddyfile"
printf 'other untouched\n' > "$CADDY_ROOT/sites-enabled/other.caddy"
cp "$CADDY_ROOT/sites-enabled/other.caddy" "$WORK/other.before"
TRACE="$WORK/trace"; : > "$TRACE"
FAIL_VALIDATE=0; FAIL_RELOAD=0
caddy_safe_path() { [[ $1 == "$WORK"/* && ! -L $1 ]]; }
caddy_assert_service() { :; }
caddy_validate() { printf 'validate\n' >> "$TRACE"; [[ $FAIL_VALIDATE == 0 ]]; }
systemctl() {
  printf 'systemctl %s\n' "$*" >> "$TRACE"
  case "$1" in is-active) return 0;; reload) [[ $FAIL_RELOAD == 0 ]];; *) return 99;; esac
}
if [[ $(uname -s) == MINGW* || $(uname -s) == MSYS* ]]; then
  chmod() { :; }
  install() { [[ $1 == -d && $2 == -m ]]; shift 3; mkdir -p "$@"; }
fi
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
caddy_publish enable
grep -qx 'systemctl reload caddy.service' "$TRACE" || fail 'missing initial reload'
cp "$CADDY_SITE" "$WORK/site.before"
: > "$TRACE"
caddy_publish enable
! grep -q 'reload' "$TRACE" || fail 'unchanged upgrade reloaded shared Caddy'
printf '\n# changed template\n' >> "$INSTALL_ROOT/deploy/Caddyfile"
FAIL_VALIDATE=1
: > "$TRACE"
if caddy_publish enable; then fail 'invalid candidate accepted'; fi
cmp "$CADDY_SITE" "$WORK/site.before"
! grep -q 'reload' "$TRACE" || fail 'invalid candidate reloaded running service'
FAIL_VALIDATE=0; FAIL_RELOAD=1
if caddy_publish enable; then fail 'failed reload accepted'; fi
cmp "$CADDY_SITE" "$WORK/site.before"
FAIL_RELOAD=0
caddy_publish enable
cmp "$CADDY_ROOT/sites-enabled/other.caddy" "$WORK/other.before"
printf '\n# manual change\n' >> "$CADDY_SITE"
cp "$CADDY_SITE" "$WORK/manual.before"
if caddy_publish enable; then fail 'manual changes overwritten'; fi
cmp "$CADDY_SITE" "$WORK/manual.before"
sha256sum "$CADDY_SITE" | cut -d' ' -f1 > "$INSTALL_ROOT/proxy/managed.sha256"
caddy_publish disable
[[ ! -e $CADDY_SITE && -f $INSTALL_ROOT/.env ]]
cmp "$CADDY_ROOT/sites-enabled/other.caddy" "$WORK/other.before"
! grep -Eq 'systemctl (stop|restart|disable)' "$TRACE" || fail 'shared service stopped'
printf '%s\n' 'PASS: unchanged proxy not reloaded, invalid/reload failure rolls back only own site, manual edits preserved, uninstall leaves other sites and shared Caddy running'
