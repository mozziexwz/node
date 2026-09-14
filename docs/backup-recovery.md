# 备份、安全恢复与完整灾难恢复

基础流程适用版本：v0.2.0 起。所有步骤都不会重装客户 VPS，也不会恢复客户 VPS 的磁盘。下文固定 v0.2.0 下载命令仅用于旧版纯 v1 恢复，不适用于含 v2 状态的新部署；不能用旧工具解释新恢复/资源保留语义。

本地未发布的 v2 / keep_last 恢复必须使用配套新恢复程序：保留端口、旧目标、命令与撤销关联，进入 `recovery_required`，管理令牌失效不代表旧业务停止。支持恢复协议的节点按 [受信恢复手册](relay-recovery.md) 逐规则核对；不得按45秒等待后直接重装/释放资源。已发布 v0.2.3 不具备该入口，发布与实测状态见 [补充2.3整改状态](supplement-2.3-status.md)。

## 两种模式

| 项目 | 网页安全恢复 | 离线完整灾难恢复 |
| --- | --- | --- |
| 适用情况 | 找回设置、文章附件、误删且未被当前规则关联的线路/节点 | 原库损坏、整站迁移、确实需要完整快照 |
| 用户、密码、邮箱关系、余额与权益 | 保留当前完整数据 | 使用备份时的数据 |
| 订单、账本、卡密、支付与请求去重 | 保留当前全部集合 | 使用备份时的完整关联数据，之后必须与支付商对账 |
| 流量游标、月份、用户规则与规则归档 | 保留当前数据；当前规则暂停 | 使用快照；规则暂停，不重放流量或任务 |
| 设置、文章与附件 | 按快照恢复；支付/购买开关保留当前值 | 按快照恢复，支付与购买强制关闭 |
| 线路、节点、执行机定义 | 恢复定义；当前用户规则需要的现有线路/节点保留现版 | 按快照恢复定义 |
| 原数据库 | 同一事务安全合并，先保存私有回滚副本 | 不读取、不覆盖、不删除，只创建全新隔离数据库 |
| 会话、验证码与 Agent 凭据 | 全部失效，节点禁用，保持维护 | 同左；待人工重新关联 |

安全恢复不是财务回滚工具。新增集合默认保留当前内容；没有对单独几张财务表进行容易遗漏的拼接。被当前用户规则引用的线路和节点不会被旧拓扑覆盖，预检会明确列出保留项。历史用户被删除后仍存在的订单、账本、卡密使用记录与计量归档保留原用户 ID，不会把它们转给其他账户。

## 网页安全恢复

1. 在设置开启维护模式，结束运行/排队任务，暂停本站转发。纯 v1 等待已下发租约失效；v2 / 混合链路核对预检的恢复资源与明确停止证明，不能用等待时长代替确认。
2. 在“备份与恢复 → 恢复备份”上传 `.msb`，点击“只预检，不恢复”。查看新增、替换、移除项和保留的当前关联。
3. 如有阻止条件，处理后重新预检。预检之后设置、拓扑或用户规则发生变化时，服务端会拒绝旧预检，要求重做。
4. 确认安全恢复。恢复前必须成功写入一个 0600、仅本机保存的加密回滚副本；失败则不提交数据库变更。该副本标记“回滚保护”，不参与自动删除，也不能从网页删除。
5. 重新登录，核对文章附件与配置。纯 v1 按预检结果重新注册/关联；含 v2 状态时先走本机受信恢复或独立停止核对，不能直接注册旧身份、重建规则或解除冻结。维护与经营开关另行审核开放；旧任务不会自动重放。

被保留的财务和流量使用恢复事务开始时的最新数据，不使用预检时缓存的数据。恢复不会重新发送支付通知或补发历史权益。

## 完整灾难恢复的边界

`msboost-restore` 只写入**全新目标**，没有“覆盖现有数据库”选项，也不会自动更改 Compose 或重启服务。因此原库损坏导致 `pg_dump` 失败，不会阻止从健康备份向新库恢复。

必须使用创建备份时的原主密钥：

- 标准一键部署：原 `/opt/msboost/.env` 中的 `MASTER_KEY`，由 Compose 环境传给工具；不要打印或发给他人。
- 未通过环境提供密钥的部署：原 `app_data` 卷中的 `/app/data/master.key`，是 **32 字节原始二进制文件**，使用 `--master-key-file /app/data/master.key`。
- 两种密钥表示不能混用；工具不会生成替代密钥。密钥错误或快照校验失败时，不创建目标库。

新库使用快照时间点的数据，**备份之后的注册、付款、卡密使用、权益与流量不会凭空找回**。即使恢复成功，也必须保持维护，与支付服务商及保留的原库对账后才能开放支付与购买。渠道启用状态在完整恢复时强制关闭；维护期间的支付回调只可能进入待复核，不自动发放权益。

### Debian 12 / Docker Compose：同一健康 PostgreSQL 实例中恢复到新库

以下适用于标准一键部署的 `/opt/msboost`，包含已有 v0.1.x 且原业务库损坏的情况。若原数据库实例本身无法启动，应在另一台 VPS 或隔离的健康 PostgreSQL 实例执行新库导入，不要修复时覆盖原数据库卷。v0.1.x 的 Compose 写死业务库名，下面单独提供经过校验的管理文件更新步骤；**不要为了取得恢复工具而强行对损坏库执行普通升级**。

先将加密备份上传为 `/root/msboost-recovery/backup.msb`。下面在 root Bash 中运行；实际操作前备份原 `.env` 与原主密钥到独立安全位置。文件均留在本机，不上传第三方。

下载并验证对应架构的独立恢复工具，不要求旧应用镜像已经包含它：

```bash
set -euo pipefail
install -d -m 700 /root/msboost-recovery
cd /root/msboost-recovery
recovery_version=v0.2.0
recovery_arch=$(dpkg --print-architecture)
case "$recovery_arch" in amd64|arm64) ;; *) echo '仅支持 amd64/arm64'; exit 1 ;; esac
recovery_asset="msboost-restore-linux-${recovery_arch}"
recovery_url="https://github.com/mozziexwz/node/releases/download/${recovery_version}"
curl -fL --proto '=https' --tlsv1.2 "$recovery_url/$recovery_asset" -o msboost-restore
curl -fL --proto '=https' --tlsv1.2 "$recovery_url/SHA256SUMS" -o SHA256SUMS
awk -v asset="$recovery_asset" '$2 == asset {print $1 "  msboost-restore"; found=1} END {if (!found) exit 1}' SHA256SUMS | sha256sum --check --strict -
chmod 700 msboost-restore
test -f backup.msb
chmod 600 backup.msb
```

停止对外服务并锁住部署管理，数据库容器保持运行。以下会产生停机；不会停止客户 VPS 的独立服务，也不会删除 Docker 卷。

```bash
cd /opt/msboost
exec 9>/run/msboost-deploy.lock
flock -n 9 || { echo '另一个安装、升级或恢复操作正在运行'; exit 1; }
compose_recovery() (
  unset MSBOOST_IMAGE MSBOOST_VERSION MSBOOST_DOMAIN MSBOOST_SITE_ADDRESS MSBOOST_DATABASE_NAME
  unset POSTGRES_PASSWORD POSTGRES_IMAGE CADDY_IMAGE DOCKER_HOST DOCKER_CONTEXT DOCKER_DEFAULT_PLATFORM
  MSBOOST_ENV_FILE=/opt/msboost/.env docker --host unix:///var/run/docker.sock compose --project-name msboost \
    --env-file /opt/msboost/.env -f /opt/msboost/deploy/compose.yml "$@"
)
recovery_stamp=$(date -u +%Y%m%d%H%M%S)
install -m 600 /opt/msboost/.env "/root/msboost-recovery/env-before-${recovery_stamp}"
compose_recovery stop caddy server
compose_recovery ps
sleep 60
```

**已有 v0.1.x 的部署：在锁仍持有、服务已停止时运行这一段。** 它下载并校验 v0.2.0 源码发布包，仅提取两个管理文件；先私有备份原文件，再更新管理脚本及 Compose。不会覆盖 `.env`、Caddyfile、证书、业务库、应用镜像或客户数据，不启动服务，也不执行数据库升级/导出。标准部署可直接执行；如果自己修改过 Compose，请先审查新旧差异，保留定制内容并确保新 `DATABASE_NAME` 参数正确。

```bash
cd /root/msboost-recovery
recovery_bundle="msboost-deploy-${recovery_version}.tar.gz"
curl -fL --proto '=https' --tlsv1.2 "$recovery_url/$recovery_bundle" -o "$recovery_bundle"
awk -v asset="$recovery_bundle" '$2 == asset {print; found=1} END {if (!found) exit 1}' SHA256SUMS | sha256sum --check --strict -
recovery_stage=$(mktemp -d /root/msboost-recovery/manager-stage.XXXXXX)
tar --extract --gzip --file "$recovery_bundle" --directory "$recovery_stage" \
  --no-same-owner --no-same-permissions deploy/manage.sh deploy/compose.yml
test -f "$recovery_stage/deploy/manage.sh" && test ! -L "$recovery_stage/deploy/manage.sh"
test -f "$recovery_stage/deploy/compose.yml" && test ! -L "$recovery_stage/deploy/compose.yml"
bash -n "$recovery_stage/deploy/manage.sh"
recovery_previous="/root/msboost-recovery/manager-before-${recovery_stamp}"
install -d -m 700 "$recovery_previous"
install -m 600 /opt/msboost/deploy/manage.sh "$recovery_previous/manage.sh"
install -m 600 /opt/msboost/deploy/compose.yml "$recovery_previous/compose.yml"
install -m 700 "$recovery_stage/deploy/manage.sh" /opt/msboost/deploy/manage.sh
install -m 600 "$recovery_stage/deploy/compose.yml" /opt/msboost/deploy/compose.yml
cd /opt/msboost
```

此时 `.env` 中的 `MSBOOST_IMAGE`/镜像 ID 和 `MSBOOST_VERSION` 仍保持原值；只是让恢复后的管理脚本能对选定新库做后续备份、升级，不会错误导出原损坏库。若此段任一步失败，保持服务停止，先处理文件/网络错误，不进行后续切换。

如旧库仍可读，建议额外保存只读导出；旧库已损坏时可跳过，**不能用失败的 `pg_dump` 强制阻止后续恢复**。新库导入不会接触旧业务库。

选择一个从未存在的新库名。此处的恢复命令利用现有 Compose 数据库网络、密码和原 `MASTER_KEY`，但运行刚校验的独立工具：

```bash
recovery_database="msboost_restore_${recovery_stamp}"
compose_recovery run --rm --no-deps --user 0:0 \
  --volume /root/msboost-recovery:/recovery:ro \
  --entrypoint /recovery/msboost-restore server \
  --backup /recovery/backup.msb \
  --postgres-new-database "$recovery_database" \
  --confirm-disaster-restore
```

如果使用的是原始 `master.key` 文件，在命令末尾再加 `--master-key-file /app/data/master.key`。不要用新安装自动生成的密钥替换原密钥。工具检查全部数据、创建新库、事务导入并读回核验，成功才输出“完整快照已写入全新隔离数据库”。同名库已存在会直接拒绝；失败创建的新隔离库保留供检查，不自动删除。

确认工具成功后，修改 `.env` 中唯一的 `MSBOOST_DATABASE_NAME` 为输出的新库名；仅改这个字段，不改密码和主密钥。以下命令只替换精确键或追加不存在的键，不修改其他配置：

```bash
if grep -q '^MSBOOST_DATABASE_NAME=' /opt/msboost/.env; then
  sed -i "s/^MSBOOST_DATABASE_NAME=.*/MSBOOST_DATABASE_NAME=${recovery_database}/" /opt/msboost/.env
else
  printf '\nMSBOOST_DATABASE_NAME=%s\n' "$recovery_database" >> /opt/msboost/.env
fi
chmod 600 /opt/msboost/.env
compose_recovery up -d --no-deps --pull never --wait server
compose_recovery up -d --no-deps --pull never --wait caddy
flock -u 9
exec 9>&-
```

登录后台确认维护仍开启，核对用户数量、钱包账本、卡密状态、订单/待复核付款、流量及文章附件。重新关联 Agent，检查线路，重新生成需要的规则。**不会自动解除维护或开启支付。** 管理员完成对账后再主动开放功能。

若原版本为 v0.1.x，切换成功且新库健康后，再使用固定版本的新引导脚本完成正常应用升级（引导脚本会校验发布包，并对 `MSBOOST_DATABASE_NAME` 指定的新库做备份；不是对原损坏库）。继续保持维护，确认升级和对账都成功后再开放营业：

```bash
curl -fsSL --proto '=https' --tlsv1.2 \
  https://raw.githubusercontent.com/mozziexwz/node/v0.2.0/install.sh \
  -o /root/msboost-recovery/install-v0.2.0.sh
bash /root/msboost-recovery/install-v0.2.0.sh upgrade --version v0.2.0
```

原业务库（通常名为 `msboost`）仍在原卷内，不要执行 `docker compose down -v`，也不要删除 `msboost_database_data`。若尚未开放营业且需要撤回切换：先停止 `caddy server`，从 `/root/msboost-recovery/env-before-*` 恢复本次保存的 `.env` 后再启动。已经发生新的业务写入后，不得直接来回切库，应先停止服务并人工对账。

### 全新 PostgreSQL 实例 / 非 Compose

在停写、隔离环境运行同一工具，原库无需可读。通过安全终端环境或密钥管理器提供 `MASTER_KEY` 与 `MSBOOST_RESTORE_ADMIN_URL`，后者格式为 `postgres://用户:密码@数据库主机:5432/postgres?sslmode=require`。工具只允许连接 `/postgres` 维护数据库并 `CREATE DATABASE msboost_restore_...`，不会连接/覆写业务库；账号需要创建数据库权限。不要把含密码 URL 写在公开命令历史或日志中。

```bash
./msboost-restore --backup /secure/backup.msb \
  --postgres-new-database msboost_restore_recovery1 \
  --confirm-disaster-restore
```

停止旧控制台服务后，管理员再手动将新控制台数据库连接切到输出的新库，使用原主密钥。原库或损坏卷离线保留；工具不自动删除或“修复”它们。

### 全新 SQLite 目录

适用于 SQLite 部署或离线检查，不是将生产 PostgreSQL 悄悄改为 SQLite：

```bash
./msboost-restore --backup /secure/backup.msb \
  --master-key-file /secure/original-master.key \
  --sqlite-output-dir /secure/msboost-restored-new \
  --confirm-disaster-restore
```

父目录必须存在，输出目录必须不存在。目录权限为 0700，保存恢复库、原主密钥和输入的加密快照。先停掉原服务，再人工配置指向此新目录；工具不自动启动服务。

## 删除备份与保留策略

备份历史显示大小、保存目标和本机目录，每份备份有“删除本机副本”。确认后仍保留审计记录和远端文件，不向 SFTP 发出删除命令。已删除本机文件不能从网页撤销。

删除与按保留期清理共用以下保护：

- 有备份或恢复进行中时拒绝清理。
- 重新读取、解密并校验有效本机副本；损坏、缺失副本不计入最少份数。
- 删除后不得低于配置的最少有效副本（未配置时为 3 份）。
- 恢复前回滚副本受保护。确需处理这些副本，由管理员离线审查后单独管理，不通过网页解除保护。
- 不跟随符号链接，不接受路径型备份 ID；先私有隔离待删文件，数据库记录提交失败时尝试原位回迁。

远程清理由管理员在远程目标独立执行。本站移除远程目标配置不会删除远程文件。
