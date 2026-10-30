# 任务与执行机 API

浏览器使用会话 Cookie，写请求需要 `X-CSRF-Token`。JSON 使用 camelCase，响应不缓存，错误为 `{ "error": "中文说明" }`。SSH 密码或私钥、免费配置和执行信封仅短期保存在内存，任务审计不存密码、私钥或原始远端日志。唯一例外是中转码暂存的 SSH，见「中转码」一节。

## SSH 主机信任

`SSH` 对象为 `{host,port,user:"root",authMethod?:"password"|"key",password,privateKey?,passphrase?,fingerprint,trustMode?,replaceFingerprint?}`。`authMethod` 省略或为 `password` 时只接受 `password`；为 `key` 时只接受 `privateKey`（OpenSSH 读得懂的 ed25519、ECDSA、RSA 私钥，不超过 16 KB，PuTTY `.ppk` 需先导出）和可选 `passphrase`，`password` 必须为空。私钥在接受任务前即完整解析，格式错误或口令不对直接返回 400。`trustMode` 接受 `tofu`、`strict`；省略时兼容原有已确认指纹客户端。两种模式都要求真实指纹，不会关闭 SSH 主机校验。

- `POST /api/fingerprints` 接受 `{host,port}`，不接收密码。在线执行机进行真实公钥握手，最多等待约 28 秒。
- 返回 `{host,port,fingerprint,algorithm,checkedAt,rememberedFingerprint,authenticationChecked:false}`，时间为 Unix 毫秒。未记住过的主机返回空 `rememberedFingerprint`。**获得公钥不代表密码正确，认证只在实际任务连接时进行。**
- 当前 UI 默认 `tofu`：提交前自动探测；首次提交明确告知用户将信任并记住主机。高级设置保留从 VPS 控制台独立核对的严格模式。首次信任弱于独立核对，不能抵御首次连接时的中间人攻击。
- 探测证明按当前用户、规范化公网 IP、端口缓存 30 分钟；中转与前置分别检查。控制面重启会丢失临时证明，需要重新探测。
- 接受任务时，将指纹按用户、规范化 IP、端口持久化至 `ssh_trust`；不保存密码，重启后保留。不同用户或端口不共享信任。
- 已记住指纹变化时拒绝提交。用户必须通过 VPS 控制台核实，在高级设置明确确认后，以 `replaceFingerprint` 携带准确的原指纹、以 `fingerprint` 携带新探测指纹。过期证明、原记录不匹配或确认后再次变化均拒绝。
- 执行期间每次 SSH 连接都固定校验该任务指纹，不会因连接失败改为接受任意公钥。

## 用户任务

`GET /api/tasks` 返回 `{tasks:Task[],limits:{deploy,relay,dd,fingerprint,cleanup,cleanup-preview,relay-manage}}`。限额字段为 `minutes,count,remaining,nextAt`；`nextAt` 为 Unix 毫秒，未受限时为零。`deploy` 限额同时计入升级节点（`upgrade`）和取回配置文件（`fetch-config`），这两种任务也跟随「部署 MSBOOST」功能开关，管理员同样不受次数限制。`relay` 限额同时计入手动配置中转、使用中转码（`relay-code`）和生成中转码时的服务器检查（`relay-check`）；`relay-manage` 是中转服务器读取和操作的单独限额。指纹任务、`relay-inventory` 和 `relay-manage` 不进入用户普通任务列表。

`POST /api/tasks` 要求 8–128 字符的 `Idempotency-Key`；HTTP 202 表示新任务，200 表示同一参数的幂等结果。重复键不能提交不同参数，幂等重投不重新执行动作。

| kind | 额外参数 |
|---|---|
| `deploy` | `ssh`；仅接受 `mode:"fresh"`。每次重新生成受管节点认证与端口，不重装操作系统；失败时安装器仍以私有快照回滚。部署前只读核验目标为 Debian 11 或以上版本。 |
| `upgrade` | 只有 `ssh`（公网 IPv4），不接受 `mode`、备注或其他参数。先只读取回节点现有配置（没有 `/etc/msboost/install.env` 或 `/root/直连.json` 时以 `not_installed` 失败，服务器不做任何改动；配置入口不是这台服务器时为 `config_host_mismatch`），再核验 Debian 11 或以上版本，然后不带 `MSBOOST_FORCE_FRESH` 重跑固定版本安装脚本，安装脚本沿用已记录的用户名、密码、端口和低熵档位。升级后的配置必须与升级前入口、端口和凭据一致，否则以 `config_changed` 失败。成功时交付同一份配置。 |
| `fetch-config` | 只有 `ssh`（公网 IPv4）。只读取节点上的 `/root/直连.json`，按部署的标准核验格式和入口，不改动服务器，不跑本地自测也不调整 BBR；`health` 只有 `service`（两个节点服务都在运行为 `running`，否则 `stopped`）和 `publicTCP`。失败编码同 `upgrade` 的前两种。 |
| `relay` | `ssh`；`clientConfig` 为 JSON 对象，只允许一个 profile、server 和 TCP 端口，其余字段（如 `trafficPattern`）原样保留；可选 `front:SSH` 增加第二台前置机，不能替代中转；`remark` 可选、最多 60 字符且不得含换行；`rateMbps` 可选，上下行各限速，省略为管理员设置的默认值（5），须在管理员设置的范围（1–10）内。网站另行写入使用者记录（`source:"manual"`、配置人邮箱），浏览器不能提交 `meta`。中转及前置 VPS 均先只读核验为 Debian 11 或以上版本。 |
| `dd` | `ssh`；`dd:{confirmErase:true,portMode:"keep"\|"new",newPort?,passwordMode:"keep"\|"new",newPassword?,loginMode?}`，仅固定 Debian 12 重装流程。密钥登录时必须给 `loginMode`：`key` 把当前私钥推导出的公钥写入新系统（`passwordMode:"keep"`，不设 root 密码，私钥本身不写入），`password` 改为新 root 密码（`passwordMode:"new"` 与 `newPassword`）；重装脚本不能同时设置密码和公钥。密码登录时不接受 `loginMode:"key"`。 |
| `cleanup-preview` / `cleanup` | 见下节；不接受前置机、客户端配置或 DD 参数 |

`GET /api/tasks/{id}` 返回元数据，所有者和管理员可审计；管理员不能读取其他用户的临时配置。

`GET /api/admin/tasks` 仅管理员可读，返回 `{tasks:[{...Task,userName}]}`，按 `createdAt` 从新到旧排列。`userName` 显示操作人的当前登录邮箱；关联账号已删除或缺失时为“已删除用户”，账号邮箱为空时为“未设置邮箱”。原 `userId` 保留，便于核对同名或已删除用户；显示名称仅在读取审计列表时计算，不写回历史任务，也不加入普通用户任务 API。

- `q`：可选关键词，去除首尾空白后，不区分大小写地匹配 `userName,userId,id,host,kind,state,remark` 中任一字段的子串；为空时不限制。
- `kind`：去除首尾空白后，精确匹配 `deploy,upgrade,fetch-config,relay,dd,fingerprint,front,cleanup-preview,cleanup,relay-check,relay-code,relay-inventory,relay-manage,relay-verify`；省略或为空时显示全部，未知类型返回 HTTP 400。
- 同时提供 `q` 和 `kind` 时取交集，不增加分页或总数字段；无匹配项返回 `{tasks:[]}`。先核验管理员权限，再校验参数和读取任务；普通用户或未登录请求返回 HTTP 403。
- 搜索仅检查上述公开审计字段，不读取密码、执行信封或客户端配置。`configAvailable` 仍仅对任务所有者且临时配置未过期时为真；管理员不能凭审计结果下载其他用户的配置。

Task 字段：`id,userId,kind,host,sshPort,sshFingerprint,mode?,remark?,state,phase,message,errorCode?,nextStep?,createdAt,updatedAt,configAvailable,configHost?,configPort?,health?,hops?,cleanup?,rateMbps?,codeTail?,verify?`。`verify` 是中转目标校验的结果，见下文「中转目标校验」。使用中转码的任务对使用者只显示打码的中转服务器 IP，不返回拥有者的 SSH 端口和指纹。指纹是公钥摘要，不是私钥；内部幂等摘要和租约不属于普通审计字段。

状态为 `queued,running,succeeded,failed,executed,unknown,interrupted`。客户端轮询至终态。`executed` 仅用于 DD：准备成功、获得重启提交标志并确认预期断线；不声称新系统已经安装完成。提交证据不完整为 `unknown`，需从 VPS 控制台核实。控制面重启或响应超时不会重放已交付动作；非 DD 中断为 `interrupted`，对应 DD 为 `unknown`。

`GET /api/tasks/{id}/config` 返回原始客户端 JSON，仅所有者可读，服务器内存副本保留 10 分钟，重启即丢失。浏览器可存入加密 IndexedDB，不承诺跨浏览器或清除网站数据后的恢复；节点配置可以用 `fetch-config` 从服务器重新读取。部署、升级和取回的默认下载名为 `直连.json`，自备中转为 `自备中转.json`。自备备注用于配置 `profileName` / `activeProfile` 和下载名基础，文件名清理路径及控制字符；浏览器同时处理 Windows 保留名。自备中转监听端口从可用范围随机选择，不按区间起点顺序递增；仍须真实绑定成功才交付配置。备注不改变节点认证。

`health` 分离 `service,localSelfTest,publicTCP,game`；`game` 保留协议兼容，当前没有游戏探测，UI 不显示未检测的 Game 行。页面显示“服务运行 / 本地自测 / 公网 TCP 可达性”，并说明游戏需自行验证。`target_tcp_reachable` 仅表示目标 TCP 可达。`hops` 字段为 `fromHost,fromPort,toHost,toPort`。

维护模式对所有角色（含管理员）阻止新部署、中转、DD、清理及权益前置执行任务；已接受任务不自动取消。历史元数据、所有者临时配置和无密码的只读指纹探测仍可使用。指纹探测要求已认证的 active 账户、正常配额、公网地址及在线执行机，不套用免费工具邮箱要求；实际创建免费任务仍在读取 SSH 凭据前执行邮箱门禁，权益前置仍检查维护状态和权益。

## 中转码

拥有者生成中转码后，朋友用中转码在拥有者的中转服务器上配置中转，拥有者不用交出 SSH。网站只在拥有者设定的次数（1–10，默认 1）和有效期（1–72 小时，默认 24）内用主密钥加密暂存 SSH（集合 `relay_code_secrets`，不进入任何备份，任何接口都不返回），到期、用完、作废、拥有者账号停用、管理员停用或删除全部、关闭功能时立即删除，删除检查每分钟一次。已经建立的中转不受影响。中转码形如 `MSB-XXXX-XXXX-XXXX`（60 位随机），查找使用 HMAC 摘要，不区分大小写。

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/relay-codes` | `{ssh,maxUses,ttlHours,rateMbps,note,acknowledge:true}`，需要 `Idempotency-Key` 和 30 分钟内的指纹检查。创建状态为 `checking` 的中转码和一次 `relay-check` 任务（计入 `relay` 限额）。检查通过才加密暂存 SSH、固定指纹并转为 `active`，有效期从此时算起；检查失败不保存任何信息并删除这个中转码。每人同时有效（含检查中）最多 5 个。返回 `{code,task}`，浏览器轮询任务后读取详情。 |
| GET | `/api/relay-codes` | 我的中转码：`{codes,stats,settings,disabled,limit,manageLimit}`。`codes[]` 含完整 `code`（仅拥有者）、`masked`、`tail`、服务器、指纹、登录方式、次数、预占、带宽、到期、`secretHeld`、删除原因、建立和失败数。 |
| GET | `/api/relay-codes/{id}` | 详情与使用记录：使用者完整邮箱、打码节点 IP、中转端口、结果和失败原因。 |
| PATCH | `/api/relay-codes/{id}` | `{extendHours?,maxUses?,note?}`，仅 SSH 暂存期间。有效期从生成时算起不超过上限（72 小时），次数只能增加且不超过上限（10）。中转服务器、登录信息和带宽不能改。 |
| POST | `/api/relay-codes/{id}/revoke` | `{deleteRelays}`。中转码立即失效，SSH 在同一事务中删除。`deleteRelays:true` 时另建一次 `relay-manage` 删除任务，SSH 只在这个任务的内存信封里；无法建立任务时照常作废并返回 `warning`。 |
| POST | `/api/relay-codes/preview` | `{code}`，返回 `{ownerMasked,serverMasked,os,maxUses,remaining,expiresAt,rateMbps,tail,usable}`。无效 404，用完、过期或作废 410。 |
| POST | `/api/relay-codes/redeem` | `{code,clientConfig,remark,front?,consent:true}`，需要 `Idempotency-Key`；`front` 为使用者自己的前置机 SSH（需指纹检查）。先预占一次，任务成功才计次，失败、中断或前置机失败（执行机会删除刚建立的中转）都归还；最后一次用完时立即删除 SSH。创建 `relay-code` 任务（计入使用者的 `relay` 限额），带宽按中转码设定。拥有者服务器上只写入使用者邮箱和节点 IP、端口，不写节点认证。 |
| GET | `/api/relay-codes/used` | 我使用的中转：打码服务器 IP 和拥有者邮箱、入口端口、我的节点、带宽、来源中转码尾号。只显示建立时的结果。 |
| POST | `/api/relay-servers/inventory` | `{ssh}` 或 `{codeId}`（需管理员允许使用暂存 SSH），只读读取服务器上全部 MSBOOST 中转。 |
| POST | `/api/relay-servers/actions` | `{ssh 或 codeId,action:rate\|pause\|resume\|restart\|delete,items,rateMbps?}`，`items` 为受管编号（1–100 条），只作用于列出的中转。 |
| GET | `/api/relay-servers/results/{taskId}` | 读取或操作完成后 10 分钟内，请求者取回 `{os,relays}` 或 `{managed}`。列表只返回打码节点 IP。 |

`preview` 和 `redeem` 共用使用尝试限流：每账号每小时 10 次、每 IP 每小时 30 次（管理员可调）。读取和操作共用 `relay-manage` 限额（默认每 15 分钟 20 次），每次写审计（不含 SSH）。只读的 `relay-check`、`relay-inventory` 不占用服务器，也不被其他任务阻挡。

使用者记录为每条中转目录下的 `meta.json`（root，0600）：`version:1,source:"code"|"manual",codeTail?,user,panel,target,port,rateMbps,remark,createdAt,verified?`（RFC 3339 UTC）。`verified:{code,at}` 是最近一次目标校验的结果代码和时间，不含节点认证。中转码来源的 `remark` 是拥有者给中转码写的备注，只写在拥有者的服务器上；使用者前置机上的记录用使用者自己的配置备注。修改带宽同步更新 `rateMbps`；暂停写入 `paused` 标记并停用服务。没有 `meta.json` 的旧中转显示为「升级前配置」。

管理员接口：`GET /api/admin/relay-codes`（统计、按用户、SSH 删除记录、设置）、`GET /api/admin/relay-codes/users/{id}`、`POST /api/admin/relay-codes/users/{id}/revoke`、`POST /api/admin/relay-codes/users/{id}/feature`（`{enabled,reason}`，停用同时作废其全部中转码）、`POST /api/admin/relay-codes/purge`（`{confirm:"DELETE_ALL_RELAY_CODE_SSH"}`）。设置通过 `PUT /api/admin/settings` 的 `relayCodes` 字段保存；关闭功能会立即删除全部暂存的 SSH。

## 中转目标校验

校验证明一条中转确实通往 MSBOOST 节点。执行机从中转入口出发，用使用者自己的节点配置建立一次短时 mieru 隧道，经隧道连接节点只监听回环地址的探测服务 `127.0.0.1:20424` 并完成一次 hello：回复必须恰好是 `version,operation,nonce,success,node_id` 五个字段，`nonce` 与本次随机值相同，`node_id` 为 32 位十六进制。只有 MSBOOST 节点运行这个服务，且只能经隧道访问，所以只通过 mieru 认证不算通过。mihomo 的本地 SOCKS 入口会先答应连接、再去建隧道，隧道失败时只是断开，所以探测服务一个字节都没回时，执行机再经隧道与对照站点做一次 TLS 握手，用来区分「隧道没通」和「隧道通了但没有探测服务」。随后经同一服务请节点向游戏检测目标发起一次 TCP 连接，再经隧道分别与对照站点和本应被节点拒绝的站点做 TLS 握手，检查节点的过滤规则；这两项只记录，不影响结论。整次校验最长 90 秒，产生几 KB 流量，按普通流量计入该线路。

结果为 `verify:{code,game?,filter?,elapsedMs}`，不含节点地址、账号或配置的任何部分：

| `code` | 含义 | 算作目标问题 |
|---|---|---|
| `ok` | 目标是 MSBOOST 节点。`game`：`ok,refused,timeout,unreachable,failed,skipped`；`filter`：`ok`、`missing`（本应拒绝的站点可以访问）、`inconclusive`（对照站点也失败，无法判断）、`skipped` | — |
| `tunnel_failed` | 隧道没有建立：探测服务没有任何回复，经隧道也连不上对照站点。配置与节点不匹配、节点已重新部署，或目标不是 mieru | 是 |
| `not_msboost` | 隧道是通的，但对端没有 MSBOOST 探测服务：探测服务的回复不符，或没有回复、对照站点却能连上。不是 MSBOOST 节点，或节点版本较低 | 是 |
| `entry_unreachable` | 7 秒内连不上中转入口 | 否，属于中转自身的问题 |
| `target_down` | 节点没有运行；只由面板判定，执行机不会回报 | 否 |
| `unavailable` | 执行机本次未能完成校验（下载、沙箱、时钟等） | 否 |

- 校验由网站安排。浏览器在 `POST /api/tasks` 中提交 `verify` 或 `reportVerified` 返回 HTTP 400。
- `relay`（免费自备中转）：管理员设置 `free:"selftest"` 时附带只记录的校验，任务详情显示「中转自测」，从不因此拒绝或撤销。没有声明校验能力的在线执行机时跳过校验，照常配置。
- `relay-code`：按 `code` 设置。`log` 只把结果记入使用记录（`verify,verifiedAt`）。`enforce` 时 `tunnel_failed`、`not_msboost` 和 `entry_unreachable` 会让执行机删除刚建立的中转和前置机上的配置，任务失败（`phase:"verify"`，`errorCode` 为 `verify_tunnel_failed`、`verify_not_msboost` 或 `verify_entry_unreachable`，`failedRole:"target"`），本次不计次；`unavailable` 不阻止建立。强制校验只派给声明了校验能力的执行机，没有时拒绝使用并提示升级执行机。
- 中转服务器 `meta.json` 写入 `verified`。网站读取服务器列表时给 `relay-inventory` 附带 `reportVerified:true`，执行机返回每条中转的 `verified`，拥有者的列表据此显示。
- `relay-verify`：面板为捐赠权益线路发起的内部任务，只有 `clientConfig`（入口改写为该规则的线路入口；规则被校验暂停时为客户节点本身）和 `verify`，不带 SSH，也不占用服务器。期限 10 分钟，不进入用户任务列表，管理员审计可见，结束 7 天后删除记录，结果保留在规则上。调度和处理见 [商务契约](contracts-commerce.md) 的「用户配置」。
- 隧道客户端是 mihomo v1.19.33，两架构 `.gz` 的 SHA-256 与节点安装器相同并固定在代码中。执行机第一次校验时从 GitHub Release 下载到服务私有的临时目录并核对摘要，之后每次启动前再次核对；它作为单独程序运行，只监听回环地址，关闭进程查找和 IPv6，校验结束即停止并删除工作目录。执行机不链接 mieru 代码。`MSBOOST_MIHOMO_BINARY` 可改用管理员自备的本机文件，按原样使用。
- 校验用的配置与 SSH 凭据一样只在执行任务的内存信封中，不写入任务记录、数据库或日志。
- 实机检查：在 Linux 上用 `go test -c -o verify-field.test ./internal/executor` 编译，再以 `MSBOOST_VERIFY_FIELD=<放客户端配置的目录> ./verify-field.test -test.run TestVerifyField -test.v` 运行，会用真实的 mihomo 逐个校验目录里的配置并打印结果；不设这个变量时该测试跳过。

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
- `relay`：仅本项目 `msboost-free-*` 服务及对应 `/etc/msboost-free/<任务ID>`，检查完整布局、固定二进制路径、配置和防火墙所有权；目录里允许使用者记录 `meta.json` 和暂停标记 `paused`。旧版本无 `managed-by` 时仍须通过完整旧布局验证；不删除第三方 GOST。
- 备份、系统账户、依赖和共享二进制缓存保留；不清理网站、Docker、其他软件或全局防火墙。不同 VPS 的前置与中转需分别预览、清理。

## 失败诊断白名单

执行机 `message` / `nextStep` 不直接入库。控制面按 `errorCode` 生成固定中文原因与建议，`phase` 同样限制为已知阶段。未知代码降级 `execution_failed`；远端 stderr、密码或配置不能作为公开错误。

| errorCode | 分类 |
|---|---|
| `ssh_timeout` / `ssh_refused` / `ssh_connect` | SSH 超时、端口拒绝、连接失败 |
| `ssh_host_changed` | 主机指纹变化，停止连接 |
| `ssh_auth` | 认证失败；不能断言一定是密码或私钥错误，也可能禁止 root 用这种方式登录 |
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

免费工具的 `deploy`、`relay` 及其自备前置目标在客户自有 VPS 调优 BBR：先记录原值，用 `sysctl -p` 只加载补充 2.4 指定的 13 个 BBR/FQ 与 TCP 参数中内核提供的项（不执行 `sysctl --system`，不重新加载其他配置），逐项读取核对后再原子写入专用 `/etc/sysctl.d/zz-msboost-bbr.conf`；重复执行保持幂等，不覆盖整份 `/etc/sysctl.conf`。控制执行机和本站捐赠权益 Relay 节点不执行该步骤。内核不支持 BBR、加载失败或任一参数未生效时恢复原值、不写入文件，任务继续并在 `health.bbr` 报告 `unavailable`（原值恢复失败为 `review_required`），不假报已启用；`/etc/sysctl.d` 或该文件的所有者、权限、符号链接检查不通过时任务在 `bbr` 阶段失败。

## 执行机管理

- `GET /api/admin/executors` 返回 `{executors:[{id,name,status,createdAt,lastSeenAt,ip,online}]}`。`status` 为 `active` / `draining` / `disabled`；draining 停止接新任务，仍可补报已有结果。有最近 90 秒内心跳的 active / draining 执行机显示在线；version 显示构建版本。
- `ip` 是最近一次轮询/心跳的请求来源，按控制面可信代理配置解析，不直接相信任意 `X-Forwarded-For`；可能是 NAT 出口，离线后保留最后观察值。
- `POST /api/admin/executors` 接受 `{name}`，返回 `{executor,token}`。令牌只显示一次，数据库仅存哈希，不能认证 relay Agent。
- `PATCH /api/admin/executors/{id}` 接受 `{name?,status?:"active"|"disabled"}`。
- `POST /api/admin/executors/{id}/enrollment` 返回新 `{token}` 并清空心跳；有已交付待确认任务时拒绝重置。
- `DELETE /api/admin/executors/{id}` 拒绝存在待核实任务的执行机。忙时普通停用进入 draining，完成后停用。独立紧急撤销接口 `POST /api/admin/executors/{id}/revoke` 要求 `{confirm:"REVOKE_EXECUTOR"}`，撤销凭据但不召回远程命令，任务保留待核对占用。
- 执行机以 `Authorization: Bearer ...` 调用 `GET /api/executor/next`，长轮询 25 秒，无任务返回 204。带唯一租约的信封仅交付一次，丢失响应不能重新执行。
- `POST /api/executor/result` 正常返回 200；同一执行机、授权及完全相同的已保存结果重复回传也返回 200，内容不同或授权失效返回 409。执行机在内存中最多保留结果 30 分钟，指数退避重试期间不领取下一任务，只补报结果、不再次执行命令；进程退出后不能从磁盘恢复敏感配置。
- `POST /api/executor/heartbeat` 每 20 秒更新心跳，长任务期间仍发送。
- 执行机与 relay Agent 接口（`/api/executor/*`、`/api/relay-agent/register`、`/api/relay-agent/v2/sync`、`/api/relay-agent/enrollment-check`）先在内存中核对令牌哈希，未知令牌不进入数据库事务；已知令牌仍在各自事务内再次校验，撤销立即生效。同一来源（IPv6 按 /64）10 分钟内鉴权失败 30 次后，后续未知令牌返回 429 并带 `Retry-After`，有效令牌不受影响；控制面暂时无法核对时返回 503。两者均应稍后重试，只有 401 表示令牌被拒绝。

执行机以 `msboost-agent --capability executor` 运行，配置 `MSBOOST_SERVER_URL`、`MSBOOST_EXECUTOR_TOKEN` 和两架构的 `GOST_*_URL` / `GOST_*_SHA256`。控制面要求 HTTPS，仅 loopback 开发允许 HTTP。GOST 使用固定官方 `go-gost/gost` Release，不使用 `latest`。中转目标校验要求执行机本身能从 GitHub Release 下载固定版本的 mihomo，或设置 `MSBOOST_MIHOMO_BINARY`；下载失败时校验记为 `unavailable`，不算使用者的问题。远端免费中转要求 root SSH、systemd 247+ / `LoadCredential`；安装可在 Debian/Ubuntu 补齐 Python 3、curl、tar，其他系统需预先准备。清理预览不安装依赖。

执行机在每次轮询和心跳中声明能力：`X-MSBOOST-Executor-Capabilities: free_relay_socks_guard`（中转防护）、`X-MSBOOST-Scoped-Cleanup: 1`（按任务清理）、`X-MSBOOST-SSH-Key: 1`（密钥登录）、`X-MSBOOST-Relay-Code: 1`（中转带宽、使用者记录、`relay-check`、`relay-code`、`relay-inventory`、`relay-manage`）、`X-MSBOOST-Relay-Verify: 1`（中转目标校验：`relay`/`relay-code` 的 `verify`、`relay-inventory` 的 `reportVerified` 和 `relay-verify`）和 `X-MSBOOST-Node-Upgrade: 1`（`upgrade`、`fetch-config`）。新能力使用独立请求头，旧面板对第一个请求头做精确比较，不受影响。控制面只把任务派给声明了全部所需能力的执行机；创建时没有这样的在线执行机则拒绝并说明需要升级。手动配置中转现在也需要 `X-MSBOOST-Relay-Code`，以保证带宽和使用者记录生效。

网站升级不会自动更新独立 Agent。v0.2.2 新诊断与清理需要新版执行机，参见 [Agent 安装与升级](agent-installation.md)。

## 内部付费前置适配

`TaskService.ProvisionFront(ctx,userID,ssh,targetHost,targetPort) (executor.Hop,error)` 将客户前置接向实际分配的站内入口。检查维护状态、有效套餐、当前探测证明和持久化主机信任；不消耗免费任务配额，也不套用免费邮箱开关。浏览器共用 `prepareSSH`，只读探测仍受 active 登录账户、探测配额、公网地址和执行机在线约束。调用方负责付费规则事务，失败须撤销站内规则。SSH 凭据短期内存持有，超时不算成功，错误使用相同白名单。


## 重启后的任务核对

面板持久保存不含密码、原始配置或脚本的任务占用记录，包括目标及前置机。重启后已领取任务继续占用原主机，等待执行机补报；未领取任务取消，不重放。任务详情在需要核实时提供解除入口：用户先在 VPS 控制台确认原命令结束，再提交 `POST /api/tasks/{id}/reconcile` 和 `{confirm:"CONFIRM_TASK_ENDED"}`。此操作不代表部署成功，也不能停止远端命令。DD 提交成功仍需等待重装结束后核实。

结果配置仍仅保留内存 10 分钟。面板在结果提交后、确认响应到达前重启，可通过同一结果补报恢复剩余下载期限，不延长原到期时间。整站恢复会撤销旧执行机认证和结果授权，保留未结束任务的核对入口。