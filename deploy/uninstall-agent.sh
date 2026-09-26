#!/usr/bin/env bash
# Remove only a proven MSBOOST-managed relay from this VPS. The panel must
# retire the matching identity first; no panel credentials are accepted here.
if [[ ${BASH_SOURCE[0]} == "$0" ]]; then set +xv; fi
relay_uninstall_fail() { printf '错误：%s\n' "$*" >&2; exit 1; }
relay_uninstall_regular() {
  [[ -f $1 && ! -L $1 && $(stat -c %h -- "$1") == 1 ]]
}
relay_uninstall_managed_manifest() (
  local dir=$1 owner=${2:-0} entry name
  [[ -d $dir && ! -L $dir && $(stat -c %u -- "$dir") == "$owner" ]] || return 1
  shopt -s nullglob dotglob
  for entry in "$dir"/*; do
    name=${entry##*/}
    case $name in
      managed-v1|gost-v3.3.0) relay_uninstall_regular "$entry" && [[ $(stat -c %u -- "$entry") == "$owner" ]] || return 1 ;;
      *) return 1 ;;
    esac
  done
)
relay_uninstall_private_dir() {
  local mode
  [[ -d $1 && ! -L $1 ]] || return 1
  mode=$(stat -c %a -- "$1") || return 1
  [[ $mode =~ ^[0-7]{3,4}$ && $((8#$mode & 0077)) == 0 ]]
}
relay_uninstall_state_manifest() (
  local dir=$1 entry name
  relay_uninstall_private_dir "$dir" || return 1
  shopt -s nullglob dotglob
  for entry in "$dir"/*; do
    name=${entry##*/}
    [[ ! -L $entry ]] || return 1
    case $name in
      relay-token.json|relay-v2-state.json|relay-v2.lock|traffic-v2-journal.json|traffic-journal.json)
        relay_uninstall_regular "$entry" || return 1 ;;
      recovery.sock) [[ -S $entry ]] || return 1 ;;
      rule-*.json)
        [[ $name =~ ^rule-[a-f0-9]{48}\.json$ ]] && relay_uninstall_regular "$entry" || return 1 ;;
      *) return 1 ;;
    esac
  done
)
relay_uninstall_state_path() {
  local public=$1 private=$2
  [[ ! -L $private ]] || return 1
  if [[ -L $public ]]; then
    [[ $(readlink -f -- "$public") == "$private" && -d $private ]] || return 1
    printf '%s\n' "$private"
  elif [[ -d $private ]]; then
    [[ ! -e $public ]] || return 1
    printf '%s\n' "$private"
  elif [[ -d $public && ! -L $public ]]; then
    printf '%s\n' "$public"
  else
    return 1
  fi
}
relay_uninstall_unit_preflight() {
  local file=$1 state=$2 owner=${3:-0}
  relay_uninstall_regular "$file" && [[ $(stat -c %u -- "$file") == "$owner" ]] || return 1
  [[ $(grep -c '^ExecStart=' "$file") == 1 ]] || return 1
  grep -Fxq 'Description=MSBOOST relay Agent' "$file" || return 1
  grep -Fxq 'EnvironmentFile=/etc/msboost-relay.env' "$file" || return 1
  grep -Fxq 'DynamicUser=true' "$file" || return 1
  grep -Fxq 'StateDirectory=msboost-relay' "$file" || return 1
  if [[ $state == /var/lib/private/msboost-relay ]]; then
    grep -Fxq 'ExecStart=/usr/local/bin/msboost-agent --capability relay --state-dir /var/lib/private/msboost-relay --gost-binary /usr/local/libexec/msboost-agent/gost-v3.3.0 --offline-policy keep_last' "$file"
  else
    grep -Fxq 'ExecStart=/usr/local/bin/msboost-agent --capability relay --state-dir /var/lib/msboost-relay --gost-binary /usr/local/libexec/msboost-agent/gost-v3.3.0 --offline-policy lease' "$file"
  fi
}
relay_uninstall_identity_preflight() {
  local state=$1 expected_id=$2 expected_server=$3 require_v2=${4:-0}
  relay_uninstall_state_manifest "$state" || return 1
  relay_uninstall_regular "$state/relay-token.json" || return 1
  [[ $require_v2 == 0 ]] || relay_uninstall_regular "$state/relay-v2-state.json" || return 1
  python3 - "$state" "$expected_id" "$expected_server" "$require_v2" <<'PY'
import json, os, sys
state, expected_id, expected_server, require_v2 = sys.argv[1:]
def unique(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError('duplicate JSON key')
        result[key] = value
    return result
def read(name):
    with open(os.path.join(state, name), 'rb') as stream:
        return json.load(stream, object_pairs_hook=unique)
try:
    token = read('relay-token.json')
    assert isinstance(token, dict) and token.get('agentId') == expected_id
    if require_v2 == '1':
        assert os.path.isfile(os.path.join(state, 'relay-v2-state.json'))
    if os.path.exists(os.path.join(state, 'relay-v2-state.json')):
        v2 = read('relay-v2-state.json')
        assert isinstance(v2, dict) and v2.get('schema') == 2
        assert v2.get('offlinePolicy') == 'keep_last'
        assert v2.get('agentId') == expected_id and v2.get('serverUrl') == expected_server
        assert v2.get('recoveryRequired') is not True
        records = v2.get('records')
        assert isinstance(records, dict)
        assert all(isinstance(record, dict) and record.get('state') == 'stopped'
                   and isinstance(record.get('command'), dict)
                   and record['command'].get('action') == 'revoke'
                   for record in records.values())
except (OSError, ValueError, AssertionError, TypeError):
    sys.exit(1)
PY
}
relay_uninstall_backup_manifest() (
  local dir=$1 owner=${2:-0} entry name
  relay_uninstall_private_dir "$dir" && [[ $(stat -c %u -- "$dir") == "$owner" ]] || return 1
  shopt -s nullglob dotglob
  for entry in "$dir"/*; do
    name=${entry##*/}
    [[ ! -L $entry ]] || return 1
    case $name in
      binary|environment|unit) relay_uninstall_regular "$entry" || return 1 ;;
      relay-state|failed-new-relay-state) relay_uninstall_state_manifest "$entry" || return 1 ;;
      *) return 1 ;;
    esac
  done
)
relay_uninstall_lock() {
  local parent=/run/msboost-agent-install path identity
  [[ -d /run && ! -L /run && $(stat -c %u -- /run) == 0 ]] || relay_uninstall_fail '锁目录父路径不可信。'
  [[ -d $parent ]] || mkdir -m 0700 -- "$parent" || relay_uninstall_fail '无法建立安装锁目录。'
  [[ -d $parent && ! -L $parent && $(stat -c '%u:%a' -- "$parent") == 0:700 ]] || relay_uninstall_fail '安装锁目录不可信。'
  path=$parent/install.lock
  if [[ ! -e $path && ! -L $path ]]; then
    (umask 077; set -o noclobber; : > "$path") 2>/dev/null || [[ -f $path ]] || relay_uninstall_fail '无法建立安装锁。'
  fi
  [[ -f $path && ! -L $path && $(stat -c '%u:%a:%h' -- "$path") == 0:600:1 ]] || relay_uninstall_fail '安装锁文件不可信。'
  identity=$(stat -c '%d:%i' -- "$path") || relay_uninstall_fail '无法读取安装锁。'
  exec {relay_uninstall_lock_fd}<>"$path" || relay_uninstall_fail '无法打开安装锁。'
  [[ $(stat -Lc '%d:%i' -- "/proc/$BASHPID/fd/$relay_uninstall_lock_fd") == "$identity" ]] || relay_uninstall_fail '安装锁在打开时被替换。'
  flock -n "$relay_uninstall_lock_fd" || relay_uninstall_fail '另一个 Agent 安装或清理正在进行。'
}

# Allow isolated tests to source pure validation helpers without changing the
# host's service, paths, shell options or locks.
if [[ ${BASH_SOURCE[0]} != "$0" ]]; then return 0; fi
set -Eeuo pipefail
umask 077
export LC_ALL=C PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
agent_id='' server='' acknowledge=0
while (( $# )); do
  case $1 in
    --agent-id|--server)
      (( $# >= 2 )) || relay_uninstall_fail "缺少 $1 参数。"
      if [[ $1 == --agent-id ]]; then agent_id=$2; else server=${2%/}; fi
      shift 2 ;;
    --acknowledge-stop) acknowledge=1; shift ;;
    *) relay_uninstall_fail "未知参数 $1。" ;;
  esac
done
[[ $agent_id =~ ^[a-f0-9]{40}$ ]] || relay_uninstall_fail '节点 ID 格式无效。'
[[ $server =~ ^https://[A-Za-z0-9][A-Za-z0-9.-]*(:[0-9]{1,5})?$ ]] || relay_uninstall_fail '必须指定原 HTTPS 控制面地址。'
[[ $acknowledge == 1 ]] || relay_uninstall_fail '须显式添加 --acknowledge-stop，确认本机连接会断开。'
[[ $(uname -s) == Linux && $(id -u) == 0 ]] || relay_uninstall_fail '仅可在目标 Linux VPS 以 root 运行。'
for tool in systemctl flock stat python3; do command -v "$tool" >/dev/null || relay_uninstall_fail "缺少 $tool。"; done
relay_uninstall_lock
managed=/usr/local/libexec/msboost-agent
binary=/usr/local/bin/msboost-agent
envfile=/etc/msboost-relay.env
unitfile=/etc/systemd/system/msboost-relay.service
unit=msboost-relay.service
marker=$managed/managed-v1
relay_uninstall_managed_manifest "$managed" || relay_uninstall_fail '受管程序目录包含外来文件或不安全链接。'
relay_uninstall_regular "$marker" && [[ $(stat -c %u -- "$marker") == 0 && $(< "$marker") == MSBOOST_AGENT_MANAGED_V1 ]] || relay_uninstall_fail '受管所有权标记缺失或无效，不清理。'
for path in "$binary" "$envfile" "$managed/gost-v3.3.0"; do
  relay_uninstall_regular "$path" && [[ $(stat -c %u -- "$path") == 0 ]] || relay_uninstall_fail "受管文件不存在或不可信：$path"
done
env_mode=$(stat -c %a -- "$envfile")
[[ $env_mode =~ ^[0-7]{3,4}$ && $((8#$env_mode & 0077)) == 0 ]] || relay_uninstall_fail '环境文件权限不安全。'
[[ $(grep -c '^MSBOOST_SERVER_URL=' "$envfile") == 1 ]] && grep -Fxq "MSBOOST_SERVER_URL=$server" "$envfile" || relay_uninstall_fail '目标控制面与本机受管配置不一致。'
[[ -d /var/lib/private && ! -L /var/lib/private ]] || relay_uninstall_fail 'systemd 私有状态父目录不可信。'
state=$(relay_uninstall_state_path /var/lib/msboost-relay /var/lib/private/msboost-relay) || relay_uninstall_fail '本机中转状态布局不可信。'
relay_uninstall_unit_preflight "$unitfile" "$state" || relay_uninstall_fail '本机服务单元不是受管 Relay 形态。'
[[ $(systemctl show "$unit" -p FragmentPath --value) == "$unitfile" && -z $(systemctl show "$unit" -p DropInPaths --value) ]] || relay_uninstall_fail 'systemd 实际单元路径或覆盖配置不匹配。'
require_v2=0
[[ $state != /var/lib/private/msboost-relay ]] || require_v2=1
relay_uninstall_identity_preflight "$state" "$agent_id" "$server" "$require_v2" || relay_uninstall_fail '本机节点身份、控制面或转发终态不匹配；不得清理可能仍在运行的业务。'
backups=()
if [[ -e /var/backups/msboost-agent || -L /var/backups/msboost-agent ]]; then
  [[ -d /var/backups && ! -L /var/backups && $(stat -c %u -- /var/backups) == 0 ]] || relay_uninstall_fail '私有备份父目录不可信。'
  relay_uninstall_private_dir /var/backups/msboost-agent && [[ $(stat -c %u -- /var/backups/msboost-agent) == 0 ]] || relay_uninstall_fail '私有备份路径不可信。'
  shopt -s nullglob
  for path in /var/backups/msboost-agent/relay.*; do
    [[ ${path##*/} =~ ^relay\.[A-Za-z0-9]{8}$ ]] && relay_uninstall_backup_manifest "$path" || relay_uninstall_fail '存在未受信的 Relay 备份，拒绝部分清理。'
    backups+=("$path")
  done
  shopt -u nullglob
fi
shared=0
for path in /etc/systemd/system/msboost-executor.service /etc/msboost-executor.env; do
  [[ ! -e $path && ! -L $path ]] || shared=1
done
systemctl is-active --quiet msboost-executor.service && shared=1 || true
printf '即将停止并删除本机受管节点 %s；旧连接会断开，且此步骤不能恢复。\n' "$agent_id"
systemctl stop "$unit" || relay_uninstall_fail '无法停止旧 Relay；没有删除状态。'
systemctl is-active --quiet "$unit" && relay_uninstall_fail '旧 Relay 仍在运行；没有删除状态。'
relay_uninstall_identity_preflight "$state" "$agent_id" "$server" "$require_v2" || relay_uninstall_fail '停止后状态变化或仍有未停止转发；没有删除状态。'
systemctl disable "$unit" >/dev/null || relay_uninstall_fail '无法禁用旧 Relay；状态尚未删除。'
rm -f -- "$unitfile" "$envfile"
systemctl daemon-reload
[[ $state == /var/lib/private/msboost-relay || $state == /var/lib/msboost-relay ]] || relay_uninstall_fail '状态路径超出受管范围。'
rm -rf -- "$state"
if [[ -L /var/lib/msboost-relay ]]; then
  [[ $(readlink -- /var/lib/msboost-relay) == private/msboost-relay || $(readlink -- /var/lib/msboost-relay) == /var/lib/private/msboost-relay ]] || relay_uninstall_fail '公开状态链接不可信。'
  rm -f -- /var/lib/msboost-relay
fi
for path in "${backups[@]}"; do
  [[ $path == /var/backups/msboost-agent/relay.* && ! -L $path ]] || relay_uninstall_fail '备份路径发生变化。'
  rm -rf -- "$path"
done
if [[ $shared == 0 ]]; then
  rm -f -- "$binary" "$managed/gost-v3.3.0" "$marker"
  rmdir -- "$managed" || relay_uninstall_fail '共享程序目录仍有其他文件，已保留以便人工核查。'
fi
printf '已清理本机受管 Relay 身份、状态、单元与同角色私有备份。'
if [[ $shared == 1 ]]; then printf '同机执行机仍在，已保留共享程序。'; fi
printf '\n请在控制面核对旧节点已退役；本脚本不会删除控制面记录或其他服务。\n'
