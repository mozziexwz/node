#!/usr/bin/env bash
# Download release-pinned standalone Agent assets; never put enrollment tokens in URLs.
set +xv
set -Eeuo pipefail
umask 077
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
version=v1.0.1
capability=''; server=''; token_file=''; offline_policy=''; offline_policy_set=0; acknowledge_restart=0; fresh_reset=0; upgrade_in_place=0
usage() {
  printf '%s\n' 'MSBOOST 执行机 / 中转节点安装入口（Debian 11/12/13，amd64/arm64）' \
    '用法：bash agent.sh --capability executor|relay --server https://panel.example.com [--version v1.0.1] [--token-file /root/private-token]' \
    'executor 为控制执行机，relay 为中转节点；请使用对应的注册令牌。' \
    '未指定 --token-file 时隐藏输入令牌；控制面地址必须为 HTTPS。' \
    '此新版入口仅允许 v0.2.4 或更新的稳定版本；旧安装器缺少 v2 状态与共享程序保护。' \
    '新装 relay 默认 keep_last；可显式 --offline-policy lease 兼容旧链。' \
    '已有 lease 服务切换 keep_last 仍需 --acknowledge-relay-restart，迁移会中断原连接。' \
    '已有 keep_last Relay 保留原身份升级：--upgrade-in-place --acknowledge-relay-restart；不需要也不接受新注册令牌，升级会中断现有连接。' \
    '旧 Relay v2 身份不会因粘贴新令牌自动替换。仅在确认旧转发全部可中断时，显式同时传 --fresh-reset --acknowledge-relay-restart；旧私有状态会留在备份。'
}
fail() { printf '错误：%s\n' "$*" >&2; exit 1; }
step() { printf '\n[%s/3] %s\n' "$1" "$2" >&2; }
while (( $# )); do
  case "$1" in
    --help|-h) usage; exit 0 ;;
    --acknowledge-relay-restart) acknowledge_restart=1; shift ;;
    --fresh-reset) fresh_reset=1; shift ;;
    --upgrade-in-place) upgrade_in_place=1; shift ;;
    --capability|--server|--version|--token-file|--offline-policy)
      (( $# >= 2 )) || fail "缺少 $1 参数"
      case "$1" in --capability) capability=$2 ;; --server) server=${2%/} ;; --version) version=$2 ;; --token-file) token_file=$2 ;; --offline-policy) offline_policy=$2; offline_policy_set=1 ;; esac
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
  [[ $offline_policy_set == 0 && $acknowledge_restart == 0 && $fresh_reset == 0 && $upgrade_in_place == 0 ]] || fail '离线策略、中转重启确认、原地升级和全新重置只适用于 relay。'
  offline_policy=lease
fi
[[ $offline_policy == lease || $offline_policy == keep_last ]] || fail 'offline-policy 仅允许 lease 或 keep_last。'
[[ "$server" =~ ^https://[A-Za-z0-9][A-Za-z0-9.-]*(:[0-9]{1,5})?$ ]] || fail '请填写 HTTPS 站点地址，不要包含路径或凭据；公网 HTTP 不适合传递令牌和 SSH 密码。'
[[ "$version" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || fail '请指定固定稳定版本 vX.Y.Z，版本数字不得包含前导零。'
# Every fetched installer must contain the shared-binary/v2 admission guards.
# Checking only the current local state would race the other role's install;
# executing an older installer to ask about its capabilities is not safe.
# Compare decimal strings, never shell arithmetic: arbitrary large valid
# semantic-version components cannot overflow and wrap below this floor.
case "${BASH_REMATCH[1]}:${BASH_REMATCH[2]}:${BASH_REMATCH[3]}" in
  0:0:*|0:1:*|0:2:[0-3]) fail '此入口仅支持 v0.2.4 或更新版本；旧 Agent 安装器缺少 v2 状态与共享程序保护，已在下载和系统操作前拒绝。网站历史版本及灾难恢复不受影响。' ;;
esac
[[ $upgrade_in_place == 0 || ${BASH_REMATCH[1]} != 0 ]] || fail '原地升级仅支持 v1.0.0 或更新版本；旧安装器不理解身份保留升级。'
[[ "$(uname -s)" == Linux && "$(id -u)" == 0 ]] || fail '请在需要注册的 Linux 服务器上以 root 运行。'
for tool in curl sha256sum mktemp; do command -v "$tool" >/dev/null || fail "缺少 $tool，请先安装该依赖。"; done
case "$(uname -m)" in x86_64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; *) fail '当前支持 amd64/arm64 架构。' ;; esac
stage=$(mktemp -d /tmp/msboost-agent-bootstrap.XXXXXXXX)
cleanup() { if [[ "$stage" == /tmp/msboost-agent-bootstrap.* && -d "$stage" && ! -L "$stage" ]]; then rm -rf -- "$stage"; fi; }
trap cleanup EXIT
base="https://github.com/mozziexwz/node/releases/download/${version}"
step 1 "下载固定版本 $version（$arch），不会在本机编译"
for file in SHA256SUMS install-agent.sh "msboost-agent-linux-${arch}"; do
  curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 --retry 3 --max-time 300 "$base/$file" -o "$stage/$file" || fail "下载 $file 失败，请检查 GitHub 连通性与正式版本是否存在。"
done
step 2 '核对安装器与 Agent 的 SHA-256'
for file in install-agent.sh "msboost-agent-linux-${arch}"; do
  expected=$(awk -v name="$file" '$2 == name {print $1}' "$stage/SHA256SUMS")
  [[ "$expected" =~ ^[a-f0-9]{64}$ ]] || fail "$file 的校验值缺失或重复，停止执行。"
  [[ "$(sha256sum "$stage/$file" | cut -d' ' -f1)" == "$expected" ]] || fail "$file 校验失败，拒绝安装。"
done
agent_sha=$expected
if [[ -z "$token_file" && $upgrade_in_place == 0 ]]; then
  [[ -r /dev/tty ]] || fail '需要交互终端或 --token-file 私有令牌文件。'
  printf '请粘贴注册令牌（输入不回显）：' >/dev/tty
  IFS= read -r -s token </dev/tty || fail '无法读取令牌。'
  printf '\n' >/dev/tty
  [[ "$token" =~ ^[A-Za-z0-9_-]{32,256}$ ]] || fail '令牌格式无效，请从对应后台入口重新复制。'
  token_file="$stage/token"
  printf '%s' "$token" > "$token_file"
  unset token
fi
step 3 '安装并启动 Agent（已有受管安装会先保存私有备份）'
relay_options=()
if [[ $capability == relay ]]; then relay_options+=(--offline-policy "$offline_policy"); fi
if (( acknowledge_restart )); then relay_options+=(--acknowledge-relay-restart); fi
if (( fresh_reset )); then relay_options+=(--fresh-reset); fi
if (( upgrade_in_place )); then relay_options+=(--upgrade-in-place); fi
token_options=()
if (( ! upgrade_in_place )); then token_options+=(--token-file "$token_file"); fi
bash "$stage/install-agent.sh" --capability "$capability" --server "$server" \
  --agent "$stage/msboost-agent-linux-${arch}" --agent-sha256 "$agent_sha" \
  "${token_options[@]}" --gost-version 3.3.0 "${relay_options[@]}" || fail 'Agent 安装未完成，请根据上方阶段与错误处理后重试；不要公开令牌文件。'
printf '\n安装流程结束。请到后台确认执行机 / 节点在线；进程已启动不等于已成功连接控制面。\n'
