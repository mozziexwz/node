#!/usr/bin/env bash
# Sourced by the verified manager. Never source a configuration/backup as code.
# MSBOOST_DISASTER_V1

disaster_tool() {
  if [[ -n ${DISASTER_TOOL:-} ]]; then return; fi
  local tool_dir arch asset expected actual tool_version=$VERSION image container entry
  # Installed management uses the matching immutable release, not latest.
  if [[ -f $INSTALL_ROOT/.env ]]; then tool_version=$(env_get "$INSTALL_ROOT/.env" MSBOOST_VERSION); fi
  [[ $tool_version =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || { die '灾难恢复工具版本无效'; return 1; }
  case "$(uname -m)" in x86_64) arch=amd64 ;; aarch64) arch=arm64 ;; *) return 1 ;; esac
  tool_dir=$(mktemp -d /tmp/msboost-disaster-tool.XXXXXXXX) || return
  DISASTER_TOOL_DIR=$tool_dir
  if [[ -f $INSTALL_ROOT/.env ]]; then
    image=$(env_get "$INSTALL_ROOT/.env" MSBOOST_IMAGE)
    [[ $(server_identity "$image") == "$(env_get "$INSTALL_ROOT/.env" MSBOOST_IMAGE_ID)" ]] || { die '本机恢复工具所属镜像身份不符'; return 1; }
    # Copy from the already verified image without starting the container. A
    # backup must remain usable while GitHub/GHCR is unavailable.
    container=$(docker create --network none --entrypoint /bin/true "$image") || return
    [[ $container =~ ^[a-f0-9]{64}$ ]] || { die '工具暂存容器 ID 无效'; return 1; }
    DISASTER_TOOL_CONTAINER=$container
    docker cp "$container:/usr/local/bin/msboost-restore" "$tool_dir/msboost-restore" || return
    docker rm "$container" >/dev/null || return
    DISASTER_TOOL_CONTAINER=
    [[ -f $tool_dir/msboost-restore && ! -L $tool_dir/msboost-restore ]] || return 1
    chmod 700 "$tool_dir/msboost-restore" || return
    DISASTER_TOOL="$tool_dir/msboost-restore"
    return
  fi
  asset="msboost-restore-linux-$arch"
  for entry in "$asset" SHA256SUMS; do
    curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 --connect-timeout 15 --max-time 600 --retry 2 \
      "https://github.com/mozziexwz/node/releases/download/$tool_version/$entry" -o "$tool_dir/$entry" || return
  done
  expected=$(awk -v a="$asset" '$2==a {print $1}' "$tool_dir/SHA256SUMS")
  [[ $expected =~ ^[a-f0-9]{64}$ ]] || { die '恢复工具校验清单无效'; return 1; }
  actual=$(sha256sum "$tool_dir/$asset"); actual=${actual%% *}
  [[ $expected == "$actual" ]] || { die '恢复工具 SHA256 不符'; return 1; }
  chmod 700 "$tool_dir/$asset" || return
  DISASTER_TOOL="$tool_dir/$asset"
}
disaster_cleanup() {
  local code=$?
  trap - EXIT
  if [[ ${DISASTER_TOOL_CONTAINER:-} =~ ^[a-f0-9]{64}$ ]]; then docker rm "$DISASTER_TOOL_CONTAINER" >/dev/null || code=1; fi
  # A scheduled snapshot may pause only these two known containers. Resume the
  # exact services that were running, even if export/pack/disk/upload failed.
  if [[ ${DISASTER_RESUME:-0} == 1 && ${#DISASTER_RUNNING[@]} -gt 0 ]]; then
    if ! compose_live start --wait --wait-timeout 180 "${DISASTER_RUNNING[@]}"; then
      note '备份后服务重新启动失败，请立即检查 msboost status/logs。'; code=1
    fi
  fi
  if [[ -n ${DISASTER_WORK:-} && $DISASTER_WORK == /root/msboost-disaster-work.* && ! -L $DISASTER_WORK ]]; then
    # On failure keep sensitive staging for diagnosis. It is always root-only.
    if [[ $code == 0 ]]; then rm -rf -- "$DISASTER_WORK"; else note "保留私有工作目录：$DISASTER_WORK"; fi
  fi
  if [[ -n ${DISASTER_TOOL_DIR:-} && $DISASTER_TOOL_DIR == /tmp/msboost-disaster-tool.* && ! -L $DISASTER_TOOL_DIR ]]; then rm -rf -- "$DISASTER_TOOL_DIR"; fi
  cleanup_stage
  exit "$code"
}
disaster_settings() {
  "$DISASTER_TOOL" disaster config-get --file "$INSTALL_ROOT/disaster.json" --field "$1"
}
disaster_assert_volume() {
  local name=$1
  case "$name" in msboost_app_data|msboost_caddy_data|msboost_caddy_config|msboost_database_data) ;; *) return 1 ;; esac
  [[ $(docker volume inspect --format '{{index .Labels "com.docker.compose.project"}}' "$name") == "$PROJECT" ]] || { die "卷 $name 不属于本站"; return 1; }
}
disaster_backup() {
  assert_managed || return
  disaster_validate_environment "$INSTALL_ROOT/.env" || return
  disaster_tool || return
  local directory filename database_name volume image running
  directory=$(disaster_settings localDir) || return
  filename="msboost-disaster-$(date -u +%Y%m%dT%H%M%SZ)-$(random_hex 8).tar.gz"
  "$DISASTER_TOOL" disaster prepare-dir --dir "$directory" || return
  database_name=$(env_get "$INSTALL_ROOT/.env" MSBOOST_DATABASE_NAME); database_name=${database_name:-msboost}
  [[ $database_name =~ ^[a-zA-Z_][a-zA-Z0-9_]{0,62}$ ]] || { die '数据库名称无效'; return 1; }
  image=$(env_get "$INSTALL_ROOT/.env" POSTGRES_IMAGE)
  [[ $image == *@sha256:* ]] || { die '数据库镜像尚未固定摘要，请先修复部署'; return 1; }
  for volume in app_data database_data caddy_data caddy_config; do disaster_assert_volume "msboost_$volume" || return; done
  DISASTER_WORK=$(mktemp -d /root/msboost-disaster-work.XXXXXXXX) || return
  install -m 600 "$INSTALL_ROOT/.env" "$DISASTER_WORK/site.env" || return
  local -a configuration=(deploy install.sh .managed-by-msboost)
  [[ ! -f $INSTALL_ROOT/disaster.json ]] || configuration+=(disaster.json)
  tar -cf "$DISASTER_WORK/deployment.tar" -C "$INSTALL_ROOT" "${configuration[@]}" || return
  DISASTER_RUNNING=()
  running=$(compose_live ps --status running --services) || return
  for volume in caddy server; do if grep -qx "$volume" <<< "$running"; then DISASTER_RUNNING+=("$volume"); fi; done
  grep -qx database <<< "$running" || { die '数据库未运行；先 repair 后备份'; return 1; }
  note '整站快照会短暂停止控制面网页和后台写入，客户独立 VPS 服务不在操作范围内。'
  if [[ ${#DISASTER_RUNNING[@]} -gt 0 ]]; then
    DISASTER_RESUME=1
    compose_live stop --timeout 60 "${DISASTER_RUNNING[@]}" || return
  fi
  compose_live exec -T database pg_dump --username=msboost --dbname="$database_name" --format=custom > "$DISASTER_WORK/database.dump" || return
  [[ -s $DISASTER_WORK/database.dump ]] || { die '数据库导出为空'; return 1; }
  # The helper is read-only and does not start the application or any worker.
  local -a key_args=()
  [[ -n $(env_get "$INSTALL_ROOT/.env" MASTER_KEY) ]] || key_args=(--master-key-file /app/data/master.key)
  compose_live run --rm --no-deps -T --user 0:0 --entrypoint /usr/local/bin/msboost-restore server export "${key_args[@]}" > "$DISASTER_WORK/state.msb" || return
  for volume in app_data caddy_data caddy_config; do
    docker run --rm --network none --read-only --user 0:0 --entrypoint tar \
      --mount "type=volume,source=msboost_$volume,target=/snapshot,readonly" "$image" -cf - -C /snapshot . > "$DISASTER_WORK/$volume.tar" || return
    "$DISASTER_TOOL" disaster validate-volume --archive "$DISASTER_WORK/$volume.tar" || return
  done
  if [[ ${#DISASTER_RUNNING[@]} -gt 0 ]]; then compose_live start --wait --wait-timeout 180 "${DISASTER_RUNNING[@]}" || return; fi
  DISASTER_RESUME=0
  "$DISASTER_TOOL" disaster pack --dir "$DISASTER_WORK" --output "$directory/$filename" || return
  note "整站快照已完成并校验：$directory/$filename（含主密钥与密码，勿公开上传）。"
  # Remote failure preserves the verified local bundle and does not prune.
  "$DISASTER_TOOL" disaster upload --config "$INSTALL_ROOT/disaster.json" --archive "$directory/$filename" || return
  "$DISASTER_TOOL" disaster retain --config "$INSTALL_ROOT/disaster.json" || return
}
disaster_configure() {
  assert_managed || return
  disaster_tool || return
  local directory days when host port user remote fingerprint password enabled prune
  directory=$(read_tty '本地整站备份目录 [/root/msboost-backup]: '); directory=${directory:-/root/msboost-backup}
  days=$(read_tty '保留天数 [30]，至少保留两份有效备份: '); days=${days:-30}
  when=$(read_tty '每天备份时间 [02:30]（此 VPS 本地时区）: '); when=${when:-02:30}
  host=$(read_tty '远程备份服务器公网 IP（留空仅本机）: ')
  local -a remote_args=()
  password=
  if [[ -n $host ]]; then
    port=$(read_tty '远程 SSH 端口 [22]: '); port=${port:-22}
    user=$(read_tty '远程 SSH 用户 [root]: '); user=${user:-root}
    remote=$(read_tty '远程目录 [/root/msboost-backup]: '); remote=${remote:-/root/msboost-backup}
    note '请在远程服务器可信控制台运行 ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub，核对 SHA256 指纹。'
    fingerprint=$(read_tty '远程服务器 SHA256 指纹（必须独立核对）: ')
    read -r -s -p '远程 SSH 密码（不回显）: ' password </dev/tty || return
    printf '\n' >&2
    remote_args=(--remote-host "$host" --remote-port "$port" --remote-user "$user" --remote-dir "$remote" --fingerprint "$fingerprint")
    note '可选远程保留期清理：每次上传成功后完整回读校验远端候选包，至少留两份；会占用下载流量和本机临时磁盘。默认不删除远端文件。'
    prune=$(read_tty '按相同保留天数自动删除远端超期整站备份？明确输入 YES 启用: ')
    [[ $prune != YES ]] || remote_args+=(--prune-remote)
  fi
  enabled=$(read_tty '启用每日自动备份？输入 YES 启用，其余仅保存设置: ')
  printf '%s' "$password" | "$DISASTER_TOOL" disaster config-save --file "$INSTALL_ROOT/disaster.json" --local-dir "$directory" --retention-days "$days" --time "$when" "${remote_args[@]}" || return
  unset password
  disaster_timer off || return
  if [[ $enabled == YES ]]; then disaster_timer on "$when" || return; fi
  note '配置已保存为 root-only 文件；远程密码不回显、不进入命令行。自动备份仅按明确启用的计划运行。'
}
disaster_timer() {
  local mode=$1 when=${2:-} file
  for file in /etc/systemd/system/msboost-disaster-backup.service /etc/systemd/system/msboost-disaster-backup.timer; do
    [[ ! -L $file ]] || { die '备份计划文件不能是符号链接'; return 1; }
    if [[ -e $file ]]; then grep -qx '# MSBOOST_DISASTER_V1' "$file" || { die "计划文件 $file 非本站所有"; return 1; }; fi
  done
  if [[ -f /etc/systemd/system/msboost-disaster-backup.timer ]]; then
    systemctl disable --now msboost-disaster-backup.timer || return
  fi
  if [[ $mode == remove ]]; then
    for file in /etc/systemd/system/msboost-disaster-backup.service /etc/systemd/system/msboost-disaster-backup.timer; do
      if [[ -f $file ]]; then rm -- "$file" || return; fi
    done
    systemctl daemon-reload || return
  fi
  if [[ $mode == on ]]; then
    [[ $when =~ ^([01][0-9]|2[0-3]):[0-5][0-9]$ ]] || return 1
    printf '%s\n' '# MSBOOST_DISASTER_V1' '[Unit]' 'Description=MSBOOST whole-site disaster backup' 'After=docker.service' '[Service]' 'Type=oneshot' 'UMask=0077' 'ExecStart=/usr/local/bin/msboost disaster-backup' 'TimeoutStartSec=infinity' > /etc/systemd/system/msboost-disaster-backup.service
    printf '%s\n' '# MSBOOST_DISASTER_V1' '[Unit]' 'Description=MSBOOST daily disaster backup schedule' '[Timer]' "OnCalendar=*-*-* $when:00" 'Persistent=true' 'RandomizedDelaySec=60' '[Install]' 'WantedBy=timers.target' > /etc/systemd/system/msboost-disaster-backup.timer
    chmod 644 /etc/systemd/system/msboost-disaster-backup.{service,timer}
    systemctl daemon-reload || return
    systemctl enable --now msboost-disaster-backup.timer || return
  fi
}
disaster_validate_environment() {
  local file=$1 key value host site public secure
  # Generated .env is data, not shell code. Reject duplicates and interpolation
  # so our first-value reader and Compose cannot disagree about identity/keys.
  local -A seen=()
  while IFS= read -r value || [[ -n $value ]]; do
    [[ -z $value || $value == \#* ]] && continue
    [[ $value =~ ^([A-Z_]+)=(.*)$ ]] || { die '备份 .env 格式不符合一键部署规范'; return 1; }
    key=${BASH_REMATCH[1]}; value=${BASH_REMATCH[2]}
    [[ -z ${seen[$key]:-} && $value != *'$'* && $value != *'`'* && $value != *'"'* && $value != *"'"* && $value != *$'\r'* ]] || { die '备份 .env 存在重复键或不允许的插值/引号'; return 1; }
    seen[$key]=1
    case "$key" in
      MSBOOST_DOMAIN|MSBOOST_SITE_ADDRESS|PUBLIC_URL|COOKIE_SECURE|MSBOOST_VERSION|MSBOOST_IMAGE|MSBOOST_IMAGE_ID|MSBOOST_DATABASE_NAME|POSTGRES_IMAGE|CADDY_IMAGE|ADMIN_EMAIL|ADMIN_PASSWORD|POSTGRES_PASSWORD|MASTER_KEY) ;;
      *) die '备份包含自定义环境字段，需人工审核恢复，未执行归档代码'; return 1 ;;
    esac
  done < "$file"
  [[ $(env_get "$file" MSBOOST_IMAGE_ID) =~ ^sha256:[a-f0-9]{64}$ && $(env_get "$file" POSTGRES_IMAGE) =~ ^postgres@sha256:[a-f0-9]{64}$ && $(env_get "$file" CADDY_IMAGE) =~ ^caddy@sha256:[a-f0-9]{64}$ ]] || { die '备份镜像固定摘要无效'; return 1; }
  [[ $(env_get "$file" MSBOOST_IMAGE) =~ ^ghcr.io/mozziexwz/node@sha256:[a-f0-9]{64}$ || $(env_get "$file" MSBOOST_IMAGE) =~ ^msboost-release:v[0-9]+\.[0-9]+\.[0-9]+-(amd64|arm64)-[a-f0-9]{12}$ ]] || { die '备份镜像不是官方固定摘要或已校验 Release 归档'; return 1; }
  [[ $(env_get "$file" ADMIN_EMAIL) =~ ^[1-9][0-9]{4,14}@qq\.com$ && -n $(env_get "$file" ADMIN_PASSWORD) && -n $(env_get "$file" POSTGRES_PASSWORD) ]] || { die '备份缺少原管理员或数据库凭据'; return 1; }
  value=$(env_get "$file" MASTER_KEY)
  [[ -z $value || $value =~ ^[a-fA-F0-9]{64}$ ]] || { die '原主密钥格式无效'; return 1; }
  host=$(env_get "$file" MSBOOST_DOMAIN); site=$(env_get "$file" MSBOOST_SITE_ADDRESS); public=$(env_get "$file" PUBLIC_URL); secure=$(env_get "$file" COOKIE_SECURE)
  if [[ $secure == true ]]; then
    valid_domain "$host" && [[ $site == "$host" && $public == "https://$host" ]] || { die '备份 HTTPS 域名配置不一致'; return 1; }
  elif [[ $secure == false ]]; then
    valid_ipv4 "$host" && [[ $site == "http://$host" && $public == "http://$host" ]] || { die '备份 HTTP 地址配置不一致'; return 1; }
  else die '备份 Cookie 安全模式无效'; return 1; fi
}
disaster_restore() {
  # The full-site path is intentionally clean-host-only. Existing installations
  # can still use the independent new-database recovery documented previously.
  [[ ! -e $INSTALL_ROOT && ! -L $INSTALL_ROOT ]] || { die '整站恢复仅用于全新目标：/opt/msboost 已存在，拒绝覆盖。已有站点请按文档恢复到新数据库。'; return 1; }
  validate_source || return
  disaster_tool || return
  local archive=$DISASTER_ARCHIVE confirm volume image recovered_db
  if [[ -z $archive ]]; then archive=$(read_tty '输入本机整站备份 .tar.gz 的绝对路径: '); fi
  [[ $archive == /* && -f $archive && ! -L $archive ]] || { die '需要本机普通备份文件的绝对路径'; return 1; }
  "$DISASTER_TOOL" disaster verify --archive "$archive" || return
  note '完整恢复将使用备份时的数据、原域名、密码、主密钥和证书；备份之后的付款必须人工对账。请先停止原站点，避免双站运行。'
  confirm=$(read_tty '确认原站点已停机，且此 VPS 是全新目标，输入 RESTORE_NEW_MSBOOST: ')
  [[ $confirm == RESTORE_NEW_MSBOOST ]] || { die '已取消'; return 1; }
  ensure_docker || return
  assert_no_collision || return
  DISASTER_WORK=$(mktemp -d /root/msboost-disaster-work.XXXXXXXX) || return
  "$DISASTER_TOOL" disaster unpack --archive "$archive" --dir "$DISASTER_WORK/bundle" || return
  local recovered="$DISASTER_WORK/bundle"
  # Only the verified current release's scripts/Compose are installed. Archived
  # scripts are retained for reference but never executed or used as Compose.
  for volume in app_data caddy_data caddy_config; do "$DISASTER_TOOL" disaster validate-volume --archive "$recovered/$volume.tar" || return; done
  image=$(env_get "$recovered/site.env" MSBOOST_IMAGE)
  disaster_validate_environment "$recovered/site.env" || return
  [[ $(env_get "$recovered/site.env" MSBOOST_VERSION) == "$VERSION" ]] || { die "需使用备份同版本的安装入口恢复；当前入口 $VERSION 与备份不符，不进行隐式升级。"; return 1; }
  STAGE=$(mktemp -d "$DISASTER_WORK/image-stage.XXXXXXXX") || return
  install -m 600 "$recovered/site.env" "$STAGE/.env" || return
  if [[ $image == ghcr.io/mozziexwz/node@sha256:* ]]; then
    if ! docker pull "$image"; then load_release_image "$(env_get "$recovered/site.env" MSBOOST_IMAGE_ID)" || return; fi
  elif [[ $image == msboost-release:v* ]]; then load_release_image "$(env_get "$recovered/site.env" MSBOOST_IMAGE_ID)" || return
  else die '本地自构建镜像未包含在整站备份内，请按离线文档先导入原镜像再人工恢复。'; return 1; fi
  image=$(env_get "$STAGE/.env" MSBOOST_IMAGE)
  [[ $(server_identity "$image") == "$(env_get "$recovered/site.env" MSBOOST_IMAGE_ID)" ]] || { die '备份应用镜像身份不一致'; return 1; }
  install -d -m 700 "$INSTALL_ROOT" || return
  printf '%s\n' 'MSBOOST_DISASTER_V1' "work=$DISASTER_WORK" > "$INSTALL_ROOT/.disaster-incomplete"
  printf '%s\n' "$MARKER" > "$INSTALL_ROOT/.managed-by-msboost"
  install -m 600 "$STAGE/.env" "$INSTALL_ROOT/.env" || return
  copy_deployment_files "$SOURCE_DIR" "$INSTALL_ROOT" || return
  # Bind all services to the selected new DB before the first app startup.
  recovered_db="msboost_restore_$(date -u +%Y%m%d%H%M%S)_$(random_hex 4)"
  env_set "$INSTALL_ROOT/.env" MSBOOST_DATABASE_NAME "$recovered_db" || return
  compose_live config --quiet || return
  compose_live pull database caddy || return
  # Do not even create app/proxy containers before import: a daemon restart
  # must not accidentally launch a still-empty site's bootstrap code.
  for volume in app_data database_data caddy_data caddy_config; do
    docker volume create --label "com.docker.compose.project=$PROJECT" --label "com.docker.compose.volume=$volume" "msboost_$volume" >/dev/null || return
  done
  image=$(env_get "$INSTALL_ROOT/.env" POSTGRES_IMAGE)
  [[ $image == *@sha256:* ]] || return 1
  for volume in app_data caddy_data caddy_config; do
    disaster_assert_volume "msboost_$volume" || return
    docker run --rm -i --network none --read-only --user 0:0 --entrypoint tar \
      --mount "type=volume,source=msboost_$volume,target=/restore" "$image" -xf - -C /restore --numeric-owner < "$recovered/$volume.tar" || return
  done
  compose_live up -d --no-deps --no-build --pull never --wait --wait-timeout 180 database || return
  local -a key_args=()
  [[ -n $(env_get "$INSTALL_ROOT/.env" MASTER_KEY) ]] || key_args=(--master-key-file /app/data/master.key)
  compose_live run --rm --no-deps -T --user 0:0 --volume "$recovered:/recovery:ro" --entrypoint /usr/local/bin/msboost-restore server \
    --backup /recovery/state.msb --postgres-new-database "$recovered_db" --confirm-disaster-restore "${key_args[@]}" || return
  [[ -f $INSTALL_ROOT/.disaster-incomplete && ! -L $INSTALL_ROOT/.disaster-incomplete ]] || return 1
  rm -- "$INSTALL_ROOT/.disaster-incomplete" || return
  # Keep the raw dump/configuration on this host; never automatically replay it.
  install -d -m 700 "$INSTALL_ROOT/backups" || return
  mv -- "$recovered" "$INSTALL_ROOT/backups/disaster-source-$(date -u +%Y%m%dT%H%M%SZ)" || return
  install_launcher || return
  start_live || { die '恢复数据已保留，但站点健康检查失败。检查 DNS/端口后 repair；不要重新安装或 purge。'; return 1; }
  note '整站恢复完成，维护模式开启、支付关闭、会话与 Agent 凭据已失效。请对账、重新关联 Agent 并验收后再开放营业。自动备份计划须重新配置。'
}
