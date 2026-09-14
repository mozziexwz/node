#!/usr/bin/env bash
# Local disposable Git/packaging contracts. No registry, release, real compiler,
# installation, Docker daemon, systemd, or network access is performed.
set -Eeuo pipefail
TEST_REPO=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
mkdir -p "$TEST_REPO/.cache"
TEST_WORK=$(mktemp -d "$TEST_REPO/.cache/release-package-test.XXXXXXXX")
trap '[[ $TEST_WORK == "$TEST_REPO/.cache/release-package-test."* && ! -L $TEST_WORK && $(realpath -m "$TEST_WORK") == "$TEST_WORK" ]] && command rm -rf -- "$TEST_WORK"' EXIT
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }

# Source completeness is stricter than copying a legacy rollback snapshot.
source "$TEST_REPO/deploy/manage.sh"
# Windows Git Bash cannot enforce POSIX ownership on this managed workspace;
# source-shape checks still run, while Linux CI exercises install/chmod itself.
if [[ $(uname -s) == MINGW* || $(uname -s) == MSYS* ]]; then
  chmod() { :; }
  install() {
    local directory=0
    while [[ ${1:-} == -* ]]; do case "$1" in -d) directory=1; shift ;; -m) shift 2 ;; *) return 2 ;; esac; done
    if [[ $directory == 1 ]]; then mkdir -p -- "$@"; else cp -- "$1" "$2"; fi
  }
fi
SOURCE_DIR="$TEST_WORK/source"
mkdir -p "$SOURCE_DIR/deploy"
for file in install.sh deploy/manage.sh deploy/compose.yml deploy/compose.build.yml deploy/Caddyfile deploy/disaster.sh deploy/backup_activity_recovery.sh deploy/relay_recovery.sh; do
  printf 'isolated-release-source\n' > "$SOURCE_DIR/$file"
done
validate_source || fail 'complete current source rejected'
for module in disaster.sh backup_activity_recovery.sh relay_recovery.sh; do
  mv -- "$SOURCE_DIR/deploy/$module" "$TEST_WORK/saved-module"
  if validate_source > "$TEST_WORK/output" 2>&1; then fail 'current source accepted a missing recovery module'; fi
  mv -- "$TEST_WORK/saved-module" "$SOURCE_DIR/deploy/$module"
done
legacy="$TEST_WORK/legacy"
mkdir -p "$legacy/deploy"
for file in install.sh deploy/manage.sh deploy/compose.yml deploy/compose.build.yml deploy/Caddyfile; do
  printf 'isolated-legacy-snapshot\n' > "$legacy/$file"
done
copy_deployment_files "$legacy" "$TEST_WORK/copied-legacy" || fail 'legacy snapshot copy compatibility regressed'
[[ -f $TEST_WORK/copied-legacy/deploy/manage.sh && ! -e $TEST_WORK/copied-legacy/deploy/relay_recovery.sh ]] || fail 'legacy copy fabricated a recovery module'

fixture="$TEST_WORK/repository"
mkdir -p "$fixture/scripts" "$fixture/deploy" "$fixture/cmd/agent" "$fixture/.runtime/release"
cp -- "$TEST_REPO/scripts/package-release.sh" "$fixture/scripts/package-release.sh"
printf 'isolated-installer\n' > "$fixture/deploy/install-agent.sh"
printf 'package main\n' > "$fixture/cmd/agent/main.go"
printf '.runtime/\n' > "$fixture/.gitignore"
git -C "$fixture" init -q
git -C "$fixture" config core.autocrlf false
git -C "$fixture" config core.hooksPath "$TEST_WORK/no-hooks"
git -C "$fixture" config commit.gpgSign false
git -C "$fixture" config tag.gpgSign false
git -C "$fixture" add -- .
git -C "$fixture" -c user.name=isolated-test -c user.email=isolated@example.invalid commit -qm 'isolated packaging fixture'
git -C "$fixture" tag v9.8.7
for arch in amd64 arm64; do printf 'isolated-prebuilt-image\n' > "$fixture/.runtime/release/msboost-image-linux-$arch.tar.gz"; done
export RELEASE_TEST_TRACE="$TEST_WORK/compiler-trace"
go() {
  [[ ${1:-} == build ]] || return 2
  local output=''
  while (( $# )); do
    case "$1" in -o) output=$2; shift 2 ;; *) shift ;; esac
  done
  [[ -n $output && $output == */.runtime/release/msboost-* ]] || return 2
  printf 'mock-build\n' >> "$RELEASE_TEST_TRACE"
  printf 'isolated-%s\n' "${GOARCH:-}" > "$output"
}
export -f go
run_package() { (cd "$fixture" && bash scripts/package-release.sh "$1") > "$TEST_WORK/output" 2>&1; }
assert_no_output() {
  [[ ! -e $RELEASE_TEST_TRACE && ! -e $fixture/.runtime/release/msboost-deploy-v9.8.7.tar.gz && ! -e $fixture/.runtime/release/SHA256SUMS ]] || fail 'rejected source wrote release artifacts'
}
printf '// unstaged change\n' >> "$fixture/cmd/agent/main.go"
if run_package v9.8.7; then fail 'unstaged source accepted'; fi
assert_no_output
git -C "$fixture" add -- cmd/agent/main.go
if run_package v9.8.7; then fail 'staged source accepted'; fi
assert_no_output
# Restore only this test-created file; no checkout/reset operation touches the
# real workspace or any user changes.
printf 'package main\n' > "$fixture/cmd/agent/main.go"
git -C "$fixture" add -- cmd/agent/main.go
printf 'package main\n' > "$fixture/cmd/agent/unreviewed.go"
if run_package v9.8.7; then fail 'untracked source accepted'; fi
assert_no_output
rm -- "$fixture/cmd/agent/unreviewed.go"
if run_package v9.8.6; then fail 'nonexistent release tag accepted'; fi
assert_no_output
printf '// reviewed next revision\n' >> "$fixture/cmd/agent/main.go"
git -C "$fixture" add -- cmd/agent/main.go
git -C "$fixture" -c user.name=isolated-test -c user.email=isolated@example.invalid commit -qm 'second reviewed fixture revision'
if run_package v9.8.7; then fail 'release tag pointing away from HEAD accepted'; fi
assert_no_output
git -C "$fixture" tag v9.8.8
run_package v9.8.8 || fail 'clean reviewed tag with ignored build assets rejected'
[[ $(wc -l < "$RELEASE_TEST_TRACE") == 4 ]] || fail 'expected two architectures and two binaries'
(cd "$fixture/.runtime/release" && sha256sum --check --strict SHA256SUMS > "$TEST_WORK/checksums") || fail 'generated artifact checksums mismatch'
tar -tzf "$fixture/.runtime/release/msboost-deploy-v9.8.8.tar.gz" > "$TEST_WORK/members"
grep -Fxq deploy/install-agent.sh "$TEST_WORK/members" || fail 'reviewed installer absent from archive'
if grep -q '^.runtime/' "$TEST_WORK/members"; then fail 'ignored local artifacts entered release archive'; fi
printf '%s\n' 'PASS: required current recovery modules, legacy rollback copying, clean/tag-bound release sources and artifact checksums'
