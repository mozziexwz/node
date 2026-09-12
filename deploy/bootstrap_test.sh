#!/usr/bin/env bash
# Release-download tests with local archives and mocked HTTPS responses only.
set -Eeuo pipefail
TEST_REPO=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
mkdir -p "$TEST_REPO/.cache"
TEST_WORK=$(mktemp -d "$TEST_REPO/.cache/bootstrap-test.XXXXXXXX")
trap '[[ -n $TEST_WORK && -d $TEST_WORK && ! -L $TEST_WORK ]] && rm -rf -- "$TEST_WORK"' EXIT
source "$TEST_REPO/install.sh"
export BOOTSTRAP_TEST_TRACE="$TEST_WORK/manager-args"
export BOOTSTRAP_TEST_EXIT=0
mkdir -p "$TEST_WORK/fixture/deploy" "$TEST_WORK/release"
printf '%s\n' '#!/usr/bin/env bash' 'printf "%s\n" "$@" > "$BOOTSTRAP_TEST_TRACE"' 'exit "$BOOTSTRAP_TEST_EXIT"' > "$TEST_WORK/fixture/deploy/manage.sh"
printf '%s\n' '# test bootstrap placeholder' > "$TEST_WORK/fixture/install.sh"
tar -czf "$TEST_WORK/release/msboost-deploy-v0.1.0.tar.gz" -C "$TEST_WORK/fixture" deploy install.sh
(cd "$TEST_WORK/release" && sha256sum msboost-deploy-v0.1.0.tar.gz > SHA256SUMS)
cp "$TEST_WORK/release/SHA256SUMS" "$TEST_WORK/good-checksum"

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
id() { [[ ${1:-} == -u ]] && printf 0; }
uname() { printf Linux; }
mktemp() { command mktemp -d "$TEST_WORK/download.XXXXXXXX"; }
curl() {
  [[ ${MOCK_FETCH_FAIL:-0} == 0 ]] || return 22
  local url='' output=''
  while [[ $# -gt 0 ]]; do
    case "$1" in https://*) url=$1; shift ;; -o) output=$2; shift 2 ;; *) shift ;; esac
  done
  if [[ $url == https://api.github.com/repos/mozziexwz/node/releases/latest ]]; then printf '{\n  "tag_name": "v0.1.0"\n}\n'; return; fi
  [[ $url == https://github.com/mozziexwz/node/releases/download/v0.1.0/* && -n $output ]] || return 22
  cp "$TEST_WORK/release/${url##*/}" "$output"
}

(bootstrap_main install --domain panel.example.com --email 12345678@qq.com)
grep -qx 'install' "$BOOTSTRAP_TEST_TRACE" || fail 'install action not forwarded'
grep -qx 'v0.1.0' "$BOOTSTRAP_TEST_TRACE" || fail 'release version not forwarded'
grep -qx -- '--source-dir' "$BOOTSTRAP_TEST_TRACE" || fail 'verified bundle path missing'
rm -- "$BOOTSTRAP_TEST_TRACE"
(bootstrap_main upgrade)
grep -qx 'upgrade' "$BOOTSTRAP_TEST_TRACE" || fail 'latest-release upgrade resolution failed'
rm -- "$BOOTSTRAP_TEST_TRACE"

printf '%064d  msboost-deploy-v0.1.0.tar.gz\n' 0 > "$TEST_WORK/release/SHA256SUMS"
if (bootstrap_main install --domain panel.example.com --email 12345678@qq.com); then fail 'tampered archive checksum accepted'; fi
[[ ! -e $BOOTSTRAP_TEST_TRACE ]] || fail 'manager executed after checksum mismatch'
cp "$TEST_WORK/good-checksum" "$TEST_WORK/release/SHA256SUMS"
MOCK_FETCH_FAIL=1
if (bootstrap_main install); then fail 'download failure reported success'; fi
[[ ! -e $BOOTSTRAP_TEST_TRACE ]] || fail 'manager executed after download failure'
MOCK_FETCH_FAIL=0
if (bootstrap_main install --version 'v0.1.0/../../bad'); then fail 'unsafe release version accepted'; fi
export BOOTSTRAP_TEST_EXIT=42
if (bootstrap_main install --domain panel.example.com --email 12345678@qq.com); then fail 'installer failure swallowed'; fi
printf '%s\n' 'PASS: offline release selection/checksum/fetch failure/argument forwarding/exit propagation'
