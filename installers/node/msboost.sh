#!/usr/bin/env bash
# msboost unattended installer for common IPv4 systemd VPS distributions.
# Usage: sudo bash msboost.sh [username] [password] [tcp_port]
# Standalone use reuses omitted values. The managed fresh workflow sets
# MSBOOST_FORCE_FRESH=1 so prior credentials and port cannot be inherited.

set -Eeuo pipefail
umask 077
export LC_ALL=C
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

readonly APP_USER="msboost"
readonly APP_GROUP="msboost"
readonly APP_DIR="/etc/msboost"
readonly STATE_DIR="/var/lib/msboost"
readonly BIN="/usr/local/bin/msboost"
readonly UNIT="/etc/systemd/system/msboost.service"
readonly SERVICE="msboost.service"
readonly STATE_FILE="${APP_DIR}/install.env"
readonly CLIENT_CONFIG="/root/直连.json"
readonly VERSION="${MSBOOST_ENGINE_VERSION:-v1.19.30}"
readonly RULE_URL="https://raw.githubusercontent.com/MetaCubeX/meta-rules-dat/meta/geo/geosite/gfw.mrs"
readonly TIMEZONE="Asia/Shanghai"
readonly LOCK_FILE="/run/msboost-installer.lock"
readonly STAGE_ROOT="/usr/local/libexec/msboost-installer"
readonly BACKUP_ROOT="/var/backups/msboost"
readonly MANAGED_MARKER="msboost-installer-v1"

STAGE_DIR=""
BACKUP_DIR=""
TEST_PID=""
TX_ACTIVE=0
COMMITTED=0
LOCK_HELD=0
CREATED_USER=0
CREATED_GROUP=0
MANAGED_INSTALL=0
OLD_BIN=0
OLD_APP=0
OLD_STATE=0
OLD_UNIT=0
OLD_CLIENT=0
OLD_ACTIVE=0
OLD_ENABLE_STATE="missing"
FW_UFW_ADDED=0
FW_FIREWALLD_RUNTIME_ADDED=0
FW_FIREWALLD_PERMANENT_ADDED=0
FW_FIREWALLD_ZONE=""
OLD_FW_PORT=""
OLD_FW_UFW_OWNED=0
OLD_FW_FIREWALLD_RUNTIME_OWNED=0
OLD_FW_FIREWALLD_PERMANENT_OWNED=0
OLD_FW_FIREWALLD_ZONE=""
NEW_FW_UFW_OWNED=0
NEW_FW_FIREWALLD_RUNTIME_OWNED=0
NEW_FW_FIREWALLD_PERMANENT_OWNED=0
OLD_FW_UFW_REMOVED=0
OLD_FW_FIREWALLD_RUNTIME_REMOVED=0
OLD_FW_FIREWALLD_PERMANENT_REMOVED=0
OLD_FW_FIREWALLD_PERMANENT_REMOVED_OFFLINE=0
TIME_STATUS="not checked"
FIREWALL_STATUS="not checked"
ROLLBACK_FAILED=0

fail() {
  echo "ERROR: $*" >&2
  exit 1
}

note() {
  echo "==> $*"
}

warn() {
  echo "WARNING: $*" >&2
}

rollback_problem() {
  warn "Rollback step failed: $*"
  ROLLBACK_FAILED=1
  return 0
}

cleanup_test() {
  if [[ -n "${TEST_PID}" ]]; then
    kill "${TEST_PID}" 2>/dev/null || true
    wait "${TEST_PID}" 2>/dev/null || true
    TEST_PID=""
  fi
}

rollback_firewall() {
  if (( FW_UFW_ADDED )); then
    ufw --force delete allow "${PORT}/tcp" >/dev/null 2>&1 || \
      rollback_problem "could not remove the newly added UFW rule for TCP/${PORT}."
  fi
  if (( FW_FIREWALLD_RUNTIME_ADDED )); then
    firewall-cmd --quiet --zone="${FW_FIREWALLD_ZONE}" --remove-port="${PORT}/tcp" >/dev/null 2>&1 || \
      rollback_problem "could not remove the new runtime firewalld rule for TCP/${PORT}."
  fi
  if (( FW_FIREWALLD_PERMANENT_ADDED )); then
    firewall-cmd --quiet --permanent --zone="${FW_FIREWALLD_ZONE}" --remove-port="${PORT}/tcp" >/dev/null 2>&1 || \
      rollback_problem "could not remove the new permanent firewalld rule for TCP/${PORT}."
  fi
  if (( OLD_FW_UFW_REMOVED )); then
    ufw allow "${OLD_FW_PORT}/tcp" >/dev/null 2>&1 || \
      rollback_problem "could not restore the prior UFW rule for TCP/${OLD_FW_PORT}."
  fi
  if (( OLD_FW_FIREWALLD_RUNTIME_REMOVED )); then
    firewall-cmd --quiet --zone="${OLD_FW_FIREWALLD_ZONE}" --add-port="${OLD_FW_PORT}/tcp" >/dev/null 2>&1 || \
      rollback_problem "could not restore the prior runtime firewalld rule for TCP/${OLD_FW_PORT}."
  fi
  if (( OLD_FW_FIREWALLD_PERMANENT_REMOVED )); then
    firewall-cmd --quiet --permanent --zone="${OLD_FW_FIREWALLD_ZONE}" --add-port="${OLD_FW_PORT}/tcp" >/dev/null 2>&1 || \
      rollback_problem "could not restore the prior permanent firewalld rule for TCP/${OLD_FW_PORT}."
  fi
  if (( OLD_FW_FIREWALLD_PERMANENT_REMOVED_OFFLINE )); then
    firewall-offline-cmd --zone="${OLD_FW_FIREWALLD_ZONE}" --add-port="${OLD_FW_PORT}/tcp" >/dev/null 2>&1 || \
      rollback_problem "could not restore the prior offline firewalld rule for TCP/${OLD_FW_PORT}."
  fi
}

rollback_install() {
  set +e
  warn "Installation failed; restoring the previous msboost installation."
  if [[ -e "${UNIT}" ]] || systemctl is-active --quiet "${SERVICE}"; then
    systemctl disable --now "${SERVICE}" >/dev/null 2>&1 || \
      rollback_problem "could not stop and disable the failed service deployment."
  fi
  rollback_firewall

  rm -f -- "${BIN}" || rollback_problem "could not remove the new executable."
  rm -rf -- "${APP_DIR}" || rollback_problem "could not remove the new configuration directory."
  rm -rf -- "${STATE_DIR}" || rollback_problem "could not remove the new state directory."
  rm -f -- "${UNIT}" || rollback_problem "could not remove the new systemd unit."
  rm -f -- "${CLIENT_CONFIG}" || rollback_problem "could not remove the new client configuration."

  if (( OLD_BIN )); then
    cp -a -- "${BACKUP_DIR}/bin" "${BIN}" || rollback_problem "could not restore the prior executable."
  fi
  if (( OLD_APP )); then
    cp -a -- "${BACKUP_DIR}/app" "${APP_DIR}" || rollback_problem "could not restore the prior configuration directory."
  fi
  if (( OLD_STATE )); then
    cp -a -- "${BACKUP_DIR}/state" "${STATE_DIR}" || rollback_problem "could not restore the prior state directory."
  fi
  if (( OLD_UNIT )); then
    cp -a -- "${BACKUP_DIR}/unit" "${UNIT}" || rollback_problem "could not restore the prior systemd unit."
  fi
  if (( OLD_CLIENT )); then
    cp -a -- "${BACKUP_DIR}/client.json" "${CLIENT_CONFIG}" || rollback_problem "could not restore the prior client configuration."
  fi

  systemctl daemon-reload >/dev/null 2>&1 || rollback_problem "systemd could not reload restored unit files."
  case "${OLD_ENABLE_STATE}" in
    enabled) systemctl enable "${SERVICE}" >/dev/null 2>&1 || rollback_problem "could not restore the enabled service state." ;;
    enabled-runtime) systemctl enable --runtime "${SERVICE}" >/dev/null 2>&1 || rollback_problem "could not restore the runtime-enabled service state." ;;
    masked) systemctl mask "${SERVICE}" >/dev/null 2>&1 || rollback_problem "could not restore the masked service state." ;;
    masked-runtime) systemctl mask --runtime "${SERVICE}" >/dev/null 2>&1 || rollback_problem "could not restore the runtime-masked service state." ;;
    disabled) systemctl disable "${SERVICE}" >/dev/null 2>&1 || rollback_problem "could not restore the disabled service state." ;;
  esac
  if (( OLD_ACTIVE )); then
    systemctl restart "${SERVICE}" >/dev/null 2>&1 || rollback_problem "could not restart the prior service."
    systemctl is-active --quiet "${SERVICE}" || rollback_problem "the prior service is not active after rollback."
  else
    systemctl stop "${SERVICE}" >/dev/null 2>&1 || true
    if systemctl is-active --quiet "${SERVICE}"; then
      rollback_problem "the service is still active although it was inactive before installation."
    fi
  fi
  if (( CREATED_USER )); then
    userdel "${APP_USER}" >/dev/null 2>&1 || rollback_problem "could not remove the newly created service user."
  fi
  if (( CREATED_GROUP )); then
    groupdel "${APP_GROUP}" >/dev/null 2>&1 || rollback_problem "could not remove the newly created service group."
  fi
  if (( ROLLBACK_FAILED )); then
    warn "Automatic rollback was incomplete; use the retained recovery copy reported below for manual restoration."
  else
    warn "The msboost application transaction was rolled back successfully."
  fi
}

on_exit() {
  local rc=$?
  trap - EXIT INT TERM
  cleanup_test
  if (( rc != 0 && TX_ACTIVE && ! COMMITTED )); then
    rollback_install
  fi
  if [[ -n "${STAGE_DIR}" && -d "${STAGE_DIR}" ]]; then
    rm -rf -- "${STAGE_DIR}"
  fi
  if [[ -n "${BACKUP_DIR}" && -d "${BACKUP_DIR}" ]]; then
    if (( rc == 0 || ! TX_ACTIVE )); then
      rm -rf -- "${BACKUP_DIR}"
    else
      warn "A root-only recovery copy was retained at ${BACKUP_DIR}."
    fi
  fi
  exit "${rc}"
}

trap on_exit EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

[[ ${EUID} -eq 0 ]] || fail "Please run this installer as root."
(( $# <= 3 )) || fail "Usage: sudo bash msboost.sh [username] [password] [tcp_port]"
[[ "${APP_DIR}" == "/etc/msboost" ]] || fail "Internal path safety check failed."
[[ "${STATE_DIR}" == "/var/lib/msboost" ]] || fail "Internal state-path safety check failed."
[[ "${STAGE_ROOT}" == "/usr/local/libexec/msboost-installer" ]] || fail "Internal staging-path safety check failed."
[[ "${BACKUP_ROOT}" == "/var/backups/msboost" ]] || fail "Internal backup-path safety check failed."
[[ "${VERSION}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || fail "Invalid engine version: ${VERSION}"

if command -v flock >/dev/null 2>&1; then
  exec 9>"${LOCK_FILE}"
  flock -n 9 || fail "Another msboost installation is already running."
  LOCK_HELD=1
fi

verify_target_ownership() {
  local path marker="" found=0 key value
  for path in "${BIN}" "${APP_DIR}" "${STATE_DIR}" "${UNIT}" "${CLIENT_CONFIG}"; do
    if [[ -e "${path}" || -L "${path}" ]]; then
      found=1
      break
    fi
  done
  if (( found )); then
    if [[ -f "${STATE_FILE}" ]]; then
      while IFS='=' read -r key value; do
        if [[ "${key}" == "MANAGED_BY" ]]; then
          marker="${value}"
          break
        fi
      done < "${STATE_FILE}"
    fi
    [[ "${marker}" == "${MANAGED_MARKER}" ]] || fail \
      "Existing target paths are not marked as an msboost installation; refusing to overwrite them."
    MANAGED_INSTALL=1
  fi
}

preflight_checks() {
  local supplied_user="${1:-}" supplied_password="${2:-}" supplied_port="${3:-}"

  [[ -d /run/systemd/system ]] || fail "A running systemd instance is required."
  if ! command -v apt-get >/dev/null 2>&1 && \
      ! command -v dnf >/dev/null 2>&1 && \
      ! command -v yum >/dev/null 2>&1; then
    fail "Supported package managers are apt, dnf, and yum."
  fi
  case "$(uname -m)" in
    x86_64|amd64) ASSET_FAMILY="amd64" ;;
    aarch64|arm64) ASSET_FAMILY="arm64" ;;
    *) fail "Unsupported CPU architecture: $(uname -m)" ;;
  esac

  [[ -z "${supplied_user}" || "${supplied_user}" =~ ^[A-Za-z0-9_.-]{1,64}$ ]] || \
    fail "Username may contain only A-Z, a-z, 0-9, dot, underscore, and hyphen."
  [[ -z "${supplied_password}" || "${supplied_password}" =~ ^[A-Za-z0-9_.-]{12,64}$ ]] || \
    fail "Password must be 12-64 characters using A-Z, a-z, 0-9, dot, underscore, or hyphen."
  if [[ -n "${supplied_port}" ]]; then
    [[ "${supplied_port}" =~ ^[0-9]{1,5}$ ]] || fail "Port must contain 1-5 decimal digits."
    supplied_port=$((10#${supplied_port}))
    (( supplied_port >= 1 && supplied_port <= 65535 )) || fail "Port must be in 1-65535."
  fi

  verify_target_ownership
}

preflight_checks "$@"

bootstrap_existing_clock() {
  local unit
  [[ -d /run/systemd/system ]] || return 0
  command -v timedatectl >/dev/null 2>&1 && timedatectl set-ntp true >/dev/null 2>&1 || true
  for unit in chrony.service chronyd.service systemd-timesyncd.service; do
    if systemctl cat "${unit}" >/dev/null 2>&1; then
      systemctl start "${unit}" >/dev/null 2>&1 || true
      break
    fi
  done
  if command -v chronyc >/dev/null 2>&1; then
    chronyc -a online >/dev/null 2>&1 || true
    chronyc -a makestep >/dev/null 2>&1 || true
  fi
}

bootstrap_existing_clock

install_dependencies() {
  note "Installing required packages"
  if command -v apt-get >/dev/null 2>&1; then
    export DEBIAN_FRONTEND=noninteractive
    export NEEDRESTART_MODE=a
    export APT_LISTCHANGES_FRONTEND=none
    apt-get -o DPkg::Lock::Timeout=180 -o Acquire::Retries=5 -o APT::Update::Error-Mode=any update -qq
    apt-get -o DPkg::Lock::Timeout=180 -o Acquire::Retries=5 install -y -qq \
      bash ca-certificates chrony coreutils curl grep gzip iproute2 mawk passwd python3 tzdata util-linux
  elif command -v dnf >/dev/null 2>&1; then
    dnf -y -q makecache --refresh
    dnf -y -q install \
      bash ca-certificates chrony coreutils curl gawk grep gzip iproute python3 shadow-utils tzdata util-linux
  elif command -v yum >/dev/null 2>&1; then
    yum -y -q makecache
    yum -y -q install \
      bash ca-certificates chrony coreutils curl gawk grep gzip iproute python3 shadow-utils tzdata util-linux
  else
    fail "Supported package managers are apt, dnf, and yum."
  fi

  local required
  for required in \
    awk chronyc curl flock getent grep groupadd groupdel gzip id install ip journalctl mktemp od \
    python3 seq sha256sum ss systemctl timedatectl tr uname useradd userdel; do
    command -v "${required}" >/dev/null 2>&1 || fail "Required command is unavailable after package installation: ${required}"
  done
  [[ -d /run/systemd/system ]] || fail "A running systemd instance is required (not merely the systemctl command)."
}

install_dependencies

if (( ! LOCK_HELD )); then
  exec 9>"${LOCK_FILE}"
  flock -n 9 || fail "Another msboost installation is already running."
  LOCK_HELD=1
fi

install -d -m 0700 "${STAGE_ROOT}"
STAGE_DIR="$(mktemp -d "${STAGE_ROOT}/run.XXXXXX")"
install -d -m 0700 "${STAGE_DIR}/app" "${STAGE_DIR}/runtime/ruleset"

is_container() {
  if command -v systemd-detect-virt >/dev/null 2>&1; then
    systemd-detect-virt --quiet --container
    return
  fi
  grep -qaE '(docker|lxc|containerd|kubepods)' /proc/1/cgroup 2>/dev/null
}

verify_clock_over_https() {
  python3 - <<'PY'
import email.utils
import sys
import time
import urllib.request

urls = (
    "https://api.github.com/",
    "https://www.cloudflare.com/",
)
for url in urls:
    try:
        request = urllib.request.Request(
            url,
            method="HEAD",
            headers={"User-Agent": "msboost-installer"},
        )
        with urllib.request.urlopen(request, timeout=10) as response:
            value = response.headers.get("Date")
        if not value:
            continue
        remote = email.utils.parsedate_to_datetime(value).timestamp()
        if abs(time.time() - remote) < 180:
            sys.exit(0)
    except Exception:
        pass
sys.exit(1)
PY
}

configure_time() {
  note "Setting ${TIMEZONE} and synchronizing the system clock with chrony"
  [[ -e "/usr/share/zoneinfo/${TIMEZONE}" ]] || fail "Timezone data for ${TIMEZONE} is unavailable."
  timedatectl set-timezone "${TIMEZONE}" 2>/dev/null || {
    ln -snf "/usr/share/zoneinfo/${TIMEZONE}" /etc/localtime
    printf '%s\n' "${TIMEZONE}" > /etc/timezone
  }

  local chrony_unit=""
  if systemctl list-unit-files --no-legend chrony.service 2>/dev/null | grep -q '^chrony\.service'; then
    chrony_unit="chrony.service"
  elif systemctl list-unit-files --no-legend chronyd.service 2>/dev/null | grep -q '^chronyd\.service'; then
    chrony_unit="chronyd.service"
  fi
  [[ -n "${chrony_unit}" ]] || fail "chrony was installed, but no chrony systemd unit was found."

  if ! systemctl enable --now "${chrony_unit}"; then
    if is_container && verify_clock_over_https; then
      warn "The container cannot control its clock; the host clock is within 180 seconds of trusted HTTPS time."
      TIME_STATUS="host-managed container clock verified over HTTPS"
      return
    fi
    fail "Could not enable and start ${chrony_unit}."
  fi

  chronyc -a online >/dev/null 2>&1 || true
  chronyc -a makestep 0.1 1 >/dev/null 2>&1 || true
  chronyc -a burst 1/2 >/dev/null 2>&1 || true
  if chronyc -a waitsync 30 0.5 0 2 >/dev/null 2>&1; then
    TIME_STATUS="chrony enabled and synchronized"
    note "System clock synchronized"
    return
  fi

  if is_container && verify_clock_over_https; then
    warn "chrony cannot discipline this container clock; host-provided time was verified within 180 seconds."
    TIME_STATUS="host-managed container clock verified over HTTPS"
    return
  fi
  if verify_clock_over_https; then
    warn "chrony has no synchronized source yet; current clock is nevertheless within 180 seconds of trusted HTTPS time."
    TIME_STATUS="chrony enabled; current clock verified over HTTPS"
    return
  fi
  chronyc -a tracking >&2 || true
  fail "The clock did not synchronize within 60 seconds; refusing an installation that could fail authentication."
}

state_value() {
  local key=$1
  [[ -f "${STATE_FILE}" ]] || return 0
  awk -F= -v wanted="${key}" '$1 == wanted { sub(/^[^=]*=/, ""); print; exit }' "${STATE_FILE}"
}

state_flag() {
  [[ "$(state_value "$1")" == "1" ]] && printf '1' || printf '0'
}

OLD_FW_PORT="$(state_value FIREWALL_PORT)"
if [[ "${OLD_FW_PORT}" =~ ^[0-9]{1,5}$ ]]; then
  OLD_FW_PORT=$((10#${OLD_FW_PORT}))
  if (( OLD_FW_PORT < 1 || OLD_FW_PORT > 65535 )); then
    OLD_FW_PORT=""
  fi
else
  OLD_FW_PORT=""
fi
OLD_FW_UFW_OWNED="$(state_flag FIREWALL_UFW_OWNED)"
OLD_FW_FIREWALLD_RUNTIME_OWNED="$(state_flag FIREWALLD_RUNTIME_OWNED)"
OLD_FW_FIREWALLD_PERMANENT_OWNED="$(state_flag FIREWALLD_PERMANENT_OWNED)"
OLD_FW_FIREWALLD_ZONE="$(state_value FIREWALLD_ZONE)"
if [[ -n "${OLD_FW_FIREWALLD_ZONE}" && ! "${OLD_FW_FIREWALLD_ZONE}" =~ ^[A-Za-z0-9_.-]+$ ]]; then
  OLD_FW_FIREWALLD_ZONE=""
  OLD_FW_FIREWALLD_RUNTIME_OWNED=0
  OLD_FW_FIREWALLD_PERMANENT_OWNED=0
fi

random_hex() {
  local bytes=$1
  od -An -N"${bytes}" -tx1 /dev/urandom | tr -d ' \n'
}

USER_NAME="${1:-}"
USER_PASS="${2:-}"
PORT="${3:-}"
OLD_CONFIG_PORT="$(state_value PORT)"
if [[ "${OLD_CONFIG_PORT}" =~ ^[0-9]{1,5}$ ]]; then
  OLD_CONFIG_PORT=$((10#${OLD_CONFIG_PORT}))
else
  OLD_CONFIG_PORT=""
fi

if [[ "${MSBOOST_FORCE_FRESH:-0}" != 1 && -z "${USER_NAME}" ]]; then
  USER_NAME="$(state_value USER_NAME)"
fi
if [[ "${MSBOOST_FORCE_FRESH:-0}" != 1 && -z "${USER_PASS}" ]]; then
  USER_PASS="$(state_value USER_PASS)"
fi
if [[ "${MSBOOST_FORCE_FRESH:-0}" != 1 && -z "${PORT}" ]]; then
  PORT="$(state_value PORT)"
fi

[[ -n "${USER_NAME}" ]] || USER_NAME="msboost-$(random_hex 8)"
[[ -n "${USER_PASS}" ]] || USER_PASS="$(random_hex 16)"
[[ "${USER_NAME}" =~ ^[A-Za-z0-9_.-]{1,64}$ ]] || fail "Username may contain only A-Z, a-z, 0-9, dot, underscore, and hyphen."
[[ "${USER_PASS}" =~ ^[A-Za-z0-9_.-]{12,64}$ ]] || fail "Password must be 12-64 characters using A-Z, a-z, 0-9, dot, underscore, or hyphen."

port_in_use() {
  local wanted=$1 sockets
  if ! sockets="$(ss -H -ltn 2>/dev/null)"; then
    fail "Could not inspect listening TCP ports with ss."
  fi
  printf '%s\n' "${sockets}" | awk -v port="${wanted}" '
    {
      address = $4
      sub(/^.*:/, "", address)
      if (address == port) found = 1
    }
    END { exit(found ? 0 : 1) }
  '
}

random_free_port() {
  local candidate raw attempt
  for attempt in $(seq 1 100); do
    raw="$(od -An -N2 -tu2 /dev/urandom | tr -d ' \n')"
    candidate=$((20000 + raw % 40000))
    if [[ "${MSBOOST_FORCE_FRESH:-0}" == 1 && -n "${OLD_CONFIG_PORT}" && "${candidate}" == "${OLD_CONFIG_PORT}" ]]; then
      continue
    fi
    if ! port_in_use "${candidate}"; then
      printf '%s' "${candidate}"
      return 0
    fi
  done
  return 1
}

if [[ -n "${PORT}" ]]; then
  [[ "${PORT}" =~ ^[0-9]{1,5}$ ]] || fail "Port must contain 1-5 decimal digits."
  PORT=$((10#${PORT}))
  (( PORT >= 1 && PORT <= 65535 )) || fail "Port must be in 1-65535."
else
  PORT="$(random_free_port)" || fail "Could not choose an unused TCP port."
fi

if (( PORT < 1024 )); then
  SYSTEMD_VERSION="$(systemctl --version | awk 'NR == 1 { print $2; exit }')"
  [[ "${SYSTEMD_VERSION}" =~ ^[0-9]+$ ]] || fail "Could not determine the systemd version required for a privileged port."
  (( SYSTEMD_VERSION >= 229 )) || fail \
    "TCP ports below 1024 require systemd 229 or newer for this non-root service."
fi

configure_time

ensure_service_user() {
  local nologin_shell existing_uid existing_gid passwd_entry existing_home existing_shell
  if getent group "${APP_GROUP}" >/dev/null 2>&1; then
    existing_gid="$(getent group "${APP_GROUP}" | awk -F: '{print $3; exit}')"
    [[ "${existing_gid}" =~ ^[0-9]+$ ]] && (( existing_gid < 1000 )) || \
      fail "The name ${APP_GROUP} is already used by a non-system group."
  else
    groupadd --system "${APP_GROUP}"
    CREATED_GROUP=1
  fi
  if id -u "${APP_USER}" >/dev/null 2>&1; then
    existing_uid="$(id -u "${APP_USER}")"
    (( existing_uid < 1000 )) || fail "The name ${APP_USER} is already used by a non-system account."
    if (( ! MANAGED_INSTALL )); then
      passwd_entry="$(getent passwd "${APP_USER}")"
      existing_home="$(printf '%s\n' "${passwd_entry}" | awk -F: '{print $6}')"
      existing_shell="$(printf '%s\n' "${passwd_entry}" | awk -F: '{print $7}')"
      [[ "${existing_home}" == "${STATE_DIR}" && "${existing_shell}" =~ /(nologin|false)$ ]] || \
        fail "The existing ${APP_USER} account does not look like an msboost service account."
    fi
  else
    nologin_shell="$(command -v nologin 2>/dev/null || true)"
    [[ -n "${nologin_shell}" ]] || nologin_shell="/usr/sbin/nologin"
    useradd --system --no-create-home --home-dir "${STATE_DIR}" --shell "${nologin_shell}" --gid "${APP_GROUP}" "${APP_USER}"
    CREATED_USER=1
  fi
}

download_file() {
  local url=$1 destination=$2
  curl --fail --location --silent --show-error \
    --retry 5 --retry-delay 2 --retry-max-time 600 --connect-timeout 15 --max-time 300 \
    "${url}" --output "${destination}"
}

download_engine() {
  local release_json="${STAGE_DIR}/release.json"
  local archive="${STAGE_DIR}/engine.gz"
  local asset_url asset_name asset_digest calculated
  local -a asset_info=()

  note "Downloading and verifying the pinned engine ${VERSION}"
  download_file "https://api.github.com/repos/MetaCubeX/mihomo/releases/tags/${VERSION}" "${release_json}"

  mapfile -t asset_info < <(python3 - "${release_json}" "${ASSET_FAMILY}" "${VERSION}" <<'PY'
import json
import re
import sys

release_path, family, version = sys.argv[1:]
with open(release_path, encoding="utf-8") as handle:
    assets = json.load(handle).get("assets", [])

if family == "amd64":
    patterns = (
        rf"^mihomo-linux-amd64-compatible-{re.escape(version)}[.]gz$",
        rf"^mihomo-linux-amd64-v1-{re.escape(version)}[.]gz$",
        rf"^mihomo-linux-amd64-{re.escape(version)}[.]gz$",
    )
else:
    patterns = (
        rf"^mihomo-linux-arm64-v8-{re.escape(version)}[.]gz$",
        rf"^mihomo-linux-arm64-{re.escape(version)}[.]gz$",
    )

for pattern in patterns:
    for asset in assets:
        if re.fullmatch(pattern, asset.get("name", "")):
            print(asset.get("browser_download_url", ""))
            print(asset.get("name", ""))
            print(asset.get("digest") or "")
            sys.exit(0)
sys.exit(1)
PY
  )

  (( ${#asset_info[@]} >= 3 )) || fail "No compatible Linux engine asset was found for ${VERSION}."
  asset_url="${asset_info[0]}"
  asset_name="${asset_info[1]}"
  asset_digest="${asset_info[2]#sha256:}"
  [[ -n "${asset_url}" && -n "${asset_name}" ]] || fail "Release metadata did not contain a valid download."
  [[ "${asset_digest}" =~ ^[0-9a-fA-F]{64}$ ]] || fail "GitHub did not provide a SHA-256 digest for ${asset_name}."

  download_file "${asset_url}" "${archive}"
  calculated="$(sha256sum "${archive}" | awk '{print $1}')"
  [[ "${calculated,,}" == "${asset_digest,,}" ]] || fail "SHA-256 verification failed for ${asset_name}."
  gzip -t "${archive}"
  gzip -dc "${archive}" > "${STAGE_DIR}/msboost"
  chmod 0755 "${STAGE_DIR}/msboost"
  "${STAGE_DIR}/msboost" -v >/dev/null
}

download_engine

note "Downloading the current GFW domain data"
download_file "${RULE_URL}" "${STAGE_DIR}/runtime/ruleset/msboost-filter.mrs"
[[ -s "${STAGE_DIR}/runtime/ruleset/msboost-filter.mrs" ]] || fail "The downloaded GFW rule data is empty."

cat > "${STAGE_DIR}/runtime/ruleset/msboost-direct.yaml" <<'EOF'
# Reviewed snapshot: 2026-09-09. Rules are evaluated before msboost-filter.
# Literal destination IPs already fall through to MATCH,DIRECT; static CDN IPs
# are deliberately omitted because they are shared and change frequently.
payload:
  # GTop100 and its current MapleStory top-20 destination domains.
  - DOMAIN-SUFFIX,gtop100.com
  - DOMAIN-SUFFIX,royals.ms
  - DOMAIN-SUFFIX,legends.ml
  - DOMAIN-SUFFIX,meowms.net
  - DOMAIN-SUFFIX,dreamms.gg
  - DOMAIN-SUFFIX,starms.cc
  - DOMAIN-SUFFIX,fantasia.ms
  - DOMAIN-SUFFIX,wingstory.org
  - DOMAIN-SUFFIX,playkuro.com
  - DOMAIN-SUFFIX,rien.ms
  - DOMAIN-SUFFIX,kook.vip
  - DOMAIN-SUFFIX,kaizenms.net
  - DOMAIN-SUFFIX,beyond-ms.com
  - DOMAIN-SUFFIX,slimetale.ms
  - DOMAIN-SUFFIX,mysticms.net
  - DOMAIN-SUFFIX,mystics.ms
  - DOMAIN-SUFFIX,ranmelle.com
  - DOMAIN-SUFFIX,yuna.ms
  - DOMAIN-SUFFIX,elluel.net
  - DOMAIN-SUFFIX,bellocan.net
  - DOMAIN-SUFFIX,maplebloom.net
  - DOMAIN-SUFFIX,discord.gg
  - DOMAIN-SUFFIX,royalstory.org

  # Nexon and MapleStory regions, including GMS, KMS, JMS, SEA, Taiwan, China,
  # MapleStory M, and MapleStory Worlds under their parent domains.
  - DOMAIN-SUFFIX,nexon.com
  - DOMAIN-SUFFIX,nexon.net
  - DOMAIN-SUFFIX,nexon.io
  - DOMAIN-SUFFIX,nexon.co.jp
  - DOMAIN-SUFFIX,nexon.co.kr
  - DOMAIN-SUFFIX,nexoncdn.co.kr
  - DOMAIN-SUFFIX,nexongames.co.kr
  - DOMAIN-SUFFIX,maplestory.com
  - DOMAIN-SUFFIX,maplesea.com
  - DOMAIN-SUFFIX,asiasoftsea.com
  - DOMAIN-SUFFIX,playpark.com
  - DOMAIN-SUFFIX,playpark.net
  - DOMAIN-SUFFIX,beanfun.com
  - DOMAIN-SUFFIX,gamania.com
  - DOMAIN-SUFFIX,sdo.com

  # Steam and first-party Valve services.
  - DOMAIN-SUFFIX,dota2.com
  - DOMAIN-SUFFIX,playartifact.com
  - DOMAIN-SUFFIX,s.team
  - DOMAIN-SUFFIX,steam.tv
  - DOMAIN-SUFFIX,steam-api.com
  - DOMAIN-SUFFIX,steam-chat.com
  - DOMAIN-SUFFIX,steamcommunity.com
  - DOMAIN-SUFFIX,steamcontent.com
  - DOMAIN-SUFFIX,steamconnecttest.com
  - DOMAIN-SUFFIX,steamdeck.com
  - DOMAIN-SUFFIX,steamgames.com
  - DOMAIN-SUFFIX,steampowered.com
  - DOMAIN-SUFFIX,steamserver.net
  - DOMAIN-SUFFIX,steamstatic.com
  - DOMAIN-SUFFIX,steamusercontent.com
  - DOMAIN-SUFFIX,underlords.com
  - DOMAIN-SUFFIX,valve.net
  - DOMAIN-SUFFIX,valvesoftware.com
  - DOMAIN,edge.steam-dns.top.comcast.net
  - DOMAIN,steamcommunity-a.akamaihd.net.edgesuite.net
  - DOMAIN,steam.apac.qtlglb.com
  - DOMAIN,steam.eca.qtlglb.com
  - DOMAIN,steam.naeu.qtlglb.com
  - DOMAIN,steam.ru.qtlglb.com
  - DOMAIN,a4e8s8k3.map2.ssl.hwcdn.net
  - DOMAIN,f3b7q2p3.ssl.hwcdn.net
  - DOMAIN,steam.cdn.on.net
  - DOMAIN,steam.cdn.orcon.net.nz
  - DOMAIN,steam.cdn.slingshot.co.nz
  - DOMAIN,steam.cdn.webra.ru
  - DOMAIN,steambroadcast.akamaized.net
  - DOMAIN,steamcdn-a.akamaihd.net
  - DOMAIN,steamcommunity-a.akamaihd.net
  - DOMAIN,steammobile.akamaized.net
  - DOMAIN,steampipe-kr.akamaized.net
  - DOMAIN,steampipe-partner.akamaized.net
  - DOMAIN,steampipe.akamaized.net
  - DOMAIN,steamstore-a.akamaihd.net
  - DOMAIN,steamusercontent-a.akamaihd.net
  - DOMAIN,steamuserimages-a.akamaihd.net
  - DOMAIN,steamvideo-a.akamaihd.net
  - DOMAIN,steamcloudsweden.blob.core.windows.net
  - DOMAIN,steamugcquincy.blob.core.windows.net
  - DOMAIN-SUFFIX,csgo.com.cn
  - DOMAIN-SUFFIX,csgo.wmsj.cn
  - DOMAIN-SUFFIX,dota2.com.cn
  - DOMAIN-SUFFIX,dota2.wmsj.cn
  - DOMAIN-SUFFIX,wmsjsteam.com
  - DOMAIN,client-update.queniuqe.com
  - DOMAIN,dl.steam.clngaa.com
  - DOMAIN,st.dl.bscstorage.net
  - DOMAIN,st.dl.eccdnx.com
  - DOMAIN,st.dl.pinyuncloud.com
  - DOMAIN,steampowered.com.8686c.com
  - DOMAIN,steamstatic.com.8686c.com
  - DOMAIN,alibaba.cdn.steampipe.steamcontent.com
  - DOMAIN,lv.queniujq.cn
  - DOMAIN,xz.pphimalayanrt.com
  - DOMAIN-SUFFIX,steamchina.com
  - DOMAIN,gstore.val.manlaxy.com

  # Discord web, API, CDN, status, activities, and attachment upload hosts.
  - DOMAIN-SUFFIX,dis.gd
  - DOMAIN-SUFFIX,discord.co
  - DOMAIN-SUFFIX,discord.com
  - DOMAIN-SUFFIX,discord.design
  - DOMAIN-SUFFIX,discord.dev
  - DOMAIN-SUFFIX,discord.gift
  - DOMAIN-SUFFIX,discord.gifts
  - DOMAIN-SUFFIX,discord.media
  - DOMAIN-SUFFIX,discord.new
  - DOMAIN-SUFFIX,discord.store
  - DOMAIN-SUFFIX,discord.tools
  - DOMAIN-SUFFIX,discord-activities.com
  - DOMAIN-SUFFIX,discordactivities.com
  - DOMAIN-SUFFIX,discordapp.com
  - DOMAIN-SUFFIX,discordapp.net
  - DOMAIN-SUFFIX,discordmerch.com
  - DOMAIN-SUFFIX,discordpartygames.com
  - DOMAIN-SUFFIX,discordsays.com
  - DOMAIN-SUFFIX,discordstatus.com
  - DOMAIN,discord-attachments-uploads-prd.storage.googleapis.com
EOF

write_server_config() {
  local destination=$1 direct_rules=$2 filter_rules=$3
  cat > "${destination}" <<EOF
mode: rule
log-level: info
find-process-mode: off

listeners:
  - name: msboost-in
    type: mieru
    listen: 0.0.0.0
    port: ${PORT}
    transport: TCP
    users:
      "${USER_NAME}": "${USER_PASS}"

rule-providers:
  msboost-direct:
    type: file
    behavior: classical
    format: yaml
    path: ${direct_rules}
  msboost-filter:
    type: http
    behavior: domain
    format: mrs
    path: ${filter_rules}
    url: "${RULE_URL}"
    interval: 86400

rules:
  - RULE-SET,msboost-direct,DIRECT
  - RULE-SET,msboost-filter,REJECT
  - MATCH,DIRECT
EOF
}

write_server_config \
  "${STAGE_DIR}/app/config.yaml" \
  "${APP_DIR}/ruleset/msboost-direct.yaml" \
  "${STATE_DIR}/ruleset/msboost-filter.mrs"
write_server_config \
  "${STAGE_DIR}/validate.yaml" \
  "${STAGE_DIR}/runtime/ruleset/msboost-direct.yaml" \
  "${STAGE_DIR}/runtime/ruleset/msboost-filter.mrs"

write_install_state() {
  local destination=$1
  cat > "${destination}" <<EOF
MANAGED_BY=${MANAGED_MARKER}
USER_NAME=${USER_NAME}
USER_PASS=${USER_PASS}
PORT=${PORT}
ENGINE_VERSION=${VERSION}
FIREWALL_PORT=${PORT}
FIREWALL_UFW_OWNED=${NEW_FW_UFW_OWNED}
FIREWALLD_RUNTIME_OWNED=${NEW_FW_FIREWALLD_RUNTIME_OWNED}
FIREWALLD_PERMANENT_OWNED=${NEW_FW_FIREWALLD_PERMANENT_OWNED}
FIREWALLD_ZONE=${FW_FIREWALLD_ZONE}
EOF
}

write_install_state "${STAGE_DIR}/app/install.env"

cat > "${STAGE_DIR}/msboost.service" <<EOF
[Unit]
Description=msboost service
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${APP_USER}
Group=${APP_GROUP}
WorkingDirectory=${STATE_DIR}
ExecStart=${BIN} -d ${STATE_DIR} -f ${APP_DIR}/config.yaml
Environment=SAFE_PATHS=${APP_DIR}/ruleset
Restart=on-failure
RestartSec=3s
SyslogIdentifier=msboost
UMask=0027
LimitNOFILE=1048576
NoNewPrivileges=true
PrivateTmp=true
ProtectHome=true
ProtectSystem=strict
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictSUIDSGID=true
LockPersonality=true
MemoryDenyWriteExecute=true
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX AF_NETLINK
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
AmbientCapabilities=CAP_NET_BIND_SERVICE
ReadWritePaths=${STATE_DIR}

[Install]
WantedBy=multi-user.target
EOF

note "Validating the staged configuration before deployment"
"${STAGE_DIR}/msboost" -d "${STAGE_DIR}/runtime" -f "${STAGE_DIR}/validate.yaml" -t

valid_global_ipv4() {
  python3 - "$1" <<'PY'
import ipaddress
import sys

try:
    value = ipaddress.ip_address(sys.argv[1])
except ValueError:
    sys.exit(1)
sys.exit(0 if value.version == 4 and value.is_global else 1)
PY
}

detect_public_ip() {
  local candidate endpoint
  if [[ -n "${MSBOOST_SERVER_IP:-}" ]]; then
    valid_global_ipv4 "${MSBOOST_SERVER_IP}" || fail "MSBOOST_SERVER_IP must be a public IPv4 address."
    printf '%s' "${MSBOOST_SERVER_IP}"
    return
  fi

  for endpoint in \
    https://api.ipify.org \
    https://ipv4.icanhazip.com \
    https://checkip.amazonaws.com; do
    candidate="$(curl --noproxy '*' -4fsS --connect-timeout 5 --max-time 12 "${endpoint}" 2>/dev/null | tr -d '[:space:]' || true)"
    if valid_global_ipv4 "${candidate}"; then
      printf '%s' "${candidate}"
      return
    fi
  done
  fail "Could not determine a public IPv4 address. Set MSBOOST_SERVER_IP and rerun."
}

PUBLIC_IP="$(detect_public_ip)"

cat > "${STAGE_DIR}/client.json" <<EOF
{
  "profiles": [
    {
      "profileName": "msboost",
      "user": {
        "name": "${USER_NAME}",
        "password": "${USER_PASS}"
      },
      "servers": [
        {
          "ipAddress": "${PUBLIC_IP}",
          "domainName": "",
          "portBindings": [
            {
              "port": ${PORT},
              "protocol": "TCP"
            }
          ]
        }
      ],
      "mtu": 1400
    }
  ],
  "activeProfile": "msboost",
  "rpcPort": 8964,
  "socks5Port": 10086,
  "loggingLevel": "INFO"
}
EOF
python3 -m json.tool "${STAGE_DIR}/client.json" >/dev/null

snapshot_existing_installation() {
  install -d -m 0700 "${BACKUP_ROOT}"
  BACKUP_DIR="$(mktemp -d "${BACKUP_ROOT}/transaction.XXXXXX")"
  if [[ -e "${BIN}" || -L "${BIN}" ]]; then
    cp -a -- "${BIN}" "${BACKUP_DIR}/bin"
    OLD_BIN=1
  fi
  if [[ -e "${APP_DIR}" || -L "${APP_DIR}" ]]; then
    cp -a -- "${APP_DIR}" "${BACKUP_DIR}/app"
    OLD_APP=1
  fi
  if [[ -e "${STATE_DIR}" || -L "${STATE_DIR}" ]]; then
    cp -a -- "${STATE_DIR}" "${BACKUP_DIR}/state"
    OLD_STATE=1
  fi
  if [[ -e "${UNIT}" || -L "${UNIT}" ]]; then
    cp -a -- "${UNIT}" "${BACKUP_DIR}/unit"
    OLD_UNIT=1
  fi
  if [[ -e "${CLIENT_CONFIG}" || -L "${CLIENT_CONFIG}" ]]; then
    cp -a -- "${CLIENT_CONFIG}" "${BACKUP_DIR}/client.json"
    OLD_CLIENT=1
  fi
  systemctl is-active --quiet "${SERVICE}" && OLD_ACTIVE=1 || true
  OLD_ENABLE_STATE="$(systemctl is-enabled "${SERVICE}" 2>/dev/null || true)"
  [[ -n "${OLD_ENABLE_STATE}" ]] || OLD_ENABLE_STATE="missing"
}

snapshot_existing_installation
TX_ACTIVE=1
ensure_service_user
if (( OLD_UNIT || OLD_ACTIVE )); then
  systemctl stop "${SERVICE}" || fail "Could not stop the previous msboost service."
  for _ in $(seq 1 20); do
    ! systemctl is-active --quiet "${SERVICE}" && break
    sleep 0.25
  done
  systemctl is-active --quiet "${SERVICE}" && fail "The previous msboost service did not stop."
fi

for _ in $(seq 1 20); do
  ! port_in_use "${PORT}" && break
  sleep 0.25
done
port_in_use "${PORT}" && fail "TCP port ${PORT} is already occupied by another process."

note "Deploying msboost transactionally"
rm -f -- "${BIN}" "${UNIT}" "${CLIENT_CONFIG}"
install -m 0755 "${STAGE_DIR}/msboost" "${BIN}"
rm -rf -- "${APP_DIR}"
rm -rf -- "${STATE_DIR}"
install -d -m 0750 -o root -g "${APP_GROUP}" "${APP_DIR}"
install -d -m 0750 -o root -g "${APP_GROUP}" "${APP_DIR}/ruleset"
install -d -m 0750 -o "${APP_USER}" -g "${APP_GROUP}" "${STATE_DIR}"
install -d -m 0770 -o "${APP_USER}" -g "${APP_GROUP}" "${STATE_DIR}/ruleset"
install -m 0640 -o root -g "${APP_GROUP}" "${STAGE_DIR}/app/config.yaml" "${APP_DIR}/config.yaml"
install -m 0640 -o root -g "${APP_GROUP}" "${STAGE_DIR}/runtime/ruleset/msboost-direct.yaml" "${APP_DIR}/ruleset/msboost-direct.yaml"
install -m 0640 -o "${APP_USER}" -g "${APP_GROUP}" "${STAGE_DIR}/runtime/ruleset/msboost-filter.mrs" "${STATE_DIR}/ruleset/msboost-filter.mrs"
install -m 0600 -o root -g root "${STAGE_DIR}/app/install.env" "${STATE_FILE}"
install -m 0644 -o root -g root "${STAGE_DIR}/msboost.service" "${UNIT}"
install -m 0600 -o root -g root "${STAGE_DIR}/client.json" "${CLIENT_CONFIG}"

SAFE_PATHS="${APP_DIR}/ruleset" "${BIN}" -d "${STATE_DIR}" -f "${APP_DIR}/config.yaml" -t
systemctl daemon-reload
systemctl unmask "${SERVICE}" >/dev/null 2>&1 || true
systemctl enable "${SERVICE}" >/dev/null
systemctl restart "${SERVICE}"

for _ in $(seq 1 30); do
  if systemctl is-active --quiet "${SERVICE}" && port_in_use "${PORT}"; then
    break
  fi
  sleep 1
done
if ! systemctl is-active --quiet "${SERVICE}" || ! port_in_use "${PORT}"; then
  journalctl -u "${SERVICE}" -n 100 --no-pager >&2 || true
  fail "msboost did not become healthy."
fi

note "Running a local end-to-end proxy self-test"
SELFTEST_PORT="$(random_free_port)" || fail "Could not choose a local self-test port."
install -d -m 0700 "${STAGE_DIR}/selftest" "${STAGE_DIR}/selftest-state"
cat > "${STAGE_DIR}/selftest/config.yaml" <<EOF
mode: rule
log-level: warning
listeners:
  - name: msboost-test-in
    type: socks
    listen: 127.0.0.1
    port: ${SELFTEST_PORT}
proxies:
  - name: msboost-loopback
    type: mieru
    server: 127.0.0.1
    port: ${PORT}
    transport: TCP
    username: "${USER_NAME}"
    password: "${USER_PASS}"
    multiplexing: MULTIPLEXING_LOW
    handshake-mode: HANDSHAKE_STANDARD
rules:
  - MATCH,msboost-loopback
EOF

"${BIN}" -d "${STAGE_DIR}/selftest-state" -f "${STAGE_DIR}/selftest/config.yaml" \
  >"${STAGE_DIR}/selftest/msboost-selftest.log" 2>&1 &
TEST_PID=$!
SELFTEST_OK=0
for _ in $(seq 1 15); do
  if curl -fsS --proxy "socks5h://127.0.0.1:${SELFTEST_PORT}" \
      --connect-timeout 3 --max-time 12 https://example.com/ --output /dev/null; then
    SELFTEST_OK=1
    break
  fi
  kill -0 "${TEST_PID}" 2>/dev/null || break
  sleep 1
done
if (( ! SELFTEST_OK )); then
  cat "${STAGE_DIR}/selftest/msboost-selftest.log" >&2 || true
  fail "The local end-to-end proxy self-test failed."
fi
cleanup_test

configure_firewall() {
  local route_interface="" detected_zone="" managed=0
  local nft_rules="" iptables_rules="" custom_rules=0 inspection_uncertain=0
  note "Updating any active local firewall"
  if (( OLD_FW_UFW_OWNED )) && [[ "${OLD_FW_PORT}" == "${PORT}" ]] && \
      command -v ufw >/dev/null 2>&1 && \
      ufw show added 2>/dev/null | grep -Eq "(^|[[:space:]])allow[[:space:]]+${PORT}/tcp([[:space:]]|$)"; then
    NEW_FW_UFW_OWNED=1
  fi
  if (( OLD_FW_FIREWALLD_PERMANENT_OWNED )) && \
      [[ "${OLD_FW_PORT}" == "${PORT}" && -n "${OLD_FW_FIREWALLD_ZONE}" ]] && \
      command -v firewall-offline-cmd >/dev/null 2>&1 && \
      ! firewall-cmd --state >/dev/null 2>&1 && \
      firewall-offline-cmd --zone="${OLD_FW_FIREWALLD_ZONE}" --query-port="${PORT}/tcp" >/dev/null 2>&1; then
    FW_FIREWALLD_ZONE="${OLD_FW_FIREWALLD_ZONE}"
    NEW_FW_FIREWALLD_PERMANENT_OWNED=1
  fi
  if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q '^Status: active'; then
    managed=1
    if ! ufw show added 2>/dev/null | grep -Eq "(^|[[:space:]])allow[[:space:]]+${PORT}/tcp([[:space:]]|$)"; then
      ufw allow "${PORT}/tcp" >/dev/null
      FW_UFW_ADDED=1
      NEW_FW_UFW_OWNED=1
    elif (( OLD_FW_UFW_OWNED )) && [[ "${OLD_FW_PORT}" == "${PORT}" ]]; then
      NEW_FW_UFW_OWNED=1
    fi
    FIREWALL_STATUS="UFW active; a local allow rule for TCP/${PORT} is present"
  fi

  if command -v firewall-cmd >/dev/null 2>&1 && firewall-cmd --state >/dev/null 2>&1; then
    managed=1
    route_interface="$(ip -4 route get 1.1.1.1 2>/dev/null | awk '
      { for (i = 1; i <= NF; i++) if ($i == "dev") { print $(i + 1); exit } }
    ')"
    if [[ -n "${route_interface}" ]]; then
      detected_zone="$(firewall-cmd --get-zone-of-interface="${route_interface}" 2>/dev/null || true)"
    fi
    if [[ -z "${detected_zone}" || "${detected_zone}" == "no zone" ]]; then
      detected_zone="$(firewall-cmd --get-default-zone)"
    fi
    [[ "${detected_zone}" =~ ^[A-Za-z0-9_.-]+$ ]] || fail "Could not determine the active firewalld zone."
    FW_FIREWALLD_ZONE="${detected_zone}"

    if ! firewall-cmd --quiet --zone="${FW_FIREWALLD_ZONE}" --query-port="${PORT}/tcp"; then
      firewall-cmd --quiet --zone="${FW_FIREWALLD_ZONE}" --add-port="${PORT}/tcp"
      FW_FIREWALLD_RUNTIME_ADDED=1
      NEW_FW_FIREWALLD_RUNTIME_OWNED=1
    elif (( OLD_FW_FIREWALLD_RUNTIME_OWNED )) && \
        [[ "${OLD_FW_PORT}" == "${PORT}" && "${OLD_FW_FIREWALLD_ZONE}" == "${FW_FIREWALLD_ZONE}" ]]; then
      NEW_FW_FIREWALLD_RUNTIME_OWNED=1
    fi
    if ! firewall-cmd --quiet --permanent --zone="${FW_FIREWALLD_ZONE}" --query-port="${PORT}/tcp"; then
      firewall-cmd --quiet --permanent --zone="${FW_FIREWALLD_ZONE}" --add-port="${PORT}/tcp"
      FW_FIREWALLD_PERMANENT_ADDED=1
      NEW_FW_FIREWALLD_PERMANENT_OWNED=1
    elif (( OLD_FW_FIREWALLD_PERMANENT_OWNED )) && \
        [[ "${OLD_FW_PORT}" == "${PORT}" && "${OLD_FW_FIREWALLD_ZONE}" == "${FW_FIREWALLD_ZONE}" ]]; then
      NEW_FW_FIREWALLD_PERMANENT_OWNED=1
    fi
    if [[ "${FIREWALL_STATUS}" == "not checked" ]]; then
      FIREWALL_STATUS="firewalld zone ${FW_FIREWALLD_ZONE}; a local allow rule for TCP/${PORT} is present"
    else
      FIREWALL_STATUS="${FIREWALL_STATUS}; firewalld zone ${FW_FIREWALLD_ZONE} also has an allow rule"
    fi
  fi

  if (( ! managed )); then
    if command -v nft >/dev/null 2>&1; then
      if ! nft_rules="$(nft list ruleset 2>/dev/null)"; then
        inspection_uncertain=1
      elif grep -Eqi 'hook[[:space:]]+input.*policy[[:space:]]+(drop|reject)' <<< "${nft_rules}"; then
        fail "A custom nftables input policy is restrictive; it cannot be changed safely by a generic installer."
      elif grep -Eqi '(^|[[:space:];])(drop|reject)([[:space:];]|$)' <<< "${nft_rules}"; then
        custom_rules=1
      fi
    fi
    if command -v iptables >/dev/null 2>&1; then
      if ! iptables_rules="$(iptables -S INPUT 2>/dev/null)"; then
        inspection_uncertain=1
      elif grep -Eq '^-P INPUT (DROP|REJECT)$' <<< "${iptables_rules}"; then
        fail "A custom iptables input policy is restrictive; it cannot be changed safely by a generic installer."
      elif grep -Eq '^-A INPUT .*(-j|-g) (DROP|REJECT)([[:space:]]|$)' <<< "${iptables_rules}"; then
        custom_rules=1
      fi
    fi
    if (( custom_rules )); then
      warn "Custom firewall reject/drop rules exist; this installer cannot prove that public TCP/${PORT} reaches the service."
      FIREWALL_STATUS="custom rules detected; public TCP/${PORT} was not externally verified"
    elif (( inspection_uncertain )); then
      warn "The host firewall could not be fully inspected; public TCP/${PORT} was not externally verified."
      FIREWALL_STATUS="inspection incomplete; public TCP/${PORT} was not externally verified"
    else
      FIREWALL_STATUS="no active UFW/firewalld or obvious default-drop rule detected; public reachability was not externally verified"
    fi
  fi
}

configure_firewall

remove_previous_owned_firewall_rules() {
  local firewalld_identity_changed=0
  [[ -n "${OLD_FW_PORT}" ]] || return 0

  if (( OLD_FW_UFW_OWNED )) && [[ "${OLD_FW_PORT}" != "${PORT}" ]] && \
      command -v ufw >/dev/null 2>&1 && \
      ufw show added 2>/dev/null | grep -Eq "(^|[[:space:]])allow[[:space:]]+${OLD_FW_PORT}/tcp([[:space:]]|$)"; then
    ufw --force delete allow "${OLD_FW_PORT}/tcp" >/dev/null
    OLD_FW_UFW_REMOVED=1
  fi

  if [[ "${OLD_FW_PORT}" != "${PORT}" || "${OLD_FW_FIREWALLD_ZONE}" != "${FW_FIREWALLD_ZONE}" ]]; then
    firewalld_identity_changed=1
  fi
  if (( firewalld_identity_changed )) && [[ -n "${OLD_FW_FIREWALLD_ZONE}" ]]; then
    if command -v firewall-cmd >/dev/null 2>&1 && firewall-cmd --state >/dev/null 2>&1; then
      if (( OLD_FW_FIREWALLD_RUNTIME_OWNED )) && \
          firewall-cmd --quiet --zone="${OLD_FW_FIREWALLD_ZONE}" --query-port="${OLD_FW_PORT}/tcp"; then
        firewall-cmd --quiet --zone="${OLD_FW_FIREWALLD_ZONE}" --remove-port="${OLD_FW_PORT}/tcp"
        OLD_FW_FIREWALLD_RUNTIME_REMOVED=1
      fi
      if (( OLD_FW_FIREWALLD_PERMANENT_OWNED )) && \
          firewall-cmd --quiet --permanent --zone="${OLD_FW_FIREWALLD_ZONE}" --query-port="${OLD_FW_PORT}/tcp"; then
        firewall-cmd --quiet --permanent --zone="${OLD_FW_FIREWALLD_ZONE}" --remove-port="${OLD_FW_PORT}/tcp"
        OLD_FW_FIREWALLD_PERMANENT_REMOVED=1
      fi
    elif (( OLD_FW_FIREWALLD_PERMANENT_OWNED )) && command -v firewall-offline-cmd >/dev/null 2>&1; then
      if firewall-offline-cmd --zone="${OLD_FW_FIREWALLD_ZONE}" --query-port="${OLD_FW_PORT}/tcp" >/dev/null 2>&1; then
        firewall-offline-cmd --zone="${OLD_FW_FIREWALLD_ZONE}" --remove-port="${OLD_FW_PORT}/tcp" >/dev/null
        OLD_FW_FIREWALLD_PERMANENT_REMOVED_OFFLINE=1
      fi
    fi
  fi
}

remove_previous_owned_firewall_rules
write_install_state "${STAGE_DIR}/final-install.env"
install -m 0600 -o root -g root "${STAGE_DIR}/final-install.env" "${STATE_FILE}"

COMMITTED=1
TX_ACTIVE=0

echo
echo "Installed and locally self-tested successfully."
echo "Server: ${PUBLIC_IP}:${PORT} (TCP)"
echo "Time synchronization: ${TIME_STATUS}."
echo "Host firewall: ${FIREWALL_STATUS}."
echo "Direct exceptions: GTop100 MapleStory snapshot, Nexon/MapleStory regions, Steam, and Discord."
echo "GFW domain data: enabled; refresh interval is 24 hours."
echo "Status: systemctl status msboost --no-pager"
echo "Logs:   journalctl -u msboost -f"
echo "Public ingress was not externally probed; cloud security groups and upstream/custom firewalls must permit TCP/${PORT}."
echo
echo "----- COPY EVERYTHING BETWEEN THE LINES INTO 直连.json -----"
cat "${CLIENT_CONFIG}"
echo "----- END CLIENT CONFIG -----"
echo "The same file is saved at: ${CLIENT_CONFIG}"
