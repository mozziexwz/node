#!/usr/bin/env bash
# Test only. Real random DynamicUser unit, production Run, and real agent CLI.
# No installer, existing Agent, public network, or fixed msboost state is touched.
# Usage: MSBOOST_DYNAMIC_USER_VALIDATION=1 bash this-script PRIVATE_FINAL_ASSETS
# Bundle: assets.sha256, bin/relayruntime.test, bin/msboost-agent, this script.
# Compile relayruntime.test with GOOS=linux GOARCH=amd64, without test build tags.
# The original pinned GOST cache is read only; no download or child GOST rule.
set +xv
set -Eeuo pipefail
umask 077

dynamic_unit_load() {
  local result
  result=$(systemctl show "$UNIT" -p LoadState --value 2>/dev/null) || [[ $result == not-found ]] || return 1
  [[ $result == loaded || $result == not-found ]] || return 1
  printf '%s\n' "$result"
}
dynamic_unit_owned() {
  local actual
  actual=$(systemctl show "$UNIT" -p Description --value) || return
  [[ $actual == "MSBOOST isolated DynamicUser validation $NAME" ]] || return 1
  actual=$(systemctl show "$UNIT" -p StateDirectory --value) || return
  [[ $actual == "$NAME" ]] || return 1
  actual=$(systemctl show "$UNIT" -p DynamicUser --value) || return
  [[ $actual == yes ]] || return 1
  actual=$(systemctl show "$UNIT" -p ExecStart --value) || return
  [[ $actual == *"path=$STAGE/relayruntime.test ;"* && $actual == *'argv[]='*'-test.run=^TestDynamicUserServiceChild$'* ]] || return 1
}
dynamic_validate_paths() {
  [[ $NAME =~ ^msboost-dyn-[a-f0-9]{12}$ && $UNIT == "$NAME.service" && $PUBLIC == "/var/lib/$NAME" && $PRIVATE == "/var/lib/private/$NAME" ]] || return 1
  [[ $STAGE =~ ^/usr/local/libexec/msboost-dynamic-user\.[A-Za-z0-9]{8}$ && -d $STAGE && ! -L $STAGE && $(realpath -e "$STAGE") == "$STAGE" && $(stat -c '%u:%g:%a:%d:%i' "$STAGE") == "$STAGE_ID" ]] || return 1
  if [[ -e $PUBLIC || -L $PUBLIC ]]; then
    [[ -L $PUBLIC && $(stat -c '%u:%g' "$PUBLIC") == 0:0 && $(realpath -e "$PUBLIC") == "$PRIVATE" ]] || return 1
  fi
  if [[ -e $PRIVATE || -L $PRIVATE ]]; then
    [[ -d $PRIVATE && ! -L $PRIVATE && $(realpath -e "$PRIVATE") == "$PRIVATE" && $(stat -c '%a' "$PRIVATE") == 700 ]] || return 1
    if [[ -n ${STATE_ID:-} ]]; then [[ $(stat -c '%d:%i' "$PRIVATE") == "$STATE_ID" ]] || return 1; fi
  fi
}
dynamic_cleanup() {
  local load attempt
  [[ ${INTENT:-0} == 1 ]] || return 0
  dynamic_validate_paths || return 1
  load=$(dynamic_unit_load) || return 1
  if [[ $load == loaded ]]; then
    dynamic_unit_owned || return 1
    journalctl --unit "$UNIT" --no-pager --output=short-iso > "$REPORTS/service-journal.log" || return 1
    systemctl stop "$UNIT" || return 1
    load=$(dynamic_unit_load) || return 1
    if [[ $load == loaded ]]; then
      dynamic_unit_owned || return 1
      [[ $(systemctl show "$UNIT" -p MainPID --value) == 0 ]] || return 1
      [[ $(systemctl show "$UNIT" -p ActiveState --value) == inactive || $(systemctl show "$UNIT" -p ActiveState --value) == failed ]] || return 1
      # A failed transient unit may remain loaded after its process exits.
      # Reset only our proven stopped unit, then require actual GC before
      # claiming that every owned service resource has been removed.
      systemctl reset-failed "$UNIT" || { [[ $(dynamic_unit_load) == not-found ]] || return 1; }
      for ((attempt=0; attempt<20; attempt++)); do
        load=$(dynamic_unit_load) || return 1
        [[ $load == loaded ]] || break
        dynamic_unit_owned || return 1
        [[ $(systemctl show "$UNIT" -p MainPID --value) == 0 ]] || return 1
        sleep 0.1
      done
      [[ $load == not-found ]] || return 1
    fi
  fi
  dynamic_validate_paths || return 1
  # Only the two pre-absent, exact random fixture state targets. The global
  # /var/lib/private parent and every non-fixture service remain untouched.
  if [[ -e $PRIVATE ]]; then
    [[ -n ${STATE_ID:-} && $(stat -c '%d:%i' "$PRIVATE") == "$STATE_ID" ]] || return 1
    rm -rf -- "$PRIVATE" || return 1
  fi
  if [[ -L $PUBLIC ]]; then rm -- "$PUBLIC" || return 1; fi
  [[ ! -e $PRIVATE && ! -L $PRIVATE && ! -e $PUBLIC && ! -L $PUBLIC ]] || return 1
  [[ $(stat -c '%u:%g:%a:%d:%i' "$STAGE") == "$STAGE_ID" ]] || return 1
  rm -rf -- "$STAGE" || return 1
  [[ ! -e $STAGE && ! -L $STAGE ]] || return 1
}

[[ ${MSBOOST_DYNAMIC_USER_VALIDATION:-} == 1 && $# == 1 && $(id -u) == 0 && $(uname -s) == Linux && $(uname -m) == x86_64 ]] || exit 2
[[ $(cat /proc/1/comm) == systemd ]] || exit 2
ASSETS=$1
[[ $ASSETS =~ ^/(root|var/tmp)/[A-Za-z0-9/_.-]+$ && -d $ASSETS && ! -L $ASSETS && $(realpath -e "$ASSETS") == "$ASSETS" && $(stat -c '%u:%g:%a' "$ASSETS") == 0:0:700 ]] || exit 2
[[ -f $ASSETS/assets.sha256 && ! -L $ASSETS/assets.sha256 ]] || exit 2
(cd "$ASSETS" && sha256sum --check --strict assets.sha256 >/dev/null)
for binary in msboost-agent relayruntime.test; do
  [[ -f $ASSETS/bin/$binary && ! -L $ASSETS/bin/$binary && $(stat -c '%u:%g' "$ASSETS/bin/$binary") == 0:0 ]] || exit 2
done
GOST=/usr/local/libexec/msboost-free/gost-676fb7f78d267b6ae73df719c0c7f2b565dde7147da935cfafbc1e1da558b6d5
[[ -f $GOST && ! -L $GOST && $(realpath -e "$GOST") == "$GOST" && $(stat -c '%u:%g:%a' "$GOST") == 0:0:755 ]] || exit 2
[[ $(sha256sum "$GOST" | cut -d ' ' -f1) == 1d8f971e9447cf4114fb1376b85c8c14e840db50ae8dbe895398a34e97c13e08 ]] || exit 2
# /run is commonly a small tmpfs on a test VPS. Stage only in an existing,
# protected disk path visible under ProtectHome/PrivateTmp; do not alter those
# service protections or create/relax a shared parent. Require all asset bytes
# plus 16 MiB of headroom before allocating any temporary directory.
for parent in /usr /usr/local /usr/local/libexec; do
  [[ -d $parent && ! -L $parent && $(realpath -e "$parent") == "$parent" && $(stat -c '%u:%g:%a' "$parent") == 0:0:755 ]] || exit 2
done
ASSET_BYTES=0
for asset in "$ASSETS/bin/msboost-agent" "$ASSETS/bin/relayruntime.test" "$GOST"; do
  size=$(stat -c '%s' "$asset")
  [[ $size =~ ^[0-9]{1,12}$ ]] || exit 2
  ASSET_BYTES=$((ASSET_BYTES + size))
done
FREE_KIB=$(df -Pk /usr/local/libexec | awk 'NR==2 {print $4}')
[[ $FREE_KIB =~ ^[0-9]{1,12}$ && $FREE_KIB -ge $(((ASSET_BYTES + 16777216 + 1023) / 1024)) ]] || { printf '%s\n' 'DYNAMIC_USER_REFUSED: insufficient disk headroom for immutable test assets' >&2; exit 2; }
NAME=msboost-dyn-$(od -An -N6 -tx1 /dev/urandom | tr -d ' \n')
[[ $NAME =~ ^msboost-dyn-[a-f0-9]{12}$ ]] || exit 2
UNIT=$NAME.service PUBLIC=/var/lib/$NAME PRIVATE=/var/lib/private/$NAME
[[ $(dynamic_unit_load) == not-found && ! -e $PUBLIC && ! -L $PUBLIC && ! -e $PRIVATE && ! -L $PRIVATE ]] || exit 2
REPORTS=$(mktemp -d /root/msboost-dynamic-user.XXXXXXXX)
STAGE=$(mktemp -d /usr/local/libexec/msboost-dynamic-user.XXXXXXXX)
chmod 0755 "$STAGE"
STAGE_ID=$(stat -c '%u:%g:%a:%d:%i' "$STAGE")
INTENT=1 STATE_ID=
on_exit() {
  local result=$?
  trap - EXIT INT TERM
  if ! dynamic_cleanup; then
    printf 'DYNAMIC_USER_CLEANUP_FAILED unit=%s state=%s stage=%s reports=%s\n' "$UNIT" "$PRIVATE" "$STAGE" "$REPORTS" >&2
    result=1
  else
    printf 'DYNAMIC_USER_OWNED_RESOURCES_REMOVED=1\n'
  fi
  printf 'DYNAMIC_USER_PRIVATE_REPORTS=%s\n' "$REPORTS"
  if [[ $result == 0 ]]; then printf 'DYNAMIC_USER_SUITE_PASS=true scope=real_unit_startup_socket_and_restart_not_forwarding_migration\n'; fi
  exit "$result"
}
trap on_exit EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
printf 'DYNAMIC_USER_PRIVATE_REPORTS=%s\nDYNAMIC_USER_RANDOM_UNIT=%s\n' "$REPORTS" "$UNIT"
SYSTEMD_VERSION=$(systemctl --version)
printf '%s\n' "${SYSTEMD_VERSION%%$'\n'*}"
for binary in msboost-agent relayruntime.test; do
  install -o root -g root -m 0755 -- "$ASSETS/bin/$binary" "$STAGE/$binary"
  [[ $(sha256sum "$STAGE/$binary" | cut -d ' ' -f1) == "$(sha256sum "$ASSETS/bin/$binary" | cut -d ' ' -f1)" ]]
done
install -o root -g root -m 0755 -- "$GOST" "$STAGE/gost"
[[ $(sha256sum "$STAGE/gost" | cut -d ' ' -f1) == 1d8f971e9447cf4114fb1376b85c8c14e840db50ae8dbe895398a34e97c13e08 ]]
# The test has no forwarding records; GOST is pinned and never replaced or run.
# RootDirectory and PrivateUsers are deliberately absent, as in the real unit.
systemd-run --unit "$UNIT" --description "MSBOOST isolated DynamicUser validation $NAME" \
  --property=Type=exec --property=DynamicUser=true --property="StateDirectory=$NAME" --property=StateDirectoryMode=0700 \
  --property=NoNewPrivileges=true --property=PrivateTmp=true --property=ProtectSystem=strict --property=ProtectHome=true \
  --property=ProtectKernelTunables=true --property=ProtectKernelModules=true --property=ProtectControlGroups=true \
  --property=RestrictSUIDSGID=true --property='RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX' --property=UMask=0077 \
  --property=CapabilityBoundingSet=CAP_NET_BIND_SERVICE --property=AmbientCapabilities=CAP_NET_BIND_SERVICE \
  --property=PrivateNetwork=true --property=Restart=no --property=RuntimeMaxSec=240 --property=TimeoutStopSec=10 \
  --setenv=MSBOOST_DYNAMIC_USER_VALIDATION=service --setenv="MSBOOST_DYNAMIC_NAME=$NAME" --setenv="GOST_TEST_BINARY=$STAGE/gost" \
  "$STAGE/relayruntime.test" '-test.run=^TestDynamicUserServiceChild$' -test.v -test.timeout=4m
dynamic_unit_owned
wait_ready() {
  local phase=$1 i
  for ((i=0; i<200; i++)); do
    dynamic_unit_owned || return 1
    if [[ -d $PRIVATE && ! -L $PRIVATE ]]; then
      if [[ -z $STATE_ID ]]; then STATE_ID=$(stat -c '%d:%i' "$PRIVATE"); else [[ $(stat -c '%d:%i' "$PRIVATE") == "$STATE_ID" ]] || return 1; fi
    fi
    if [[ -f $PRIVATE/dynamic-$phase.json && ! -L $PRIVATE/dynamic-$phase.json ]]; then
      dynamic_validate_paths || return 1
      return 0
    fi
    [[ $(systemctl show "$UNIT" -p ActiveState --value) == active ]] || return 1
    sleep 0.1
  done
  return 1
}
host_phase() {
  local phase=$1 path=$PUBLIC pid
  [[ $phase == first || $phase == restart ]] || return 1
  wait_ready "$phase" || return 1
  pid=$(systemctl show "$UNIT" -p MainPID --value) || return
  [[ $pid =~ ^[1-9][0-9]+$ && $pid != "${FIRST_PID:-}" ]] || return 1
  [[ $phase == first ]] || path=$PRIVATE
  [[ ! -e $REPORTS/$phase-snapshot.json && ! -L $REPORTS/$phase-snapshot.json ]] || return 1
  "$STAGE/msboost-agent" --capability relay-recovery --state-dir "$path" --recovery-action snapshot --recovery-file "$REPORTS/$phase-snapshot.json" > "$REPORTS/$phase-cli.log" 2>&1 || return
  MSBOOST_DYNAMIC_USER_VALIDATION=host MSBOOST_DYNAMIC_NAME=$NAME MSBOOST_DYNAMIC_PHASE=$phase MSBOOST_DYNAMIC_PID=$pid MSBOOST_DYNAMIC_REPORTS=$REPORTS \
    "$STAGE/relayruntime.test" '-test.run=^TestDynamicUserRootEvidence$' -test.v -test.timeout=30s > "$REPORTS/$phase-evidence.log" 2>&1 || return
  cat "$REPORTS/$phase-evidence.log"
  if [[ $phase == first ]]; then FIRST_PID=$pid; fi
}
host_phase first
dynamic_unit_owned
systemctl restart "$UNIT"
host_phase restart
dynamic_unit_owned
printf 'DYNAMIC_USER_STARTUP_SOCKET_RESTART_ASSERTIONS_PASS=true\n'
