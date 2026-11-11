package executor

import (
	"errors"
	"net/netip"
	"strconv"
	"strings"
)

// Fail2banOptions asks a deploy or an upgrade to protect the server's SSH with
// fail2ban. Only the panel or the local program sets it: a browser cannot, and
// an executor from before this option ignores the field and skips the step.
type Fail2banOptions struct {
	// Allow lists addresses that are never banned: the panel's executors.
	Allow []string `json:"allow,omitempty"`
	// ClientBits also allows the address this SSH session comes from, as a
	// prefix of that many bits: 32 keeps the single address, 16 covers a home
	// connection whose address changes. Zero adds nothing.
	ClientBits int `json:"clientBits,omitempty"`
}

// MaxFail2banAllow bounds the allowlist a request may carry.
const MaxFail2banAllow = 32

// Fail2banStepNote announces the step to a caller that shows progress.
const Fail2banStepNote = "Protecting SSH with fail2ban"

func ValidateFail2ban(o *Fail2banOptions) error {
	if o == nil {
		return nil
	}
	if len(o.Allow) > MaxFail2banAllow {
		return errors.New("SSH 防护白名单最多 32 项")
	}
	for _, address := range o.Allow {
		if PublicIP(address) != nil {
			return errors.New("SSH 防护白名单只接受公网 IP")
		}
	}
	switch o.ClientBits {
	case 0, 16, 24, 32:
		return nil
	}
	return errors.New("SSH 防护的来源网段无效")
}

// fail2banStep is appended to a deploy or upgrade script after the node is
// running; nil adds nothing. The step never fails the task: it reports
// MSBOOST_FAIL2BAN on its own, like BBR.
func fail2banStep(o *Fail2banOptions) string {
	if o == nil {
		return ""
	}
	allow := make([]string, 0, len(o.Allow))
	for _, address := range o.Allow {
		if ip, err := netip.ParseAddr(address); err == nil {
			allow = append(allow, ip.String())
		}
	}
	return encodedAssignment("f2b_allow", strings.Join(allow, " ")) + encodedAssignment("f2b_client_bits", strconv.Itoa(o.ClientBits)) + fail2banScript
}

// fail2banStatus is the step's report, limited to its vocabulary.
func fail2banStatus(out []byte) string {
	switch status := marker(out, "MSBOOST_FAIL2BAN"); status {
	case "enabled", "existing", "review_required":
		return status
	}
	return "unavailable"
}

// The defaults are those of the fail2ban_script installer: one day for five
// failures within fifteen minutes, doubling up to a week for repeat offenders.
//
// Only two files are written, both marked: a jail.d/*.conf and a
// fail2ban.d/*.conf. fail2ban reads them after the distribution's defaults and
// before jail.local, so whatever the server's owner configures later takes
// precedence, and a server that already has its own configuration is left
// exactly as it is ("existing").
//
// The configuration with the allowlist is in place before the package is
// installed: Debian starts the service at once, and it looks back over the
// last failures, which may include this very session's earlier attempts.
//
// The step runs in a subshell inside an if, so nothing in it can end the
// surrounding deploy; every command is therefore checked explicitly. Its
// standard input is detached: the outer script itself arrives on stdin.
const fail2banScript = `
printf 'MSBOOST_STEP=` + Fail2banStepNote + `\n'
msboost_fail2ban() {
  local dir=/etc/fail2ban
  local jail="$dir/jail.d/zz-msboost.conf" purge="$dir/fail2ban.d/zz-msboost.conf"
  local marker='# Managed by MSBOOST (msboost-fail2ban-v1)'
  local deadline=$((SECONDS + 300))
  export DEBIAN_FRONTEND=noninteractive NEEDRESTART_MODE=a APT_LISTCHANGES_FRONTEND=none

  [ "$(id -u)" = 0 ] || return 1
  [ -d /run/systemd/system ] || return 1
  command -v apt-get >/dev/null 2>&1 || return 1
  command -v dpkg-query >/dev/null 2>&1 || return 1

  f2b_have() {
    local state
    state=$(dpkg-query -W -f='${Status}' "$1" 2>/dev/null || true)
    [ "$state" = 'install ok installed' ]
  }
  f2b_marked() {
    local first=''
    [ -f "$1" ] && [ ! -L "$1" ] || return 1
    IFS= read -r first < "$1" || true
    [ "$first" = "$marker" ]
  }

  # Whatever the owner configured stays untouched: any *.local, another
  # jail.d/*.conf, or a packaged file that was edited.
  local path found changed installed=0
  if [ -e "$dir" ] || [ -L "$dir" ]; then
    [ -d "$dir" ] && [ ! -L "$dir" ] || return 10
    found=$(find "$dir" -maxdepth 2 -name '*.local' -print -quit 2>/dev/null || true)
    [ -z "$found" ] || return 10
    for path in "$dir"/jail.d/*.conf; do
      [ -e "$path" ] || [ -L "$path" ] || continue
      case "$path" in "$dir/jail.d/defaults-debian.conf" | "$jail") continue ;; esac
      return 10
    done
    for path in "$jail" "$purge"; do
      if [ -e "$path" ] || [ -L "$path" ]; then
        f2b_marked "$path" || return 10
      fi
    done
  fi
  if f2b_have fail2ban; then
    installed=1
    changed=$(dpkg --verify fail2ban 2>/dev/null || true)
    if grep -Eq ' /etc/fail2ban/(jail\.conf|fail2ban\.conf|jail\.d/defaults-debian\.conf)$' <<< "$changed"; then
      return 10
    fi
  fi

  # The allowlist: loopback, the panel's executors, and where this session
  # comes from as the server sees it.
  local allow='127.0.0.1/8 ::1' entry='' client item
  local -a items=() session=()
  f2b_allow_add() {
    case " $allow " in *" $1 "*) ;; *) allow+=" $1" ;; esac
  }
  read -r -a items <<< "$f2b_allow" || true
  for item in ${items[@]+"${items[@]}"}; do
    [[ "$item" =~ ^[0-9A-Fa-f:.]{2,45}$ ]] || return 1
    f2b_allow_add "$item"
  done
  read -r -a session <<< "${SSH_CONNECTION:-}" || true
  client=${session[0]:-}
  client=${client#::ffff:}
  if [[ "$client" =~ ^([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})$ ]]; then
    local o1=$((10#${BASH_REMATCH[1]})) o2=$((10#${BASH_REMATCH[2]})) o3=$((10#${BASH_REMATCH[3]})) o4=$((10#${BASH_REMATCH[4]}))
    if (( o1 <= 255 && o2 <= 255 && o3 <= 255 && o4 <= 255 )); then
      case "$f2b_client_bits" in
        32) entry="$o1.$o2.$o3.$o4" ;;
        24) entry="$o1.$o2.$o3.0/24" ;;
        16) entry="$o1.$o2.0.0/16" ;;
      esac
    fi
  elif [[ "$client" == *:* && "$client" =~ ^[0-9A-Fa-f:]{2,39}$ ]]; then
    entry=$client
  fi
  if [ "$f2b_client_bits" != 0 ]; then
    # Never start the service without the caller on the allowlist.
    [ -n "$entry" ] || return 1
    f2b_allow_add "$entry"
  fi

  # The ports sshd really listens on; "port = ssh" would only mean 22.
  local ports='' unit sshd_bin port_list
  if command -v ss >/dev/null 2>&1; then
    ports+=$'\n'$(ss -tlnp 2>/dev/null | awk '$1 == "LISTEN" && /"sshd"/ { n = split($4, part, ":"); print part[n] }' || true)
  fi
  for unit in ssh.socket sshd.socket; do
    if systemctl is-active --quiet "$unit" 2>/dev/null; then
      ports+=$'\n'$(systemctl show "$unit" -p Listen 2>/dev/null | grep -oE ':[0-9]+ \(Stream\)' | grep -oE '[0-9]+' || true)
    fi
  done
  sshd_bin=$(command -v sshd 2>/dev/null || true)
  if [ -z "$sshd_bin" ] && [ -x /usr/sbin/sshd ]; then sshd_bin=/usr/sbin/sshd; fi
  if [ -n "$sshd_bin" ]; then
    ports+=$'\n'$("$sshd_bin" -T 2>/dev/null | awk 'tolower($1) == "port" { print $2 }' || true)
  fi
  ports+=$'\n'${session[3]:-}
  port_list=$(printf '%s\n' "$ports" | awk '/^[0-9]+$/ && $1 >= 1 && $1 <= 65535 && !seen[$1 + 0]++ { print $1 + 0 }' | sort -n | paste -sd, - || true)
  [[ "$port_list" =~ ^[0-9]+(,[0-9]+)*$ ]] || port_list=22

  # Downloads are bounded and may be cut short; the installation itself runs
  # from the local cache and is never interrupted.
  f2b_net() {
    local left=$((deadline - SECONDS))
    (( left > 5 )) || return 1
    timeout "$left" apt-get -o DPkg::Lock::Timeout=60 -o Acquire::Retries=2 "$@"
  }
  f2b_install() {
    local package missing=''
    for package in "$@"; do
      f2b_have "$package" || missing+=" $package"
    done
    [ -n "$missing" ] || return 0
    # shellcheck disable=SC2086
    if ! f2b_net install -y -qq --no-install-recommends --download-only $missing; then
      f2b_net update -qq || true
      # shellcheck disable=SC2086
      f2b_net install -y -qq --no-install-recommends --download-only $missing || return 1
    fi
    # shellcheck disable=SC2086
    apt-get -o DPkg::Lock::Timeout=60 install -y -qq --no-install-recommends --no-download $missing
  }
  f2b_write() {
    local target=$1 staged
    staged=$(mktemp "${target%/*}/.msboost-f2b.XXXXXXXX") || return 1
    if cat > "$staged" && chown root:root -- "$staged" && chmod 0644 -- "$staged" && mv -T -- "$staged" "$target"; then
      return 0
    fi
    rm -f -- "$staged"
    return 1
  }
  local action='' backend=auto increment=1
  f2b_render() {
    printf '%s\n' "$marker" \
      '# Defaults for a server without its own fail2ban configuration. Anything in' \
      '# jail.local or jail.d/*.local takes precedence over this file.' \
      '[sshd]' 'enabled = true' 'mode = normal' "backend = $backend" "port = $port_list" "ignoreip = $allow" || return 1
    if [ -n "$action" ]; then printf 'banaction = %s\n' "$action" || return 1; fi
    printf '%s\n' 'bantime = 1d' 'findtime = 15m' 'maxretry = 5' || return 1
    if [ "$increment" = 1 ]; then
      printf '%s\n' 'bantime.increment = true' 'bantime.factor = 1' 'bantime.maxtime = 1w' || return 1
    fi
  }
  f2b_render_purge() {
    printf '%s\n' "$marker" \
      '# Keep ban history for 30 days so repeat offenders are recognised.' \
      '[Definition]' 'dbpurgeage = 2592000'
  }
  f2b_action() {
    if command -v nft >/dev/null 2>&1 && nft list tables >/dev/null 2>&1; then
      if [ -f "$dir/action.d/nftables-multiport.conf" ]; then action=nftables-multiport; return 0; fi
      if [ -f "$dir/action.d/nftables.conf" ]; then action='nftables[type=multiport]'; return 0; fi
    fi
    if command -v iptables >/dev/null 2>&1 && iptables -S INPUT >/dev/null 2>&1; then
      if [ -f "$dir/action.d/iptables-multiport.conf" ]; then action=iptables-multiport; return 0; fi
      if [ -f "$dir/action.d/iptables.conf" ]; then action='iptables[type=multiport]'; return 0; fi
    fi
    return 1
  }
  f2b_wait() {
    local attempt
    for ((attempt = 0; attempt < 20; attempt++)); do
      fail2ban-client status sshd >/dev/null 2>&1 && return 0
      systemctl is-failed --quiet fail2ban 2>/dev/null && return 1
      sleep 1
    done
    return 1
  }

  # What to return to if anything below fails.
  local was_active=0 was_enabled=0 had_jail=0 had_purge=0 created=''
  if systemctl is-active --quiet fail2ban 2>/dev/null; then was_active=1; fi
  if systemctl is-enabled --quiet fail2ban 2>/dev/null; then was_enabled=1; fi
  if [ -f "$jail" ]; then cp -p -- "$jail" "$work/f2b-jail.prev" || return 1; had_jail=1; fi
  if [ -f "$purge" ]; then cp -p -- "$purge" "$work/f2b-purge.prev" || return 1; had_purge=1; fi
  # Returns 1 after putting everything back, 20 when that did not work either.
  f2b_undo() {
    local bad=0 made
    if [ "$had_jail" = 1 ]; then f2b_write "$jail" < "$work/f2b-jail.prev" || bad=1; else rm -f -- "$jail" || bad=1; fi
    if [ "$had_purge" = 1 ]; then f2b_write "$purge" < "$work/f2b-purge.prev" || bad=1; else rm -f -- "$purge" || bad=1; fi
    if [ "$installed" != 1 ]; then
      # Installed by this run: better stopped than running on the
      # distribution's defaults without the allowlist.
      if command -v fail2ban-client >/dev/null 2>&1; then
        systemctl disable --now fail2ban >/dev/null 2>&1 || bad=1
      else
        for made in $created; do rmdir -- "$made" 2>/dev/null || true; done
      fi
    elif [ "$was_active" = 1 ]; then
      { systemctl restart fail2ban >/dev/null 2>&1 && f2b_wait; } || bad=1
    else
      systemctl stop fail2ban >/dev/null 2>&1 || true
      if [ "$was_enabled" != 1 ]; then systemctl disable fail2ban >/dev/null 2>&1 || true; fi
    fi
    [ "$bad" = 0 ] && return 1
    return 20
  }

  local mode
  for path in "$dir" "$dir/jail.d" "$dir/fail2ban.d"; do
    if [ -e "$path" ] || [ -L "$path" ]; then
      [ -d "$path" ] && [ ! -L "$path" ] && [ "$(stat -c %u -- "$path")" = 0 ] || return 1
      mode=$(stat -c %a -- "$path") || return 1
      (( (8#$mode & 022) == 0 )) || return 1
    else
      mkdir -- "$path" || { f2b_undo; return $?; }
      created="$path $created"
      chmod 0755 -- "$path" && chown root:root -- "$path" || { f2b_undo; return $?; }
    fi
  done

  # Only the nft command is needed; nftables.service is deliberately left
  # alone, as its ruleset would flush other firewalls' rules.
  f2b_install python3-systemd nftables || true
  if python3 -c 'import systemd.journal' >/dev/null 2>&1; then backend=systemd; fi

  if [ "$installed" != 1 ]; then
    { f2b_render | f2b_write "$jail"; } || { f2b_undo; return $?; }
    { f2b_render_purge | f2b_write "$purge"; } || { f2b_undo; return $?; }
    f2b_install fail2ban || { f2b_undo; return $?; }
    command -v fail2ban-client >/dev/null 2>&1 || { f2b_undo; return $?; }
  fi

  f2b_action || { f2b_undo; return $?; }
  local version major minor
  version=$(fail2ban-client --version 2>/dev/null | grep -oE '[0-9]+\.[0-9]+' | head -n 1 || true)
  major=${version%%.*}
  minor=${version#*.}
  if [[ "$major" =~ ^[0-9]+$ && "$minor" =~ ^[0-9]+$ ]] && (( major == 0 && minor < 11 )); then increment=0; fi

  local restart=1
  f2b_render > "$work/f2b-jail.new" || { f2b_undo; return $?; }
  if [ -f "$jail" ] && cmp -s -- "$work/f2b-jail.new" "$jail" && fail2ban-client status sshd >/dev/null 2>&1; then
    restart=0
  else
    f2b_write "$jail" < "$work/f2b-jail.new" || { f2b_undo; return $?; }
  fi
  if [ "$had_purge" != 1 ] || ! f2b_render_purge | cmp -s -- - "$purge"; then
    { f2b_render_purge | f2b_write "$purge"; } || { f2b_undo; return $?; }
    restart=1
  fi
  fail2ban-client -t >/dev/null 2>&1 || { f2b_undo; return $?; }
  systemctl enable fail2ban >/dev/null 2>&1 || { f2b_undo; return $?; }
  if [ "$restart" = 1 ]; then
    systemctl restart fail2ban >/dev/null 2>&1 || { f2b_undo; return $?; }
  fi
  f2b_wait || { f2b_undo; return $?; }
  return 0
}
if (msboost_fail2ban) < /dev/null >> "$work/fail2ban.log" 2>&1; then
  f2b_status=enabled
else
  case $? in
    10) f2b_status=existing ;;
    20) f2b_status=review_required ;;
    *) f2b_status=unavailable ;;
  esac
fi
printf 'MSBOOST_FAIL2BAN=%s\n' "$f2b_status"
`
