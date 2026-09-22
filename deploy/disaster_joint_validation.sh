#!/usr/bin/env bash
# Test-only, explicitly scoped joint validation. Does not install a site,
# replace a launcher, touch global timer files, or run disaster_restore/purge.
# Usage: MSBOOST_JOINT_VALIDATION=1 bash ... start PRIVATE_ASSETS IMAGE_ID
# Assets are a NEW FINAL attested Linux validation bundle, not a directory
# being updated by another run. Compile the independent child with:
# GOOS=linux GOARCH=amd64 go test -tags msboost_joint_test -c ./internal/control
#   -o bin/joint-control.test
# Include this script, install.sh, deploy/, bin/msboost-{server,restore}, the
# child, and frontend build in the bundle's final assets.sha256. IMAGE_ID is
# already built from exactly these server/restore binaries (checked below).
# Requires no existing overlap with 172.30.86.0/24, root Docker/systemd/Python3,
# the already verified cached GOST, and >=4GiB free. No images are pulled here.
set +xv
set -Eeuo pipefail
umask 077
[[ ${MSBOOST_JOINT_VALIDATION:-} == 1 && $(id -u) == 0 && $(uname -s) == Linux && $(uname -m) == x86_64 ]] || exit 2
SCRIPT=$(realpath -e -- "${BASH_SOURCE[0]}")
ACTION=${1:-}
shift || exit 2
local_docker() { (unset DOCKER_HOST DOCKER_CONTEXT DOCKER_DEFAULT_PLATFORM; command docker --host unix:///var/run/docker.sock "$@"); }
die_joint() { printf 'JOINT_REFUSED: %s\n' "$*" >&2; return 1; }
read_work() {
  WORK=$1
  [[ $WORK =~ ^/root/msboost-disaster-joint\.[A-Za-z0-9]{8}$ && ! -L $WORK && $(realpath -e "$WORK") == "$WORK" && $(stat -c '%u:%g:%a' "$WORK") == 0:0:700 ]] || return 1
  [[ -f $WORK/channel/owner && ! -L $WORK/channel/owner && -f $WORK/assets && ! -L $WORK/assets && -f $WORK/image-id && ! -L $WORK/image-id ]] || return 1
  JOINT_PROJECT=$(<"$WORK/channel/owner")
  [[ $JOINT_PROJECT =~ ^msboost-joint-[a-z0-9]{8}$ ]] || return 1
  ASSETS=$(<"$WORK/assets"); IMAGE_ID=$(<"$WORK/image-id")
  [[ $ASSETS =~ ^/(root|var/tmp)/[A-Za-z0-9/_.-]+$ && ! -L $ASSETS && $(realpath -e "$ASSETS") == "$ASSETS" && $(stat -c '%u:%g:%a' "$ASSETS") == 0:0:700 && $IMAGE_ID =~ ^sha256:[a-f0-9]{64}$ ]] || return 1
  [[ -f $ASSETS/assets.sha256 && ! -L $ASSETS/assets.sha256 ]]
}
dc() {
  (unset MSBOOST_IMAGE MSBOOST_VERSION MSBOOST_DOMAIN MSBOOST_SITE_ADDRESS MSBOOST_DATABASE_NAME POSTGRES_IMAGE CADDY_IMAGE POSTGRES_PASSWORD ADMIN_EMAIL ADMIN_PASSWORD MASTER_KEY PUBLIC_URL COOKIE_SECURE
   MSBOOST_ENV_FILE="$WORK/site/.env" local_docker compose --project-name "$JOINT_PROJECT" --env-file "$WORK/site/.env" -f "$WORK/site/deploy/compose.yml" -f "$WORK/site/deploy/compose-joint.yml" "$@")
}
assert_owner() {
  local kind=$1 name=$2 actual
  case "$kind" in
    container) actual=$(local_docker inspect --type container --format '{{index .Config.Labels "msboost.joint"}}' "$name") || return ;;
    volume) actual=$(local_docker volume inspect --format '{{index .Labels "msboost.joint"}}' "$name") || return ;;
    network) actual=$(local_docker network inspect --format '{{index .Labels "msboost.joint"}}' "$name") || return ;;
    *) return 1 ;;
  esac
  [[ $actual == "$JOINT_PROJECT" ]]
}
gate() {
  local expected=$1
  dc run --rm --no-deps -T --user 0:0 --entrypoint /usr/local/bin/msboost-restore server backup-pause status > "$WORK/gate.json" || return
  python3 - "$WORK/gate.json" "$expected" <<'PY'
import json,sys
r=json.load(open(sys.argv[1])); assert r['gateActive'] == (sys.argv[2]=='true')
assert not any(k in r for k in ('token','tokenHash'))
PY
}

if [[ $ACTION == phase ]]; then
  [[ $# == 2 ]] || exit 2
  read_work "$1" || exit 2
  PHASE=$2
  [[ $PHASE =~ ^(manual|timer|dumpfail|packfail|uploadfail)$ && ! -t 0 ]] || exit 2
  python3 - "$WORK/site/disaster.json" "$PHASE" "$WORK" <<'PY'
import json,sys
c=json.load(open(sys.argv[1]))
if sys.argv[2]=='uploadfail': assert c.get('remoteHost')=='8.8.8.8' and c.get('remoteDir')==sys.argv[3]+'/remote'
else: assert not c.get('remoteHost')
PY
  if [[ $PHASE == uploadfail ]]; then
    # The saved synthetic public-looking destination must never be retried
    # from the host namespace, even if somebody invokes this child directly.
    ip -j link | python3 -c 'import json,sys; n=json.load(sys.stdin); assert len(n)==1 and n[0]["ifname"]=="lo"'
  fi
  (cd "$ASSETS" && sha256sum --check --strict assets.sha256 >/dev/null)
  source "$ASSETS/deploy/manage.sh"
  source "$ASSETS/deploy/disaster.sh"
  INSTALL_ROOT="$WORK/site" PROJECT=$JOINT_PROJECT SOURCE_DIR=$ASSETS
  DISASTER_WORK= DISASTER_TOOL_DIR= DISASTER_TOOL_CONTAINER= DISASTER_PAUSE_RELEASE=0 DISASTER_RESUME=0
  DISASTER_RUNNING=()
  # The production installation guard deliberately accepts /opt/msboost only.
  # This test substitutes only ownership/path identity for its fresh random
  # fixture. All production gate/export/packing/upload/cleanup logic is intact.
  assert_managed() {
    [[ ! -L $INSTALL_ROOT && $(stat -c '%u:%g:%a' "$INSTALL_ROOT") == 0:0:700 && $(<"$INSTALL_ROOT/.managed-by-msboost") == MSBOOST_DEPLOY_V1 && $(stat -c '%u:%g:%a' "$INSTALL_ROOT/.env") == 0:0:600 ]] || return 1
    local service id
    for service in database server caddy; do id=$(dc ps --all --quiet "$service") || return; [[ $id =~ ^[a-f0-9]{64}$ ]] || return 1; assert_owner container "$id" || return; done
  }
  disaster_tool() { [[ -x $ASSETS/bin/msboost-restore && ! -L $ASSETS/bin/msboost-restore ]] || return 1; DISASTER_TOOL="$ASSETS/bin/msboost-restore"; }
  cleanup_stage() { [[ -z ${STAGE:-} ]]; }
  # Narrow adapter: only the four exact hard-coded product volume names are
  # translated. No arbitrary replacement, source execution, mocked export,
  # or operations on an Agent/GOST/container daemon are permitted.
  docker() {
    local name volume mount image
    if [[ $# == 5 && $1 == volume && $2 == inspect && $3 == --format ]]; then
      name=$5
      case "$name" in msboost_app_data|msboost_database_data|msboost_caddy_data|msboost_caddy_config) ;; *) return 2 ;; esac
      volume="${JOINT_PROJECT}_${name#msboost_}"; assert_owner volume "$volume" || return
      local_docker volume inspect --format "$4" "$volume"
    elif [[ $# == 6 && $1 == inspect && $2 == --type && $3 == container && $4 == --format && $6 =~ ^[a-f0-9]{64}$ ]]; then
      assert_owner container "$6" || return
      local_docker "$@"
    elif [[ $# == 17 && $1 == run && $2 == --rm && $3 == --network && $4 == none && $5 == --read-only && $6 == --user && $7 == 0:0 && $8 == --entrypoint && $9 == tar && ${10} == --mount && ${13} == -cf && ${14} == - && ${15} == -C && ${16} == /snapshot && ${17} == . ]]; then
      mount=${11}; image=${12}
      [[ $mount =~ ^type=volume,source=msboost_(app_data|caddy_data|caddy_config),target=/snapshot,readonly$ ]] || return 2
      volume="${JOINT_PROJECT}_${BASH_REMATCH[1]}"; assert_owner volume "$volume" || return
      [[ $image == "$(env_get "$INSTALL_ROOT/.env" POSTGRES_IMAGE)" ]] || return 2
      gate true || return
      printf 'volume-export:%s:gate=true\n' "$volume" >> "$WORK/$PHASE.trace" || return
      local_docker run --rm --label "msboost.joint=$JOINT_PROJECT" --network none --read-only --user 0:0 --entrypoint tar --mount "type=volume,source=$volume,target=/snapshot,readonly" "$image" -cf - -C /snapshot .
    else die_joint 'Docker operation outside exact disaster adapter'; return 2; fi
  }
  compose_live() {
    case "${1:-}" in
      stop)
        [[ "$*" == 'stop --timeout 60 caddy server' ]] || return 2
        gate true || return
        printf 'stop:caddy,server:gate=true\n' >> "$WORK/$PHASE.trace" || return ;;
      start)
        [[ "$*" == 'start caddy server' ]] || return 2
        gate false || return
        printf 'start:caddy,server:gate=false\n' >> "$WORK/$PHASE.trace" || return ;;
      ps) ;;
      exec)
        [[ "$*" == 'exec -T database pg_dump --username=msboost --dbname=msboost --format=custom' ]] || return 2
        gate true || return
        printf 'pg-dump:gate=true\n' >> "$WORK/$PHASE.trace" || return
        # Genuine output-write failure in the local Docker CLI, not a fake
        # exit status. Only this child gets RLIMIT_FSIZE; cleanup is unlimited.
        if [[ $PHASE == dumpfail ]]; then (ulimit -c 0; ulimit -f 1; dc "$@"); return; fi ;;
      run)
        case "$*" in
          'run --rm --no-deps -T --user 0:0 --entrypoint /usr/local/bin/msboost-restore server backup-pause begin'|'run --rm --no-deps -T --user 0:0 --entrypoint /usr/local/bin/msboost-restore server backup-pause end') ;;
          'run --rm --no-deps -T --user 0:0 --entrypoint /usr/local/bin/msboost-restore server export') gate true || return; printf 'state-export:gate=true\n' >> "$WORK/$PHASE.trace" || return ;;
          *) return 2 ;;
        esac ;;
      *) return 2 ;;
    esac
    dc "$@"
  }
  mktemp() {
    [[ $# == 2 && $1 == -d && $2 == /root/msboost-disaster-work.XXXXXXXX ]] || return 2
    local path
    path=$(command mktemp "$@") || return
    printf '%s\n' "$path" > "$WORK/$PHASE.work" || { printf 'JOINT_UNRECORDED_PRIVATE_STAGING=%s\n' "$path" >&2; return 1; }
    printf '%s\n' "$path"
  }
  if [[ $PHASE == packfail ]]; then
    # The actual packer must reject this pre-existing private output without
    # overwriting it. Freeze only filename ingredients, never the gate token.
    date() { if [[ "$*" == '-u +%Y%m%dT%H%M%SZ' ]]; then printf '%s\n' "$(<"$WORK/collision-time")"; else command date "$@"; fi; }
    random_hex() { if [[ $1 == 8 ]]; then printf '%s\n' "$(<"$WORK/collision-nonce")"; else openssl rand -hex "$1"; fi; }
  fi
  [[ $PHASE != timer || ( -n ${INVOCATION_ID:-} && -n ${JOURNAL_STREAM:-} ) ]] || exit 2
  trap disaster_cleanup EXIT
  trap 'exit 143' HUP INT TERM
  disaster_backup
  exit 0
fi

if [[ $ACTION == upload-netns ]]; then
  [[ $# == 1 ]] || exit 2
  read_work "$1" || exit 2
  # sysfs can still belong to the caller's namespace after unshare --net.
  # Netlink, like Go net.Interfaces in the child, observes this actual netns.
  ip -j link | python3 -c 'import json,sys; n=json.load(sys.stdin); assert len(n)==1 and n[0]["ifname"]=="lo"'
  ip link set lo up
  ip address add 8.8.8.8/32 dev lo
  MSBOOST_JOINT_FIXTURE=readonly-sftp MSBOOST_JOINT_WORK="$WORK" "$ASSETS/bin/joint-control.test" -test.run '^TestDisasterJointReadOnlySFTP$' -test.count=1 -test.timeout=9m > "$WORK/sftp.log" 2>&1 &
  SFTP_PID=$!
  for ((i=0;i<100;i++)); do [[ ! -f $WORK/sftp-ready ]] || break; kill -0 "$SFTP_PID"; sleep 0.1; done
  [[ -f $WORK/sftp-ready ]]
  result=0
  bash "$SCRIPT" phase "$WORK" uploadfail </dev/null || result=$?
  for ((i=0;i<50;i++)); do [[ ! -f $WORK/sftp-create-denied ]] || break; kill -0 "$SFTP_PID"; sleep 0.1; done
  printf 'stop\n' > "$WORK/sftp-stop"
  wait "$SFTP_PID"
  [[ $result != 0 && -f $WORK/sftp-used ]] || exit 1
  exit "$result"
fi

[[ $ACTION == start && $# == 2 ]] || exit 2
ASSETS=$1 IMAGE_ID=$2
[[ $ASSETS =~ ^/(root|var/tmp)/[A-Za-z0-9/_.-]+$ && ! -L $ASSETS && $(realpath -e "$ASSETS") == "$ASSETS" && $(stat -c '%u:%g:%a' "$ASSETS") == 0:0:700 && $IMAGE_ID =~ ^sha256:[a-f0-9]{64}$ ]] || exit 2
(cd "$ASSETS" && sha256sum --check --strict assets.sha256 >/dev/null)
for executable in bin/joint-control.test bin/msboost-restore bin/msboost-server; do [[ -f $ASSETS/$executable && ! -L $ASSETS/$executable && -x $ASSETS/$executable ]] || exit 2; done
for program in docker python3 systemd-run systemctl unshare ip openssl; do command -v "$program" >/dev/null; done
[[ $(local_docker image inspect --format '{{.Id}}' "$IMAGE_ID") == "$IMAGE_ID" ]]
[[ $(df -Pk /root | awk 'NR==2 {print $4}') -ge 4194304 ]] || { die_joint 'at least 4GiB free required'; exit 1; }
GOST=/usr/local/libexec/msboost-free/gost-676fb7f78d267b6ae73df719c0c7f2b565dde7147da935cfafbc1e1da558b6d5
[[ ! -L $GOST && $(stat -c '%u:%g:%a' "$GOST") == 0:0:755 ]]
printf '%s  %s\n' 1d8f971e9447cf4114fb1376b85c8c14e840db50ae8dbe895398a34e97c13e08 "$GOST" | sha256sum --check --strict >/dev/null
WORK=$(mktemp -d /root/msboost-disaster-joint.XXXXXXXX)
JOINT_PROJECT="msboost-joint-$(printf '%s' "${WORK##*.}" | tr '[:upper:]' '[:lower:]')"
mkdir -m 700 "$WORK/channel" "$WORK/site" "$WORK/backups"
printf '%s\n' "$JOINT_PROJECT" > "$WORK/channel/owner"
printf '%s\n' "$ASSETS" > "$WORK/assets"
printf '%s\n' "$IMAGE_ID" > "$WORK/image-id"
printf 'JOINT_PRIVATE_REPORT_DIRECTORY=%s\n' "$WORK"
ALIAS= TIMER= DATA_CONTAINER= RESOURCES=0 TIMER_CREATED=0
cleanup_joint() {
  local result=$? item containers volumes networks remaining load phase retained
  trap - EXIT
  trap '' HUP INT TERM
  if [[ $TIMER_CREATED == 1 ]]; then
    # Intent is set before creation: a lost response does not prove that no
    # unit exists. Check the timer and service separately, including partial
    # creation. Do not stop a unit whose exact random ownership is uncertain.
    if load=$(systemctl show "$TIMER.timer" -p LoadState --value 2>/dev/null) || [[ $load == not-found ]]; then
      if [[ $load != not-found ]]; then
        if [[ $load == loaded && $(systemctl show "$TIMER.timer" -p Description --value) == "$JOINT_PROJECT strict full-backup fixture timer" && $(systemctl show "$TIMER.timer" -p Triggers --value) == "$TIMER.service" ]]; then systemctl stop "$TIMER.timer" >/dev/null 2>&1 || result=1; else result=1; fi
      fi
    else result=1; fi
    if load=$(systemctl show "$TIMER.service" -p LoadState --value 2>/dev/null) || [[ $load == not-found ]]; then
      if [[ $load != not-found ]]; then
        if [[ $load == loaded && $(systemctl show "$TIMER.service" -p Description --value) == "$JOINT_PROJECT strict full-backup fixture" ]]; then systemctl stop "$TIMER.service" >/dev/null 2>&1 || result=1; else result=1; fi
      fi
    else result=1; fi
  fi
  if [[ $RESOURCES == 1 ]]; then
    if local_docker info >/dev/null 2>&1 && containers=$(local_docker ps -aq --filter "label=msboost.joint=$JOINT_PROJECT") && volumes=$(local_docker volume ls -q --filter "label=msboost.joint=$JOINT_PROJECT") && networks=$(local_docker network ls -q --filter "label=msboost.joint=$JOINT_PROJECT"); then
    while IFS= read -r item; do [[ -z $item ]] || { assert_owner container "$item" && local_docker logs "$item" > "$WORK/container-${item:0:12}.log" 2>&1 && local_docker rm -f "$item" >/dev/null; } || result=1; done <<< "$containers"
    for item in app_data database_data caddy_data caddy_config; do
      if grep -Fxq -- "${JOINT_PROJECT}_$item" <<< "$volumes"; then assert_owner volume "${JOINT_PROJECT}_$item" && remaining=$(local_docker ps -aq --filter "volume=${JOINT_PROJECT}_$item") && [[ -z $remaining ]] && local_docker volume rm "${JOINT_PROJECT}_$item" >/dev/null || result=1; fi
    done
    if [[ -n $networks ]]; then assert_owner network "${JOINT_PROJECT}_control" && local_docker network rm "${JOINT_PROJECT}_control" >/dev/null || result=1; fi
    if remaining=$(local_docker ps -aq --filter "label=msboost.joint=$JOINT_PROJECT") && [[ -z $remaining ]] && remaining=$(local_docker volume ls -q --filter "label=msboost.joint=$JOINT_PROJECT") && [[ -z $remaining ]] && remaining=$(local_docker network ls -q --filter "label=msboost.joint=$JOINT_PROJECT") && [[ -z $remaining ]]; then :; else result=1; fi
    else result=1; fi
  fi
  if [[ -n $ALIAS ]]; then
    if remaining=$(local_docker image ls --format '{{.Repository}}:{{.Tag}}' "$ALIAS"); then
      if [[ -n $remaining ]]; then [[ $remaining == "$ALIAS" && $(local_docker image inspect --format '{{.Id}}' "$ALIAS") == "$IMAGE_ID" ]] && local_docker image rm "$ALIAS" >/dev/null || result=1; fi
    else result=1; fi
  fi
  # Production failure cleanup deliberately retains these private diagnostic
  # directories. Their exact path manifests also remain under private_reports;
  # successful test resources cleanup does not claim these files were erased.
  for phase in manual timer dumpfail packfail uploadfail; do
    if [[ -f $WORK/$phase.work && ! -L $WORK/$phase.work ]]; then
      retained=$(<"$WORK/$phase.work")
      if [[ $retained =~ ^/root/msboost-disaster-work\.[A-Za-z0-9]{8}$ && -d $retained && ! -L $retained ]]; then printf 'JOINT_RETAINED_PRIVATE_STAGING=%s\n' "$retained"; fi
    fi
  done
  printf 'JOINT_FINISHED_EXIT=%s private_reports=%s\n' "$result" "$WORK"
  exit "$result"
}
trap cleanup_joint EXIT
trap 'exit 143' HUP INT TERM
[[ -z $(local_docker ps -aq --filter "label=com.docker.compose.project=$JOINT_PROJECT") ]]
for volume in app_data database_data caddy_data caddy_config; do ! local_docker volume inspect "${JOINT_PROJECT}_$volume" >/dev/null 2>&1; done
! local_docker network inspect "${JOINT_PROJECT}_control" >/dev/null 2>&1
# Retain the exact official Compose subnet, but refuse any existing overlap.
ip -j -4 route show table all > "$WORK/routes.json"
mapfile -t networks < <(local_docker network ls -q)
local_docker network inspect "${networks[@]}" > "$WORK/networks.json"
python3 - "$WORK" <<'PY'
import ipaddress,json,sys
w=sys.argv[1]; wanted=ipaddress.ip_network('172.30.86.0/24')
values=[r['dst'] for r in json.load(open(w+'/routes.json')) if r.get('dst') not in (None,'default')]
values += [c['Subnet'] for n in json.load(open(w+'/networks.json')) for c in n.get('IPAM',{}).get('Config',[]) or [] if c.get('Subnet')]
assert not any((n:=ipaddress.ip_network(x,strict=False)).version==4 and n.overlaps(wanted) for x in values), 'Compose subnet already in use'
PY
source "$ASSETS/deploy/manage.sh"
# Restore fixture variables overridden by sourcing the product manager.
PROJECT=$JOINT_PROJECT
POSTGRES_IMAGE=$(local_docker image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' postgres:17-bookworm | awk '/^postgres@sha256:[a-f0-9]+$/ {print; exit}')
CADDY_IMAGE=$(local_docker image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' caddy:2-alpine | awk '/^caddy@sha256:[a-f0-9]+$/ {print; exit}')
[[ $POSTGRES_IMAGE =~ ^postgres@sha256:[a-f0-9]{64}$ && $CADDY_IMAGE =~ ^caddy@sha256:[a-f0-9]{64}$ ]]
ALIAS="msboost-release:$VERSION-amd64-$(openssl rand -hex 6)"
! local_docker image inspect "$ALIAS" >/dev/null 2>&1
local_docker image tag "$IMAGE_ID" "$ALIAS"
cp -a -- "$ASSETS/deploy" "$WORK/site/deploy"
install -m 600 "$ASSETS/install.sh" "$WORK/site/install.sh"
printf 'MSBOOST_DEPLOY_V1\n' > "$WORK/site/.managed-by-msboost"
printf 'MSBOOST_VERSION=%s\nMSBOOST_IMAGE=%s\nMSBOOST_IMAGE_ID=%s\nPOSTGRES_IMAGE=%s\nCADDY_IMAGE=%s\nMSBOOST_DATABASE_NAME=msboost\nMSBOOST_DOMAIN=8.8.8.8\nMSBOOST_SITE_ADDRESS=http://8.8.8.8\nPUBLIC_URL=http://8.8.8.8\nCOOKIE_SECURE=false\nADMIN_EMAIL=123456789@qq.com\nADMIN_PASSWORD=%s\nPOSTGRES_PASSWORD=%s\nMASTER_KEY=%s\n' "$VERSION" "$ALIAS" "$IMAGE_ID" "$POSTGRES_IMAGE" "$CADDY_IMAGE" "$(random_admin_password)" "$(openssl rand -hex 24)" "$(openssl rand -hex 32)" > "$WORK/site/.env"
printf '{"version":1,"localDir":"%s/backups","retentionDays":30,"time":"02:30"}\n' "$WORK" > "$WORK/site/disaster.json"
{
  printf 'services:\n  server:\n    labels:\n      msboost.joint: %s\n  database:\n    labels:\n      msboost.joint: %s\n  caddy:\n    ports: !override []\n    labels:\n      msboost.joint: %s\nnetworks:\n  control:\n    internal: true\n    labels:\n      msboost.joint: %s\nvolumes:\n' "$JOINT_PROJECT" "$JOINT_PROJECT" "$JOINT_PROJECT" "$JOINT_PROJECT"
  for volume in app_data database_data caddy_data caddy_config; do printf '  %s:\n    name: %s_%s\n    labels:\n      msboost.joint: %s\n' "$volume" "$JOINT_PROJECT" "$volume" "$JOINT_PROJECT"; done
} > "$WORK/site/deploy/compose-joint.yml"
RESOURCES=1
for binary in msboost-server msboost-restore; do
  expected=$(sha256sum "$ASSETS/bin/$binary"); expected=${expected%% *}
  actual=$(local_docker run --rm --label "msboost.joint=$JOINT_PROJECT" --network none --read-only --user 0:0 --cap-drop ALL --entrypoint sha256sum "$IMAGE_ID" "/usr/local/bin/$binary"); actual=${actual%% *}
  [[ $expected == "$actual" ]] || { die_joint 'image does not match the final attested source binaries'; exit 1; }
done
dc config --quiet
dc up -d --no-build --pull never --wait --wait-timeout 180 > "$WORK/start.log" 2>&1
# Real files in each original volume make empty/fabricated tar substitutes fail.
for volume in app_data caddy_data caddy_config; do
  assert_owner volume "${JOINT_PROJECT}_$volume"
  local_docker run --rm --label "msboost.joint=$JOINT_PROJECT" --network none --user 0:0 --entrypoint sh --mount "type=volume,source=${JOINT_PROJECT}_$volume,target=/proof" "$POSTGRES_IMAGE" -c 'umask 077; printf "%s\n" "$1" > /proof/joint-proof' fixture "$JOINT_PROJECT:$volume"
done
DATA_CONTAINER=$(local_docker run --detach --label "msboost.joint=$JOINT_PROJECT" --network "${JOINT_PROJECT}_control" --read-only --user 0:0 --cap-drop ALL --security-opt no-new-privileges --tmpfs /tmp:rw,nosuid,nodev,mode=1777 --env-file "$WORK/site/.env" --env MSBOOST_JOINT_FIXTURE=data-plane --env DATA_DIR=/joint/runtime --env DATABASE_HOST=database --env DATABASE_NAME=msboost --env DATABASE_USER=msboost --env DATABASE_SSLMODE=disable --mount "type=bind,source=$WORK/channel,target=/joint" --mount "type=bind,source=$ASSETS/bin/joint-control.test,target=/fixture/control.test,readonly" --mount "type=bind,source=$GOST,target=/fixture/gost,readonly" --entrypoint /fixture/control.test "$IMAGE_ID" -test.run '^TestDisasterJointDataPlane$' -test.v -test.count=1 -test.timeout=28m)
[[ $DATA_CONTAINER =~ ^[a-f0-9]{64}$ ]]; assert_owner container "$DATA_CONTAINER"
wait_phase() {
  local expected=$1 attempt
  for ((attempt=0;attempt<120;attempt++)); do
    [[ $(local_docker inspect --format '{{.State.Running}}' "$DATA_CONTAINER") == true ]] || { die_joint 'data plane exited; no retry/reconnection allowed'; return 1; }
    if [[ -f $WORK/channel/report.json ]] && python3 - "$WORK/channel/report.json" "$expected" <<'PY'
import json,sys
r=json.load(open(sys.argv[1])); assert r['originalDials']==1 and r['originalReconnects']==0
sys.exit(0 if r['phase']==sys.argv[2] else 1)
PY
    then cp -- "$WORK/channel/report.json" "$WORK/$expected.json"; return 0; fi
    sleep 1
  done
  die_joint 'phase did not receive fresh real control ACK and original TCP proof'
}
signal_phase() { printf '%s\n' "$1" > "$WORK/channel/phase.new"; mv -T -- "$WORK/channel/phase.new" "$WORK/channel/phase"; }
finish_phase() {
  local phase=$1
  gate false
  dc exec -T server curl --fail --silent --max-time 5 http://127.0.0.1:8080/api/health >/dev/null
  signal_phase "$phase-complete"
  wait_phase "$phase-complete"
  grep -qx 'stop:caddy,server:gate=true' "$WORK/$phase.trace"
  grep -qx 'start:caddy,server:gate=false' "$WORK/$phase.trace"
  printf 'JOINT_PHASE_PASS=%s gate_released=true original_services_healthy=true original_tcp_and_gost_unchanged=true\n' "$phase"
}
verify_bundles() {
  local phase=$1 archive number=0 destination volume
  for archive in "$WORK/backups"/*.tar.gz; do
    [[ -f $archive ]] || continue
    "$ASSETS/bin/msboost-restore" disaster verify --archive "$archive" >/dev/null
    destination="$WORK/verified-$phase-$number"; number=$((number+1))
    "$ASSETS/bin/msboost-restore" disaster unpack --archive "$archive" --dir "$destination" >/dev/null
    cmp -- "$WORK/site/.env" "$destination/site.env"
    [[ $(head -c 5 "$destination/database.dump") == PGDMP && -s $destination/state.msb && ! -e $destination/backup-pause.token ]]
    # Parse/decompress the entire real custom dump offline, without replaying
    # its SQL or connecting to either the live or a replacement database.
    local_docker run --rm --label "msboost.joint=$JOINT_PROJECT" --network none --read-only --user 0:0 --cap-drop ALL --mount "type=bind,source=$destination/database.dump,target=/snapshot.dump,readonly" --entrypoint pg_restore "$POSTGRES_IMAGE" --exit-on-error --file=/dev/null /snapshot.dump
    MSBOOST_JOINT_FIXTURE=verify-export MSBOOST_JOINT_WORK="$WORK" MSBOOST_JOINT_BUNDLE="$destination" "$ASSETS/bin/joint-control.test" -test.run '^TestDisasterJointEncryptedArchive$' -test.v -test.count=1 -test.timeout=1m > "$WORK/export-check-$phase-$number.log" 2>&1
    for volume in app_data caddy_data caddy_config; do
      "$ASSETS/bin/msboost-restore" disaster validate-volume --archive "$destination/$volume.tar" >/dev/null
      [[ $(tar -xOf "$destination/$volume.tar" ./joint-proof) == "$JOINT_PROJECT:$volume" ]]
    done
  done
  case "$phase" in manual) [[ $number == 1 ]] ;; timer) [[ $number == 2 ]] ;; uploadfail) [[ $number == 3 ]] ;; *) return 2 ;; esac
}
wait_phase ready
signal_phase manual-running; wait_phase manual-running
bash "$SCRIPT" phase "$WORK" manual </dev/null > "$WORK/manual.log" 2>&1
verify_bundles manual
finish_phase manual
signal_phase timer-running; wait_phase timer-running
TIMER="$JOINT_PROJECT-backup"
[[ $(systemctl show "$TIMER.service" -p LoadState --value) == not-found && $(systemctl show "$TIMER.timer" -p LoadState --value) == not-found ]]
TIMER_CREATED=1
systemd-run --unit "$TIMER" --description "$JOINT_PROJECT strict full-backup fixture" --on-active=2s --timer-property=AccuracySec=1s --timer-property="Description=$JOINT_PROJECT strict full-backup fixture timer" --property=Type=oneshot --property=RemainAfterExit=yes --property=TimeoutStartSec=8min --property=StandardInput=null --property="Environment=MSBOOST_JOINT_VALIDATION=1" /bin/bash "$SCRIPT" phase "$WORK" timer > "$WORK/timer-create.log" 2>&1
timer_done=0
for ((i=0;i<480;i++)); do
  state=$(systemctl show "$TIMER.service" -p ActiveState --value)
  if [[ $state == active ]]; then timer_done=1; break; fi
  [[ $state != failed ]] || break
  [[ $(local_docker inspect --format '{{.State.Running}}' "$DATA_CONTAINER") == true ]]
  sleep 1
done
journalctl -u "$TIMER.service" --no-pager > "$WORK/timer.log"
[[ $timer_done == 1 && $(systemctl show "$TIMER.service" -p Result --value) == success && $(systemctl show "$TIMER.service" -p ExecMainStatus --value) == 0 ]]
verify_bundles timer
finish_phase timer
for phase in dumpfail packfail uploadfail; do
  signal_phase "$phase-running"; wait_phase "$phase-running"
  if [[ $phase == packfail ]]; then
    date -u +%Y%m%dT%H%M%SZ > "$WORK/collision-time"; openssl rand -hex 8 > "$WORK/collision-nonce"
    collision="$WORK/backups/msboost-disaster-$(<"$WORK/collision-time")-$(<"$WORK/collision-nonce").tar.gz"
    printf 'owned-pack-collision\n' > "$collision"
  fi
  result=0
  if [[ $phase == uploadfail ]]; then
    # No external route/interface is present in the upload process namespace.
    unshare --net -- /bin/bash "$SCRIPT" upload-netns "$WORK" </dev/null > "$WORK/$phase.log" 2>&1 || result=$?
  else bash "$SCRIPT" phase "$WORK" "$phase" </dev/null > "$WORK/$phase.log" 2>&1 || result=$?; fi
  [[ $result != 0 ]]
  finish_phase "$phase"
  case "$phase" in
    dumpfail) [[ $(stat -c %s "$(<"$WORK/dumpfail.work")/database.dump") == 1024 ]]; ! grep -q '^state-export:' "$WORK/$phase.trace" ;;
    packfail) [[ $(<"$collision") == owned-pack-collision ]]; rm -- "$collision" ;;
    uploadfail) [[ -f $WORK/sftp-used && -f $WORK/sftp-create-denied && $(<"$WORK/sftp-create-denied") == open-write-create-excl-permission-denied ]]; grep -Fq '[disaster/upload]' "$WORK/$phase.log"; verify_bundles uploadfail ;;
  esac
done
signal_phase finish
[[ $(local_docker wait "$DATA_CONTAINER") == 0 ]]
local_docker logs "$DATA_CONTAINER" > "$WORK/data-plane.log" 2>&1
grep -q 'JOINT_DATA_PHASE=finish' "$WORK/data-plane.log"
printf 'JOINT_FULL_ARCHIVE_SUITE_PASS T03=true T04=true T26=true original_dials=1 original_reconnects=0 actual_systemd_timer=true actual_pg_dump=true actual_encrypted_export=true actual_original_volume_tars=true actual_archive_verify=true\n'
