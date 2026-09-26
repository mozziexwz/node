# 商务与中转 API 契约

实现位于 `internal/control/commerce*.go`、`payments*.go`、`relay*.go` 与 `internal/relayruntime`。本文描述接口契约；对应版本是否已经发布和通过现场验收，应另行核对 Release、CI 与部署记录。JSON 字段为 camelCase，金额为整数分，时间戳为毫秒，流量为字节。用户操作只接受整枫叶金额，即 cents 必须为 100 的整数倍；用户界面不显示人民币符号或小数。所有用户请求以会话决定身份，写请求带 `X-CSRF-Token`，不能传入 userId 代替授权。错误为非 2xx `{"error":"说明"}`，界面直接显示真实错误。

## 权益和枫叶

- `GET /api/plans` → `{plans: Plan[],purchasedCounts:{planId:次数},purchaseRequireVerifiedEmail}`。Plan 为 `{id,name,priceCents,days,trafficBytes,rateMbps,maxPurchasesPerUser,level,trial,currentHoldersOnly,enabled,version,createdAt}`。无示例权益；允许 0 枫叶、1–31 天，`level` 为 1–3。`maxPurchasesPerUser` 为 0–1000000 整数，0 无限；历史 paid 记录计次，免费/枫叶/在线渠道统一按事务检查。同 requestId 重试不再计次；限制调整时，正在支付的旧记录采用创建快照与当前限制中更严格的一项，条件不再满足的有效通知进入 `paid_review`，不能丢弃真实款项。
- `currentHoldersOnly=true` 时，仅当前权益仍有效且当前权益 ID 正好等于目标 Plan ID 的用户可续兑；需与 `enabled=true` 配合，`enabled=false` 仍对所有用户关闭。`trial=true` 时，服务端扫描该用户所有已履约体验权益；从未体验者可兑，任何体验权益一旦成功履约后终身拒绝再次兑换或续兑。两项规则不是前端按钮限制。
- `GET /api/wallet` → `{balanceCents,ledger:[{id,userId,kind,amountCents,balanceAfter,reference,reason,createdAt,cardCode?}]}`。本人兑换码流水在读取时解密 `cardCode`；不把明文重新写入账本。
- `POST /api/wallet/redeem` 输入 `{code,requestId}`。requestId 为 8–128 字符，返回 `{balanceCents}`。兑换码只增加枫叶，账本与使用记录原子提交。同 requestId 网络重试幂等；换新 requestId 再次使用无效或已使用代码返回 409“兑换码无效或已被使用”，不重复提示成功，也不泄露精确状态。
- `GET /api/orders` → `{orders:Order[]}`；`GET /api/orders/{id}` → Order。
- `POST /api/orders` 输入 `{planId,channelId,requestId,confirmReplace:true}`。枫叶渠道的技术 ID 仍为 `balance`，其余为实际在线渠道 ID。返回 Order；非枫叶渠道提供 `paymentUrl` 跳转真实收银台。枫叶扣减与权益发放原子完成。
- `purchaseRequireVerifiedEmail=true` 时，兑换事务要求当前用户邮箱已验证；开启此设置需 SMTP 测试就绪。有效渠道通知到达时若邮箱、次数、`currentHoldersOnly` 或 `trial` 条件已不满足，转 `paid_review`，不擅自退款或发放权益。
- Order为 `{id,userId,plan,amountCents,currency:"CNY",channelId,state,requestId,tradeNo?,paymentUrl?,createdAt,expiresAt,paidAt?,entitlementVersion,reviewReason?}`。state为pending/paid/expired/paid_review；paid_review说明收到有效签名款项但需要管理员人工核对，不覆盖现有新权益。
- 再次兑换门槛：剩余真实毫秒 `<30天` OR 剩余字节 `<10000000000`。新权益覆盖、非叠加。体验权益不适用续兑；`currentHoldersOnly` 另要求当前有效且同权益。每用户仅一个未过期待处理记录，30 分钟有效。
- `GET /api/admin/plans`、`POST /api/admin/plans`、`PUT/DELETE /api/admin/plans/{id}`。保存体是 Plan 字段（创建省略 id/version/createdAt），`priceCents % 100 == 0`。兑换记录引用阻止删除，下架通过 `enabled:false`。
- `GET /api/admin/cards` → `{cards:[{id,code,amountCents,status,batch,usedBy?,usedByEmail?,createdAt,usedAt?}]}`，完整 code，仅管理员可枚举。默认排除 archived；`?status=archived` 查看已归档，`?status=all` 查看全部，也可按 active/disabled/used 筛选。删除已用兑换码是归档，不删除兑换幂等或账本记录。
- `POST /api/admin/cards` 输入 `{count,amountCents,batch}`，count 1–1000，返回完整cards。
- `POST /api/admin/cards/batch` 输入 `{ids,action:"enable"|"disable"|"delete"|"export"}` → `{ok:true,cards}`。已使用兑换码不可再启用，删除转 archived；未用可删除。code 在数据库用 AES-GCM 密文，兑换匹配 SHA256。
- `GET /api/admin/orders` → `{orders}`，额外返回每单userEmail；普通订单列表不返回其他用户信息。

## 易支付

- `GET /api/payment-channels` → `{channels:[{id,name,type,version,enabled,configured}]}`；type是alipay/wxpay。
- `GET /api/admin/payment-channels` → `{channels:[{id,name,version,type,gateway,merchantId,enabled,configured,notifyUrl,returnUrlTemplate}]}`。秘密永不回显。两个地址由 HTTPS `PUBLIC_URL` 自动生成，后台只读显示并可复制；每笔下单自动传入实际 `notify_url`、`return_url`，无需填写可编辑的通知地址。`returnUrlTemplate` 中 `{orderId}` 会替换为实际订单号，模板本身不是付款链接。普通用户渠道列表不提供这两个管理字段。
- `POST /api/admin/payment-channels`、`PUT /api/admin/payment-channels/{id}` 输入 `{name,version:"v1"|"v2",type:"alipay"|"wxpay",gateway,merchantId,enabled,merchantKey?,privateKey?,platformPublicKey?}`。v1用merchantKey；v2用后两个RSA PEM/base64字段。留空保留同版本旧秘密；版本切换须补全。网关和PUBLIC_URL必须HTTPS。
- `DELETE /api/admin/payment-channels/{id}`，有订单引用时阻止删除。
- `GET/POST /api/payments/epay/{channel}/notify` 仅真实异步签名通知履约。返回纯文本success/fail。v1 MD5，v2 RSA SHA256；核验商户、渠道、订单、交易号、金额、状态和CNY（协议缺currency时绑定订单CNY）。重复幂等，交易号不能跨订单复用，晚到或权益变更转paid_review。
- 同步返回入口为 `PUBLIC_URL/?order={实际订单号}`。网页保留登录后的订单目标，只通过需认证且校验本人归属的 `GET /api/orders/{id}` 展示状态；不信任返回查询参数中的支付状态或签名，不通过同步回跳入账。在线订单创建弹窗与返回页均支持最多13次、间隔5秒的本地订单查询，单次请求10秒超时；终态、错误或次数耗尽停止自动查询，仍可手动刷新。`paid` 才刷新账户权益，`paid_review`、`expired`、待通知和查询错误明确区分。这里不是向支付平台主动查单。
- 当前支付交互是已签名的真实网页跳转 `/submit.php` 或 `/api/pay/submit`；二维码API、主动查询、退款自动化尚未接入，不能显示演示二维码或“测试支付成功”。

## 线路管理

- `GET /api/routes` → `{routes,protocols:["tcp","tls"],strategies:["round","rand","fifo"],leaseSeconds:45}`。普通用户只收到已启用且 `route.level <= user.level` 的线路简表，并按管理员持久化顺序返回；online 来源最近真实 Agent 同步。入口 IP、地址清单、节点 ID 与转发链不向普通用户列表返回，下载配置仍包含实际必需的入口。
- `GET /api/admin/routes` 返回完整隧道。Route `{id,name,type:"port_forward"|"tunnel",trafficMode:"both"|"upload"|"download",trafficMultiplierPermille,entryAgentId,entryAddress,entryAddresses:[],addressPreference:"auto"|"ipv4"|"ipv6",hops:Stage[],exit:Stage,requireFront,enabled,version,online,rateMbps,level,order}`。`level` 为 1–3；`order` 是服务端持久化的显示顺序。倍率为 1–100000 整数千分值，1000=1 倍（旧数据 0 按 1 倍兼容）；UI 以 0.001–100 倍显示。计量方向以入口接收为上传、入口发送为下载，在规则创建时保存计费快照。
- port_forward仅配置入口，直接转发客户目标，不分配出口；tunnel包含单入口、有序0–8个中间跳及出口池。入口客户端协议固定TCP，Stage选择的是到该层节点的传输协议。新tunnel不能把同一入口再当出口；旧单节点直转自动按port_forward兼容。
- Stage `{agentIds:[id...],protocol:"tcp"|"tls",strategy:"round"|"rand"|"fifo",connectIp?:string}`。每层1–8候选，同层做负载，禁止重复节点/环路。显式connectIp必须在所有候选节点的地址集合中；留空根据addressPreference选择每节点已登记的IPv4/IPv6，无匹配地址明确报错。显式选择优先于地址偏好。
- entryAddresses可填写最多16个公网IP/域名，留空从入口节点登记地址取得并保存entryAuto=true；自动模式在节点登记地址变更后重新派生入口，不保留旧IP。旧entryAddress单值仍兼容。单一客户端JSON按地址偏好选取一个入口，不虚构多入口自动故障切换。
- `POST /api/admin/routes`、`PUT/DELETE /api/admin/routes/{id}`；存在用户规则时阻止删除或结构修改，需先迁移。
- `POST /api/admin/routes/{id}/move` 输入 `{direction:"up"|"down"}`，在持久化线路顺序中移动一位。返回更新后的线路；顺序不依赖名称、在线心跳或 map 遍历。
- `POST /api/admin/routes/{id}/purge-rules` 输入 `{confirm:true}`，撤销该线路的全部客户规则并返回受影响数量。只接受管理员与精确线路 ID；涉及 v2 时仍等待各段明确停止，不把“已发出撤销”冒充资源已释放。
- `GET /api/admin/relay-agents` → `{agents}`。与executor列表及令牌独立。
- `POST /api/admin/relay-agents`、`PUT /api/admin/relay-agents/{id}` 输入 `{name,address,addresses?:[],enabled,portRanges:[{start,end}]}`。address为主公网IP，addresses额外登记IPv4/IPv6；去重后含主地址最多16个。清空addresses仅保留主地址，不假称自动发现网卡。端口最多50段，1–65535不重叠。节点级requireFront已取消：旧值和旧请求字段忽略；仅保留隧道级requireFront。引用中的用户规则阻止变更地址集合。
- 创建返回 `{agent,enrollmentToken,installArgs}`，一次性注册令牌15分钟有效。installArgs为 `{installer:"deploy/install-agent.sh",args:[...],tokenEnvironment:"MSBOOST_RELAY_ENROLLMENT_TOKEN",instructions}`；args使用安装器支持的 `--token-file`，需替换实际Agent路径和审核后SHA256，令牌单独保存到root私有0600文件，不放进命令行。不是给cmd/agent传入不存在的 `--enrollment-token` 参数，也不从网页自动执行shell。编辑不回显token。缩端口池、改IP、增强前置要求若破坏现有规则则阻止并说明。
- `DELETE /api/admin/relay-agents/{id}`有路线/规则/租约引用时阻止。删除不卸载远端机器。
- `POST /api/admin/relay-agents/{id}/enrollment` 重新签发15分钟注册令牌并撤销旧令牌。纯 v1 返回旧租约截止；v2 返回恢复核对警告，不承诺45秒停止，相关资源继续保留。重装会中断原连接；需要保留既有进程时使用本机受信恢复流程，不用注册令牌冒充热接管。
- `GET /api/admin/user-rules` → `{rules}`，包括 `userEmail` 及脱敏诊断字段；界面支持邮箱/线路/状态筛选。
- `GET/PATCH/DELETE /api/admin/user-rules/{ruleId}` 提供详情、`{paused:true|false}` 暂停/恢复及撤销。纯 v1 按最后租约失效后归档；v2 / 混合链路等待所有 v2 段精确 revoke 停止 ACK，未确认前不释放端口或旧目标。恢复核对期间禁止普通删除/恢复覆盖。管理员不能通过此接口取得用户客户端配置或认证秘密。线路/节点删除冲突给出具体关联与管理入口。

## 用户配置

- `GET /api/user/rules` → `{rules:UserRule[]}`。关键字段 `{id,routeId,routeName,state,targetHost,targetPort,version,createdAt,hasFront,trafficBytes,inputBytes,outputBytes,trafficMode,trafficMultiplierPermille}`。普通用户响应不含入口 IP/端口、segments、配置密文、目标认证 hash 或 TLS 材料；规则创建/暂停响应使用相同脱敏。管理员规则接口保留诊断 segments 但同样去除密钥、目标列表和证书材料。页面按 `inputBytes` 显示已用上行、按 `outputBytes` 显示已用下行；小于十进制 1GB 用 MB，否则用 GB，保留两位小数。
- v0.2.0 增加 `effectiveRateMbps,appliedRateMbps,syncState,readySegments,totalSegments`。有效值为用户当前速率与线路上限的较小值；全部规则、每跳新版本 ACK 就绪后才显示已生效。更改用户速率或权益会先使旧 ACK 失效，不能将陈旧 active 显示为新政策已生效。用户线路列表 `rateMbps` 是有效速率，`userRateMbps` 为用户当前速率；权益卡是兑换时授予值。用户权益流量重置时，当前规则的 `trafficBytes`、`inputBytes` 和 `outputBytes` 同步归零并建立新的 Agent 累计基线；月历史与审计不删除。
- `POST /api/user/routes/{route}/rules` 输入 `{config:<上传的JSON对象>,requestId,front?:{host,port,user,password,fingerprint}}`。严格单profile/server/port/TCP，所有线路的规范化目标、端口、协议和用户名密码身份必须相同。不是只比较文件名。
- front 为可选自备前置；隧道 requireFront 时必填，节点旧字段不再继承。真实 fingerprint 从现有 executor 指纹探测获取。服务器在每个节点允许范围内随机选择未占用、未待撤销且未被恢复保留的端口 → Agent 真实 GOST 绑定 ACK → executor 前置指向本站入口 → 成功后替换最终客户端入口和加密保存。调用可等待约数分钟，请前端显示实际准备状态，勿重复提交。SSH 仅存在请求/执行器内存，失败撤销本站规则。
- state为pending（等待绑定）、awaiting_front（本站准备/前置执行）、active（GOST监听ACK完成）、paused、failed、revoking、suspended、quota_exhausted。active只表示运行时监听准备好，不保证公网路径或游戏登录成功。
- `PATCH /api/user/routes/{route}/rules` 输入 `{paused:true|false}`。
- `DELETE /api/user/routes/{route}/rules` → 202。纯 v1 返回 `{state:"revoking",maximumLeaseSeconds:45}`；涉及 v2 返回 `{state:"revoking",stopStatus:"pending",message}`，不返回虚假的租约截止。立即撤销服务器下载密文，v2 资源需等待明确停止确认。
- `GET /api/user/routes/{route}/config` → 登录鉴权下载JSON，固定socks5Port10086；已有文件流量耗尽但未到期可下载，到期不可下载。不存在任何公共配置文件URL。
- `POST /api/user/routes/{route}/diagnose` 对本人当前有效、active 且等级允许的配置执行一次入口 TCP 探测。成功或失败均返回 HTTP 200 诊断对象 `{routeName,path,status:"success"|"failed",latencyMs,packetLossPercent,error}`；用户界面把 `path` 规范化为 `入口(线路名)->目标(MSBOOST)`。失败样本不能伪装成功；接口有用户级限流，单次结果不代表持续 SLA。
- `POST /api/user/target/reset` 输入 `{confirm:true}`，撤销所有旧线路。纯 v1 返回 `{state:"revoking",retryAfter:时间戳毫秒}`；涉及 v2 返回 `{state:"revoking",stopStatus:"pending",message}`，全体旧实例确认撤销前不允许配置新目标。
- `GET /api/user/traffic` → `{trafficUsed,trafficTotal,months:{"2026-09":字节},accounting}`。入口按规则保存的流量方向及倍率计费，多跳只计一次，各线路共用账户额度。整数千分倍率的小数字节余数随cursor持久化，同一epoch分批上报不会截断丢失；去重、溢出和旧权益保护继续有效。月历史不随买套餐清空。

## Agent运行契约和运维边界

`App.RegisterCommerce(mux)`、`App.RegisterRelay(mux)` 注册端点；启动 `StartCommerce(ctx)`、`StartRelay(ctx)` 处理兑换记录过期和失效规则。`SetFrontProvisioner(tasks.ProvisionFront)` 接入执行器。Relay Agent 在 Linux 运行并需要独立 GOST v3 二进制。全新 Relay 默认 keep_last，注册后使用 `/api/relay-agent/v2/sync`；显式 `--offline-policy lease` 才使用 `/api/relay-agent/sync` 的 5 秒短租约协议。协议结构位于 `internal/relayruntime/protocol.go`，既有节点不自动迁移。

GOST每条规则独立子进程，固定配置与源IP白名单；多跳内部节点只允许上一跳候选源IP，前置模式入口仅允许前置源IP。部署网络必须确保节点出站源IP与登记address一致。域名目标首次创建时解析并检查所有地址为公网，然后固定IP，DNS变更需重配。控制流不承载游戏字节。已用官方GOST v3.3.0（官方checksum核验）通过本机回环真实双跳TCP、运行ACK、累计Observer和撤销关闭端口的集成测试；这不替代客户VPS公网路径验收。

首次部署仍需下游绑定ACK后才发布入口；v2 已运行入口不因下游管理心跳过期而撤销。GOST Observer提供真实监听状态和累计traffic，令牌不会授予SSH/DD能力。纯 v1 离线超过租约由watchdog停止；v2 使用独立 `/api/relay-agent/v2/sync`，命令/流量分别明确 ACK，遗漏、坏响应和管理失联不删除既有配置。Linux父进程死亡保护仍保留。恢复、限额与状态字段详见 [Relay v2 契约](relay-v2-contract.md)。

TLS跳使用GOST 3.3.0的tls listener与forward+tls连接器。每条用户规则、每个TLS节点生成独立ECDSA证书，服务端仅保存AES-GCM加密私钥；同步响应只向该节点交付自己的私钥，上一层只得到公开信任证书与节点DNS身份。GOST要求secure=true、固定CA和serverName，最低TLS1.2；不使用InsecureSkipVerify。证书有效期一年，纯 v1 剩余不足7天时自动轮换；含 v2 的整链普通重连不轮换，须经 [root 显式证书维护](relay-recovery.md) 更新选中规则，可能重建其连接。过期证书不会降低验证要求。运行时将证书/密钥写入私有临时文件，正常结束或启动失败会清理；异常崩溃遗留需检查。源IP白名单仍保护入站；当前不是双向客户端证书认证。

计量停止依赖控制面可达性；v1 另有45秒租约，v2 / keep_last 失联期间新发生的到期/封禁/超额决定延后执行，非精确到最后一字节的分布式硬配额。v2 旧权益样本计入原账务周期而不扣新权益；跨月或时钟异常且无法精确拆分的区间进入待核对。异常断电可损失 Observer 最后尚未产出的样本，缓存耗尽必须显示计量降级。当前速率是每条规则双向分别限速；全账户共享速率、UDP、双向客户端证书认证、无损故障迁移没有宣称完成。FLVX 未完整迁入或宣称兼容。

网页安全恢复不回滚商务数据：白名单只恢复站点设置、文章附件、线路与节点定义，完整保留当前用户身份、订单、卡密、流水、请求/支付去重、支付配置、权益、用户规则、计量和未知新集合。当前规则引用的线路节点保留当前拓扑；预检需带当前恢复范围摘要。恢复后保持维护并撤销 Agent 凭据；涉及 v2 时保留资源和运行关联，冻结自动覆盖，不能推定旧转发已停止。受保护接管不会自动开放支付或营业。完整快照仅由离线 `msboost-restore` 导入全新库，绝不在线覆写旧库，见 [恢复指南](backup-recovery.md) 与 [逐规则受信恢复](relay-recovery.md)。

参考一手文档：[GOST转发](https://gost.run/tutorials/port-forwarding/)、[forward连接器](https://gost.run/en/reference/connectors/forward/)、[3.3.0对应TLS实现](https://github.com/go-gost/x/blob/v0.16.0/internal/util/tls/tls.go)、[Observer](https://v3.gost.run/en/concepts/observer/)、[速率限制](https://v3.gost.run/concepts/limiter/)、[准入控制](https://gost.run/en/concepts/admission/)、[易支付RSA签名](https://yzf.yzfpay.com/doc/sign_note.html)、[用户指定支付指南](https://dujiao-next.com/payment/guide)。
