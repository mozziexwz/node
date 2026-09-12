# 本地验收记录 · 2026-09-12

范围：本仓库 0.1 候选实现；Windows 本机隔离数据库与浏览器。没有连接或修改真实客户 VPS，没有真实付款、邮件投递或公网发布。

## 已通过

- `go test -count=1 ./...`：身份、事务、并发邀请码/卡密、资金幂等、支付签名、任务门禁、SSH 指纹钉扎、DD 提交证据、配置解析、中转 ACK/租约/计量、附件权限、工单限制和备份恢复保护。
- `go vet ./...` 静态检查。
- GOST 官方 3.3.0 校验后的本机集成：真实双跳 TCP 转发、Observer 计数、规则撤销后端口关闭。显式设置 `GOST_TEST_BINARY` 运行，不以跳过测试作为通过。
- `npm --prefix apps/web run build`：TypeScript 检查与 Vite 生产构建。
- `npm --prefix apps/web test`：真实运行服务的 HTTP 验收，包括登录、CSRF、管理接口、SMTP 门槛、批量邀请码、文章、普通用户权限、卡密并发兑换、重复余额购买、工单及加密备份。
- 浏览器人工操作验收：空白登录表单与未勾选协议、真实管理员登录、后台主要页面加载、Markdown 编辑/对照/草稿保存；390px 移动端无整页横向溢出，菜单可展开和关闭。页面使用已确认的白橙色 UI 样式。
- Windows 本地服务编译；Linux amd64 与 arm64 的 Server / Agent 交叉编译。
- 原始节点脚本、固定重装脚本、站点管理与 Agent 安装脚本的 `bash -n`。

自动化浏览器脚本 `apps/web/tests/acceptance.test.mjs` 已提供，但本轮没有声称运行该独立脚本；以上浏览器检查通过实际浏览器操作完成。Linux race 检查已加入 CI，本机未运行 Linux CI。当前机器没有 Docker，因此未声称 Compose 容器启动已验收。

## 尚未验收 / 未完成

- 真实 Debian/Ubuntu VPS 的部署与修复；可丢弃 VPS 的 Debian 12 DD；公网前置/中转/游戏认证路径。
- Linux 真机 Agent 安装、进程崩溃、网络分区及失联租约演练。
- PostgreSQL 容器集成与容量压测，Docker / Caddy / DNS / TLS 部署。
- SMTP 服务商真实投递、真实 Turnstile 域名与商户付款回调。
- SFTP 真机传输、异机恢复、财务对账与灾难恢复演练。
- 规划中尚未交付的 UDP、FLVX 全量兼容、二维码/查单/退款、分表架构及高级备份等见 [安全与差距](security-and-limits.md)。

测试数据仅保存在被 Git 忽略的 `.runtime`；含随机本地管理员、测试用户/套餐/已兑换卡密、草稿与加密备份，不属于生产初始化数据。本地服务入口为 `http://127.0.0.1:8080`，重启方法见 [README](../README.md)。
