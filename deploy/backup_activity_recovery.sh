#!/usr/bin/env bash
# Local, interactive reconciliation of an interrupted WEB backup marker only.
# Never called by a timer, and never clears a whole-site backup-pause gate.
# MSBOOST_BACKUP_ACTIVITY_RECOVERY_V1

backup_activity_docker() (
  unset DOCKER_HOST DOCKER_CONTEXT DOCKER_DEFAULT_PLATFORM
  docker --host unix:///var/run/docker.sock "$@"
)

backup_activity_open_tty() {
  if ! exec {tty_fd}<>/dev/tty; then die '核对异常网页备份必须使用本机交互终端'; return 1; fi
  [[ -t $tty_fd ]] || { die '确认必须来自本机终端，不能使用 stdin 文件或管道'; return 1; }
}

backup_activity_read_confirmation() {
  if ! IFS= read -r -u "$tty_fd" -p "请输入 RECONCILE_BACKUP $operation_id 确认核销（其他输入取消）: " confirmation; then
    die '输入已取消或结束；未发送核销请求'
    return 1
  fi
}

backup_activity_environment_template() {
  # Compare inside Docker's template, returning one boolean only. Never emit
  # a DATABASE_URL value, password, full Config.Env or even an unexpected value.
  # Counting exact matches also rejects duplicate configuration entries.
  local database_name=$1 expected key template='' condition='' part index=0
  for expected in DATA_DIR=/app/data DATABASE_HOST=database DATABASE_USER=msboost "DATABASE_NAME=$database_name" DATABASE_SSLMODE=disable DATABASE_URL= DATABASE_PORT=5432; do
    key=${expected%%=*}=
    printf -v part '{{$v%d := ""}}{{range .Config.Env}}{{if and (ge (len .) %d) (eq (slice . 0 %d) "%s")}}{{if eq . "%s"}}{{$v%d = printf "%%s1" $v%d}}{{else}}{{$v%d = printf "%%s!" $v%d}}{{end}}{{end}}{{end}}' \
      "$index" "${#key}" "${#key}" "$key" "$expected" "$index" "$index" "$index" "$index"
    template+=$part
    if [[ $key == DATABASE_URL= || $key == DATABASE_PORT= ]]; then
      printf -v part ' (or (eq $v%d "") (eq $v%d "1"))' "$index" "$index"
    else printf -v part ' (eq $v%d "1")' "$index"; fi
    condition+=$part
    index=$((index + 1))
  done
  printf '%s{{and%s}}' "$template" "$condition"
}

backup_activity_reconcile_site() (
  set +xv
  ulimit -c 0 || return
  local LC_ALL=C inspection='' operation_id='' fingerprint='' confirmation=''
  export -n inspection operation_id fingerprint confirmation
  trap 'inspection=; operation_id=; fingerprint=; confirmation=' EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  [[ $# == 0 ]] || { die 'backup-reconcile 不接受参数，只能在本机终端逐项核对'; return 2; }
  [[ $(id -u) == 0 && $(uname -s) == Linux ]] || { die '异常网页备份核销仅允许在目标 Linux 服务器以 root 运行'; return 1; }
  assert_managed || return
  local file mode image image_id image_metadata os arch source database_id server_id identity database_name tty_fd
  for file in /opt "$INSTALL_ROOT" "$INSTALL_ROOT/.env" "$INSTALL_ROOT/.managed-by-msboost"; do
    [[ ! -L $file && $(stat -c %u "$file") == 0 ]] || { die '安装配置或父目录归属异常'; return 1; }
    mode=$(stat -c %a "$file") || return
    [[ $mode =~ ^[0-7]{3,4}$ && $((8#$mode & 0022)) == 0 ]] || { die '安装配置或父目录可被其他用户写入'; return 1; }
  done
  mode=$(stat -c %a "$INSTALL_ROOT/.env") || return
  [[ $((8#$mode & 0077)) == 0 ]] || { die '.env 必须仅 root 可读写'; return 1; }
  image=$(env_get "$INSTALL_ROOT/.env" MSBOOST_IMAGE)
  image_id=$(env_get "$INSTALL_ROOT/.env" MSBOOST_IMAGE_ID)
  [[ $image =~ ^ghcr.io/mozziexwz/node@sha256:[a-f0-9]{64}$ || $image =~ ^msboost-release:v[0-9]+\.[0-9]+\.[0-9]+-(amd64|arm64)-[a-f0-9]{12}$ ]] || { die '只允许已校验的官方发布镜像，请先升级或修复正式部署'; return 1; }
  [[ $image_id =~ ^sha256:[a-f0-9]{64}$ ]] || { die '已安装镜像 ID 无效'; return 1; }
  image_metadata=$(backup_activity_docker image inspect "$image" --format '{{.Os}}|{{.Architecture}}|{{.Id}}|{{index .Config.Labels "org.opencontainers.image.source"}}') || return
  IFS='|' read -r os arch identity source <<< "$image_metadata"
  [[ $os == linux && $arch == "$(platform_arch)" && $identity == "$image_id" && $source == https://github.com/mozziexwz/node ]] || { die '核销工具镜像身份不符；未修改数据库'; return 1; }
  database_name=$(env_get "$INSTALL_ROOT/.env" MSBOOST_DATABASE_NAME); database_name=${database_name:-msboost}
  [[ $database_name =~ ^[a-zA-Z_][a-zA-Z0-9_]{0,62}$ ]] || { die '已安装数据库名称无效'; return 1; }
  database_id=$(backup_activity_docker ps --quiet --no-trunc --filter "label=com.docker.compose.project=$PROJECT" --filter label=com.docker.compose.service=database) || return
  [[ $database_id =~ ^[a-f0-9]{64}$ ]] || { die '本站数据库容器未运行或不唯一；不会启动或重建服务'; return 1; }
  identity=$(backup_activity_docker inspect --type container --format '{{index .Config.Labels "com.docker.compose.project"}}|{{index .Config.Labels "com.docker.compose.service"}}|{{.State.Status}}|{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$database_id") || return
  [[ $identity == "$PROJECT|database|running|healthy" ]] || { die '本站数据库容器归属或健康状态异常；未修改数据库'; return 1; }
  identity=$(backup_activity_docker volume inspect --format '{{index .Labels "com.docker.compose.project"}}|{{index .Labels "com.docker.compose.volume"}}' msboost_app_data) || return
  [[ $identity == "$PROJECT|app_data" ]] || { die '原 app_data 数据卷不存在或不属于本站，不能验证原备份锁'; return 1; }
  server_id=$(backup_activity_docker ps --all --quiet --no-trunc --filter "label=com.docker.compose.project=$PROJECT" --filter label=com.docker.compose.service=server) || return
  [[ $server_id =~ ^[a-f0-9]{64}$ ]] || { die '原 server 容器缺失或不唯一，不能确认备份实际使用的数据卷'; return 1; }
  identity=$(backup_activity_docker inspect --type container --format '{{.Image}}|{{index .Config.Labels "com.docker.compose.project"}}|{{index .Config.Labels "com.docker.compose.service"}}|{{range .Mounts}}{{if eq .Destination "/app/data"}}{{.Type}}|{{.Name}}{{end}}{{end}}' "$server_id") || return
  [[ $identity == "$image_id|$PROJECT|server|volume|msboost_app_data" ]] || { die '原 server 镜像或 /app/data 挂载不符；拒绝使用替代目录或数据卷核销'; return 1; }
  identity=$(backup_activity_docker inspect --type container --format "$(backup_activity_environment_template "$database_name")" "$server_id") || return
  [[ $identity == true ]] || { die '原 server 的数据目录 / 数据库连接与当前受管配置不一致或存在重复覆盖字段；未发送核销请求'; return 1; }
  local -a helper=(run --rm -i --pull never --read-only --user 0:0 --cap-drop ALL --cap-add DAC_READ_SEARCH --security-opt no-new-privileges:true --log-driver none --ulimit core=0
    --network "container:$database_id" --env-file "$INSTALL_ROOT/.env"
    --env DATABASE_URL= --env DATABASE_HOST=127.0.0.1 --env DATABASE_PORT=5432
    --env DATABASE_USER=msboost --env "DATABASE_NAME=$database_name" --env DATABASE_SSLMODE=disable
    --mount type=volume,source=msboost_app_data,target=/app/data,readonly --env DATA_DIR=/app/data
    --entrypoint /usr/local/bin/msboost-restore "$image_id" backup-activity)
  backup_activity_open_tty || return
  # Only this fixed six-line versioned transport is parsed. No source/eval/jq,
  # no JSON values interpreted as shell, and no fallback for older helpers.
  inspection=$(backup_activity_docker "${helper[@]}" inspect-lines </dev/null) || { die '只读核对未成功；请检查已安装版本和原备份锁，未发送核销请求'; return 1; }
  [[ ${#inspection} -le 512 && $inspection != *$'\r'* ]] || { die '只读核对返回格式无效；未发送核销请求'; return 1; }
  local -a fields=()
  mapfile -t fields <<< "$inspection"
  [[ ${#fields[@]} == 6 && ${fields[0]} == MSBOOST_BACKUP_ACTIVITY_INSPECT_V1 && ${fields[1]} =~ ^[01]$ && ${fields[2]} =~ ^[01]$ &&
     ( ${fields[3]} == - || ${fields[3]} =~ ^[A-Za-z0-9_-]{1,100}$ ) &&
     ( ${fields[4]} == - || ${fields[4]} =~ ^[a-f0-9]{64}$ ) && ${fields[5]} =~ ^(0|[1-9][0-9]{0,18})$ ]] || { die '只读核对返回格式无效；未发送核销请求'; return 1; }
  if [[ ${fields[1]} == 0 ]]; then
    [[ ${fields[2]} == 0 && ${fields[3]} == - && ${fields[4]} == - && ${fields[5]} == 0 ]] || { die '只读核对返回状态矛盾'; return 1; }
    note '没有待核对的网页备份记录；未修改数据库或文件。'
    return 0
  fi
  operation_id=${fields[3]}; fingerprint=${fields[4]}
  note "待核对网页备份 ID：$operation_id"
  note "记录开始时间（Unix 毫秒）：${fields[5]}"
  note '此操作仅把无法确认结果的旧网页备份标记为 interrupted_unknown，并保留审计；不表示备份成功或可恢复。'
  note '不删除本机 / 远端备份、不停止任何服务、不解锁整站 backup-pause 门禁、不更改财务或中转恢复状态。'
  if [[ ${fields[2]} != 1 || $operation_id == - || $fingerprint == - || ${fields[5]} == 0 ]]; then
    die '当前证据不允许核销：任务可能仍在运行、原锁缺失 / 身份不符、旧记录无锁证据，或整站门禁未解除。确认输入不能绕过这些检查。'
    return 1
  fi
  backup_activity_read_confirmation || return
  [[ $confirmation == "RECONCILE_BACKUP $operation_id" ]] || { note '已取消，未发送核销请求。'; return 1; }
  exec {tty_fd}>&-
  # A fresh lock acquisition and same-transaction marker fingerprint check in
  # the helper, not this user's confirmation, decide whether mutation is safe.
  if ! printf '%s\n' "$operation_id" "$fingerprint" "$confirmation" | backup_activity_docker "${helper[@]}" reconcile; then
    die '核销未确认成功；请重新只读核对，不要绕过原锁或手工删除任务记录。所有服务保持原状。'
    return 1
  fi
  inspection=; fingerprint=; confirmation=
  note '已核销为结果未知的中断记录；请另行核验备份文件是否完整。本操作没有证明任何备份成功。'
)
