#!/usr/bin/env bash
# Explicit disposable Linux validation, not an installer for a customer site.
# The pinned SSH harness creates and attests WORK; no production path is used.
set -Eeuo pipefail
umask 077
[[ ${MSBOOST_ISOLATED_LINUX_TEST:-} == 1 && $# == 1 && $(id -u) == 0 && $(uname -s) == Linux ]] || exit 2
[[ ${MSBOOST_NATIVE_ONLY:-0} != 1 || ${MSBOOST_SKIP_NATIVE:-0} != 1 ]] || exit 2
WORK=$1
[[ $WORK =~ ^/var/tmp/msboost-linux-validation\.[A-Za-z0-9]{8}$ && -d $WORK && ! -L $WORK && $(realpath -e "$WORK") == "$WORK" && $(stat -c '%u:%g:%a' "$WORK") == 0:0:700 ]] || exit 2
[[ -f $WORK/.validation-owner && ! -L $WORK/.validation-owner && $(<"$WORK/.validation-owner") == MSBOOST_ISOLATED_VALIDATION_V1 ]] || exit 2
cd "$WORK"
sha256sum --check --strict assets.sha256 >/dev/null
TEST_ID=$(printf '%s' "${WORK##*.}" | tr '[:upper:]' '[:lower:]')
PROJECT="msboost-check-$TEST_ID"
IMAGE="$PROJECT:current-source"
SWAP_PATH="$WORK/validation.swap"
SWAP_ID= SWAP_ACTIVE=0 DOCKER_READY=0
START_DISK=$(df -Pk "$WORK" | awk 'NR==2 {print $3}')
INITIAL_BUNDLE_DISK=$(du -sk -- "$WORK" | awk '{print $1}')
[[ $(df -Pk "$WORK" | awk 'NR==2 {print $4}') -ge 4194304 ]] || { printf 'REFUSED: need at least 4GiB free for bounded validation.\n'; exit 1; }

local_docker() { (unset DOCKER_HOST DOCKER_CONTEXT DOCKER_DEFAULT_PLATFORM; command docker --host unix:///var/run/docker.sock "$@"); }
dc() {
  (unset MSBOOST_IMAGE MSBOOST_VERSION MSBOOST_DOMAIN MSBOOST_SITE_ADDRESS MSBOOST_DATABASE_NAME POSTGRES_IMAGE CADDY_IMAGE POSTGRES_PASSWORD ADMIN_EMAIL ADMIN_PASSWORD MASTER_KEY PUBLIC_URL COOKIE_SECURE
   MSBOOST_ENV_FILE="$WORK/test.env" local_docker compose --project-name "$PROJECT" --env-file "$WORK/test.env" -f "$WORK/deploy/compose.yml" -f "$WORK/compose-test.yml" "$@")
}
check_control_subnet() {
  local -a network_ids=()
  local network
  ip -j -4 route show table all > "$WORK/routes.json" || return
  mapfile -t network_ids < <(local_docker network ls --quiet)
  [[ ${#network_ids[@]} -ge 1 ]] || return 1
  for network in "${network_ids[@]}"; do [[ $network =~ ^[a-f0-9]{12,64}$ ]] || return 1; done
  local_docker network inspect "${network_ids[@]}" > "$WORK/networks.json" || return
  local_docker run --rm --network none --read-only --user 0:0 --cap-drop ALL --label "msboost.validation=$TEST_ID" \
    --mount "type=bind,source=$WORK/routes.json,target=/routes.json,readonly" \
    --mount "type=bind,source=$WORK/networks.json,target=/networks.json,readonly" --entrypoint python3 "$IMAGE" -c '
import ipaddress,json,sys
scope=ipaddress.ip_network("172.30.86.0/24")
routes=json.load(open("/routes.json"))
networks=json.load(open("/networks.json"))
values=[r["dst"] for r in routes if r.get("dst") not in (None,"default")]
values += [c["Subnet"] for n in networks for c in (n.get("IPAM",{}).get("Config") or []) if c.get("Subnet")]
for raw in values:
    candidate=ipaddress.ip_network(raw,strict=False)
    if candidate.version==scope.version and candidate.overlaps(scope):
        print("REFUSED: fixed Compose subnet overlaps an existing host route or Docker network.",file=sys.stderr)
        sys.exit(1)
print("ISOLATED_VALIDATION_SUBNET_COLLISION_CHECK=passed")
' || return
}
activity_inspect() {
  dc run --rm --no-deps -T --user 0:0 "$@" --volume "${PROJECT}_app_data:/app/data:ro" --entrypoint /usr/local/bin/msboost-restore server backup-activity inspect > "$WORK/activity-report.json"
}
activity_assert() {
  local mode=$1
  local_docker run --rm --network none --read-only --user 0:0 --cap-drop ALL --label "msboost.validation=$TEST_ID" \
    --mount "type=bind,source=$WORK/activity-report.json,target=/report.json,readonly" --entrypoint python3 "$IMAGE" -c '
import json,sys
r=json.load(open("/report.json"));mode=sys.argv[1]
assert "lockIdentity" not in r and "operation" not in r
if mode=="held": assert r["pending"] is True and r["eligible"] is False
elif mode=="eligible":
    assert r["pending"] is True and r["eligible"] is True
    print(r["fingerprint"])
elif mode=="unknown": assert r["status"]=="interrupted_unknown"
elif mode=="empty": assert r["pending"] is False and r["eligible"] is False
else: raise Exception("unsupported assertion")
' "$mode"
}
check_readonly_activity() {
  local container operation="activity-$TEST_ID-$(random_hex 8)" fingerprint metadata after ready=0 attempt before state_after
  printf '%s\n' 'ISOLATED_VALIDATION_STAGE=original-readonly-volume-activity'
  container=$(dc run --detach --no-deps -T --user 10001:10001 --volume "$WORK/bin/control.test:/activity.test:ro" \
    --entrypoint /activity.test --env MSBOOST_CI_ACTIVITY_FIXTURE=hold --env "MSBOOST_CI_ACTIVITY_ID=$operation" server \
    -test.run '^TestBackupActivityComposeFixture$' -test.count=1 -test.timeout=6m) || return
  [[ $container =~ ^[a-f0-9]{64}$ && $(local_docker inspect --format '{{index .Config.Labels "msboost.validation"}}' "$container") == "$TEST_ID" ]] || return 1
  for ((attempt=0;attempt<60;attempt++)); do
    if local_docker logs "$container" 2>&1 | grep -qx CI_BACKUP_ACTIVITY_OWNER_READY; then ready=1; break; fi
    [[ $(local_docker inspect --format '{{.State.Running}}' "$container") == true ]] || return 1
    sleep 1
  done
  [[ $ready == 1 ]] || return 1
  metadata=$(dc exec -T server stat -c '%d:%i:%u:%g:%a:%s' /app/data/backup-activity.lock) || return
  [[ $metadata =~ ^[0-9]+:[0-9]+:10001:10001:600:65$ ]] || return 1
  before=$(dc exec -T database psql -U msboost -d msboost -tAX -c 'SELECT md5(payload) FROM control_state WHERE id=1') || return
  activity_inspect --cap-add DAC_READ_SEARCH || return
  activity_assert held || return
  state_after=$(dc exec -T database psql -U msboost -d msboost -tAX -c 'SELECT md5(payload) FROM control_state WHERE id=1') || return
  [[ $before == "$state_after" ]] || return 1
  [[ $(local_docker inspect --format '{{index .Config.Labels "msboost.validation"}}' "$container") == "$TEST_ID" ]] || return 1
  local_docker kill --signal KILL "$container" >/dev/null || return
  local_docker wait "$container" >/dev/null || return
  activity_inspect || return
  activity_assert held || return # root without DAC cannot read UID10001 mode0600.
  activity_inspect --cap-add DAC_READ_SEARCH || return
  fingerprint=$(activity_assert eligible) || return
  [[ $fingerprint =~ ^[a-f0-9]{64}$ ]] || return 1
  printf '%s\n%s\nRECONCILE_BACKUP %s\n' "$operation" "$fingerprint" "$operation" |
    dc run --rm --no-deps -T --user 0:0 --cap-add DAC_READ_SEARCH --volume "${PROJECT}_app_data:/app/data:ro" \
      --entrypoint /usr/local/bin/msboost-restore server backup-activity reconcile > "$WORK/activity-report.json" || return
  activity_assert unknown || return
  activity_inspect --cap-add DAC_READ_SEARCH || return
  activity_assert empty || return
  after=$(dc exec -T server stat -c '%d:%i:%u:%g:%a:%s' /app/data/backup-activity.lock) || return
  [[ $metadata == "$after" ]] || return 1
  local_docker rm "$container" >/dev/null || return
  printf '%s\n' 'ISOLATED_VALIDATION_ACTIVITY_PASS: same original inode; live owner rejected; SIGKILL releases flock; read-only original volume; UID0 requires DAC_READ_SEARCH; PG interrupted_unknown.'
}
cleanup_validation() {
  local status=$? container volume current_swaps containers volumes networks images remaining
  trap - EXIT
  trap '' HUP INT TERM
  if [[ $DOCKER_READY == 1 ]]; then
    if local_docker info >/dev/null 2>&1 &&
       containers=$(local_docker ps -aq --filter "label=msboost.validation=$TEST_ID") &&
       volumes=$(local_docker volume ls -q --filter "label=msboost.validation=$TEST_ID") &&
       networks=$(local_docker network ls -q --filter "label=msboost.validation=$TEST_ID") &&
       images=$(local_docker image ls -q "$IMAGE"); then
    for container in $containers; do
      [[ $(local_docker inspect --format '{{index .Config.Labels "msboost.validation"}}' "$container") == "$TEST_ID" ]] && local_docker rm -f "$container" >/dev/null || status=1
    done
    for volume in app_data database_data caddy_data caddy_config; do
      if grep -Fxq -- "${PROJECT}_$volume" <<< "$volumes"; then
        [[ $(local_docker volume inspect --format '{{index .Labels "msboost.validation"}}' "${PROJECT}_$volume") == "$TEST_ID" && -z $(local_docker ps -aq --filter "volume=${PROJECT}_$volume") ]] && local_docker volume rm "${PROJECT}_$volume" >/dev/null || status=1
      fi
    done
    if [[ -n $networks ]]; then
      [[ $(local_docker network inspect --format '{{index .Labels "msboost.validation"}}' "${PROJECT}_control") == "$TEST_ID" ]] && local_docker network rm "${PROJECT}_control" >/dev/null || status=1
    fi
    if [[ -n $images ]]; then
      [[ $(local_docker image inspect --format '{{index .Config.Labels "msboost.validation"}}' "$IMAGE") == "$TEST_ID" ]] && local_docker image rm "$IMAGE" >/dev/null || status=1
    fi
    if remaining=$(local_docker ps -aq --filter "label=msboost.validation=$TEST_ID") && [[ -z $remaining ]] &&
       remaining=$(local_docker volume ls -q --filter "label=msboost.validation=$TEST_ID") && [[ -z $remaining ]] &&
       remaining=$(local_docker network ls -q --filter "label=msboost.validation=$TEST_ID") && [[ -z $remaining ]] &&
       remaining=$(local_docker image ls -q "$IMAGE") && [[ -z $remaining ]]; then :; else status=1; fi
    else status=1; fi
  fi
  if [[ $SWAP_ACTIVE == 1 ]]; then
    if current_swaps=$(swapon --noheadings --raw --show=NAME) &&
       [[ ! -L $SWAP_PATH && $(stat -c '%d:%i:%u:%g:%a' "$SWAP_PATH") == "$SWAP_ID:0:0:600" ]] &&
       { ! grep -Fxq -- "$SWAP_PATH" <<< "$current_swaps" || swapoff -- "$SWAP_PATH"; }; then
      SWAP_ACTIVE=0
      printf '%s\n' 'ISOLATED_VALIDATION_SWAP_DISABLED=1'
    else status=1; fi
  fi
  if [[ $SWAP_ACTIVE == 0 && -f $SWAP_PATH && ! -L $SWAP_PATH && $(stat -c '%d:%i:%u:%g:%a' "$SWAP_PATH") == "$SWAP_ID:0:0:600" ]]; then rm -- "$SWAP_PATH" || status=1; fi
  printf 'ISOLATED_VALIDATION_PROJECT_AND_SWAP_CLEANED=%s\n' "$([[ $status == 0 ]] && printf 1 || printf failed)"
  exit "$status"
}
trap cleanup_validation EXIT
trap 'exit 143' HUP INT TERM

if [[ ${MSBOOST_SKIP_NATIVE:-0} != 1 ]]; then
printf '%s\n' 'ISOLATED_VALIDATION_STAGE=short-linux-socket-and-original-lock'
GOST_CACHE=/usr/local/libexec/msboost-free/gost-676fb7f78d267b6ae73df719c0c7f2b565dde7147da935cfafbc1e1da558b6d5
[[ -f $GOST_CACHE && ! -L $GOST_CACHE && $(stat -c '%u:%g:%a' "$GOST_CACHE") == 0:0:755 ]]
printf '%s  %s\n' 1d8f971e9447cf4114fb1376b85c8c14e840db50ae8dbe895398a34e97c13e08 "$GOST_CACHE" | sha256sum --check --strict >/dev/null
# A private mount namespace shortens /tmp without modifying the host mount or
# leaking a long Unix socket pathname; all generated files remain under WORK.
# This private mount must retain normal /tmp traversal for the intentionally
# unprivileged SO_PEERCRED test child; WORK itself remains root-only mode0700.
mkdir -m 1777 test-tmp
unshare --mount --net -- bash -c '
  set -Eeuo pipefail
  mount --make-rprivate /
  mount --bind "$1/test-tmp" /tmp
  ip link set lo up
  export TMPDIR=/tmp
  "$1/bin/control.test" -test.run "^TestBackupActivity(LockLinux|LockPersistent|LockValidate|LockNever|LockRejects)" -test.count=1 -test.v -test.timeout=2m
  "$1/bin/relayruntime.test" -test.run "^TestV2(RecoveryLinux|StateDirectoryLock)" -test.count=1 -test.v -test.timeout=3m
  env MSBOOST_TEST_LOOPBACK_NETNS=1 MSBOOST_REAL_GOST_RECOVERY=1 GOST_TEST_BINARY="$2" "$1/bin/relayruntime.test" -test.run "^TestRealGost(ProtectedRecoveryKeepsOriginalConnection|KeepLastNewConnectionsAndRuleIsolation|LeaseExpiryClosesExistingConnection|TwoHopForwardingObserverAndRevocation)$" -test.count=1 -test.v -test.timeout=5m
  env MSBOOST_TEST_LOOPBACK_NETNS=1 MSBOOST_REAL_CONTROL_OUTAGE=1 GOST_TEST_BINARY="$2" "$1/bin/control.test" -test.run "^TestRealControlBackupPauseAndOfflineConnection$" -test.count=1 -test.v -test.timeout=8m
' msboost-test "$WORK" "$GOST_CACHE" </dev/null
bash deploy/relay_recovery_test.sh
bash deploy/backup_activity_recovery_test.sh
bash deploy/agent_migration_test.sh
else
  printf '%s\n' 'ISOLATED_VALIDATION_NATIVE_SEPARATE_EVIDENCE_REQUIRED=1'
fi
if [[ ${MSBOOST_NATIVE_ONLY:-} == 1 ]]; then
  printf '%s\n' 'ISOLATED_VALIDATION_NATIVE_PASS: actual Linux original flock/root socket/GOST recovery/control listener outage and isolated shell symlink contracts; no Docker/swap mutation.'
  exit 0
fi
printf '%s\n' 'ISOLATED_VALIDATION_STAGE=temporary-swap-and-official-docker'
[[ ! -e $SWAP_PATH && ! -L $SWAP_PATH ]]
fallocate -l 1G -- "$SWAP_PATH"
chmod 600 -- "$SWAP_PATH"
SWAP_ID=$(stat -c '%d:%i' -- "$SWAP_PATH")
mkswap -- "$SWAP_PATH" >/dev/null
SWAP_ACTIVE=1
swapon -- "$SWAP_PATH"
# Reuse the product's official Debian repository installation and its refusal
# to uninstall conflicting Docker packages. This does not install MSBOOST.
source "$WORK/deploy/manage.sh"
ensure_docker
# Sourcing the product only defines functions, but restore the isolated ID.
PROJECT="msboost-check-$TEST_ID"
[[ -z $(local_docker ps -aq --filter "label=com.docker.compose.project=$PROJECT") ]]
for volume in app_data database_data caddy_data caddy_config; do ! local_docker volume inspect "${PROJECT}_$volume" >/dev/null 2>&1; done
! local_docker network inspect "${PROJECT}_control" >/dev/null 2>&1
DOCKER_READY=1

printf 'MSBOOST_IMAGE=%s\nMSBOOST_DOMAIN=localhost\nMSBOOST_SITE_ADDRESS=http://localhost\nPUBLIC_URL=http://localhost\nCOOKIE_SECURE=false\nADMIN_EMAIL=123456789@qq.com\nADMIN_PASSWORD=%s\nPOSTGRES_PASSWORD=%s\nMASTER_KEY=%s\n' \
  "$IMAGE" "$(random_hex 24)" "$(random_hex 24)" "$(random_hex 32)" > "$WORK/test.env"
chmod 600 "$WORK/test.env"
{
  printf 'services:\n  server:\n    labels:\n      msboost.validation: %s\n  database:\n    labels:\n      msboost.validation: %s\n  caddy:\n    ports: !override ["127.0.0.1::80"]\n    labels:\n      msboost.validation: %s\nnetworks:\n  control:\n    internal: true\n    labels:\n      msboost.validation: %s\nvolumes:\n' "$TEST_ID" "$TEST_ID" "$TEST_ID" "$TEST_ID"
  for volume in app_data database_data caddy_data caddy_config; do printf '  %s:\n    name: %s_%s\n    labels:\n      msboost.validation: %s\n' "$volume" "$PROJECT" "$volume" "$TEST_ID"; done
} > "$WORK/compose-test.yml"
printf '%s\n' 'ISOLATED_VALIDATION_STAGE=build-current-source-precompiled-image'
install -m 0600 "$WORK/deploy/validation.dockerignore" "$WORK/.dockerignore"
local_docker build --label "msboost.validation=$TEST_ID" -t "$IMAGE" -f "$WORK/deploy/validation.Dockerfile" "$WORK" >/dev/null
dc pull database caddy >/dev/null
check_control_subnet
CURRENT_DISK=$(df -Pk "$WORK" | awk 'NR==2 {print $3}')
(( CURRENT_DISK - START_DISK + INITIAL_BUNDLE_DISK <= 3145728 )) || { printf 'REFUSED: validation storage including test bundle exceeded 3GiB budget.\n'; exit 1; }
printf 'ISOLATED_VALIDATION_DISK_GROWTH_KIB=%s\n' "$((CURRENT_DISK-START_DISK+INITIAL_BUNDLE_DISK))"
dc up -d --no-build --pull never --wait --wait-timeout 180 >/dev/null
bound=$(dc port caddy 80)
[[ $bound =~ ^127\.0\.0\.1:[0-9]+$ ]]
curl --fail --silent --show-error --noproxy '*' "http://$bound/api/health" | grep -q '"status":"ok"'
printf '%s\n' 'ISOLATED_VALIDATION_COMPOSE_HEALTHY_LOOPBACK=1'
check_readonly_activity

printf '%s\n' 'ISOLATED_VALIDATION_STAGE=all-linux-go-test-packages-with-real-postgres'
PG_PASSWORD=$(env_get "$WORK/test.env" POSTGRES_PASSWORD)
printf 'MSBOOST_TEST_POSTGRES_URL=postgres://msboost:%s@database:5432/postgres?sslmode=disable\n' "$PG_PASSWORD" > "$WORK/postgres-test.env"
chmod 600 "$WORK/postgres-test.env"
PG_PASSWORD=
for package in control disaster executor relayruntime deploy; do
  printf 'ISOLATED_VALIDATION_PACKAGE=%s\n' "$package"
  package_dir="/validation/internal/$package"
  [[ $package == deploy ]] && package_dir=/validation/deploy
  mkdir -p "$WORK/internal/$package"
  local_docker run --rm --network "${PROJECT}_control" --label "msboost.validation=$TEST_ID" --user 0:0 --read-only --tmpfs /tmp:rw,nosuid,exec,size=128m \
    --env-file "$WORK/postgres-test.env" --mount "type=bind,source=$WORK/bin/$package.test,target=/validation.test,readonly" --mount "type=bind,source=$WORK,target=/validation,readonly" --workdir "$package_dir" \
    --entrypoint /validation.test "$IMAGE" -test.count=1 -test.v -test.timeout=8m
done
printf '%s\n' 'ISOLATED_VALIDATION_PASS: actual Linux full test packages, real PostgreSQL, original-volume activity reconciliation and current-source Compose; cross-compiled native execution, not race instrumentation. Opt-in GOST cases require their separate native evidence.'
