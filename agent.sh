#!/usr/bin/env bash
# Download release-pinned standalone Agent assets; never put enrollment tokens in URLs.
set -Eeuo pipefail
umask 077
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
version=v0.2.3
capability=''; server=''; token_file=''
usage() {
  printf '%s\n' 'MSBOOST 执行机 / 中转节点安装入口（Debian 12，amd64/arm64）' \
    '用法：bash agent.sh --capability executor|relay --server https://panel.example.com [--version v0.2.3] [--token-file /root/private-token]' \
    'executor 为控制执行机，relay 为中转节点；请使用对应的注册令牌。' \
    '未指定 --token-file 时隐藏输入令牌；控制面地址必须为 HTTPS。'
}
fail() { printf '错误：%s\n' "$*" >&2; exit 1; }
step() { printf '\n[%s/3] %s\n' "$1" "$2" >&2; }
while (( $# )); do
  case "$1" in
    --help|-h) usage; exit 0 ;;
    --capability|--server|--version|--token-file)
      (( $# >= 2 )) || fail "缺少 $1 参数"
      case "$1" in --capability) capability=$2 ;; --server) server=${2%/} ;; --version) version=$2 ;; --token-file) token_file=$2 ;; esac
      shift 2 ;;
    *) fail "未知参数 $1" ;;
  esac
done
[[ "$capability" == executor || "$capability" == relay ]] || fail '请选择 executor（控制执行机）或 relay（中转节点）。'
[[ "$server" =~ ^https://[A-Za-z0-9][A-Za-z0-9.-]*(:[0-9]{1,5})?$ ]] || fail '请填写 HTTPS 站点地址，不要包含路径或凭据；公网 HTTP 不适合传递令牌和 SSH 密码。'
[[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || fail '请指定固定正式版本 vX.Y.Z。'
[[ "$(uname -s)" == Linux && "$(id -u)" == 0 ]] || fail '请在需要注册的 Linux 服务器上以 root 运行。'
for tool in curl sha256sum mktemp; do command -v "$tool" >/dev/null || fail "缺少 $tool，请先安装该依赖。"; done
case "$(uname -m)" in x86_64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; *) fail '当前支持 amd64/arm64 架构。' ;; esac
stage=$(mktemp -d /tmp/msboost-agent-bootstrap.XXXXXXXX)
cleanup() { [[ "$stage" == /tmp/msboost-agent-bootstrap.* && -d "$stage" && ! -L "$stage" ]] && rm -rf -- "$stage"; }
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
if [[ -z "$token_file" ]]; then
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
bash "$stage/install-agent.sh" --capability "$capability" --server "$server" \
  --agent "$stage/msboost-agent-linux-${arch}" --agent-sha256 "$agent_sha" \
  --token-file "$token_file" --gost-version 3.3.0 || fail 'Agent 安装未完成，请根据上方阶段与错误处理后重试；不要公开令牌文件。'
printf '\n安装流程结束。请到后台确认执行机 / 节点在线；进程已启动不等于已成功连接控制面。\n'
