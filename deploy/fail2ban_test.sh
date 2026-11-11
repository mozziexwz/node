#!/usr/bin/env bash
# Runs the fail2ban step of a node deploy against the real fail2ban.
#
#   fail2ban_test.sh container STEP
#     Inside a disposable official Debian container (root, network, NET_ADMIN).
#     The packages, the configuration parser and nft are real; systemd is not
#     running there, so starting the service is stood in for.
#
#   MSBOOST_FAIL2BAN_HOST_TEST=1 fail2ban_test.sh host STEP
#     On a disposable machine with systemd and without fail2ban (a CI runner).
#     Everything is real, and fail2ban is removed again afterwards. A machine
#     that cannot download fail2ban from its package archive skips the check
#     with a warning: that says nothing about the step.
#
# STEP is the script written by:
#   MSBOOST_FAIL2BAN_STEP_OUT=/path go test ./internal/executor -run '^TestFail2banStepDump$'
# It allows 1.1.1.1 and 2606:4700:4700::1111 plus the session's own address.
set -Eeuo pipefail
umask 077

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
[[ $# == 2 && ( $1 == container || $1 == host ) && -f $2 ]] || { printf 'usage: fail2ban_test.sh container|host STEP\n' >&2; exit 2; }
[[ $(id -u) == 0 && $(uname -s) == Linux ]] || { printf 'Run as root on a disposable Linux system.\n' >&2; exit 2; }
mode=$1
step=$2
readonly jail=/etc/fail2ban/jail.d/zz-msboost.conf purge=/etc/fail2ban/fail2ban.d/zz-msboost.conf
readonly session='203.0.113.77 51234 10.0.0.5 22022'
export DEBIAN_FRONTEND=noninteractive

installed() { [[ $(dpkg-query -W -f='${Status}' fail2ban 2>/dev/null || true) == 'install ok installed' ]]; }

work=$(mktemp -d /run/msboost-f2b-test.XXXXXXXX)
shims=$work/shims.sh
: > "$shims"

# run_step prints what a deploy would read back: the step runs from standard
# input under the deploy's shell options, followed by more of the script.
run_step() {
  local task
  task=$(mktemp -d "$work/task.XXXXXXXX")
  {
    printf 'set -Eeuo pipefail\numask 077\nwork=%q\n' "$task"
    cat "$shims" "$step"
    printf "printf 'DEPLOY_CONTINUES\\\\n'\n"
  } > "$task/run.sh"
  SSH_CONNECTION=$session bash -s < "$task/run.sh" || fail "the step ended the deploy: $(cat "$task/fail2ban.log" 2>/dev/null)"
}
expect_status() {
  local output
  output=$(run_step)
  grep -qx 'DEPLOY_CONTINUES' <<< "$output" || fail "the deploy did not continue after the step: $output"
  grep -qx "MSBOOST_FAIL2BAN=$1" <<< "$output" || fail "expected $1, got: $output $(cat "$work"/task.*/fail2ban.log 2>/dev/null | tail -n 40)"
}

if [[ $mode == container ]]; then
  [[ ! -d /run/systemd/system && -r /etc/os-release ]] || fail 'container mode is for a container without systemd'
  release=$(. /etc/os-release && printf '%s %s' "${ID:-}" "${VERSION_ID:-}")
  [[ $release =~ ^debian\ (11|12|13)$ ]] || fail "an official Debian 11-13 container is required, not $release"
  ! installed && [[ ! -e /etc/fail2ban ]] || fail 'the container already has fail2ban'

  # The step insists on systemd; give it the directory and stand in for the
  # service manager. fail2ban-client stays real except for asking a server
  # that cannot run here.
  mkdir -p /run/systemd/system
  state=$work/state
  mkdir "$state"
  cat > "$shims" <<SHIMS
state=$state
systemctl() {
  case "\$1" in
    is-active) [[ "\${*: -1}" == fail2ban && -e "\$state/active" ]] ;;
    is-enabled) [[ -e "\$state/enabled" ]] ;;
    is-failed) return 1 ;;
    enable) : > "\$state/enabled" ;;
    restart) command fail2ban-client -t >/dev/null 2>&1 && : > "\$state/active" ;;
    stop) rm -f "\$state/active" ;;
    disable)
      rm -f "\$state/enabled"
      if [[ " \$* " == *" --now "* ]]; then rm -f "\$state/active"; fi
      ;;
  esac
}
fail2ban-client() {
  if [[ "\${1:-}" == status ]]; then [[ -e "\$state/active" ]]; return; fi
  command fail2ban-client "\$@"
}
SHIMS

  # A fresh image has no package lists: the step has to fetch them itself.
  expect_status enabled
  installed || fail 'fail2ban was not installed'
  for file in "$jail" "$purge"; do
    [[ -f $file && ! -L $file && $(stat -c '%U:%G:%a' "$file") == root:root:644 ]] || fail "$file is not a root-owned 0644 file"
    [[ $(head -n 1 "$file") == '# Managed by MSBOOST (msboost-fail2ban-v1)' ]] || fail "$file is not marked"
  done
  [[ ! -e /etc/fail2ban/jail.local && ! -e /etc/fail2ban/fail2ban.local ]] || fail 'the step wrote a file that belongs to the owner'

  fail2ban-client -t > /dev/null || fail 'the real fail2ban rejects the configuration'
  dump=$(fail2ban-client -d 2>/dev/null) || fail 'the real fail2ban cannot read the configuration'
  sees() {
    grep -Eq -- "$1" <<< "$dump" || { grep -E "sshd|dbpurgeage" <<< "$dump" | cut -c1-240 >&2; fail "fail2ban $(fail2ban-client --version 2>/dev/null | head -n 1) does not apply: $2"; }
  }
  sees "\['add', 'sshd', 'systemd'\]" 'the journal backend'
  for address in '127\.0\.0\.1/8' '::1' '1\.1\.1\.1' '2606:4700:4700::1111' '203\.0\.113\.77'; do
    sees "'sshd', 'addignoreip'.*'$address'" "allowlist entry $address"
  done
  sees "'sshd', 'maxretry', '?5'?\]" 'maxretry = 5'
  sees "'sshd', 'findtime', '?15m'?\]" 'findtime = 15m'
  sees "'sshd', 'bantime', '?1d'?\]" 'bantime = 1d'
  sees "'sshd', 'bantime\.increment', '?[Tt]rue'?\]" 'bantime.increment'
  sees "'sshd', 'bantime\.factor', '?1'?\]" 'bantime.factor = 1'
  sees "'sshd', 'bantime\.maxtime', '?1w'?\]" 'bantime.maxtime = 1w'
  # nftables-multiport, or the unified nftables action where only that exists.
  sees "'sshd', 'addaction', 'nftables" 'the nftables action'
  sees "'port', '22022'" 'the SSH port of this session'
  sees "'dbpurgeage', '?2592000'?\]" 'thirty days of ban history'

  # Running it again leaves the files as they are.
  before=$(sha256sum "$jail" "$purge")
  expect_status enabled
  [[ $(sha256sum "$jail" "$purge") == "$before" ]] || fail 'a second run changed the configuration'

  # The owner's jail.local takes precedence over these defaults, and from then
  # on the step leaves the server alone.
  printf '[sshd]\nmaxretry = 3\nignoreip = 198.51.100.7\n' > /etc/fail2ban/jail.local
  chmod 0644 /etc/fail2ban/jail.local
  dump=$(fail2ban-client -d 2>/dev/null) || fail 'fail2ban cannot read the configuration next to a jail.local'
  sees "'sshd', 'maxretry', '?3'?\]" "the owner's maxretry"
  sees "'sshd', 'addignoreip'.*'198\.51\.100\.7'" "the owner's allowlist"
  ! grep -Eq "'sshd', 'addignoreip'.*'1\.1\.1\.1'" <<< "$dump" || fail "the defaults were not overridden by the owner's allowlist"
  expect_status existing
  [[ $(sha256sum "$jail" "$purge") == "$before" && $(cat /etc/fail2ban/jail.local) == $'[sshd]\nmaxretry = 3\nignoreip = 198.51.100.7' ]] || fail 'an existing configuration was changed'

  printf 'PASS: %s, %s: configuration applied, idempotent, and second to jail.local\n' "$release" "$(fail2ban-client --version 2>/dev/null | head -n 1)"
  exit 0
fi

# Host mode: the real service on a disposable systemd machine.
[[ ${MSBOOST_FAIL2BAN_HOST_TEST:-} == 1 ]] || { printf 'Set MSBOOST_FAIL2BAN_HOST_TEST=1 on a disposable machine: this installs and removes fail2ban.\n' >&2; exit 2; }
[[ -d /run/systemd/system ]] || fail 'host mode needs systemd'
! installed && [[ ! -e /etc/fail2ban ]] || fail 'this machine already has fail2ban; refusing to touch it'
cleanup() {
  local result=$?
  trap - EXIT
  systemctl stop fail2ban 2>/dev/null || true
  apt-get purge -y -qq fail2ban > /dev/null 2>&1 || true
  rm -f -- "$jail" "$purge" /etc/fail2ban/jail.local
  rmdir /etc/fail2ban/jail.d /etc/fail2ban/fail2ban.d /etc/fail2ban 2>/dev/null || true
  rm -rf -- "$work"
  exit "$result"
}
trap cleanup EXIT

# The check needs fail2ban from this machine's package archive. Runners now and
# then cannot resolve theirs for minutes; the step would then report
# "unavailable", correctly, and there would be nothing to check.
packages_available() {
  local -a apt=(apt-get -o DPkg::Lock::Timeout=120 -o Acquire::Retries=2)
  local -a fetch=(install -y -qq --no-install-recommends --download-only fail2ban python3-systemd nftables)
  timeout 150 "${apt[@]}" "${fetch[@]}" > "$work/archive.log" 2>&1 && return 0
  timeout 150 "${apt[@]}" update -qq >> "$work/archive.log" 2>&1 || true
  timeout 150 "${apt[@]}" "${fetch[@]}" >> "$work/archive.log" 2>&1
}
if ! packages_available; then
  reason=$(grep -m 1 -E '^(E|W|Err):' "$work/archive.log" | cut -c1-200 || true)
  printf '::warning title=fail2ban check on a real service skipped::This machine could not download fail2ban from its package archive, so the check did not run. %s\n' "$reason"
  printf 'SKIP: the package archive is unreachable; the fail2ban step was not checked against a real service here.\n'
  exit 0
fi

expect_status enabled
systemctl is-active --quiet fail2ban || fail 'fail2ban is not running'
systemctl is-enabled --quiet fail2ban || fail 'fail2ban does not start at boot'
! systemctl is-enabled --quiet nftables 2>/dev/null || fail 'nftables.service was enabled'
fail2ban-client status sshd > /dev/null || fail 'the sshd jail is not running'
ignored=$(fail2ban-client get sshd ignoreip)
for address in 127.0.0.0/8 ::1 1.1.1.1 2606:4700:4700::1111 203.0.113.77; do
  grep -qF -- "$address" <<< "$ignored" || fail "the running jail does not ignore $address: $ignored"
done
[[ $(fail2ban-client get sshd bantime) == 86400 ]] || fail 'bantime is not one day'
[[ $(fail2ban-client get sshd findtime) == 900 ]] || fail 'findtime is not fifteen minutes'
[[ $(fail2ban-client get sshd maxretry) == 5 ]] || fail 'maxretry is not five'
[[ $(fail2ban-client get dbpurgeage | grep -oE '[0-9]+' | head -n 1) == 2592000 ]] || fail 'ban history is not kept for thirty days'

# A ban becomes a firewall rule on the session's SSH port, and goes again.
fail2ban-client set sshd banip 198.51.100.200 > /dev/null || fail 'banning failed'
rules=''
for attempt in 1 2 3 4 5 6 7 8 9 10; do
  rules=$(nft list ruleset 2>/dev/null || true)
  if grep -qF 198.51.100.200 <<< "$rules"; then break; fi
  sleep 1
done
grep -qF 198.51.100.200 <<< "$rules" || fail "a ban did not reach the firewall: $(tail -n 20 /var/log/fail2ban.log 2>/dev/null)"
grep -Eq 'dport .*22022' <<< "$rules" || fail "the ban does not cover the SSH port: $rules"
fail2ban-client set sshd unbanip 198.51.100.200 > /dev/null || fail 'unbanning failed'

# A second run restarts nothing.
pid=$(systemctl show fail2ban -p MainPID --value)
before=$(sha256sum "$jail" "$purge")
expect_status enabled
[[ $(systemctl show fail2ban -p MainPID --value) == "$pid" && $(sha256sum "$jail" "$purge") == "$before" ]] || fail 'a second run restarted or reconfigured the service'

# With the owner's own configuration present, nothing is touched.
printf '[sshd]\nmaxretry = 3\n' > /etc/fail2ban/jail.local
expect_status existing
[[ $(systemctl show fail2ban -p MainPID --value) == "$pid" && $(sha256sum "$jail" "$purge") == "$before" ]] || fail 'an existing configuration was changed'

printf 'PASS: %s with systemd: installed, running, banning on the SSH port, idempotent\n' "$(fail2ban-client --version 2>/dev/null | head -n 1)"
