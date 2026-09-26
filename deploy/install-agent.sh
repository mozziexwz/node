#!/usr/bin/env bash
# Install the locally built MSBOOST Agent on an explicitly chosen Linux machine.
# No credentials, installer payloads or customer JSON are fetched from this script.
if [[ ${BASH_SOURCE[0]} == "$0" ]]; then set +xv; fi
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
新装 relay 默认启用 keep_last；可显式 --offline-policy lease 兼容旧链。
已有 lease 服务切换 keep_last 还需 --acknowledge-relay-restart。
已有 keep_last 身份可在维护窗口显式 --upgrade-in-place --acknowledge-relay-restart；
此模式不接受 --token-file，保留原身份和规则，但重启会中断现有连接。
已有 keep_last 状态会优先使用旧管理令牌，不能靠粘贴新注册令牌重新绑定控制面。
仅在旧转发允许全部断开时，显式传 --fresh-reset --acknowledge-relay-restart；
安装器只归档本项目受管的 Relay v2 状态，保留 root 私有备份后重新注册。
HELP
}
fail() { printf '错误：%s\n' "$*" >&2; exit 1; }
step() { printf '\n  → %s\n' "$*" >&2; }
install_agent_parse_args() {
capability=''; server=''; agent_file=''; agent_sha=''; token_file=''; gost_version=''; offline_policy=''; offline_policy_set=0; acknowledge_restart=0; fresh_reset=0; upgrade_in_place=0
while (( $# )); do
  case "$1" in
    --help|-h) usage; exit 0 ;;
    --acknowledge-relay-restart) acknowledge_restart=1; shift ;;
    --fresh-reset) fresh_reset=1; shift ;;
    --upgrade-in-place) upgrade_in_place=1; shift ;;
    --capability|--server|--agent|--agent-sha256|--token-file|--gost-version|--offline-policy)
      (( $# >= 2 )) || fail "缺少 $1 参数"
      case "$1" in
        --capability) capability=$2 ;;
        --server) server=${2%/} ;;
        --agent) agent_file=$2 ;;
        --agent-sha256) agent_sha=$2 ;;
        --token-file) token_file=$2 ;;
        --gost-version) gost_version=$2 ;;
        --offline-policy) offline_policy=$2; offline_policy_set=1 ;;
      esac
      shift 2 ;;
    *) fail "未知参数 $1" ;;
  esac
done
[[ "$capability" == executor || "$capability" == relay ]] || fail '请选择 executor（控制执行机）或 relay（中转节点）。'
if [[ $capability == relay ]]; then
  [[ -n $offline_policy ]] || offline_policy=keep_last
  [[ $fresh_reset == 0 || ( $offline_policy == keep_last && $acknowledge_restart == 1 ) ]] || fail '全新重置中转节点需要 keep_last，并同时传 --acknowledge-relay-restart 明确确认旧连接将断开。'
  [[ $upgrade_in_place == 0 || ( $fresh_reset == 0 && $offline_policy == keep_last && $acknowledge_restart == 1 && -z $token_file ) ]] || fail '原地升级仅用于已有 keep_last 中转节点：须传 --acknowledge-relay-restart，且不得传新注册令牌、--fresh-reset 或 lease。'
else
  [[ $offline_policy_set == 0 && $acknowledge_restart == 0 && $fresh_reset == 0 && $upgrade_in_place == 0 ]] || fail '中转离线策略、重启确认、原地升级和全新重置只适用于 relay。'
  offline_policy=lease
fi
[[ $offline_policy == lease || $offline_policy == keep_last ]] || fail 'offline-policy 仅允许 lease 或 keep_last。'
}

relay_v2_evidence_at() {
  [[ -e $1 || -L $1 || -e $2 || -L $2 ]] && return 0
  [[ -f $3 ]] && grep -Eq -- '--offline-policy([= ]+)keep_last' "$3"
}
relay_v2_evidence() { relay_v2_evidence_at /var/lib/msboost-relay/relay-v2-state.json /var/lib/private/msboost-relay/relay-v2-state.json /etc/systemd/system/msboost-relay.service; }
relay_binary_supports_v2() {
  local help
  help=$("$1" --help 2>&1) || return
  grep -Eq -- '^[[:space:]]*--?offline-policy([=[:space:]]|$)' <<< "$help"
}

# systemd 252 keeps /var/lib/msboost-relay as a root-owned symlink even
# inside the DynamicUser mount namespace. keep_last intentionally rejects
# symlink state paths, so use the exact underlying StateDirectory location.
# Both names identify the same existing state; do not move/chown user data or
# weaken the runtime's private-directory/link/owner checks.
relay_service_state_dir() {
  case "$1:$2" in
    relay:keep_last) printf '%s\n' /var/lib/private/msboost-relay ;;
    relay:lease|executor:lease) printf '%s\n' /var/lib/msboost-relay ;;
    *) return 1 ;;
  esac
}

agent_supported_debian() (
  [[ -r /etc/os-release ]] || return 1
  local ID VERSION_ID VERSION_CODENAME expected
  . /etc/os-release
  case ${VERSION_ID:-} in
    11) expected=bullseye ;;
    12) expected=bookworm ;;
    13) expected=trixie ;;
    *) return 1 ;;
  esac
  [[ ${ID:-} == debian && ${VERSION_CODENAME:-} == "$expected" ]]
)

relay_unit_uses_v2() {
  local line section='' count=0 policy=0 role=0 state_path=0 gost_path=0 i
  local -a words=()
  [[ -f $1 && ! -L $1 ]] || return 1
  while IFS= read -r line || [[ -n $line ]]; do
    [[ $line != *$'\r'* && $line != *\\ ]] || return 1
    [[ $line != \#* && $line != \;* && -n $line ]] || continue
    if [[ $line == \[*\] ]]; then section=$line; continue; fi
    if [[ $line =~ ^[[:space:]]*ExecStart[[:space:]]*= && $line != ExecStart=* ]]; then return 1; fi
    [[ $line != ExecStart=* ]] || {
      [[ $section == '[Service]' ]] || return 1
      ((count+=1)); [[ $count == 1 ]] || return 1
      read -r -a words <<< "${line#ExecStart=}"
      [[ ${words[0]:-} == /usr/local/bin/msboost-agent ]] || return 1
      for ((i=1; i<${#words[@]}; i+=2)); do
        (( i+1 < ${#words[@]} )) || return 1
        case ${words[i]} in
          --capability) [[ ${words[i+1]} == relay && $role == 0 ]] || return 1; role=1 ;;
          --offline-policy) [[ ${words[i+1]} == keep_last && $policy == 0 ]] || return 1; policy=1 ;;
          --state-dir) [[ $state_path == 0 && ( ${words[i+1]} == /var/lib/msboost-relay || ${words[i+1]} == /var/lib/private/msboost-relay ) ]] || return 1; state_path=1 ;;
          --gost-binary) [[ $gost_path == 0 && ${words[i+1]} == /usr/local/libexec/msboost-agent/gost-v3.3.0 ]] || return 1; gost_path=1 ;;
          *) return 1 ;;
        esac
      done
    }
  done < "$1"
  [[ $count == 1 && $role == 1 && $policy == 1 && $state_path == 1 && $gost_path == 1 ]]
}

# Resolve only systemd's two documented StateDirectory layouts. Never follow
# an arbitrary link supplied at either path into a third-party directory.
relay_state_location_at() {
  local public=$1 private=$2
  [[ ! -L $private ]] || return 1
  if [[ -L $public ]]; then
    [[ $(readlink -f -- "$public") == "$private" && -d $private ]] || return 1
    printf '%s\n' "$private"
  elif [[ -d $private ]]; then
    [[ ! -e $public ]] || return 1
    printf '%s\n' "$private"
  elif [[ -d $public ]]; then
    printf '%s\n' "$public"
  elif [[ -e $public || -L $public || -e $private || -L $private ]]; then
    return 1
  fi
}

relay_state_has_identity_at() {
  local dir=$1
  [[ -e $dir/relay-v2-state.json || -L $dir/relay-v2-state.json ]]
}

# A fresh reset archives the complete private directory. Reject unexpected
# entries, symlinks, hardlinks and nested directories so no other application
# data can be moved under an MSBOOST-sounding path.
relay_fresh_state_manifest_at() (
  local dir=$1 entry name mode
  [[ -d $dir && ! -L $dir ]] || return 1
  mode=$(stat -c %a -- "$dir") || return 1
  [[ $mode =~ ^[0-7]{3,4}$ && $((8#$mode & 0077)) == 0 ]] || return 1
  shopt -s nullglob dotglob
  for entry in "$dir"/*; do
    name=${entry##*/}
    [[ ! -L $entry ]] || return 1
    case $name in
      relay-token.json|relay-v2-state.json|relay-v2.lock|traffic-v2-journal.json|traffic-journal.json)
        [[ -f $entry && $(stat -c %h -- "$entry") == 1 ]] || return 1 ;;
      recovery.sock) [[ -S $entry ]] || return 1 ;;
      rule-*.json)
        # relayruntime.randomID is 24 random bytes encoded as 48 hex chars.
        [[ $name =~ ^rule-[a-f0-9]{48}\.json$ && -f $entry && $(stat -c %h -- "$entry") == 1 ]] || return 1 ;;
      *) return 1 ;;
    esac
  done
)

relay_unit_state_path() {
  local unit_path=$1 line i
  local -a words=()
  relay_unit_uses_v2 "$unit_path" || return 1
  while IFS= read -r line; do
    [[ $line == ExecStart=* ]] || continue
    read -r -a words <<< "${line#ExecStart=}"
    for ((i=1; i<${#words[@]}; i+=2)); do
      if [[ ${words[i]} == --state-dir ]]; then printf '%s\n' "${words[i+1]}"; return 0; fi
    done
  done < "$unit_path"
  return 1
}

relay_fresh_reset_preflight_at() {
  local public=$1 private=$2 old_unit=$3 marker=$4 requested=$5 state='' unit_state=''
  relay_reset_state=''
  state=$(relay_state_location_at "$public" "$private") || fail '旧中转状态路径含未受信链接或布局，拒绝重置。'
  [[ -n $state ]] || return 0
  if [[ $requested == 0 ]]; then
    if relay_state_has_identity_at "$state" || { [[ -e $state/relay-token.json || -L $state/relay-token.json ]] && relay_unit_uses_v2 "$old_unit"; }; then
      fail '检测到已有 Relay v2 身份。旧 keep_last 状态会优先使用旧管理令牌，粘贴新注册令牌不会重新绑定。若需全新注册，请确认所有旧转发可中断，再显式使用 --fresh-reset --acknowledge-relay-restart。'
    fi
    return 0
  fi
  [[ -f $marker && ! -L $marker && $(< "$marker") == MSBOOST_AGENT_MANAGED_V1 ]] || fail '旧 Agent 无本项目受管所有权标记，拒绝归档状态。'
  [[ $(stat -c %u -- "$marker") == 0 && -f $old_unit && ! -L $old_unit && $(stat -c %u -- "$old_unit") == 0 ]] || fail '旧 Agent 所有权不可信，拒绝归档状态。'
  unit_state=$(relay_unit_state_path "$old_unit") || fail '旧 Relay systemd 单元不是本项目受管的 keep_last 形态，拒绝归档状态。'
  [[ $(readlink -f -- "$unit_state") == "$state" ]] || fail '旧 Relay 单元与状态目录不一致，拒绝归档状态。'
  relay_fresh_state_manifest_at "$state" || fail '旧 Relay 状态目录包含外来文件、不安全链接或权限，拒绝移动；请人工核查。'
  relay_reset_state=$state
}

# A token file proves only /register succeeded. A nonempty durable control
# epoch is written by runV2 only after a successful authenticated /v2/sync;
# otherwise systemd can be active while the dashboard still says offline.
relay_registration_ready_at() {
  local dir=$1 policy=$2
  [[ -f $dir/relay-token.json && ! -L $dir/relay-token.json ]] || return 1
  [[ $policy == keep_last ]] || return 0
  [[ -f $dir/relay-v2-state.json && ! -L $dir/relay-v2-state.json ]] || return 1
  grep -Eq '"controlEpoch":"[A-Za-z0-9_.:-]+"' "$dir/relay-v2-state.json" || return 1
  ! grep -Eq '"recoveryRequired":true' "$dir/relay-v2-state.json"
}

# In-place v2 upgrade never consumes a new enrollment token. The old managed
# unit, private state, authenticated sync, and control origin must all agree
# before the first service stop and again immediately before mutation.
relay_in_place_upgrade_preflight_at() {
  local public=$1 private=$2 old_unit=$3 marker=$4 old_env=$5 expected_server=$6
  local state='' unit_state='' line count=0 origin_count=0 token_count=0 state_id='' token_id='' state_origin=''
  relay_upgrade_state=''; relay_upgrade_identity=''
  [[ -f $marker && ! -L $marker && $(< "$marker") == MSBOOST_AGENT_MANAGED_V1 && $(stat -c '%u:%h' -- "$marker") == 0:1 ]] || fail '旧 Agent 无可信受管标记，拒绝原地升级。'
  [[ -f $old_unit && ! -L $old_unit && $(stat -c '%u:%h' -- "$old_unit") == 0:1 ]] || fail '旧 Relay 单元所有权不可信，拒绝原地升级。'
  unit_state=$(relay_unit_state_path "$old_unit") || fail '旧 Relay 单元不是受管 keep_last 形态，拒绝原地升级。'
  state=$(relay_state_location_at "$public" "$private") || fail '旧 Relay 状态路径不可信，拒绝原地升级。'
  [[ $state == "$private" && $(readlink -f -- "$unit_state") == "$state" ]] || fail '旧 Relay 单元与私有状态目录不一致，拒绝原地升级。'
  relay_fresh_state_manifest_at "$state" || fail '旧 Relay 状态含外来文件或不安全链接，拒绝原地升级。'
  relay_registration_ready_at "$state" keep_last || fail '旧 Relay 尚未完成有效控制同步，或处于恢复核对；拒绝原地升级。'
  [[ -f $old_env && ! -L $old_env && $(stat -c '%u:%a:%h' -- "$old_env") == 0:600:1 ]] || fail '旧 Relay 环境文件权限或所有权不可信，拒绝原地升级。'
  while IFS= read -r line || [[ -n $line ]]; do
    ((count+=1))
    case "$line" in
      "MSBOOST_SERVER_URL=$expected_server") ((origin_count+=1)) ;;
      MSBOOST_RELAY_ENROLLMENT_TOKEN=*)
        [[ $line =~ ^MSBOOST_RELAY_ENROLLMENT_TOKEN=[A-Za-z0-9_-]{32,256}$ ]] || fail '旧 Relay 环境文件格式异常，拒绝原地升级。'
        ((token_count+=1)) ;;
      *) fail '旧 Relay 环境文件或控制面地址与请求不一致，拒绝原地升级。' ;;
    esac
  done < "$old_env"
  [[ $origin_count == 1 && $token_count -le 1 && $count -eq $((origin_count+token_count)) ]] || fail '旧 Relay 环境文件含重复或缺失字段，拒绝原地升级。'
  state_id=$(grep -oE '"agentId":"[A-Za-z0-9_.:-]+"' "$state/relay-v2-state.json" | head -n 1) || true
  token_id=$(grep -oE '"agentId":"[A-Za-z0-9_.:-]+"' "$state/relay-token.json" | head -n 1) || true
  state_origin=$(grep -oE '"serverUrl":"https://[A-Za-z0-9][A-Za-z0-9.-]*(:[0-9]{1,5})?"' "$state/relay-v2-state.json" | head -n 1) || true
  [[ -n $state_id && $state_id == "$token_id" && $state_origin == "\"serverUrl\":\"$expected_server\"" ]] || fail '旧 Relay 私有状态与注册身份或控制面地址不一致，拒绝原地升级。'
  relay_upgrade_state=$state
  relay_upgrade_identity=$state_id
}

relay_migration_preflight() {
  local role=$1 policy=$2 active=$3 acknowledged=$4 candidate=$5 evidence=0
  relay_v2_evidence && evidence=1 || true
  if [[ $role == relay ]]; then
    [[ $policy == keep_last || $evidence == 0 ]] || fail '已有 keep_last 状态，拒绝静默降级；请使用兼容 v2 的版本，不能删除状态或用旧库回滚。'
    [[ $policy != keep_last || $active == 0 || $acknowledged == 1 ]] || fail '此次升级会中断原连接；安排维护窗口后显式传 --acknowledge-relay-restart。'
  fi
  if [[ $evidence == 1 || $policy == keep_last ]]; then
    relay_binary_supports_v2 "$candidate" || fail '指定 Agent 不支持显式 v2 策略，拒绝覆盖本机共享程序（包括 executor 安装）；尚未停止原服务。'
  fi
}

relay_rollback_allowed() {
  local role=$1 prior=$2
  relay_v2_evidence || return 0
  [[ -f $prior/binary && ! -L $prior/binary ]] && relay_binary_supports_v2 "$prior/binary" || return 1
  [[ $role != relay ]] || relay_unit_uses_v2 "$prior/unit"
}

install_agent_cleanup() {
  local code=$?
  trap - EXIT
  if (( mutation && ! committed )); then
    systemctl stop "$unit" >/dev/null 2>&1 || true
    systemctl disable "$unit" >/dev/null 2>&1 || true
    if (( ${relay_reset_moved:-0} )); then
      local state_restore_ok=1
      if [[ -e $relay_reset_state || -L $relay_reset_state ]]; then
        if [[ -d $relay_reset_state && ! -L $relay_reset_state && ! -e $backup/failed-new-relay-state ]] && mv -T -- "$relay_reset_state" "$backup/failed-new-relay-state"; then
          :
        else
          state_restore_ok=0
        fi
      fi
      if (( state_restore_ok )) && [[ -d $backup/relay-state && ! -L $backup/relay-state && ! -e $relay_reset_state ]] && mv -T -- "$backup/relay-state" "$relay_reset_state"; then
        :
      else
        state_restore_ok=0
      fi
      if (( ! state_restore_ok )); then
        printf '全新重置失败且旧状态未能安全自动恢复；已停止服务，旧私有状态留在 %s。不要重复安装，请人工核查。\n' "$backup" >&2
        [[ "$stage" == /tmp/msboost-agent.* ]] && rm -rf -- "$stage"
        exit "$code"
      fi
    fi
    if ! relay_rollback_allowed "$capability" "$backup"; then
      printf '新 v2 状态已存在，禁止自动回滚到旧程序/lease 单元。已停用本次安装的服务，保留兼容新文件和全部状态；请用兼容 v2 版本修复。原私有备份：%s\n' "$backup" >&2
      [[ "$stage" == /tmp/msboost-agent.* ]] && rm -rf -- "$stage"
      exit "$code"
    fi
    local key destination
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

agent_install_lock_at() {
  local directory=$1 parent mode lock_path lock_identity
  parent=$(dirname -- "$directory") || return
  while :; do
    [[ -d $parent && ! -L $parent && $(stat -c %u -- "$parent") == 0 ]] || fail '安装锁父目录不是受保护的 root 目录。'
    mode=$(stat -c %a -- "$parent") || return
    [[ $mode =~ ^[0-7]{3,4}$ && $((8#$mode & 0022)) == 0 ]] || fail '安装锁父目录允许他人写入。'
    [[ $parent != / ]] || break
    parent=$(dirname -- "$parent") || return
  done
  [[ -d $directory ]] || mkdir -m 0700 -- "$directory" || [[ -d $directory ]] || return
  [[ ! -L $directory && $(stat -c '%u:%a' -- "$directory") == 0:700 ]] || fail '安装锁目录身份或权限无效。'
  lock_path=$directory/install.lock
  if [[ ! -e $lock_path && ! -L $lock_path ]]; then
    (umask 077; set -o noclobber; : > "$lock_path") 2>/dev/null || [[ -f $lock_path ]] || return
  fi
  [[ -f $lock_path && ! -L $lock_path && $(stat -c '%u:%a:%h' -- "$lock_path") == 0:600:1 ]] || fail '安装锁文件身份或权限无效。'
  lock_identity=$(stat -c '%d:%i' -- "$lock_path") || return
  exec {agent_install_lock_fd}<>"$lock_path" || return
  [[ $(stat -Lc '%d:%i' -- "/proc/$BASHPID/fd/$agent_install_lock_fd") == "$lock_identity" ]] || fail '安装锁被替换。'
  flock -n "$agent_install_lock_fd" || fail '另一个 Agent 安装或修复正在进行；没有停止任何服务。'
  # Never unlink this inode: both roles keep the same lock for the whole process.
}

# Sourcing exposes pure policy helpers to offline tests; no options, files,
# services, environment or shell settings change until this entrypoint runs.
if [[ ${BASH_SOURCE[0]} != "$0" ]]; then return 0; fi
set -Eeuo pipefail
umask 077
export LC_ALL=C
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
install_agent_parse_args "$@"
step '检查参数、系统与私有令牌文件'
[[ "$server" =~ ^https://[A-Za-z0-9][A-Za-z0-9.-]*(:[0-9]{1,5})?$ ]] || fail '请填写 HTTPS 域名地址，不要包含凭据、路径或查询参数。'
[[ "$agent_sha" =~ ^[a-fA-F0-9]{64}$ ]] || fail '必须通过 --agent-sha256 指定校验值。'
[[ "$gost_version" == 3.3.0 ]] || fail '必须指定 --gost-version 3.3.0；不使用 latest。'
[[ -f "$agent_file" && ! -L "$agent_file" ]] || fail 'Agent 程序必须是普通文件，不能是符号链接。'
if (( ! upgrade_in_place )); then
  [[ -f "$token_file" && ! -L "$token_file" ]] || fail '请提供私有的普通 --token-file 文件。'
fi
[[ "$(uname -s)" == Linux ]] || fail '该 systemd 安装器仅支持 Linux。'
[[ "$(id -u)" == 0 ]] || fail '请以 root 运行。'
agent_supported_debian || fail 'Agent 仅支持 Debian 11/12/13，不支持其他发行版或版本代号不一致的系统。'
command -v systemctl >/dev/null || fail '系统缺少 systemd。'
[[ "$(systemctl --version | awk 'NR==1 {print $2}')" -ge 247 ]] || fail '需要 systemd 247 或更高版本。'
command -v flock >/dev/null || fail '系统缺少 util-linux flock；拒绝无锁安装。'
agent_install_lock_at /run/msboost-agent-install
if (( ! upgrade_in_place )); then
  [[ "$(stat -c %u "$token_file")" == 0 ]] || fail '令牌文件必须归 root 所有。'
  token_mode=$(stat -c %a "$token_file")
  (( (8#$token_mode & 077) == 0 )) || fail '令牌文件不能允许用户组或其他用户访问，请设置 chmod 600。'
  token=$(< "$token_file")
  [[ "$token" =~ ^[A-Za-z0-9_-]{32,256}$ ]] || fail '令牌文件只能包含对应角色的注册令牌。'
fi
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
relay_reset_state=''; relay_reset_moved=0
if [[ $capability == relay ]]; then
  if (( upgrade_in_place )); then
    relay_in_place_upgrade_preflight_at /var/lib/msboost-relay /var/lib/private/msboost-relay "$unitfile" "$managed/managed-v1" "$envfile" "$server"
  else
    relay_fresh_reset_preflight_at /var/lib/msboost-relay /var/lib/private/msboost-relay "$unitfile" "$managed/managed-v1" "$fresh_reset"
  fi
fi
backup=''; mutation=0; committed=0; was_active=0
systemctl is-active --quiet "$unit" && was_active=1 || true
stage=$(mktemp -d /tmp/msboost-agent.XXXXXX)
previous_enable_state=$(systemctl is-enabled "$unit" 2>/dev/null || true)
trap install_agent_cleanup EXIT

step '准备已校验的 Agent 与运行依赖'
install -m 0755 "$agent_file" "$stage/msboost-agent"
[[ "$(sha256sum "$stage/msboost-agent" | cut -d' ' -f1)" == "$agent_sha" ]] || fail '暂存 Agent 的 SHA-256 校验失败。'
# Execute capability probes only from the verified private copy, never from a
# caller-supplied file that could change after its initial checksum was read.
relay_migration_preflight "$capability" "$offline_policy" "$was_active" "$acknowledge_restart" "$stage/msboost-agent"
if [[ $offline_policy == keep_last ]]; then
  relay_binary_supports_v2 "$stage/msboost-agent" || fail '指定 Agent 不支持显式 v2 策略；尚未停止原服务，请选择兼容发布版本。'
fi
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
  if (( ! upgrade_in_place )); then printf 'MSBOOST_RELAY_ENROLLMENT_TOKEN=%s\n' "$token" >> "$stage/environment"; fi
fi
unset token
relay_policy_arg=''
if [[ $capability == relay ]]; then relay_policy_arg=" --offline-policy $offline_policy"; fi
relay_state_dir=$(relay_service_state_dir "$capability" "$offline_policy")
cat > "$stage/unit" <<EOF
[Unit]
Description=MSBOOST ${capability} Agent
After=network-online.target
Wants=network-online.target
[Service]
Type=simple
DynamicUser=true
EnvironmentFile=${envfile}
ExecStart=${binary} --capability ${capability} --state-dir ${relay_state_dir} --gost-binary ${managed}/gost-v3.3.0${relay_policy_arg}
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
[[ -d /var/backups && ! -L /var/backups && $(stat -c %u -- /var/backups) == 0 && ! -L /var/backups/msboost-agent ]] || fail 'Agent 备份父路径不是受信 root 目录。'
install -d -m 0700 /var/backups/msboost-agent
[[ -d /var/backups/msboost-agent && ! -L /var/backups/msboost-agent && $(stat -c '%u:%a' -- /var/backups/msboost-agent) == 0:700 ]] || fail 'Agent 私有备份目录所有权或权限不安全。'
backup=$(mktemp -d "/var/backups/msboost-agent/${capability}.XXXXXXXX")
[[ -d $backup && ! -L $backup && $(stat -c '%u:%a' -- "$backup") == 0:700 ]] || fail 'Agent 私有备份创建失败或权限不安全。'
[[ ! -f "$binary" ]] || cp -p -- "$binary" "$backup/binary"
[[ ! -f "$envfile" ]] || cp -p -- "$envfile" "$backup/environment"
[[ ! -f "$unitfile" ]] || cp -p -- "$unitfile" "$backup/unit"
# Runtime may persist its first v2 record during download/preparation. Recheck
# immediately before mutation while the cross-role installation lock is held.
was_active=0; systemctl is-active --quiet "$unit" && was_active=1 || true
relay_migration_preflight "$capability" "$offline_policy" "$was_active" "$acknowledge_restart" "$stage/msboost-agent"
if [[ $capability == relay ]]; then
  if (( upgrade_in_place )); then
    relay_in_place_upgrade_preflight_at /var/lib/msboost-relay /var/lib/private/msboost-relay "$unitfile" "$managed/managed-v1" "$envfile" "$server"
  else
    relay_fresh_reset_preflight_at /var/lib/msboost-relay /var/lib/private/msboost-relay "$unitfile" "$managed/managed-v1" "$fresh_reset"
  fi
fi
if [[ -n $relay_reset_state ]]; then
  [[ $relay_reset_state == /var/lib/msboost-relay || $relay_reset_state == /var/lib/private/msboost-relay ]] || fail '拒绝移动非受管状态路径。'
  [[ ! -L $(dirname -- "$relay_reset_state") && $(stat -c %u -- "$(dirname -- "$relay_reset_state")") == 0 ]] || fail '状态父目录不受 root 管理，拒绝移动。'
  [[ $(stat -c %d -- "$relay_reset_state") == "$(stat -c %d -- "$backup")" ]] || fail '旧状态和私有备份不在同一文件系统，拒绝非原子的重置。'
fi
mutation=1
if (( fresh_reset )) && [[ -n $relay_reset_state ]]; then
  systemctl stop "$unit" >/dev/null || fail '无法停止旧中转 Agent，状态未移动。'
  systemctl is-active --quiet "$unit" && fail '旧中转 Agent 仍在运行，拒绝移动状态。'
  relay_fresh_state_manifest_at "$relay_reset_state" || fail '停止服务后状态目录发生变化，拒绝移动。'
  mv -T -- "$relay_reset_state" "$backup/relay-state" || fail '旧 Relay 状态归档失败，未启动新服务。'
  relay_reset_moved=1
  # Keep systemd's public StateDirectory link from dangling while the new
  # DynamicUser service is prepared; systemd will set the runtime owner.
  mkdir -m 0700 -- "$relay_reset_state" || fail '无法创建空白受管状态目录，正尝试恢复旧状态。'
  printf '旧 Relay v2 状态已归档到 root 私有备份；现有转发连接已明确中断。\n' >&2
else
  systemctl stop "$unit" >/dev/null 2>&1 || true
fi
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
if [[ $capability == relay ]]; then
  registered=0
  for attempt in $(seq 1 30); do
    systemctl is-active --quiet "$unit" || fail '中转 Agent 在注册期间退出，将尝试回滚。'
    if relay_registration_ready_at "$relay_state_dir" "$offline_policy"; then registered=1; break; fi
    sleep 1
  done
  (( registered )) || fail '中转 Agent 未完成注册及真实 v2 控制同步：可能是令牌失效、WAF 阻断、网络故障或恢复核对。进程 active 不代表面板在线；已停止本次安装并尝试恢复旧状态，请检查服务日志。'
  if (( upgrade_in_place )); then
    upgraded_id=$(grep -oE '"agentId":"[A-Za-z0-9_.:-]+"' "$relay_upgrade_state/relay-v2-state.json" | head -n 1) || true
    [[ -n $upgraded_id && $upgraded_id == "$relay_upgrade_identity" ]] || fail '原地升级后 Relay 身份发生变化；已停止新服务并尝试回滚，请检查私有备份和后台状态。'
  fi
fi
committed=1
printf '\n安装完成：%s\n程序和 GOST 已校验。请在控制面后台确认在线与注册状态。\n私有安装前备份：%s\n查看本机日志：journalctl -u %s -n 80 --no-pager\n请勿公开令牌或完整环境文件。\n' "$unit" "$backup" "$unit"
