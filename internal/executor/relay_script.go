package executor

// User values arrive only in base64-decoded variables. Python renders JSON and
// systemd receives fixed paths; no supplied value becomes executable shell text.
const relayInstallScript = `
[ "$(id -u)" = 0 ]
command -v systemctl >/dev/null
systemd_version=$(systemctl --version | awk 'NR==1 {print $2}')
[ "$systemd_version" -ge 247 ]
work=$(mktemp -d /run/msboost-relay.XXXXXX)
unit="msboost-free-${task_id}.service"
confdir="/etc/msboost-free/${task_id}"
[ ! -e "$confdir" ]
[ ! -e "/etc/systemd/system/$unit" ]
installed=0
ufw_owned=0
firewalld_runtime_owned=0
firewalld_permanent_owned=0
firewalld_zone=''
cleanup() {
  rm -rf -- "$work"
  if [ "$installed" = 0 ]; then
    if [ "$ufw_owned" = 1 ]; then ufw --force delete allow "$port/tcp" >/dev/null 2>&1 || true; fi
    if [ "$firewalld_runtime_owned" = 1 ]; then firewall-cmd --zone="$firewalld_zone" --remove-port="$port/tcp" >/dev/null 2>&1 || true; fi
    if [ "$firewalld_permanent_owned" = 1 ]; then firewall-cmd --permanent --zone="$firewalld_zone" --remove-port="$port/tcp" >/dev/null 2>&1 || true; fi
    systemctl disable --now "$unit" >/dev/null 2>&1 || true
    rm -f -- "/etc/systemd/system/$unit"
    rm -rf -- "$confdir"
    systemctl daemon-reload >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT
if ! command -v python3 >/dev/null || ! command -v curl >/dev/null || ! command -v tar >/dev/null; then
  [ -f /etc/os-release ]
  . /etc/os-release
  case "$ID" in debian|ubuntu) ;; *) exit 1 ;; esac
  export DEBIAN_FRONTEND=noninteractive
  apt-get update > "$work/packages.log" 2>&1
  apt-get install -y --no-install-recommends python3 curl tar ca-certificates >> "$work/packages.log" 2>&1
fi
python3 - "$target_host" "$target_port" <<'PY'
import socket,sys
with socket.create_connection((sys.argv[1],int(sys.argv[2])),timeout=10): pass
PY
curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 --max-time 180 "$gost_url" -o "$work/gost.tar.gz"
printf '%s  %s\n' "$gost_sha" "$work/gost.tar.gz" | sha256sum -c - >/dev/null
tar -xzf "$work/gost.tar.gz" -C "$work" gost
"$work/gost" -V >/dev/null
install -d -m 0755 /usr/local/libexec/msboost-free
install -m 0755 "$work/gost" "/usr/local/libexec/msboost-free/gost-${gost_sha}"
install -d -m 0700 "$confdir"
cat > "/etc/systemd/system/$unit" <<EOF
[Unit]
Description=MSBOOST customer TCP forwarding
After=network-online.target
Wants=network-online.target
[Service]
Type=simple
DynamicUser=true
LoadCredential=config:${confdir}/config.json
ExecStart=/usr/local/libexec/msboost-free/gost-${gost_sha} -C %d/config
Restart=on-failure
RestartSec=3
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
for attempt in $(seq 1 12); do
  port=$(python3 - <<'PY'
import secrets,socket
for _ in range(100):
    port=20000+secrets.randbelow(40000)
    with socket.socket() as s:
        try: s.bind(('0.0.0.0',port))
        except OSError: continue
        print(port);break
else: raise SystemExit(1)
PY
)
  python3 - "$port" "$target_host" "$target_port" "$confdir/config.json" "$rate_bytes" <<'PY'
import json,sys
p,host,target,path,rate=sys.argv[1:]
address=('['+host+']' if ':' in host else host)+':'+target
config={'services':[{'name':'msboost-free','addr':':'+p,'handler':{'type':'tcp'},'listener':{'type':'tcp'},'forwarder':{'nodes':[{'name':'target','addr':address}]},'limiter':'per-port'}],'limiters':[{'name':'per-port','limits':['$ 625000B 625000B']} ]}
if int(rate)==0:
    config['services'][0].pop('limiter')
    config.pop('limiters')
else:
    config['limiters'][0]['limits']=['$ '+rate+'B '+rate+'B']
with open(path,'w') as f: json.dump(config,f)
PY
  systemctl reset-failed "$unit" >/dev/null 2>&1 || true
  systemctl restart "$unit"
  sleep 2
  if systemctl is-active --quiet "$unit"; then
    pid=$(systemctl show "$unit" -p MainPID --value)
    if python3 - "$pid" "$port" <<'PY'
import os,sys
pid,port=sys.argv[1],int(sys.argv[2])
inodes=set()
for fd in os.listdir('/proc/'+pid+'/fd'):
    try: val=os.readlink('/proc/'+pid+'/fd/'+fd)
    except OSError: continue
    if val.startswith('socket:['): inodes.add(val[8:-1])
for name in ['tcp','tcp6']:
    with open('/proc/net/'+name) as f:
        for line in list(f)[1:]:
            v=line.split()
            if int(v[1].rsplit(':',1)[1],16)==port and v[3]=='0A' and v[9] in inodes: raise SystemExit(0)
raise SystemExit(1)
PY
    then
      if command -v ufw >/dev/null && ufw status 2>/dev/null | grep -q '^Status: active'; then
        if ! ufw show added 2>/dev/null | grep -Eq "(^|[[:space:]])allow[[:space:]]+$port/tcp([[:space:]]|$)"; then
          ufw allow "$port/tcp" >/dev/null
          ufw_owned=1
          printf '%s\n' "$port" > "$confdir/ufw-owned"
        fi
      fi
      if command -v firewall-cmd >/dev/null && firewall-cmd --state >/dev/null 2>&1; then
        firewalld_zone=$(firewall-cmd --get-default-zone)
        [[ "$firewalld_zone" =~ ^[A-Za-z0-9_.-]+$ ]]
        if ! firewall-cmd --quiet --zone="$firewalld_zone" --query-port="$port/tcp"; then
          firewall-cmd --zone="$firewalld_zone" --add-port="$port/tcp" >/dev/null
          firewalld_runtime_owned=1
          printf '%s %s\n' "$firewalld_zone" "$port" > "$confdir/firewalld-runtime-owned"
        fi
        if ! firewall-cmd --quiet --permanent --zone="$firewalld_zone" --query-port="$port/tcp"; then
          firewall-cmd --permanent --zone="$firewalld_zone" --add-port="$port/tcp" >/dev/null
          firewalld_permanent_owned=1
          printf '%s %s\n' "$firewalld_zone" "$port" > "$confdir/firewalld-permanent-owned"
        fi
      fi
      systemctl enable "$unit" >/dev/null
      installed=1
      printf 'MSBOOST_RELAY_PORT=%s\n' "$port"
      exit 0
    fi
  fi
  systemctl stop "$unit" >/dev/null 2>&1 || true
done
exit 1
`

const relayCleanupScript = `
confdir="/etc/msboost-free/${task_id}"
systemctl disable --now "msboost-free-${task_id}.service" >/dev/null 2>&1 || true
if [ -f "$confdir/ufw-owned" ]; then
  read -r port < "$confdir/ufw-owned"
  if [[ "$port" =~ ^[0-9]+$ ]]; then ufw --force delete allow "$port/tcp" >/dev/null 2>&1 || true; fi
fi
for mode in runtime permanent; do
  if [ -f "$confdir/firewalld-${mode}-owned" ]; then
    read -r zone port < "$confdir/firewalld-${mode}-owned"
    if [[ "$zone" =~ ^[A-Za-z0-9_.-]+$ && "$port" =~ ^[0-9]+$ ]]; then
      args=()
      if [ "$mode" = permanent ]; then args+=(--permanent); fi
      firewall-cmd "${args[@]}" --zone="$zone" --remove-port="$port/tcp" >/dev/null 2>&1 || true
    fi
  fi
done
rm -f -- "/etc/systemd/system/msboost-free-${task_id}.service"
rm -rf -- "$confdir"
systemctl daemon-reload
`
