#!/usr/bin/env bash
# Host Caddy lifecycle. Only msboost.caddy and msboost-custom belong to this app.
# Never source the application .env in this service; it contains private keys.
CADDY_ROOT=/etc/caddy
CADDY_SITE=/etc/caddy/sites-enabled/msboost.caddy
CADDY_CUSTOM=/etc/caddy/msboost-custom
CADDY_OWNER=MSBOOST_SYSTEM_CADDY_V1

caddy_safe_path() {
  local path=$1
  [[ $path == /etc/caddy || $path == /etc/caddy/* ]] || return 1
  while [[ $path != /etc ]]; do
    [[ ! -L $path ]] || { die "Caddy 路径不能是符号链接：$path"; return 1; }
    if [[ -e $path ]]; then
      [[ $(stat -c %u "$path") == 0 ]] || { die "Caddy 配置必须属于 root：$path"; return 1; }
      [[ -z $(find "$path" -maxdepth 0 -perm /022 -print) ]] || { die "Caddy 配置不能由非 root 修改：$path"; return 1; }
    fi
    path=${path%/*}
  done
}

caddy_assert_service() {
  local command reload
  command=$(systemctl show caddy.service --property=ExecStart --value) || return
  reload=$(systemctl show caddy.service --property=ExecReload --value) || return
  [[ $command == *'/usr/bin/caddy run'* && $command == *'--config /etc/caddy/Caddyfile'* && $command != *'--resume'* &&
     $reload == *'/usr/bin/caddy reload'* && $reload == *'--config /etc/caddy/Caddyfile'* &&
     $(systemctl show caddy.service --property=User --value) == caddy ]] || {
    die '已有 Caddy 不是使用 /etc/caddy/Caddyfile 的标准 caddy.service；请人工确认服务，安装器不会接管自定义服务或 API 配置。'; return 1;
  }
  ! systemctl is-active --quiet caddy-api.service || { die 'caddy-api.service 正在运行，不自动接管'; return 1; }
}

caddy_validate() {
  # Match the official service account, including certificate storage ownership.
  runuser -u caddy -- /usr/bin/caddy validate --config "$CADDY_ROOT/Caddyfile" --adapter caddyfile
}

# The reviewed official .deb includes the standard service, user and Caddyfile.
# A pinned release avoids depending on Cloudsmith's mutable repository signing
# metadata. Never reinstall an existing package or change its shared service.
caddy_install_package() (
  local arch hash temp version=2.11.4
  arch=$(dpkg --print-architecture) || return
  case "$arch" in
    amd64) hash=1c6f5404f3622e46d401d81f4af59677d46b886229c6694d60fd936b87c72d3bb5d1fcf42b55c8d555769fa75acf434ab618fc7e0df2c79cf8512ee580d38d06 ;;
    arm64) hash=c43c62b7b583b31c682b3c3e1a31cf03759fbab01dcb0fc7d7fc3a5ce1bef43403583e26133920634a730a9fe31dae1386af4d3f9f3fc19fcc2c29ebf19de235 ;;
    *) die 'Caddy 官方固定软件包仅支持 amd64/arm64'; return 1 ;;
  esac
  apt-get update || return
  DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends ca-certificates curl init-system-helpers passwd || return
  temp=$(mktemp -d /tmp/msboost-caddy-package.XXXXXXXX) || return
  trap 'rm -f -- "$temp/caddy.deb"; rmdir -- "$temp"' EXIT
  curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --tlsv1.2 \
    --retry 3 --connect-timeout 15 --max-time 180 \
    "https://github.com/caddyserver/caddy/releases/download/v$version/caddy_${version}_linux_${arch}.deb" -o "$temp/caddy.deb" || return
  printf '%s  %s\n' "$hash" "$temp/caddy.deb" | sha512sum --check --strict || return
  [[ $(dpkg-deb --field "$temp/caddy.deb" Package) == caddy &&
     $(dpkg-deb --field "$temp/caddy.deb" Version) == "$version" &&
     $(dpkg-deb --field "$temp/caddy.deb" Architecture) == "$arch" ]] || { die 'Caddy 软件包身份或架构不符'; return 1; }
  # The artifact is public. Let APT's unprivileged sandbox read it without
  # granting write access to any part of the root-owned temporary directory.
  chmod 0755 "$temp" && chmod 0644 "$temp/caddy.deb" || return
  DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends "$temp/caddy.deb" || return
)

caddy_ensure() {
  local temp path unit suffix package_status fresh=0
  if ! command -v ss >/dev/null; then
    apt-get update || return
    DEBIAN_FRONTEND=noninteractive apt-get install -y iproute2 || return
  fi
  if ! command -v caddy >/dev/null; then
    # Debian postinst starts the service. Reject orphan units and drop-ins
    # before package installation, so their hooks cannot run before validation.
    for path in /etc/systemd/system.control /run/systemd/system.control /run/systemd/transient /run/systemd/generator.early \
      /etc/systemd/system /etc/systemd/system.attached /run/systemd/system /run/systemd/system.attached /run/systemd/generator \
      /usr/local/lib/systemd/system /lib/systemd/system /usr/lib/systemd/system /run/systemd/generator.late; do
      for unit in caddy.service caddy-api.service; do
        for suffix in '' .d .wants .requires .upholds; do
          [[ ! -e $path/$unit$suffix && ! -L $path/$unit$suffix ]] || { die "发现已有 Caddy unit 或扩展，未安装或覆盖：$path/$unit$suffix"; return 1; }
        done
      done
    done
    for path in /usr/bin/caddy /etc/caddy /var/lib/caddy /var/log/caddy; do
      [[ ! -e $path && ! -L $path ]] || { die "发现已有 Caddy 资源但命令不可用，未安装或覆盖：$path"; return 1; }
    done
    ! getent passwd caddy >/dev/null && ! getent group caddy >/dev/null || { die '已有 Caddy 用户或组但命令不可用，未接管'; return 1; }
    package_status=$(dpkg-query -W -f='${db:Status-Status}' caddy 2>/dev/null) || {
      [[ $? == 1 && -z $package_status ]] || { die '无法核对已有 Caddy 软件包状态，未安装'; return 1; }
    }
    [[ -z $package_status || $package_status == not-installed ]] || { die '已有 Caddy 软件包但命令不可用，请先人工修复；不会重装或升级共享服务。'; return 1; }
    ! systemctl is-active --quiet caddy.service && ! systemctl is-active --quiet caddy-api.service || { die '已有 Caddy 服务正在运行，未安装或接管'; return 1; }
    [[ -z $(ss -H -ltn '( sport = :80 or sport = :443 )') ]] || { die '80/443 被其他服务占用，请先处理；不会停止该服务'; return 1; }
    note '安装经固定 SHA512 校验的官方 Caddy v2.11.4 软件包和 systemd 服务（应用和数据库继续使用预构建容器）。'
    caddy_install_package || return
    fresh=1
  fi
  caddy_assert_service || return
  if ! systemctl is-active --quiet caddy.service && [[ -n $(ss -H -ltn '( sport = :80 or sport = :443 )') ]]; then
    die '系统 Caddy 未运行，但 80/443 被其他服务占用；未停止或接管该服务。'; return 1
  fi
  caddy_safe_path "$CADDY_ROOT/Caddyfile" || return
  [[ -f $CADDY_ROOT/Caddyfile ]] || { die 'Caddyfile 不存在；未覆盖已有 Caddy 服务'; return 1; }
  caddy_safe_path "$CADDY_ROOT/sites-enabled" || return
  caddy_safe_path "$CADDY_CUSTOM" || return
  install -d -m 0755 "$CADDY_ROOT/sites-enabled" || return
  if [[ ! -e $CADDY_CUSTOM ]]; then
    install -d -o root -g caddy -m 0750 "$CADDY_CUSTOM" || return
    printf '%s\n' "$CADDY_OWNER" > "$CADDY_CUSTOM/.msboost-owner"
  fi
  [[ -f $CADDY_CUSTOM/.msboost-owner && ! -L $CADDY_CUSTOM/.msboost-owner && $(<"$CADDY_CUSTOM/.msboost-owner") == "$CADDY_OWNER" ]] || { die 'msboost-custom 目录不属于本安装器，拒绝接管'; return 1; }
  # Append the shared import once. Never regenerate the main Caddyfile on upgrade.
  if ! grep -Eq '^import (/etc/caddy/)?sites-enabled/\*\.caddy[[:space:]]*$' "$CADDY_ROOT/Caddyfile"; then
    if [[ -f $INSTALL_ROOT/.env && $(env_get "$INSTALL_ROOT/.env" MSBOOST_PROXY_MODE) == systemd ]]; then
      die '共享 Caddyfile 的站点 import 已被移除；请核对后恢复 import /etc/caddy/sites-enabled/*.caddy，不会覆盖共享入口。'; return 1
    fi
    temp=$(mktemp "$CADDY_ROOT/.msboost-import.XXXXXXXX") || return
    cp -p -- "$CADDY_ROOT/Caddyfile" "$temp" || return
    printf '\nimport /etc/caddy/sites-enabled/*.caddy\n' >> "$temp"
    if ! runuser -u caddy -- /usr/bin/caddy validate --config "$temp" --adapter caddyfile; then rm -- "$temp"; return 1; fi
    cp -p -- "$CADDY_ROOT/Caddyfile" "$CADDY_ROOT/Caddyfile.before-msboost-$(date -u +%Y%m%dT%H%M%S)" || return
    mv -f -- "$temp" "$CADDY_ROOT/Caddyfile" || return
  fi
  [[ $fresh == 0 ]] || note 'Caddy 已安装；后续 MSBOOST 升级不会自动升级 Caddy 软件包。'
}

caddy_assert_owned_site() {
  caddy_safe_path "$CADDY_SITE" || return
  [[ ! -e $CADDY_SITE ]] && return 0
  [[ -f $CADDY_SITE ]] && grep -qx "# $CADDY_OWNER" "$CADDY_SITE" || { die 'msboost.caddy 已被其他配置占用'; return 1; }
  if [[ -f $INSTALL_ROOT/proxy/managed.sha256 ]]; then
    [[ $(sha256sum "$CADDY_SITE" | cut -d' ' -f1) == "$(<"$INSTALL_ROOT/proxy/managed.sha256")" ]] || {
      die 'msboost.caddy 有手工修改；请将自定义内容移入 /etc/caddy/msboost-custom/*.caddy 后重试，不会覆盖修改。'; return 1;
    }
  fi
}

caddy_publish() (
  local mode=${1:-enable} candidate previous had=0 changed=1 site
  caddy_assert_service || return
  caddy_assert_owned_site || return
  caddy_safe_path "$CADDY_ROOT/Caddyfile" || return
  install -d -m 0700 "$INSTALL_ROOT/proxy" || return
  candidate=$(mktemp "$CADDY_ROOT/sites-enabled/.msboost-next.XXXXXXXX") || return
  previous=$(mktemp "$INSTALL_ROOT/proxy/.previous.XXXXXXXX") || return
  trap 'rm -f -- "$candidate" "$previous"' EXIT
  if [[ -f $CADDY_SITE ]]; then cp -p -- "$CADDY_SITE" "$previous" || return; had=1; fi
  if [[ $mode == enable ]]; then
    site=$(env_get "$INSTALL_ROOT/.env" MSBOOST_SITE_ADDRESS)
    valid_domain "$site" || { [[ $site == http://* ]] && valid_ipv4 "${site#http://}"; } || { die 'Caddy 站点地址无效'; return 1; }
    sed "s|MSBOOST_SITE_ADDRESS_PLACEHOLDER|$site|g" "$INSTALL_ROOT/deploy/Caddyfile" > "$candidate" || return
    chmod 0644 "$candidate" || return
    if [[ $had == 1 ]] && cmp -s "$candidate" "$CADDY_SITE"; then changed=0
    else mv -f -- "$candidate" "$CADDY_SITE" || return; fi
  elif [[ $mode == disable ]]; then
    if [[ $had == 1 ]]; then rm -- "$CADDY_SITE" || return; else return 0; fi
  else return 2; fi
  # Disk writes are atomic. Caddy never watches files automatically. Validate
  # the entire shared configuration before reloading; restore only OUR file.
  if [[ $changed == 1 ]] || ! systemctl is-active --quiet caddy.service; then
    if ! caddy_validate; then
      if [[ $had == 1 ]]; then cp -p -- "$previous" "$candidate" && mv -f -- "$candidate" "$CADDY_SITE"; else rm -f -- "$CADDY_SITE"; fi
      die 'Caddy 完整配置校验失败，本站配置已恢复，运行中的其他网站未重载。'; return 1
    fi
    if systemctl is-active --quiet caddy.service; then
      if ! systemctl reload caddy.service; then
        if [[ $had == 1 ]]; then cp -p -- "$previous" "$candidate" && mv -f -- "$candidate" "$CADDY_SITE"; else rm -f -- "$CADDY_SITE"; fi
        caddy_validate && systemctl reload caddy.service || true
        die 'Caddy 重载失败，已尝试恢复本站配置；未停止共享 Caddy。'; return 1
      fi
    elif [[ $mode == enable ]]; then systemctl enable --now caddy.service || return; fi
  fi
  if [[ $mode == enable ]]; then
    sha256sum "$CADDY_SITE" | cut -d' ' -f1 > "$INSTALL_ROOT/proxy/managed.sha256"
  fi
)

caddy_remove_custom() {
  caddy_safe_path "$CADDY_CUSTOM" || return
  [[ ! -e $CADDY_CUSTOM ]] && return 0
  [[ -f $CADDY_CUSTOM/.msboost-owner && ! -L $CADDY_CUSTOM/.msboost-owner && $(<"$CADDY_CUSTOM/.msboost-owner") == "$CADDY_OWNER" ]] || return 1
  [[ -z $(find "$CADDY_CUSTOM" -mindepth 1 -maxdepth 1 -type d -print) ]] || { die 'MSBOOST 扩展目录包含子目录，请先核对；不递归清理未知内容。'; return 1; }
  [[ $CADDY_CUSTOM == /etc/caddy/msboost-custom && $(realpath -e "$CADDY_CUSTOM") == /etc/caddy/msboost-custom ]] || return 1
  rm -rf -- /etc/caddy/msboost-custom
}

caddy_export() {
  caddy_assert_owned_site || return
  caddy_safe_path "$CADDY_CUSTOM" || return
  # A flat directory keeps restore scope explicit; no links or arbitrary paths.
  local file
  while IFS= read -r -d '' file; do
    [[ -f $file && ! -L $file && ( ${file##*/} == .msboost-owner || ${file##*/} =~ ^[a-zA-Z0-9_-]+\.caddy$ ) ]] || { die 'MSBOOST 扩展目录仅允许普通 *.caddy 文件'; return 1; }
    caddy_safe_path "$file" || return
  done < <(find "$CADDY_CUSTOM" -mindepth 1 -maxdepth 1 -print0)
  tar -cf "$1" -C "$CADDY_ROOT" msboost-custom || return
}

caddy_import() (
  local file directory=$1 operation=${2:-} journal="$CADDY_ROOT/.msboost-restore-import" name digest candidate= new_journal=0
  local -a published=()
  declare -A published_hash=()
  # Ordinary errors roll back only this invocation's new files. A hard kill
  # leaves the journal, so the same verified archive can resume safely.
  trap '
    status=$?
    if (( status != 0 )); then
      clean=1
      for file in "${published[@]}"; do
        if caddy_safe_path "$file" && [[ -f $file && ! -L $file && $(sha256sum "$file" | cut -d" " -f1) == "${published_hash[$file]}" ]]; then rm -- "$file" || clean=0
        else clean=0; fi
      done
      if [[ $new_journal == 1 && $clean == 1 && -f $journal && ! -L $journal && $(head -n 1 "$journal") == "$operation" ]]; then rm -- "$journal" || true; fi
    fi
    [[ -z $candidate || ! -f $candidate ]] || rm -f -- "$candidate"
    exit "$status"
  ' EXIT
  [[ -d $directory/msboost-custom && ! -L $directory/msboost-custom ]] || return 1
  # Whole archive was validated before extraction. Only explicit owned files
  # are installed; archived main configs, service units and certificates never run.
  while IFS= read -r -d '' file; do
    [[ -f $file && ! -L $file && ( ${file##*/} == .msboost-owner || ${file##*/} =~ ^[a-zA-Z0-9_-]+\.caddy$ ) ]] || return 1
  done < <(find "$directory/msboost-custom" -mindepth 1 -print0)
  [[ $(<"$directory/msboost-custom/.msboost-owner") == "$CADDY_OWNER" ]] || return 1
  caddy_ensure || return
  [[ $operation =~ ^[a-f0-9]{64}$ ]] || { die '扩展恢复需要已核验备份的 SHA-256 操作标识'; return 1; }
  caddy_safe_path "$journal" || return
  if [[ -e $journal ]]; then
    [[ -f $journal && ! -L $journal && $(head -n 1 "$journal") == "$operation" ]] || { die '存在另一次未结束的扩展恢复，未覆盖；请保留其恢复记录核对'; return 1; }
  else
    (umask 077; set -o noclobber; printf '%s\n' "$operation" > "$journal") || return
    new_journal=1
  fi
  # Preflight ALL destinations before writing any of them. A matching file is
  # resumable only when this operation recorded ownership before its publication.
  while IFS= read -r -d '' file; do
    name=${file##*/}; digest=$(sha256sum "$file" | cut -d' ' -f1)
    caddy_safe_path "$CADDY_CUSTOM/$name" || return
    if [[ -e $CADDY_CUSTOM/$name || -L $CADDY_CUSTOM/$name ]]; then
      grep -Fxq "$digest $name" "$journal" && [[ -f $CADDY_CUSTOM/$name && ! -L $CADDY_CUSTOM/$name ]] && cmp -s "$file" "$CADDY_CUSTOM/$name" || { die '恢复目标已有其他或已修改的 MSBOOST 扩展，不覆盖'; return 1; }
    fi
  done < <(find "$directory/msboost-custom" -maxdepth 1 -type f -name '*.caddy' -print0)
  while IFS= read -r -d '' file; do
    name=${file##*/}; digest=$(sha256sum "$file" | cut -d' ' -f1)
    [[ ! -f $CADDY_CUSTOM/$name ]] || continue
    grep -Fxq "$digest $name" "$journal" || printf '%s %s\n' "$digest" "$name" >> "$journal" || return
    candidate=$(mktemp "$CADDY_ROOT/.msboost-restore-next.XXXXXXXX") || return
    if ! install -o root -g caddy -m 0640 "$file" "$candidate"; then rm -f -- "$candidate"; return 1; fi
    # No overwriting: a concurrent administrator's file is never ours.
    if ! ln -- "$candidate" "$CADDY_CUSTOM/$name"; then rm -f -- "$candidate"; return 1; fi
    published+=("$CADDY_CUSTOM/$name"); published_hash[$CADDY_CUSTOM/$name]=$digest
    rm -- "$candidate" || return
  done < <(find "$directory/msboost-custom" -maxdepth 1 -type f -name '*.caddy' -print0)
)

caddy_import_commit() {
  local operation=$1 journal="$CADDY_ROOT/.msboost-restore-import"
  caddy_safe_path "$journal" || return
  [[ -f $journal && ! -L $journal && $(head -n 1 "$journal") == "$operation" ]] || return 1
  rm -- "$journal"
}
