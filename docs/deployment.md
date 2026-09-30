# MSBOOST v2.0 部署与运维

控制面支持 Debian 12/13（amd64/arm64）。v2.2.2 支持从 v2.0.1 的 systemd Caddy 安装备份后升级，**不支持从旧 Docker Caddy 安装升级或迁移。** 使用旧 Docker Caddy 时先另行保存重要数据，使用对应旧版本管理器清理本站，再全新安装；全新安装会生成新的管理员密码与主密钥。客户 VPS 工具和独立 Agent 不随控制面安装被清理。

## 安装

域名解析到 VPS，开放 TCP 80/443。使用 Cloudflare 时设置 Full (strict)，确保源站可以签发证书。安装器从官方源安装 Docker 和 Caddy；Docker Engine 需要 28 或更新版本，防止旧版回环端口发布的网络隔离问题。已有 Docker 不会被静默升级；已有自定义 Caddy/API 服务不会被自动接管。

```sh
curl -fsSL --proto '=https' --tlsv1.2 https://raw.githubusercontent.com/mozziexwz/node/v2.2.2/install.sh -o /root/msboost-install.sh
# 审核脚本与正式 Release 资产后执行。
bash /root/msboost-install.sh install --domain panel.example.com --email 12345678@qq.com
```

安装优先拉取 GHCR 预构建镜像；失败时下载并校验同版本 Release 预构建归档，不静默本机编译。两种来源都失败则保留现场并停止。应用镜像与 PostgreSQL 记录不可变身份；正式资产以 [Release](https://github.com/mozziexwz/node/releases/tag/v2.2.2) 为准。

首次管理员密码保存在 root-only `/opt/msboost/.env`。不要分享该文件或完整 `docker compose config` 输出。应用和数据库仍由 Compose 管理，只有 Caddy 改成官方系统服务。

### 内存与备份容量

镜像默认设置 `GOMEMLIMIT=384MiB`，让 Go 在大附件备份时更早回收内存；可在私有 `.env` 中调整。这是软目标，不是容器内存上限，活动数据、数据库和系统仍会额外占用内存。接近附件容量上限的控制面建议至少预留 2 GiB 内存，仍需按实际数据量和并发监测，不能把 100 MiB 备份文件上限等同于进程内存需求。低内存测试中默认回收策略曾触及 1 GiB 限制；配置软目标后同组容量用例通过，但这不代表所有负载均可在 1 GiB VPS 上运行。

## 配置分别放在哪里

| 路径 | 用途 | MSBOOST 升级行为 |
| --- | --- | --- |
| `/etc/caddy/Caddyfile` | 共享入口和全局设置 | 首次接入只添加一次站点 import，并保留原文件副本；不整份生成 |
| `/etc/caddy/sites-enabled/msboost.caddy` | 自动生成的本站反代 | 仅本站模板需要更新时替换；手改冲突会停止，不覆盖 |
| `/etc/caddy/sites-enabled/其他名字.caddy` | 你的其他网站、软件 | 不改动、不备份、不删除 |
| `/etc/caddy/msboost-custom/*.caddy` | 本站自定义指令 | 升级保留，整站备份包含，彻底清理删除 |
| `/opt/msboost/.env` | 网站私有配置、密钥 | 升级和修复保留；不传给 Caddy 服务 |
| `/var/lib/caddy` | Caddy 共享证书等运行数据 | 不删除，不纳入 MSBOOST 整站备份 |

应用仅监听宿主机 `127.0.0.1:18080`，数据库无宿主机端口；80/443 由系统 Caddy 管理。其他宿主机应用可以直接反代到 `127.0.0.1:自己的端口`。其他容器应用也应把其端口发布到回环地址，不需要加入 MSBOOST 网络。

## 添加其他网站示例

以 root 编辑 `/etc/caddy/sites-enabled/my-app.caddy`：

```caddyfile
app.example.com {
    reverse_proxy 127.0.0.1:3000
}
```

保存后检查完整配置，成功才重载：

```sh
caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile &&
systemctl reload caddy
```

无需修改 MSBOOST Compose。其他网站需要各自的数据、证书和配置备份，MSBOOST 不承担这些站点的恢复。

MSBOOST 本站扩展放在 `/etc/caddy/msboost-custom/自定义英文名.caddy`，仅包含站点内指令，不再套域名外层或全局块。文件名只用英文字母、数字、下划线和连字符；禁止链接和嵌套目录。文件建议 root:caddy、0640。外部引用的证书、文件等仍须另行备份。自定义完也要执行完整校验和重载。

## 日常维护

运行 `msboost` 使用中文菜单；也可执行：

```sh
msboost status
msboost logs
msboost upgrade --version v2.2.2
msboost repair
msboost disaster-backup
msboost disaster-status
msboost uninstall
msboost purge
```

以上 upgrade 仅适用于已使用 systemd Caddy 的新版安装。升级先备份原配置和数据库，再替换应用；健康检查失败尝试恢复旧应用与本站配置，不自动回滚业务写入或数据库迁移。

普通升级不升级 Caddy 软件包，本站生成配置没有变化时不重载 Caddy。需要改变本站代理配置时先验证完整共享配置，再通过 systemd reload；失败只回退本站文件，不覆盖其他网站。Caddy reload 通常不停止普通 HTTP 服务，但某些 WebSocket/长连接可能重连，不能承诺所有软件零中断。

`uninstall` 移除本站反代入口、容器和专用网络，保留所有 MSBOOST 数据及扩展，用 `repair` 恢复。`purge` 需要两次确认，仅删除本站入口/扩展、`/opt/msboost` 和 `msboost_app_data`、`msboost_database_data` 两个业务卷。不会停止或卸载共享 Caddy，不删除共享证书、其他站点、其他容器、Docker 或独立 Agent。

整站备份、异地验证与恢复见[备份说明](disaster-backup.md)。恢复只支持 v2.0 新格式，并要求目标没有 MSBOOST 安装或同名资源；目标可以有其他由标准 Caddyfile 管理的网站。自定义 JSON/API Caddy、特殊插件/服务单元由管理员自行审核，不自动改写。
