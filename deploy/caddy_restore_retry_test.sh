#!/usr/bin/env bash
set -Eeuo pipefail
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
source "$ROOT/deploy/manage.sh"
WORK=$(mktemp -d "${TMPDIR:-/tmp}/msboost-import-test.XXXXXXXX")
trap '[[ -d $WORK && ! -L $WORK ]] && command rm -rf -- "$WORK"' EXIT
CADDY_ROOT="$WORK/caddy"; CADDY_CUSTOM="$CADDY_ROOT/msboost-custom"
mkdir -p "$CADDY_CUSTOM" "$WORK/archive/msboost-custom"
printf '%s\n' "$CADDY_OWNER" > "$WORK/archive/msboost-custom/.msboost-owner"
printf 'test.example { respond "test" }\n' > "$WORK/archive/msboost-custom/test.caddy"
operation=$(printf same-archive | sha256sum | cut -d' ' -f1)
caddy_ensure() { :; }
caddy_safe_path() { [[ $1 == "$WORK"/* && ! -L $1 ]]; }
install() { [[ $1 == -o && $2 == root && $3 == -g && $4 == caddy && $5 == -m && $6 == 0640 ]] || return 99; cp -- "$7" "$8"; }
caddy_import "$WORK/archive" "$operation"
cp "$CADDY_CUSTOM/test.caddy" "$WORK/before"
caddy_import "$WORK/archive" "$operation"
cmp "$WORK/before" "$CADDY_CUSTOM/test.caddy"
different=$(printf different-archive | sha256sum | cut -d' ' -f1)
if caddy_import "$WORK/archive" "$different"; then exit 1; fi
printf 'admin modification\n' >> "$CADDY_CUSTOM/test.caddy"
cp "$CADDY_CUSTOM/test.caddy" "$WORK/modified"
if caddy_import "$WORK/archive" "$operation"; then exit 1; fi
cmp "$WORK/modified" "$CADDY_CUSTOM/test.caddy"
cp "$WORK/before" "$CADDY_CUSTOM/test.caddy"
caddy_import_commit "$operation"
[[ ! -e $CADDY_ROOT/.msboost-restore-import ]]
if caddy_import "$WORK/archive" "$operation"; then exit 1; fi
rm -- "$CADDY_CUSTOM/test.caddy"
printf 'second.example { respond "second" }\n' > "$WORK/archive/msboost-custom/second.caddy"
install_count=0
install() {
  install_count=$((install_count+1))
  [[ $install_count -lt 2 ]] || return 1
  cp -- "$7" "$8"
}
if caddy_import "$WORK/archive" "$operation"; then exit 1; fi
[[ ! -e $CADDY_CUSTOM/test.caddy && ! -e $CADDY_CUSTOM/second.caddy && ! -e $CADDY_ROOT/.msboost-restore-import ]]
printf 'PASS: interrupted owned import resumes; different operation, manual edits and unowned identical files are preserved\n'
