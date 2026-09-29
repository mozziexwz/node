#!/usr/bin/env bash
# Historical stop-gate regression fixture. Never sourced by production entrypoints.
disaster_backup_maintenance() {
  set +xv
  [[ $# == 0 ]] || { die '手动维护备份不接受参数或自动确认选项'; return 2; }
  [[ $(id -u) == 0 && $(uname -s) == Linux ]] || { die '手动维护备份仅允许目标 Linux 服务器 root 执行'; return 1; }
  [[ -z ${INVOCATION_ID:-} && -z ${SYSTEMD_EXEC_PID:-} && -z ${JOURNAL_STREAM:-} ]] || { die '手动维护备份禁止从 systemd 自动任务调用；定时备份仍使用严格门禁'; return 1; }
  local maintenance_tty_fd maintenance_confirmation='' result=0
  export -n maintenance_confirmation
  disaster_maintenance_open_tty || return
  note '风险：本次备份将短暂停止原有控制面服务，v1 / 混合链可能在短租约到期后断开，既有游戏连接可能中断，需要自行重连。'
  note '这里只允许你明确接受链路离线能力不足的中断风险；执行任务、网页备份活动、恢复冲突、坏记录和所有并发门禁仍不可绕过。'
  note '这是单次手动操作，不会放宽自动计划、迁移节点或停止客户 Agent / GOST。'
  if ! disaster_maintenance_read || [[ $maintenance_confirmation != BACKUP_WITH_RELAY_INTERRUPTION ]]; then
    exec {maintenance_tty_fd}>&-
    note '已取消手动维护备份，未执行停站。'
    return 1
  fi
  if ! disaster_backup maintenance; then result=1; fi
  exec {maintenance_tty_fd}>&-
  return "$result"
}
disaster_backup() {
  # Ordinary/manual-safe and timer entrypoints always default to strict.
  # Environment variables cannot opt into maintenance; that internal argument
  # is accepted only during the wrapper's currently open, confirmed root TTY.
  local mode=${1:-strict}
  [[ $# -le 1 && ( $mode == strict || $mode == maintenance ) ]] || return 2
  if [[ $mode == maintenance ]]; then
    [[ ${maintenance_confirmation:-} == BACKUP_WITH_RELAY_INTERRUPTION ]] && disaster_maintenance_tty_valid || { die '缺少本轮真实终端风险确认，未执行停站'; return 1; }
  fi
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
  if [[ $mode == strict ]]; then note '安全整站快照须先通过完整 v2 keep_last 链门禁；自动计划不会接受旧链路中断风险。客户独立 VPS 服务不在操作范围内。'; fi
  disaster_pause_begin "$mode" || return
  if [[ ${#DISASTER_RUNNING[@]} -gt 0 ]]; then
    DISASTER_RESUME=1
    compose_live stop --timeout 60 "${DISASTER_RUNNING[@]}" || return
  fi
  note '  → 导出 PostgreSQL 原始快照'
  compose_live exec -T database pg_dump --username=msboost --dbname="$database_name" --format=custom > "$DISASTER_WORK/database.dump" || return
  [[ -s $DISASTER_WORK/database.dump ]] || { die '数据库导出为空'; return 1; }
  # The helper is read-only and does not start the application or any worker.
  local -a key_args=()
  [[ -n $(env_get "$INSTALL_ROOT/.env" MASTER_KEY) ]] || key_args=(--master-key-file /app/data/master.key)
  note '  → 只读导出加密业务快照'
  compose_live run --rm --no-deps -T --user 0:0 --entrypoint /usr/local/bin/msboost-restore server export "${key_args[@]}" > "$DISASTER_WORK/state.msb" || return
  for volume in app_data caddy_data caddy_config; do
    note "  → 导出数据卷：$volume"
    docker run --rm --network none --read-only --user 0:0 --entrypoint tar \
      --mount "type=volume,source=msboost_$volume,target=/snapshot,readonly" "$image" -cf - -C /snapshot . > "$DISASTER_WORK/$volume.tar" || return
    note "  → 校验数据卷归档：$volume"
    "$DISASTER_TOOL" disaster validate-volume --archive "$DISASTER_WORK/$volume.tar" || return
  done
  note '  → 解除备份写入冻结'
  if ! disaster_pause_end; then disaster_pause_recovery_hint; return 1; fi
  note '  → 恢复备份前正在运行的服务并检查健康'
  if [[ ${#DISASTER_RUNNING[@]} -gt 0 ]]; then disaster_resume_services "${DISASTER_RUNNING[@]}" || return; fi
  DISASTER_RESUME=0
  note '  → 打包并校验整站备份'
  "$DISASTER_TOOL" disaster pack --dir "$DISASTER_WORK" --output "$directory/$filename" || return
  note "整站快照已完成并校验：$directory/$filename（含主密钥与密码，勿公开上传）。"
  # Remote failure preserves the verified local bundle and does not prune.
  note '  → 按已保存配置处理异地上传与保留策略'
  "$DISASTER_TOOL" disaster upload --config "$INSTALL_ROOT/disaster.json" --archive "$directory/$filename" || return
  "$DISASTER_TOOL" disaster retain --config "$INSTALL_ROOT/disaster.json" || return
}

# Bash's elapsed clock avoids wall-clock/NTP changes during the shared deadline.
disaster_resume_clock() { printf '%s' "$SECONDS"; }

disaster_resume_services() {
  # Older Compose v2 supports up --wait but not start --wait. Never substitute
  # up here: only the exact existing services paused by this snapshot may start.
  # All services share one health-polling deadline. Docker CLI calls use the
  # local daemon, but are not forcibly interrupted if the daemon itself stalls.
  local service container identity state health required all_ready now remaining
  local deadline=$(( $(disaster_resume_clock) + 180 ))
  local -a containers=() services=()
  [[ $# -gt 0 && $# -le 2 ]] || { die '需要明确的原有备份服务集合'; return 1; }
  for service in "$@"; do
    [[ $service == caddy || $service == server ]] || { die '备份恢复不能启动其他服务'; return 1; }
    [[ " ${services[*]} " != *" $service "* ]] || { die '备份恢复服务重复'; return 1; }
    container=$(compose_live ps --all --quiet "$service") || return
    [[ $container =~ ^[a-f0-9]{64}$ ]] || { die "原有 $service 容器缺失或不唯一，未创建替代容器"; return 1; }
    identity=$(docker inspect --type container --format '{{index .Config.Labels "com.docker.compose.project"}}|{{index .Config.Labels "com.docker.compose.service"}}' "$container") || return
    [[ $identity == "$PROJECT|$service" ]] || { die "原有 $service 容器归属不符"; return 1; }
    containers+=("$container"); services+=("$service")
  done
  compose_live start "${services[@]}" || return
  while :; do
    now=$(disaster_resume_clock)
    [[ $now -lt $deadline ]] || { die '原有服务在 180 秒内未全部恢复健康'; return 1; }
    all_ready=1
    for container in "${containers[@]}"; do
      # Only state flags are read, never service environment or health output.
      identity=$(docker inspect --type container --format '{{.State.Status}}|{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}|{{if .Config.Healthcheck}}{{if .Config.Healthcheck.Test}}{{if eq (index .Config.Healthcheck.Test 0) "NONE"}}none{{else}}required{{end}}{{else}}none{{end}}{{else}}none{{end}}' "$container") || { die '原有服务容器在恢复期间消失'; return 1; }
      IFS='|' read -r state health required <<< "$identity"
      case "$state" in
        exited|dead|removing) die '原有服务启动后退出，未通过恢复检查'; return 1 ;;
        running|created|restarting|paused) ;;
        *) die '原有服务运行状态无效'; return 1 ;;
      esac
      [[ $health != unhealthy ]] || { die '原有服务健康检查失败'; return 1; }
      [[ $required == none || $required == required ]] || { die '原有服务健康检查状态无效'; return 1; }
      if [[ $state != running || ( $required == required && $health != healthy ) ]]; then all_ready=0; fi
    done
    now=$(disaster_resume_clock)
    [[ $now -lt $deadline ]] || { die '原有服务在 180 秒内未全部恢复健康'; return 1; }
    [[ $all_ready == 0 ]] || return 0
    remaining=$((deadline - now)); [[ $remaining -le 2 ]] || remaining=2
    sleep "$remaining" || return
  done
}

disaster_pause_recovery_hint() {
  local command
  note '备份写入冻结可能仍然存在；它不会超时自动解除。不要删除原 token 文件或重新生成替代 token。'
  note "私有恢复材料：$DISASTER_WORK/backup-pause.token（仅本机 root 可读，请勿上传或粘贴内容）。"
  printf -v command 'MSBOOST_ENV_FILE=%q docker compose --project-name %q --env-file %q -f %q run --rm --no-deps -T --user 0:0 --entrypoint /usr/local/bin/msboost-restore server backup-pause end < %q' \
    "$INSTALL_ROOT/.env" "$PROJECT" "$INSTALL_ROOT/.env" "$INSTALL_ROOT/deploy/compose.yml" "$DISASTER_WORK/backup-pause.token"
  note '排除数据库 / Docker 故障后，在本机 root 终端执行以下命令，必须确认退出码为 0，再恢复原有控制面服务：'
  note "$command"
  note "备份前正在运行的控制面服务：${DISASTER_RUNNING[*]:-无}。此流程不会启动或重启客户 Agent / GOST。"
}

disaster_pause_begin() {
  set +xv
  local token mode=${1:-strict}
  [[ -d $DISASTER_WORK && ! -L $DISASTER_WORK ]] || { die '备份门禁需要私有工作目录'; return 1; }
  token=$(random_hex 32) || return
  [[ $token =~ ^[a-f0-9]{64}$ ]] || { die '备份门禁随机凭据生成失败'; return 1; }
  # Persist the only unlock credential before any database request. Exclusive
  # creation plus the mktemp root-only parent prevents accidental replacement.
  (umask 077; set -o noclobber; printf '%s\n' "$token" > "$DISASTER_WORK/backup-pause.token") || return
  unset token
  chmod 600 "$DISASTER_WORK/backup-pause.token" || return
  sync -f "$DISASTER_WORK/backup-pause.token" || return
  note "备份门禁私有工作目录：$DISASTER_WORK；异常退出时请保留其中的 backup-pause.token。"
  # Record cleanup intent BEFORE begin: a failed/lost response does not prove
  # that PostgreSQL rolled back. A missing gate makes end safely idempotent.
  DISASTER_PAUSE_RELEASE=1
  if [[ $mode == maintenance ]]; then
    if ! printf '%s\n' "$(<"$DISASTER_WORK/backup-pause.token")" "$maintenance_confirmation" |
      compose_live run --rm --no-deps -T --user 0:0 --entrypoint /usr/local/bin/msboost-restore server backup-pause begin-maintenance; then
      die '手动维护备份门禁未确认成功，未执行停站；任务、备份活动、恢复冲突和无效记录不能通过风险确认绕过。'
      return 1
    fi
    note '手动维护门禁已建立：你已接受旧链路因短租约中断的风险。此备份不承诺不断流，其他安全门禁仍有效。'
  else
    if ! compose_live run --rm --no-deps -T --user 0:0 --entrypoint /usr/local/bin/msboost-restore server backup-pause begin < "$DISASTER_WORK/backup-pause.token"; then
      die '停站备份门禁未确认成功，未执行停站；旧工具、v1 / 混合链、未确认配置或恢复状态都会阻止备份。'
      return 1
    fi
    note '停站备份门禁已建立，全部控制面业务写入暂时冻结；该检查仅核对已确认状态，不代表真实线路不断流验收已完成。'
  fi
}

disaster_pause_end() {
  [[ ${DISASTER_PAUSE_RELEASE:-0} == 1 ]] || return 0
  [[ -f $DISASTER_WORK/backup-pause.token && ! -L $DISASTER_WORK/backup-pause.token ]] || { die '原备份门禁 token 文件缺失或类型无效，拒绝尝试替代凭据'; return 1; }
  compose_live run --rm --no-deps -T --user 0:0 --entrypoint /usr/local/bin/msboost-restore server backup-pause end < "$DISASTER_WORK/backup-pause.token" || return
  DISASTER_PAUSE_RELEASE=0
}


disaster_cleanup() {
  local code=$?
  trap - EXIT
  if [[ $code != 0 && -n ${DISASTER_RUN_ID:-} && -n ${DISASTER_TOOL:-} ]]; then
    disaster_record "${DISASTER_FAILURE_STAGE:-failed}" || true
  fi
  if [[ ${DISASTER_TOOL_CONTAINER:-} =~ ^[a-f0-9]{64}$ ]]; then docker rm "$DISASTER_TOOL_CONTAINER" >/dev/null || code=1; fi
  # Never restart the control plane before confirmed unlock. This also handles
  # an uncertain begin result, partial stop, failed export and an ordinary signal.
  if ! disaster_pause_end; then
    disaster_pause_recovery_hint
    code=1
  fi
  # A scheduled snapshot may pause only these two known containers. Resume the
  # exact services that were running, even if export/pack/disk/upload failed.
  if [[ ${DISASTER_PAUSE_RELEASE:-0} != 1 && ${DISASTER_RESUME:-0} == 1 && ${#DISASTER_RUNNING[@]} -gt 0 ]]; then
    if ! disaster_resume_services "${DISASTER_RUNNING[@]}"; then
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

disaster_maintenance_open_tty() {
  if ! exec {maintenance_tty_fd}<>/dev/tty; then die '手动维护备份必须使用本机真实交互终端'; return 1; fi
  disaster_maintenance_tty_valid || { die '手动维护备份不接受管道、文件或无终端输入'; return 1; }
}
disaster_maintenance_tty_valid() { [[ ${maintenance_tty_fd:-} =~ ^[0-9]+$ && -t $maintenance_tty_fd ]]; }
disaster_maintenance_read() {
  IFS= read -r -u "$maintenance_tty_fd" -p '逐次确认请输入 BACKUP_WITH_RELAY_INTERRUPTION（其他输入取消）: ' maintenance_confirmation || { die '确认已取消或结束，未执行停站'; return 1; }
}


disaster_assert_volume() {
  [[ $(docker volume inspect --format '{{index .Labels "com.docker.compose.project"}}' "$1") == "$PROJECT" ]]
}
