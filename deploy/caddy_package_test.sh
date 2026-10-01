#!/usr/bin/env bash
# Offline package acquisition fixtures. Never install a package, contact the
# network, access host services, or write outside this invocation's temp tree.
set -Eeuo pipefail
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
source "$ROOT/deploy/manage.sh"
WORK=$(mktemp -d "${TMPDIR:-/tmp}/msboost-caddy-package-test.XXXXXXXX")
trap '[[ -d $WORK && ! -L $WORK ]] && command rm -rf -- "$WORK"' EXIT
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
declare -F caddy_install_package >/dev/null || fail 'missing caddy_install_package'
# If production later bypasses a shell mock with `command`, fail closed rather
# than reach the real network, package manager or system services on the runner.
mkdir "$WORK/blocked-bin"
for BLOCKED_COMMAND in apt-get curl dpkg dpkg-query dpkg-deb systemctl getent ss caddy; do
  printf '#!/usr/bin/env bash\nprintf "REFUSED: unmocked fixture command\\n" >&2\nexit 97\n' > "$WORK/blocked-bin/$BLOCKED_COMMAND"
  chmod 0755 "$WORK/blocked-bin/$BLOCKED_COMMAND"
done
PATH="$WORK/blocked-bin:$PATH"

# Exercise the production function with real checksum verification. Only the
# two reviewed public artifact digests are replaced with this fixture's digest;
# URL selection, error handling, package metadata checks and cleanup stay intact.
printf 'public isolated Caddy package fixture\n' > "$WORK/public.deb"
FIXTURE_SHA=$(sha512sum "$WORK/public.deb" | cut -d' ' -f1)
declare -f caddy_install_package > "$WORK/helper.original"
PIN_COUNT=$(grep -Eo '[a-f0-9]{128}' "$WORK/helper.original" | wc -l | tr -d '[:space:]')
[[ $PIN_COUNT == 2 ]] || fail 'expected exactly two fixed public package digests'
sed -E "s/[a-f0-9]{128}/$FIXTURE_SHA/g" "$WORK/helper.original" > "$WORK/helper.fixture"
source "$WORK/helper.fixture"

# Map host resource probes into the fixture, so the guards do not depend on a
# Caddy installed by earlier CI steps. Account and package queries are modeled
# below. This path mapping is confined to this generated test-only function.
mkdir -p "$WORK/host/etc/systemd/system" "$WORK/host/run/systemd/system" "$WORK/host/lib/systemd/system" "$WORK/host/usr/lib/systemd/system" "$WORK/host/usr/bin" "$WORK/host/var/lib" "$WORK/host/var/log"
declare -f caddy_ensure | sed \
  -e 's|/etc/|@ETC@/|g' -e 's|/run/|@RUN@/|g' -e 's|/usr/local/lib/systemd/|@LOCALLIB@/|g' -e 's|/usr/lib/systemd/|@USRLIB@/|g' -e 's|/lib/systemd/|@LIB@/|g' \
  -e 's|/usr/bin/caddy|@CADDYBIN@|g' -e 's|/var/lib/caddy|@CADDYSTATE@|g' -e 's|/var/log/caddy|@CADDYLOG@|g' \
  -e "s|@ETC@/|$WORK/host/etc/|g" -e "s|@USRLIB@/|$WORK/host/usr/lib/systemd/|g" \
  -e "s|@RUN@/|$WORK/host/run/|g" -e "s|@LOCALLIB@/|$WORK/host/usr/local/lib/systemd/|g" \
  -e "s|@LIB@/|$WORK/host/lib/systemd/|g" -e "s|@CADDYBIN@|$WORK/host/usr/bin/caddy|g" \
  -e "s|@CADDYSTATE@|$WORK/host/var/lib/caddy|g" -e "s|@CADDYLOG@|$WORK/host/var/log/caddy|g" > "$WORK/ensure.fixture"
source "$WORK/ensure.fixture"

CASE_ROOT= TRACE= ARCH= FAULT= HAVE_CADDY=0 PACKAGE_STATUS= ACTIVE_MAIN=0 ACTIVE_API=0 BUSY_PORTS=0 HAVE_USER=0 HAVE_GROUP=0
package_directory() { [[ -f $CASE_ROOT/created-directory ]] && cat "$CASE_ROOT/created-directory"; }

# Suppress environment-dependent binary discovery, including a Caddy installed
# by an earlier CI integration test. All other command invocations remain real.
command() {
  if [[ $# == 2 && $1 == -v ]]; then
    case $2 in ss) return 0;; caddy) [[ $HAVE_CADDY == 1 ]]; return;; esac
  fi
  builtin command "$@"
}
dpkg() {
  [[ $# == 1 && $1 == --print-architecture ]] || return 90
  printf 'architecture %s\n' "$ARCH" >> "$TRACE"
  [[ $FAULT != architecture-error ]] || return 1
  printf '%s\n' "$ARCH"
}
dpkg-query() {
  [[ $# == 3 && $1 == -W && $2 == '-f=${db:Status-Status}' && $3 == caddy ]] || return 90
  printf 'registered-package %s\n' "$PACKAGE_STATUS" >> "$TRACE"
  [[ $FAULT != package-query-error ]] || return 2
  [[ $FAULT != package-query-error-with-output ]] || { printf 'installed\n'; return 1; }
  [[ -n $PACKAGE_STATUS ]] || return 1
  printf '%s\n' "$PACKAGE_STATUS"
}
dpkg-deb() {
  local directory value
  directory=$(package_directory) || return 90
  [[ $# == 3 && $1 == --field && $2 == "$directory/caddy.deb" ]] || return 90
  printf 'package-field %s\n' "$3" >> "$TRACE"
  case $3 in
    Package) value=caddy; [[ $FAULT != wrong-package ]] || value=foreign-package;;
    Version) value=2.11.4; [[ $FAULT != wrong-version ]] || value=2.11.5;;
    Architecture) value=$ARCH; [[ $FAULT != wrong-package-architecture ]] || value=foreign-arch;;
    *) return 90;;
  esac
  [[ $FAULT != metadata-error ]] || return 1
  printf '%s\n' "$value"
}
apt-get() {
  local directory
  printf 'apt-get %s\n' "$*" >> "$TRACE"
  case "$*" in
    update) [[ $FAULT != update-error ]]; return;;
    'install -y --no-install-recommends ca-certificates curl init-system-helpers passwd') [[ $FAULT != dependencies-error ]]; return;;
  esac
  directory=$(package_directory) || return 90
  [[ $# == 4 && $1 == install && $2 == -y && $3 == --no-install-recommends && $4 == "$directory/caddy.deb" ]] || return 90
  [[ -f $directory/caddy.deb && ! -L $directory/caddy.deb ]] || return 90
  [[ $(command sha512sum "$directory/caddy.deb" | cut -d' ' -f1) == "$FIXTURE_SHA" ]] || return 90
  if [[ $(uname -s) == Linux ]]; then
    [[ $(stat -c %a "$directory") == 755 && $(stat -c %a "$directory/caddy.deb") == 644 ]] || return 90
  fi
  printf 'local-package-install\n' >> "$TRACE"
  [[ $FAULT != local-install-error ]]
}
mktemp() {
  local directory
  [[ $# == 2 && $1 == -d && $2 == /tmp/msboost-caddy-package.XXXXXXXX ]] || return 90
  printf 'create-package-directory\n' >> "$TRACE"
  [[ $FAULT != temp-error ]] || return 1
  directory=$(command mktemp -d "$CASE_ROOT/package.XXXXXXXX") || return
  printf '%s\n' "$directory" > "$CASE_ROOT/created-directory"
  printf '%s\n' "$directory"
}
curl() {
  local directory output= url= argument previous=
  directory=$(package_directory) || return 90
  printf 'download\n' >> "$TRACE"
  for argument in "$@"; do
    [[ $previous != -o ]] || output=$argument
    [[ $argument != https://* ]] || url=$argument
    previous=$argument
  done
  [[ $url == "https://github.com/caddyserver/caddy/releases/download/v2.11.4/caddy_2.11.4_linux_${ARCH}.deb" && $output == "$directory/caddy.deb" ]] || return 90
  cp "$WORK/public.deb" "$output" || return
  [[ $FAULT != corrupt-download ]] || printf 'tampered bytes\n' >> "$output"
  if [[ $FAULT == unexpected-temp-file ]]; then
    printf 'retain unrelated file\n' > "$directory/unrelated.keep"
    return 22
  fi
  [[ $FAULT != download-error ]]
}
sha512sum() {
  [[ $# == 2 && $1 == --check && $2 == --strict ]] || return 90
  printf 'verify-package-checksum\n' >> "$TRACE"
  command sha512sum "$@"
}
chmod() {
  local directory
  directory=$(package_directory) || return 90
  [[ $# == 2 && ( $1 == 0755 && $2 == "$directory" || $1 == 0644 && $2 == "$directory/caddy.deb" ) ]] || return 90
  command chmod "$@"
}
rm() {
  local directory
  directory=$(package_directory) || return 90
  [[ $# == 3 && $1 == -f && $2 == -- && $3 == "$directory/caddy.deb" ]] || return 90
  printf 'remove-package-file\n' >> "$TRACE"
  command rm "$@"
}
rmdir() {
  local directory
  directory=$(package_directory) || return 90
  [[ $# == 1 && $1 == "$directory" || $# == 2 && $1 == -- && $2 == "$directory" ]] || return 90
  printf 'remove-package-directory\n' >> "$TRACE"
  command rmdir "$@"
}
ss() { [[ $BUSY_PORTS == 0 ]] || printf 'LISTEN existing-service\n'; }
getent() {
  [[ $# == 2 && $2 == caddy ]] || return 90
  printf 'account-query %s\n' "$1" >> "$TRACE"
  case $1 in passwd) [[ $HAVE_USER == 1 ]];; group) [[ $HAVE_GROUP == 1 ]];; *) return 90;; esac
}
systemctl() {
  [[ $# == 3 && $1 == is-active && $2 == --quiet ]] || return 90
  printf 'service-state %s\n' "$3" >> "$TRACE"
  case $3 in caddy.service) [[ $ACTIVE_MAIN == 1 ]];; caddy-api.service) [[ $ACTIVE_API == 1 ]];; *) return 90;; esac
}
# Stop caddy_ensure before any config operation. The guard fixtures test package
# acquisition boundaries; publication/real service checks have separate tests.
caddy_assert_service() { printf 'assert-existing-service\n' >> "$TRACE"; return 75; }

prepare_case() {
  local name=$1
  CASE_ROOT="$WORK/$name"; mkdir "$CASE_ROOT"
  TRACE="$CASE_ROOT/trace"; : > "$TRACE"
  ARCH=amd64; FAULT=; HAVE_CADDY=0; PACKAGE_STATUS=; ACTIVE_MAIN=0; ACTIVE_API=0; BUSY_PORTS=0; HAVE_USER=0; HAVE_GROUP=0
  command rm -f -- "$WORK/host/etc/systemd/system/caddy.service" "$WORK/host/run/systemd/system/caddy.service" "$WORK/host/var/lib/caddy" "$WORK/host/var/log/caddy"
  for PREVIOUS_DIRECTORY in "$WORK/host/run/systemd/system/caddy.service.d" "$WORK/host/etc/systemd/system/caddy-api.service.d" "$WORK/host/etc/systemd/system/caddy.service.wants"; do
    [[ ! -d $PREVIOUS_DIRECTORY ]] || command rmdir -- "$PREVIOUS_DIRECTORY"
  done
}
assert_no_acquisition() {
  ! grep -Eq '^(apt-get |download$|create-package-directory$)' "$TRACE" || fail "$1 attempted package acquisition"
}
assert_cleaned() {
  local directory
  [[ -f $CASE_ROOT/created-directory ]] || return 0
  directory=$(package_directory)
  [[ ! -e $directory && ! -L $directory ]] || fail "$1 left its package temp directory"
}
expect_package_failure() {
  local name=$1
  if caddy_install_package > "$CASE_ROOT/output" 2>&1; then fail "$name accepted failure"; fi
  if [[ $FAULT != local-install-error ]]; then
    ! grep -Eq '^apt-get install .*[/]caddy\.deb$' "$TRACE" || fail "$name attempted to install an unverified package"
  fi
  assert_cleaned "$name"
}

for ARCH_CASE in amd64 arm64; do
  prepare_case "success-$ARCH_CASE"; ARCH=$ARCH_CASE
  if ! caddy_install_package > "$CASE_ROOT/output" 2>&1; then cat "$CASE_ROOT/output" >&2; fail "$ARCH_CASE package acquisition failed"; fi
  [[ $(grep -c '^local-package-install$' "$TRACE") == 1 ]] || fail "$ARCH_CASE missing exact local package install"
  grep -qx verify-package-checksum "$TRACE" || fail "$ARCH_CASE skipped checksum verification"
  assert_cleaned "$ARCH_CASE"
done
for STATUS_CASE in absent not-installed; do
  prepare_case "ensure-clean-$STATUS_CASE"
  [[ $STATUS_CASE == absent ]] || PACKAGE_STATUS=$STATUS_CASE
  if caddy_ensure > "$CASE_ROOT/output" 2>&1; then fail 'fixture service assertion unexpectedly accepted'; else RESULT=$?; fi
  [[ $RESULT == 75 ]] || { cat "$CASE_ROOT/output" >&2; fail "$STATUS_CASE clean host never reached service validation"; }
  [[ $(grep -c '^local-package-install$' "$TRACE") == 1 ]] || fail "$STATUS_CASE clean host did not install exact local artifact"
  assert_cleaned "$STATUS_CASE clean host"
done
for FAULT_CASE in update-error dependencies-error temp-error download-error corrupt-download wrong-package wrong-version wrong-package-architecture metadata-error local-install-error architecture-error; do
  prepare_case "$FAULT_CASE"; FAULT=$FAULT_CASE
  expect_package_failure "$FAULT_CASE"
  [[ $FAULT_CASE != architecture-error ]] || assert_no_acquisition architecture-error
done
prepare_case unknown-architecture; ARCH=riscv64
expect_package_failure unknown-architecture
assert_no_acquisition unknown-architecture

prepare_case unexpected-temp-file; FAULT=unexpected-temp-file
if caddy_install_package > "$CASE_ROOT/output" 2>&1; then fail 'unexpected tempfile accepted'; fi
PACKAGE_DIRECTORY=$(package_directory)
[[ ! -e $PACKAGE_DIRECTORY/caddy.deb && -f $PACKAGE_DIRECTORY/unrelated.keep ]] || fail 'cleanup removed unexpected temp contents or retained its own artifact'
! grep -Eq '^apt-get install .*[/]caddy\.deb$' "$TRACE" || fail 'download failure attempted package installation'

for STATUS_CASE in installed config-files unpacked half-configured triggers-pending; do
  prepare_case "registered-$STATUS_CASE"; PACKAGE_STATUS=$STATUS_CASE
  if caddy_ensure > "$CASE_ROOT/output" 2>&1; then fail "$STATUS_CASE registered package accepted"; fi
  assert_no_acquisition "$STATUS_CASE registered package"
done
for GUARD_CASE in active-main active-api busy-ports custom-unit custom-unit-symlink runtime-unit runtime-drop-in persistent-api-drop-in persistent-orphan-wants existing-state existing-logs existing-user existing-group existing-binary package-query-error package-query-error-with-output; do
  # Git Bash may emulate symlinks by copying the target, so exercise the real
  # dangling-link case only on Linux, where CI runs this entire fixture.
  if [[ $GUARD_CASE == custom-unit-symlink && $(uname -s) != Linux ]]; then continue; fi
  prepare_case "$GUARD_CASE"
  case $GUARD_CASE in
    active-main) ACTIVE_MAIN=1;;
    active-api) ACTIVE_API=1;;
    busy-ports) BUSY_PORTS=1;;
    custom-unit) printf 'foreign unit\n' > "$WORK/host/etc/systemd/system/caddy.service";;
    custom-unit-symlink) ln -s "$WORK/missing-unit" "$WORK/host/etc/systemd/system/caddy.service";;
    runtime-unit) printf 'foreign runtime unit\n' > "$WORK/host/run/systemd/system/caddy.service";;
    runtime-drop-in) mkdir "$WORK/host/run/systemd/system/caddy.service.d";;
    persistent-api-drop-in) mkdir "$WORK/host/etc/systemd/system/caddy-api.service.d";;
    persistent-orphan-wants) mkdir "$WORK/host/etc/systemd/system/caddy.service.wants";;
    existing-state) printf 'foreign state\n' > "$WORK/host/var/lib/caddy";;
    existing-logs) printf 'foreign log\n' > "$WORK/host/var/log/caddy";;
    existing-user) HAVE_USER=1;;
    existing-group) HAVE_GROUP=1;;
    existing-binary) HAVE_CADDY=1;;
    package-query-error|package-query-error-with-output) FAULT=$GUARD_CASE;;
  esac
  if caddy_ensure > "$CASE_ROOT/output" 2>&1; then fail "$GUARD_CASE guard accepted"; fi
  assert_no_acquisition "$GUARD_CASE"
done
# systemd also discovers generated, transient, attached and /usr/local units.
# Every supported search root must reject an orphan API service drop-in before
# the official .deb postinst gets a chance to start any service.
for UNIT_ROOT in etc/systemd/system.control run/systemd/system.control run/systemd/transient run/systemd/generator.early \
  etc/systemd/system etc/systemd/system.attached run/systemd/system run/systemd/system.attached run/systemd/generator \
  usr/local/lib/systemd/system lib/systemd/system usr/lib/systemd/system run/systemd/generator.late; do
  prepare_case "load-path-${UNIT_ROOT//\//-}"
  mkdir -p "$WORK/host/$UNIT_ROOT/caddy-api.service.d"
  if caddy_ensure > "$CASE_ROOT/output" 2>&1; then fail "$UNIT_ROOT drop-in accepted"; fi
  assert_no_acquisition "$UNIT_ROOT drop-in"
  command rmdir -- "$WORK/host/$UNIT_ROOT/caddy-api.service.d"
done
printf '%s\n' 'PASS: pinned Caddy packages verify before install, failures clean only owned temporary files, unsupported architecture and existing-service guards prevent acquisition'
