#!/usr/bin/env bash
# Local root explicitly authorizes stopping this MSBOOST-managed relay.
# Panel connectivity, retirement and durable forwarding state are not gates.
# Preserve private recovery material; never remove another role's programs.
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
      managed-v1|gost-v3.3.0|relay-firewall) relay_uninstall_regular "$entry" && [[ $(stat -c %u -- "$entry") == "$owner" ]] || return 1 ;;
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
      relay-token.json|relay-enrollment.json|relay-v2-state.json|relay-health.json|relay-v2.lock|traffic-v2-journal.json|traffic-journal.json)
        relay_uninstall_regular "$entry" || return 1 ;;
      .msboost-*|.relay-v2-*)
        [[ $name =~ ^\.(msboost|relay-v2)-[0-9]+$ ]] && relay_uninstall_regular "$entry" || return 1 ;;
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
        # This is a destructive local stop, not an online retirement proof.
        # ready/failed/recovery/unknown records must not trap the VPS owner.
except (OSError, ValueError, AssertionError, TypeError):
    sys.exit(1)
PY
}
relay_uninstall_stopped() {
  local unit=$1 active
  active=$(systemctl show "$unit" -p ActiveState --value) || return 1
  [[ $active == inactive || $active == failed ]] || return 1
  [[ $(systemctl show "$unit" -p MainPID --value) == 0 &&
     $(systemctl show "$unit" -p ControlPID --value) == 0 &&
     -z $(systemctl show "$unit" -p ControlGroup --value) ]]
}
relay_uninstall_help() {
  printf '%s\n' \
    '用法：uninstall-agent.sh --agent-id 节点ID --server https://面板域名 --acknowledge-stop [--check]' \
    '只在目标节点 VPS 以 root 运行。确认后会停止本机 Relay 及全部转发。' \
    '不要求面板在线、先退役或转发终态；但节点身份、路径归属必须匹配，进程必须真正停止。' \
    '自动保留 root 私有备份及既有备份；不删除控制面记录、执行机或其他服务。' \
    '--check 仅检查本机身份和受管文件，不停止服务、不删除文件。'
}
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
agent_id='' server='' acknowledge=0 check_only=0
while (( $# )); do
  case $1 in
    --agent-id|--server)
      (( $# >= 2 )) || relay_uninstall_fail "缺少 $1 参数。"
      if [[ $1 == --agent-id ]]; then agent_id=$2; else server=${2%/}; fi
      shift 2 ;;
    --acknowledge-stop) acknowledge=1; shift ;;
    --check) check_only=1; shift ;;
    --help|-h) relay_uninstall_help; exit 0 ;;
    *) relay_uninstall_fail "未知参数 $1。" ;;
  esac
done
[[ $agent_id =~ ^[a-f0-9]{40}$ ]] || relay_uninstall_fail '节点 ID 格式无效。'
[[ $server =~ ^https://[A-Za-z0-9][A-Za-z0-9.-]*(:[0-9]{1,5})?$ ]] || relay_uninstall_fail '必须指定原 HTTPS 控制面地址。'
[[ $acknowledge == 1 || $check_only == 1 ]] || relay_uninstall_fail '须显式添加 --acknowledge-stop，确认本机连接会断开。'
[[ $(uname -s) == Linux && $(id -u) == 0 ]] || relay_uninstall_fail '仅可在目标 Linux VPS 以 root 运行。'
for tool in systemctl flock stat python3 tar; do command -v "$tool" >/dev/null || relay_uninstall_fail "缺少 $tool。"; done
if (( ! check_only )); then relay_uninstall_lock; fi
managed=/usr/local/libexec/msboost-agent
binary=/usr/local/bin/msboost-agent
envfile=/etc/msboost-relay.env
unitfile=/etc/systemd/system/msboost-relay.service
unit=msboost-relay.service
firewall_unit=/etc/systemd/system/msboost-relay-firewall.service
firewall_paths=()
if [[ -e $firewall_unit || -L $firewall_unit ]]; then
  relay_uninstall_regular "$firewall_unit" && [[ $(stat -c %u "$firewall_unit") == 0 && $((8#$(stat -c %a "$firewall_unit") & 0022)) == 0 ]] &&
    grep -Fxq '# MSBOOST_RELAY_FIREWALL_MANAGED_V1' "$firewall_unit" &&
    grep -Fxq 'ExecStart=/usr/local/libexec/msboost-agent/relay-firewall --capability relay-firewall' "$firewall_unit" &&
    [[ $(grep -c '^ExecStart=' "$firewall_unit") == 1 && -z $(systemctl show msboost-relay-firewall.service -p DropInPaths --value) ]] &&
    ! grep -Eq '^[[:space:]]*Exec(StartPre|StartPost|Stop|StopPost|Reload|Condition)[[:space:]]*=' "$firewall_unit" || relay_uninstall_fail '防火墙维护单元归属不明确，没有停止服务。'
  relay_uninstall_regular "$managed/relay-firewall" && [[ $(stat -c %u "$managed/relay-firewall") == 0 && $((8#$(stat -c %a "$managed/relay-firewall") & 0022)) == 0 ]] || relay_uninstall_fail '防火墙维护程序不可信，没有停止服务。'
  firewall_paths+=("${firewall_unit#/}")
fi
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
[[ $(systemctl show "$unit" -p KillMode --value) == control-group ]] || relay_uninstall_fail '服务不是整组停止模式，无法保证转发子进程停止。'
require_v2=0
[[ $state != /var/lib/private/msboost-relay ]] || require_v2=1
relay_uninstall_state_manifest "$state" || relay_uninstall_fail '状态目录包含未识别文件或不安全链接；尚未停止服务。请检查目录文件归属。'
relay_uninstall_identity_preflight "$state" "$agent_id" "$server" "$require_v2" || relay_uninstall_fail '指定节点 ID 或面板地址与本机身份不符，或身份文件已损坏；尚未停止服务。请核对命令是否属于这台 VPS。'
if (( check_only )); then
  printf '检查通过：本机节点 %s，面板 %s。未停止服务、未清理文件。\n' "$agent_id" "$server"
  exit 0
fi
[[ -d /var/backups && ! -L /var/backups && $(stat -c %u -- /var/backups) == 0 ]] || relay_uninstall_fail '私有备份父目录不可信。'
if [[ ! -e /var/backups/msboost-agent && ! -L /var/backups/msboost-agent ]]; then
  mkdir -m 0700 -- /var/backups/msboost-agent
fi
relay_uninstall_private_dir /var/backups/msboost-agent && [[ $(stat -c %u -- /var/backups/msboost-agent) == 0 ]] || relay_uninstall_fail '私有备份路径不可信。'
backup=$(mktemp -d /var/backups/msboost-agent/uninstall-relay.XXXXXXXX)
printf '卸载备份目录：%s（仅 root 可读，请勿公开）\n' "$backup"
shared=0
for path in /etc/systemd/system/msboost-executor.service /etc/msboost-executor.env; do
  [[ ! -e $path && ! -L $path ]] || shared=1
done
systemctl is-active --quiet msboost-executor.service && shared=1 || true
printf '即将停止并卸载本机受管节点 %s；全部旧连接会断开。不会等待面板或转发终态。\n' "$agent_id"
systemctl stop "$unit" || relay_uninstall_fail '无法停止旧 Relay；没有删除状态。'
relay_uninstall_stopped "$unit" || relay_uninstall_fail 'Relay 或转发子进程尚未完全退出；没有删除文件。'
relay_uninstall_state_manifest "$state" || relay_uninstall_fail '停止后目录出现未识别文件或不安全链接；没有删除文件。'
relay_uninstall_identity_preflight "$state" "$agent_id" "$server" "$require_v2" || relay_uninstall_fail '停止后节点身份发生变化；没有删除文件。'
# Existing backups are outside the deletion scope and never gate uninstall.
tar -czf "$backup/relay.tar.gz" -C / -- "${unitfile#/}" "${envfile#/}" "${binary#/}" "${managed#/}" "${state#/}" "${firewall_paths[@]}" || relay_uninstall_fail '无法保存完整私有备份；服务已停止，但没有删除文件。'
tar -tzf "$backup/relay.tar.gz" >/dev/null || relay_uninstall_fail '私有备份校验失败；服务已停止，但没有删除文件。'
systemctl disable "$unit" >/dev/null || relay_uninstall_fail '无法禁用旧 Relay；状态尚未删除。'
if (( ${#firewall_paths[@]} )); then
  systemctl stop msboost-relay-firewall.service || relay_uninstall_fail '维护服务停止失败，已保留组件和备份。'
  "$managed/relay-firewall" --capability relay-firewall-cleanup || relay_uninstall_fail '本项目防火墙规则尚未清理，已保留组件和备份；不删除其他规则。'
  systemctl disable msboost-relay-firewall.service >/dev/null || relay_uninstall_fail '维护服务禁用失败，已保留组件。'
  rm -f -- "$firewall_unit" "$managed/relay-firewall"
fi
rm -f -- "$unitfile" "$envfile"
systemctl daemon-reload
[[ $state == /var/lib/private/msboost-relay || $state == /var/lib/msboost-relay ]] || relay_uninstall_fail '状态路径超出受管范围。'
rm -rf -- "$state"
if [[ -L /var/lib/msboost-relay ]]; then
  [[ $(readlink -- /var/lib/msboost-relay) == private/msboost-relay || $(readlink -- /var/lib/msboost-relay) == /var/lib/private/msboost-relay ]] || relay_uninstall_fail '公开状态链接不可信。'
  rm -f -- /var/lib/msboost-relay
fi
if [[ $shared == 0 ]]; then
  rm -f -- "$binary" "$managed/gost-v3.3.0" "$marker"
  rmdir -- "$managed" || relay_uninstall_fail '共享程序目录仍有其他文件，已保留以便人工核查。'
fi
printf '已停止并清理本机受管 Relay 身份、状态与单元；既有私有备份全部保留。'
if [[ $shared == 1 ]]; then printf '同机执行机仍在，已保留共享程序。'; fi
printf '\n本次卸载备份：%s/relay.tar.gz\n' "$backup"
printf '本脚本不会删除控制面记录或其他服务。若后台仍有该节点，请处理关联业务并删除或封存；本机卸载不会自动释放面板预留端口。\n'
