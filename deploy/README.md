# Debian 12/13 控制面部署管理契约

入口为仓库根 `install.sh`，部署包由 GitHub Release 提供：

- 仓库：`mozziexwz/node`，当前入口默认版本 `v0.3.1`（正式资产发布前不可运行这个标签）。
- 文件：`msboost-deploy-v0.3.1.tar.gz`、`SHA256SUMS`。部署包顶层直接包含 `install.sh`、`deploy/`；显式源码构建还需完整应用源码与 Dockerfile。
- 普通安装优先拉取 `ghcr.io/mozziexwz/node:v0.3.1`，拉取成功后把实际镜像摘要记入 `.env`。GHCR 无权读取或暂时不可用时，明确提示并从同版本 GitHub Release 下载 `msboost-image-linux-amd64.tar.gz` 或 `msboost-image-linux-arm64.tar.gz` 与校验清单，载入 CI 预构建镜像；两个预构建来源都失败才停止，绝不自动源码编译。PostgreSQL / Caddy 拉取失败直接停止，不触发应用镜像回退。
- SHA256 清单与部署包来自同一 HTTPS Release，提供完整性检查，不是独立发布签名。运行 root 脚本前应审核来源。

在目标 Debian 12/13（amd64 或 arm64）服务器运行，不需要自己的电脑拥有公网 IP。域名模式要求域名 DNS 指向目标 VPS，且公网 TCP 80/443 可达；脚本不会停止其他占用端口的服务。

```sh
curl -fsSL https://raw.githubusercontent.com/mozziexwz/node/v0.3.1/install.sh -o /tmp/msboost-install.sh
# 审核下载的脚本后执行；无参数显示菜单。
sudo bash /tmp/msboost-install.sh install --domain panel.example.com --email 12345678@qq.com
```

以上域名/邮箱需要替换为自己的真实信息。IP 调试使用 `--ip VPS的IPv4 --allow-insecure-http`，只有提供明确风险标志或手动输入 `HTTP_RISK` 才会开启。HTTP 会明文传输会话、密码和配置，只适用于临时隔离 UI 调试；生产必须 HTTPS。Executor / Relay Agent 仍要求 HTTPS（本机 loopback 开发除外），不能把公开 HTTP 调试站点当作真实 SSH 执行平台。

脚本在 `/opt/msboost` 创建 root-only `.env`，为管理员密码、数据库密码和主密钥分别生成独立随机值。密码不输出到安装日志；使用 root 安全读取 `.env`，并将密钥与备份另行离机保管。不要把该文件提交到 Git、粘贴工单或分享 `docker compose config` 的完整结果。安装成功只代表容器和本机 Caddy/证书检查通过，不代表客户 VPS、支付、SMTP 或游戏链路已验收。

## 生命周期

安装后可使用 `sudo msboost` 菜单，或这些命令：

```sh
sudo msboost status
sudo msboost logs
sudo msboost upgrade --version v0.3.1
sudo msboost upgrade             # 解析 GitHub 最新正式 Release 的具体 tag
sudo msboost repair
sudo msboost uninstall          # 保留数据、配置、密钥、证书与备份
sudo msboost purge              # 单独操作，必须在终端进行两次确认
```

升级在应用新配置前保存私有 `.env`/部署文件和 PostgreSQL `pg_dump` 一致性快照到 `/opt/msboost/backups/`。健康检查失败会尝试恢复旧镜像摘要和旧配置，并返回失败；不会自动覆盖业务数据库，也不会把数据库迁移自动倒退。旧版本无法读取已迁移数据时必须人工处理，快照应离机备份。不要在有未结束 DD 或 SSH 任务时升级：服务重启会使内存一次性凭据和免费配置丢失，任务不会自动重放。

`repair` 只恢复当前部署，不重建管理员、不生成新主密钥、不清空数据库。GHCR 不可用时允许恢复同版本 Release 预构建归档，但载入的 imageID 必须等于原记录。归档镜像使用唯一 `msboost-release:` 本地标签，完整 ID 保存在 `MSBOOST_IMAGE_ID`，每次启动前都核验；本机归档镜像丢失可重下载，原 ID 也丢失则拒绝猜测恢复。显式源码构建的应用镜像使用本机保留副本。应用与依赖镜像摘要固定后，repair 不会因标签变化悄悄换版本。临时下载位于 `.stage.随机/image-cache`，操作退出清理，不保留归档副本。应用 upgrade 默认保留已有 PostgreSQL / Caddy 摘要；需要升级它们时，应先完成离机备份，再人工将 `.env` 的 `POSTGRES_IMAGE` / `CADDY_IMAGE` 改为经过审核的兼容版本，并进行独立验收，尤其不能直接跨 PostgreSQL 主版本换镜像。

卸载只执行本项目 Compose `down`，不删除任何数据卷。彻底清理要求依次输入 `DELETE_MSBOOST` 与 `/opt/msboost`，检查项目归属后仅删除四个固定卷：`msboost_app_data`、`msboost_database_data`、`msboost_caddy_data`、`msboost_caddy_config`，以及该安装目录和安装器自己的命令入口。清理不可撤销，只能从独立备份恢复。不会调用全局 prune，不卸载 Docker、不删除共享镜像，也不触碰其他项目、客户 VPS 或独立 Agent。Docker 的官方 apt 仓库配置随 Docker 保留。

## 显式源码构建

只有预构建镜像不可用且操作者明确愿意承担构建成本时使用：

```sh
sudo bash /tmp/msboost-install.sh install --domain panel.example.com --email 12345678@qq.com --build
sudo msboost upgrade --version v0.3.1 --build
```

这仍需要 Release 部署包包含完整源码。安装器使用唯一的本地镜像标签，避免覆盖供回退使用的旧镜像。普通 `compose.yml` 没有 build 字段；手工源码构建必须额外指定 `compose.build.yml`。

## 离线测试

```sh
for f in install.sh deploy/*.sh; do bash -n "$f"; done
bash deploy/manage_test.sh
bash deploy/bootstrap_test.sh
```

测试把 Docker、网络下载和系统写入替换成 mock，只在项目 `.cache` 创建临时文件。它验证安装/修复/升级回退/卸载/安全清理、镜像校验和 HTTP 风险确认；没有冒充真实 Debian、ACME 或 GHCR 端到端验收。Git Bash 下跳过无法表达的 Unix mode 操作，Linux CI 使用真实临时文件权限命令。

Docker 官方仓库安装方式依据 [Docker Debian 安装文档](https://docs.docker.com/engine/install/debian/)；注意 Docker 发布的端口可能绕过 UFW 常规规则，应使用云防火墙及 Docker 支持的网络规则限制暴露面。
