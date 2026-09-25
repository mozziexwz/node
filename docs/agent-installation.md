# 控制执行机与节点 Agent 安装

后台的控制执行机对应 `executor`，节点对应 `relay`。两种能力使用独立令牌，建议分机部署。执行器负责经过授权的客户 VPS SSH 任务，节点承载本站隧道/转发；安装 Agent 不会把网站部署到客户 VPS。

Agent 目标为 Debian 11/12/13 amd64/arm64，要求 Linux、root 和 systemd 247+；其中 Debian 11 已结束官方 LTS，公开生产节点建议 Debian 12/13。控制面安装本身只支持 Debian 12/13，详见[平台支持范围](platform-support.md)。控制面必须是可正常验证证书的 HTTPS 源站。临时 IP HTTP 网站仅适合界面调试，不能用于正式 Agent 注册和业务凭据传输。

下面的 v0.3.3 固定入口已对应[正式 Release](https://github.com/mozziexwz/node/releases/tag/v0.3.3)和全绿的[标签 CI](https://github.com/mozziexwz/node/actions/runs/36163149523)；安装前仍须核对该版本资产与 `SHA256SUMS`，不能仅凭源码版本号判断。公开制品、真实 VPS 结果及未覆盖项见[正式发布后验收记录](acceptance-vps-20260926.md)。网站升级不会自动升级、重启或改变任何独立 Agent；执行机行为修复必须升级执行机 Agent 才能生效。

## 1. 固定版本一键入口

先在后台创建控制执行机或生成节点注册令牌，然后在拟注册服务器的 root 终端执行后台提供的命令。等价的固定版本入口如下：

```sh
apt-get update
apt-get install -y ca-certificates curl tar
curl --fail --location --proto '=https' --tlsv1.2 \
  https://raw.githubusercontent.com/mozziexwz/node/v0.3.3/agent.sh \
  -o /root/msboost-agent.sh
bash /root/msboost-agent.sh \
  --capability executor \
  --server https://panel.example.com \
  --version v0.3.3
```

节点将能力改为 `--capability relay`。控制面地址必须是完整 HTTPS 源站，不含路径、查询或用户名密码。v0.3.3 的 `agent.sh` 默认固定 `v0.3.3`；仅接受固定 `vX.Y.Z` 版本，不取得开发分支或 `latest` 运行程序。v0.3.3 已发布，但安装前仍须核对所选版本的 Release 资产和摘要；其他版本号不能自动视为已发布。

脚本在真实终端显示隐藏输入提示，此时粘贴对应令牌并回车；令牌不会回显或进入命令行参数。节点注册令牌是短时单次用途，过期后在后台重新生成。执行器使用独立执行器令牌，二者不可混用。

无交互场景可以追加 `--token-file /root/msboost-token`。文件必须是 root 所有的普通文件、不可为符号链接，权限 `0600` 或更严格，内容仅为相应令牌。不要把令牌写进 shell 历史、URL、公开日志或 Git。交互输入的临时令牌文件由 bootstrap 退出时清理；自行提供的文件由操作者保管和清理。

### Release 下载与校验

`agent.sh` 从选定 GitHub Release 下载三个资产：

- `SHA256SUMS`
- `install-agent.sh`
- 匹配服务器的 `msboost-agent-linux-amd64` 或 `msboost-agent-linux-arm64`

通过 HTTPS 下载后，安装脚本与二进制分别核对清单哈希，匹配才执行。Release/资产不可用、哈希缺失/重复/不匹配、架构不支持时明确停止，不自动现场编译。哈希清单与资产共享 GitHub HTTPS/Release 信任边界，不等于独立发布签名。

新版 Agent 入口仅允许 **v0.2.4 或更新**的固定稳定版本，在下载、读取令牌和系统操作前拒绝旧版本；旧安装器缺少共享程序锁及v2状态准入，不能靠让它先运行来自检兼容。这只限制新版入口，不影响网站或灾备的历史精确版本恢复，也不能阻止root主动运行历史脚本；已有v2节点不得这样绕过保护。显式 lease 仍受兼容，但 v0.3.0 全新 Relay 安装默认 keep_last。

这里说明的是当前安装协议。v0.3.3 的 Release 已公开，但摘要验证和节点实际在线/转发核对仍不可省略；本版验收不覆盖真实游戏协议、实际支付/邮件、arm64 真机运行及容量上限，详见[验收记录](acceptance-vps-20260926.md)。

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

安装先保存旧文件/服务状态，再更新和重启。服务启动失败时尝试恢复原程序、私有环境、systemd unit、启用状态和之前的运行状态，并保留备份。Relay 节点若已有 v2 身份，v0.3.3 普通安装会在变更前拒绝；仅在明确允许旧转发断开并归档旧状态时使用下述全新重置。不要用已消费节点注册令牌进行一次新的注册。

### 升级既有 Agent

**升级网站容器不会自动更新其他 VPS 上的 Agent。** v0.2.2 修复了 Debian `/run` 禁止执行导致的中转失败、GOST 凭据文件名缺少后缀以及 DD 缺少非交互用户名。必须更新执行机才能生效，仅更新网页无效。新诊断和清理对新版 GOST 凭据路径的兼容也需要新版执行机。同机两种能力共享 `/usr/local/bin/msboost-agent`，应在同一维护窗口更新并核验两个服务。

1. 网站开启维护模式，暂停新任务和规则。等待所有已接受的 `queued` / `running` 执行任务结束，再操作执行机。**不要在任务仍运行时禁用执行机、重置令牌或重启服务**：可能导致结果无法回传，已交付的 DD/清理不会被撤回。`unknown` / `interrupted` 任务先从 VPS 控制台核实，不能用升级代替结果确认。
2. 确认控制面域名不变，私密备份 Agent 环境与安装备份；更新节点还需保留整个 `/var/lib/msboost-relay`，特别是 `relay-token.json` 和流量日志。节点更新会中断其连接，需安排维护窗口。
3. v0.3.3 正式资产已公开；核对 Release 与清单后，使用上方固定 **`v0.3.3` 入口**更新执行机。可从 `/etc/msboost-executor.env` 取其专用令牌，放入仅含令牌的 `0600` 私有文件。**完整 `.env` 不能直接作为 `--token-file`**，也不要 `source` 或公开打印环境文件。执行任务必须先排空。未持有 v2 私有身份的活动 lease 节点若切换 keep_last，仍需明确 `--offline-policy keep_last --acknowledge-relay-restart`，且会中断原连接；已有 v2 身份则受下一条的拒绝边界约束。
4. **不要将已有 keep_last 节点的普通重跑当成升级。** v0.3.3 安装器发现旧 v2 身份时，会在停止服务前拒绝：本地持久 `managementToken`/`agentId` 会优先于新粘贴的注册令牌。需要保留旧身份、端口和业务的节点走[受信中转恢复](relay-recovery.md)或另行维护验证；本版没有无损原地重装入口。只有旧状态可丢弃且旧转发全部允许中断时，才执行下节的双重确认全新重置。不要手动删除所有权标记、状态或令牌。
5. 安装器校验 Release，并对新 Relay 等待注册与首个经过认证的 v2 控制同步，确认私有状态写入新的控制 epoch 且未进入恢复核对，才报告完成。随后仍要检查后台在线心跳和实际转发。同机另一个能力也须在确认无执行任务后重启并核验。最后退出维护，以无破坏性任务验证交付；不要把 DD 或真实清理当作自动升级自测。

目前没有自动排空任务或版本协商机制，维护、等待和在线核验需管理员执行。本机 `active` 不等于已连通控制面；安装器回滚文件也不等于回滚客户 VPS 上已经执行的操作。

### 控制面全新安装后节点仍显示离线

旧 Relay 节点的 `/var/lib/msboost-relay` 私有状态包含原控制面管理身份。即使本机 `msboost-relay.service` 显示 `active`，在新控制面粘贴新注册令牌并普通重跑安装器也不会自动替换该身份；应先检查后台是否存在对应节点、`journalctl -u msboost-relay` 的脱敏报错和控制面 HTTPS 连通性。不要把 systemd 的 `active` 当作注册成功，也不要公开令牌或直接删除私有状态。

如果确认旧控制面/旧转发不再保留，且允许当前节点上的所有转发断开，可在新后台创建**未注册、无关联业务**的新节点记录并领取一次性注册令牌，使用上方 **v0.3.3** 固定入口，并在 relay 安装命令追加 `--fresh-reset --acknowledge-relay-restart`。已确认 v2 身份或被线路、用户中转引用的现有记录禁止普通令牌轮换；不能靠重装绕过，应先走[受信中转恢复](relay-recovery.md)并核对旧业务。必须保持 `--offline-policy keep_last`；已是默认值，但也可明确写出。安装器会核验旧 Agent 所有权、受管单元与状态目录内容，将旧状态归档到 root 私有安装前备份，失败时尝试回滚；不接受外来文件、任意符号链接或未受管安装。新令牌仍通过隐藏输入或权限 `0600` 的私有文件提供，不作为命令行参数。若现有控制面已将该 v2 节点列为 `recovery_required`，全新重置不能自动解除冻结，应先按受信恢复流程处理；否则安装器会因无法完成同步而失败。重注册后务必确认后台节点在线、规则重新下发并实际测试转发。此流程会中断旧连接，不能用作无感升级，也不会在普通安装中自动执行。

## 3. Relay 策略与固定 GOST 版本

### v0.3.0：新装默认 keep_last

v0.3.0 的全新 `--capability relay` 安装在未指定策略时选择 `keep_last`。如某条旧链必须继续短租约行为，应显式传 `--offline-policy lease`；兼容不等于推荐新装继续使用旧策略。v0.2.4 的历史行为是默认 lease、显式选择 keep_last，既有服务不会因为网站或安装入口版本变化而自行迁移。

对已经运行的 Relay，任何需要重启 Agent/GOST 的安装或策略切换都可能中断原连接。未持有 v2 私有身份的活动 lease 服务迁移到 keep_last 时必须逐次传 `--offline-policy keep_last --acknowledge-relay-restart`，明确承认这次维护风险；缺少确认时安装器在停止旧服务前拒绝。已有 v2 身份默认会在变更前拒绝，不能只追加重启确认来绕过。建议优先准备全新 Debian 12 节点并完整验收，再安排旧节点维护迁移，而不是把版本升级描述为无感切换。

按完整线路迁移并等待所有节点实际确认 keep_last/当前配置/停止语义后，再开启严格整站自动备份。网站升级不自动更新节点。原 v2 状态存在时拒绝静默降级；executor 和 relay 共用程序，也不能通过安装旧 executor 覆盖 v2 程序。首次迁移失败且已生成 v2 状态时，不自动启动旧 lease 程序回滚，保留兼容文件、停用失败服务和私有备份供修复；不删除状态或恢复旧库。

受保护本机换管理凭据及灾难恢复接管见[受信中转恢复](relay-recovery.md)，不要用会重启 Agent 的安装命令代替热重载。

Debian 12 / systemd 252 的 DynamicUser 在服务内部也保留 `/var/lib/msboost-relay` 符号链接。因此 keep_last 单元直接使用同一数据的真实路径 `/var/lib/private/msboost-relay`；保留 StateDirectory、0700、动态用户和所有符号链接/属主安全检查。普通安装不会搬移或清空旧数据；仅上述显式全新重置会在停机后将受管状态归档到 root 私有备份。显式 lease 仍使用兼容路径。root 恢复入口仍可使用受保护的公共目录别名。不要自行修改父目录权限、跟随任意链接或 chown 现有状态；其他 systemd 版本尤其 ID-map 挂载的适配需另行验收，不以 nobody UID 作通配放行。

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

本地服务 `active`、心跳或监听 ACK 仅说明对应阶段正常，不证明公网游戏路径、多跳计量或 DD 重装成功。开放业务前需在自有可丢弃 VPS 单独验证安装/修复/DD、keep_last 管理失联保留、显式 lease 到期失效、Agent 重启、客户端认证和实际计量，记录结果。

网站的 `msboost uninstall`/`purge` 不处理独立 Agent 或客户 VPS 软件；v0.3.3 新装仍默认 keep_last，不把控制面失联当作撤销，仍需受信明确停止或独立节点维护与核对。显式 lease 才会在租约到期后停止。Agent 运维与控制面数据清理是分开的操作。

控制执行机和本站捐赠权益 Relay Agent 的安装过程不会应用客户 VPS 的 BBR 参数。该配置仅由免费工具任务在客户自有的 MSBOOST 目标、中转服务器或自备前置机上执行，不能通过安装 Agent 间接修改这些受管服务器。

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
