#!/usr/bin/env bash
# MSBOOST_DEPLOY_V1 — installation and lifecycle owner for /opt/msboost only.
set -Eeuo pipefail
umask 077

INSTALL_ROOT=/opt/msboost
PROJECT=msboost
MARKER=MSBOOST_DEPLOY_V1
VERSION=v0.3.1
SOURCE_DIR=
DOMAIN=
IP_ADDRESS=
ADMIN_EMAIL=
ALLOW_HTTP=0
BUILD=0
RECOVER_INCOMPLETE=0
STAGE=
SNAPSHOT=
DISASTER_ARCHIVE=

die() { printf '错误：%s\n' "$*" >&2; return 1; }
note() { printf '%s\n' "$*" >&2; }
usage() {
  printf '%s\n' \
    'MSBOOST 控制面 Debian 12/13（amd64/arm64），安装目录 /opt/msboost，Compose 项目 msboost' \
    '  msboost install --domain panel.example.com --email 12345678@qq.com' \
    '  msboost install --ip SERVER_IPV4 --email 12345678@qq.com --allow-insecure-http' \
    '  msboost upgrade [--version vX.Y.Z] [--build]' \
    '  bash install.sh upgrade --version vX.Y.Z --recover-incomplete  （仅恢复 v0.1.1 的失败首次安装）' \
    '  msboost repair|status|logs|uninstall|purge' \
    '  msboost admin-password        本机交互修改已有管理员密码（不停止服务）' \
    '  msboost backup-reconcile      核对异常网页备份（不停止服务，不解锁整站门禁）' \
    '  msboost relay-recovery        受保护中转恢复 / TLS 维护（私有文件交互）' \
    '  msboost disaster-backup|disaster-config|disaster-disable' \
    '  msboost disaster-backup-maintenance  单次手动维护备份（真实终端确认，接受旧链路中断）' \
    '  bash install.sh disaster-restore --archive /root/msboost-backup/整站备份.tar.gz' \
    '  --build 需显式选择，并提供已校验的完整源码包。' \
    'uninstall 保留配置、密钥、数据库、应用数据、证书及备份。' \
    'purge 需要两次终端确认，只删除本站安装及四个站点数据卷。'
}
read_tty() {
  local prompt=$1 answer
  if ! read -r -p "$prompt" answer </dev/tty; then die '需要终端交互；安装可传 --domain/--ip、--email 与 HTTP 风险标志'; return 1; fi
  printf '%s' "$answer"
}
menu() {
  printf '\n%s\n' 'MSBOOST 网站部署管理' '  1) 安装网站' '  2) 升级（先备份）' '  3) 修复（保留配置和密钥）' '  4) 查看状态' '  5) 查看日志' '  6) 卸载（保留全部数据）' '  7) 彻底清理（不可恢复）' '  8) 一键整站灾难备份' '  9) 设置整站备份目录 / 远程密码 / 每日计划' '  10) 一键灾难恢复（仅全新目标）' '  11) 停用整站自动备份计划' '  12) 修改已有管理员密码（仅本机 root）' '  13) 核对异常网页备份（不停止服务）' '  14) 手动维护备份（确认旧链路可能中断）' '  15) 中转恢复 / TLS 维护（受保护私有文件）' '  0) 退出' >&2
  local choice; choice=$(read_tty '请选择: ')
  case "$choice" in 1) printf install ;; 2) printf upgrade ;; 3) printf repair ;; 4) printf status ;; 5) printf logs ;; 6) printf uninstall ;; 7) printf purge ;; 8) printf disaster-backup ;; 9) printf disaster-config ;; 10) printf disaster-restore ;; 11) printf disaster-disable ;; 12) printf admin-password ;; 13) printf backup-reconcile ;; 14) printf disaster-backup-maintenance ;; 15) printf relay-recovery ;; 0) printf exit ;; *) die '无效选择' ;; esac
}
require_platform() (
  [[ $(id -u) == 0 ]] || { die '请在目标服务器以 root 或 sudo 运行'; return 1; }
  [[ $(uname -s) == Linux && -r /etc/os-release ]] || { die '控制面仅支持 Debian 12/13 Linux'; return 1; }
  local ID VERSION_ID VERSION_CODENAME expected
  . /etc/os-release
  case ${VERSION_ID:-} in
    12) expected=bookworm ;;
    13) expected=trixie ;;
    *) die '控制面仅支持 Debian 12/13；其他系统请自行审核部署文件'; return 1 ;;
  esac
  [[ ${ID:-} == debian && ${VERSION_CODENAME:-} == "$expected" ]] || { die 'Debian 系统版本与代号不一致，拒绝配置软件源'; return 1; }
  case "$(uname -m)" in x86_64|aarch64) ;; *) die '当前只发布 amd64/arm64 镜像'; return 1 ;; esac
)
debian_codename() (
  require_platform || return
  local VERSION_ID
  . /etc/os-release
  case $VERSION_ID in 12) printf bookworm ;; 13) printf trixie ;; esac
)
require_release_version() {
  [[ $VERSION =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$ ]] || { die '版本格式必须为 vX.Y.Z'; return 2; }
}
assert_root_path() {
  [[ $INSTALL_ROOT == /opt/msboost && ! -L /opt && ! -L /opt/msboost ]] || { die '安装路径必须是非符号链接 /opt/msboost'; return 1; }
  [[ $(realpath -m -- "$INSTALL_ROOT") == /opt/msboost ]] || { die '安装目录解析异常'; return 1; }
}
assert_managed() {
  assert_root_path || return
  [[ -d $INSTALL_ROOT && ! -L $INSTALL_ROOT/.managed-by-msboost && -f $INSTALL_ROOT/.managed-by-msboost && $(<"$INSTALL_ROOT/.managed-by-msboost") == "$MARKER" ]] || { die '目录不是此安装器管理的项目，拒绝覆盖或删除'; return 1; }
  [[ ! -L $INSTALL_ROOT/.env && -f $INSTALL_ROOT/.env && $(stat -c %u "$INSTALL_ROOT/.env") == 0 ]] || { die '.env 必须是 root 所有的普通文件'; return 1; }
  [[ ! -L $INSTALL_ROOT/deploy && -d $INSTALL_ROOT/deploy ]] || { die 'deploy 目录异常'; return 1; }
}
env_get() {
  local file=$1 key=$2
  awk -v k="$key" 'index($0,k "=")==1 {sub(/^[^=]*=/,""); print; exit}' "$file"
}
env_set() {
  local file=$1 key=$2 value=$3
  [[ $key =~ ^[A-Z_]+$ && $value != *$'\n'* && $value != *$'\r'* ]] || { die '无效环境变量'; return 1; }
  awk -v k="$key" -v v="$value" 'BEGIN {found=0} index($0,k "=")==1 {if(!found)print k "=" v;found=1;next} {print} END {if(!found)print k "=" v}' "$file" > "$file.next" || return
  chmod 600 "$file.next" || return
  mv -f -- "$file.next" "$file"
}
random_hex() { od -An -N "$1" -tx1 /dev/urandom | tr -d ' \n'; }
random_admin_password() {
  # Keep the full 192 bits of entropy while separating every five hex digits.
  # This guarantees the bootstrap credential obeys the six-digit password rule.
  local raw i
  raw=$(random_hex 24) || return
  for ((i=0; i<${#raw}; i+=5)); do
    (( i == 0 )) || printf '-'
    printf '%s' "${raw:i:5}"
  done
  printf '\n'
}
valid_domain() {
  local host=$1 label
  [[ ${#host} -le 253 && $host == *.* && $host =~ ^[a-z0-9.-]+$ && $host =~ \.[a-z]{2,63}$ ]] || return 1
  local -a labels; IFS=. read -r -a labels <<< "$host"
  for label in "${labels[@]}"; do [[ ${#label} -ge 1 && ${#label} -le 63 && $label =~ ^[a-z0-9]([a-z0-9-]*[a-z0-9])?$ ]] || return 1; done
}
valid_ipv4() {
  local part; local -a parts
  [[ $1 =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]] || return 1
  IFS=. read -r -a parts <<< "$1"
  for part in "${parts[@]}"; do [[ ${#part} -le 3 && ( $part == 0 || $part != 0* ) && $((10#$part)) -le 255 ]] || return 1; done
  [[ $1 != 0.0.0.0 ]]
}
collect_install_settings() {
  [[ -z $DOMAIN || -z $IP_ADDRESS ]] || { die '--domain 与 --ip 不能同时使用'; return 1; }
  if [[ -z $DOMAIN && -z $IP_ADDRESS ]]; then
    local host; host=$(read_tty '请输入已解析到本 VPS 的域名（推荐 HTTPS）或此 VPS 的 IPv4: ')
    if valid_ipv4 "$host"; then IP_ADDRESS=$host; else DOMAIN=${host,,}; fi
  fi
  if [[ -n $DOMAIN ]]; then
    DOMAIN=${DOMAIN,,}; valid_domain "$DOMAIN" || { die '域名格式无效，不要带协议、端口或路径'; return 1; }
  else
    valid_ipv4 "$IP_ADDRESS" || { die 'IP 调试模式当前要求有效 IPv4'; return 1; }
    note '高风险：HTTP 不加密管理员密码、Cookie、SSH 凭据和配置；只供临时隔离调试。请勿在公网 HTTP 上开展真实业务。'
    if [[ $ALLOW_HTTP != 1 ]]; then [[ $(read_tty '确认风险请输入 HTTP_RISK: ') == HTTP_RISK ]] || { die '未确认 HTTP 风险'; return 1; }; fi
  fi
  if [[ -z $ADMIN_EMAIL ]]; then ADMIN_EMAIL=$(read_tty '首次管理员 QQ 邮箱（纯数字@qq.com）: '); fi
  [[ $ADMIN_EMAIL =~ ^[1-9][0-9]{4,14}@qq\.com$ ]] || { die '请输入纯数字 QQ 邮箱'; return 1; }
}
write_initial_environment() {
  require_release_version || return
  local host=$DOMAIN site=$DOMAIN secure=true public
  if [[ -n $IP_ADDRESS ]]; then host=$IP_ADDRESS; site="http://$IP_ADDRESS"; secure=false; fi
  public="https://$host"; [[ $secure == true ]] || public="http://$host"
  [[ ! -e $INSTALL_ROOT/.env ]] || { die '拒绝重建已有 .env，修复不会重置密钥'; return 1; }
  {
    printf 'MSBOOST_DOMAIN=%s\nMSBOOST_SITE_ADDRESS=%s\nPUBLIC_URL=%s\nCOOKIE_SECURE=%s\n' "$host" "$site" "$public" "$secure"
    printf 'MSBOOST_VERSION=%s\nMSBOOST_IMAGE=ghcr.io/mozziexwz/node:%s\n' "$VERSION" "$VERSION"
    printf 'ADMIN_EMAIL=%s\nADMIN_PASSWORD=%s\nPOSTGRES_PASSWORD=%s\nMASTER_KEY=%s\n' "$ADMIN_EMAIL" "$(random_admin_password)" "$(random_hex 32)" "$(random_hex 32)"
  } > "$INSTALL_ROOT/.env"
  chmod 600 "$INSTALL_ROOT/.env"
}
ensure_docker() {
  if ! command -v curl >/dev/null; then
    apt-get update || return
    DEBIAN_FRONTEND=noninteractive apt-get install -y ca-certificates curl || return
  fi
  if command -v docker >/dev/null && docker compose version >/dev/null 2>&1; then
    docker info >/dev/null 2>&1 || { die 'Docker 不可用；请修复 Docker 服务，安装器不会重置现有 Docker'; return 1; }
  else
    local pkg suite
    for pkg in docker.io docker-compose podman-docker containerd runc; do
      if [[ $(dpkg-query -W -f='${Status}' "$pkg" 2>/dev/null || true) == 'install ok installed' ]]; then
        die "检测到 $pkg，请自行评估其他工作负载并迁移到官方 Docker；不会自动卸载冲突包"; return 1
      fi
    done
    note '从 Docker 官方 Debian apt 仓库安装 Engine 与 Compose 插件；不使用第三方镜像安装脚本。'
    [[ ! -L /etc/apt/keyrings/msboost-docker.asc && ! -L /etc/apt/sources.list.d/msboost-docker.sources ]] || { die 'Docker apt 配置目标不能是符号链接'; return 1; }
    if [[ -f /etc/apt/sources.list.d/msboost-docker.sources ]] && ! grep -qx '# MSBOOST_DEPLOY_V1' /etc/apt/sources.list.d/msboost-docker.sources; then
      die 'msboost-docker.sources 已存在且不属于安装器，拒绝覆盖'; return 1
    fi
    apt-get update || return
    DEBIAN_FRONTEND=noninteractive apt-get install -y ca-certificates curl || return
    install -d -m 0755 /etc/apt/keyrings || return
    curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 --connect-timeout 15 --max-time 120 https://download.docker.com/linux/debian/gpg -o /etc/apt/keyrings/msboost-docker.asc || return
    chmod 0644 /etc/apt/keyrings/msboost-docker.asc || return
    suite=$(debian_codename) || return
    printf '# MSBOOST_DEPLOY_V1\nTypes: deb\nURIs: https://download.docker.com/linux/debian\nSuites: %s\nComponents: stable\nArchitectures: %s\nSigned-By: /etc/apt/keyrings/msboost-docker.asc\n' "$suite" "$(dpkg --print-architecture)" > /etc/apt/sources.list.d/msboost-docker.sources || return
    chmod 0644 /etc/apt/sources.list.d/msboost-docker.sources || return
    apt-get update || return
    DEBIAN_FRONTEND=noninteractive apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin || return
    systemctl enable --now docker || return
    docker info >/dev/null || return
  fi
  docker compose up --help | grep -q -- --wait-timeout || { die '需要支持 up --wait-timeout 的 Docker Compose v2'; return 1; }
}
compose_at() {
  local directory=$1 environment=$2; shift 2
  (unset MSBOOST_IMAGE MSBOOST_VERSION MSBOOST_DOMAIN MSBOOST_SITE_ADDRESS MSBOOST_DATABASE_NAME POSTGRES_PASSWORD POSTGRES_IMAGE CADDY_IMAGE
   MSBOOST_ENV_FILE="$environment" docker compose --project-name "$PROJECT" --env-file "$environment" -f "$directory/deploy/compose.yml" "$@")
}
compose_live() { compose_at "$INSTALL_ROOT" "$INSTALL_ROOT/.env" "$@"; }

# A pre-existing DOCKER_HOST/context must never redirect lifecycle or deletion
# commands to another machine. Only the target host's standard local daemon.
docker() { (unset DOCKER_HOST DOCKER_CONTEXT DOCKER_DEFAULT_PLATFORM; command docker --host unix:///var/run/docker.sock "$@"); }

assert_no_collision() {
  local name
  [[ -z $(docker ps -aq --filter "label=com.docker.compose.project=$PROJECT") ]] || { die '已存在 msboost 项目的容器，拒绝接管'; return 1; }
  for name in msboost_app_data msboost_database_data msboost_caddy_data msboost_caddy_config; do
    if docker volume inspect "$name" >/dev/null 2>&1; then die "发现未登记数据卷 $name，请先确认其归属；不会覆盖"; return 1; fi
  done
  if docker network inspect msboost_control >/dev/null 2>&1; then die '已存在 msboost_control 网络，拒绝接管'; return 1; fi
  if command -v ss >/dev/null && [[ -n $(ss -H -ltn '( sport = :80 or sport = :443 )') ]]; then die 'TCP 80 或 443 已被使用；请先处理冲突，不会停止其他服务'; return 1; fi
}
validate_source() {
  [[ -n $SOURCE_DIR && -d $SOURCE_DIR ]] || { die '需要校验过的 release bundle；请使用仓库根 install.sh'; return 1; }
  SOURCE_DIR=$(realpath -- "$SOURCE_DIR")
  local file
  # Current release sources must be complete before any host mutation. The
  # copy helper remains optional for modules only when copying old snapshots.
  for file in install.sh deploy/manage.sh deploy/compose.yml deploy/compose.build.yml deploy/Caddyfile deploy/disaster.sh deploy/backup_activity_recovery.sh deploy/relay_recovery.sh; do
    [[ -f $SOURCE_DIR/$file && ! -L $SOURCE_DIR/$file ]] || { die "部署包缺少普通文件 $file"; return 1; }
  done
}
copy_deployment_files() {
  local from=$1 to=$2 file
  install -d -m 0700 "$to/deploy" || return
  for file in manage.sh compose.yml compose.build.yml Caddyfile; do
    install -m 0600 "$from/deploy/$file" "$to/deploy/$file" || return
  done
  # Optional only for snapshot/rollback compatibility with pre-disaster releases.
  if [[ -f $from/deploy/disaster.sh && ! -L $from/deploy/disaster.sh ]]; then install -m 0600 "$from/deploy/disaster.sh" "$to/deploy/disaster.sh" || return; fi
  if [[ -f $from/deploy/backup_activity_recovery.sh && ! -L $from/deploy/backup_activity_recovery.sh ]]; then install -m 0600 "$from/deploy/backup_activity_recovery.sh" "$to/deploy/backup_activity_recovery.sh" || return; fi
  if [[ -f $from/deploy/relay_recovery.sh && ! -L $from/deploy/relay_recovery.sh ]]; then install -m 0600 "$from/deploy/relay_recovery.sh" "$to/deploy/relay_recovery.sh" || return; fi
  install -m 0700 "$from/install.sh" "$to/install.sh" || return
  chmod 0700 "$to/deploy/manage.sh"
}
install_launcher() {
  if [[ -e /usr/local/bin/msboost || -L /usr/local/bin/msboost ]]; then
    [[ ! -L /usr/local/bin/msboost && -f /usr/local/bin/msboost ]] && grep -qx '# MSBOOST_DEPLOY_V1' /usr/local/bin/msboost || { die '/usr/local/bin/msboost 已存在且不属于安装器'; return 1; }
  fi
  printf '%s\n' '#!/usr/bin/env bash' '# MSBOOST_DEPLOY_V1' 'exec bash /opt/msboost/deploy/manage.sh "$@"' > /usr/local/bin/msboost
  chmod 0755 /usr/local/bin/msboost
}
prepare_stage() {
  require_release_version || return
  note '  → 准备私有部署文件，保留已有密码与主密钥'
  STAGE=$(mktemp -d "$INSTALL_ROOT/.stage.XXXXXXXX") || return
  copy_deployment_files "$SOURCE_DIR" "$STAGE" || return
  install -m 0600 "$INSTALL_ROOT/.env" "$STAGE/.env" || return
  env_set "$STAGE/.env" MSBOOST_VERSION "$VERSION" || return
  if [[ $BUILD == 1 ]]; then
    local item
    for item in Dockerfile go.mod go.sum cmd internal installers apps; do
      [[ -e $SOURCE_DIR/$item ]] || { die "--build 需要完整源码：缺少 $item"; return 1; }
    done
    (cd "$SOURCE_DIR" && tar --exclude=node_modules --exclude=dist --exclude=.env -cf - Dockerfile go.mod go.sum cmd internal installers apps) | (cd "$STAGE" && tar -xf -) || return
    env_set "$STAGE/.env" MSBOOST_IMAGE "msboost-local:$VERSION-$(random_hex 6)" || return
  else
    env_set "$STAGE/.env" MSBOOST_IMAGE "ghcr.io/mozziexwz/node:$VERSION" || return
  fi
  compose_at "$STAGE" "$STAGE/.env" config --quiet || return
}
acquire_images() {
  note '  → 获取数据库、反向代理和应用镜像'
  # Dependency failures are distinct from a GHCR application-image failure.
  compose_at "$STAGE" "$STAGE/.env" pull database caddy || { die 'PostgreSQL/Caddy 镜像拉取失败，停止部署，不会误用应用镜像备用来源'; return 1; }
  if [[ $BUILD == 1 ]]; then
    note '已显式选择源码构建；这会占用较多内存、CPU、磁盘和时间。'
    (unset MSBOOST_IMAGE MSBOOST_VERSION MSBOOST_DOMAIN MSBOOST_SITE_ADDRESS MSBOOST_DATABASE_NAME POSTGRES_PASSWORD POSTGRES_IMAGE CADDY_IMAGE
     MSBOOST_ENV_FILE="$STAGE/.env" docker compose --project-name "$PROJECT" --env-file "$STAGE/.env" -f "$STAGE/deploy/compose.yml" -f "$STAGE/deploy/compose.build.yml" build server) || return
  else
    if ! compose_at "$STAGE" "$STAGE/.env" pull server; then
      note 'GHCR 应用镜像拉取失败；正在回退到同版本 GitHub Release 的 CI 预构建镜像归档（不是源码编译）。'
      load_release_image || { die 'GHCR 与 Release 预构建镜像均不可用，停止部署并保留当前配置/数据；不会静默本地编译'; return 1; }
    fi
  fi
  # Record immutable digests after successful pulls. A later retag cannot make
  # rollback or repair accidentally start different images under an old tag.
  freeze_images
}
freeze_images() {
  local server postgres caddy
  server=$(env_get "$STAGE/.env" MSBOOST_IMAGE)
  if [[ $server != msboost-local:* && $server != msboost-release:* ]]; then freeze_image MSBOOST_IMAGE "$server" ghcr.io/mozziexwz/node || return; fi
  record_server_identity || return
  postgres=$(env_get "$STAGE/.env" POSTGRES_IMAGE); postgres=${postgres:-postgres:17-bookworm}
  caddy=$(env_get "$STAGE/.env" CADDY_IMAGE); caddy=${caddy:-caddy:2-alpine}
  freeze_image POSTGRES_IMAGE "$postgres" postgres || return
  freeze_image CADDY_IMAGE "$caddy" caddy || return
}
freeze_image() {
  local key=$1 reference=$2 repository=$3 digest candidate digests
  digest=
  digests=$(docker image inspect "$reference" --format '{{range .RepoDigests}}{{println .}}{{end}}') || return
  while IFS= read -r candidate; do
    if [[ $candidate =~ ^[a-z0-9./_-]+@sha256:[a-f0-9]{64}$ && ( $candidate == "$repository"@sha256:* || $candidate == "docker.io/library/$repository"@sha256:* ) ]]; then digest=$candidate; break; fi
  done <<< "$digests"
  [[ -n $digest ]] || { die "无法读取 $repository 的已拉取镜像摘要，停止应用新配置"; return 1; }
  env_set "$STAGE/.env" "$key" "$digest"
}
platform_arch() {
  case "$(uname -m)" in x86_64) printf amd64 ;; aarch64) printf arm64 ;; *) die '不支持的镜像架构'; return 1 ;; esac
}
server_identity() {
  local reference=$1 identity os arch image_id source
  identity=$(docker image inspect "$reference" --format '{{.Os}}|{{.Architecture}}|{{.Id}}|{{index .Config.Labels "org.opencontainers.image.source"}}') || return
  IFS='|' read -r os arch image_id source <<< "$identity"
  [[ $os == linux && $arch == "$(platform_arch)" && $image_id =~ ^sha256:[a-f0-9]{64}$ && $source == https://github.com/mozziexwz/node ]] || { die '应用镜像 OS/架构/imageID/source 标签不符合本项目'; return 1; }
  printf '%s' "$image_id"
}
record_server_identity() {
  local image_id
  image_id=$(server_identity "$(env_get "$STAGE/.env" MSBOOST_IMAGE)") || return
  env_set "$STAGE/.env" MSBOOST_IMAGE_ID "$image_id"
}
load_release_image() {
  local expected_id=${1:-} arch archive tag path expected actual output image_id local_tag
  arch=$(platform_arch) || return
  archive="msboost-image-linux-$arch.tar.gz"
  tag="ghcr.io/mozziexwz/node:$VERSION"
  path="$STAGE/image-cache"
  install -d -m 0700 "$path" || return
  local asset
  for asset in "$archive" SHA256SUMS; do
    curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 --connect-timeout 15 --max-time 1800 --max-filesize 1073741824 --retry 2 \
      "https://github.com/mozziexwz/node/releases/download/$VERSION/$asset" -o "$path/$asset" || return
  done
  expected=$(awk -v asset="$archive" '$2 == asset || $2 == "*" asset {print $1}' "$path/SHA256SUMS")
  [[ $expected =~ ^[a-fA-F0-9]{64}$ ]] || { die 'Release 镜像的校验值缺失、重复或无效'; return 1; }
  actual=$(sha256sum "$path/$archive") || return; actual=${actual%% *}
  [[ ${actual,,} == ${expected,,} ]] || { die 'Release 镜像归档 SHA256 不一致；拒绝载入'; return 1; }
  output=$(docker load --input "$path/$archive") || return
  printf '%s\n' "$output" | grep -Fxq -- "Loaded image: $tag" || { die '归档未载入约定版本的应用标签'; return 1; }
  image_id=$(server_identity "$tag") || return
  if [[ -n $expected_id && $image_id != "$expected_id" ]]; then die '归档 imageID 与原部署不一致；拒绝以同名版本替换原镜像'; return 1; fi
  local_tag="msboost-release:$VERSION-$arch-${image_id:7:12}"
  docker tag "$image_id" "$local_tag" || return
  env_set "$STAGE/.env" MSBOOST_IMAGE "$local_tag" || return
  env_set "$STAGE/.env" MSBOOST_IMAGE_ID "$image_id"
}
snapshot_configuration() {
  [[ ! -L $INSTALL_ROOT/backups ]] || { die '备份目录不能是符号链接'; return 1; }
  install -d -m 0700 "$INSTALL_ROOT/backups" || return
  SNAPSHOT=$(mktemp -d "$INSTALL_ROOT/backups/deploy-$(date -u +%Y%m%dT%H%M%SZ).XXXXXXXX") || return
  copy_deployment_files "$INSTALL_ROOT" "$SNAPSHOT" || return
  install -m 0600 "$INSTALL_ROOT/.env" "$SNAPSHOT/.env" || return
}
snapshot_deployment() {
  local database_name
  database_name=$(env_get "$INSTALL_ROOT/.env" MSBOOST_DATABASE_NAME)
  database_name=${database_name:-msboost}
  [[ $database_name =~ ^[a-zA-Z_][a-zA-Z0-9_]{0,62}$ ]] || { die '当前数据库名称无效，停止备份与升级'; return 1; }
  snapshot_configuration || return
  note '升级前保存私有配置副本与 PostgreSQL 一致性快照；这些备份含敏感数据，请另行离机保管。'
  if ! compose_live exec -T database pg_dump --username=msboost --dbname="$database_name" --format=custom > "$SNAPSHOT/database.dump"; then
    die '数据库备份失败，停止升级。若站点已卸载，请先 repair 恢复服务。'; return 1
  fi
  [[ -s $SNAPSHOT/database.dump ]] || { die '数据库备份为空，停止升级'; return 1; }
  chmod 0600 "$SNAPSHOT/database.dump"
}
check_frontend() {
  local public host port
  public=$(env_get "$INSTALL_ROOT/.env" PUBLIC_URL)
  host=$(env_get "$INSTALL_ROOT/.env" MSBOOST_DOMAIN)
  case "$public" in
    "https://$host") valid_domain "$host" || return; port=443 ;;
    "http://$host") valid_ipv4 "$host" || return; port=80 ;;
    *) die 'PUBLIC_URL 与站点地址不匹配，请修正 .env'; return 1 ;;
  esac
  # Test Caddy and real hostname/certificate locally. This is not a claim that
  # external DNS, every firewall or any business/SSH/payment path was verified.
  curl --fail --silent --show-error --noproxy '*' --connect-timeout 5 --max-time 10 \
    --retry 12 --retry-delay 5 --retry-all-errors --resolve "$host:$port:127.0.0.1" "$public/api/health" >/dev/null
}
start_live() {
  [[ ! -e $INSTALL_ROOT/.disaster-incomplete && ! -L $INSTALL_ROOT/.disaster-incomplete ]] || { die '灾难恢复尚未完成，拒绝启动空或部分恢复的站点。请保留数据与私有工作目录，按恢复文档人工核查。'; return 1; }
  note '  → 启动容器并检查应用、反向代理及 HTTPS'
  local expected_id actual_id
  expected_id=$(env_get "$INSTALL_ROOT/.env" MSBOOST_IMAGE_ID)
  actual_id=$(server_identity "$(env_get "$INSTALL_ROOT/.env" MSBOOST_IMAGE)") || return
  [[ $expected_id =~ ^sha256:[a-f0-9]{64}$ && $actual_id == "$expected_id" ]] || { die '已安装应用 imageID 发生变化或缺失，拒绝启动；请运行 repair 核实原版本'; return 1; }
  # A bind-mounted Caddyfile can change without changing Compose's service
  # hash. Start/verify the application first, then recreate only the proxy so
  # it remounts the current file and actually loads its new (or rolled-back)
  # configuration. Never force-recreate the database or other dependencies.
  compose_live up -d --no-build --pull never --wait --wait-timeout 180 database server || return
  compose_live up -d --no-build --pull never --no-deps --force-recreate --wait --wait-timeout 180 caddy || return
  check_frontend
}
replace_live_environment() (
  # Same-directory rename prevents interruption/disk-full from truncating keys.
  local next_env
  next_env=$(mktemp "$INSTALL_ROOT/.env.replace.XXXXXXXX") || return
  trap 'rm -f -- "$next_env"' EXIT
  install -m 0600 "$1" "$next_env" || return
  mv -f -- "$next_env" "$INSTALL_ROOT/.env"
)
apply_stage() {
  copy_deployment_files "$STAGE" "$INSTALL_ROOT" || return
  replace_live_environment "$STAGE/.env" || return
  start_live
}
restore_deployment() {
  [[ -n $SNAPSHOT && -f $SNAPSHOT/.env ]] || return 1
  copy_deployment_files "$SNAPSHOT" "$INSTALL_ROOT" || return
  replace_live_environment "$SNAPSHOT/.env" || return
  start_live
}
cleanup_stage() {
  # No user-provided path or broad Docker cleanup is accepted here.
  if [[ -n ${STAGE:-} && $STAGE == "$INSTALL_ROOT"/.stage.* && ! -L $STAGE && -d $STAGE && $(realpath -m "$STAGE") == "$STAGE" ]]; then rm -rf -- "$STAGE"; fi
}
install_site() {
  assert_root_path || return
  [[ ! -e $INSTALL_ROOT ]] || { die '/opt/msboost 已存在；已有安装请用 repair/upgrade，绝不重新生成密钥'; return 1; }
  validate_source || return
  collect_install_settings || return
  ensure_docker || return
  assert_no_collision || return
  install -d -m 0700 "$INSTALL_ROOT" || return
  printf '%s\n' "$MARKER" > "$INSTALL_ROOT/.managed-by-msboost"
  write_initial_environment || return
  copy_deployment_files "$SOURCE_DIR" "$INSTALL_ROOT" || return
  install_launcher || return
  prepare_stage || return
  acquire_images || return
  if ! apply_stage; then
    compose_live down --timeout 30 || true
    die '安装未通过健康/HTTPS检查。配置、密钥与已产生的数据已保留；修正 DNS/防火墙/网络后执行 msboost repair。'
    return 1
  fi
  note "安装完成：$(env_get "$INSTALL_ROOT/.env" PUBLIC_URL)"
  note '随机管理员凭据与独立主密钥保存在 root-only /opt/msboost/.env。请安全读取并另行备份；不会把密码输出到安装日志。'
  note '这只验证了控制面容器和本机反向代理。还需配置 SMTP/支付/Executor/Relay，并分别验收真实业务。'
}
upgrade_site() {
  assert_managed || return
  [[ ! -e $INSTALL_ROOT/.disaster-incomplete && ! -L $INSTALL_ROOT/.disaster-incomplete ]] || { die '灾难导入未完成，升级不会绕过恢复隔离标记'; return 1; }
  validate_source || return
  ensure_docker || return
  if [[ $RECOVER_INCOMPLETE == 1 ]]; then recover_incomplete_site; return; fi
  prepare_stage || return
  acquire_images || return
  snapshot_deployment || return
  if ! apply_stage; then
    note "升级失败，尝试恢复旧镜像与配置；保留备份 $SNAPSHOT。"
    if restore_deployment; then note '旧镜像和配置已恢复。此次升级仍为失败；未自动回滚业务数据或数据库迁移。'
    else note '旧配置已尽力恢复但服务检查仍失败；保留全部数据和快照，请人工检查。'; fi
    return 1
  fi
  note "升级完成：$VERSION；旧配置/数据库快照保存在 $SNAPSHOT。请验证业务后再自行归档备份。"
}
recover_incomplete_site() {
  # Narrow recovery for the v0.1.1 os-release collision, not a way to bypass
  # database backups for an existing or merely stopped deployment.
  local resources name key
  [[ $(env_get "$INSTALL_ROOT/.env" MSBOOST_VERSION) == '12 (bookworm)' &&
     $(env_get "$INSTALL_ROOT/.env" MSBOOST_IMAGE) == 'ghcr.io/mozziexwz/node:12 (bookworm)' &&
     -z $(env_get "$INSTALL_ROOT/.env" MSBOOST_IMAGE_ID) ]] || {
    die '--recover-incomplete 仅用于版本被写成 12 (bookworm) 且从未启动的失败安装；已有业务请正常 repair/upgrade'; return 1;
  }
  [[ $BUILD == 0 ]] || { die '首次安装恢复只使用预构建镜像'; return 1; }
  for key in ADMIN_EMAIL ADMIN_PASSWORD POSTGRES_PASSWORD MASTER_KEY; do
    [[ -n $(env_get "$INSTALL_ROOT/.env" "$key") ]] || { die "缺少现有 $key，停止恢复；不会生成替代密钥或密码"; return 1; }
  done
  resources=$(docker ps -aq --filter "label=com.docker.compose.project=$PROJECT") || return
  [[ -z $resources ]] || { die '检测到现有 MSBOOST 容器，拒绝首次安装恢复；不会停止或删除业务'; return 1; }
  resources=$(docker volume ls --format '{{.Name}}') || return
  while IFS= read -r name; do
    case "$name" in msboost_app_data|msboost_database_data|msboost_caddy_data|msboost_caddy_config)
      die "检测到已有数据卷 $name，拒绝跳过数据库备份；不会删除数据"; return 1 ;;
    esac
  done <<< "$resources"
  resources=$(docker network ls --format '{{.Name}}') || return
  while IFS= read -r name; do
    [[ $name != msboost_control ]] || { die '检测到已有 MSBOOST 网络，停止首次安装恢复'; return 1; }
  done <<< "$resources"
  assert_no_collision || return
  prepare_stage || return
  acquire_images || return
  snapshot_configuration || return
  note "已保存原始配置与脚本到 $SNAPSHOT；恢复保留域名、管理员密码和主密钥。"
  if ! apply_stage; then
    if [[ $(env_get "$INSTALL_ROOT/.env" MSBOOST_VERSION) == "$VERSION" && -n $(env_get "$INSTALL_ROOT/.env" MSBOOST_IMAGE_ID) ]]; then
      die '恢复未通过健康/HTTPS检查；新版本配置及全部密钥已保留。修正 DNS/防火墙后执行 msboost repair；不要彻底清理。'
    else
      die '部署文件写入失败；原始配置与备份保留。检查磁盘/权限后重新执行新安装脚本的 --recover-incomplete 命令；不要彻底清理。'
    fi
    return 1
  fi
  install_launcher || return
  note "失败的首次安装已恢复到 $VERSION：$(env_get "$INSTALL_ROOT/.env" PUBLIC_URL)；密码仍在 root-only /opt/msboost/.env。"
}
repair_site() {
  assert_managed || return
  [[ ! -e $INSTALL_ROOT/.disaster-incomplete && ! -L $INSTALL_ROOT/.disaster-incomplete ]] || { die '灾难导入尚未完成，repair 不会绕过恢复隔离标记；请先检查保留的私有恢复目录。'; return 1; }
  [[ $BUILD != 1 ]] || { die 'repair 不编译；请通过 install.sh upgrade --version 指定版本 --build 获取完整源码'; return 1; }
  ensure_docker || return
  compose_live config --quiet || return
  local image expected_id; image=$(env_get "$INSTALL_ROOT/.env" MSBOOST_IMAGE); expected_id=$(env_get "$INSTALL_ROOT/.env" MSBOOST_IMAGE_ID)
  VERSION=$(env_get "$INSTALL_ROOT/.env" MSBOOST_VERSION)
  [[ $VERSION =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$ ]] || { die '已安装版本记录无效'; return 1; }
  STAGE=$(mktemp -d "$INSTALL_ROOT/.stage.XXXXXXXX") || return
  install -m 0600 "$INSTALL_ROOT/.env" "$STAGE/.env" || return
  compose_live pull database caddy || { die 'PostgreSQL/Caddy 镜像不可用；保留当前数据和配置'; return 1; }
  case "$image" in
    ghcr.io/mozziexwz/node:v*|ghcr.io/mozziexwz/node@sha256:*)
      if ! compose_live pull server; then note 'GHCR 不可用，尝试同版本 Release 预构建归档，并检查原 imageID。'; load_release_image "$expected_id" || return; fi ;;
    msboost-release:v*)
      if ! docker image inspect "$image" >/dev/null 2>&1; then
        [[ $expected_id =~ ^sha256:[a-f0-9]{64}$ ]] || { die '缺少原 imageID，不能安全恢复归档镜像'; return 1; }
        note '本机归档镜像丢失，下载同版本预构建归档恢复。'; load_release_image "$expected_id" || return
      fi ;;
    msboost-local:v*) docker image inspect "$image" >/dev/null || { die '本机构建镜像丢失，请显式重新构建该版本'; return 1; } ;;
    *) die '镜像来源异常，请使用固定发布标签'; return 1 ;;
  esac
  freeze_images || return
  if [[ -n $expected_id && $(env_get "$STAGE/.env" MSBOOST_IMAGE_ID) != "$expected_id" ]]; then die '修复发现当前镜像与原 imageID 不一致，停止且保留旧配置'; return 1; fi
  snapshot_configuration || return
  replace_live_environment "$STAGE/.env" || return
  start_live || { die '修复未通过健康检查；未重置任何配置/密钥/数据'; return 1; }
  note '修复完成：重建必要容器，保留现有版本、管理员、密钥与所有数据。'
}
uninstall_site() {
  assert_managed || return
  if declare -F disaster_timer >/dev/null; then disaster_timer off || return; fi
  compose_live down --timeout 30 || return
  note '已卸载本站容器与专用网络；数据库、应用数据、TLS证书、密钥、.env 和备份均保留。恢复请运行 msboost repair。'
  note '未删除 Docker、镜像或其他项目；未操作任何客户 VPS 和独立 Agent。'
}
purge_site() {
  assert_managed || return
  note '不可恢复操作：将删除 MSBOOST 的数据库、应用文件、证书、主密钥、配置及本机部署备份。请先完成离机备份。'
  [[ $(read_tty '第一次确认，请输入 DELETE_MSBOOST: ') == DELETE_MSBOOST ]] || { die '清理已取消'; return 1; }
  [[ $(read_tty '第二次确认，请输入完整路径 /opt/msboost: ') == /opt/msboost ]] || { die '清理已取消'; return 1; }
  if declare -F disaster_timer >/dev/null; then disaster_timer remove || return; fi
  local volume label
  for volume in msboost_app_data msboost_database_data msboost_caddy_data msboost_caddy_config; do
    if docker volume inspect "$volume" >/dev/null 2>&1; then
      label=$(docker volume inspect --format '{{index .Labels "com.docker.compose.project"}}' "$volume")
      [[ $label == "$PROJECT" ]] || { die "数据卷 $volume 不属于项目 msboost，停止清理"; return 1; }
    fi
  done
  compose_live down --timeout 30 || return
  for volume in msboost_app_data msboost_database_data msboost_caddy_data msboost_caddy_config; do
    if docker volume inspect "$volume" >/dev/null 2>&1; then
      [[ -z $(docker ps -aq --filter "volume=$volume") ]] || { die "$volume 仍被容器引用，拒绝删除"; return 1; }
      docker volume rm "$volume" || return
    fi
  done
  assert_managed || return
  remove_managed_installation || return
  note '已永久删除 /opt/msboost 与四个本站命名卷；只能从独立离机备份恢复。Docker、镜像、其他项目与独立 Agent 均未删除。'
}
remove_managed_installation() {
  [[ $INSTALL_ROOT == /opt/msboost && $(realpath -m "$INSTALL_ROOT") == /opt/msboost && ! -L $INSTALL_ROOT ]] || { die '最终清理路径验证失败'; return 1; }
  if [[ -f /usr/local/bin/msboost && ! -L /usr/local/bin/msboost ]] && grep -qx '# MSBOOST_DEPLOY_V1' /usr/local/bin/msboost; then rm -- /usr/local/bin/msboost || return; fi
  # The fixed installation directory is checked again immediately before removal.
  [[ $INSTALL_ROOT == /opt/msboost && $(realpath -m "$INSTALL_ROOT") == /opt/msboost && ! -L $INSTALL_ROOT ]] || { die '最终清理路径验证失败'; return 1; }
  rm -rf -- /opt/msboost || return
}

# Only terminal input is accepted. Keeping this small function separate also
# allows offline contract tests to inject synthetic answers without real TTYs.
admin_password_read() {
  local fd=$1 variable=$2 prompt=$3
  if ! IFS= read -r -s -u "$fd" -p "$prompt" "$variable"; then
    printf '\n' >&2
    die '输入已取消或结束；未发送改密请求'
    return 1
  fi
  printf '\n' >&2
}
admin_password_open_tty() {
  if ! exec {tty_fd}<>/dev/tty; then die '此操作必须使用本机交互终端，不接受 stdin 文件或管道密码'; return 1; fi
  [[ -t $tty_fd ]] || { die '改密输入必须来自终端'; return 1; }
}

admin_password_site() (
  # Never trace secret expansion, even if an operator started bash with -x/-v.
  # The subshell keeps this setting and all secret variables out of its caller.
  set +xv
  ulimit -c 0 || return
  local LC_ALL=C admin_email_input='' admin_password_first='' admin_password_second='' admin_confirm_input=''
  export -n admin_email_input admin_password_first admin_password_second admin_confirm_input
  trap 'admin_password_first=; admin_password_second=; admin_email_input=; admin_confirm_input=' EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  [[ $(id -u) == 0 && $(uname -s) == Linux ]] || { die '管理员改密仅允许在目标 Linux 服务器以 root 运行'; return 1; }
  assert_managed || return
  local file mode image image_id database_id identity database_name tty_fd
  # No untrusted writable parent/file may supply the selected image or DB.
  for file in /opt "$INSTALL_ROOT" "$INSTALL_ROOT/.env" "$INSTALL_ROOT/.managed-by-msboost"; do
    [[ ! -L $file && $(stat -c %u "$file") == 0 ]] || { die '安装配置或父目录归属异常'; return 1; }
    mode=$(stat -c %a "$file") || return
    [[ $mode =~ ^[0-7]{3,4}$ && $((8#$mode & 0022)) == 0 ]] || { die '安装配置或父目录可被其他用户写入'; return 1; }
  done
  mode=$(stat -c %a "$INSTALL_ROOT/.env") || return
  [[ $((8#$mode & 0077)) == 0 ]] || { die '.env 必须仅 root 可读写'; return 1; }
  image=$(env_get "$INSTALL_ROOT/.env" MSBOOST_IMAGE)
  image_id=$(env_get "$INSTALL_ROOT/.env" MSBOOST_IMAGE_ID)
  [[ $image =~ ^ghcr.io/mozziexwz/node@sha256:[a-f0-9]{64}$ || $image =~ ^msboost-release:v[0-9]+\.[0-9]+\.[0-9]+-(amd64|arm64)-[a-f0-9]{12}$ ]] || { die '本机改密只使用已校验的官方发布镜像，请先升级或修复正式部署'; return 1; }
  [[ $image_id =~ ^sha256:[a-f0-9]{64}$ && $(server_identity "$image") == "$image_id" ]] || { die '改密工具镜像身份不符；未修改数据库'; return 1; }
  database_name=$(env_get "$INSTALL_ROOT/.env" MSBOOST_DATABASE_NAME); database_name=${database_name:-msboost}
  [[ $database_name =~ ^[a-zA-Z_][a-zA-Z0-9_]{0,62}$ ]] || { die '已安装数据库名称无效'; return 1; }
  database_id=$(docker ps --quiet --no-trunc --filter "label=com.docker.compose.project=$PROJECT" --filter label=com.docker.compose.service=database) || return
  [[ $database_id =~ ^[a-f0-9]{64}$ ]] || { die '本站数据库容器未运行或不唯一；不会启动或重建服务'; return 1; }
  identity=$(docker inspect --type container --format '{{index .Config.Labels "com.docker.compose.project"}}|{{index .Config.Labels "com.docker.compose.service"}}|{{.State.Status}}|{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$database_id") || return
  [[ $identity == "$PROJECT|database|running|healthy" ]] || { die '本站数据库容器归属或健康状态异常；未修改数据库'; return 1; }
  admin_password_open_tty || return
  note '仅修改已存在管理员的登录密码，撤销其旧会话和待完成密码恢复请求。'
  note '不停止或重启 server/Caddy/数据库/节点，不更改维护模式、角色、余额或套餐。'
  admin_password_read "$tty_fd" admin_email_input '目标管理员邮箱（不回显）: ' || return
  admin_email_input=${admin_email_input,,}
  [[ ${#admin_email_input} -le 254 && $admin_email_input =~ ^[a-z0-9._%+-]+@[a-z0-9.-]+\.[a-z]{2,63}$ ]] || { die '管理员邮箱格式无效'; return 1; }
  admin_password_read "$tty_fd" admin_password_first '新密码（12–72 字节，不回显）: ' || return
  admin_password_read "$tty_fd" admin_password_second '再次输入新密码（不回显）: ' || return
  [[ ${#admin_password_first} -ge 12 && ${#admin_password_first} -le 72 && $admin_password_first == "$admin_password_second" ]] || { die '密码须为 12–72 字节且两次一致；未修改密码'; return 1; }
  admin_password_read "$tty_fd" admin_confirm_input "确认仅修改 $admin_email_input，请输入 RESET $admin_email_input（不回显；其他输入取消）: " || return
  [[ $admin_confirm_input == "RESET $admin_email_input" ]] || { note '已取消，未修改密码。'; return 1; }
  exec {tty_fd}>&-
  # Use the pinned image ID and the exact local database container's loopback
  # namespace: no DNS/remote Docker context can redirect this local operation.
  # Existing deployment credentials are reused; NEW passwords only cross stdin.
  # No Compose up/run, migrations, maintenance switch or server restart occurs.
  if ! printf '%s\n' "$admin_email_input" "$admin_password_first" "$admin_password_second" "$admin_confirm_input" |
    docker run --rm -i --pull never --read-only --user 0:0 --cap-drop ALL --security-opt no-new-privileges:true --log-driver none --ulimit core=0 \
      --network "container:$database_id" --env-file "$INSTALL_ROOT/.env" \
      --env DATABASE_URL= --env DATABASE_HOST=127.0.0.1 --env DATABASE_PORT=5432 \
      --env DATABASE_USER=msboost --env "DATABASE_NAME=$database_name" --env DATABASE_SSLMODE=disable \
      --entrypoint /usr/local/bin/msboost-restore "$image_id" admin-password; then
    die '本机改密未确认成功；服务未停止，配置未重建。请核对已安装版本支持此命令后重试。'
    return 1
  fi
  admin_password_first=; admin_password_second=
  note '.env 中 ADMIN_PASSWORD 仍是首次初始化值，不会覆盖已有管理员的新密码；无需编辑或展示它。'
)

manage_main() {
  local action version_given=0
  if [[ $# == 0 ]]; then action=$(menu); else action=$1; shift; fi
  case "$action" in help|--help|-h) usage; return 0 ;; exit) return 0 ;; stop) action=uninstall ;; esac
  if [[ $action == admin-password && $# != 0 ]]; then die 'admin-password 不接受参数；邮箱和密码只能从本机终端输入'; return 2; fi
  if [[ $action == backup-reconcile && $# != 0 ]]; then die 'backup-reconcile 不接受参数，只能从本机终端确认具体备份记录'; return 2; fi
  if [[ $action == disaster-backup-maintenance && $# != 0 ]]; then die '手动维护备份不接受参数、自动确认或强制选项'; return 2; fi
  if [[ $action == relay-recovery && $# != 0 ]]; then die 'relay-recovery 不接受参数，只能通过本机终端选择私有请求 / 结果文件'; return 2; fi
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --version|--source-dir|--domain|--ip|--email|--archive)
        [[ $# -ge 2 && $2 != --* ]] || { die "缺少 $1 参数"; return 2; }
        case "$1" in --version) VERSION=$2; version_given=1 ;; --source-dir) SOURCE_DIR=$2 ;; --domain) DOMAIN=$2 ;; --ip) IP_ADDRESS=$2 ;; --email) ADMIN_EMAIL=$2 ;; --archive) DISASTER_ARCHIVE=$2 ;; esac
        shift 2 ;;
      --allow-insecure-http) ALLOW_HTTP=1; shift ;;
      --build) BUILD=1; shift ;;
      --recover-incomplete) RECOVER_INCOMPLETE=1; shift ;;
      *) die "未知参数 $1"; return 2 ;;
    esac
  done
  case "$action" in install|upgrade|repair|status|logs|uninstall|purge|disaster-backup|disaster-backup-maintenance|disaster-config|disaster-disable|disaster-restore|admin-password|backup-reconcile|relay-recovery) ;; *) usage; return 2 ;; esac
  require_platform || return
  require_release_version || return
  assert_root_path || return
  [[ -z $DISASTER_ARCHIVE || $action == disaster-restore ]] || { die '--archive 仅用于灾难恢复'; return 2; }
  if [[ $action == relay-recovery ]]; then
    local relay_module_dir relay_module_file relay_module_mode
    relay_module_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
    for relay_module_file in "$relay_module_dir" "$relay_module_dir/backup_activity_recovery.sh" "$relay_module_dir/relay_recovery.sh"; do
      [[ ! -L $relay_module_file && $(stat -c %u "$relay_module_file") == 0 ]] || { die '中转恢复模块缺失或归属异常'; return 1; }
      relay_module_mode=$(stat -c %a "$relay_module_file") || return
      [[ $relay_module_mode =~ ^[0-7]{3,4}$ && $((8#$relay_module_mode & 0022)) == 0 ]] || { die '中转恢复模块可被其他用户写入'; return 1; }
    done
    [[ -f $relay_module_dir/backup_activity_recovery.sh && -f $relay_module_dir/relay_recovery.sh ]] || { die '中转恢复模块必须为普通文件'; return 1; }
    source "$relay_module_dir/backup_activity_recovery.sh"
    source "$relay_module_dir/relay_recovery.sh"
  fi
  if [[ $action == backup-reconcile ]]; then
    local backup_activity_module backup_activity_file backup_activity_mode
    backup_activity_module="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)/backup_activity_recovery.sh"
    [[ -f $backup_activity_module && ! -L $backup_activity_module ]] || { die '本版本尚未包含异常网页备份核对模块'; return 1; }
    for backup_activity_file in "$(dirname "$backup_activity_module")" "$backup_activity_module"; do
      [[ ! -L $backup_activity_file && $(stat -c %u "$backup_activity_file") == 0 ]] || { die '本机核对模块归属异常'; return 1; }
      backup_activity_mode=$(stat -c %a "$backup_activity_file") || return
      [[ $backup_activity_mode =~ ^[0-7]{3,4}$ && $((8#$backup_activity_mode & 0022)) == 0 ]] || { die '本机核对模块可被其他用户写入'; return 1; }
    done
    source "$backup_activity_module"
  fi
  if [[ $action == disaster-* || $action == uninstall || $action == purge ]]; then
    local disaster_module
    disaster_module="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)/disaster.sh"
    if [[ -f $disaster_module && ! -L $disaster_module ]]; then source "$disaster_module"
    elif [[ $action == disaster-* ]]; then die '本版本尚未包含整站灾难管理模块'; return 1; fi
  fi
  if [[ $action == disaster-restore && -z $SOURCE_DIR ]]; then
    assert_managed || return
    exec bash "$INSTALL_ROOT/install.sh" disaster-restore --version "$VERSION" --archive "$DISASTER_ARCHIVE"
  fi
  if [[ $RECOVER_INCOMPLETE == 1 && $action != upgrade ]]; then die '--recover-incomplete 只能用于 upgrade'; return 2; fi
  if [[ $action != install && ( -n $DOMAIN || -n $IP_ADDRESS || -n $ADMIN_EMAIL || $ALLOW_HTTP == 1 ) ]]; then die '域名/IP/邮箱仅用于首次安装；升级修复不会重置配置'; return 2; fi
  if [[ $action == upgrade && -z $SOURCE_DIR ]]; then
    assert_managed || return
    local -a args=(upgrade)
    [[ $version_given == 0 ]] || args+=(--version "$VERSION")
    [[ $BUILD == 0 ]] || args+=(--build)
    [[ $RECOVER_INCOMPLETE == 0 ]] || args+=(--recover-incomplete)
    exec bash "$INSTALL_ROOT/install.sh" "${args[@]}"
  fi
  # The lock covers every mutation and is retained until this process exits.
  # /run is root-owned; /run/lock may be world-writable on Debian. Do not open
  # a predictable root lock file in a directory where another user can plant it.
  [[ ! -L /run/msboost-deploy.lock && ( ! -e /run/msboost-deploy.lock || ( -f /run/msboost-deploy.lock && $(stat -c %u /run/msboost-deploy.lock) == 0 ) ) ]] || { die '部署锁文件归属异常'; return 1; }
  exec 9>/run/msboost-deploy.lock
  flock -n 9 || { die '另一个 MSBOOST 部署管理操作正在运行'; return 1; }
  trap cleanup_stage EXIT
  if [[ $action == disaster-* ]]; then trap disaster_cleanup EXIT; trap 'exit 130' INT; trap 'exit 143' TERM; fi
  case "$action" in
    install) install_site ;; upgrade) upgrade_site ;; repair) repair_site ;;
    uninstall) uninstall_site ;; purge) purge_site ;;
    status) assert_managed && compose_live ps ;;
    logs) assert_managed && compose_live logs --tail 200 server caddy database ;;
    admin-password) admin_password_site ;;
    backup-reconcile) backup_activity_reconcile_site ;;
    relay-recovery) relay_recovery_site ;;
    disaster-backup) disaster_backup ;;
    disaster-backup-maintenance) disaster_backup_maintenance ;;
    disaster-config) disaster_configure ;;
    disaster-disable) assert_managed && disaster_timer off && note '整站自动备份已停用；配置和已保存的本机/远程备份未删除。' ;;
    disaster-restore) disaster_restore ;;
  esac
}

if [[ ${BASH_SOURCE[0]} == "$0" ]]; then manage_main "$@"; fi
