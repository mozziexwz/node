#!/usr/bin/env bash
# Production online path contracts. No real Docker, network, /opt or systemd.
set -Eeuo pipefail
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
WORK=$(mktemp -d "${TMPDIR:-/tmp}/msboost-online-test.XXXXXXXX")
trap '[[ -d $WORK && ! -L $WORK ]] && command rm -rf -- "$WORK"' EXIT
source "$ROOT/deploy/disaster.sh"
INSTALL_ROOT="$WORK/site"; PROJECT=msboost; mkdir -p "$INSTALL_ROOT/deploy" "$WORK/backups"
printf test > "$INSTALL_ROOT/.env"; touch "$INSTALL_ROOT/install.sh" "$INSTALL_ROOT/.managed-by-msboost"
TRACE="$WORK/trace"; : > "$TRACE"
trace() { printf '%s\n' "$*" >> "$TRACE"; }
die() { printf '%s\n' "$*" >&2; return 1; }
note() { :; }
assert_managed() { :; }
disaster_validate_environment() { :; }
disaster_tool() { DISASTER_TOOL=tool; }
disaster_assert_volume() { :; }
disaster_settings() { case "$1" in localDir) printf '%s' "$WORK/backups";; hasRemote) printf true;; *) return 1;; esac; }
env_get() { [[ $2 == MASTER_KEY ]] && printf private-fixture-key; }
random_hex() { printf 0123456789abcdef; }
mktemp() { command mktemp -d "$WORK/staging.XXXXXXXX"; }
compose_live() {
  trace "compose $*"
  case "$1" in ps) printf 'database\nserver\ncaddy\n';;
    run) [[ "$*" == *' server export-online '* && "$*" != *backup-pause* ]] || return 90; [[ ${FAULT:-} != export ]] ;;
    *) die 'online backup tried to change services';;
  esac
}
tool() {
  [[ $1 == disaster ]] || return 1; shift
  trace "tool $*"
  case "$1" in prepare-dir|record|schedule-record) :;;
    pack-online) while [[ $# -gt 0 ]]; do if [[ $1 == --output ]]; then printf verified > "$2"; break; fi; shift; done;;
    upload) [[ ${FAULT:-} != upload ]];;
    retain) [[ ${FAULT:-} != retention ]];;
    config-save) [[ "$*" == *--verify-remote* ]] || return 91; return 1;;
    *) return 92;;
  esac
}
disaster_backup
[[ -z $DISASTER_RUN_ID ]] && grep -q 'pack-online' "$TRACE" && grep -q 'stage remote' "$TRACE"
! grep -q 'backup-pause\|compose stop\|compose start\|pg_dump' "$TRACE"
FAULT=upload
if disaster_backup; then die 'upload failure reported success'; exit 1; fi
[[ $DISASTER_FAILURE_STAGE == remote_failed && -n $DISASTER_RUN_ID ]]
FAULT=export
if disaster_backup; then die 'export failure reported success'; exit 1; fi
! grep -q 'compose stop\|compose start' "$TRACE"
# Configuration validation failure must not install/disable a timer or run a
# backup, even when the user requested enabling it. Local-only validation here
# avoids reading a password from any test terminal.
read_tty() { case "$1" in '启用每日'*) printf YES;; *) printf '';; esac; }
disaster_timer() { die 'validation failure changed timer'; exit 1; }
if disaster_configure; then die 'failed validation reported enabled'; exit 1; fi
printf '%s\n' 'PASS: online backup never stops services, separates upload failure, validates configuration before touching timer'
