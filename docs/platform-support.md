# 平台支持范围

| 用途 | Debian 11 | Debian 12 | Debian 13 |
|---|---|---|---|
| 控制面一键安装与 Docker Compose | 不支持 | 支持 | 支持 |
| 独立 Executor / Relay Agent | 可安装，需自行解决系统安全维护 | 支持 | 支持 |
| 客户 VPS 的免费 MSBOOST 部署 / 自备中转 / BBR | 部署、中转与定域清理已实机通过 | 支持 | 支持 |
| 面板「DD 系统」目标 | 固定重装 Debian 12 | 固定重装 Debian 12 | 固定重装 Debian 12 |

控制面安装器只接受版本代号匹配的 Debian 12 `bookworm` 或 Debian 13 `trixie`、amd64/arm64，并按当前系统版本选择 Docker 官方 apt 仓库。Docker [当前官方安装要求](https://docs.docker.com/engine/install/debian/)只列 Debian 12/13；控制面的容器运行系统与宿主版本可不同，不应手工把 Debian 13 的 apt 软件源写成 `bookworm`。

Debian 11 `bullseye` 已在 2026-08-31 [结束 Debian 官方 LTS](https://www.debian.org/News/2026/20260831)，默认不再获得 Debian 的安全更新。它仅用于用户自有 VPS 上的兼容性测试或有独立扩展安全维护的环境；公开生产控制面请使用 Debian 12/13。免费工具的成功检查包括真实服务启动、配置文件存在、BBR/FQ 参数与公网 TCP 可达性；这些检查不能代替游戏实际连接验收。

控制面版本与 Agent 版本独立。网站升级不会更改任何已注册执行机或中转节点的二进制；涉及执行机 BBR 行为的修复，必须按[Agent 安装与升级](agent-installation.md)逐台安装对应版本并确认在线。既有 Relay 策略迁移与服务重启会影响连接，不能把升级描述为无感。

v0.3.3 免费部署/自备中转在修改目标系统前要求 `/etc/os-release` 明确为 Debian 且版本至少 11；通过这个前置检查不等于更高的 Debian 版本已完成实机兼容验收。客户 VPS 工具仅执行受管组件全新安装，不执行系统 DD；面板的独立 DD 功能仍只重装 Debian 12。
