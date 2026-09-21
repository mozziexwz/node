#!/usr/bin/env bash
# No network, apt, Docker, systemd or host registration is executed.
set -Eeuo pipefail
TEST_REPO=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
mkdir -p "$TEST_REPO/.cache"
TEST_WORK=$(mktemp -d "$TEST_REPO/.cache/agent-bootstrap-test.XXXXXXXX")
trap '[[ -d $TEST_WORK && ! -L $TEST_WORK ]] && rm -rf -- "$TEST_WORK"' EXIT
export AGENT_TEST_ASSETS="$TEST_WORK/assets" AGENT_TEST_TRACE="$TEST_WORK/trace"
mkdir -p "$AGENT_TEST_ASSETS"
printf '%s\n' '#!/usr/bin/env bash' 'printf "%s\n" "$@" > "$AGENT_TEST_TRACE"' 'exit "${AGENT_TEST_INSTALL_EXIT:-0}"' > "$AGENT_TEST_ASSETS/install-agent.sh"
printf 'mock-prebuilt-agent' > "$AGENT_TEST_ASSETS/msboost-agent-linux-amd64"
(cd "$AGENT_TEST_ASSETS" && sha256sum --text install-agent.sh msboost-agent-linux-amd64 > SHA256SUMS)
printf 'abcdefghijklmnopqrstuvwxyz1234567890' > "$TEST_WORK/token"
id() {
  [[ -z ${AGENT_TEST_EARLY_TRACE:-} ]] || printf 'id\n' >> "$AGENT_TEST_EARLY_TRACE"
  [[ ${1:-} == -u ]] && printf 0
}
uname() {
  [[ -z ${AGENT_TEST_EARLY_TRACE:-} ]] || printf 'uname\n' >> "$AGENT_TEST_EARLY_TRACE"
  case "${1:-}" in -s) printf Linux ;; -m) printf x86_64 ;; *) return 1 ;; esac
}
curl() {
  [[ -z ${AGENT_TEST_EARLY_TRACE:-} ]] || printf 'curl\n' >> "$AGENT_TEST_EARLY_TRACE"
  [[ ${AGENT_TEST_DOWNLOAD_FAIL:-0} == 0 ]] || return 22
  local url='' output=''
  while (( $# )); do case "$1" in https://*) url=$1; shift ;; -o) output=$2; shift 2 ;; *) shift ;; esac; done
  [[ $url == "https://github.com/mozziexwz/node/releases/download/${AGENT_TEST_RELEASE:-v9.8.7}/"* && -n $output ]] || return 22
  cp "$AGENT_TEST_ASSETS/${url##*/}" "$output"
}
export -f id uname curl
run_bootstrap() { bash "$TEST_REPO/agent.sh" --capability executor --server https://panel.example.com --version v9.8.7 --token-file "$TEST_WORK/token"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
if ! run_bootstrap > "$TEST_WORK/success.log" 2>&1; then
  cat "$TEST_WORK/success.log" >&2
  fail 'bootstrap success fixture failed'
fi
grep -q '安装流程结束' "$TEST_WORK/success.log" || fail 'missing Chinese completion guidance'
grep -qx executor "$AGENT_TEST_TRACE" || fail 'capability not forwarded'
grep -qx -- '--agent-sha256' "$AGENT_TEST_TRACE" || fail 'checksum not forwarded'
! grep -q 'abcdefghijklmnopqrstuvwxyz1234567890' "$TEST_WORK/success.log" || fail 'enrollment token leaked'
rm -- "$AGENT_TEST_TRACE"
bash "$TEST_REPO/agent.sh" --capability relay --server https://panel.example.com --version v9.8.7 --token-file "$TEST_WORK/token" --offline-policy keep_last --acknowledge-relay-restart > "$TEST_WORK/keep-last.log" 2>&1 || fail 'explicit keep_last forwarding failed'
grep -qx -- '--offline-policy' "$AGENT_TEST_TRACE" || fail 'offline policy option not forwarded'
grep -qx keep_last "$AGENT_TEST_TRACE" || fail 'keep_last not forwarded'
grep -qx -- '--acknowledge-relay-restart' "$AGENT_TEST_TRACE" || fail 'maintenance acknowledgement missing'
rm -- "$AGENT_TEST_TRACE"
if bash "$TEST_REPO/agent.sh" --capability executor --offline-policy keep_last > "$TEST_WORK/wrong-role.log" 2>&1; then fail 'executor accepted relay policy'; fi
if bash "$TEST_REPO/agent.sh" --capability relay --offline-policy unknown > "$TEST_WORK/wrong-policy.log" 2>&1; then fail 'unknown policy accepted'; fi
[[ ! -f $AGENT_TEST_TRACE ]] || fail 'invalid policy ran installer'
# The new entrypoint must not hand a protected host to a legacy installer.
# Exercise both roles and relay policies; reject before even platform/root
# inspection, not merely after a failed download or an installer mutation.
export AGENT_TEST_EARLY_TRACE="$TEST_WORK/early-side-effects"
for old_version in v0.0.0 v0.1.99 v0.2.0 v0.2.3 v0.1.999999999999999999999999999999999999999999; do
  for mode in executor relay relay-lease relay-keep-last; do
    args=(--capability relay --server https://panel.example.com --version "$old_version" --token-file "$TEST_WORK/token")
    case "$mode" in
      executor) args[1]=executor ;;
      relay-lease) args+=(--offline-policy lease) ;;
      relay-keep-last) args+=(--offline-policy keep_last) ;;
    esac
    if bash "$TEST_REPO/agent.sh" "${args[@]}" > "$TEST_WORK/old-version.log" 2>&1; then fail 'legacy unsafe installer version accepted'; fi
    grep -Fq '旧 Agent 安装器缺少 v2 状态与共享程序保护' "$TEST_WORK/old-version.log" || fail 'old version hit an unrelated guard'
    [[ ! -e $AGENT_TEST_EARLY_TRACE && ! -e $AGENT_TEST_TRACE ]] || fail 'old installer version reached privilege/download/mutation paths'
  done
done
for bad_version in v00.2.4 v0.02.4 v0.2.04 v0.2.4-beta v0.2.4+build v-1.2.4 v0.2 v0.2.4.1; do
  if bash "$TEST_REPO/agent.sh" --capability relay --server https://panel.example.com --version "$bad_version" --token-file "$TEST_WORK/token" > "$TEST_WORK/bad-version.log" 2>&1; then fail 'noncanonical stable version accepted'; fi
  grep -Fq '固定稳定版本 vX.Y.Z，版本数字不得包含前导零' "$TEST_WORK/bad-version.log" || fail 'invalid version hit an unrelated guard'
  [[ ! -e $AGENT_TEST_EARLY_TRACE && ! -e $AGENT_TEST_TRACE ]] || fail 'invalid version reached privilege/download/mutation paths'
done
unset AGENT_TEST_EARLY_TRACE
# All supported branches, including components too large for machine integer
# arithmetic, use only synthetic assets. No version is queried over a network.
for supported in v0.2.4 v0.2.5 v0.3.0 v1.0.0 v9.8.7 v0.2.999999999999999999999999999999999999999999 v0.999999999999999999999999999999999999999999.0 v999999999999999999999999999999999999999999.0.0; do
  AGENT_TEST_RELEASE="$supported" bash "$TEST_REPO/agent.sh" --capability relay --server https://panel.example.com --version "$supported" --token-file "$TEST_WORK/token" > "$TEST_WORK/supported-version.log" 2>&1 || fail 'supported version comparison failed or overflowed'
  [[ -f $AGENT_TEST_TRACE ]] || fail 'supported version never reached the synthetic installer'
  rm -- "$AGENT_TEST_TRACE"
done
export AGENT_TEST_DOWNLOAD_FAIL=1
if run_bootstrap > "$TEST_WORK/download.log" 2>&1; then fail 'download error swallowed'; fi
grep -q '下载.*失败' "$TEST_WORK/download.log" || fail 'missing Chinese download error'
[[ ! -f $AGENT_TEST_TRACE ]] || fail 'installer ran after download failure'
export AGENT_TEST_DOWNLOAD_FAIL=0
printf 'tampered-agent' > "$AGENT_TEST_ASSETS/msboost-agent-linux-amd64"
if run_bootstrap > "$TEST_WORK/checksum.log" 2>&1; then fail 'tampered agent accepted'; fi
[[ ! -f $AGENT_TEST_TRACE ]] || fail 'installer ran after checksum failure'
grep -q '校验失败' "$TEST_WORK/checksum.log" || fail 'missing Chinese checksum error'
printf 'mock-prebuilt-agent' > "$AGENT_TEST_ASSETS/msboost-agent-linux-amd64"
export AGENT_TEST_INSTALL_EXIT=42
if run_bootstrap > "$TEST_WORK/install.log" 2>&1; then fail 'child installer error swallowed'; fi
grep -q '安装未完成' "$TEST_WORK/install.log" || fail 'missing Chinese installer failure'
printf '%s\n' 'PASS: Agent bootstrap verified assets, minimum safe installer version before side effects, canonical overflow-safe semver, Chinese progress/errors, token privacy and exit propagation'
