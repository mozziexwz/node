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
MOCK_LATEST_BODY=$'{\n  "tag_name": "v0.1.1"\n}'
mkdir -p "$TEST_WORK/fixture/deploy" "$TEST_WORK/release"
printf '%s\n' '#!/usr/bin/env bash' 'printf "%s\n" "$@" > "$BOOTSTRAP_TEST_TRACE"' 'exit "$BOOTSTRAP_TEST_EXIT"' > "$TEST_WORK/fixture/deploy/manage.sh"
printf '%s\n' '# test bootstrap placeholder' > "$TEST_WORK/fixture/install.sh"
tar -czf "$TEST_WORK/release/msboost-deploy-v0.1.1.tar.gz" -C "$TEST_WORK/fixture" deploy install.sh
(cd "$TEST_WORK/release" && sha256sum msboost-deploy-v0.1.1.tar.gz > SHA256SUMS)
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
  if [[ $url == https://api.github.com/repos/mozziexwz/node/releases/latest ]]; then printf '%s\n' "$MOCK_LATEST_BODY"; return; fi
  [[ $url == https://github.com/mozziexwz/node/releases/download/v0.1.1/* && -n $output ]] || return 22
  cp "$TEST_WORK/release/${url##*/}" "$output"
}

(bootstrap_main install --domain panel.example.com --email 12345678@qq.com)
grep -qx 'install' "$BOOTSTRAP_TEST_TRACE" || fail 'install action not forwarded'
grep -qx 'v0.1.1' "$BOOTSTRAP_TEST_TRACE" || fail 'release version not forwarded'
grep -qx -- '--source-dir' "$BOOTSTRAP_TEST_TRACE" || fail 'verified bundle path missing'
rm -- "$BOOTSTRAP_TEST_TRACE"
(bootstrap_main upgrade)
grep -qx 'upgrade' "$BOOTSTRAP_TEST_TRACE" || fail 'latest-release upgrade resolution failed'
rm -- "$BOOTSTRAP_TEST_TRACE"
MOCK_LATEST_BODY='{"url":"https://api.github.com/repos/mozziexwz/node/releases/1","id":1,"tag_name":"v0.1.1","target_commitish":"main","assets":[],"body":"release note with escaped \"tag_name\": \"v9.9.9\""}'
(bootstrap_main upgrade)
grep -qx 'upgrade' "$BOOTSTRAP_TEST_TRACE" || fail 'compact GitHub release response did not resolve upgrade'
grep -qx 'v0.1.1' "$BOOTSTRAP_TEST_TRACE" || fail 'compact release tag parsed incorrectly'
rm -- "$BOOTSTRAP_TEST_TRACE"
MOCK_LATEST_BODY='{"url":"https://api.github.com/repos/mozziexwz/node/releases/1","tag_name":"v0.1.1/../../bad"}'
if (bootstrap_main upgrade); then fail 'compact API response bypassed strict tag validation'; fi
[[ ! -e $BOOTSTRAP_TEST_TRACE ]] || fail 'manager executed with invalid API tag'
MOCK_LATEST_BODY='{"url":"https://api.github.com/repos/mozziexwz/node/releases/1","assets":[]}'
if (bootstrap_main upgrade); then fail 'API response without a tag was accepted'; fi
[[ ! -e $BOOTSTRAP_TEST_TRACE ]] || fail 'manager executed without an API tag'

printf '%064d  msboost-deploy-v0.1.1.tar.gz\n' 0 > "$TEST_WORK/release/SHA256SUMS"
if (bootstrap_main install --domain panel.example.com --email 12345678@qq.com); then fail 'tampered archive checksum accepted'; fi
[[ ! -e $BOOTSTRAP_TEST_TRACE ]] || fail 'manager executed after checksum mismatch'
cp "$TEST_WORK/good-checksum" "$TEST_WORK/release/SHA256SUMS"
MOCK_FETCH_FAIL=1
if (bootstrap_main install); then fail 'download failure reported success'; fi
[[ ! -e $BOOTSTRAP_TEST_TRACE ]] || fail 'manager executed after download failure'
MOCK_FETCH_FAIL=0
if (bootstrap_main install --version 'v0.1.1/../../bad'); then fail 'unsafe release version accepted'; fi
export BOOTSTRAP_TEST_EXIT=42
if (bootstrap_main install --domain panel.example.com --email 12345678@qq.com); then fail 'installer failure swallowed'; fi
printf '%s\n' 'PASS: compact/formatted release JSON, strict tags, checksum, fetch failure, forwarding and exit propagation'
