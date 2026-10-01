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
readonly PROBE_SERVICE="msboost-tcp-probe.service"
readonly PROBE_UNIT="/etc/systemd/system/msboost-tcp-probe.service"
readonly PROBE_SOURCE="${APP_DIR}/relay_tcp_probe.py"
readonly PROBE_CONFIG="${APP_DIR}/tcp-probe.json"
readonly PROBE_PORT=20424
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
OLD_PROBE_UNIT=0
OLD_PROBE_ACTIVE=0
OLD_PROBE_ENABLE_STATE="missing"
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
  if [[ -e "${PROBE_UNIT}" || -L "${PROBE_UNIT}" ]] || systemctl is-active --quiet "${PROBE_SERVICE}"; then
    systemctl stop "${PROBE_SERVICE}" >/dev/null 2>&1 || rollback_problem "could not stop the failed TCP probe deployment."
  fi
  if [[ -e "${UNIT}" || -L "${UNIT}" ]] || systemctl is-active --quiet "${SERVICE}"; then
    systemctl stop "${SERVICE}" >/dev/null 2>&1 || rollback_problem "could not stop the failed service deployment."
  fi
  clear_unit_links "${PROBE_SERVICE}" || rollback_problem "could not clear new companion enable/mask links."
  clear_unit_links "${SERVICE}" || rollback_problem "could not clear new main enable/mask links."

  rollback_firewall

  rm -f -- "${BIN}" || rollback_problem "could not remove the new executable."
  rm -rf -- "${APP_DIR}" || rollback_problem "could not remove the new configuration directory."
  rm -rf -- "${STATE_DIR}" || rollback_problem "could not remove the new state directory."
  rm -f -- "${UNIT}" || rollback_problem "could not remove the new systemd unit."
  rm -f -- "${PROBE_UNIT}" || rollback_problem "could not remove the new TCP probe unit."
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
    if [[ -L "${BACKUP_DIR}/unit" ]]; then
      install -m 0644 -o root -g root "${STAGE_DIR}/msboost.service" "${UNIT}" || rollback_problem "could not restore verified main template temporarily."
    else
      cp -a -- "${BACKUP_DIR}/unit" "${UNIT}" || rollback_problem "could not restore the prior systemd unit."
    fi
  fi
  if (( OLD_CLIENT )); then
    cp -a -- "${BACKUP_DIR}/client.json" "${CLIENT_CONFIG}" || rollback_problem "could not restore the prior client configuration."
  fi
  if (( OLD_PROBE_UNIT )); then
    if [[ -L "${BACKUP_DIR}/probe-unit" ]]; then
      install -m 0644 -o root -g root "${STAGE_DIR}/msboost-tcp-probe.service" "${PROBE_UNIT}" || rollback_problem "could not restore verified companion template temporarily."
    else
      cp -a -- "${BACKUP_DIR}/probe-unit" "${PROBE_UNIT}" || rollback_problem "could not restore the prior TCP probe unit."
    fi
  fi
  restore_unit_links "${SERVICE}" 0 || rollback_problem "could not restore main enable links."
  restore_unit_links "${PROBE_SERVICE}" 0 || rollback_problem "could not restore companion enable links."

  systemctl daemon-reload >/dev/null 2>&1 || rollback_problem "systemd could not reload restored unit files."
  if (( OLD_ACTIVE )); then
    systemctl start "${SERVICE}" >/dev/null 2>&1 || rollback_problem "could not start the prior service."
    systemctl is-active --quiet "${SERVICE}" || rollback_problem "the prior service is not active after rollback."
  else
    systemctl stop "${SERVICE}" >/dev/null 2>&1 || true
    systemctl is-active --quiet "${SERVICE}" && rollback_problem "the prior main service should be inactive."
  fi
  if (( OLD_PROBE_ACTIVE )); then
    systemctl start "${PROBE_SERVICE}" >/dev/null 2>&1 || rollback_problem "could not start the prior TCP probe."
    systemctl is-active --quiet "${PROBE_SERVICE}" || rollback_problem "the prior TCP probe is not active after rollback."
  else
    systemctl stop "${PROBE_SERVICE}" >/dev/null 2>&1 || true
    systemctl is-active --quiet "${PROBE_SERVICE}" && rollback_problem "the prior TCP probe should be inactive."
  fi
  # Restore runtime masks only after active services have been recovered. This
  # also preserves simultaneous persistent and runtime enables exactly.
  restore_unit_links "${SERVICE}" 1 || rollback_problem "could not restore main mask links."
  restore_unit_links "${PROBE_SERVICE}" 1 || rollback_problem "could not restore companion mask links."
  if (( OLD_UNIT )) && [[ -L "${BACKUP_DIR}/unit" ]]; then
    rm -f -- "${UNIT}"
    cp -a -- "${BACKUP_DIR}/unit" "${UNIT}" || rollback_problem "could not restore the persisted main mask."
  fi
  if (( OLD_PROBE_UNIT )) && [[ -L "${BACKUP_DIR}/probe-unit" ]]; then
    rm -f -- "${PROBE_UNIT}"
    cp -a -- "${BACKUP_DIR}/probe-unit" "${PROBE_UNIT}" || rollback_problem "could not restore the persisted companion mask."
  fi
  systemctl daemon-reload >/dev/null 2>&1 || rollback_problem "systemd could not reload restored masks."
  verify_restored_unit_state "${SERVICE}" "${OLD_ACTIVE}" "${OLD_ENABLE_STATE}"
  verify_restored_unit_state "${PROBE_SERVICE}" "${OLD_PROBE_ACTIVE}" "${OLD_PROBE_ENABLE_STATE}"
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
  for path in "${BIN}" "${APP_DIR}" "${STATE_DIR}" "${UNIT}" "${PROBE_UNIT}" "${CLIENT_CONFIG}"; do
    if [[ -L "${path}" ]]; then
      [[ ( "${path}" == "${UNIT}" || "${path}" == "${PROBE_UNIT}" ) && "$(readlink "${path}")" == /dev/null ]] || fail "Refusing a symbolic link at managed target ${path}."
      # Persisted masks are accepted later only with exact saved template hashes.
    fi
    if [[ -e "${path}" ]]; then
      found=1
    fi
  done
  if (( found )); then
    [[ ! -L "${STATE_FILE}" ]] || fail "Refusing a symbolic-link installation marker."
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
  [[ -x /usr/bin/python3 ]] || fail "The companion requires /usr/bin/python3."
  python3 -c 'import sys; sys.exit(0 if sys.version_info >= (3, 8) else 1)' || fail "Python 3.8 or newer is required."
  SYSTEMD_VERSION="$(systemctl --version | awk 'NR == 1 { print $2; exit }')"
  [[ "${SYSTEMD_VERSION}" =~ ^[0-9]+$ ]] && (( SYSTEMD_VERSION >= 231 )) || fail "systemd 231 or newer is required for service hardening."
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
    (( candidate != PROBE_PORT )) || continue
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

(( PORT != PROBE_PORT )) || fail "TCP/${PROBE_PORT} is reserved for the loopback TCP probe."

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
    [[ "${existing_gid}" =~ ^[0-9]+$ ]] && (( existing_gid > 0 && existing_gid < 1000 )) || \
      fail "The name ${APP_GROUP} is already used by a non-system group."
  else
    groupadd --system "${APP_GROUP}"
    CREATED_GROUP=1
  fi
  if id -u "${APP_USER}" >/dev/null 2>&1; then
    existing_uid="$(id -u "${APP_USER}")"
    (( existing_uid > 0 && existing_uid < 1000 )) || fail "The name ${APP_USER} is already used by a non-system account."
    passwd_entry="$(getent passwd "${APP_USER}")"
    existing_home="$(printf '%s\n' "${passwd_entry}" | awk -F: '{print $6}')"
    existing_shell="$(printf '%s\n' "${passwd_entry}" | awk -F: '{print $7}')"
    [[ "${existing_home}" == "${STATE_DIR}" && "${existing_shell}" =~ /(nologin|false)$ && "$(id -g "${APP_USER}")" == "${existing_gid}" ]] || \
      fail "The existing ${APP_USER} account does not look like an msboost service account."
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
MAIN_UNIT_SHA256=$(sha256sum "${STAGE_DIR}/msboost.service" | awk '{print $1}')
TCP_PROBE_UNIT_SHA256=$(sha256sum "${STAGE_DIR}/msboost-tcp-probe.service" | awk '{print $1}')
EOF
}

write_main_unit() {
  cat > "$1" <<EOF
[Unit]
Description=msboost service
After=network-online.target
Wants=network-online.target msboost-tcp-probe.service

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

}

write_probe_unit() {
  cat > "$1" <<'MSBOOST_TCP_PROBE_UNIT'
[Unit]
Description=msboost TCP probe v2
After=msboost.service
PartOf=msboost.service
BindsTo=msboost.service

[Service]
Type=simple
User=msboost
Group=msboost
ExecStart=/usr/bin/python3 -B /etc/msboost/relay_tcp_probe.py --config /etc/msboost/tcp-probe.json
Restart=on-failure
RestartSec=3s
SyslogIdentifier=msboost-tcp-probe
UMask=0027
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
RestrictAddressFamilies=AF_INET
CapabilityBoundingSet=
MemoryMax=64M
TasksMax=8
LimitNOFILE=64

[Install]
WantedBy=msboost.service
MSBOOST_TCP_PROBE_UNIT
}

write_probe_files() {
  # Fixed, reviewed source: no second installer or remote code download.
  cat > "${STAGE_DIR}/app/relay_tcp_probe.py" <<'MSBOOST_TCP_PROBE_PY'
#!/usr/bin/env python3
# Managed by msboost-installer-v1: tcp-probe-v2
"""MSBOOST v2 full-path TCP probe, Python 3.7+, standard library only.

The production command accepts only the fixed loopback configuration. Targets
arrive over an authenticated MSBOOST path and are checked against a stable IPv4
policy. A successful reply means a real TCP connect completed; game data is
never read. All durations are measured by the client, not returned here.
"""

import argparse
import collections
import errno
import json
import logging
import re
import signal
import socket
import threading
import time
import uuid


MAX_LINE_BYTES = 4096
Limits = collections.namedtuple(
    "Limits", "idle lifetime connect write max_sessions max_connects",
    defaults=(3.0, 15.0, 2.0, 2.0, 4, 3))
DEFAULT_LIMITS = Limits()
NONCE = re.compile(r"\A[0-9a-f]{32}\Z")
SCALAR = r'(?:"[A-Za-z0-9_.-]*"|true|false|-?(?:0|[1-9][0-9]*))'
FIELD = r'"[A-Za-z0-9_.-]*"[ \t\r]*:[ \t\r]*' + SCALAR
FLAT_JSON = re.compile(r'\A[ \t\r]*\{[ \t\r]*' + FIELD +
                       r'(?:[ \t\r]*,[ \t\r]*' + FIELD +
                       r')*[ \t\r]*\}[ \t\r]*\Z')
HELLO_FIELDS = frozenset(("version", "operation", "nonce"))
CONNECT_FIELDS = HELLO_FIELDS | frozenset(("target_address", "target_port"))

IPV4 = re.compile(r"\A(0|[1-9][0-9]{0,2})\.(0|[1-9][0-9]{0,2})\."
                  r"(0|[1-9][0-9]{0,2})\.(0|[1-9][0-9]{0,2})\Z")

# Fixed numeric prefix policy; no Python-version-dependent is_global/private
# classification or DNS lookup. Entire special-purpose/deprecated blocks are
# rejected conservatively, including 192.0.0.9/10 anycast exceptions.
DENIED_IPV4 = (
    (0x00000000, 8), (0x0a000000, 8), (0x64400000, 10),
    (0x7f000000, 8), (0xa9fe0000, 16), (0xac100000, 12),
    (0xc0000000, 24), (0xc0000200, 24), (0xc0586300, 24),
    (0xc0a80000, 16), (0xc6120000, 15), (0xc6336400, 24),
    (0xcb007100, 24), (0xe0000000, 4), (0xf0000000, 4))

# Production values are fixed, rather than accepting user-controlled budgets,
# additional peers, or listener addresses through a configuration file.
EXPECTED_CONFIG = {
    "listen_address": "127.0.0.1", "listen_port": 20424,
    "allowed_peer": "127.0.0.1", "max_sessions": 4,
    "max_connect_requests": 3, "idle_timeout": 3,
    "session_lifetime": 15, "connect_timeout": 2,
    "write_timeout": 2, "max_line_bytes": MAX_LINE_BYTES}


class ProtocolError(ValueError):
    """The connection must be discarded without probing a target."""


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ProtocolError("duplicate field")
        result[key] = value
    return result


def canonical_ipv4(value):
    if type(value) is not str:
        raise ProtocolError("invalid IPv4 type")
    match = IPV4.fullmatch(value)
    if match is None:
        raise ProtocolError("invalid IPv4")
    octets = tuple(int(part) for part in match.groups())
    if any(part > 255 for part in octets):
        raise ProtocolError("invalid IPv4 octet")
    address = 0
    for part in octets:
        address = (address << 8) | part
    return address


def public_unicast_ipv4(value):
    address = canonical_ipv4(value)
    return not any(address >> (32 - prefix) == network >> (32 - prefix)
                   for network, prefix in DENIED_IPV4)


def parse_request(payload):
    if len(payload) > MAX_LINE_BYTES:
        raise ProtocolError("line too long")
    try:
        text = payload.decode("utf-8", "strict")
    except UnicodeDecodeError:
        raise ProtocolError("invalid UTF-8")
    if FLAT_JSON.fullmatch(text) is None:
        raise ProtocolError("invalid flat JSON")
    try:
        request = json.loads(text, object_pairs_hook=unique_object)
    except (ValueError, TypeError) as error:
        raise ProtocolError(str(error))
    if type(request.get("version")) is not int or request["version"] != 2:
        raise ProtocolError("invalid version")
    if not isinstance(request.get("nonce"), str) or NONCE.fullmatch(request["nonce"]) is None:
        raise ProtocolError("invalid nonce")
    operation = request.get("operation")
    if operation == "hello":
        fields = HELLO_FIELDS
    elif operation == "connect":
        fields = CONNECT_FIELDS
        canonical_ipv4(request.get("target_address"))
        port = request.get("target_port")
        if type(port) is not int or not 1 <= port <= 65535:
            raise ProtocolError("invalid target port")
    else:
        raise ProtocolError("invalid operation")
    if frozenset(request) != fields:
        raise ProtocolError("unknown or missing field")
    return request


def remaining(deadline, clock=time.monotonic):
    timeout = deadline - clock()
    if timeout <= 0:
        raise socket.timeout("deadline exceeded")
    return timeout


def read_line(stream, deadline, clock=time.monotonic):
    payload = bytearray()
    while True:
        stream.settimeout(remaining(deadline, clock))
        chunk = stream.recv(min(512, MAX_LINE_BYTES + 1 - len(payload)))
        # A late successful OS return still cannot exceed the shared deadline.
        remaining(deadline, clock)
        if not chunk:
            raise EOFError("peer closed connection")
        newline = chunk.find(b"\n")
        if newline >= 0:
            if newline != len(chunk) - 1:
                raise ProtocolError("unsolicited bytes after line")
            payload.extend(chunk[:newline])
            if len(payload) > MAX_LINE_BYTES:
                raise ProtocolError("line too long")
            return bytes(payload)
        payload.extend(chunk)
        if len(payload) > MAX_LINE_BYTES:
            raise ProtocolError("line too long")


def write_reply(stream, reply, deadline, clock=time.monotonic):
    wire = json.dumps(reply, ensure_ascii=True, separators=(",", ":")).encode("ascii") + b"\n"
    stream.settimeout(remaining(deadline, clock))
    stream.sendall(wire)
    remaining(deadline, clock)


def tcp_connect(address, port, timeout):
    # Numeric canonical IPv4 + an AF_INET socket: no DNS and no application IO.
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as target:
        target.settimeout(timeout)
        target.connect((address, port))


def connect_error(error):
    if isinstance(error, (socket.timeout, TimeoutError)) or getattr(error, "errno", None) == errno.ETIMEDOUT:
        return "timeout"
    code = getattr(error, "errno", None)
    if code == errno.ECONNREFUSED:
        return "refused"
    if code in (errno.ENETUNREACH, errno.EHOSTUNREACH, errno.ENETDOWN,
                getattr(errno, "EHOSTDOWN", -1), errno.EADDRNOTAVAIL):
        return "unreachable"
    return "failed"


class ProbeServer:
    """Bounded loopback server. Constructor seams are for local unit tests only."""

    def __init__(self, listen_address="127.0.0.1", listen_port=20424,
                 connector=tcp_connect, limits=DEFAULT_LIMITS, clock=time.monotonic):
        if listen_address != "127.0.0.1" or type(listen_port) is not int or not 0 <= listen_port <= 65535:
            raise ValueError("loopback IPv4 binding required")
        self.node_id = uuid.uuid4().hex
        self.connector = connector
        self.limits = limits
        self.clock = clock
        self.stop_event = threading.Event()
        self.slots = threading.BoundedSemaphore(limits.max_sessions)
        # A Python signal handler may request_stop on this same thread while
        # acceptance is being registered. Reentrancy avoids a signal deadlock.
        self.lock = threading.RLock()
        self.clients = set()
        self.workers = set()
        self.listener = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        try:
            self.listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            self.listener.bind((listen_address, listen_port))
            self.listener.listen(limits.max_sessions)
            self.listener.settimeout(0.25)
            self.address = self.listener.getsockname()
        except BaseException:
            self.listener.close()
            raise

    def serve_forever(self):
        try:
            while not self.stop_event.is_set():
                try:
                    client, peer = self.listener.accept()
                except socket.timeout:
                    continue
                except OSError:
                    if self.stop_event.is_set():
                        break
                    raise
                if peer[0] != "127.0.0.1" or not self.slots.acquire(blocking=False):
                    client.close()
                    continue
                deadline = self.clock() + self.limits.lifetime
                worker = threading.Thread(target=self._run_session, args=(client, deadline), daemon=True)
                with self.lock:
                    if self.stop_event.is_set():
                        client.close()
                        self.slots.release()
                        break
                    self.clients.add(client)
                    self.workers.add(worker)
                    # Stop/join snapshots cannot observe an unstarted worker.
                    try:
                        worker.start()
                    except BaseException:
                        self.clients.discard(client)
                        self.workers.discard(worker)
                        client.close()
                        self.slots.release()
                        raise
        finally:
            self.request_stop()
            self.join_workers()

    def _run_session(self, client, deadline):
        try:
            client.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
            self._handle_session(client, deadline)
        except (ProtocolError, EOFError, OSError, ValueError):
            # Malformed/expired sessions close without exposing arbitrary text.
            pass
        finally:
            client.close()
            with self.lock:
                self.clients.discard(client)
                self.workers.discard(threading.current_thread())
            self.slots.release()

    def _handle_session(self, client, deadline):
        seen_nonces = set()
        hello = parse_request(read_line(client, min(deadline, self.clock() + self.limits.idle), self.clock))
        if hello["operation"] != "hello":
            raise ProtocolError("hello required")
        seen_nonces.add(hello["nonce"])
        self._reply(client, hello, True, deadline)
        for unused in range(self.limits.max_connects):
            request = parse_request(read_line(client, min(deadline, self.clock() + self.limits.idle), self.clock))
            if request["operation"] != "connect" or request["nonce"] in seen_nonces:
                raise ProtocolError("invalid session order or repeated nonce")
            seen_nonces.add(request["nonce"])
            if not public_unicast_ipv4(request["target_address"]):
                self._reply(client, request, False, deadline, "target_not_allowed")
                continue
            try:
                connect_deadline = min(deadline, self.clock() + self.limits.connect)
                self.connector(request["target_address"], request["target_port"],
                               remaining(connect_deadline, self.clock))
                remaining(connect_deadline, self.clock)
            except OSError as error:
                self._reply(client, request, False, deadline, connect_error(error))
            else:
                self._reply(client, request, True, deadline)

    def _reply(self, client, request, success, deadline, error=None):
        reply = dict(request)
        reply["node_id"] = self.node_id
        reply["success"] = success
        if error is not None:
            reply["error"] = error
        write_reply(client, reply, min(deadline, self.clock() + self.limits.write), self.clock)

    def request_stop(self):
        self.stop_event.set()
        self.listener.close()
        with self.lock:
            clients = tuple(self.clients)
        for client in clients:
            try:
                client.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass

    def join_workers(self):
        deadline = time.monotonic() + self.limits.connect + 0.5
        with self.lock:
            workers = tuple(self.workers)
        for worker in workers:
            worker.join(max(0, deadline - time.monotonic()))


def load_config(path):
    with open(path, "r", encoding="utf-8") as source:
        config = json.load(source, object_pairs_hook=unique_object)
    if type(config) is not dict or set(config) != set(EXPECTED_CONFIG):
        raise ValueError("unknown or missing configuration field")
    for key, expected in EXPECTED_CONFIG.items():
        if type(config[key]) is not type(expected) or config[key] != expected:
            raise ValueError("fixed loopback configuration and limits required")
    return config


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--config", required=True)
    parser.add_argument("--validate-config", action="store_true",
                        help="validate fixed configuration without binding a socket")
    arguments = parser.parse_args()
    logging.basicConfig(level=logging.INFO, format="%(levelname)s %(message)s")
    try:
        config = load_config(arguments.config)
        if arguments.validate_config:
            return 0
        server = ProbeServer(config["listen_address"], config["listen_port"])
    except (OSError, ValueError) as error:
        logging.error("Unable to start v2 probe: %s", error)
        return 1

    def stop(signum, frame):
        server.request_stop()

    signal.signal(signal.SIGINT, stop)
    signal.signal(signal.SIGTERM, stop)
    logging.info("MSBOOST TCP probe v2 listening on 127.0.0.1:20424")
    server.serve_forever()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
MSBOOST_TCP_PROBE_PY
  cat > "${STAGE_DIR}/app/tcp-probe.json" <<'MSBOOST_TCP_PROBE_CONFIG'
{"listen_address":"127.0.0.1","listen_port":20424,"allowed_peer":"127.0.0.1","max_sessions":4,"max_connect_requests":3,"idle_timeout":3,"session_lifetime":15,"connect_timeout":2,"write_timeout":2,"max_line_bytes":4096}
MSBOOST_TCP_PROBE_CONFIG
}

write_main_unit "${STAGE_DIR}/msboost.service"
write_probe_unit "${STAGE_DIR}/msboost-tcp-probe.service"
write_probe_files
write_install_state "${STAGE_DIR}/app/install.env"
python3 - "${STAGE_DIR}/app/relay_tcp_probe.py" <<'PY'
import ast
import sys
with open(sys.argv[1], 'rb') as source:
    ast.parse(source.read(), filename=sys.argv[1])
PY
python3 -B "${STAGE_DIR}/app/relay_tcp_probe.py" --config "${STAGE_DIR}/app/tcp-probe.json" --validate-config

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

verify_existing_resources() {
  python3 - "${STAGE_DIR}" "${MANAGED_INSTALL}" <<'MSBOOST_IDENTITY_PY'
import hashlib
import os
import pwd
import grp
import stat
import subprocess
import sys

stage, managed = sys.argv[1:]
def reject(reason):
    raise SystemExit("ERROR: Refusing to overwrite unknown resources: " + reason)
def read(path):
    with open(path, 'rb') as source:
        return source.read()
def check(path, owners=(0,), exact=None):
    current = path
    while current != '/':
        if os.path.islink(current):
            reject('symlink ' + current)
        parent = os.path.dirname(current)
        if parent == current:
            break
        current = parent
    if not os.path.lexists(path):
        return
    value = os.lstat(path)
    if (not (stat.S_ISREG(value.st_mode) or stat.S_ISDIR(value.st_mode)) or
            value.st_uid not in owners or value.st_mode & 0o002 or os.path.ismount(path) or
            (stat.S_ISREG(value.st_mode) and value.st_nlink != 1)):
        reject('unsafe owner/type/mode ' + path)
    if exact and (value.st_uid, value.st_gid, stat.S_IMODE(value.st_mode)) != exact:
        reject('unexpected owner/mode ' + path)
    if value.st_mode & 0o020 and (value.st_uid, value.st_gid) != (uid, gid):
        reject('unsafe group write ' + path)

uid = gid = -1
try:
    group = grp.getgrnam('msboost')
    gid = group.gr_gid
    if not 0 < gid < 1000:
        reject('non-system msboost group')
except KeyError:
    pass
try:
    account = pwd.getpwnam('msboost')
    uid = account.pw_uid
    if (not 0 < uid < 1000 or account.pw_gid != gid or account.pw_dir != '/var/lib/msboost' or
            account.pw_shell.rsplit('/', 1)[-1] not in ('nologin', 'false')):
        reject('msboost account identity')
except KeyError:
    pass
if managed == '1' and (uid < 0 or gid < 0):
    reject('managed service account missing')

allowed = {
    '/etc/msboost': {'config.yaml', 'install.env', 'ruleset', 'relay_tcp_probe.py', 'tcp-probe.json'},
    '/etc/msboost/ruleset': {'msboost-direct.yaml'},
    '/var/lib/msboost': {'cache.db', 'cache.db-shm', 'cache.db-wal', 'ruleset'},
    '/var/lib/msboost/ruleset': {'msboost-filter.mrs'},
}
for directory, names in allowed.items():
    check(directory, (0, uid))
    if os.path.lexists(directory):
        if not os.path.isdir(directory) or not set(os.listdir(directory)) <= names:
            reject('unknown directory contents ' + directory)
        for name in os.listdir(directory):
            path = directory + '/' + name
            check(path, (0, uid))
            if os.path.isdir(path) and path not in allowed:
                reject('unexpected directory ' + path)
for path in ('/usr/local/bin/msboost', '/root/直连.json', '/etc/msboost/install.env'):
    check(path)
settings = {}
state_path = '/etc/msboost/install.env'
if os.path.lexists(state_path):
    check(state_path, exact=(0, 0, 0o600))
    for line in read(state_path).decode('utf-8').splitlines():
        if '=' not in line:
            reject('invalid installation state line')
        key, value = line.split('=', 1)
        if key in settings:
            reject('duplicate installation state field')
        settings[key] = value
    if settings.get('MANAGED_BY') != 'msboost-installer-v1':
        reject('installation state marker')
probe_paths = ('/etc/msboost/relay_tcp_probe.py', '/etc/msboost/tcp-probe.json',
               '/etc/systemd/system/msboost-tcp-probe.service')
probe_present = any(os.path.lexists(path) for path in probe_paths)
if probe_present and not all(os.path.lexists(path) for path in probe_paths):
    reject('incomplete companion resource set')

def unit_identity(path, expected, key, legacy=None):
    if os.path.islink(path):
        value = os.lstat(path)
        check(os.path.dirname(path))
        if (os.readlink(path) != '/dev/null' or (value.st_uid, value.st_gid) != (0, 0) or
                not probe_present or settings.get(key) != hashlib.sha256(expected).hexdigest()):
            reject('unverifiable persisted mask ' + path)
    else:
        check(path, exact=(0, 0, 0o644))
        if os.path.lexists(path) and read(path) not in (expected, legacy):
            reject('unit template ' + path)

if probe_present:
    for path, expected in zip(probe_paths[:2], (stage + '/app/relay_tcp_probe.py', stage + '/app/tcp-probe.json')):
        check(path, exact=(0, gid, 0o640))
        if read(path) != read(expected):
            reject('companion source/config identity ' + path)
    unit_identity(probe_paths[2], read(stage + '/msboost-tcp-probe.service'), 'TCP_PROBE_UNIT_SHA256')
main_path = '/etc/systemd/system/msboost.service'
expected = read(stage + '/msboost.service')
legacy = expected.replace(b'Wants=network-online.target msboost-tcp-probe.service', b'Wants=network-online.target')
unit_identity(main_path, expected, 'MAIN_UNIT_SHA256', legacy)
if os.path.lexists(main_path) and not os.path.islink(main_path):
    if read(main_path) == expected and not probe_present:
        reject('main dependency missing companion')

for unit in ('msboost.service', 'msboost-tcp-probe.service'):
    unit_path = '/etc/systemd/system/' + unit
    for root in ('/etc/systemd/system', '/run/systemd/system', '/usr/lib/systemd/system', '/lib/systemd/system'):
        if os.path.lexists(root + '/' + unit + '.d'):
            reject('unit drop-in ' + unit)
    runtime = '/run/systemd/system/' + unit
    if os.path.lexists(runtime):
        if not os.path.islink(runtime) or os.readlink(runtime) != '/dev/null' or not os.path.lexists(unit_path):
            reject('unknown runtime unit/mask ' + unit)
        value = os.lstat(runtime)
        if (value.st_uid, value.st_gid) != (0, 0):
            reject('runtime mask owner ' + unit)
    properties = {}
    for key in ('FragmentPath', 'DropInPaths', 'LoadState'):
        properties[key] = subprocess.check_output(['systemctl', 'show', unit, '-p', key, '--value'],
                stderr=subprocess.DEVNULL, universal_newlines=True, timeout=15).strip()
    if properties['DropInPaths']:
        reject('loaded drop-in ' + unit)
    accepted = (unit_path, runtime) if os.path.islink(runtime) else (unit_path,)
    if os.path.islink(runtime) or os.path.islink(unit_path):
        accepted += ('/dev/null',)
    if properties['FragmentPath'] and properties['FragmentPath'] not in accepted:
        reject('unexpected loaded unit fragment ' + unit)
    if not os.path.lexists(unit_path) and (properties['FragmentPath'] or properties['LoadState'] not in ('', 'not-found') or subprocess.call(['systemctl', 'is-active', '--quiet', unit]) == 0):
        reject('unowned loaded unit ' + unit)
    target = 'multi-user.target' if unit == 'msboost.service' else 'msboost.service'
    for root in ('/etc/systemd/system', '/run/systemd/system'):
        check(root)
        check(root + '/' + target + '.wants')
        if os.path.lexists(root + '/' + target + '.wants') and not os.path.isdir(root + '/' + target + '.wants'):
            reject('enable-link parent is not a directory')
        for directory, dirs, files in os.walk(root, followlinks=False):
            for filename in files:
                candidate = directory + '/' + filename
                if filename != unit and os.path.islink(candidate) and os.path.abspath(os.path.join(directory, os.readlink(candidate))) == unit_path:
                    reject('unexpected unit alias ' + candidate)
            if unit not in files:
                continue
            link = directory + '/' + unit
            if link in (unit_path, runtime):
                continue
            if link != root + '/' + target + '.wants/' + unit:
                reject('unexpected enable link ' + link)
            if not os.path.islink(link) or os.path.abspath(os.path.join(os.path.dirname(link), os.readlink(link))) != unit_path or (os.lstat(link).st_uid, os.lstat(link).st_gid) != (0, 0):
                reject('unknown enable link ' + link)
if subprocess.call(['systemctl', 'is-active', '--quiet', 'msboost-tcp-probe.service']) == 0:
    if not probe_present or subprocess.call(['systemctl', 'is-active', '--quiet', 'msboost.service']) != 0:
        reject('inconsistent companion activity')
MSBOOST_IDENTITY_PY
}

verify_probe_port_owner() {
  local sockets pid commandline expected_pid
  sockets="$(ss -H -ltnp "sport = :${PROBE_PORT}")" || fail "Could not inspect TCP/${PROBE_PORT}."
  [[ -n "${sockets}" ]] || return 0
  [[ -e "${PROBE_UNIT}" || -L "${PROBE_UNIT}" ]] && systemctl is-active --quiet "${PROBE_SERVICE}" || fail "TCP/${PROBE_PORT} is occupied by an unknown process."
  expected_pid="$(systemctl show "${PROBE_SERVICE}" -p MainPID --value)"
  [[ "${expected_pid}" =~ ^[1-9][0-9]*$ ]] || fail "The companion has no verifiable MainPID."
  python3 - "${sockets}" "${expected_pid}" "${APP_USER}" <<'PY'
import os
import pwd
import re
import sys
rows, expected, username = sys.argv[1:]
for row in rows.splitlines():
    fields = row.split()
    if len(fields) < 5 or fields[3] != '127.0.0.1:20424':
        raise SystemExit('ERROR: Probe port has an unexpected listening address.')
    pids = re.findall(r'pid=([0-9]+)', row)
    if not pids or set(pids) != {expected}:
        raise SystemExit('ERROR: Probe port is not exclusively owned by its service.')
with open('/proc/' + expected + '/cmdline', 'rb') as handle:
    command = handle.read().rstrip(b'\0').split(b'\0')
if command != [b'/usr/bin/python3', b'-B', b'/etc/msboost/relay_tcp_probe.py', b'--config', b'/etc/msboost/tcp-probe.json']:
    raise SystemExit('ERROR: Probe process command identity does not match.')
if os.stat('/proc/' + expected).st_uid != pwd.getpwnam(username).pw_uid:
    raise SystemExit('ERROR: Probe process owner does not match.')
PY
}

unit_state_paths() {
  local service=$1 target
  [[ "${service}" == "${SERVICE}" ]] && target="multi-user.target" || target="msboost.service"
  printf '%s\n' "/run/systemd/system/${service}" "/etc/systemd/system/${target}.wants/${service}" "/run/systemd/system/${target}.wants/${service}"
}

snapshot_unit_links() {
  local service=$1 path index=0
  install -d -m 0700 "${BACKUP_DIR}/${service}.links"
  while IFS= read -r path; do
    if [[ -e "${path}" || -L "${path}" ]]; then
      cp -a -- "${path}" "${BACKUP_DIR}/${service}.links/${index}"
    fi
    index=$((index + 1))
  done < <(unit_state_paths "${service}")
}

clear_unit_links() {
  local service=$1 path
  while IFS= read -r path; do
    rm -f -- "${path}" || return
  done < <(unit_state_paths "${service}")
}

restore_unit_links() {
  local service=$1 include_mask=$2 path saved index=0
  while IFS= read -r path; do
    saved="${BACKUP_DIR}/${service}.links/${index}"
    if [[ -e "${saved}" || -L "${saved}" ]]; then
      if [[ "${include_mask}" == 1 || "$(readlink "${saved}" 2>/dev/null || true)" != /dev/null ]]; then
        install -d -m 0755 "$(dirname "${path}")" || return
        rm -f -- "${path}" || return
        cp -a -- "${saved}" "${path}" || return
      fi
    fi
    index=$((index + 1))
  done < <(unit_state_paths "${service}")
}

verify_restored_unit_state() {
  local service=$1 active=$2 enabled=$3 actual
  actual="$(systemctl is-enabled "${service}" 2>/dev/null || true)"
  [[ -n "${actual}" ]] || actual="missing"
  [[ "${actual}" == "${enabled}" ]] || rollback_problem "${service} enable/mask state did not restore (${actual}; expected ${enabled})."
  if (( active )); then
    systemctl is-active --quiet "${service}" || rollback_problem "${service} prior activity did not restore."
  else
    if systemctl is-active --quiet "${service}"; then
      rollback_problem "${service} should remain inactive after rollback."
    fi
  fi
}

verify_existing_resources
verify_probe_port_owner

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
  if [[ -e "${PROBE_UNIT}" || -L "${PROBE_UNIT}" ]]; then
    cp -a -- "${PROBE_UNIT}" "${BACKUP_DIR}/probe-unit"
    OLD_PROBE_UNIT=1
  fi
  snapshot_unit_links "${SERVICE}"
  snapshot_unit_links "${PROBE_SERVICE}"
  systemctl is-active --quiet "${PROBE_SERVICE}" && OLD_PROBE_ACTIVE=1 || true
  OLD_PROBE_ENABLE_STATE="$(systemctl is-enabled "${PROBE_SERVICE}" 2>/dev/null || true)"
  [[ -n "${OLD_PROBE_ENABLE_STATE}" ]] || OLD_PROBE_ENABLE_STATE="missing"
  systemctl is-active --quiet "${SERVICE}" && OLD_ACTIVE=1 || true
  OLD_ENABLE_STATE="$(systemctl is-enabled "${SERVICE}" 2>/dev/null || true)"
  [[ -n "${OLD_ENABLE_STATE}" ]] || OLD_ENABLE_STATE="missing"
}

snapshot_existing_installation
verify_existing_resources
verify_probe_port_owner
TX_ACTIVE=1
ensure_service_user
if (( OLD_PROBE_UNIT || OLD_PROBE_ACTIVE )); then
  systemctl stop "${PROBE_SERVICE}" || fail "Could not stop the previous TCP probe."
  systemctl is-active --quiet "${PROBE_SERVICE}" && fail "The previous TCP probe did not stop."
fi
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

for _ in $(seq 1 20); do
  ! port_in_use "${PROBE_PORT}" && break
  sleep 0.25
done
port_in_use "${PROBE_PORT}" && fail "Loopback probe TCP/${PROBE_PORT} is still occupied."

note "Deploying msboost transactionally"
clear_unit_links "${SERVICE}"
clear_unit_links "${PROBE_SERVICE}"
rm -f -- "${BIN}" "${UNIT}" "${PROBE_UNIT}" "${CLIENT_CONFIG}"
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
install -m 0640 -o root -g "${APP_GROUP}" "${STAGE_DIR}/app/relay_tcp_probe.py" "${PROBE_SOURCE}"
install -m 0640 -o root -g "${APP_GROUP}" "${STAGE_DIR}/app/tcp-probe.json" "${PROBE_CONFIG}"
install -m 0644 -o root -g root "${STAGE_DIR}/msboost.service" "${UNIT}"
install -m 0644 -o root -g root "${STAGE_DIR}/msboost-tcp-probe.service" "${PROBE_UNIT}"
install -m 0600 -o root -g root "${STAGE_DIR}/client.json" "${CLIENT_CONFIG}"

SAFE_PATHS="${APP_DIR}/ruleset" "${BIN}" -d "${STATE_DIR}" -f "${APP_DIR}/config.yaml" -t
systemctl daemon-reload
systemctl enable "${SERVICE}" "${PROBE_SERVICE}" >/dev/null
systemctl restart "${SERVICE}"

for _ in $(seq 1 30); do
  if systemctl is-active --quiet "${SERVICE}" && systemctl is-active --quiet "${PROBE_SERVICE}" && port_in_use "${PORT}" && port_in_use "${PROBE_PORT}"; then
    break
  fi
  sleep 1
done
if ! systemctl is-active --quiet "${SERVICE}" || ! systemctl is-active --quiet "${PROBE_SERVICE}" || ! port_in_use "${PORT}" || ! port_in_use "${PROBE_PORT}"; then
  journalctl -u "${SERVICE}" -u "${PROBE_SERVICE}" -n 100 --no-pager >&2 || true
  fail "msboost did not become healthy."
fi

probe_hello() {
  # One deadline covers SOCKS5 and hello, including fragmented replies.
  python3 - "$1" <<'MSBOOST_PROBE_HELLO_PY'
import json
import re
import socket
import sys
import time
import uuid

socks_port = int(sys.argv[1])
deadline = time.monotonic() + 5
stream = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
def budget():
    left = deadline - time.monotonic()
    if left <= 0:
        raise TimeoutError('probe hello warm-up deadline exceeded')
    stream.settimeout(left)
def send(payload):
    budget()
    stream.sendall(payload)
def exact(size):
    result = bytearray()
    while len(result) < size:
        budget()
        part = stream.recv(size - len(result))
        if not part:
            raise ValueError('premature EOF in SOCKS5 reply')
        result.extend(part)
    return bytes(result)
def unique(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError('duplicate JSON field')
        result[key] = value
    return result
try:
    budget()
    stream.connect(('127.0.0.1', socks_port or 20424))
    if socks_port:
        send(b'\x05\x01\x00')
        if exact(2) != b'\x05\x00':
            raise ValueError('SOCKS5 authentication unsupported or invalid')
        send(b'\x05\x01\x00\x01\x7f\x00\x00\x01' + (20424).to_bytes(2, 'big'))
        header = exact(4)
        if header[:3] != b'\x05\x00\x00':
            raise ValueError('SOCKS5 CONNECT failed or invalid')
        if header[3] == 1:
            exact(4)
        elif header[3] == 4:
            exact(16)
        elif header[3] == 3:
            length = exact(1)[0]
            if not length:
                raise ValueError('empty SOCKS5 bound hostname')
            exact(length)
        else:
            raise ValueError('invalid SOCKS5 bound address type')
        exact(2)
    nonce = uuid.uuid4().hex
    request = {'version': 2, 'operation': 'hello', 'nonce': nonce}
    send(json.dumps(request, separators=(',', ':')).encode('ascii') + b'\n')
    received = bytearray()
    while True:
        budget()
        part = stream.recv(4098 - len(received))
        if not part:
            raise ValueError('premature EOF in probe hello')
        received.extend(part)
        offset = received.find(b'\n')
        if offset >= 0:
            if offset > 4096 or offset != len(received) - 1:
                raise ValueError('invalid hello line boundary')
            raw = bytes(received[:offset])
            budget()
            break
        if len(received) > 4096:
            raise ValueError('probe hello exceeds 4096 bytes')
    text = raw.decode('utf-8', errors='strict')
    scalar = r'(?:"[A-Za-z0-9_.-]*"|true|false|0|[1-9][0-9]*)'
    field = r'"[a-z_]+"[ \t\r]*:[ \t\r]*' + scalar
    if not re.fullmatch(r'[ \t\r]*\{[ \t\r]*' + field + r'(?:[ \t\r]*,[ \t\r]*' + field + r')*[ \t\r]*\}[ \t\r]*', text):
        raise ValueError('invalid raw hello JSON')
    response = json.loads(text, object_pairs_hook=unique)
    if (set(response) != {'version', 'operation', 'nonce', 'node_id', 'success'} or
            type(response['version']) is not int or response['version'] != 2 or
            response['operation'] != 'hello' or response['nonce'] != nonce or
            type(response['node_id']) is not str or not re.fullmatch('[0-9a-f]{32}', response['node_id']) or
            response['success'] is not True):
        raise ValueError('probe hello identity or fields mismatch')
    budget()
except (OSError, ValueError, TypeError) as error:
    raise SystemExit('ERROR: TCP probe hello self-test failed: ' + str(error))
finally:
    stream.close()
MSBOOST_PROBE_HELLO_PY
}

note "Validating the loopback TCP probe health"
probe_hello 0

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
if ! probe_hello "${SELFTEST_PORT}"; then
  cat "${STAGE_DIR}/selftest/msboost-selftest.log" >&2 || true
  fail "The SOCKS5 -> Mieru -> loopback TCP probe self-test failed."
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
