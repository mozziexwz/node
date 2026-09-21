#!/usr/bin/env bash
# Pure function/ordering checks. Never calls a real systemd unit or host state.
set -Eeuo pipefail
ROOT=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
mkdir -p "$ROOT/.cache"
WORK=$(mktemp -d "$ROOT/.cache/dynamic-user-mock.XXXXXXXX")
trap 'rm -rf -- "$WORK"' EXIT
SOURCE="$ROOT/deploy/relay_dynamic_user_validation.sh"
for function_name in dynamic_unit_load dynamic_unit_owned dynamic_cleanup; do
  awk -v name="$function_name" '$0 == name "() {" {copy=1} copy {print} copy && /^}$/ {exit}' "$SOURCE" > "$WORK/$function_name.sh"
  source "$WORK/$function_name.sh"
done
NAME=msboost-dyn-123456789abc UNIT=msboost-dyn-123456789abc.service STAGE=/usr/local/libexec/msboost-dynamic-user.abcdefgh
systemctl() {
  if [[ $MODE == cleanup-loaded && $1 == stop && $2 == "$UNIT" ]]; then printf 'stop\n' >> "$WORK/lifecycle"; return 0; fi
  if [[ $MODE == cleanup-loaded && $1 == reset-failed && $2 == "$UNIT" ]]; then printf 'reset-failed\n' >> "$WORK/lifecycle"; printf complete > "$WORK/reset-done"; return 0; fi
  [[ $1 == show && $2 == "$UNIT" && $3 == -p && $5 == --value ]] || return 2
  case "$4" in
    LoadState) case "$MODE" in unknown-load) printf 'error\n'; return 1 ;; not-found) printf 'not-found\n'; return 1 ;; cleanup-loaded) if [[ -e $WORK/reset-done ]]; then printf 'not-found\n'; else printf 'loaded\n'; fi ;; *) printf 'loaded\n' ;; esac ;;
    Description) if [[ $MODE == foreign-description ]]; then printf 'other unit\n'; else printf 'MSBOOST isolated DynamicUser validation %s\n' "$NAME"; fi ;;
    StateDirectory) if [[ $MODE == foreign-state ]]; then printf 'msboost-relay\n'; else printf '%s\n' "$NAME"; fi ;;
    DynamicUser) if [[ $MODE == static-user ]]; then printf 'no\n'; else printf 'yes\n'; fi ;;
    ExecStart) if [[ $MODE == foreign-binary ]]; then printf '{ path=/usr/bin/other ; argv[]=other ; }\n'; else printf '{ path=%s/relayruntime.test ; argv[]=%s/relayruntime.test -test.run=^TestDynamicUserServiceChild$ -test.v -test.timeout=4m ; }\n' "$STAGE" "$STAGE"; fi ;;
    MainPID) printf '0\n' ;;
    ActiveState) printf 'failed\n' ;;
    *) return 2 ;;
  esac
}
[[ $(MODE=valid dynamic_unit_load) == loaded ]]
[[ $(MODE=not-found dynamic_unit_load) == not-found ]]
if MODE=unknown-load dynamic_unit_load; then exit 1; fi
MODE=valid dynamic_unit_owned
for mode in foreign-description foreign-state static-user foreign-binary; do
  if MODE=$mode dynamic_unit_owned; then printf 'accepted %s\n' "$mode" >&2; exit 1; fi
done
# Cleanup must not reach mutation when target identities are unavailable, the
# daemon's load result is unknown, or a same-name unit is not exactly ours.
INTENT=1 REPORTS="$WORK"
dynamic_validate_paths() { [[ $MODE != bad-path ]]; }
systemctl_mutations=0
journalctl() { printf 'journal reached\n' >> "$WORK/mutations"; }
for mode in bad-path unknown-load foreign-description foreign-state static-user foreign-binary; do
  if MODE=$mode dynamic_cleanup; then printf 'unsafe cleanup accepted %s\n' "$mode" >&2; exit 1; fi
done
[[ ! -e $WORK/mutations ]]
(
  # These exact paths do not exist. All destructive commands and stat are
  # overridden, so the positive cleanup simulation cannot touch host assets.
  PUBLIC=/var/lib/$NAME PRIVATE=/var/lib/private/$NAME STATE_ID= STAGE_ID=0:0:755:1:2
  stat() { [[ $* == "-c %u:%g:%a:%d:%i $STAGE" ]] || return 2; printf '%s\n' "$STAGE_ID"; }
  rm() { [[ $* == "-rf -- $STAGE" ]] || return 2; printf 'stage-remove\n' >> "$WORK/lifecycle"; }
  MODE=cleanup-loaded dynamic_cleanup
)
[[ $(cat "$WORK/lifecycle") == $'stop\nreset-failed\nstage-remove' ]]
grep -Fq -- '--property=PrivateNetwork=true' "$SOURCE"
grep -Fq -- '--property=DynamicUser=true' "$SOURCE"
grep -Fq -- '--property=StateDirectoryMode=0700' "$SOURCE"
grep -Fq -- 'MSBOOST_DYNAMIC_USER_VALIDATION=host' "$SOURCE"
grep -Fq -- '"$STAGE/msboost-agent" --capability relay-recovery' "$SOURCE"
grep -Fq -- 'systemctl restart "$UNIT"' "$SOURCE"
grep -Fq -- 'STAGE=$(mktemp -d /usr/local/libexec/msboost-dynamic-user.XXXXXXXX)' "$SOURCE"
! grep -Fq -- 'mktemp -d /run/msboost-dynamic-user.' "$SOURCE"
grep -Fq -- 'ASSET_BYTES + 16777216 + 1023' "$SOURCE"
space_line=$(grep -n '^FREE_KIB=' "$SOURCE" | cut -d: -f1)
stage_line=$(grep -n '^STAGE=$(mktemp ' "$SOURCE" | cut -d: -f1)
[[ $space_line -lt $stage_line ]]
! grep -Eq 'systemctl (stop|restart) msboost-relay|StateDirectory=msboost-relay|/var/lib/private/msboost-relay' "$SOURCE"
bash -n "$SOURCE"
printf 'dynamic user validation guards: PASS (no real systemd/network/host state)\n'
