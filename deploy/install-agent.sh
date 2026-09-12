#!/usr/bin/env bash
# Install the locally built MSBOOST Agent on an explicitly chosen Linux machine.
# No credentials, installer payloads or customer JSON are fetched from this script.
set -Eeuo pipefail
umask 077
export LC_ALL=C
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

usage() {
  cat <<'HELP'
Usage: sudo bash deploy/install-agent.sh \
  --capability executor|relay --server https://YOUR_DOMAIN \
  --agent /absolute/path/to/msboost-agent-linux-ARCH \
  --agent-sha256 EXPECTED_SHA256 --token-file /root/agent-token \
  --gost-version 3.3.0

Build the Agent from this reviewed repository for the target architecture first:
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o dist/msboost-agent-linux-amd64 ./cmd/agent
  sha256sum dist/msboost-agent-linux-amd64

The token file must be root-owned, mode 0600 or stricter, and contain ONLY the
corresponding executor token or relay enrollment token. Tokens are never echoed.
GOST 3.3.0 is the only currently audited/pinned version accepted by this installer.
Existing installations are changed only when the MSBOOST ownership marker exists.
Failed service starts restore the previous binary, service and environment file.
HELP
}
fail() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
capability=''; server=''; agent_file=''; agent_sha=''; token_file=''; gost_version=''
while (( $# )); do
  case "$1" in
    --help|-h) usage; exit 0 ;;
    --capability|--server|--agent|--agent-sha256|--token-file|--gost-version)
      (( $# >= 2 )) || fail "Missing value for $1"
      case "$1" in
        --capability) capability=$2 ;;
        --server) server=${2%/} ;;
        --agent) agent_file=$2 ;;
        --agent-sha256) agent_sha=$2 ;;
        --token-file) token_file=$2 ;;
        --gost-version) gost_version=$2 ;;
      esac
      shift 2 ;;
    *) fail "Unknown option $1" ;;
  esac
done

[[ "$capability" == executor || "$capability" == relay ]] || fail 'Choose exactly one capability: executor or relay.'
[[ "$server" =~ ^https://[A-Za-z0-9][A-Za-z0-9.-]*(:[0-9]{1,5})?$ ]] || fail 'Use an HTTPS origin with a DNS hostname, without credentials, path or query.'
[[ "$agent_sha" =~ ^[a-fA-F0-9]{64}$ ]] || fail 'An explicit --agent-sha256 is required.'
[[ "$gost_version" == 3.3.0 ]] || fail 'Explicit --gost-version 3.3.0 is required; no latest downloads are used.'
[[ -f "$agent_file" && ! -L "$agent_file" ]] || fail 'The explicitly selected Agent binary must be a regular file.'
[[ -f "$token_file" && ! -L "$token_file" ]] || fail 'Provide a private regular --token-file.'
[[ "$(uname -s)" == Linux ]] || fail 'This systemd installer supports Linux only.'
[[ "$(id -u)" == 0 ]] || fail 'Run this installer as root.'
command -v systemctl >/dev/null || fail 'systemd is required.'
[[ "$(systemctl --version | awk 'NR==1 {print $2}')" -ge 247 ]] || fail 'systemd 247 or newer is required.'
[[ "$(stat -c %u "$token_file")" == 0 ]] || fail 'The token file must be owned by root.'
token_mode=$(stat -c %a "$token_file")
(( (8#$token_mode & 077) == 0 )) || fail 'The token file must not be accessible by group or others.'
token=$(< "$token_file")
[[ "$token" =~ ^[A-Za-z0-9_-]{32,256}$ ]] || fail 'The token file must contain only the selected capability token.'
agent_sha=${agent_sha,,}
[[ "$(sha256sum "$agent_file" | cut -d' ' -f1)" == "$agent_sha" ]] || fail 'Agent SHA256 verification failed.'

case "$(uname -m)" in
  x86_64) arch=amd64; expected_machine=62; gost_sha=676fb7f78d267b6ae73df719c0c7f2b565dde7147da935cfafbc1e1da558b6d5 ;;
  aarch64|arm64) arch=arm64; expected_machine=183; gost_sha=d03699e3f385d4ff5dad68046712adfcc7515325a064d2ab046e0bece30f8f8f ;;
  *) fail 'Supported architectures: amd64 and arm64.' ;;
esac
[[ "$(od -An -tx1 -N4 "$agent_file" | tr -d ' \n')" == 7f454c46 ]] || fail 'Agent must be a Linux ELF binary.'
[[ "$(od -An -tu2 -j18 -N2 "$agent_file" | tr -d ' \n')" == "$expected_machine" ]] || fail 'Agent architecture does not match this server.'

managed=/usr/local/libexec/msboost-agent
binary=/usr/local/bin/msboost-agent
envfile="/etc/msboost-${capability}.env"
unit="msboost-${capability}.service"
unitfile="/etc/systemd/system/${unit}"
for path in "$managed" "$binary" "$envfile" "$unitfile"; do
  [[ ! -L "$path" ]] || fail "Refusing symlink target: $path"
done
if [[ ! -f "$managed/managed-v1" ]]; then
  [[ ! -e "$binary" && ! -e "$envfile" && ! -e "$unitfile" && ! -e "$managed" ]] || fail 'Existing paths are not marked as managed by MSBOOST; inspect them before installing.'
else
  [[ "$(< "$managed/managed-v1")" == MSBOOST_AGENT_MANAGED_V1 ]] || fail 'The existing ownership marker does not match this installer.'
fi
stage=$(mktemp -d /tmp/msboost-agent.XXXXXX)
backup=''; mutation=0; committed=0; was_active=0
systemctl is-active --quiet "$unit" && was_active=1 || true
previous_enable_state=$(systemctl is-enabled "$unit" 2>/dev/null || true)
cleanup() {
  local code=$?
  trap - EXIT
  if (( mutation && ! committed )); then
    systemctl stop "$unit" >/dev/null 2>&1 || true
    systemctl disable "$unit" >/dev/null 2>&1 || true
    for key in binary environment unit; do
      case "$key" in binary) destination=$binary ;; environment) destination=$envfile ;; unit) destination=$unitfile ;; esac
      if [[ -f "$backup/$key" ]]; then cp -p -- "$backup/$key" "$destination"; else rm -f -- "$destination"; fi
    done
    systemctl daemon-reload >/dev/null 2>&1 || true
    case "$previous_enable_state" in
      enabled) systemctl enable "$unit" >/dev/null 2>&1 || true ;;
      enabled-runtime) systemctl enable --runtime "$unit" >/dev/null 2>&1 || true ;;
      masked) systemctl mask "$unit" >/dev/null 2>&1 || true ;;
      masked-runtime) systemctl mask --runtime "$unit" >/dev/null 2>&1 || true ;;
    esac
    if (( was_active )); then systemctl start "$unit" >/dev/null 2>&1 || true; fi
    printf 'Installation failed; prior files were restored. Private backup: %s\n' "$backup" >&2
  fi
  [[ "$stage" == /tmp/msboost-agent.* ]] && rm -rf -- "$stage"
  exit "$code"
}
trap cleanup EXIT

install -m 0755 "$agent_file" "$stage/msboost-agent"
[[ "$(sha256sum "$stage/msboost-agent" | cut -d' ' -f1)" == "$agent_sha" ]] || fail 'Staged Agent SHA256 verification failed.'
if [[ "$capability" == relay ]]; then
  command -v curl >/dev/null || fail 'Install curl and ca-certificates first.'
  command -v tar >/dev/null || fail 'Install tar first.'
  gost_url="https://github.com/go-gost/gost/releases/download/v3.3.0/gost_3.3.0_linux_${arch}.tar.gz"
  curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 --max-time 180 "$gost_url" -o "$stage/gost.tar.gz"
  printf '%s  %s\n' "$gost_sha" "$stage/gost.tar.gz" | sha256sum -c - >/dev/null
  tar -xzf "$stage/gost.tar.gz" -C "$stage" gost
  [[ -f "$stage/gost" && ! -L "$stage/gost" ]] || fail 'Pinned archive did not contain a regular gost binary.'
  "$stage/gost" -V >/dev/null
fi
printf 'MSBOOST_SERVER_URL=%s\n' "$server" > "$stage/environment"
if [[ "$capability" == executor ]]; then
  printf 'MSBOOST_EXECUTOR_TOKEN=%s\n' "$token" >> "$stage/environment"
  cat >> "$stage/environment" <<'PINS'
GOST_AMD64_URL=https://github.com/go-gost/gost/releases/download/v3.3.0/gost_3.3.0_linux_amd64.tar.gz
GOST_AMD64_SHA256=676fb7f78d267b6ae73df719c0c7f2b565dde7147da935cfafbc1e1da558b6d5
GOST_ARM64_URL=https://github.com/go-gost/gost/releases/download/v3.3.0/gost_3.3.0_linux_arm64.tar.gz
GOST_ARM64_SHA256=d03699e3f385d4ff5dad68046712adfcc7515325a064d2ab046e0bece30f8f8f
PINS
else
  printf 'MSBOOST_RELAY_ENROLLMENT_TOKEN=%s\n' "$token" >> "$stage/environment"
fi
unset token
cat > "$stage/unit" <<EOF
[Unit]
Description=MSBOOST ${capability} Agent
After=network-online.target
Wants=network-online.target
[Service]
Type=simple
DynamicUser=true
EnvironmentFile=${envfile}
ExecStart=${binary} --capability ${capability} --state-dir /var/lib/msboost-relay --gost-binary ${managed}/gost-v3.3.0
Restart=on-failure
RestartSec=5
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictSUIDSGID=true
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
UMask=0077
EOF
if [[ "$capability" == relay ]]; then
  cat >> "$stage/unit" <<'RELAY'
StateDirectory=msboost-relay
StateDirectoryMode=0700
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
AmbientCapabilities=CAP_NET_BIND_SERVICE
RELAY
else
  printf 'CapabilityBoundingSet=\n' >> "$stage/unit"
fi
printf '\n[Install]\nWantedBy=multi-user.target\n' >> "$stage/unit"

install -d -m 0700 /var/backups/msboost-agent
backup=$(mktemp -d "/var/backups/msboost-agent/${capability}.XXXXXXXX")
[[ ! -f "$binary" ]] || cp -p -- "$binary" "$backup/binary"
[[ ! -f "$envfile" ]] || cp -p -- "$envfile" "$backup/environment"
[[ ! -f "$unitfile" ]] || cp -p -- "$unitfile" "$backup/unit"
mutation=1
systemctl stop "$unit" >/dev/null 2>&1 || true
install -d -m 0755 "$managed"
printf 'MSBOOST_AGENT_MANAGED_V1\n' > "$managed/managed-v1"
if [[ "$capability" == relay ]]; then install -m 0755 "$stage/gost" "$managed/gost-v3.3.0"; fi
install -m 0755 "$stage/msboost-agent" "$binary"
install -m 0600 "$stage/environment" "$envfile"
install -m 0644 "$stage/unit" "$unitfile"
systemctl daemon-reload
systemctl enable "$unit" >/dev/null
systemctl restart "$unit"
sleep 3
systemctl is-active --quiet "$unit" || fail 'Agent service did not remain running; inspect journalctl after rollback.'
committed=1
printf 'Installed %s with verified Agent/GOST pins. Verify its live status in the control panel.\nPrivate pre-install backup: %s\n' "$unit" "$backup"
