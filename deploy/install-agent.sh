#!/usr/bin/env bash
# Install the locally built MSBOOST Agent on an explicitly chosen Linux machine.
# No credentials, installer payloads or customer JSON are fetched from this script.
set -Eeuo pipefail
umask 077
export LC_ALL=C
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

usage() {
  cat <<'HELP'
用法：sudo bash deploy/install-agent.sh \
  --capability executor|relay --server https://YOUR_DOMAIN \
  --agent /absolute/path/to/msboost-agent-linux-ARCH \
  --agent-sha256 EXPECTED_SHA256 --token-file /root/agent-token \
  --gost-version 3.3.0

通常请使用仓库根 agent.sh 下载预构建版本；仅源码部署时先为目标架构构建：
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o dist/msboost-agent-linux-amd64 ./cmd/agent
  sha256sum dist/msboost-agent-linux-amd64

令牌文件必须归 root 所有，权限 0600 或更严格，只包含对应角色的注册令牌。
安装过程不会回显令牌。GOST 固定为已审核的 3.3.0，不使用 latest。
只更新带 MSBOOST 所有权标记的安装；服务启动失败时尝试恢复原程序、服务与配置。
HELP
}
fail() { printf '错误：%s\n' "$*" >&2; exit 1; }
step() { printf '\n  → %s\n' "$*" >&2; }
capability=''; server=''; agent_file=''; agent_sha=''; token_file=''; gost_version=''
while (( $# )); do
  case "$1" in
    --help|-h) usage; exit 0 ;;
    --capability|--server|--agent|--agent-sha256|--token-file|--gost-version)
      (( $# >= 2 )) || fail "缺少 $1 参数"
      case "$1" in
        --capability) capability=$2 ;;
        --server) server=${2%/} ;;
        --agent) agent_file=$2 ;;
        --agent-sha256) agent_sha=$2 ;;
        --token-file) token_file=$2 ;;
        --gost-version) gost_version=$2 ;;
      esac
      shift 2 ;;
    *) fail "未知参数 $1" ;;
  esac
done

step '检查参数、系统与私有令牌文件'
[[ "$capability" == executor || "$capability" == relay ]] || fail '请选择 executor（控制执行机）或 relay（中转节点）。'
[[ "$server" =~ ^https://[A-Za-z0-9][A-Za-z0-9.-]*(:[0-9]{1,5})?$ ]] || fail '请填写 HTTPS 域名地址，不要包含凭据、路径或查询参数。'
[[ "$agent_sha" =~ ^[a-fA-F0-9]{64}$ ]] || fail '必须通过 --agent-sha256 指定校验值。'
[[ "$gost_version" == 3.3.0 ]] || fail '必须指定 --gost-version 3.3.0；不使用 latest。'
[[ -f "$agent_file" && ! -L "$agent_file" ]] || fail 'Agent 程序必须是普通文件，不能是符号链接。'
[[ -f "$token_file" && ! -L "$token_file" ]] || fail '请提供私有的普通 --token-file 文件。'
[[ "$(uname -s)" == Linux ]] || fail '该 systemd 安装器仅支持 Linux。'
[[ "$(id -u)" == 0 ]] || fail '请以 root 运行。'
command -v systemctl >/dev/null || fail '系统缺少 systemd。'
[[ "$(systemctl --version | awk 'NR==1 {print $2}')" -ge 247 ]] || fail '需要 systemd 247 或更高版本。'
[[ "$(stat -c %u "$token_file")" == 0 ]] || fail '令牌文件必须归 root 所有。'
token_mode=$(stat -c %a "$token_file")
(( (8#$token_mode & 077) == 0 )) || fail '令牌文件不能允许用户组或其他用户访问，请设置 chmod 600。'
token=$(< "$token_file")
[[ "$token" =~ ^[A-Za-z0-9_-]{32,256}$ ]] || fail '令牌文件只能包含对应角色的注册令牌。'
agent_sha=${agent_sha,,}
[[ "$(sha256sum "$agent_file" | cut -d' ' -f1)" == "$agent_sha" ]] || fail 'Agent SHA-256 校验失败。'

case "$(uname -m)" in
  x86_64) arch=amd64; expected_machine=62; gost_sha=676fb7f78d267b6ae73df719c0c7f2b565dde7147da935cfafbc1e1da558b6d5 ;;
  aarch64|arm64) arch=arm64; expected_machine=183; gost_sha=d03699e3f385d4ff5dad68046712adfcc7515325a064d2ab046e0bece30f8f8f ;;
  *) fail '当前仅支持 amd64 和 arm64 架构。' ;;
esac
[[ "$(od -An -tx1 -N4 "$agent_file" | tr -d ' \n')" == 7f454c46 ]] || fail 'Agent 必须是 Linux ELF 程序。'
[[ "$(od -An -tu2 -j18 -N2 "$agent_file" | tr -d ' \n')" == "$expected_machine" ]] || fail 'Agent 架构与本服务器不一致。'

managed=/usr/local/libexec/msboost-agent
binary=/usr/local/bin/msboost-agent
envfile="/etc/msboost-${capability}.env"
unit="msboost-${capability}.service"
unitfile="/etc/systemd/system/${unit}"
for path in "$managed" "$binary" "$envfile" "$unitfile"; do
  [[ ! -L "$path" ]] || fail "拒绝覆盖符号链接：$path"
done
if [[ ! -f "$managed/managed-v1" ]]; then
  [[ ! -e "$binary" && ! -e "$envfile" && ! -e "$unitfile" && ! -e "$managed" ]] || fail '已有文件不属于 MSBOOST 受管安装，拒绝接管；请先核查。'
else
  [[ "$(< "$managed/managed-v1")" == MSBOOST_AGENT_MANAGED_V1 ]] || fail '已有安装的所有权标记不正确。'
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
    printf '安装失败，已尝试恢复原文件与服务。请核查服务状态；私有备份：%s\n' "$backup" >&2
  fi
  [[ "$stage" == /tmp/msboost-agent.* ]] && rm -rf -- "$stage"
  exit "$code"
}
trap cleanup EXIT

step '准备已校验的 Agent 与运行依赖'
install -m 0755 "$agent_file" "$stage/msboost-agent"
[[ "$(sha256sum "$stage/msboost-agent" | cut -d' ' -f1)" == "$agent_sha" ]] || fail '暂存 Agent 的 SHA-256 校验失败。'
if [[ "$capability" == relay ]]; then
  command -v curl >/dev/null || fail '请先安装 curl 与 ca-certificates。'
  command -v tar >/dev/null || fail '请先安装 tar。'
  gost_url="https://github.com/go-gost/gost/releases/download/v3.3.0/gost_3.3.0_linux_${arch}.tar.gz"
  curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 --max-time 180 "$gost_url" -o "$stage/gost.tar.gz" || fail 'GOST 下载失败，请检查服务器访问 GitHub 的网络。'
  printf '%s  %s\n' "$gost_sha" "$stage/gost.tar.gz" | sha256sum -c - >/dev/null || fail 'GOST SHA-256 校验失败。'
  tar -xzf "$stage/gost.tar.gz" -C "$stage" gost
  [[ -f "$stage/gost" && ! -L "$stage/gost" ]] || fail 'GOST 发布包缺少正确程序文件。'
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

step '保存私有回滚备份并安装服务'
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
step '启动服务并检查本机运行状态'
systemctl restart "$unit"
sleep 3
systemctl is-active --quiet "$unit" || fail 'Agent 未保持运行，将尝试回滚；请随后检查服务日志。'
committed=1
printf '\n安装完成：%s\n程序和 GOST 已校验。请在控制面后台确认在线与注册状态。\n私有安装前备份：%s\n查看本机日志：journalctl -u %s -n 80 --no-pager\n请勿公开令牌或完整环境文件。\n' "$unit" "$backup" "$unit"
