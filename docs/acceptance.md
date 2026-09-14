# 本地验收记录 · 2026-09-12

范围：下面先记录实现阶段的本机验收，随后补充 VPS 发布测试。没有连接或修改真实客户 VPS，没有真实付款或邮件投递。

## 已通过

- `go test -count=1 ./...`：身份、事务、并发邀请码/卡密、资金幂等、支付签名、任务门禁、SSH 指纹钉扎、DD 提交证据、配置解析、中转 ACK/租约/计量、附件权限、工单限制和备份恢复保护。
- `go vet ./...` 静态检查。
- GOST 官方 3.3.0 校验后的本机集成：真实双跳 TCP 转发、Observer 计数、规则撤销后端口关闭。显式设置 `GOST_TEST_BINARY` 运行，不以跳过测试作为通过。
- `npm --prefix apps/web run build`：TypeScript 检查与 Vite 生产构建。
- `npm --prefix apps/web test`：真实运行服务的 HTTP 验收，包括登录、CSRF、管理接口、SMTP 门槛、批量邀请码、文章、普通用户权限、卡密并发兑换、重复余额购买、工单及加密备份。
- 浏览器人工操作验收：空白登录表单与未勾选协议、真实管理员登录、后台主要页面加载、Markdown 编辑/对照/草稿保存；390px 移动端无整页横向溢出，菜单可展开和关闭。页面使用已确认的白橙色 UI 样式。
- Windows 本地服务编译；Linux amd64 与 arm64 的 Server / Agent 交叉编译。
- 原始节点脚本、固定重装脚本、站点管理与 Agent 安装脚本的 `bash -n`。

最初一轮通过实际浏览器操作完成；补充修改后，已另外通过项目无头浏览器脚本验证后台各页、导航、用户单位、弹窗边界、完整编辑器全屏、移动端和权限流程。全屏截图经人工查看。所有自动化用 `scripts/test-http.mjs` 创建全新临时数据，不修改已有预览数据。当前本地机器没有 Docker，容器验证在 GitHub Linux runner 执行。

## VPS 发布验证补充

- 附件正文与 13 张截图逐项核对，修改对应 [补充清单](requirements/supplement.md)。
- 新增真实 GOST TCP/TLS 双跳、错误证书拒绝，限购并发/跨渠道、邮箱门槛、卡密新请求/幂等重试、入口脱敏和工单/文章排序测试通过。
- 两套离线部署测试验证安装、修复、回退、保数据卸载、精确范围清理、GHCR/Release 预构建备用来源、SHA/imageID/架构/标签校验及紧凑 JSON 解析；不是实际 VPS 操作。
- 首轮 GitHub Linux race、静态检查、HTTP/浏览器/GOST/脚本测试通过；双架构镜像构建成功，PostgreSQL 和控制服务健康。Caddy 启动暴露了动态地址池冲突，因此 v0.1.0 的发布被阻止，未生成正式 Release。
- v0.1.1 的 [完整发布流水线](https://github.com/mozziexwz/node/actions/runs/34673089928) 已全部成功：Linux 自动化测试、双架构构建、真实 PostgreSQL/控制服务/Caddy Compose 启动、反向代理健康 JSON 与版本检查、两架构预构建归档导入与标签/架构/来源校验。
- [公开 Release v0.1.1](https://github.com/mozziexwz/node/releases/tag/v0.1.1) 已发布（源码提交 `cf6d6e8edb7313b0cc0bd32b2ae50b2c6c6dc6dc`）。部署包、安装器、两份 Agent 与两份镜像归档全部匿名下载，SHA-256 与清单及 GitHub 资产摘要一致；Agent ELF 架构正确。公开入口 `install.sh` 与该标签的 Git blob 完全相同。
- GHCR 匿名清单验证包含 `linux/amd64` 与 `linux/arm64`。实际 Compose 冒烟测试在 Linux amd64 runner 上运行，arm64 完成构建、导入和文件架构校验；没有声称执行过 arm64 真机业务或公网 ACME。

## v0.1.2 Debian 安装故障回归

用户实际 Debian 12 安装日志暴露 v0.1.1 缺陷：`require_platform` 加载 `/etc/os-release` 将发布 `VERSION` 覆盖为 `12 (bookworm)`。上述 v0.1.1 CI 直接运行 Compose，离线测试替换了平台检查，未覆盖这一入口；其通过不能证明 Debian 一键安装成功。

v0.1.2 隔离平台元数据，增加写入/使用发布版本前的校验、仅针对该错误且无业务容器/数据卷的恢复参数、私有配置备份和原子替换。Windows 本地 Go 测试、网页构建、安装器离线回归通过；恢复测试覆盖 Docker 检查失败/已有容器/已有卷/已有网络/缺失密钥/已有 imageID 拒绝、拉取失败不改配置、健康检查失败后 repair、密码与密钥原样保留。

- v0.1.2 [发布流水线](https://github.com/mozziexwz/node/actions/runs/34694803871) 全部通过。真实 Debian 12 容器读取原始 `/etc/os-release`，验证平台变量隔离及默认/显式版本的安装 CLI；仅安装动作被安全替换，不在该回归容器内执行 apt 或真实部署。
- Linux race/HTTP/浏览器/GOST/安装器回归通过；双架构镜像构建、amd64 上实际 Compose 数据库/控制服务/Caddy 健康启动和版本检查通过；两份镜像归档导入及架构校验通过。
- [Release v0.1.2](https://github.com/mozziexwz/node/releases/tag/v0.1.2) 于 2026-09-12 12:59:06 UTC 发布，源码提交 `c148dfffc1138880e5b7e7d49ea81bf570810c05`。六份载荷均已匿名下载，SHA-256 与清单及 GitHub 资产摘要一致；两架构 Agent ELF 检查通过。GHCR 匿名清单包含 linux/amd64 和 linux/arm64。
- 公开入口 `install.sh` 与审核源码一致，SHA-256 为 `fbd507692fdb993e8fe31f9210d6856804c97b2682a9f0065ac2868382d375de`。以上不代表已接入用户 VPS 执行恢复或通过该域名的公网 HTTPS 验收。

## v0.2.0 最终补充说明整改 · 本机回归

- 按最终 DOCX 正文与 9 张截图实施，来源摘要与逐项范围见 [补充清单](requirements/supplement.md)。先前未完成编辑的 DOCX 不作为依据。
- `go test -count=1 ./...` 与 `go vet ./...` 通过，覆盖新增 TOFU 持久钉扎和替换、任务结果白名单、清理预览约束/一次性执行、用户转发管理/全部规则逐跳改速率 ACK、安全恢复财务计量闭包与规则拓扑保留。
- 离线 SQLite 完整恢复、原损坏库保留、错密钥不建目标、已有目标拒绝、余额订单恢复、PostgreSQL 连接参数禁止覆盖目标、无效备份不能凑最少副本等测试通过。
- 网页 v0.2.0 生产构建通过；隔离 HTTP/模型测试 9 项和真实浏览器流程通过，验证默认公告、所有角色导航分组、无页面溢出、清理未预览无执行按钮、安全/离线恢复入口。恢复页与清理页截图已人工查看。
- 固定 GOST 3.3.0 真实 TCP/TLS 双跳、Observer、撤销、错误节点名与 CA 拒绝再次通过。
- 三套隔离引导/生命周期脚本回归通过，包括中文阶段、SHA 失败停止、Agent 子进程失败传播、令牌不输出、选定恢复库后的升级快照确实备份该库。无 apt/systemd/真实主机安装动作。
- 本机为 Windows，Linux UID/符号链接清理 fixture 与真实 PostgreSQL 新库导入测试交给 CI 的隔离 Linux/PostgreSQL 17 服务执行。本机没有 Docker；不能把本机跳过 Linux/PG 测试称为通过，实际远端证据见下节。

### v0.2.0 远端验证

- 发布代码 `3279cfe1e1fc3fef46a993f1a523e0f4cbc241cc` 的 [主分支完整 CI](https://github.com/mozziexwz/node/actions/runs/34713806670) 已通过。Linux race/vet、Debian 12 入口、HTTP/浏览器/GOST 和安装器回归均成功。
- 日志明确记录 `TestCleanupPythonIsolatedInventoryAndMutation`、`TestCleanupPythonMSBOOSTOwnershipAndRetainedData` 全部通过，含符号链接、未知布局、改动缓存、服务附加钩子、`Also` 和 `PropagatesStopTo` 拒绝；清理仅作用于隔离 fixture，不操作 runner 的真实服务。
- `TestOfflineRestorePostgresIntegration` 在真实 PostgreSQL 17 服务创建新库、导入并读回校验，同名数据库再次导入拒绝；不是 mock 或 skipped。测试 PostgreSQL 随 CI job 销毁，没有访问用户原库。
- 公开 v0.2.0 `install.sh` 已匿名下载，与发布标签的 Git blob 一致；SHA-256：`cc851f86f8c3e1198ccfc6e2c6db53479c4b88c1222e0fbcf461336a0aaae93c`。
- [v0.2.0 正式发布流水线](https://github.com/mozziexwz/node/actions/runs/34714000912) 的 verify、image、release 全部成功。两架构镜像构建完成；Linux amd64 runner 实际启动 PostgreSQL、控制服务及 Caddy，健康 JSON 和 v0.2.0 版本检查通过；两架构归档导入及来源/架构校验通过。没有声称 arm64 真机或公网 ACME 验收。
- [Release v0.2.0](https://github.com/mozziexwz/node/releases/tag/v0.2.0) 于 2026-09-12 19:31:04 UTC 公开发布，代码标签仍指向 `3279cfe1e1fc3fef46a993f1a523e0f4cbc241cc`，旧标签未覆盖。部署包、安装器、两份 Agent、两份独立恢复工具及两份预构建镜像归档均已匿名下载，8 份载荷 SHA-256 与清单及 GitHub 资产摘要一致；4 份程序 ELF 架构正确，部署包不含私有环境、备份、附件原稿或运行数据。
- GHCR 匿名清单包含 `linux/amd64` 与 `linux/arm64`，索引摘要为 `sha256:07100bfc472f7a60667621a3733a89ca2546edf42db693261f18bd93b3d16219`。以上均为隔离 CI / 公开分发验证，未操作用户 VPS 或真实付款。

## v0.2.0 时点尚未验收 / 未完成（历史）

- 真实 Debian/Ubuntu VPS 的部署与修复；可丢弃 VPS 的 Debian 12 DD；公网前置/中转/游戏认证路径。
- Linux 真机 Agent 安装、进程崩溃、网络分区及失联租约演练。
- PostgreSQL 容量压测、真实 Debian 12 主机安装、生产 DNS / 公网 ACME / TLS 验收（CI 的 HTTP Compose 启动已通过）。
- SMTP 服务商真实投递、真实 Turnstile 域名与商户付款回调。
- SFTP 真机传输、异机恢复、财务对账与灾难恢复演练。
- 规划中尚未交付的 UDP、FLVX 全量兼容、二维码/查单/退款、分表架构及高级备份等见 [安全与差距](security-and-limits.md)。

测试数据仅保存在被 Git 忽略的 `.runtime`；含随机本地管理员、测试用户/套餐/已兑换卡密、草稿与加密备份，不属于生产初始化数据。本地服务入口为 `http://127.0.0.1:8080`，重启方法见 [README](../README.md)。

## 2026-09-15 补充2.3 / Relay v2 本地整改

本节不是新的正式 Release，也不覆盖上文各历史版本当时的结论。

- 最新本机 Go 全包测试及静态检查、TypeScript/Vite 构建、HTTP 9 项和浏览器 34 项通过；新增 root 维护入口、恢复/终态历史、混合安装回滚和保护私有凭据的反例测试。
- 用户授权临时 Debian 12 上，固定校验真实 GOST 3.3.0、生产 Run 和三拓扑（单跳TCP、双跳TCP、三跳TLS）各5/30分钟全部通过。每个原TCP仅拨号1次、重连0次；30分钟各1799次回显。管理端为分阶段受控故障服务器，不是实际面板停机30分钟，不能代替24小时耐久。
- 真实 App 注册/命令ACK/备份门禁＋关HTTP监听5分钟、Linux原锁/root socket、真实PostgreSQL和原卷只读Compose检查均有独立执行入口。是否实际通过以 [整改状态](supplement-2.3-status.md#本轮实际执行证据2026-09-15) 最新记录为准；交叉编译或脚本存在不作为通过证据。
- 本轮没有重启生产控制面、部署公开新镜像、真实商户扣款或向真实客户邮箱发信。原有临时 VPS 转发服务保留，仅操作本轮隔离测试资源。

完整 T01–T26 分层映射及未测项见 [Relay v2 验收证据](relay-v2-verification.md)。完整手动/定时灾备与原TCP的联合验收、24小时耐久、真实QQ邮件渲染等未完成时，不能宣称“全部生产功能验收通过”。
