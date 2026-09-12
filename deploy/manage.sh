#!/usr/bin/env bash
# MSBOOST_DEPLOY_V1 — installation and lifecycle owner for /opt/msboost only.
set -Eeuo pipefail
umask 077

INSTALL_ROOT=/opt/msboost
PROJECT=msboost
MARKER=MSBOOST_DEPLOY_V1
VERSION=v0.1.0
SOURCE_DIR=
DOMAIN=
IP_ADDRESS=
ADMIN_EMAIL=
ALLOW_HTTP=0
BUILD=0
STAGE=
SNAPSHOT=

die() { printf '错误：%s\n' "$*" >&2; return 1; }
note() { printf '%s\n' "$*" >&2; }
usage() {
  printf '%s\n' \
    'MSBOOST Debian 12 (amd64/arm64), fixed root /opt/msboost, Compose project msboost' \
    '  msboost install --domain panel.example.com --email 12345678@qq.com' \
    '  msboost install --ip SERVER_IPV4 --email 12345678@qq.com --allow-insecure-http' \
    '  msboost upgrade [--version vX.Y.Z] [--build]' \
    '  msboost repair|status|logs|uninstall|purge' \
    '  --build is explicit and requires a verified release source bundle.' \
    'uninstall keeps .env, keys, database, application data, certificates and backups.' \
    'purge requires two TTY confirmations and removes only this installation and its four named volumes.'
}
read_tty() {
  local prompt=$1 answer
  if ! read -r -p "$prompt" answer </dev/tty; then die '需要终端交互；安装可传 --domain/--ip、--email 与 HTTP 风险标志'; return 1; fi
  printf '%s' "$answer"
}
menu() {
  note '1) 安装  2) 升级  3) 修复  4) 状态  5) 日志  6) 卸载（保留数据）  7) 彻底清理  0) 退出'
  local choice; choice=$(read_tty '请选择: ')
  case "$choice" in 1) printf install ;; 2) printf upgrade ;; 3) printf repair ;; 4) printf status ;; 5) printf logs ;; 6) printf uninstall ;; 7) printf purge ;; 0) printf exit ;; *) die '无效选择' ;; esac
}
require_platform() {
  [[ $(id -u) == 0 ]] || { die '请在目标服务器以 root 或 sudo 运行'; return 1; }
  [[ $(uname -s) == Linux && -r /etc/os-release ]] || { die '仅支持 Debian 12 Linux'; return 1; }
  local ID VERSION_ID
  . /etc/os-release
  [[ $ID == debian && $VERSION_ID == 12 ]] || { die '仅支持 Debian 12；其他系统请自行审核部署文件'; return 1; }
  case "$(uname -m)" in x86_64|aarch64) ;; *) die '当前只发布 amd64/arm64 镜像'; return 1 ;; esac
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
  local host=$DOMAIN site=$DOMAIN secure=true public
  if [[ -n $IP_ADDRESS ]]; then host=$IP_ADDRESS; site="http://$IP_ADDRESS"; secure=false; fi
  public="https://$host"; [[ $secure == true ]] || public="http://$host"
  [[ ! -e $INSTALL_ROOT/.env ]] || { die '拒绝重建已有 .env，修复不会重置密钥'; return 1; }
  {
    printf 'MSBOOST_DOMAIN=%s\nMSBOOST_SITE_ADDRESS=%s\nPUBLIC_URL=%s\nCOOKIE_SECURE=%s\n' "$host" "$site" "$public" "$secure"
    printf 'MSBOOST_VERSION=%s\nMSBOOST_IMAGE=ghcr.io/mozziexwz/node:%s\n' "$VERSION" "$VERSION"
    printf 'ADMIN_EMAIL=%s\nADMIN_PASSWORD=%s\nPOSTGRES_PASSWORD=%s\nMASTER_KEY=%s\n' "$ADMIN_EMAIL" "$(random_hex 24)" "$(random_hex 32)" "$(random_hex 32)"
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
    local pkg
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
    printf '# MSBOOST_DEPLOY_V1\nTypes: deb\nURIs: https://download.docker.com/linux/debian\nSuites: bookworm\nComponents: stable\nArchitectures: %s\nSigned-By: /etc/apt/keyrings/msboost-docker.asc\n' "$(dpkg --print-architecture)" > /etc/apt/sources.list.d/msboost-docker.sources || return
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
  (unset MSBOOST_IMAGE MSBOOST_VERSION MSBOOST_DOMAIN MSBOOST_SITE_ADDRESS POSTGRES_PASSWORD POSTGRES_IMAGE CADDY_IMAGE
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
  for file in install.sh deploy/manage.sh deploy/compose.yml deploy/compose.build.yml deploy/Caddyfile; do
    [[ -f $SOURCE_DIR/$file && ! -L $SOURCE_DIR/$file ]] || { die "部署包缺少普通文件 $file"; return 1; }
  done
}
copy_deployment_files() {
  local from=$1 to=$2 file
  install -d -m 0700 "$to/deploy" || return
  for file in manage.sh compose.yml compose.build.yml Caddyfile; do
    install -m 0600 "$from/deploy/$file" "$to/deploy/$file" || return
  done
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
  # Dependency failures are distinct from a GHCR application-image failure.
  compose_at "$STAGE" "$STAGE/.env" pull database caddy || { die 'PostgreSQL/Caddy 镜像拉取失败，停止部署，不会误用应用镜像备用来源'; return 1; }
  if [[ $BUILD == 1 ]]; then
    note '已显式选择源码构建；这会占用较多内存、CPU、磁盘和时间。'
    (unset MSBOOST_IMAGE MSBOOST_VERSION MSBOOST_DOMAIN MSBOOST_SITE_ADDRESS POSTGRES_PASSWORD POSTGRES_IMAGE CADDY_IMAGE
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
snapshot_deployment() {
  install -d -m 0700 "$INSTALL_ROOT/backups" || return
  SNAPSHOT=$(mktemp -d "$INSTALL_ROOT/backups/deploy-$(date -u +%Y%m%dT%H%M%SZ).XXXXXXXX") || return
  copy_deployment_files "$INSTALL_ROOT" "$SNAPSHOT" || return
  install -m 0600 "$INSTALL_ROOT/.env" "$SNAPSHOT/.env" || return
  note '升级前保存私有配置副本与 PostgreSQL 一致性快照；这些备份含敏感数据，请另行离机保管。'
  if ! compose_live exec -T database pg_dump --username=msboost --dbname=msboost --format=custom > "$SNAPSHOT/database.dump"; then
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
  local expected_id actual_id
  expected_id=$(env_get "$INSTALL_ROOT/.env" MSBOOST_IMAGE_ID)
  actual_id=$(server_identity "$(env_get "$INSTALL_ROOT/.env" MSBOOST_IMAGE)") || return
  [[ $expected_id =~ ^sha256:[a-f0-9]{64}$ && $actual_id == "$expected_id" ]] || { die '已安装应用 imageID 发生变化或缺失，拒绝启动；请运行 repair 核实原版本'; return 1; }
  compose_live up -d --no-build --pull never --wait --wait-timeout 180 || return
  check_frontend
}
apply_stage() {
  copy_deployment_files "$STAGE" "$INSTALL_ROOT" || return
  install -m 0600 "$STAGE/.env" "$INSTALL_ROOT/.env" || return
  start_live
}
restore_deployment() {
  [[ -n $SNAPSHOT && -f $SNAPSHOT/.env ]] || return 1
  copy_deployment_files "$SNAPSHOT" "$INSTALL_ROOT" || return
  install -m 0600 "$SNAPSHOT/.env" "$INSTALL_ROOT/.env" || return
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
  validate_source || return
  ensure_docker || return
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
repair_site() {
  assert_managed || return
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
  install -m 0600 "$STAGE/.env" "$INSTALL_ROOT/.env" || return
  start_live || { die '修复未通过健康检查；未重置任何配置/密钥/数据'; return 1; }
  note '修复完成：重建必要容器，保留现有版本、管理员、密钥与所有数据。'
}
uninstall_site() {
  assert_managed || return
  compose_live down --timeout 30 || return
  note '已卸载本站容器与专用网络；数据库、应用数据、TLS证书、密钥、.env 和备份均保留。恢复请运行 msboost repair。'
  note '未删除 Docker、镜像或其他项目；未操作任何客户 VPS 和独立 Agent。'
}
purge_site() {
  assert_managed || return
  note '不可恢复操作：将删除 MSBOOST 的数据库、应用文件、证书、主密钥、配置及本机部署备份。请先完成离机备份。'
  [[ $(read_tty '第一次确认，请输入 DELETE_MSBOOST: ') == DELETE_MSBOOST ]] || { die '清理已取消'; return 1; }
  [[ $(read_tty '第二次确认，请输入完整路径 /opt/msboost: ') == /opt/msboost ]] || { die '清理已取消'; return 1; }
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
manage_main() {
  local action version_given=0
  if [[ $# == 0 ]]; then action=$(menu); else action=$1; shift; fi
  case "$action" in help|--help|-h) usage; return 0 ;; exit) return 0 ;; stop) action=uninstall ;; esac
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --version|--source-dir|--domain|--ip|--email)
        [[ $# -ge 2 && $2 != --* ]] || { die "缺少 $1 参数"; return 2; }
        case "$1" in --version) VERSION=$2; version_given=1 ;; --source-dir) SOURCE_DIR=$2 ;; --domain) DOMAIN=$2 ;; --ip) IP_ADDRESS=$2 ;; --email) ADMIN_EMAIL=$2 ;; esac
        shift 2 ;;
      --allow-insecure-http) ALLOW_HTTP=1; shift ;;
      --build) BUILD=1; shift ;;
      *) die "未知参数 $1"; return 2 ;;
    esac
  done
  case "$action" in install|upgrade|repair|status|logs|uninstall|purge) ;; *) usage; return 2 ;; esac
  [[ $VERSION =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$ ]] || { die '版本格式必须为 vX.Y.Z'; return 2; }
  require_platform || return
  assert_root_path || return
  if [[ $action != install && ( -n $DOMAIN || -n $IP_ADDRESS || -n $ADMIN_EMAIL || $ALLOW_HTTP == 1 ) ]]; then die '域名/IP/邮箱仅用于首次安装；升级修复不会重置配置'; return 2; fi
  if [[ $action == upgrade && -z $SOURCE_DIR ]]; then
    assert_managed || return
    local -a args=(upgrade)
    [[ $version_given == 0 ]] || args+=(--version "$VERSION")
    [[ $BUILD == 0 ]] || args+=(--build)
    exec bash "$INSTALL_ROOT/install.sh" "${args[@]}"
  fi
  # The lock covers every mutation and is retained until this process exits.
  # /run is root-owned; /run/lock may be world-writable on Debian. Do not open
  # a predictable root lock file in a directory where another user can plant it.
  [[ ! -L /run/msboost-deploy.lock && ( ! -e /run/msboost-deploy.lock || ( -f /run/msboost-deploy.lock && $(stat -c %u /run/msboost-deploy.lock) == 0 ) ) ]] || { die '部署锁文件归属异常'; return 1; }
  exec 9>/run/msboost-deploy.lock
  flock -n 9 || { die '另一个 MSBOOST 部署管理操作正在运行'; return 1; }
  trap cleanup_stage EXIT
  case "$action" in
    install) install_site ;; upgrade) upgrade_site ;; repair) repair_site ;;
    uninstall) uninstall_site ;; purge) purge_site ;;
    status) assert_managed && compose_live ps ;;
    logs) assert_managed && compose_live logs --tail 200 server caddy database ;;
  esac
}

if [[ ${BASH_SOURCE[0]} == "$0" ]]; then manage_main "$@"; fi
