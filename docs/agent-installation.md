# 控制执行机与节点 Agent 安装

后台的控制执行机对应 `executor`，节点对应 `relay`。两种能力使用独立令牌，建议分机部署。执行器负责经过授权的客户 VPS SSH 任务，节点承载本站隧道/转发；安装 Agent 不会把网站部署到客户 VPS。

默认目标为 Debian 12 amd64/arm64，要求 Linux、root 和 systemd 247+。控制面必须是可正常验证证书的 HTTPS 源站。临时 IP HTTP 网站仅适合界面调试，不能用于正式 Agent 注册和业务凭据传输。

## 1. 固定版本一键入口

先在后台创建控制执行机或生成节点注册令牌，然后在拟注册服务器的 root 终端执行后台提供的命令。等价的固定版本入口如下：

```sh
apt-get update
apt-get install -y ca-certificates curl tar
curl --fail --location --proto '=https' --tlsv1.2 \
  https://raw.githubusercontent.com/mozziexwz/node/v0.1.2/agent.sh \
  -o /root/msboost-agent.sh
bash /root/msboost-agent.sh \
  --capability executor \
  --server https://panel.example.com \
  --version v0.1.2
```

节点将能力改为 `--capability relay`。控制面地址必须是完整 HTTPS 源站，不含路径、查询或用户名密码。`agent.sh` 默认 `v0.1.2`，仅接受稳定的固定 `vX.Y.Z` 版本，不取得开发分支或 `latest` 运行程序。

脚本在真实终端显示隐藏输入提示，此时粘贴对应令牌并回车；令牌不会回显或进入命令行参数。节点注册令牌是短时单次用途，过期后在后台重新生成。执行器使用独立执行器令牌，二者不可混用。

无交互场景可以追加 `--token-file /root/msboost-token`。文件必须是 root 所有的普通文件、不可为符号链接，权限 `0600` 或更严格，内容仅为相应令牌。不要把令牌写进 shell 历史、URL、公开日志或 Git。交互输入的临时令牌文件由 bootstrap 退出时清理；自行提供的文件由操作者保管和清理。

### Release 下载与校验

`agent.sh` 从选定 GitHub Release 下载三个资产：

- `SHA256SUMS`
- `install-agent.sh`
- 匹配服务器的 `msboost-agent-linux-amd64` 或 `msboost-agent-linux-arm64`

通过 HTTPS 下载后，安装脚本与二进制分别核对清单哈希，匹配才执行。Release/资产不可用、哈希缺失/重复/不匹配、架构不支持时明确停止，不自动现场编译。哈希清单与资产共享 GitHub HTTPS/Release 信任边界，不等于独立发布签名。

这里说明的是当前安装协议；某版本是否已发布，以相应 Release 资产实际可用性为准，不预先声明真实客户业务完成验收。

## 2. 安装内容和失败恢复

经校验的 `install-agent.sh` 再次核验明确传入的 Agent SHA256、Linux ELF、本机架构、令牌文件权限及 systemd 版本。它只更新带 MSBOOST 所有权标记的既有安装；未登记文件/目录和符号链接会被拒绝。

| 路径 / 服务 | 用途 |
|---|---|
| `/usr/local/bin/msboost-agent` | Agent 程序 |
| `/usr/local/libexec/msboost-agent/managed-v1` | 安装器所有权标记 |
| `/etc/msboost-executor.env` | root-only 执行器地址和令牌 |
| `/etc/msboost-relay.env` | root-only 节点地址和注册信息 |
| `msboost-executor.service` | 控制执行机服务 |
| `msboost-relay.service` | 节点服务 |
| `/var/lib/msboost-relay` | 节点规则状态和注册后持久化令牌 |
| `/var/backups/msboost-agent/能力.随机后缀/` | 私有安装前程序、环境和服务配置备份 |

服务采用动态受限用户、私有临时目录、只读系统目录、`NoNewPrivileges` 与受限 capabilities；节点另有监听所需权限。执行器客户 SSH 凭据和免费配置不写进节点持久目录。

安装先保存旧文件/服务状态，再更新和重启。服务启动失败时尝试恢复原程序、私有环境、systemd unit、启用状态和之前的运行状态，并保留备份。重跑前应确认使用的匹配令牌仍有效；不要用已消费节点注册令牌进行一次新的注册。

## 3. 固定 GOST 版本

节点安装下载固定 **GOST v3.3.0**，按安装器内固定哈希校验后执行。执行器环境保存两种架构的固定地址/哈希，客户自备中转任务在目标机再次核验匹配资产。

| 架构 | 上游资产 | 安装器固定 SHA256 |
|---|---|---|
| Linux amd64 | `gost_3.3.0_linux_amd64.tar.gz` | `676fb7f78d267b6ae73df719c0c7f2b565dde7147da935cfafbc1e1da558b6d5` |
| Linux arm64 | `gost_3.3.0_linux_arm64.tar.gz` | `d03699e3f385d4ff5dad68046712adfcc7515325a064d2ab046e0bece30f8f8f` |

来源为 [GOST v3.3.0 Release](https://github.com/go-gost/gost/releases/tag/v3.3.0)。安装器仅接受 `--gost-version 3.3.0`，不使用 `latest` 下载。

## 4. 接入和核验

```sh
# 控制执行机
systemctl status msboost-executor
journalctl -u msboost-executor --since '10 minutes ago' --no-pager

# 节点
systemctl status msboost-relay
journalctl -u msboost-relay --since '10 minutes ago' --no-pager
```

随后检查后台心跳。节点先配置真实公网地址、允许监听范围与实际出站地址，再建立隧道、选择入口/中间节点/出口。防火墙只开放业务需要的端口；NAT/多网卡环境应核验登记的跳间来源与真实出站一致。

本地服务 `active`、心跳或监听 ACK 仅说明对应阶段正常，不证明公网游戏路径、多跳计量或 DD 重装成功。开放业务前需在自有可丢弃 VPS 单独验证安装/修复/DD、断网租约失效、Agent 重启、客户端认证和实际计量，记录结果。

网站的 `msboost uninstall`/`purge` 不处理独立 Agent 或客户 VPS 软件；停止控制面后托管转发依照租约失效机制撤销。Agent 运维与控制面数据清理是分开的操作。

## 5. 显式源码或离线安装

默认使用 Release 入口。需要自己审核构建或离线传输时，可以从确定版本构建，将程序和同版本 `deploy/install-agent.sh` 安全复制到目标机，再明确提供哈希与私有令牌文件：

```sh
mkdir -p dist
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -o dist/msboost-agent-linux-amd64 ./cmd/agent
sha256sum dist/msboost-agent-linux-amd64

sudo bash deploy/install-agent.sh \
  --capability executor \
  --server https://panel.example.com \
  --agent /root/msboost-agent-linux-amd64 \
  --agent-sha256 "$REVIEWED_AGENT_SHA256" \
  --token-file /root/msboost-executor-token \
  --gost-version 3.3.0
```

将变量替换为审核过的真实哈希；ARM64 使用 `GOARCH=arm64` 和对应文件名。这是明确选择的运维方式，不是 Release 下载失败后的自动回退。节点安装仍需访问固定 GOST 资产，除非另行审核并实现离线分发。
