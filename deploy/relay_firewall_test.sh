#!/usr/bin/env bash
# Real UFW regression: private config mounts AND private network. Never enable
# UFW in the host namespace, including when the child is called accidentally.
set -Eeuo pipefail
if [[ ${1:-} != --private-child ]]; then
  [[ $(id -u) == 0 && $# == 1 && -f $1 ]] || { printf 'Run as root with the compiled relayfirewall test binary.\n' >&2; exit 1; }
  exec unshare --mount --net -- bash "$0" --private-child "$(readlink -f "$1")"
fi
[[ $# == 2 && $(id -u) == 0 && $(readlink /proc/self/ns/net) != "$(readlink /proc/1/ns/net)" ]] || exit 1
for command in ufw ip iptables ip6tables nsenter python3; do command -v "$command" >/dev/null; done
work=$(mktemp -d /tmp/msboost-firewall-test.XXXXXXXX)
trap '[[ $work =~ ^/tmp/msboost-firewall-test\.[A-Za-z0-9]{8}$ && -d $work && ! -L $work ]] && rm -rf -- "$work"' EXIT
mount --make-rprivate /
cp -a /etc/ufw "$work/ufw"
cp -a /etc/default/ufw "$work/default-ufw"
mount --bind "$work/ufw" /etc/ufw
mount --bind "$work/default-ufw" /etc/default/ufw
ip link set lo up
ufw --force enable
ufw allow 22/tcp
MSBOOST_FIREWALL_NETNS_TEST=1 MSBOOST_FIREWALL_REAL_UFW=1 "$2" -test.v -test.timeout=90s
