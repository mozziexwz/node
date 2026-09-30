# Agent 安装与节点维护

网站升级不会自动升级或重启独立 Agent。客户 VPS 免费工具与后台受管节点是不同服务。

## 新安装

在后台“执行机”或“节点”生成命令和专用一次性令牌。命令下载固定版本预构建程序，核对 SHA-256，不在 VPS 编译。令牌隐藏输入，不放进 URL 或公开日志。执行机和节点不能共用令牌。

安装结束前检查注册与管理连接。后台应显示在线，再实测转发或无破坏性任务。Cloudflare 不应对 Agent API 要求浏览器验证或缓存 API。

## 升级与诊断

已安装节点入口为“升级 / 检查连接”，复制后台命令在该节点 root 终端执行。保留原身份升级使用 `--upgrade-in-place --acknowledge-relay-restart`，不需要新令牌。先下载目标版本脚本；Agent/GOST 重启会断开现有连接，应安排维护窗口。失败保留私有回滚材料，不删除状态强行注册。

只读诊断：
```bash
bash /tmp/msboost-agent-install.sh --capability relay --server https://panel.example.com --diagnose
```
检查服务、面板 HTTPS 及本机状态文件，不重启或打印令牌。连通成功不是业务质量保证。

## 离线与旧站升级

### 节点本机防火墙

新版安装器增加 `msboost-relay-firewall.service`，使用独立维护程序 `/usr/local/libexec/msboost-agent/relay-firewall`，不与执行机共用文件。Relay 仍以 DynamicUser 运行。维护组件只读取该服务实际监听的 TCP 端口，在 UFW 中维护带所有权标记的 `MSBOOST-RELAY` 子链；不禁用 UFW、不开放整段端口、不改 SSH、其他应用或管理员规则。手工 deny 仍优先。

删除/暂停后撤除对应许可；面板离线而本机继续转发时保留许可；UFW 重载后重新核对，重载期间新连接可能短暂失败。卸载节点时清理本项目许可和维护组件。

自动管理目前适用于 UFW。未启用 UFW 时不修改其他防火墙；运行中的 firewalld 会报告需管理员处理，不自动绕过。自定义 nftables、服务商安全组和来源限制仍可能阻断连接。本地维护完成不等于公网或游戏验收。

查看 `systemctl status msboost-relay-firewall.service`、`journalctl -u msboost-relay-firewall.service -n 80 --no-pager`。受管规则用 `iptables -S MSBOOST-RELAY` / `ip6tables -S MSBOOST-RELAY` 查看，不列入 `ufw status` 的普通端口列表，见 [UFW 框架说明](https://manpages.debian.org/bookworm/ufw/ufw-framework.8.en.html)。新 Agent 先协商状态扩展，避免旧面板严格 JSON 校验拒绝同步；只升级面板不会让旧 Agent 自动获得此功能。

Relay 只支持 `keep_last`：面板离线时保留最后确认配置。新版本拒绝 lease 运行模式。离线期间新撤销、过期、封禁或流量决定延后下发；机器/进程重启不能保留原 TCP 会话。

旧站升级前核对实际节点协议，发现旧节点会列出名称和地址并保留当前站点。先在旧面板升级节点确认在线，再升级网站。不会把历史记录直接改成“已支持新版”；尚未注册的空节点不阻止升级。

## 恢复和重装

网站从备份恢复后，使用面板服务器 `msboost relay-reconnect`（菜单 16），见[恢复向导](relay-recovery.md)，无需手工传送 JSON。不要用日常安装代替恢复或删除持久状态绕过核对。

舍弃旧节点重新安装前，先删除关联线路并确认全部转发停止，再从后台申请全新重装。仅使用该次申请提供的 `--fresh-reset --acknowledge-relay-restart` 命令；这不是无损升级。

## 卸载本机节点

在节点维护窗口复制卸载命令，到对应 VPS 的 root 终端运行。明确传入 `--acknowledge-stop` 后，脚本会停止 Relay 和其转发子进程、保存私有备份，再清理本机组件。面板离线、节点记录已删除、正在转发或处于恢复核对状态均不阻止本机卸载，无需后台登录凭据。

先检查而不停止服务，可在命令末尾添加 `--check`。节点 ID、面板地址必须与本机身份匹配；路径归属不明、身份文件损坏或进程无法真正停止时会说明原因并保留文件，不提供误删其他服务的强制开关。

既有备份全部保留，本次备份在 `/var/backups/msboost-agent/uninstall-relay.XXXXXXXX/relay.tar.gz`，仅 root 可读，包含私有凭据，请勿公开。同机执行机及共享程序会保留。卸载中断连接，备份不是无损恢复承诺。

卸载不自动删除面板记录、关联业务或释放预留端口；后台仍有记录时，另行处理关联业务后删除或封存。网站 uninstall/purge 不处理独立 Agent 或客户 VPS。修复旧版卸载问题只需下载新卸载脚本，不需要先升级面板或 Agent。


## 执行机卸载与残缺安装清理

后台执行机、节点列表提供“本机清理说明”。在目标 VPS 下载同版本 `deploy/cleanup-agent.sh`，先使用 `--capability executor --check` 或 `--capability relay --check` 查看受管路径；确认后改用 `--cleanup`，终端要求输入角色确认词及本机主机名。默认扫描不停止服务、不删除文件。

该入口支持受管身份或环境文件缺失、损坏的安装，仍核对受管标记、固定路径及进程已经停止。只清理选中的角色，保留共享 Agent/GOST、另一个角色和既有备份。本次备份在 `/var/backups/msboost-agent/cleanup-角色.XXXXXXXX/components.tar.gz`，仅 root 可读。遇到外来文件、未知服务覆盖配置或停止失败，会保留文件并说明原因。

执行机请先在后台停用，等已有任务结果回传后再清理；紧急直接停止可能丢失内存中的结果。程序版本、最近真实同步与管理认证分别显示，systemd active 不能代替连接或转发验收。
