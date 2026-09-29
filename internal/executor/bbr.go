package executor

// Optimization is separate from provisioning. Only our candidate keys are
// applied; unrelated sysctl configuration is never reloaded.
const bbrTuneScript = `
msboost_phase=bbr
[ "$(id -u)" = 0 ]
[ -d /etc/sysctl.d ] && [ ! -L /etc/sysctl.d ]
[ "$(stat -c %u -- /etc/sysctl.d)" = 0 ]
bbr_dir_mode=$(stat -c %a -- /etc/sysctl.d)
(( (8#$bbr_dir_mode & 022) == 0 ))
bbr_conf=/etc/sysctl.d/zz-msboost-bbr.conf
[ ! -L "$bbr_conf" ]
if [ -e "$bbr_conf" ]; then
  [ -f "$bbr_conf" ] && [ "$(stat -c '%u:%h' -- "$bbr_conf")" = 0:1 ]
  bbr_mode=$(stat -c %a -- "$bbr_conf")
  (( (8#$bbr_mode & 022) == 0 ))
fi
msboost_bbr_rollback() {
  local key value failed=0
  while IFS='=' read -r key value; do
    sysctl -w "$key=$value" >> "$work/bbr-rollback.log" 2>&1 || failed=1
  done < "$work/bbr-before"
  return "$failed"
}
msboost_bbr_apply() {
  if ! command -v sysctl >/dev/null || ! command -v modprobe >/dev/null; then
    export DEBIAN_FRONTEND=noninteractive
    apt-get update > "$work/bbr-packages.log" 2>&1 &&
      apt-get install -y --no-install-recommends procps kmod >> "$work/bbr-packages.log" 2>&1 || return 1
  fi
  modprobe tcp_bbr 2> "$work/bbr-modprobe.log" || [ -d /sys/module/tcp_bbr ] || return 1
  [ -d /sys/module/tcp_bbr ] || return 1
  cat > "$work/bbr-candidate" <<'MSBOOST_BBR_CONF' || return 1
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
  : > "$work/bbr-before" || return 1
  : > "$work/bbr-supported" || return 1
  local key value previous
  while IFS='=' read -r key value; do
    if previous=$(sysctl -n "$key" 2>/dev/null); then
      printf '%s=%s\n' "$key" "$previous" >> "$work/bbr-before" || return 1
      printf '%s=%s\n' "$key" "$value" >> "$work/bbr-supported" || return 1
    elif [[ "$key" == net.core.default_qdisc || "$key" == net.ipv4.tcp_congestion_control ]]; then
      return 1
    fi
  done < "$work/bbr-candidate"
  if ! sysctl -p "$work/bbr-supported" > "$work/bbr-sysctl.log" 2>&1; then
    msboost_bbr_rollback || return 2
    return 1
  fi
  while IFS='=' read -r key value; do
    if [[ "$(sysctl -n "$key" 2>/dev/null)" != "$value" ]]; then
      msboost_bbr_rollback || return 2
      return 1
    fi
  done < "$work/bbr-supported"
  local candidate
  candidate=$(mktemp /etc/sysctl.d/.msboost-bbr.XXXXXXXX) || { msboost_bbr_rollback || return 2; return 1; }
  if ! install -o root -g root -m 0644 "$work/bbr-supported" "$candidate" || ! mv -T -- "$candidate" "$bbr_conf"; then
    rm -f -- "$candidate"
    msboost_bbr_rollback || return 2
    return 1
  fi
}
bbr_status=enabled
if msboost_bbr_apply; then
  :
else
  bbr_code=$?
  bbr_status=unavailable
  if [[ $bbr_code == 2 ]]; then bbr_status=review_required; fi
fi
printf 'MSBOOST_BBR=%s\n' "$bbr_status"
msboost_phase=install
`
