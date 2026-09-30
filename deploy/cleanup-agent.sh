#!/usr/bin/env bash
# Explicit local-root cleanup of ONE managed role, including missing identity
# files. No panel availability or forwarding retirement proof is required.
# Unknown ownership, symlinks, drop-ins and outside paths still fail closed.
set +xv
set -Eeuo pipefail
umask 077
export LC_ALL=C PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
fail() { printf '错误：%s\n' "$*" >&2; exit 1; }
role= mode=check
while (( $# )); do
  case $1 in
    --capability) (( $# >= 2 )) || fail '缺少角色'; role=$2; shift 2 ;;
    --check) mode=check; shift ;;
    --cleanup) mode=cleanup; shift ;;
    --help|-h) printf '%s\n' 'cleanup-agent.sh --capability relay|executor [--check|--cleanup]' '默认只扫描。清理需在本机 root 终端核对清单并输入两次确认；保留私有备份和共享程序。'; exit 0 ;;
    *) fail "未知参数 $1" ;;
  esac
done
[[ $role == relay || $role == executor ]] || fail '请选择 relay（中转节点）或 executor（执行机）'
[[ $(uname -s) == Linux && $(id -u) == 0 ]] || fail '仅在目标 Linux VPS 以 root 运行'
for tool in systemctl stat flock tar find readlink sha256sum mountpoint; do command -v "$tool" >/dev/null || fail "缺少 $tool"; done
regular() { [[ -f $1 && ! -L $1 && $(stat -c '%u:%h' "$1") == 0:1 ]]; }
safe_parent() { local p=$1; while [[ $p != / ]]; do [[ ! -L $p ]] || return 1; if [[ -e $p ]]; then [[ -d $p && $(stat -c %u "$p") == 0 && -z $(find "$p" -maxdepth 0 -perm /022 -print) ]] || return 1; fi; p=${p%/*}; [[ -n $p ]] || p=/; done; }
managed=/usr/local/libexec/msboost-agent
marker=$managed/managed-v1
safe_parent "$managed" && regular "$marker" && [[ $(<"$marker") == MSBOOST_AGENT_MANAGED_V1 ]] || fail '找不到可信受管标记；不会删除外来组件'
unit=msboost-$role.service
unitfile=/etc/systemd/system/$unit
envfile=/etc/msboost-$role.env
safe_parent /etc/systemd/system && safe_parent /var/lib && safe_parent /var/lib/private || fail '受管父目录不可信'
paths=()
managed_files=("$unitfile" "$envfile")
firewall_unit=/etc/systemd/system/msboost-relay-firewall.service
if [[ $role == relay && ( -e $firewall_unit || -L $firewall_unit ) ]]; then
  regular "$firewall_unit" && [[ $((8#$(stat -c %a "$firewall_unit") & 0022)) == 0 ]] && grep -Fxq '# MSBOOST_RELAY_FIREWALL_MANAGED_V1' "$firewall_unit" &&
    grep -Fxq 'ExecStart=/usr/local/libexec/msboost-agent/relay-firewall --capability relay-firewall' "$firewall_unit" &&
    [[ $(grep -c '^ExecStart=' "$firewall_unit") == 1 && -z $(systemctl show msboost-relay-firewall.service -p DropInPaths --value) ]] &&
    ! grep -Eq '^[[:space:]]*Exec(StartPre|StartPost|Stop|StopPost|Reload|Condition)[[:space:]]*=' "$firewall_unit" || fail '防火墙维护单元归属不明确，未清理'
  regular "$managed/relay-firewall" && [[ $((8#$(stat -c %a "$managed/relay-firewall") & 0022)) == 0 ]] || fail '维护程序归属不明确，未清理'
  managed_files+=("$firewall_unit" "$managed/relay-firewall")
fi
declare -A original_hash=()
for p in "${managed_files[@]}"; do
  [[ ! -e $p && ! -L $p ]] && continue
  regular "$p" || fail "文件归属或链接不可信：$p"
  original_hash[$p]=$(sha256sum "$p" | cut -d' ' -f1)
  paths+=("$p")
done
unchanged_files() {
  local p
  for p in "${managed_files[@]}"; do
    if [[ -n ${original_hash[$p]:-} ]]; then regular "$p" && [[ $(sha256sum "$p" | cut -d' ' -f1) == "${original_hash[$p]}" ]] || return 1
    else [[ ! -e $p && ! -L $p ]] || return 1; fi
  done
}
fragment=$(systemctl show "$unit" -p FragmentPath --value) || fail '无法读取服务状态'
[[ -z $(systemctl show "$unit" -p DropInPaths --value) ]] || fail '服务有额外覆盖配置，不能自动清理未知配置'
if [[ -f $unitfile ]]; then
  grep -Fxq "Description=MSBOOST $role Agent" "$unitfile" && grep -Fxq "EnvironmentFile=$envfile" "$unitfile" && [[ $(grep -c '^ExecStart=' "$unitfile") == 1 ]] || fail '服务单元不是本项目受管形态'
  exec_line=$(grep '^ExecStart=' "$unitfile")
  if [[ $role == relay ]]; then
    [[ $exec_line == 'ExecStart=/usr/local/bin/msboost-agent --capability relay --state-dir /var/lib/private/msboost-relay --gost-binary /usr/local/libexec/msboost-agent/gost-v3.3.0 --offline-policy keep_last' || $exec_line == 'ExecStart=/usr/local/bin/msboost-agent --capability relay --state-dir /var/lib/msboost-relay --gost-binary /usr/local/libexec/msboost-agent/gost-v3.3.0 --offline-policy lease' ]] || fail '中转服务执行参数不匹配'
  else
    [[ $exec_line == 'ExecStart=/usr/local/bin/msboost-agent --capability executor --state-dir /var/lib/msboost-relay --gost-binary /usr/local/libexec/msboost-agent/gost-v3.3.0' ]] || fail '执行机服务执行参数不匹配'
  fi
  grep -Fxq 'DynamicUser=true' "$unitfile" || fail '服务不是受管私有用户形态'
  ! grep -Eq '^[[:space:]]*(Exec(StartPre|StartPost|Stop|StopPost|Reload|Condition)|RootDirectory|RootImage|BindPaths|BindReadOnlyPaths|EnvironmentFile)[[:space:]]*=' <(grep -vFx "EnvironmentFile=$envfile" "$unitfile") || fail '服务包含自定义执行钩子或路径映射，未自动清理'
  [[ $fragment == "$unitfile" && $(systemctl show "$unit" -p KillMode --value) == control-group ]] || fail '实际服务或整组停止模式不匹配'
else
  [[ -z $fragment && $(systemctl show "$unit" -p MainPID --value) == 0 && $(systemctl show "$unit" -p ControlPID --value) == 0 && -z $(systemctl show "$unit" -p ControlGroup --value) ]] || fail '单元缺失但仍有未知受管进程，未清理'
fi
public=/var/lib/msboost-$role
private=/var/lib/private/msboost-$role
state=
if [[ -L $public ]]; then
  [[ -d $private && ! -L $private && $(readlink -f "$public") == "$private" ]] || fail '状态链接指向受管范围外'
  state=$private
elif [[ -e $public ]]; then
  [[ -d $public && ! -L $public && ! -e $private && ! -L $private ]] || fail '状态布局不明确'
  state=$public
elif [[ -e $private || -L $private ]]; then
  [[ -d $private && ! -L $private ]] || fail '私有状态路径不可信'
  state=$private
fi
scan_state() {
  [[ -n $state ]] || return 0
  [[ -d $state && ! -L $state && $((8#$(stat -c %a "$state") & 0077)) == 0 ]] || return 1
  ! mountpoint -q "$state" || return 1
  local p name
  while IFS= read -r -d '' p; do
    name=${p##*/}
    [[ ! -L $p ]] || return 1
    case $name in
      relay-token.json|relay-enrollment.json|relay-v2-state.json|relay-health.json|relay-v2.lock|traffic-v2-journal.json|traffic-journal.json) [[ -f $p && $(stat -c %h "$p") == 1 ]] || return 1 ;;
      rule-*.json) [[ $name =~ ^rule-[a-f0-9]{48}\.json$ && -f $p && $(stat -c %h "$p") == 1 ]] || return 1 ;;
      .msboost-*|.relay-v2-*) [[ $name =~ ^\.(msboost|relay-v2)-[0-9]+$ && -f $p && $(stat -c %h "$p") == 1 ]] || return 1 ;;
      recovery.sock) [[ -S $p ]] || return 1 ;;
      *) return 1 ;;
    esac
  done < <(find "$state" -mindepth 1 -maxdepth 1 -print0)
}
scan_state || fail '状态包含未知文件、不安全链接或非私有权限，未停止或删除服务'
[[ -z $state ]] || paths+=("$state")
printf '本机：%s；清理角色：%s\n' "$(hostname)" "$role"
printf '将处理的受管路径：\n'
if (( ${#paths[@]} )); then printf '  %s\n' "${paths[@]}"; else printf '  未发现此角色的残留组件\n'; exit 0; fi
printf '%s\n' '共享 Agent / GOST、另一个角色、其他应用和以前的私有备份均保留。'
[[ $mode == cleanup ]] || { printf '扫描完成，尚未停止服务或删除文件。确认需要清理时改用 --cleanup。\n'; exit 0; }
[[ -r /dev/tty && -w /dev/tty ]] || fail '清理必须从本机交互终端执行，不接受管道确认'
printf '清理会断开此角色的连接。输入 CLEAN_MSBOOST_%s 确认：' "${role^^}" >/dev/tty
IFS= read -r confirm </dev/tty
[[ $confirm == CLEAN_MSBOOST_${role^^} ]] || fail '已取消'
printf '再次核对本机和清单，输入主机名 %s：' "$(hostname)" >/dev/tty
IFS= read -r confirm </dev/tty
[[ $confirm == "$(hostname)" ]] || fail '已取消'
safe_parent /run || fail '锁父目录不可信'
lockdir=/run/msboost-agent-install
[[ -e $lockdir ]] || mkdir -m 0700 "$lockdir"
[[ -d $lockdir && ! -L $lockdir && $(stat -c '%u:%a' "$lockdir") == 0:700 ]] || fail '安装锁目录不可信'
lock=$lockdir/install.lock
[[ -e $lock || -L $lock ]] || (set -o noclobber; : > "$lock")
regular "$lock" && [[ $(stat -c %a "$lock") == 600 ]] || fail '安装锁文件不可信'
lock_identity=$(stat -c '%d:%i' "$lock")
exec 9<>"$lock"
[[ $(stat -Lc '%d:%i' "/proc/$BASHPID/fd/9") == "$lock_identity" ]] || fail '安装锁在打开时被替换'
flock -n 9 || fail '另一个安装或卸载正在进行'
unchanged_files || fail '确认期间服务或环境文件发生变化，未停止或删除'
[[ $(systemctl show "$unit" -p FragmentPath --value) == "$fragment" && -z $(systemctl show "$unit" -p DropInPaths --value) ]] || fail '确认期间服务来源发生变化，未停止或删除'
scan_state || fail '确认期间状态发生变化，未清理'
safe_parent /var/backups || fail '备份父目录不可信'
backupdir=/var/backups/msboost-agent
[[ -e $backupdir ]] || mkdir -m 0700 "$backupdir"
safe_parent "$backupdir" && [[ $(stat -c %a "$backupdir") == 700 ]] || fail '私有备份目录不可信'
backup=$(mktemp -d "$backupdir/cleanup-$role.XXXXXXXX")
if [[ -n $fragment ]]; then systemctl stop "$unit" || fail '服务停止失败，没有删除文件'; fi
active=$(systemctl show "$unit" -p ActiveState --value)
[[ $active == inactive || $active == failed ]] || fail '服务尚未停止，没有删除文件'
[[ $(systemctl show "$unit" -p MainPID --value) == 0 && $(systemctl show "$unit" -p ControlPID --value) == 0 && -z $(systemctl show "$unit" -p ControlGroup --value) ]] || fail '仍有受管进程，没有删除文件'
scan_state || fail '停止后发现未知内容，没有删除文件'
unchanged_files || fail '停止后服务或环境文件发生变化，没有删除文件'
relative=(); for p in "${paths[@]}"; do relative+=("${p#/}"); done
tar -czf "$backup/components.tar.gz" -C / -- "${relative[@]}" && tar -tzf "$backup/components.tar.gz" >/dev/null || fail '私有备份失败，没有删除文件'
if [[ -n $fragment ]]; then systemctl disable "$unit" || fail '禁用服务失败，没有删除文件'; fi
if [[ $role == relay && -n ${original_hash[$firewall_unit]:-} ]]; then
  systemctl stop msboost-relay-firewall.service || fail '防火墙维护服务停止失败，组件和备份已保留'
  "$managed/relay-firewall" --capability relay-firewall-cleanup || fail '本项目规则尚未完全清理，已保留组件；不删除其他防火墙规则'
  systemctl disable msboost-relay-firewall.service || fail '维护服务禁用失败，组件已保留'
fi
for p in "${managed_files[@]}"; do [[ ! -e $p ]] || { regular "$p" || fail '文件归属变化，停止清理'; rm -- "$p"; }; done
if [[ -n $state ]]; then
  [[ $state == /var/lib/msboost-$role || $state == /var/lib/private/msboost-$role ]] || fail '状态范围异常'
  scan_state || fail '状态变化，停止清理'
  find "$state" -mindepth 1 -maxdepth 1 -exec rm -- {} +
  rmdir -- "$state"
fi
if [[ -L $public ]]; then [[ $(readlink "$public") == private/msboost-$role || $(readlink "$public") == /var/lib/private/msboost-$role ]] || fail '公开链接变化，已保留'; rm -- "$public"; fi
systemctl daemon-reload
printf '此角色已清理；共享程序保留。可恢复私有备份：%s/components.tar.gz\n' "$backup"
