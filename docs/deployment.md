# MSBOOST 部署与运维

控制面目标为 **Debian 12/13，amd64 或 arm64**。[v0.3.3 正式 Release](https://github.com/mozziexwz/node/releases/tag/v0.3.3)与[标签 CI](https://github.com/mozziexwz/node/actions/runs/36163149523)已发布并通过核验。安装器从公开 Release 下载部署包并校验 SHA256，默认使用预构建镜像 `ghcr.io/mozziexwz/node:v0.3.3`，不要求在 VPS 编译 Go 或网页。实际公开资产、真实 VPS 结果和未覆盖项见[正式发布后验收记录](acceptance-vps-20260926.md)；客户 VPS 与 DD 的不同范围见[平台支持说明](platform-support.md)。

本文按当前 `install.sh`、`deploy/manage.sh`、Compose 和 `agent.sh` 编写。安装优先从 GHCR 拉取镜像；若访问失败，使用同版本 Release 的预构建镜像归档。两种渠道都不可用才明确失败，不会静默改为源码构建。v0.3.3 已有 Debian 11/12/13 客户 VPS 免费全新部署与公网 TCP 验收；这不等于真实支付、游戏协议、arm64 真机运行、DD 或完整整站卸载/恢复通过，详见[验收记录](acceptance-vps-20260926.md)。

## 1. Debian 12/13 一键安装

推荐在全新 Debian 12/13 VPS 上以 root 安装 v0.3.3。先将真实域名解析到该 VPS，开放 TCP 80、443，并确保服务器能够访问 Docker、GitHub 和 GHCR。安装目录固定为 `/opt/msboost`，Compose 项目名为 `msboost`。下列入口固定到已发布的 v0.3.3；执行前仍须核对[Release](https://github.com/mozziexwz/node/releases/tag/v0.3.3)、`SHA256SUMS` 和本机适用架构，不能用未经核验的分支脚本替代。

```sh
apt-get update
apt-get install -y ca-certificates curl tar
curl --fail --location --proto '=https' --tlsv1.2 \
  https://raw.githubusercontent.com/mozziexwz/node/v0.3.3/install.sh \
  -o /root/msboost-install.sh
bash /root/msboost-install.sh install \
  --domain panel.example.com \
  --email 12345678@qq.com
```

将示例域名、邮箱替换为自己的值。执行 `bash /root/msboost-install.sh` 不带动作参数时显示交互菜单。v0.3.3 入口的首次安装默认版本为 `v0.3.3`；也可传入明确的 `--version vX.Y.Z`。

安装入口下载该 Release 的 `msboost-deploy-vX.Y.Z.tar.gz` 和 `SHA256SUMS`，核验部署包哈希后才解包执行；拒绝归档里的绝对路径、路径穿越、链接和设备文件。清单与部署包共享 GitHub HTTPS/Release 信任边界，SHA256 不等于独立发布签名。

应用镜像先尝试 `ghcr.io/mozziexwz/node:指定版本`。若 GHCR 不可访问，安装器按本机架构下载同一 Release 的 `msboost-image-linux-amd64.tar.gz` 或 `msboost-image-linux-arm64.tar.gz`，核对 `SHA256SUMS` 后通过 `docker load` 导入。它们由 CI 预构建，不是在目标 VPS 现场编译。归档仅在私有临时 stage 中使用，操作退出后清理，不作为持久镜像备份。PostgreSQL/Caddy 拉取失败会直接停止，不触发应用镜像备用下载。

没有 Docker 时，安装器按 Debian 12/13 实际版本代号从 Docker 官方 Debian apt 仓库安装 Engine 和 Compose v2。检测到冲突的容器软件、已有 `msboost` 项目或同名卷/网络、占用的 80/443、已存在的安装目录时会停止，不接管其他工作负载。已有安装使用 `repair` 或 `upgrade`。

### 首次管理员和密钥

安装器独立随机生成管理员密码、PostgreSQL 密码与 32 字节主密钥，写入 root 所有、权限 `0600` 的 `/opt/msboost/.env`，不会输出密码到安装日志。通过受信任的服务器终端或编辑器安全读取该文件，完成登录，并把主密钥、配置另行离机备份。不要将它们粘贴到公开日志或工单。

仅在私有 root 终端读取管理员两项，不输出数据库密码和主密钥：

```sh
sed -n '/^ADMIN_EMAIL=/p; /^ADMIN_PASSWORD=/p' /opt/msboost/.env
```

管理员环境变量只用于首次引导。数据库已有管理员时，修改 `ADMIN_PASSWORD` 不会重置密码；新版提供 `msboost admin-password`（菜单12）由本机root交互修改，详见[管理员改密](admin-password.md)。管理员不支持邮件找回；普通及未验证会员可使用登录页找回。

### HTTPS 和临时 IP HTTP

域名模式由 Caddy 提供 HTTPS，使用 `PUBLIC_URL=https://域名`、`COOKIE_SECURE=true`。这是正式接入 Executor、节点和传递业务凭据所需的方式。

只有在隔离环境临时查看界面时，才使用 IPv4 HTTP 模式：

```sh
bash /root/msboost-install.sh install \
  --ip 203.0.113.10 \
  --email 12345678@qq.com \
  --allow-insecure-http
```

HTTP 会明文传输密码、Cookie 和业务数据，只适合临时界面调试。未提供显式标志时，交互安装要求输入 `HTTP_RISK`。Agent 安装入口和运行端要求 HTTPS，所以 IP HTTP 模式不能完成正式的 Executor/节点接入。

改成域名 HTTPS 时需同步维护 `.env` 的 `MSBOOST_DOMAIN`、`MSBOOST_SITE_ADDRESS`、`PUBLIC_URL`、`COOKIE_SECURE`，再执行 `msboost repair`。修复不会自行重置这些字段。

### v0.1.1 首次安装报 `12 (bookworm)` 的保留配置恢复

这是旧安装器读取操作系统信息时覆盖项目版本号的缺陷。不要再选择「彻底清理」；在 root 终端下载新入口，使用专门恢复参数：

```sh
curl -fsSL --proto '=https' --tlsv1.2 https://raw.githubusercontent.com/mozziexwz/node/v0.3.0/install.sh -o /root/msboost-install-v0.3.0.sh && \
  bash /root/msboost-install-v0.3.0.sh upgrade --version v0.3.0 --recover-incomplete
```

恢复会确认 `.env` 记录正是旧版污染值、没有原应用 imageID、没有 MSBOOST 容器/四个数据卷/专用网络；任何一项不符都会停止。不会删除数据、重置密钥，也不会跳过已有业务数据库的备份。原始 `.env` 和部署脚本先保存到 `/opt/msboost/backups/deploy-*`，然后保留全部密码、主密钥及站点配置继续安装。备份含秘密，请离机安全保管。

如果恢复已经启动容器，但 HTTPS 健康检查失败，先检查域名解析与 TCP 80/443，再运行 `msboost repair`。此时不要重复使用首次安装恢复参数。若检查发现已有容器或数据卷而拒绝恢复，请保留它们并提供脱敏错误信息，不要为通过检查而删卷。

## 2. 生命周期管理

安装后提供 `/usr/local/bin/msboost`。管理器只处理带正确所有权标记的 `/opt/msboost`，通过锁防止并行部署，且固定连接本机 Docker socket，不受继承的远程 Docker context 或 `DOCKER_HOST` 影响。

| 命令 | 行为 |
|---|---|
| `msboost` | 交互管理菜单 |
| `bash /root/msboost-install.sh install ...` | 新机器首次安装入口；下载和校验 Release 部署包 |
| `msboost install ...` | 管理层保留 install 动作，但需要 bootstrap 提供部署包；新机器用上一行入口 |
| `msboost upgrade` | 查询 GitHub 最新 Release 版本，再下载、校验并升级；镜像仍固定具体标签 |
| `msboost upgrade --version vX.Y.Z` | 使用明确版本升级 |
| `msboost repair` | 使用当前版本重建必要容器，保留配置、管理员、密钥和数据 |
| `msboost status` | 查看 Compose 状态 |
| `msboost logs` | 查看 server、Caddy、数据库最近 200 行日志 |
| `msboost uninstall` | 移除本站容器和专用网络，保留所有数据与配置 |
| `msboost purge` | 双重终端确认后永久清理本站目录和归属匹配的四个数据卷 |

安装/修复等待容器健康，再通过本机 Caddy 检查实际域名与证书的 `/api/health`。这不证明外部 DNS、所有防火墙、SSH、支付或游戏路径正常。

### 升级、备份和回退

升级先准备配置并拉取目标镜像（必要时使用同版本 Release 镜像归档），然后在 `/opt/msboost/backups/deploy-时间.随机后缀/` 保存旧 `.env`、部署文件及 PostgreSQL `pg_dump --format=custom` 一致性快照。两种应用镜像来源均失败或数据库备份失败就停止升级。

成功取得镜像后，配置固化为不可变 digest。Release 归档导入的应用镜像使用含版本/架构/image ID 的独立本地标签，并将完整 ID 保存为 `MSBOOST_IMAGE_ID`，每次启动前核验标签仍对应此 ID。修复缺失的归档镜像时，重新下载所得 ID 必须与原记录一致才接受。这样修复/回退不会因同名标签变化而无意启动其他镜像。升级保留当前 PostgreSQL、Caddy 的已固定 digest，不顺带隐式升级数据库或代理依赖。

应用新版本后的健康检查失败时，脚本尝试恢复旧镜像和旧配置，保留快照。它**不自动回滚数据库迁移或业务写入**。恢复仍失败时需保留当前数据并人工排查。部署快照不代替应用文件、独立主密钥和离机备份。

源码构建必须显式选择：

```sh
msboost upgrade --version v0.3.3 --build
```

该路径使用校验过的完整 Release 源码，叠加 `deploy/compose.build.yml`，为本次构建生成唯一的 `msboost-local:版本-随机后缀` 标签，会占用更多资源。GHCR/Release 镜像下载失败都不会自动触发编译。`repair` 不执行构建；本地构建镜像丢失时需明确重新构建该版本。

v0.2.2 起，启动先等待数据库和应用健康，再只强制重建 Caddy，使绑定的 Caddyfile 与进程实际配置一致；安装、修复和失败回退均使用相同流程，不强制重建数据库。仅文件内容变化不会触发 Compose 自动重建代理，因此旧版本单纯 `up` 可能仍运行旧配置。升级或修复会短暂中断代理连接，请安排维护窗口。

### 卸载和彻底清理

```sh
msboost uninstall
# 使用相同配置和数据恢复：
msboost repair
```

普通卸载保留 `.env`、主密钥、数据库、应用文件、TLS 状态、本机备份和管理入口。不会卸载 Docker、镜像、其他项目、独立 Agent 或客户 VPS 软件。

`msboost purge` 要求在 TTY 中依次输入 `DELETE_MSBOOST` 和 `/opt/msboost`。检查真实目录、所有权标记、卷的 Compose 项目归属及是否仍被容器引用后，删除安装目录、本站启动入口及四个卷：

- `msboost_app_data`
- `msboost_database_data`
- `msboost_caddy_data`
- `msboost_caddy_config`

彻底清理后只能从独立离机备份恢复。不要用 `down -v` 或清空密钥代替重启或普通卸载。

## 3. Compose 配置与网络

Compose 默认运行预构建 MSBOOST、PostgreSQL 17 和 Caddy。仅 Caddy 的 80/443 发布到宿主机；数据库 5432 和服务 8080 不直接向公网开放。应用移除 Linux capabilities 并启用 `no-new-privileges`。

| 字段 | 用途 |
|---|---|
| `MSBOOST_VERSION` | 固定目标版本；v0.3.3 入口默认 `v0.3.3`，安装时仍核对 Release 资产与清单 |
| `MSBOOST_IMAGE` | v0.3.3 默认从 `ghcr.io/mozziexwz/node:v0.3.3` 获取，取得后固定 digest；归档方式记录独立本地标签 |
| `MSBOOST_DATABASE_NAME` | 默认 `msboost`；仅在离线恢复核验全新数据库后手工切换。升级快照也备份这个选定库，不改 PostgreSQL 初始数据库名 |
| `MSBOOST_IMAGE_ID` | 归档方式记录完整 `sha256:...` 镜像 ID，启动前与标签解析结果核对 |
| `POSTGRES_IMAGE` / `CADDY_IMAGE` | 首次取得后保存不可变 digest，普通应用升级保留它们 |
| `MSBOOST_DOMAIN` | 真实域名或临时调试 IPv4 |
| `MSBOOST_SITE_ADDRESS` | 域名模式为纯域名，调试模式为 `http://IPv4` |
| `PUBLIC_URL` | 浏览器实际完整源站，供 Origin 检查及业务链接/回调使用 |
| `COOKIE_SECURE` | HTTPS 为 `true`，临时 HTTP 为 `false` |
| `ADMIN_EMAIL` / `ADMIN_PASSWORD` | 首次管理员 |
| `POSTGRES_PASSWORD` | 独立随机数据库密码 |
| `MASTER_KEY` | 32 随机字节的 hex/base64，不得在已有数据上随意换钥 |

网络为 `172.30.86.0/24`，Caddy 固定 `172.30.86.2`，服务只信任 `TRUSTED_PROXY_CIDRS=172.30.86.2/32`。Caddy 仅对官方 Cloudflare IPv4/IPv6 网段解析访客 IP，并将单个已验证地址交给应用；直连伪造头被忽略。网络冲突时必须同时调整网络、Caddy 地址和应用可信地址，不得信任整个公网；增加其他代理时重新核验可信链。配置要求 Caddy ≥ 2.8，范围维护、Worker/Pseudo IPv4 限制及隔离验收见 [Cloudflare 客户端 IP 信任链](cloudflare-proxy.md)。

Compose 通过分离数据库字段构造 PostgreSQL URL。私有容器网络使用 `DATABASE_SSLMODE=disable`，外部数据库不可直接照抄。避免无意保留优先级更高的 `DATABASE_URL`。

应用卷保存私有文件/平台备份，数据库卷保存 PostgreSQL，其余两卷保存 Caddy 状态。安装器生成的主密钥在私有 `.env`，需要与卷数据分别备份。只保数据库却丢失主密钥，不能解密存储的凭据和配置。

自行维护 Compose 时，从 `.env.example` 建立私有配置，在已审核版本执行 `docker compose --env-file .env -f deploy/compose.yml pull` 和 `up -d --no-build`。手工 Compose 不会自动建立安装器标记、启动入口和快照流程，不应让 `msboost` 接管未登记目录或卷。

## 4. 控制执行机与节点

控制面不会自动充当 Executor 或节点。后台分别生成执行器令牌与节点注册令牌，在拟注册服务器执行固定版本的 [Agent 安装入口](agent-installation.md)：

```sh
curl --fail --location --proto '=https' --tlsv1.2 \
  https://raw.githubusercontent.com/mozziexwz/node/v0.3.3/agent.sh \
  -o /root/msboost-agent.sh
bash /root/msboost-agent.sh \
  --capability executor \
  --server https://panel.example.com \
  --version v0.3.3
```

节点改为 `--capability relay`，使用独立节点注册令牌。v0.3.3 全新 Relay 安装默认 `keep_last`；如确需兼容旧短租约，须显式传 `--offline-policy lease`。未持有 v2 私有身份的活动 lease 服务切换为 keep_last 会重启 Agent/GOST，必须在维护窗口追加 `--acknowledge-relay-restart`；已有 v2 身份普通重跑会在变更前拒绝，只有确认旧转发可断开且旧状态可丢弃时才能走[显式全新重置](agent-installation.md#控制面全新安装后节点仍显示离线)。默认隐藏终端输入令牌，不把秘密放进 URL 或命令参数。脚本从同版本 Release 下载 Agent、安装脚本与 SHA256 清单，校验后安装 systemd 服务；全新 keep_last 节点还须取得持久身份、首个受认证控制同步及有效控制 epoch 后才报告完成。网站升级不会自动更新或切换这些独立 Agent。

建议 Executor、节点分机部署。客户 VPS 是任务目标，不需安装整套网站。节点上线后先设置地址/端口范围，再建立隧道；心跳或监听 ACK 不等同于客户端到端连通。

免费工具在客户自有 VPS 执行“部署 MSBOOST”“配置中转服务器”或配置自备前置机前，先检查目标是否为 Debian 11 或更新版本；其他系统在改变 sysctl、下载或安装前拒绝，并提示从 VPS 服务商面板重装 Debian 11 或更新版本。网页在提交前另行提示“仅支持 Debian 系统部署”。「部署 MSBOOST」只执行受管组件的全新安装，重置该节点的受管配置、账号及端口，不执行系统 DD，也不提供“安全升级/修复”；这与上文仅针对**控制面网站**的 `msboost repair` 是不同操作。

通过平台检查后，免费工具会写入专用的 `/etc/sysctl.d/zz-msboost-bbr.conf`，执行该文件的 `sysctl -p`、`sysctl --system` 后再次应用该文件，并逐项核对全部 BBR/FQ 与 TCP 参数；重复执行保持幂等，不覆盖整份 `/etc/sysctl.conf`。仅在旧 `99-msboost-bbr.conf` 内容完全匹配且归 root 所有时移除旧文件。此步骤只属于这些客户自有目标 VPS，不用于控制执行机，也不用于本站捐赠权益节点。目标内核不支持或任一参数未生效时任务失败。

## 5. 首次业务验收与恢复

基础部署后，应在自有、可丢弃环境逐项记录版本、时间和结果：

1. HTTPS、登录、权限隔离、CSRF、真实 SMTP 投递；注册验证、免费工具验证、权益兑换验证三个策略分别验证。
2. Executor 领取任务、SSH 指纹、客户 VPS 全新安装与配置获取；DD 单独确认目标磁盘和备份，并核验重启后的系统。
3. 节点/隧道实际转发与计量、keep_last 管理失联保留、显式 lease 到期撤销、重启和到期清理、客户端认证。
4. 真实兑换渠道的金额、签名、重复/晚到回调及异常款；同步回跳不是到账凭证。
5. 加密平台备份、独立主密钥和隔离恢复；数据库快照不包含客户 VPS 整盘或浏览器保存的免费配置。

在线恢复要求维护模式、无运行任务及本站转发停止证明等条件，并先产生回滚备份。如果旧快照会回退后续枫叶、兑换码、兑换去重、权益或流量，服务拒绝直接覆盖，需保留当前数据离线对账。恢复后退出会话并撤销 Agent 令牌，需要重新接入。完整范围见 [安全模型与限制](security-and-limits.md)。

## 6. 本地开发和直接运行

开发使用 Go 1.27.1、Node.js 24 和 npm，依赖由 `go.sum` 与网页 lockfile 固定；构建命令见 [README](../README.md)。`node scripts/dev-server.mjs` 运行 `.runtime/msboost-server`（Windows 为 `.exe`），使用网页 `apps/web/dist`、`127.0.0.1:8080`、隔离数据 `.runtime/acceptance-data`；随机本地管理员保存在 `.runtime/acceptance-credentials.json`。脚本不负责构建，不读取生产 `.env`。

直接运行 `cmd/server` 时 Go 程序不自动读取 `.env`，需通过受限服务账户/服务管理器注入 `LISTEN_ADDR`、`PUBLIC_URL`、`DATA_DIR`、`WEB_DIR`、数据库字段、`MASTER_KEY` 和精确可信代理等环境。无数据库配置使用 `DATA_DIR/msboost.db`，无主密钥配置生成 `DATA_DIR/master.key`。TLS、目录权限、进程管理和备份需自行配置；`go run` 不是生产服务管理方案。
