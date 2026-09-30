#!/usr/bin/env bash
# Real service sandbox + real UFW, both in private namespaces, no host UFW changes.
set -Eeuo pipefail
[[ $# == 2 ]] || { printf 'usage: relay_firewall_systemd_test.sh AGENT_BINARY FIREWALL_TEST_BINARY\n' >&2; exit 2; }
[[ $(id -u) == 0 && -f $1 && -f $2 ]] || exit 2
repo=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
work=$(mktemp -d /root/msboost-firewall-test.XXXXXXXX)
install -m 0700 "$1" "$work/agent"
install -m 0700 "$2" "$work/firewall.test"
python3 - "$repo/deploy/install-agent.sh" "$work/firewall-unit" <<'PY'
import pathlib,sys
source=pathlib.Path(sys.argv[1]).read_text()
unit=source.split("cat > \"$stage/firewall-unit\" <<'FIREWALL'\n",1)[1].split("\nFIREWALL",1)[0]
pathlib.Path(sys.argv[2]).write_text(unit+"\n")
PY
[[ $work =~ ^/root/msboost-firewall-test\.[A-Za-z0-9]{8}$ && -d $work && ! -L $work ]]
[[ $(ufw status) == 'Status: inactive' ]]
for unit in msboost-relay.service msboost-relay-firewall.service; do
  [[ ! -e /etc/systemd/system/$unit && ! -e /run/systemd/system/$unit ]]
  [[ -z $(systemctl show "$unit" -p FragmentPath --value) && $(systemctl show "$unit" -p MainPID --value) == 0 ]]
done
[[ ! -e /run/msboost-firewall-probe ]]
# Fresh Debian/Ubuntu runners need not have installed any libexec program yet.
# Create only the standard parent; each fixture still gets its own mktemp path.
[[ ! -L /usr/local/libexec ]]
mkdir -p /usr/local/libexec
[[ -d /usr/local/libexec && $(stat -c %u /usr/local/libexec) == 0 ]]
[[ $(stat -c %a /usr/local/libexec) =~ ^[0-7]?[0-7][0145][0145]$ ]]
stage=$(mktemp -d /usr/local/libexec/msboost-firewall-test.XXXXXXXX)
chmod 0755 "$stage"
suffix=${stage##*.}; ns=msbf-$suffix; peer=msbc-$suffix
created=0; ns_created=0; peer_created=0
snapshot_host_rules() {
  # iptables-save emits wall-clock comments and live built-in-chain counters
  # on some versions. Compare all policy/rule content, not that moving metadata.
  "$1" | sed '/^#/d; s/ \[[0-9][0-9]*:[0-9][0-9]*\]$/ [0:0]/'
}
cleanup() {
  local result=$?
  trap - EXIT
  if [[ $created == 1 ]]; then
    journalctl -u msboost-relay.service -u msboost-relay-firewall.service --since '-5 min' --no-pager -o cat > "$work/ufw-systemd.log"
    systemctl stop msboost-relay.service msboost-relay-firewall.service
    nsenter --net="/run/netns/$ns" -- "$stage/agent" --capability relay-firewall-cleanup
    for unit in msboost-relay.service msboost-relay-firewall.service; do
      cmp -s "$work/ufw-$unit" "/run/systemd/system/$unit" || { printf 'Unit changed; retained\n' >&2; exit 2; }
      rm -- "/run/systemd/system/$unit"
    done
    systemctl daemon-reload
    systemctl reset-failed msboost-relay.service msboost-relay-firewall.service 2>/dev/null || true
  fi
  [[ $peer_created == 0 ]] || ip netns delete "$peer"
  [[ $ns_created == 0 ]] || ip netns delete "$ns"
  # Keep the small private fixture for reproducibility; no broad deletion.
  printf 'PRIVATE_SERVICE_FIXTURE=%s\n' "$stage"
  snapshot_host_rules iptables-save > "$work/ufw-host-after"
  snapshot_host_rules ip6tables-save > "$work/ufw6-host-after"
  diff -u "$work/ufw-host-before" "$work/ufw-host-after"
  diff -u "$work/ufw6-host-before" "$work/ufw6-host-after"
  printf 'HOST_FIREWALL_UNCHANGED\n'
  exit "$result"
}
install -m 0755 "$work/agent" "$stage/agent"
install -m 0755 "$work/firewall.test" "$stage/firewall.test"
cp -a /etc/ufw "$stage/ufw"
cp -a /etc/default/ufw "$stage/default-ufw"
snapshot_host_rules iptables-save > "$work/ufw-host-before"
snapshot_host_rules ip6tables-save > "$work/ufw6-host-before"
trap cleanup EXIT
ip netns add "$ns"; ns_created=1
ip netns add "$peer"; peer_created=1
ip -n "$ns" link set lo up
ip -n "$peer" link set lo up
ip -n "$ns" link add msbf-server type veth peer name msbf-client
ip -n "$ns" link set msbf-client netns "$peer"
ip -n "$ns" addr add 10.237.54.1/24 dev msbf-server
ip -n "$peer" addr add 10.237.54.2/24 dev msbf-client
ip -n "$ns" link set msbf-server up
ip -n "$peer" link set msbf-client up
ufw_private() {
  nsenter --net="/run/netns/$ns" -- unshare --mount --propagation private -- bash -c '
    set -Eeuo pipefail
    mount --bind "$1/ufw" /etc/ufw
    mount --bind "$1/default-ufw" /etc/default/ufw
    shift
    ufw "$@"
  ' ufw-fixture "$stage" "$@"
}
ufw_private --force enable
ufw_private allow 22/tcp
sed "s|/usr/local/libexec/msboost-agent/relay-firewall|$stage/agent|" "$work/firewall-unit" > "$work/ufw-msboost-relay-firewall.service"
cat >> "$work/ufw-msboost-relay-firewall.service" <<EOF
[Service]
NetworkNamespacePath=/run/netns/$ns
BindReadOnlyPaths=$stage/ufw:/etc/ufw $stage/default-ufw:/etc/default/ufw
EOF
cat > "$work/ufw-msboost-relay.service" <<EOF
[Unit]
Description=MSBOOST disposable UFW DynamicUser probe
[Service]
DynamicUser=true
ExecStart=$stage/firewall.test -test.run=^TestFirewallServiceChild$ -test.v -test.timeout=120s
Environment=MSBOOST_FIREWALL_SERVICE_CHILD=1
RuntimeDirectory=msboost-firewall-probe
RuntimeDirectoryMode=0700
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
CapabilityBoundingSet=
NetworkNamespacePath=/run/netns/$ns
EOF
for unit in msboost-relay.service msboost-relay-firewall.service; do install -m 0644 "$work/ufw-$unit" "/run/systemd/system/$unit"; done
created=1
systemctl daemon-reload
systemctl start msboost-relay-firewall.service msboost-relay.service
for attempt in $(seq 1 50); do [[ ! -f /run/msboost-firewall-probe/ready ]] || break; sleep 1; done
[[ $(cat /run/msboost-firewall-probe/ready) == ready ]]
port=$(cat /run/msboost-firewall-probe/port)
[[ $port =~ ^[0-9]+$ ]]
python3 - "$port" <<'PY'
import json,sys
report=json.load(open('/run/msboost-relay-firewall/status.json'))
assert report['backend']=='ufw' and not report.get('error') and int(sys.argv[1]) in report['ports'],report
PY
probe() { nsenter --net="/run/netns/$peer" -- python3 - "$port" <<'PY'
import socket,sys
with socket.create_connection(('10.237.54.1',int(sys.argv[1])),2) as s:
 s.settimeout(2);s.sendall(b'echo');assert s.recv(4)==b'echo'
PY
}
probe
# Repeated administrator operations must serialize with the continuously
# running helper, not race UFW's multi-command chain teardown/rebuild.
for reload in $(seq 1 12); do
  ufw_private reload
  sleep 2
  probe
done
systemctl stop msboost-relay.service
sleep 3
! nsenter --net="/run/netns/$ns" -- iptables -C MSBOOST-RELAY -p tcp -m tcp --dport "$port" -m comment --comment msboost-relay-firewall-v1 -j ACCEPT
printf 'REAL_UFW_SYSTEMD_DYNAMIC_USER_PASS port=%s reload_repaired=true stopped_port_removed=true\n' "$port"
