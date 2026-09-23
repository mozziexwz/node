# MSBOOST 发布说明

## v0.3.1 · 真实 VPS 排障与平台兼容

源码版本号本身不代表正式发布。v0.3.1 须等标签 CI、双架构镜像及 Release 资产全部完成后才可按下列入口安装。

- 控制面新增 Debian 13，保留 Debian 12；按实际系统代号选择 Docker apt 软件源。控制面不接受 Debian 11。客户 VPS 免费部署工具以 Debian 11/12/13 为兼容目标，但 Debian 11 已结束官方 LTS，不建议承载公开生产服务；面板 DD 目标仍固定 Debian 12。
- 修复部分 VPS 的 `/etc/sysctl.conf` 或 `99-sysctl.conf` 覆盖 BBR 参数：新版执行机在客户 VPS 使用 `zz-msboost-bbr.conf`，完整重放后再次应用并核对；仅移除内容完全匹配的旧项目文件，不覆盖运营者配置。必须升级执行机 Agent；单独升级网站不会更改已有 Agent 的脚本。
- 修复 Debian 11 systemd 247 不认识 `%d` 凭据路径导致免费自备中转启动失败：改用已在 247 上验证的 `${CREDENTIALS_DIRECTORY}/config.json` 无 shell 路径展开，继续通过 `LoadCredential` 隔离配置；清理器同时识别新版、v0.3.0 和更早的受管单元。德国 Debian 11 VPS 已通过真实自备中转安装及限定范围清理，原节点保持运行。
- 修复从严格权限源码目录构建时，非 root 应用进程无法读取固定 DD 脚本导致容器不健康。镜像构建现在明确授予公开内置脚本读取权限。
- 会员端枫叶数量统一显示“个”；线路状态去掉旧租约与协议代次等运维信息；单次 TCP 连接初检不再伪装为丢包率测试，也不宣称游戏协议验收。
- 真实 Debian 13 控制面全新构建、HTTPS、管理登录、只读后台接口、修复/保留数据卸载/恢复通过；5 台 Debian 11/12/13 客户 VPS 免费部署与 BBR/FQ 检查通过。两台完成 DD 重装并再部署；Debian 11 免费自备中转与定域清理通过，单节点捐赠中转获得真实 HTTP 200。范围和未覆盖项见[本轮验收记录](acceptance-vps-20260923.md)。发布制品须待标签 CI 和校验全部通过，源码验收本身不代表已发布。

```sh
curl -fsSL --proto '=https' --tlsv1.2 https://raw.githubusercontent.com/mozziexwz/node/v0.3.1/install.sh -o /root/msboost-install.sh
bash /root/msboost-install.sh
```

## 历史：v0.3.0 · 第三次最终版补充说明 2.4

源码版本号本身不代表正式发布。请以标签、Release 资产、双架构镜像、摘要及 CI 均成功为可安装条件；生产 msboost.de 和现有 Agent 不会自动升级。

- 注册与找回密码统一显示“密码至少8字符”；服务端同时拒绝少于8字符、等于完整邮箱或邮箱前缀，以及包含连续6位或以上数字的密码。未验证会员仍可找回，管理员仍只能在服务器本机改密。
- 会员端与后台统一“捐赠权益、权益、兑换、枫叶、兑换码、兑换记录”用语；金额不再显示人民币符号和小数，用户界面按整数枫叶展示。存储/API 的兼容字段不作为用户文案。
- 正常权益支持 `currentHoldersOnly`：只允许当前仍有效且当前权益 ID 匹配者续兑，用于高负载时停止新用户进入；体验权益对从未体验者开放，但同一用户全部体验权益合计终身仅一次且不能续兑。服务端对所有兑换渠道一致校验。
- 中转线路卡片删除旧策略说明，显示已用上行/下行流量；不足1GB用MB、达到1GB用GB，均保留两位小数。配置下载旁增加一次 TCP 路径样本诊断，返回 `入口(隧道名称)->目标(MSBOOST)`、成功/失败、入口建连延迟和单次样本丢包率；不验证 Mieru 登录或游戏协议。
- 捐赠线路支持稳定自定义排序；权益流量重置时同步清零各线路配置的当前上行/下行显示，但保留历史计量与审计。捐赠权益和免费自备中转配置从可用范围随机选端口，不再顺序暴露分配规律。
- 权益与线路增加 L1–L3 等级；高等级可使用同级及更低等级线路，低等级不能越级。管理员可一键清理指定线路上的全部客户规则，操作仍由服务端授权和审计。
- 管理员界面移除给自己新建工单；所有已登录用户（包括管理员）显示工单消息铃铛，有未读回复时显示橙色及数量，没有时为灰色。文章分类以方括号展示，置顶使用图钉/视觉标记而非“置顶”文字。
- 免费工具在客户自有 VPS 执行部署 MSBOOST、配置中转服务器或自备前置机时，应用规定的 BBR/FQ 与 TCP sysctl；控制执行机和本站捐赠权益节点不修改。实现使用独立 `sysctl.d` 文件，依次执行该文件的 `sysctl -p` 与 `sysctl --system` 并逐项核对全部参数，避免覆盖客户原有整份 `/etc/sysctl.conf`。
- v0.3.0 全新 Relay 安装默认 `keep_last`；显式 `--offline-policy lease` 保留旧链兼容。已有活动 lease 服务切换必须在维护窗口传 `--acknowledge-relay-restart`，因为 Agent/GOST 重启会影响现有连接。网站和 Agent 均不自动升级，优先建议全新 Debian 12 安装。

本轮耐久验收标准为 30 分钟，不要求 24 小时。既有 v0.2.4 的三拓扑与真实后端 30 分钟证据可以继续作为 Relay v2 基线，但不能替代 v0.3.0 新增功能、最终制品或真实业务环境的独立验证。详细范围见[补充说明 2.4 整改状态](supplement-2.4-status.md)。

## 安装与升级（正式资产发布后）

```sh
curl -fsSL --proto '=https' --tlsv1.2 https://raw.githubusercontent.com/mozziexwz/node/v0.3.0/install.sh -o /root/msboost-install.sh
bash /root/msboost-install.sh
```

候选期不要运行尚不存在或资产不完整的标签。正式发布后仍建议在全新 Debian 12 VPS 安装并隔离核对；确需升级健康旧站时，先离机保存数据库、主密钥和配置，再在维护窗口执行 `msboost upgrade --version v0.3.0`。网站升级不会升级独立 Agent，也不会替现有 Relay 选择新策略。

---

## 历史：MSBOOST v0.2.4 · 补充说明 2.3 安装调试版

v0.2.4 已发布：[标签 CI](https://github.com/mozziexwz/node/actions/runs/35633522811) 全绿，双架构镜像和 9 个 Release 资产通过独立下载与摘要校验。30 分钟耐久测试达到本轮验收标准。不会自动更新 msboost.de 或其他现有节点。

- 允许正常及未验证会员找回密码，禁止管理员和禁用账户邮件找回；验证码限流、单次使用、失败次数及旧会话撤销已回归。
- 管理脚本菜单12 / `msboost admin-password` 支持本机root交互修改已有管理员密码，不停止服务、不重建账号、不把新密码写入 `.env` 或命令行。
- 统一五类品牌邮件及纯文本版本，完成补充UI、枫叶标识、底部联系邮箱、免费工具文案和不可变ID线路排序；保留已确认页面结构。
- 新增显式试验 `keep_last`：持久命令、停止确认、离线保留、有界计量日志、恢复冻结及逐规则受信接管；增加网页备份原卷锁核销和受保护root恢复/TLS维护入口。
- 修复 Debian 12/systemd 252 的 DynamicUser 公共状态链接与严格状态检查冲突：新 keep_last 单元使用同一私有目录的直接路径，保留权限和拒绝任意链接检查，不迁移/删除已有数据。
- 预构建部署包、网站/Agent入口和版本检查统一，Compose优先固定镜像；同版Release备用镜像仍校验SHA256及架构/身份，不静默现场编译。
- 新版Agent入口于下载前拒绝低于v0.2.4的旧安装器，防止显式旧版本绕过v2/共享程序保护；网站及灾备历史版本恢复不受影响。

## 安装与升级

```sh
curl -fsSL --proto '=https' --tlsv1.2 https://raw.githubusercontent.com/mozziexwz/node/v0.2.4/install.sh -o /root/msboost-install.sh
bash /root/msboost-install.sh
```

健康旧站先在维护窗口执行 `msboost upgrade --version v0.2.4`，成功后才能使用 `msboost admin-password`；仅下载入口或运行 `repair` 不会升级旧镜像。升级保留配置、管理员、主密钥和数据库，并先创建私有备份；请离机保存备份。

**默认仍是 lease。** 网站升级不自动更新独立 Agent，也不自动开启 keep_last。旧节点必须按照[Agent迁移说明](https://github.com/mozziexwz/node/blob/v0.2.4/docs/agent-installation.md)逐整条线路维护升级；Agent/GOST重启会中断原连接。存在v2状态禁止静默降级。控制面失联时新的封禁、到期和超额无法实时下达到keep_last节点，恢复后再核对，不承诺离线期间实时撤销。

## 验证边界

本机 HTTP9项、浏览器35项、Go/脚本回归，以及临时Debian12的PG/Compose、真实GOST多跳/TLS、三拓扑5/30分钟、真实后端连续停站5/30分钟、撤销与丢ACK、受信恢复、私有磁盘故障和证书时间边界均有独立证据。完整记录及尚未完成项见[整改状态](https://github.com/mozziexwz/node/blob/v0.2.4/docs/supplement-2.3-status.md)与[验收矩阵](https://github.com/mozziexwz/node/blob/v0.2.4/docs/relay-v2-verification.md)。

这不是全T01–T26、真实QQ投递、商户实扣、游戏登录、arm64真机或容量的生产合格声明。30 分钟耐久已通过，但不替代其他未测场景；实际制品发布和部署结果须分别核验，不用源码测试替代。

---

## 历史：MSBOOST v0.2.3 · 补充说明 2.2 整改

- 登录标题改为两行“一键部署你的 / 独立IP游戏节点”，第二行整行橙色。
- 文章增加资讯、新文章靠前、独立置顶及组内最顶/最底排序；新建文章即可上传私有草稿附件，明确发布前不会公开。
- 后台显示易支付自动生成的通知地址、返回地址；付款回跳登录后定位本人订单，有限轮询真实状态，异常款单独提示。V1/V2 回调验签和幂等履约仍由服务端负责。
- 网页异地备份支持 SSH 密码与私钥两种认证，凭据密文保存且不回传；固定主机指纹、私有目录与回读完整性检查保留。
- 增加一键整站灾难备份、本机/远程目录、每日计划与保留天数，默认 `/root/msboost-backup`。远端清理须额外明确启用，校验全部候选后至少保留两份。
- 全新目标一键恢复原数据、密钥、应用文件与证书，导入新数据库并保持维护、关闭支付。未完成导入的隔离标记阻止“修复”误启动空站点。既有站点不覆盖，仍可使用独立新库恢复流程。

## 升级与使用

```sh
msboost upgrade --version v0.2.3
msboost disaster-config
msboost disaster-backup
```

Docker Compose 继续优先拉取固定预构建镜像，GHCR 失败时校验同版本 Release 备用镜像。升级保留管理员、主密钥与业务数据，不自动更新生产主站或独立 Agent。本轮没有新增必须升级独立 Agent 的执行协议。

完整说明：[补充说明整改](https://github.com/mozziexwz/node/blob/v0.2.3/docs/supplement-2.2-changes.md)、[整站灾难备份/恢复](https://github.com/mozziexwz/node/blob/v0.2.3/docs/disaster-backup.md)。完整备份含敏感主密钥与密码，不可公开传播；恢复后须人工对账、重新关联 Agent，再主动开放营业。

自动测试使用合成订单和隔离数据，不代表真实商户扣款、SMTP 投递、游戏登录或容量验收。历史真实 VPS 证据见下方旧版本记录；不会用旧标签覆盖发布。

---

## 历史：MSBOOST v0.2.2 · 真实 VPS 回归修复

按补充说明 2.1 与三台明确授权的 Debian 12 临时 VPS 实测修复；保留既定 UI 和数据保护边界。

- 自备中转不再从 `noexec` 的 `/run` 执行 GOST，改用受保护目录校验并原子发布；不关闭哈希检查、不修改挂载选项。
- GOST systemd 凭据使用完整 `config.json` 文件名，修复 `Unsupported Config Type ""`；启动失败返回明确阶段和诊断。
- DD 显式指定非交互 root 用户，避免上游等待用户名或消耗外层脚本输入。VPS1 已实测完成 Debian 12 重装并核验新根盘与 systemd，不仅是提交成功。
- Caddy 只信任官方 Cloudflare 网段，向应用转交单个验证后的客户端 IP，直连伪造头无效。
- **v0.2.2 额外修复**：Compose 不会因为绑定文件内容变化自动加载 Caddyfile。安装、升级、修复与回退先等待应用健康，再仅重建 Caddy，确保新旧配置实际生效；不强制重建数据库。
- 指纹提示面向普通用户；卸载卡片与表单同列等宽，后台预检后直接确认删除，不展示内部路径或要求手输。服务端摘要、一次消费与所有权保护不变。
- 后台版本号使用实际服务构建版本，与健康接口一致，不再固定显示开发版字符串。

## 安装与升级

```sh
msboost upgrade --version v0.2.2
```

升级先保存配置和 PostgreSQL 快照，保留管理员、主密钥与数据。可能短暂中断 HTTP 连接，应安排维护窗口。不要卸载清空；原库损坏时按 [完整恢复指南](https://github.com/mozziexwz/node/blob/v0.2.2/docs/backup-recovery.md) 恢复到全新库。

**网站升级不更新独立执行机。** 待在途任务结束，按 [Agent 指南](https://github.com/mozziexwz/node/blob/v0.2.2/docs/agent-installation.md) 更新执行机到同版本，才会执行新的中转/DD 逻辑。

新机器使用 [Debian 12 一键安装](https://github.com/mozziexwz/node/blob/v0.2.2/docs/deployment.md)。Docker Compose 优先预构建 GHCR 镜像，失败时使用同版本 Release 预构建归档；均校验，不自动现场编译。提供安装、升级、修复、卸载保留数据和独立彻底清理。

## 实测边界

真实临时 VPS 已验证全新部署、配置不变修复、认证直连/中转/前置双跳、受管范围卸载及 Debian 12 DD 完成。隔离 Compose 合成业务已通过 42 项商务、内容、权限与工单检查。详情及后续恢复/生命周期结果见 [验收记录](https://github.com/mozziexwz/node/blob/main/docs/acceptance-vps-20260913.md)。

这不等于全部生产功能验收：真实游戏登录、SMTP 投递、商户实扣、付费多跳公网计量、arm64 真机与大规模容量仍需相应环境和独立证据。既有标签不覆盖。
