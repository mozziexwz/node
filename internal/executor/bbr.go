package executor

// bbrTuneScript is injected only into customer-owned VPS tasks (deploy,
// customer relay and optional customer front). It is deliberately not used by
// the control-plane installer or by managed executor/donation-relay Agents.
// A dedicated sysctl.d file makes repeated runs idempotent without replacing
// the operator's /etc/sysctl.conf.
const bbrTuneScript = `
msboost_phase=bbr
[ "$(id -u)" = 0 ]
[ -d /etc/sysctl.d ]
[ ! -L /etc/sysctl.d ]
[ "$(stat -c %u -- /etc/sysctl.d)" = 0 ]
bbr_dir_mode=$(stat -c %a -- /etc/sysctl.d)
(( (8#$bbr_dir_mode & 022) == 0 ))
if ! command -v sysctl >/dev/null || ! command -v modprobe >/dev/null; then
  [ -f /etc/os-release ]
  . /etc/os-release
  case "$ID" in debian|ubuntu) ;; *) exit 1 ;; esac
  export DEBIAN_FRONTEND=noninteractive
  apt-get update > "$work/bbr-packages.log" 2>&1
  apt-get install -y --no-install-recommends procps kmod >> "$work/bbr-packages.log" 2>&1
fi
modprobe tcp_bbr 2> "$work/bbr-modprobe.log" || [ -d /sys/module/tcp_bbr ]
[ -d /sys/module/tcp_bbr ]
bbr_conf=/etc/sysctl.d/zz-msboost-bbr.conf
[ ! -L "$bbr_conf" ]
if [ -e "$bbr_conf" ]; then
  [ -f "$bbr_conf" ]
  [ "$(stat -c %u -- "$bbr_conf")" = 0 ]
fi
cat > "$work/zz-msboost-bbr.conf" <<'MSBOOST_BBR_CONF'
net.core.default_qdisc=fq
net.ipv4.tcp_congestion_control=bbr
net.ipv4.tcp_fastopen=3
net.ipv4.tcp_timestamps=1
net.ipv4.tcp_sack=1
net.ipv4.tcp_window_scaling=1
net.ipv4.tcp_mtu_probing=1
net.ipv4.tcp_adv_win_scale=1
net.core.somaxconn=4096
net.ipv4.tcp_max_syn_backlog=4096
net.core.netdev_max_backlog=10000
net.ipv4.conf.all.rp_filter=2
net.ipv4.conf.default.rp_filter=2
MSBOOST_BBR_CONF
chmod 0644 "$work/zz-msboost-bbr.conf"
if [ ! -f "$bbr_conf" ] || ! cmp -s "$work/zz-msboost-bbr.conf" "$bbr_conf"; then
  install -o root -g root -m 0644 "$work/zz-msboost-bbr.conf" "$bbr_conf"
fi
# v0.3.0 used a 99-* file. Remove only an exact copy of our old configuration;
# an operator-modified file is left untouched and the new drop-in wins at boot.
legacy_bbr_conf=/etc/sysctl.d/99-msboost-bbr.conf
if [ ! -L "$legacy_bbr_conf" ] && [ -f "$legacy_bbr_conf" ] &&
   [ "$(stat -c %u -- "$legacy_bbr_conf")" = 0 ] &&
   cmp -s "$work/zz-msboost-bbr.conf" "$legacy_bbr_conf"; then
  rm -f -- "$legacy_bbr_conf"
fi
sysctl -p "$bbr_conf" > "$work/bbr-sysctl.log"
sysctl --system >> "$work/bbr-sysctl.log" 2>&1
# procps replays /etc/sysctl.conf after sysctl.d, and provider images may set
# the same keys there. Reapply our dedicated file after that replay; the zz-*
# name also wins over Debian's 99-sysctl.conf on the next systemd boot.
sysctl -p "$bbr_conf" >> "$work/bbr-sysctl.log" 2>&1
while IFS='=' read -r bbr_key bbr_value; do
  [ "$(sysctl -n "$bbr_key")" = "$bbr_value" ]
done < "$bbr_conf"
msboost_phase=install
`
