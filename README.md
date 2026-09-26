# MSBOOST

MSBOOST 是自托管控制面板。会员在网页上将游戏节点部署到自己的 VPS，配置自备中转或使用捐赠权益转发；管理员管理执行机、节点、权益、兑换、工单和备份。系统 DD 使用固定版本的 [bin456789/reinstall](https://github.com/bin456789/reinstall) 脚本。

当前版本：**v1.0.0**。控制面支持 Debian 12/13；客户 VPS 免费部署工具支持 Debian 11/12/13；网站 DD 功能重装为 Debian 12。部署前请阅读[平台支持范围](docs/platform-support.md)和[安全边界](docs/security-and-limits.md)。

## 一键安装

在全新 Debian 12/13 VPS 上，先将域名解析到服务器、开放 TCP 80/443，再以 root 在私有终端执行固定版本入口：

```sh
apt-get update && apt-get install -y curl ca-certificates
curl -fsSL --proto '=https' --tlsv1.2 https://raw.githubusercontent.com/mozziexwz/node/v1.0.0/install.sh -o /root/msboost-install.sh
bash /root/msboost-install.sh
```

安装器优先拉取 `ghcr.io/mozziexwz/node:v1.0.0` 预构建镜像；GHCR 不可用时下载并校验同版本 [Release](https://github.com/mozziexwz/node/releases/tag/v1.0.0) 的预构建归档。两者均失败则停止，不在 VPS 静默编译。安装前应核对 Release 的 `SHA256SUMS` 与目标架构。Caddy 自动签发 HTTPS 证书。初始管理员密码保存在仅 root 可读的 `/opt/msboost/.env`，不会写入安装日志；在私有终端查看：

```sh
sed -n '/^ADMIN_EMAIL=/p; /^ADMIN_PASSWORD=/p' /opt/msboost/.env
```

无域名的 IP HTTP 模式只供临时调试，不应用于真实 SSH 凭据、Agent、支付或生产业务。网站升级不会自动升级独立 Agent；请逐台按[Agent 安装文档](docs/agent-installation.md)维护。

```sh
msboost                 # 交互菜单
msboost upgrade         # 备份后升级
msboost repair          # 保留数据库、管理员与主密钥，重建服务
msboost status
msboost logs
msboost uninstall       # 停站、移除容器，保留数据
msboost purge           # 单独双重确认的彻底清理
msboost admin-password  # 本机 root 交互修改管理员密码
```

`uninstall` 不删除数据卷和配置；`purge` 仅清理本安装器管理的站点数据，不处理客户 VPS 或独立 Agent。旧站升级前先做离机备份，在维护窗口运行 `msboost upgrade --version v1.0.0`，并单独安排 Agent 更新。完整操作见[部署说明](docs/deployment.md)、[备份与恢复](docs/backup-recovery.md)。

## 组成

| 组件 | 位置 | 职责 |
|---|---|---|
| 网页 | `apps/web` | React / TypeScript / Vite，会员工具与管理后台 |
| 控制服务 | `cmd/server`、`internal/control` | 会话、权限、任务、权益、转发和备份 |
| Executor | `cmd/agent --capability executor`、`internal/executor` | 受限 SSH 任务执行，不持久保存客户一次性 SSH 密码 |
| Relay Agent | `cmd/agent --capability relay`、`internal/relayruntime` | 管理受管 GOST 转发进程，不具备 SSH/DD 执行能力 |
| 数据与部署 | `deploy`、`Dockerfile` | PostgreSQL、Caddy HTTPS、镜像和安装生命周期 |

数据库当前以事务串行化更新业务状态，**没有千人容量或多实例高可用证据**。正式使用前应自行验证所需游戏线路、支付渠道、邮件和故障恢复；发布版本不等于每个第三方环境均完成验收。

## 本地开发与测试

需要 Go 1.27.1、Node.js 24 和 npm。在仓库根目录执行：

```sh
npm --prefix apps/web ci
npm --prefix apps/web run build
go test ./...
go vet ./...
```

本地隔离服务：

```sh
node scripts/dev-server.mjs
```

访问 `http://127.0.0.1:8080`。开发凭据生成于忽略目录 `.runtime/acceptance-credentials.json`，不得用于生产或提交到仓库。另见[部署与开发说明](docs/deployment.md#开发热更新)。

## 文档

- [部署与升级](docs/deployment.md)、[Agent 安装](docs/agent-installation.md)、[平台支持](docs/platform-support.md)
- [安全模型与限制](docs/security-and-limits.md)、[Relay 离线与恢复](docs/relay-v2-contract.md)、[受信恢复操作](docs/relay-recovery.md)
- [备份与恢复](docs/backup-recovery.md)、[整站灾备](docs/disaster-backup.md)
- [身份接口](docs/contracts-identity.md)、[任务接口](docs/contracts-tasks.md)、[商务与线路接口](docs/contracts-commerce.md)
- [v1.0.0 发布说明](docs/release-notes.md)、[第三方来源与许可](THIRD_PARTY_NOTICES.md)

用户提供的 MSBOOST 脚本保存在 `installers/node/msboost.sh`；DD 脚本使用审查过的固定上游提交，见 [reinstall 清单](installers/reinstall/manifest.json)。不会随客户点击自动追踪上游 `main`。

## 许可

仓库所有者尚未指定本项目自有代码的分发许可；仓库公开不等于授予任意复制、修改或再分发许可。第三方组件适用各自许可，详见[第三方说明](THIRD_PARTY_NOTICES.md)。
