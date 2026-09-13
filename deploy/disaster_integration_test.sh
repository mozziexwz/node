#!/usr/bin/env bash
# Real Docker/PostgreSQL disaster cycle, ONLY on an empty dedicated GitHub runner.
# Invocation: sudo env GITHUB_ACTIONS=true MSBOOST_CI_DISASTER=1 PATH="$PATH" \
#   bash deploy/disaster_integration_test.sh
# No production host, remote SFTP, public listener, payment or SSH task is used.
set -Eeuo pipefail
umask 077
[[ ${GITHUB_ACTIONS:-} == true && ${MSBOOST_CI_DISASTER:-} == 1 && $(id -u) == 0 && $(uname -s) == Linux ]] || {
  printf '%s\n' 'REFUSED: requires root Linux GitHub Actions and explicit MSBOOST_CI_DISASTER=1.' >&2
  exit 2
}
CI_REPO=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
for dependency in docker curl jq tar sha256sum flock stat; do command -v "$dependency" >/dev/null || { printf 'Missing dependency: %s\n' "$dependency" >&2; exit 1; }; done
printf '%s\n' 'CI_DISASTER_STAGE=initial-directory-metadata'
stat --printf='CI_DISASTER_PATH %n %u:%g:%a\n' -- / /opt /root
source "$CI_REPO/deploy/manage.sh"
source "$CI_REPO/deploy/disaster.sh"
[[ $INSTALL_ROOT == /opt/msboost && ! -e $INSTALL_ROOT && ! -L $INSTALL_ROOT && ! -L /opt && $(realpath -m /opt) == /opt ]] || { die 'CI refuses an existing or redirected installation'; exit 1; }
for existing in /usr/local/bin/msboost /etc/systemd/system/msboost-disaster-backup.service /etc/systemd/system/msboost-disaster-backup.timer; do
  [[ ! -e $existing && ! -L $existing ]] || { die 'CI refuses an existing launcher or timer'; exit 1; }
done
docker info >/dev/null
[[ -z $(docker ps -aq --filter label=com.docker.compose.project=msboost) ]] || { die 'CI found existing MSBOOST containers'; exit 1; }
for volume in msboost_app_data msboost_database_data msboost_caddy_data msboost_caddy_config; do
  if docker volume inspect "$volume" >/dev/null 2>&1; then die 'CI found an existing MSBOOST volume'; exit 1; fi
done
if docker network inspect msboost_control >/dev/null 2>&1; then die 'CI found an existing MSBOOST network'; exit 1; fi

CI_ROOT=$(mktemp -d /root/msboost-disaster-ci.XXXXXXXX)
CI_TOKEN="ci-$(random_hex 16)"
CI_MARKER="MSBOOST_CI_DISASTER_V1:$CI_TOKEN"
printf '%s\n' "$CI_MARKER" > "$CI_ROOT/.ci-owner"
CI_OVERRIDE="$CI_ROOT/compose-ci.yml"
CI_TOOL="$CI_ROOT/msboost-restore"
CI_ARCHIVES="$CI_ROOT/archives"
CI_BUILD_TAG="msboost-local:$VERSION"
CI_IMAGE= CI_IMAGE_ID= CI_TOOL_CONTAINER=
CI_OPT_IDENTITY= CI_OPT_ORIGINAL_MODE= CI_OPT_CHANGED=0
CI_WORKS=()
declare -A CI_CADDY_METADATA=()
DISASTER_WORK= DISASTER_TOOL_DIR= DISASTER_TOOL_CONTAINER= DISASTER_RESUME=0
DISASTER_RUNNING=()

# Every resource this test creates carries a random ownership label. Reuse the
# normal local Docker socket, and never inherit a remote Docker context.
ci_docker() { (unset DOCKER_HOST DOCKER_CONTEXT DOCKER_DEFAULT_PLATFORM; command docker --host unix:///var/run/docker.sock "$@"); }
docker() {
  if [[ ${1:-} == volume && ${2:-} == create ]]; then
    [[ ${*: -1} =~ ^msboost_(app_data|database_data|caddy_data|caddy_config)$ ]] || return 1
    ci_docker volume create --label "msboost.ci.disaster=$CI_TOKEN" "${@:3}"
  elif [[ ${1:-} == run ]]; then
    ci_docker run --label "msboost.ci.disaster=$CI_TOKEN" "${@:2}"
  else ci_docker "$@"; fi
}
ci_assert_owner() {
  [[ $INSTALL_ROOT == /opt/msboost && ! -L /opt && ! -L $INSTALL_ROOT && $(realpath -m "$INSTALL_ROOT") == /opt/msboost &&
     -f $INSTALL_ROOT/.ci-disaster-owner && ! -L $INSTALL_ROOT/.ci-disaster-owner && $(<"$INSTALL_ROOT/.ci-disaster-owner") == "$CI_MARKER" ]]
}
ci_remove_site() {
  local volume label container
  if [[ -e $INSTALL_ROOT || -L $INSTALL_ROOT ]]; then ci_assert_owner || { die 'CI cleanup refuses an installation without its exact marker'; return 1; }; fi
  for container in $(docker ps -aq --filter label=com.docker.compose.project=msboost); do
    [[ $(docker inspect --format '{{index .Config.Labels "msboost.ci.disaster"}}' "$container") == "$CI_TOKEN" ]] || { die 'CI cleanup refuses a foreign container'; return 1; }
  done
  for container in $(docker ps -aq --filter "label=msboost.ci.disaster=$CI_TOKEN"); do
    [[ $(docker inspect --format '{{index .Config.Labels "msboost.ci.disaster"}}' "$container") == "$CI_TOKEN" ]] || return 1
    docker rm -f "$container" >/dev/null || return
  done
  if docker network inspect msboost_control >/dev/null 2>&1; then
    [[ $(docker network inspect --format '{{index .Labels "msboost.ci.disaster"}}' msboost_control) == "$CI_TOKEN" ]] || return 1
    docker network rm msboost_control >/dev/null || return
  fi
  for volume in msboost_app_data msboost_database_data msboost_caddy_data msboost_caddy_config; do
    if docker volume inspect "$volume" >/dev/null 2>&1; then
      label=$(docker volume inspect --format '{{index .Labels "msboost.ci.disaster"}}' "$volume")
      [[ $label == "$CI_TOKEN" && -z $(docker ps -aq --filter "volume=$volume") ]] || { die 'CI cleanup refuses an unowned or referenced volume'; return 1; }
      docker volume rm "$volume" >/dev/null || return
    fi
  done
  if [[ -d $INSTALL_ROOT ]]; then ci_assert_owner || return; rm -rf -- /opt/msboost || return; fi
}
ci_prepare_opt() {
  local mode identity
  [[ -d /opt && ! -L /opt && $(stat -c '%u:%g' -- /opt) == 0:0 ]] || { die 'CI refuses an unexpected /opt directory owner or type'; return 1; }
  identity=$(stat -c '%d:%i' -- /opt) || return
  mode=$(stat -c '%a' -- /opt) || return
  [[ $identity =~ ^[0-9]+:[0-9]+$ && $mode =~ ^[0-7]{3}$ ]] || { die 'CI refuses unknown /opt metadata'; return 1; }
  CI_OPT_IDENTITY=$identity; CI_OPT_ORIGINAL_MODE=$mode
  if [[ $mode == 777 ]]; then
    # This exact runner-image condition is known and verified above. Record
    # intent before chmod so an ordinary signal cannot lose the restore state.
    CI_OPT_CHANGED=1
    chmod 0755 -- /opt || return
    [[ ! -L /opt && $(stat -c '%d:%i:%u:%g:%a' -- /opt) == "$CI_OPT_IDENTITY:0:0:755" ]] || { die 'CI /opt metadata changed during preparation'; return 1; }
    printf '%s\n' 'CI_DISASTER_STAGE=temporary-opt-mode-755'
  else
    (( (8#$mode & 0022) == 0 )) || { die 'CI refuses an unknown writable /opt state'; return 1; }
    printf '%s\n' 'CI_DISASTER_STAGE=opt-mode-already-safe'
  fi
  stat --printf='CI_DISASTER_PATH %n %u:%g:%a\n' -- /opt
}
ci_restore_opt() {
  [[ $CI_OPT_CHANGED == 1 ]] || return 0
  local mode
  [[ $CI_OPT_ORIGINAL_MODE == 777 && -d /opt && ! -L /opt && $(stat -c '%d:%i:%u:%g' -- /opt) == "$CI_OPT_IDENTITY:0:0" ]] || { die 'CI refuses to restore externally replaced /opt metadata'; return 1; }
  mode=$(stat -c '%a' -- /opt) || return
  if [[ $mode == 755 ]]; then
    chmod 0777 -- /opt || return
  elif [[ $mode != 777 ]]; then
    die 'CI refuses to overwrite an external /opt permission change'; return 1
  fi
  # 777 already means no mutation happened (e.g. an interrupt before chmod),
  # or the original mode was restored. Never change a third-party mode.
  [[ ! -L /opt && $(stat -c '%d:%i:%u:%g:%a' -- /opt) == "$CI_OPT_IDENTITY:0:0:777" ]] || { die 'CI could not verify restored /opt metadata'; return 1; }
  CI_OPT_CHANGED=0
  printf '%s\n' 'CI_DISASTER_OPT_MODE_RESTORED=1'
  stat --printf='CI_DISASTER_PATH %n %u:%g:%a\n' -- /opt
}
ci_cleanup() {
  local status=$? work
  trap - EXIT
  # Finish the narrow permission rollback during ordinary signals. SIGKILL or
  # runner loss cannot be recovered by a shell trap; this is a disposable CI VM.
  trap '' HUP INT TERM
  [[ -f $CI_ROOT/.ci-owner && ! -L $CI_ROOT/.ci-owner && $(<"$CI_ROOT/.ci-owner") == "$CI_MARKER" ]] || { ci_restore_opt || true; exit 1; }
  ci_remove_site || status=1
  ci_restore_opt || status=1
  if [[ $CI_TOOL_CONTAINER =~ ^[a-f0-9]{64}$ ]] && docker inspect "$CI_TOOL_CONTAINER" >/dev/null 2>&1; then
    [[ $(docker inspect --format '{{index .Config.Labels "msboost.ci.disaster"}}' "$CI_TOOL_CONTAINER") == "$CI_TOKEN" ]] && docker rm -f "$CI_TOOL_CONTAINER" >/dev/null || status=1
  fi
  for work in "${CI_WORKS[@]}" "${DISASTER_WORK:-}"; do
    [[ -n $work ]] || continue
    if [[ -e $work || -L $work ]]; then
      [[ $work =~ ^/root/msboost-disaster-work\.[A-Za-z0-9]{8}$ && -d $work && ! -L $work && $(realpath -m "$work") == "$work" && $(stat -c '%u:%a' "$work") == 0:700 ]] || { status=1; continue; }
      rm -rf -- "$work" || status=1
    fi
  done
  # Only tags created by this invocation are eligible for removal; never prune.
  for work in "$CI_IMAGE" "$CI_BUILD_TAG"; do
    [[ -n $work ]] || continue
    if docker image inspect "$work" >/dev/null 2>&1; then
      [[ $(docker image inspect --format '{{index .Config.Labels "msboost.ci.disaster"}}' "$work") == "$CI_TOKEN" ]] && docker image rm "$work" >/dev/null || status=1
    fi
  done
  [[ $CI_ROOT =~ ^/root/msboost-disaster-ci\.[A-Za-z0-9]{8}$ && ! -L $CI_ROOT && $(realpath -m "$CI_ROOT") == "$CI_ROOT" ]] || exit 1
  rm -rf -- "$CI_ROOT" || status=1
  printf 'CI_DISASTER_TEMP_RESOURCES_REMOVED=%s\n' "$([[ $status == 0 ]] && printf 1 || printf 'checked-after-failure')"
  exit "$status"
}
trap ci_cleanup EXIT
trap 'exit 143' HUP INT TERM
ci_prepare_opt

# Attach the test marker immediately when the real restore creates its root,
# including failures before it manages to copy Compose or the environment.
install() {
  command install "$@" || return
  if [[ " $* " == *' -d '* ]]; then
    local item
    for item in "$@"; do
      if [[ $item == /opt/msboost ]]; then
        [[ ! -L /opt && ! -L /opt/msboost && -d /opt/msboost ]] || return 1
        printf '%s\n' "$CI_MARKER" > /opt/msboost/.ci-disaster-owner
      fi
    done
  fi
}

# Only loopback gets a dynamic HTTP port. No host database port is published,
# so the separate CI PostgreSQL service on 5432 is untouched.
printf '%s\n' \
  'services:' \
  '  server:' '    labels:' "      msboost.ci.disaster: $CI_TOKEN" \
  '  database:' '    labels:' "      msboost.ci.disaster: $CI_TOKEN" \
  '  caddy:' '    ports: !override ["127.0.0.1::80"]' '    labels:' "      msboost.ci.disaster: $CI_TOKEN" \
  'networks:' '  control:' '    labels:' "      msboost.ci.disaster: $CI_TOKEN" \
  'volumes:' \
  '  app_data:' '    labels:' "      msboost.ci.disaster: $CI_TOKEN" \
  '  database_data:' '    labels:' "      msboost.ci.disaster: $CI_TOKEN" \
  '  caddy_data:' '    labels:' "      msboost.ci.disaster: $CI_TOKEN" \
  '  caddy_config:' '    labels:' "      msboost.ci.disaster: $CI_TOKEN" > "$CI_OVERRIDE"
compose_live() {
  ( unset MSBOOST_IMAGE MSBOOST_VERSION MSBOOST_DOMAIN MSBOOST_SITE_ADDRESS MSBOOST_DATABASE_NAME POSTGRES_PASSWORD POSTGRES_IMAGE CADDY_IMAGE
    export MSBOOST_ENV_FILE="$INSTALL_ROOT/.env"
    docker compose --project-name msboost --env-file "$INSTALL_ROOT/.env" -f "$INSTALL_ROOT/deploy/compose.yml" -f "$CI_OVERRIDE" "$@" )
}
require_platform() { :; } # The CI runner is Ubuntu; product remains Debian 12.
ensure_docker() { docker info >/dev/null; }
install_launcher() { :; } # Never install a system-wide command during CI.
read_tty() { [[ $1 == *RESTORE_NEW_MSBOOST* ]] || return 1; printf RESTORE_NEW_MSBOOST; }
disaster_tool() { [[ -x $CI_TOOL && ! -L $CI_TOOL ]] || return 1; DISASTER_TOOL=$CI_TOOL; }
check_frontend() {
  local bound
  bound=$(compose_live port caddy 80)
  [[ $bound =~ ^127\.0\.0\.1:[0-9]+$ ]] || { die 'CI proxy must bind loopback only'; return 1; }
  curl --fail --silent --show-error --noproxy '*' --retry 10 --retry-delay 1 --retry-all-errors "http://$bound/api/health" > "$CI_ROOT/health.json" || return
  jq -e '.status == "ok"' "$CI_ROOT/health.json" >/dev/null
}

note 'CI: build current source and pin dependency images; no release is published.'
if docker image inspect "$CI_BUILD_TAG" >/dev/null 2>&1; then die 'CI refuses to overwrite an existing local build tag'; exit 1; fi
docker build --build-arg "VERSION=$VERSION" --label "msboost.ci.disaster=$CI_TOKEN" --tag "$CI_BUILD_TAG" "$CI_REPO"
CI_IMAGE_ID=$(server_identity "$CI_BUILD_TAG")
CI_IMAGE="msboost-release:$VERSION-$(platform_arch)-${CI_IMAGE_ID:7:12}"
docker tag "$CI_BUILD_TAG" "$CI_IMAGE"
# A release-shaped local alias exercises the unmodified production allowlist.
# Only the artifact resolver is overridden: it accepts this invocation's exact
# locally built image ID, never an arbitrary image or a public moving tag.
load_release_image() {
  [[ $1 == "$CI_IMAGE_ID" && $(server_identity "$CI_IMAGE") == "$CI_IMAGE_ID" ]] || return 1
  env_set "$STAGE/.env" MSBOOST_IMAGE "$CI_IMAGE"
}
docker pull postgres:17-bookworm >/dev/null
docker pull caddy:2-alpine >/dev/null
CI_TOOL_CONTAINER=$(docker create --network none --label "msboost.ci.disaster=$CI_TOKEN" --entrypoint /bin/true "$CI_IMAGE")
[[ $CI_TOOL_CONTAINER =~ ^[a-f0-9]{64}$ ]]
docker cp "$CI_TOOL_CONTAINER:/usr/local/bin/msboost-restore" "$CI_TOOL"
docker rm "$CI_TOOL_CONTAINER" >/dev/null; CI_TOOL_CONTAINER=
chmod 700 "$CI_TOOL"

install -d -m 700 "$INSTALL_ROOT"
printf '%s\n' "$MARKER" > "$INSTALL_ROOT/.managed-by-msboost"
SOURCE_DIR=$CI_REPO
copy_deployment_files "$SOURCE_DIR" "$INSTALL_ROOT"
DOMAIN= IP_ADDRESS=127.0.0.1 ADMIN_EMAIL=123456789@qq.com
write_initial_environment
env_set "$INSTALL_ROOT/.env" MSBOOST_IMAGE "$CI_IMAGE"
STAGE=$(mktemp -d "$INSTALL_ROOT/.stage.XXXXXXXX")
install -m 600 "$INSTALL_ROOT/.env" "$STAGE/.env"
freeze_images
replace_live_environment "$STAGE/.env"
cleanup_stage; STAGE=
start_live
POSTGRES_TEST_IMAGE=$(env_get "$INSTALL_ROOT/.env" POSTGRES_IMAGE)
CI_KEY_BEFORE=$(env_get "$INSTALL_ROOT/.env" MASTER_KEY)
[[ -n $CI_KEY_BEFORE ]]

# Seed only synthetic state while the test app is quiesced. No business state,
# password, token or full database JSON is printed to the CI log.
compose_live stop --timeout 30 caddy server >/dev/null
compose_live exec -T database psql --username=msboost --dbname=msboost -v ON_ERROR_STOP=1 >/dev/null <<'SQL'
WITH source AS (SELECT payload::jsonb AS state FROM control_state WHERE id=1),
admin AS (SELECT account.key FROM source CROSS JOIN LATERAL jsonb_each(source.state->'users') AS account WHERE account.value->>'role'='admin' LIMIT 1)
UPDATE control_state SET payload=jsonb_set(
  jsonb_set(
    jsonb_set(source.state, '{settings}', (source.state->'settings') ||
      '{"ciDisasterProof":"before-snapshot","maintenance":false,"paidCreate":true,"planSale":true,"cards":true,"paywx":true,"payali":true}'::jsonb),
    '{docs}', (source.state->'docs') ||
      '{"articles":{"ci-article":{"id":"ci-article","title":"CI disaster proof","body":"before-snapshot","category":"test","published":true,"attachments":[{"id":"ci-attachment","name":"proof.txt","size":5,"type":"text/plain"}]}},"attachments":{"ci-attachment":{"articleId":"ci-article","metadata":{"id":"ci-attachment","name":"proof.txt","size":5,"type":"text/plain"},"bytes":"cHJvb2Y="}},"payment_channels":{"ci-channel":{"id":"ci-channel","enabled":true}}}'::jsonb),
  ARRAY['users',admin.key,'balanceCents'], '54321'::jsonb)::text,
  revision=revision+1 FROM source, admin WHERE control_state.id=1;
SQL
CI_USERS_BEFORE=$(compose_live exec -T database psql --username=msboost --dbname=msboost -tAX -c "SELECT md5((payload::jsonb->'users')::text) FROM control_state WHERE id=1")
[[ $CI_USERS_BEFORE =~ ^[a-f0-9]{32}$ ]]
for volume in app_data caddy_data caddy_config; do
  docker run --rm --network none --read-only --user 0:0 --entrypoint sh --mount "type=volume,source=msboost_$volume,target=/proof" "$POSTGRES_TEST_IMAGE" \
    -c 'umask 077; printf "%s\n" "MSBOOST isolated volume proof" > /proof/ci-proof.txt; chmod 600 /proof/ci-proof.txt; chown 10001:10001 /proof/ci-proof.txt'
done
for volume in caddy_data caddy_config; do
  metadata=$(docker run --rm --network none --read-only --user 0:0 --entrypoint stat --mount "type=volume,source=msboost_$volume,target=/proof,readonly" "$POSTGRES_TEST_IMAGE" -c '%u:%g:%a' -- /proof/caddy)
  [[ $metadata =~ ^[0-9]+:[0-9]+:[0-7]{3,4}$ ]]
  CI_CADDY_METADATA[$volume]=$metadata
  printf 'CI_DISASTER_CADDY_METADATA_BEFORE %s %s\n' "$volume" "$metadata"
done
disaster_resume_services caddy server >/dev/null
printf '%s\n' 'CI_DISASTER_STAGE=config-directory-metadata'
stat --printf='CI_DISASTER_PATH %n %u:%g:%a\n' -- "$INSTALL_ROOT" "$CI_ROOT"
printf '%s\n' 'CI_DISASTER_STAGE=prepare-install-directory'
"$CI_TOOL" disaster prepare-dir --dir "$INSTALL_ROOT"
printf '%s\n' 'CI_DISASTER_STAGE=prepare-archive-directory'
"$CI_TOOL" disaster prepare-dir --dir "$CI_ARCHIVES"
printf '%s\n' 'CI_DISASTER_STAGE=config-save'
"$CI_TOOL" disaster config-save --file "$INSTALL_ROOT/disaster.json" --local-dir "$CI_ARCHIVES" --retention-days 30 --time 02:30 </dev/null
printf '%s\n' 'CI_DISASTER_STAGE=config-saved'
note 'CI: real stop, pg_dump, encrypted export, volume archives, pack and verification.'
disaster_backup
CI_WORKS+=("$DISASTER_WORK")
[[ $(compose_live ps --status running --services | sort | tr '\n' ' ') == 'caddy database server ' ]]
mapfile -t CI_BUNDLES < <(find "$CI_ARCHIVES" -maxdepth 1 -type f -name 'msboost-disaster-*.tar.gz')
[[ ${#CI_BUNDLES[@]} == 1 ]]
CI_ARCHIVE=${CI_BUNDLES[0]}
"$CI_TOOL" disaster verify --archive "$CI_ARCHIVE" >/dev/null
"$CI_TOOL" disaster unpack --archive "$CI_ARCHIVE" --dir "$CI_ROOT/inspection"
[[ $(head -c 5 "$CI_ROOT/inspection/database.dump") == PGDMP && -s $CI_ROOT/inspection/state.msb ]]
cmp "$INSTALL_ROOT/.env" "$CI_ROOT/inspection/site.env"
for volume in app_data caddy_data caddy_config; do "$CI_TOOL" disaster validate-volume --archive "$CI_ROOT/inspection/$volume.tar"; done

# Model complete loss of ONLY this explicitly marked CI site. The verified
# recovery bundle is outside /opt/msboost and survives removal of its volumes.
note 'CI: remove the owned synthetic site and exercise the real cold restore.'
ci_remove_site
[[ ! -e $INSTALL_ROOT && -f $CI_ARCHIVE ]]
DISASTER_ARCHIVE=$CI_ARCHIVE
DISASTER_WORK= DISASTER_RESUME=0 STAGE=
disaster_restore
[[ ! -e $INSTALL_ROOT/.disaster-incomplete && ! -L $INSTALL_ROOT/.disaster-incomplete ]]
[[ $(env_get "$INSTALL_ROOT/.env" MASTER_KEY) == "$CI_KEY_BEFORE" ]]
CI_DATABASE=$(env_get "$INSTALL_ROOT/.env" MSBOOST_DATABASE_NAME)
[[ $CI_DATABASE =~ ^msboost_restore_[a-z0-9_]+$ && $CI_DATABASE != msboost ]]
CI_USERS_AFTER=$(compose_live exec -T database psql --username=msboost --dbname="$CI_DATABASE" -tAX -c "SELECT md5((payload::jsonb->'users')::text) FROM control_state WHERE id=1")
[[ $CI_USERS_BEFORE == "$CI_USERS_AFTER" ]]
CI_STATE_OK=$(compose_live exec -T database psql --username=msboost --dbname="$CI_DATABASE" -tAX -v ON_ERROR_STOP=1 <<'SQL'
SELECT
  payload::jsonb #>> '{settings,ciDisasterProof}' = 'before-snapshot'
  AND payload::jsonb #>> '{settings,maintenance}' = 'true'
  AND payload::jsonb #>> '{settings,paidCreate}' = 'false'
  AND payload::jsonb #>> '{settings,planSale}' = 'false'
  AND payload::jsonb #>> '{settings,cards}' = 'false'
  AND payload::jsonb #>> '{settings,paywx}' = 'false'
  AND payload::jsonb #>> '{settings,payali}' = 'false'
  AND payload::jsonb #>> '{docs,articles,ci-article,body}' = 'before-snapshot'
  AND payload::jsonb #>> '{docs,articles,ci-article,attachments,0,id}' = 'ci-attachment'
  AND payload::jsonb #>> '{docs,attachments,ci-attachment,articleId}' = 'ci-article'
  AND payload::jsonb #>> '{docs,attachments,ci-attachment,bytes}' = 'cHJvb2Y='
  AND payload::jsonb #>> '{docs,payment_channels,ci-channel,enabled}' = 'false'
  AND payload::jsonb->'sessions' = '{}'::jsonb
  AND EXISTS (SELECT 1 FROM jsonb_each(payload::jsonb->'users') WHERE value->>'balanceCents' = '54321')
FROM control_state WHERE id=1;
SQL
)
[[ $CI_STATE_OK == t ]]
[[ $(compose_live exec -T database psql --username=msboost --dbname=msboost -tAX -c "SELECT to_regclass('public.control_state') IS NULL") == t ]]
for volume in app_data caddy_data caddy_config; do
  proof=$(docker run --rm --network none --read-only --user 0:0 --entrypoint sh --mount "type=volume,source=msboost_$volume,target=/proof,readonly" "$POSTGRES_TEST_IMAGE" -c 'cat /proof/ci-proof.txt; stat -c "%u:%g:%a" /proof/ci-proof.txt')
  [[ $proof == $'MSBOOST isolated volume proof\n10001:10001:600' ]]
done
for volume in caddy_data caddy_config; do
  metadata=$(docker run --rm --network none --read-only --user 0:0 --entrypoint stat --mount "type=volume,source=msboost_$volume,target=/proof,readonly" "$POSTGRES_TEST_IMAGE" -c '%u:%g:%a' -- /proof/caddy)
  [[ $metadata == "${CI_CADDY_METADATA[$volume]}" ]]
  printf 'CI_DISASTER_CADDY_METADATA_RESTORED %s %s\n' "$volume" "$metadata"
done
check_frontend
bound=$(compose_live port caddy 80)
curl --fail --silent --show-error --noproxy '*' "http://$bound/api/settings" > "$CI_ROOT/restored-public-settings.json"
jq -e '.maintenance == true and .paywx == false and .payali == false and .planSale == false' "$CI_ROOT/restored-public-settings.json" >/dev/null
printf '%s\n' 'PASS: real Docker cold recovery preserved user identity/balance, master key, article/attachment and three volume files; restored a NEW PostgreSQL DB; maintenance on and payments off.'
