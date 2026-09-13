# Cloudflare 客户端 IP 信任链

默认 Compose 的路径是「客户端 → Cloudflare（可选）→ Caddy → 应用」。应用只信任 Caddy 的固定地址 `172.30.86.2/32`；不扩大为 Cloudflare 段、整个 Docker 网络或公网。

`deploy/Caddyfile` 为 Caddy 配置 15 条 IPv4、7 条 IPv6 官方 Cloudflare 网络（2026-09-13 核对）。只有实际 TCP 对端属于这些网络时，Caddy 才解析 `CF-Connecting-IP`，缺失时按 `X-Forwarded-For` 从右向左找客户端。非 Cloudflare 对端的自报 IP 头无效，采用实际对端地址。相关行为见 [Caddy 可信代理文档](https://caddyserver.com/docs/caddyfile/options#trusted_proxies) 和 [严格解析文档](https://caddyserver.com/docs/caddyfile/options#trusted_proxies_strict)。需要 Caddy 2.8 或更高版本。

Caddy 给应用的 `X-Forwarded-For` 被覆盖为单个已验证的 `{client_ip}`，而不是「访客、Cloudflare 边缘」原始链；其他 IP 身份头会移除。这样应用不会把最后一个 Cloudflare 边缘地址误认为执行机 IP，且不接受直连伪造值。执行机表显示的是最近心跳观察到的来源，可能为 NAT 出口；已有旧 IP 会在下一次成功心跳更新，不代表主机独占公网地址。

## 运维边界

- Cloudflare 地址按发布版本维护，不在启动时自动下载并信任。更新前核对官方 [IPv4 清单](https://www.cloudflare.com/ips-v4)、[IPv6 清单](https://www.cloudflare.com/ips-v6)，同步修改配置和回归测试。不配置 `0.0.0.0/0`、`::/0` 或 `private_ranges`。
- 域名 DNS 直接解析到源站也可使用；不能因为前面还有额外负载均衡器，就把该中间层未经审核地加入可信列表。若 Docker/rootless 网络隐藏 TCP 来源，仍会拒绝信任头并显示该中间层地址，需单独核验网络而非放宽到全网。
- Cloudflare 的同域 Worker 可以改变所报告地址；跨域 Worker 可能显示 Cloudflare 的固定 Worker 地址。启用 Pseudo IPv4 的 `Overwrite Headers` 会改写 IPv6 访客地址，若需原始地址应由域名管理员选择关闭该模式或 `Add Header`。移除访客 IP 的 Managed Transform 也会限制识别能力；这些均不由本站自动修改。[Cloudflare 头部说明](https://developers.cloudflare.com/fundamentals/reference/http-headers/)
- 此配置不是「只允许 Cloudflare 访问源站」的防火墙或源站认证。直连仍可访问，但不能伪造 IP 头；应用权限、CSRF 和凭据认证不变。
- 普通应用升级保留原 Caddy 镜像 digest。部署前应在隔离环境用实际固定镜像执行 `caddy validate`；过旧镜像不应直接套用新指令，需计划升级依赖。配置本身不修改防火墙或 Cloudflare 账户。

## 隔离回归

`go test ./deploy -count=1` 检查官方网段快照、严格解析和应用固定可信边界。设置 `CADDY_TEST_BINARY` 为经过校验的 Caddy 二进制，再运行 `go test ./deploy -run TestRealCaddyClientIP -count=1 -v`，会启动临时回环 HTTP 服务，真实验证：直连伪造头被忽略、CF 优先、IPv6、XFF 右侧解析、异常头回退及应用仅收到单值 XFF。

可信 CF 路径测试只在临时配置把 CF 段替换成回环测试地址，不修改生产配置，不冒用公网 CF 地址，不读业务数据、不占用 80/443、不修改运行中的 Caddy。
