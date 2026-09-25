# 任务与执行机 API（v0.3.0 候选）

浏览器使用会话 Cookie，写请求需要 `X-CSRF-Token`。JSON 使用 camelCase，响应不缓存，错误为 `{ "error": "中文说明" }`。SSH 密码、免费配置和执行信封仅短期保存在内存，任务审计不存密码或原始远端日志。

## SSH 主机信任

`SSH` 对象为 `{host,port,user:"root",password,fingerprint,trustMode?,replaceFingerprint?}`。`trustMode` 接受 `tofu`、`strict`；省略时兼容原有已确认指纹客户端。两种模式都要求真实指纹，不会关闭 SSH 主机校验。

- `POST /api/fingerprints` 接受 `{host,port}`，不接收密码。在线执行机进行真实公钥握手，最多等待约 28 秒。
- 返回 `{host,port,fingerprint,algorithm,checkedAt,rememberedFingerprint,authenticationChecked:false}`，时间为 Unix 毫秒。未记住过的主机返回空 `rememberedFingerprint`。**获得公钥不代表密码正确，认证只在实际任务连接时进行。**
- 当前 UI 默认 `tofu`：提交前自动探测；首次提交明确告知用户将信任并记住主机。高级设置保留从 VPS 控制台独立核对的严格模式。首次信任弱于独立核对，不能抵御首次连接时的中间人攻击。
- 探测证明按当前用户、规范化公网 IP、端口缓存 30 分钟；中转与前置分别检查。控制面重启会丢失临时证明，需要重新探测。
- 接受任务时，将指纹按用户、规范化 IP、端口持久化至 `ssh_trust`；不保存密码，重启后保留。不同用户或端口不共享信任。
- 已记住指纹变化时拒绝提交。用户必须通过 VPS 控制台核实，在高级设置明确确认后，以 `replaceFingerprint` 携带准确的原指纹、以 `fingerprint` 携带新探测指纹。过期证明、原记录不匹配或确认后再次变化均拒绝。
- 执行期间每次 SSH 连接都固定校验该任务指纹，不会因连接失败改为接受任意公钥。

## 用户任务

`GET /api/tasks` 返回 `{tasks:Task[],limits:{deploy,relay,dd,fingerprint,cleanup,cleanup-preview}}`。限额字段为 `minutes,count,remaining,nextAt`；`nextAt` 为 Unix 毫秒，未受限时为零。指纹任务不进入用户普通任务列表。

`POST /api/tasks` 要求 8–128 字符的 `Idempotency-Key`；HTTP 202 表示新任务，200 表示同一参数的幂等结果。重复键不能提交不同参数，幂等重投不重新执行动作。

| kind | 额外参数 |
|---|---|
| `deploy` | `ssh`；仅接受 `mode:"fresh"`。每次重新生成受管节点认证与端口，不重装操作系统；失败时安装器仍以私有快照回滚。部署前只读核验目标为 Debian 11 或以上版本。 |
| `relay` | `ssh`；`clientConfig` 为 JSON 对象，只允许一个 profile、server 和 TCP 端口；可选 `front:SSH` 增加第二台前置机，不能替代中转；`remark` 可选、最多 60 字符且不得含换行。中转及前置 VPS 均先只读核验为 Debian 11 或以上版本。 |
| `dd` | `ssh`；`dd:{confirmErase:true,portMode:"keep"\|"new",newPort?,passwordMode:"keep"\|"new",newPassword?}`，仅固定 Debian 12 重装流程 |
| `cleanup-preview` / `cleanup` | 见下节；不接受前置机、客户端配置或 DD 参数 |

`GET /api/tasks/{id}` 返回元数据，所有者和管理员可审计；管理员不能读取其他用户的临时配置。`GET /api/admin/tasks` 返回 `{tasks:Task[]}`。

Task 字段：`id,userId,kind,host,sshPort,sshFingerprint,mode?,remark?,state,phase,message,errorCode?,nextStep?,createdAt,updatedAt,configAvailable,configHost?,configPort?,health?,hops?,cleanup?`。指纹是公钥摘要，不是私钥；内部幂等摘要和租约不属于普通审计字段。

状态为 `queued,running,succeeded,failed,executed,unknown,interrupted`。客户端轮询至终态。`executed` 仅用于 DD：准备成功、获得重启提交标志并确认预期断线；不声称新系统已经安装完成。提交证据不完整为 `unknown`，需从 VPS 控制台核实。控制面重启或响应超时不会重放已交付动作；非 DD 中断为 `interrupted`，对应 DD 为 `unknown`。

`GET /api/tasks/{id}/config` 返回原始客户端 JSON，仅所有者可读，服务器内存副本保留 10 分钟，重启即丢失。浏览器可存入加密 IndexedDB，不承诺跨浏览器或清除网站数据后的恢复。部署默认下载名为 `直连.json`，自备中转为 `自备中转.json`。自备备注用于配置 `profileName` / `activeProfile` 和下载名基础，文件名清理路径及控制字符；浏览器同时处理 Windows 保留名。自备中转监听端口从可用范围随机选择，不按区间起点顺序递增；仍须真实绑定成功才交付配置。备注不改变节点认证。

`health` 分离 `service,localSelfTest,publicTCP,game`；`game` 保留协议兼容，当前没有游戏探测，UI 不显示未检测的 Game 行。页面显示“服务运行 / 本地自测 / 公网 TCP 可达性”，并说明游戏需自行验证。`target_tcp_reachable` 仅表示目标 TCP 可达。`hops` 字段为 `fromHost,fromPort,toHost,toPort`。

维护模式对所有角色（含管理员）阻止新部署、中转、DD、清理及权益前置执行任务；已接受任务不自动取消。历史元数据、所有者临时配置和无密码的只读指纹探测仍可使用。指纹探测要求已认证的 active 账户、正常配额、公网地址及在线执行机，不套用免费工具邮箱要求；实际创建免费任务仍在读取 SSH 凭据前执行邮箱门禁，权益前置仍检查维护状态和权益。

## 预览与清理

均发送至 `POST /api/tasks`，使用前述 SSH 与幂等机制：

```json
{
  "kind": "cleanup-preview",
  "ssh": { "host": "公网IP", "port": 22, "user": "root", "password": "本次密码", "fingerprint": "真实指纹", "trustMode": "tofu" },
  "cleanup": { "scope": "msboost", "confirm": false }
}
```

`scope` 仅允许 `msboost`、`relay`。成功 Task 携带 `cleanup:{scope,digest,items:[{path,kind}],removed:false}`；`digest` 为 64 位小写 SHA256，`kind` 为 `file`、`directory` 或 `firewall`。预览会创建/获取互斥锁文件，但不停止服务、删除组件或修改防火墙；缺少 Python 3 时明确失败，不为预览自动安装依赖。未发现受管内容可返回空清单。

UI 折叠入口为“卸载 MSBOOST”或“卸载自备中转”。点击“继续”先自动核对主机并生成后台预览，不删除内容；成功后显示“确认删除”按钮，点击后以新幂等键提交。界面不再展示内部路径清单或要求手输确认词；修改 SSH 信息会作废当前预览，必须重新检查。后台仍执行全部范围、摘要与一次性授权校验：

```json
{
  "kind": "cleanup",
  "ssh": { "host": "同一公网IP", "port": 22, "user": "root", "password": "本次密码", "fingerprint": "同一真实指纹", "trustMode": "tofu" },
  "cleanup": { "scope": "msboost", "previewId": "成功预览任务ID", "digest": "预览摘要", "confirm": true }
}
```

控制面要求同用户、同主机/端口/公钥、同 scope/digest，且预览成功于 10 分钟内。一份预览最多用于一个清理任务；即使清理中断也不能复用，须先核实 VPS 并重新预览。

执行机重新枚举并验证所有权、完整文件摘要、服务及受管防火墙记录；内容变化即中止。运行中的 `cache.db` 或规则更新也会使预览失效，这是安全约束，不能跳过。成功结果必须返回相同摘要和 `removed:true`；失败可能已停止部分服务或移除部分文件，不能声称全部完成，不自动重试。

- `msboost`：仅 `/etc/msboost`、`/var/lib/msboost`、`/usr/local/bin/msboost`、`/etc/systemd/system/msboost.service`、`/root/直连.json` 及原状态记录为本项目创建的防火墙规则。要求 `msboost-installer-v1` 标记与匹配服务/配置；未知子文件、符号链接、挂载点、权限异常或额外 systemd 执行钩子均拒绝。
- `relay`：仅本项目 `msboost-free-*` 服务及对应 `/etc/msboost-free/<任务ID>`，检查完整布局、固定二进制路径、配置和防火墙所有权。旧版本无 `managed-by` 时仍须通过完整旧布局验证；不删除第三方 GOST。
- 备份、系统账户、依赖和共享二进制缓存保留；不清理网站、Docker、其他软件或全局防火墙。不同 VPS 的前置与中转需分别预览、清理。

## 失败诊断白名单

执行机 `message` / `nextStep` 不直接入库。控制面按 `errorCode` 生成固定中文原因与建议，`phase` 同样限制为已知阶段。未知代码降级 `execution_failed`；远端 stderr、密码或配置不能作为公开错误。

| errorCode | 分类 |
|---|---|
| `ssh_timeout` / `ssh_refused` / `ssh_connect` | SSH 超时、端口拒绝、连接失败 |
| `ssh_host_changed` | 主机指纹变化，停止连接 |
| `ssh_auth` | 认证失败；不能断言一定是密码错误，也可能禁止 root 密码登录 |
| `ssh_handshake` / `ssh_session` | 握手或会话建立失败 |
| `task_timeout` / `executor_offline` | 执行超时/连接中断，或执行机响应中断 |
| `unsupported_system` / `missing_dependency` | 系统/架构不支持，或依赖不可用 |
| `download_failed` / `integrity_failed` | 下载或资源完整性校验失败 |
| `archive_failed` / `binary_unusable` / `install_failed` | 解压失败、已校验程序架构/启动检查失败、受管组件写入失败 |
| `target_unreachable` / `service_failed` | 目标 TCP 不可达，或服务启动/监听检查失败 |
| `executor_configuration` | 缺少固定版本 GOST 资源配置 |
| `ownership_failed` / `cleanup_changed` / `cleanup_failed` | 所有权不符、范围变化、清理未全部完成 |
| `invalid_config` / `invalid_request` | 配置或任务参数无效 |
| `execution_failed` | 仅确定远端步骤失败，证据不足以进一步归因 |

主要阶段为 `ssh_connect,ssh_host_key,ssh_auth,ssh_handshake,ssh_session,preflight,dependencies,download,integrity,extract,binary,bbr,install,service,config,target,relay,front,prepare,submission_unknown,ownership,cleanup,execution,executor`，UI 显示中文。CPU 上升、能探测到指纹等现象不能单独证明任务失败原因。

GOST 下载归档保持固定 SHA256 校验，程序在受保护的 `/usr/local/libexec/msboost-free` 磁盘目录暂存、校验和原子发布；不在 Debian 默认可能挂载为 `noexec` 的 `/run` 中执行 ELF，也不修改挂载安全选项。已有共享缓存程序只核验、不覆盖。DD 使用仓库固定版本的 `bin456789/reinstall` 脚本，显式传入 `--username root`、密码与 SSH 端口，关闭其标准输入以避免交互提示吞掉外层执行指令。

免费工具的 `deploy`、`relay` 及其自备前置目标在客户自有 VPS 写入专用 `/etc/sysctl.d/99-msboost-bbr.conf`，执行 `sysctl -p /etc/sysctl.d/99-msboost-bbr.conf` 和 `sysctl --system`，再逐项读取核对补充 2.4 指定的全部 13 个 BBR/FQ 与 TCP 参数；重复执行保持幂等，不覆盖整份 `/etc/sysctl.conf`。控制执行机和本站捐赠权益 Relay 节点不执行该步骤。内核不支持、文件安全检查失败、加载失败或任一参数未生效时，任务按 `bbr`/安装阶段失败，不假报成功。

## 执行机管理

- `GET /api/admin/executors` 返回 `{executors:[{id,name,status,createdAt,lastSeenAt,ip,online}]}`。`status` 为管理状态 `active` / `disabled`；`online` 仅在启用、有心跳且间隔小于 90 秒时为 true。
- `ip` 是最近一次轮询/心跳的请求来源，按控制面可信代理配置解析，不直接相信任意 `X-Forwarded-For`；可能是 NAT 出口，离线后保留最后观察值。
- `POST /api/admin/executors` 接受 `{name}`，返回 `{executor,token}`。令牌只显示一次，数据库仅存哈希，不能认证 relay Agent。
- `PATCH /api/admin/executors/{id}` 接受 `{name?,status?:"active"|"disabled"}`。
- `POST /api/admin/executors/{id}/enrollment` 返回新 `{token}` 并清空心跳；有已交付待确认任务时拒绝重置。
- `DELETE /api/admin/executors/{id}` 撤销令牌，不能召回已交付任务或证明 VPS 操作已取消。
- 执行机以 `Authorization: Bearer ...` 调用 `GET /api/executor/next`，长轮询 25 秒，无任务返回 204。带唯一租约的信封仅交付一次，丢失响应不能重新执行。
- `POST /api/executor/result` 正常返回 200，重复/过期租约返回 409；可重试结果交付，不可重试执行。
- `POST /api/executor/heartbeat` 每 20 秒更新心跳，长任务期间仍发送。

执行机以 `msboost-agent --capability executor` 运行，配置 `MSBOOST_SERVER_URL`、`MSBOOST_EXECUTOR_TOKEN` 和两架构的 `GOST_*_URL` / `GOST_*_SHA256`。控制面要求 HTTPS，仅 loopback 开发允许 HTTP。GOST 使用固定官方 `go-gost/gost` Release，不使用 `latest`。远端免费中转要求 root SSH、systemd 247+ / `LoadCredential`；安装可在 Debian/Ubuntu 补齐 Python 3、curl、tar，其他系统需预先准备。清理预览不安装依赖。

网站升级不会自动更新独立 Agent。v0.2.2 新诊断与清理需要新版执行机，参见 [Agent 安装与升级](agent-installation.md)。

## 内部付费前置适配

`TaskService.ProvisionFront(ctx,userID,ssh,targetHost,targetPort) (executor.Hop,error)` 将客户前置接向实际分配的站内入口。检查维护状态、有效套餐、当前探测证明和持久化主机信任；不消耗免费任务配额，也不套用免费邮箱开关。浏览器共用 `prepareSSH`，只读探测仍受 active 登录账户、探测配额、公网地址和执行机在线约束。调用方负责付费规则事务，失败须撤销站内规则。SSH 凭据短期内存持有，超时不算成功，错误使用相同白名单。
