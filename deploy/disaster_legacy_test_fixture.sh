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
