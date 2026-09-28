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

Relay 只支持 `keep_last`：面板离线时保留最后确认配置。新版本拒绝 lease 运行模式。离线期间新撤销、过期、封禁或流量决定延后下发；机器/进程重启不能保留原 TCP 会话。

旧站升级前核对实际节点协议，发现旧节点会列出名称和地址并保留当前站点。先在旧面板升级节点确认在线，再升级网站。不会把历史记录直接改成“已支持新版”；尚未注册的空节点不阻止升级。

## 恢复和重装

网站从备份恢复后，使用面板服务器 `msboost relay-reconnect`（菜单 16），见[恢复向导](relay-recovery.md)，无需手工传送 JSON。不要用日常安装代替恢复或删除持久状态绕过核对。

舍弃旧节点重新安装前，先删除关联线路并确认全部转发停止，再从后台申请全新重装。仅使用该次申请提供的 `--fresh-reset --acknowledge-relay-restart` 命令；这不是无损升级。

Agent 卸载使用对应版本的 `deploy/uninstall-agent.sh`，阅读帮助并确认能力。网站 uninstall/purge 不处理独立 Agent 或客户 VPS，不要手工删除身份、密钥或所有权标记。
