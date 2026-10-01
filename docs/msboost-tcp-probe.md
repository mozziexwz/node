# MSBOOST 全程 TCP 测速

原有 MSBOOST 新部署流程同时安装 v2 测速服务，配合 `MSBOOST启动器_全程TCP测速.bat` 的菜单 `[3]` 使用。客户沿用核心和配置，无需第二份安装命令。现有节点不会因网站更新自动获得此能力。

启动器分别查询出口 IPv4、到配置入口的 TCP 延迟，以及沿当前代理路径到目标的全程连接延迟。游戏项从发送 `connect` 请求开始，到收到最终节点完整的连接成功确认结束；节点只有真实 TCP 建连成功才确认，随后立即关闭目标 socket。暖机、出口查询和应用握手不计入，入口延迟不叠加。这是连接请求与确认的耗时，不能等同于游戏内 Ping。

## 部署与生命周期

`installers/node/msboost.sh` 内嵌完整 Python 程序，复用现有 Python 3 依赖。`installers/node/tcp_probe.py` 是可维护源码，测试核对其与内嵌版本一致；执行机仍只传送原来的一份安装脚本。

| 资源 | 路径 |
| --- | --- |
| 程序 | `/etc/msboost/relay_tcp_probe.py` |
| 配置 | `/etc/msboost/tcp-probe.json` |
| systemd 服务 | `/etc/systemd/system/msboost-tcp-probe.service` |
| 远端监听 | `127.0.0.1:20424` |

服务以 `msboost:msboost` 运行，程序和配置由 root 管理。服务随主服务启动、停止和重启。安装事务覆盖文件及原服务状态恢复；卸载预览列出 companion，确认后先停止服务再删除文件。未知文件、unit 指令、drop-in、symlink 和端口占用会阻止覆盖或清理。没有新增公网监听或防火墙放行规则。

安装重试只允许恢复能验证本项目身份的启用和屏蔽状态；不明屏蔽别名不会被覆盖。清理仍拒绝屏蔽 unit 的 symlink，管理员应先对受管服务解除屏蔽，再重新生成清理预览及确认摘要。

透明转发、多跳或入口出口分离场景中，只在最终执行目标 TCP 连接的 MSBOOST 节点安装服务。启动器连接本地 `127.0.0.1:<配置的 SOCKS5 端口>`，SOCKS5 CONNECT 目标为**最终节点的** `127.0.0.1:20424`。转发节点保持既有完整通道。

部署自测使用临时 Mieru 客户端，经实际 SOCKS5 通道完成 v2 `hello`，检查版本、nonce、node_id 和精确字段。自测确认协议可达；游戏目标连接和真实多跳线路须另行验收。

## 协议与限制

UTF-8 无 BOM、LF 分帧的单行 JSON，LF 前最多 4096 字节；CRLF 中的 CR 计入长度。只接受规定的扁平字段集合，不接受重复字段、转义字符串、未知字段、嵌套对象或类型替代。

每条会话先发送 `hello`：

```json
{"version":2,"operation":"hello","nonce":"0123456789abcdef0123456789abcdef"}
```

成功回复回显请求，增加进程启动时生成的 32 位小写十六进制 `node_id` 和 `success:true`。之后最多发送三条不同 nonce 的 `connect`：

```json
{"version":2,"operation":"connect","nonce":"abcdef0123456789abcdef0123456789","target_address":"35.155.204.207","target_port":8585}
```

成功回复回显请求，加上同会话的 `node_id` 和 `success:true`。合法请求的连接失败加上 `success:false` 与 `error`，枚举只有 `timeout`、`refused`、`unreachable`、`target_not_allowed`、`failed`。协议非法或顺序错误直接关闭连接。回复不携带服务端时长。

目标只能是规范的公网单播 IPv4 和 1–65535 的整数端口；不解析域名，不接受 IPv6。固定网段策略拒绝内网、共享地址空间、回环、链路本地、文档、基准测试、组播和保留地址，避免 Python 不同版本的地址判定变化。

| 限制 | 值 |
| --- | --- |
| 并发会话 | 4 |
| 单会话 connect 次数，含失败 | 3 |
| 空闲等待 / 会话寿命 | 3 秒 / 15 秒 |
| 单次目标建连 / 回复写入 | 各最多 2 秒，同时受会话剩余寿命限制 |
| 客户端暖机 / 单次采样 | 5 秒总时限 / 2 秒总时限 |

客户端校验 nonce、目标和 node_id，协议错误或超时后废弃会话。有效失败回复允许复用会话；成功样本取平均，失败不补零。node_id 用于会话一致性，不能证明运行核心与磁盘配置属于同一节点，连接保护依赖现有 MSBOOST 通道。

## 复验

```sh
python3 -B -m unittest discover -s installers/node -p 'test_*.py' -v
go test ./internal/executor
bash -n installers/node/msboost.sh
```

Windows 上可设置 `MSBOOST_TEST_LAUNCHER_BAT` 为新版 BAT 的完整路径，再运行同一组 Python 测试。兼容性测试只加载 BAT 的函数定义，使用本机 SOCKS5 转发和可控目标，不启动原有核心或菜单，不修改用户配置。没有该环境变量或 Windows PowerShell 时会跳过这项可选测试。

Windows 实机复验另有 `scripts/test-msboost-fullpath.py` 和配套 PowerShell 脚本，仅供项目组验收。新版 BAT 与现有核心路径由调用者显式传入；普通用户只需要启动器、核心和节点配置，无需随附这些测试脚本。

先检查 Python、配套 PowerShell 和 BAT 内嵌源码的语法。这一步不读取节点凭据、不启动核心，也不使用网络：

```powershell
python -B scripts/test-msboost-fullpath.py --check --launcher "$launcher" --core "$core"
```

部署完成并取得新的私有客户端 JSON 后，显式指定配置、入口公网 IPv4 与实际游戏目标。下面的路径及地址变量由调用者设置，配置中的唯一活动服务器必须与 `--node` 完全一致：

```powershell
python -B scripts/test-msboost-fullpath.py --run --config "$privateClientJson" --node "$entryIp" --target "35.155.204.207:8585" --launcher "$launcher" --core "$core" --menu
```

默认逐一测试 `HANDSHAKE_DEFAULT`、`HANDSHAKE_STANDARD`、`HANDSHAKE_NO_WAIT`，每个目标采样三次。可重复提供 `--target IPv4:port` 来替换默认目标，或使用互斥的 `--targets primary secondary`；`primary` 从实际 BAT 顶部读取，`secondary` 为 `51.222.56.192:8484`。公网目标拒绝或超时按失败记录，不能为通过验收而改写为成功。

透明转发复验时，私有配置保留最终节点凭据，服务器地址和端口指向实际转发入口；`--node` 校验入口，`--expected-exit` 校验实际菜单查询的出口 IPv4。默认出口预期与入口相同，入口和出口分离时显式指定最终节点：

```powershell
python -B scripts/test-msboost-fullpath.py --run --config "$privateForwardClientJson" --node "$entryIp" --expected-exit "$finalIp" --target "35.155.204.207:8585" --launcher "$launcher" --core "$core" --menu
```

脚本为各握手模式创建只允许当前用户及 SYSTEM 访问的临时配置，清除继承的 MIERU 配置环境，使用独立回环 SOCKS5 端口并关闭 RPC，核验监听属于本次创建的核心 PID。结束时只终止该 PID，删除自己的临时配置，并比对原 BAT、核心、输入配置、状态文件及已有核心进程。输出只含公开测量结果，不打印凭据、原始配置或核心日志。`--menu` 调用原 `Check-IpAddress` 页面，仅替换控制台输出捕获与回车等待；出口查询、入口 TCP 与全程测速仍执行真实函数。

真实上线前，在干净测试 VPS 走原部署入口，核对两个服务运行、20424 仅监听回环、SOCKS5/Mieru 自测通过，再用新 BAT 查询出口、入口和游戏项。改变游戏公网目标后直接复测，无需重装节点。

覆盖目标拒绝、丢包、连接延迟、目标接受但不发送应用数据，以及透明转发和自备前置线路。观察目标 socket 完成与确认发送的顺序；核对提前 SOCKS5 成功不会产生虚假样本。覆盖两个 v2 节点、旧节点和磁盘配置与运行配置错位。

本地模拟、协议单元测试及附件记载的历史单节点试装，不代表本次修改已在新部署 VPS 或真实透明转发链路通过验收。

## 2026-10-01 实机验收记录

获得两台已有测试 VPS 的全新部署授权后，通过原 `executor.Engine` 部署入口执行 Debian 检查、BBR、凭据生成、单脚本安装、代理 hello 自测和公网入口检查。两台均为 Debian 12 / Python 3.11.2，最终主服务与测速服务均为 active / enabled。具体地址、端口、配置和备份路径保存在私有验收记录中。

两台测速服务以非 root 的 `msboost` 用户运行，仅监听 `127.0.0.1:20424`；从 Windows 直接连接公网 20424 均被拒绝。源程序与仓库源码摘要一致。主服务停止、启动和重启时，测速服务随之切换，重启生成新的 node_id。实机协议检查通过内网目标拒绝、关闭公网端口返回 refused、真实游戏 TCP 成功，以及单会话三次请求限制。

用实际新版 BAT 的函数、实际 `MSBOOST.exe` 和原出口 IP 查询执行测试。只捕获控制台输出和回车等待，原启动器、核心、用户状态和已有核心进程保持不变。主游戏目标为 `35.155.204.207:8585`，下表每格为三次成功样本的平均值，单位 ms：

| 实际路径 | DEFAULT | STANDARD | NO_WAIT |
| --- | ---: | ---: | ---: |
| 本地 → 节点 A → 游戏（最终重装后） | 194 | 182 | 194 |
| 本地 → 节点 B → 游戏 | 191 | 192 | 193 |
| 本地 → A 透明转发 → B 节点 → 游戏 | 188 | 343 | 202 |

透明转发临时使用节点 A 的原入口转发到 B，A 的主服务及测速服务在该测试期间停止；有效 hello、游戏连接和 IP 查询实际来自最终 B 节点。页面正确显示 A 入口和 B 出口。三种握手模式全部通过，临时转发已退出，A 服务已恢复并随后完成清理重装。此测试验证两台所提供 VPS 的真实透明链路，没有验证规划示例中的日本地理线路或未提供的自备前置。

不修改或重装最终节点，改为 `1.1.1.1:443` 后，两台直连和透明链路的三种握手模式也全部三次成功。它是协议切换目标的可达对照，并非第二个游戏服务器。备用游戏 `51.222.56.192:8484` 的启动器测试全部超时，两台 VPS 直接 socket connect 也均超时；该目标连通性验收仍为失败，未改写为通过。透明 STANDARD 样本存在网络抖动，表内保留实际平均值。

真实回滚在节点 A 通过：仅对安装器副本注入最后代理 hello 自测失败，确认 10 个静态文件（包含节点及客户端凭据）、6 个运行时/持久服务链接和两项服务的 active/enabled 状态完全恢复，恢复后的 hello 成功。启用、运行时启用及屏蔽的其他状态组合在隔离事务测试中验证。

追加取得实际清理授权后，节点 A 按原清理 API 的预览摘要执行卸载，返回 `removed:true`；核对 8 项受管路径、两项服务、各自启用链接及 20424 监听全部消失，再立即通过原部署入口重装，最终客户端配置更新至新凭据和端口。节点 B 原手动试装文件按已核验摘要先备份迁移，未扩大产品对未知同名资源的接受范围。

实际磁盘与运行配置错位验收通过：同一隔离核心以 A 配置启动，保持 PID 和 SOCKS5 监听连续且不重新加载，只将其私有临时磁盘 JSON 改为 B 配置。实际 BAT 随后读取并显示 B 入口，但出口仍为 A，游戏三次全部成功，平均约 201 ms。这验证游戏测量遵循实际 SOCKS5 路径；node_id 仍不能认证磁盘配置与运行节点相同。

实际无测速服务的负向验收通过：临时停止节点 B 的 companion，主服务持续运行，原页面仍查询出正确出口及入口 TCP 数值；游戏项明确显示“节点未提供兼容的版本 2 全程 TCP 测速服务或服务不可达”，没有成功样本和平均值。正向实测工具对此返回失败，独立负向断言确认预期行为；companion 已恢复。此项覆盖缺少 v2 服务的真实代理路径，不声称验证所有第三方旧协议实现。

验证结果：本地 44 项 Python 测试通过（含实际 BAT 兼容测试），Debian 12 隔离执行同一源码的 32 项协议测试通过，附件 Packaging 与 20 组 Latency 测试通过，`go test ./... -count=1`、`go vet ./...`、安装器 Bash 语法及 diff 空白检查通过。另在 Debian 12 以 nobody 身份执行五项严格隔离的清理测试及其子用例，通过真实文件、链接和属主检查验证非 root CI 兼容性；未操作生产服务或防火墙。Python 3.9/3.11/3.13 的 CI 矩阵已配置。

最终客户端配置与详细环境记录仅保存在本地私有目录及各节点 root 私有目录中，不进入公开仓库。验收结束后两台均运行各自的直连 MSBOOST 节点。
