#!/usr/bin/env bash
# Opt-in real data-plane test. No production enrollment, services or firewall.
set -Eeuo pipefail
[[ $# == 3 ]] || { printf 'Usage: bash relay_endurance_test.sh /absolute/relay.test /absolute/gost 5m|30m|24h\n' >&2; exit 2; }
test_binary=$1
gost_binary=$2
duration=$3
[[ $(uname -s) == Linux && $(id -u) == 0 ]] || { printf 'Linux root required for a NEW network namespace.\n' >&2; exit 2; }
case "$duration" in 5m) deadline=8m;; 30m) deadline=33m;; 24h) deadline=24h3m;; *) printf 'Duration must explicitly be 5m, 30m, or 24h.\n' >&2; exit 2;; esac
for binary in "$test_binary" "$gost_binary"; do
  [[ $binary == /* && -f $binary && ! -L $binary && -x $binary ]] || { printf 'Absolute nonsymlink executable files required.\n' >&2; exit 2; }
done
printf '%s  %s\n' '1d8f971e9447cf4114fb1376b85c8c14e840db50ae8dbe895398a34e97c13e08' "$gost_binary" | sha256sum --check --strict
command -v unshare >/dev/null
command -v ip >/dev/null
printf 'REAL_GOST_ISOLATED_RUN duration=%s topology=single_tcp,two_hop_tcp,three_hop_tls reconnects=forbidden\n' "$duration"
# No existing namespace is entered. Only the newly created lo link is changed.
# The Go test independently checks net.Interfaces; /sys can reflect host sysfs.
exec unshare --net -- bash -c '
  set -Eeuo pipefail
  ip link set lo up
  exec env MSBOOST_TEST_LOOPBACK_NETNS=1 GOST_TEST_BINARY="$2" MSBOOST_KEEP_LAST_DURATION="$3" "$1" -test.run "^TestRealGostKeepLastEndurance$" -test.parallel=3 -test.count=1 -test.v -test.timeout="$4"
' msboost-isolated-endurance "$test_binary" "$gost_binary" "$duration" "$deadline" </dev/null
