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
id() { [[ ${1:-} == -u ]] && printf 0; }
uname() { case "${1:-}" in -s) printf Linux ;; -m) printf x86_64 ;; *) return 1 ;; esac; }
curl() {
  [[ ${AGENT_TEST_DOWNLOAD_FAIL:-0} == 0 ]] || return 22
  local url='' output=''
  while (( $# )); do case "$1" in https://*) url=$1; shift ;; -o) output=$2; shift 2 ;; *) shift ;; esac; done
  [[ $url == https://github.com/mozziexwz/node/releases/download/v9.8.7/* && -n $output ]] || return 22
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
printf '%s\n' 'PASS: Agent bootstrap verified assets, Chinese progress/errors, token privacy and exit propagation'
