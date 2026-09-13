# MSBOOST v0.2.2 · 真实 VPS 回归修复

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
