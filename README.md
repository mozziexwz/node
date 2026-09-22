# MSBOOST

MSBOOST 自托管控制面板：用户通过网页把 MSBOOST 脚本部署到自己的 VPS，管理员维护执行器、捐赠中转线路、权益与枫叶、兑换渠道及平台备份。系统重装使用固定版本的 [bin456789/reinstall](https://github.com/bin456789/reinstall) 脚本。

**v0.3.0 是否可安装，以[正式 Release](https://github.com/mozziexwz/node/releases/tag/v0.3.0)的资产、双架构镜像及 CI 成功状态为准，不能只看源码版本号。** 若该版本尚未完整发布，请继续使用已校验的 [v0.2.4](https://github.com/mozziexwz/node/releases/tag/v0.2.4)。生产控制面和现有 Agent 不会自动升级。历史已在授权临时 VPS 验证节点部署/修复、真实认证直连、自备中转与前置多跳，以及受管卸载，见[历史排查记录](docs/acceptance-vps-20260913.md)。尚未完成真实商户付款、真实游戏登录及千人容量生产验收。网页依据已确认 UI、[v4.1 产品规划](docs/requirements/product-v4.1.md)和[补充说明](docs/requirements/supplement.md)实现；规划不是全部功能的生产验收证明。安全边界见[安全模型与限制](docs/security-and-limits.md)。

v0.3.0 落实第三次最终版补充说明 2.4：密码统一为至少 8 字符并增加弱密码约束；会员端和后台统一“权益、兑换、枫叶、兑换码、兑换记录”用语；正常权益可限制为仅当前有效同权益用户续兑，体验权益每位用户终身仅一次；线路支持稳定排序、随机配置端口、双向流量及重置联动和一次性 TCP 路径诊断；增加 L1–L3 权益/线路准入、一键清理指定线路客户规则、工单未读铃铛及文章分类/图钉展示；免费工具仅在客户自有目标 VPS 应用规定的 BBR/FQ 参数。新安装的 Relay 默认使用 `keep_last`；显式 `--offline-policy lease` 仅作为旧链兼容。已有活动 lease 服务切换策略会重启 Agent/GOST，必须传入 `--acknowledge-relay-restart`。建议在全新 Debian 12 VPS 安装 v0.3.0，不自动改动生产站点或现有 Agent。详细状态见[补充说明 2.4 整改状态](docs/supplement-2.4-status.md)。

补充2.3的会员密码找回（允许未验证会员，禁止管理员）、管理员本机改密、邮件/文案，以及 Relay v2 离线保留和受保护恢复维护仍然保留。三拓扑真实 GOST 与真实控制面停站的 30 分钟耐久测试已满足本轮验收标准；实现、其他真实验证与未测边界见[补充2.3整改状态](docs/supplement-2.3-status.md)及[验收矩阵](docs/relay-v2-verification.md)，不宣称生产全功能无感验收完成。

v0.2.3 按[补充说明 2.2](docs/supplement-2.2-changes.md) 增加资讯、置顶/排序、新建草稿附件，补齐支付回跳反馈与回调地址说明，支持远程 SSH 密码备份和[一键整站灾难备份/全新目标恢复](docs/disaster-backup.md)。历史 VPS 验收记录对应之前版本，不替代本轮新增功能的独立回归。

## Debian 12 一键安装

v0.3.0 Release 资产实际发布后，在准备部署的全新 VPS 上以 **root** 执行，支持 amd64 / arm64。候选期不要运行尚不存在或资产不完整的标签；需要立即安装时，把下列固定入口改为已发布的 `v0.2.4/install.sh`：

```sh
apt-get update && apt-get install -y curl ca-certificates
curl -fsSL --proto '=https' --tlsv1.2 https://raw.githubusercontent.com/mozziexwz/node/v0.3.0/install.sh -o /root/msboost-install.sh
bash /root/msboost-install.sh
```

选择「安装」，输入已解析到该 VPS 的域名及管理员 QQ 邮箱。v0.3.0 发布后默认拉取预构建的 `ghcr.io/mozziexwz/node:v0.3.0`；若镜像仓库无法匿名访问，则从同版本 GitHub Release 下载、校验并导入预构建镜像。无需 VPS 安装 Go / Node 或编译源码。缺少 Docker 时使用官方 Debian 仓库安装 Engine 与 Compose 插件；Caddy 自动 HTTPS 需要域名解析正确、TCP 80/443 可达且未被其他服务占用。随机管理员密码保存在 root 私有的 `/opt/msboost/.env`，不输出到安装日志。仅在自己的私有终端查看：

```sh
sed -n '/^ADMIN_EMAIL=/p; /^ADMIN_PASSWORD=/p' /opt/msboost/.env
```

无域名时可选择 IP HTTP 临时界面调试模式（需要明确接受风险）；**真实在线任务、Agent、支付和敏感配置使用 HTTPS**。HTTP 不是安全业务部署方案，勿输入真实 SSH 凭据。

```sh
msboost                 # 交互菜单
msboost upgrade         # 获取最新正式 Release，先备份再升级
msboost repair          # 重建容器，保留数据库、管理员和主密钥
msboost status
msboost logs
msboost uninstall       # 停止网站并移除容器，保留全部数据
msboost purge           # 独立的双重确认彻底清理，谨慎执行
msboost disaster-config # 目录、保留天数、远端 SSH 密码与每日计划
msboost disaster-backup # 一键完整快照，默认 /root/msboost-backup
msboost disaster-disable # 停用整站自动备份，不删除备份
msboost admin-password  # 菜单12：本机root交互修改已有管理员密码
```

安装位置固定 `/opt/msboost`。`uninstall` 不删除数据卷、密钥或配置，之后可 `msboost repair` 恢复。`purge` 只删除本安装器拥有的目录和四个站点卷，不卸载 Docker、不清空其他项目、不卸载客户 VPS 或独立 Agent。完整说明见 [部署文档](docs/deployment.md)。

v0.1.1 如果安装报 `ghcr.io/mozziexwz/node:12 (bookworm)`，是安装器版本变量污染，**不要彻底清理**。使用 [保留配置的恢复步骤](docs/deployment.md#v011-首次安装报-12-bookworm-的保留配置恢复)，新版本会检查未产生业务数据后继续安装。

推荐在全新 Debian 12 VPS 安装 v0.3.0，并在隔离环境核对后再迁移业务。确需升级既有健康站点时，应等待正式 Release，在维护窗口执行 `msboost upgrade --version v0.3.0`；升级保留原 `.env`、管理员和主密钥，并在切换前备份当前选定数据库。已卸载保数据的站点先 `msboost repair` 再升级。网站升级不会自动更新独立执行机或节点，请按[Agent 安装与更新](docs/agent-installation.md)逐台安排；不要把网站版本变化误认为 Relay 策略已经切换。原库损坏时不要强行普通升级，按[独立新数据库恢复](docs/backup-recovery.md)校验快照并导入新库；持有整站包且使用全新 VPS 时，走[整站恢复入口](docs/disaster-backup.md#全新-vps-一键恢复)，不要先安装空站点。旧库、密钥及备份保留，恢复后保持维护并人工对账。

## 组成与职责

| 组件 | 位置 | 职责 |
|---|---|---|
| 网页 | `apps/web` | React / TypeScript / Vite，用户工具与管理后台；身份和业务状态来自服务端 |
| 控制服务 | `cmd/server`、`internal/control` | 会话、权限、任务调度、枫叶账本、权益、兑换通知、线路、内容及备份 |
| Executor | `cmd/agent --capability executor`、`internal/executor` | 在被授权的任务内通过 SSH 操作客户 VPS；不保存客户一次性 SSH 密码 |
| Relay Agent | `cmd/agent --capability relay`、`internal/relayruntime` | 在本站中转服务器管理固定 GOST 子进程、监听 ACK、租约与流量；没有 SSH / DD 执行能力 |
| 离线恢复 | `cmd/restore`、`internal/disaster` | 使用原密钥导入全新隔离数据库；整站工具处理私有归档/异地备份，全新站点切换需明确确认 |
| 数据与密钥 | SQLite 或 PostgreSQL、`DATA_DIR` | 事务业务状态、加密配置和备份；主密钥需单独保护 |
| 部署 | `deploy`、`Dockerfile` | Caddy HTTPS、控制服务、PostgreSQL；Agent 在选定机器上另行安装 |

当前数据库把业务状态保存在单行 JSON 中，通过 SQL 事务串行化更新。它尚不是按用户、订单、流水分表的规模化架构，也没有 1000 用户容量证据；不要根据支持 PostgreSQL 就推断多实例高可用或生产承载量。

## 本地启动

准备 Go **1.27.1**、Node.js **24** 和 npm。`go.mod` 的语言版本声明为 1.26.0，当前构建镜像与 CI 使用 Go 1.27.1。以下命令均从仓库根目录执行；不需要复制生产 `.env`。

Windows PowerShell：

```powershell
npm --prefix apps/web ci
npm --prefix apps/web run build
New-Item -ItemType Directory -Force .runtime | Out-Null
go build -trimpath -o .runtime/msboost-server.exe ./cmd/server
node scripts/dev-server.mjs
```

Linux / macOS：

```sh
npm --prefix apps/web ci
npm --prefix apps/web run build
mkdir -p .runtime
go build -trimpath -o .runtime/msboost-server ./cmd/server
node scripts/dev-server.mjs
```

访问 <http://127.0.0.1:8080>。首次运行会生成本地管理员凭据 `.runtime/acceptance-credentials.json` 和独立数据目录 `.runtime/acceptance-data`；在本机查看凭据登录，不要上传、截图传播或用于生产。关闭启动终端或按 Ctrl+C 停止服务。已有管理员的密码不会因重新启动自动重置。

开发脚本主动清除继承的数据库连接与主密钥变量，固定使用本机隔离数据目录，不会借用生产数据库。`.runtime` 已被 Git 忽略，但仍需保护本地文件权限。

该入口同时提供网页和 API，满足同源 Cookie / CSRF 要求。单独运行 `npm --prefix apps/web run dev` 会打开 Vite 5173，不能与固定 `PUBLIC_URL=8080` 的开发脚本直接混用登录写操作；需要热更新时见 [部署与开发说明](docs/deployment.md#开发热更新)。

## 检查与测试

```sh
go test ./...
go vet ./...
npm --prefix apps/web run build
```

保持上面的隔离开发服务运行，在另一个终端执行真实 HTTP 和浏览器测试：

```sh
npm --prefix apps/web test
npm --prefix apps/web run test:browser
```

这些测试会向本地测试数据写入账户、权益、兑换码、文章和备份，不可指向生产。浏览器测试在 Windows 使用已安装的 Chrome；Linux 需先在 `apps/web` 中执行 `npx playwright install chromium`，并具备浏览器系统依赖。真实 GOST 集成测试需要将 `GOST_TEST_BINARY` 设为已核验 SHA256 的 GOST 3.3.0 二进制绝对路径；未设置时对应测试明确跳过。Linux CI 另执行 `go test -race ./...`。这些命令和测试代码不等于一次已通过的生产验收记录。

## 部署与使用顺序

1. 阅读 [安全模型与限制](docs/security-and-limits.md)，准备域名、TLS、数据库与主密钥备份策略。
2. 按上述一键入口或 [部署说明](docs/deployment.md) 拉取固定版本镜像。Release 部署包、备用预构建镜像和 Agent 使用 `SHA256SUMS` 校验；两种预构建来源均失败时停止，不会静默改为源码构建。
3. 登录后台，先检查注册与工具开放开关，再配置真实 SMTP、兑换渠道和自有权益。初始数据库不包含可兑换的示例权益或示例线路。
4. 按 [Agent 安装说明](docs/agent-installation.md) 独立安装 Executor 与 Relay Agent，核对能力、令牌、地址、端口池和在线心跳。
5. 先在可丢弃 VPS 验证脚本安装、修复、重装和中转，再开放对应业务。DD 会擦除目标磁盘，必须由目标服务器所有者明确确认；任务发出或 API 健康不代表重装成功。

## 文档与来源

- [部署、配置与升级](docs/deployment.md)
- [Agent 安装与固定 GOST 校验值](docs/agent-installation.md)
- [身份与管理接口](docs/contracts-identity.md)、[任务接口](docs/contracts-tasks.md)、[商务与线路接口](docs/contracts-commerce.md)
- [安全模型、验收范围与差距](docs/security-and-limits.md)
- [补充说明 2.4 / v0.3.0 整改与验证状态](docs/supplement-2.4-status.md)
- [本次本地验收记录](docs/acceptance.md)
- [原始需求及导入校验](docs/requirements/source-manifest.json)、[第三方来源与许可说明](THIRD_PARTY_NOTICES.md)

用户提供的 MSBOOST 脚本原样保存在 `installers/node/msboost.sh`。重装脚本保留上游原件和固定资源提交的执行副本；来源、补丁与 SHA256 见 [reinstall 清单](installers/reinstall/manifest.json)。更新来源是维护操作，需审查差异并重新测试，不会随客户点击安装自动追踪上游 `main`。

## 许可

仓库所有者尚未指定本项目自有代码的分发许可，因此没有添加根 `LICENSE`，也不因仓库公开而表示获得任意复制、修改或再分发许可。第三方组件继续适用各自许可；reinstall 的 GPLv3 全文保存在 [installers/reinstall/LICENSE](installers/reinstall/LICENSE)。详见 [第三方说明](THIRD_PARTY_NOTICES.md)。
