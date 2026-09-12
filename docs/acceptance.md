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

## 尚未验收 / 未完成

- 真实 Debian/Ubuntu VPS 的部署与修复；可丢弃 VPS 的 Debian 12 DD；公网前置/中转/游戏认证路径。
- Linux 真机 Agent 安装、进程崩溃、网络分区及失联租约演练。
- PostgreSQL 容量压测、真实 Debian 12 主机安装、生产 DNS / 公网 ACME / TLS 验收（CI 的 HTTP Compose 启动已通过）。
- SMTP 服务商真实投递、真实 Turnstile 域名与商户付款回调。
- SFTP 真机传输、异机恢复、财务对账与灾难恢复演练。
- 规划中尚未交付的 UDP、FLVX 全量兼容、二维码/查单/退款、分表架构及高级备份等见 [安全与差距](security-and-limits.md)。

测试数据仅保存在被 Git 忽略的 `.runtime`；含随机本地管理员、测试用户/套餐/已兑换卡密、草稿与加密备份，不属于生产初始化数据。本地服务入口为 `http://127.0.0.1:8080`，重启方法见 [README](../README.md)。
