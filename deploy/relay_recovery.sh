#!/usr/bin/env bash
# MSBOOST_RELAY_RECOVERY_V1 — root-only private-file transport, never a timer.
# Depends only on the trusted local Docker/config template helpers in
# backup_activity_recovery.sh. It does not invoke that module's reconciliation.

relay_recovery_open_tty() {
  if ! exec {recovery_tty_fd}<>/dev/tty; then die '中转恢复必须使用本机真实交互终端'; return 1; fi
  [[ -t $recovery_tty_fd ]] || { die '中转恢复不接受自动确认或管道终端'; return 1; }
}
relay_recovery_read() {
  IFS= read -r -u "$recovery_tty_fd" -p "$2" "$1" || { die '输入已取消或结束，未执行请求'; return 1; }
}
relay_recovery_assert_parent() {
  local path=$1 mode
  [[ $path == /* && $path != *[[:cntrl:]]* && $(realpath -m -- "$path") == "$path" ]] || { die '文件必须使用不含跳转或控制字符的规范绝对路径'; return 1; }
  while :; do
    [[ -d $path && ! -L $path && $(stat -c %u -- "$path") == 0 ]] || { die '文件的上级目录必须由 root 所有且不能是符号链接'; return 1; }
    mode=$(stat -c %a -- "$path") || return
    [[ $mode =~ ^[0-7]{3,4}$ && $((8#$mode & 0022)) == 0 ]] || { die '文件上级目录可被其他用户写入'; return 1; }
    [[ $path != / ]] || break
    path=$(dirname -- "$path") || return
  done
}
relay_recovery_assert_input() {
  local path=$1 size
  [[ $path == /* && $path != *[[:cntrl:]]* && $(realpath -m -- "$path") == "$path" && -f $path && ! -L $path ]] || { die '请求必须是规范绝对路径的普通文件，不接受符号链接'; return 1; }
  relay_recovery_assert_parent "$(dirname -- "$path")" || return
  [[ $(stat -c '%u:%a:%h' -- "$path") == 0:600:1 ]] || { die '请求文件必须 root 所有、0600 且没有额外硬链接'; return 1; }
  size=$(stat -c %s -- "$path") || return
  [[ $size =~ ^[0-9]{1,9}$ && $size -gt 0 && $size -le 33554432 ]] || { die '请求文件必须非空且不超过 32 MiB'; return 1; }
}
relay_recovery_assert_output() {
  local path=$1 parent
  [[ $path == /* && $path != */ && $path != *[[:cntrl:]]* && $(realpath -m -- "$path") == "$path" && ! -e $path && ! -L $path ]] || { die '输出必须是不存在的规范绝对路径；禁止覆盖或使用符号链接'; return 1; }
  parent=$(dirname -- "$path") || return
  relay_recovery_assert_parent "$parent" || return
  [[ $(stat -c '%u:%a' -- "$parent") == 0:700 ]] || { die '输出目录必须预先由 root 创建并设为 0700'; return 1; }
}

relay_recovery_site_identity() {
  assert_managed || return
  local file mode image metadata os arch identity source server_id
  for file in /opt "$INSTALL_ROOT" "$INSTALL_ROOT/.env" "$INSTALL_ROOT/.managed-by-msboost"; do
    [[ ! -L $file && $(stat -c %u "$file") == 0 ]] || { die '安装配置或父目录归属异常'; return 1; }
    mode=$(stat -c %a "$file") || return
    [[ $mode =~ ^[0-7]{3,4}$ && $((8#$mode & 0022)) == 0 ]] || { die '安装配置或父目录可被其他用户写入'; return 1; }
  done
  [[ $(stat -c '%u:%a:%h' -- "$INSTALL_ROOT/.env") == 0:600:1 ]] || { die '.env 必须为原 root-only 0600 配置文件'; return 1; }
  [[ $(env_get "$INSTALL_ROOT/.env" MASTER_KEY) =~ ^[a-fA-F0-9]{64}$ ]] || { die '受管配置缺少有效的原 MASTER_KEY；不会生成替代密钥'; return 1; }
  image=$(env_get "$INSTALL_ROOT/.env" MSBOOST_IMAGE)
  recovery_image_id=$(env_get "$INSTALL_ROOT/.env" MSBOOST_IMAGE_ID)
  [[ $image =~ ^ghcr.io/mozziexwz/node@sha256:[a-f0-9]{64}$ || $image =~ ^msboost-release:v[0-9]+\.[0-9]+\.[0-9]+-(amd64|arm64)-[a-f0-9]{12}$ ]] || { die '只允许原已校验官方发布镜像'; return 1; }
  [[ $recovery_image_id =~ ^sha256:[a-f0-9]{64}$ ]] || { die '已安装镜像 ID 无效'; return 1; }
  metadata=$(backup_activity_docker image inspect "$image" --format '{{.Os}}|{{.Architecture}}|{{.Id}}|{{index .Config.Labels "org.opencontainers.image.source"}}') || return
  IFS='|' read -r os arch identity source <<< "$metadata"
  [[ $os == linux && $arch == "$(platform_arch)" && $identity == "$recovery_image_id" && $source == https://github.com/mozziexwz/node ]] || { die '恢复工具镜像身份不符'; return 1; }
  recovery_database_name=$(env_get "$INSTALL_ROOT/.env" MSBOOST_DATABASE_NAME); recovery_database_name=${recovery_database_name:-msboost}
  [[ $recovery_database_name =~ ^[a-zA-Z_][a-zA-Z0-9_]{0,62}$ ]] || { die '受管数据库名称无效'; return 1; }
  recovery_database_id=$(backup_activity_docker ps --quiet --no-trunc --filter "label=com.docker.compose.project=$PROJECT" --filter label=com.docker.compose.service=database) || return
  [[ $recovery_database_id =~ ^[a-f0-9]{64}$ ]] || { die '本站数据库容器未运行或不唯一'; return 1; }
  identity=$(backup_activity_docker inspect --type container --format '{{index .Config.Labels "com.docker.compose.project"}}|{{index .Config.Labels "com.docker.compose.service"}}|{{.State.Status}}|{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$recovery_database_id") || return
  [[ $identity == "$PROJECT|database|running|healthy" ]] || { die '本站数据库容器归属或健康状态异常'; return 1; }
  server_id=$(backup_activity_docker ps --all --quiet --no-trunc --filter "label=com.docker.compose.project=$PROJECT" --filter label=com.docker.compose.service=server) || return
  [[ $server_id =~ ^[a-f0-9]{64}$ ]] || { die '原 server 容器缺失或不唯一'; return 1; }
  identity=$(backup_activity_docker inspect --type container --format '{{.Image}}|{{index .Config.Labels "com.docker.compose.project"}}|{{index .Config.Labels "com.docker.compose.service"}}' "$server_id") || return
  [[ $identity == "$recovery_image_id|$PROJECT|server" ]] || { die '原 server 镜像或归属不符'; return 1; }
  identity=$(backup_activity_docker inspect --type container --format "$(backup_activity_environment_template "$recovery_database_name")" "$server_id") || return
  [[ $identity == true ]] || { die '原 server 实际数据目录 / 数据库配置不符或有重复覆盖字段'; return 1; }
  # Fixed template selects only the two identity fields. Values travel solely
  # through an anonymous pipe into a no-network/no-logging pinned helper; never
  # interpolate the expected key into a template, argv, terminal or error file.
  if ! identity=$(set -o pipefail
    backup_activity_docker inspect --type container --format '{{- $comma := false -}}[{{- range .Config.Env -}}{{- if and (ge (len .) 11) (or (eq (slice . 0 11) "MASTER_KEY=") (eq (slice . 0 11) "PUBLIC_URL=")) -}}{{- if $comma -}},{{- end -}}{{json .}}{{- $comma = true -}}{{- end -}}{{- end -}}]' "$server_id" 2>/dev/null |
      backup_activity_docker run --rm -i --pull never --read-only --user 0:0 --cap-drop ALL --security-opt no-new-privileges:true --log-driver none --ulimit core=0 \
        --network none --env-file "$INSTALL_ROOT/.env" --entrypoint /usr/local/bin/msboost-restore "$recovery_image_id" relay-recovery verify-environment 2>/dev/null
  ); then die '原 server 密钥 / 公网地址无法安全核对；未执行恢复请求'; return 1; fi
  [[ $identity == true ]] || { die '原 server 密钥 / 公网地址与受管配置不符；未执行恢复请求'; return 1; }
}

relay_recovery_private_cleanup() {
  local code=$?
  trap - EXIT
  if [[ -n ${recovery_work:-} ]]; then
    if [[ ${recovery_published:-0} == 1 ]]; then
      note "受保护结果已保存：$recovery_output；请勿公开包含凭据的 JSON。"
    else
      note "未确认完成，结果可能已提交。私有现场保留于：$recovery_work；先核对 status / 原请求，不要生成替代凭据。"
    fi
  fi
  exit "$code"
}

relay_recovery_site() (
  set +xv
  umask 077
  ulimit -c 0 || return
  [[ $# == 0 ]] || { die 'relay-recovery 不接受命令行参数，只允许本机终端选择操作和私有文件'; return 2; }
  [[ $(id -u) == 0 && $(uname -s) == Linux ]] || { die '中转恢复仅允许目标 Linux 服务器 root 执行'; return 1; }
  [[ -z ${INVOCATION_ID:-} && -z ${SYSTEMD_EXEC_PID:-} && -z ${JOURNAL_STREAM:-} ]] || { die '中转恢复禁止自动任务调用'; return 1; }
  declare -F backup_activity_docker >/dev/null && declare -F backup_activity_environment_template >/dev/null || { die '缺少配套受保护本机身份核对模块'; return 1; }
  local recovery_action='' recovery_input='' recovery_output='' recovery_confirmation='' recovery_work='' recovery_published=0
  local recovery_tty_fd recovery_input_fd recovery_image_id recovery_database_id recovery_database_name recovery_parent recovery_parent_id recovery_input_id
  export -n recovery_action recovery_input recovery_output recovery_confirmation recovery_work
  trap relay_recovery_private_cleanup EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  relay_recovery_site_identity || return
  relay_recovery_open_tty || return
  note '操作：inspect / prepare / status / finalize / tls-inspect / tls-rotate。此入口不停止或重启服务，不拉取镜像。'
  note 'prepare 的结果包含受信接管凭据；所有结果和错误只写私有文件，不能粘贴到公共日志。TLS 轮换可能需要中断连接。'
  relay_recovery_read recovery_action '输入本次操作名: ' || return
  case "$recovery_action" in inspect|prepare|status|finalize|tls-inspect|tls-rotate) ;; *) die '不支持的操作；未执行请求'; return 1 ;; esac
  if [[ $recovery_action != status ]]; then
    relay_recovery_read recovery_input '已核对请求 JSON 的绝对路径（root 0600）: ' || return
    relay_recovery_assert_input "$recovery_input" || return
    recovery_input_id=$(stat -c '%d:%i' -- "$recovery_input") || return
  fi
  relay_recovery_read recovery_output '新的结果文件绝对路径（父目录 root 0700，不覆盖）: ' || return
  relay_recovery_assert_output "$recovery_output" || return
  recovery_parent=$(dirname -- "$recovery_output") || return
  recovery_parent_id=$(stat -c '%d:%i' -- "$recovery_parent") || return
  note "本次操作：$recovery_action；输出：$recovery_output。请独立核对原站隔离、节点快照与请求里的精确确认字段。"
  relay_recovery_read recovery_confirmation "输入 RELAY_RECOVERY $recovery_action 确认本次操作（其他输入取消）: " || return
  [[ $recovery_confirmation == "RELAY_RECOVERY $recovery_action" ]] || { note '已取消，未执行请求。'; return 1; }
  exec {recovery_tty_fd}>&-
  relay_recovery_assert_output "$recovery_output" || return
  [[ $(stat -c '%d:%i' -- "$recovery_parent") == "$recovery_parent_id" ]] || { die '输出目录已改变，未执行请求'; return 1; }
  if [[ $recovery_action == status ]]; then exec {recovery_input_fd}</dev/null
  else
    relay_recovery_assert_input "$recovery_input" || return
    [[ $(stat -c '%d:%i' -- "$recovery_input") == "$recovery_input_id" ]] || { die '原请求文件已被替换，未执行请求'; return 1; }
    exec {recovery_input_fd}<"$recovery_input" || return
    [[ $(stat -Lc '%d:%i' -- "/proc/$BASHPID/fd/$recovery_input_fd") == "$recovery_input_id" ]] || { die '请求文件打开后的身份不符，未执行请求'; return 1; }
  fi
  recovery_work=$(mktemp -d "$recovery_parent/.msboost-relay-recovery.XXXXXXXX") || return
  [[ -d $recovery_work && ! -L $recovery_work && $(stat -c '%u:%a' -- "$recovery_work") == 0:700 ]] || { die '私有暂存目录身份无效'; return 1; }
  (set -o noclobber; : > "$recovery_work/result.partial"; : > "$recovery_work/error.log") || return
  [[ $(stat -c '%u:%a:%h' -- "$recovery_work/result.partial") == 0:600:1 && $(stat -c '%u:%a:%h' -- "$recovery_work/error.log") == 0:600:1 ]] || return 1
  sync -f "$recovery_work" || return
  note "私有操作目录：$recovery_work（异常中断时保留；不公开其中内容）。"
  if ! backup_activity_docker run --rm -i --pull never --read-only --user 0:0 --cap-drop ALL --security-opt no-new-privileges:true --log-driver none --ulimit core=0 \
    --network "container:$recovery_database_id" --env-file "$INSTALL_ROOT/.env" \
    --env DATABASE_URL= --env DATABASE_HOST=127.0.0.1 --env DATABASE_PORT=5432 --env DATABASE_USER=msboost --env "DATABASE_NAME=$recovery_database_name" --env DATABASE_SSLMODE=disable \
    --entrypoint /usr/local/bin/msboost-restore "$recovery_image_id" relay-recovery "$recovery_action" <&"$recovery_input_fd" > "$recovery_work/result.partial" 2> "$recovery_work/error.log"; then
    die '受保护请求未确认成功；没有把结果或错误内容输出到终端。请保留私有现场核对，不要盲目重试 prepare。'
    return 1
  fi
  exec {recovery_input_fd}<&-
  [[ -s $recovery_work/result.partial && ! -L $recovery_work/result.partial ]] || { die '工具没有返回完整结果；提交状态未知'; return 1; }
  sync -f "$recovery_work/result.partial" && sync -f "$recovery_work" || return
  relay_recovery_assert_output "$recovery_output" || return
  [[ $(stat -c '%d:%i' -- "$recovery_parent") == "$recovery_parent_id" ]] || { die '输出目录改变；保留原私有结果，未发布'; return 1; }
  # GNU ln -T is atomic no-replace publication. Unlike rename/mv, it cannot
  # overwrite a destination that another root operation created meanwhile.
  ln -T -- "$recovery_work/result.partial" "$recovery_output" || { die '输出发布失败或路径已存在，原私有结果保留'; return 1; }
  sync -f "$recovery_output" && sync -f "$recovery_parent" || return
  recovery_published=1
  rm -- "$recovery_work/result.partial" "$recovery_work/error.log" || return
  sync -f "$recovery_work" || return
  rmdir -- "$recovery_work" || return
  sync -f "$recovery_parent" || return
)
