# Cloudflare 客户端 IP

路径为「访客 → Cloudflare（可选）→ 宿主机 Caddy → 回环发布端口 → 应用」。应用仅信任专用 Docker 桥接网关 `172.30.86.1/32`，不信任整个 Docker 子网或所有公网地址。Docker Engine 必须为 28 或更新版本。

本站片段用实际 TCP 对端 `remote_ip` 匹配官方 Cloudflare IPv4/IPv6 清单（2026-09-29 核对），只在匹配且头部形态符合 IP 时采用 `CF-Connecting-IP`。不修改共享 Caddy 的全局 trusted_proxies 配置，不影响其他网站。

直接连接时使用实际对端地址；不会接受客户端伪造的 X-Forwarded-For。Cloudflare 头缺失或明显无效也使用实际对端，不回退信任自报 XFF。最终应用继续用严格 IP 解析，不能把畸形字符串当成合法地址。其他 IP 身份头在转发时移除，XFF 覆盖为一个值。

Cloudflare Worker、Pseudo IPv4 的覆盖模式、移除访客 IP 的 Managed Transform 可能改变或隐藏访客地址；显示 NAT/边缘地址不代表主机独占 IP。本站不自动修改 Cloudflare 设置，也不提供只允许 Cloudflare 访问源站的防火墙规则。

地址清单按版本审核，不在启动时在线下载信任。变更须同步配置和测试；不要添加全网、private_ranges 或未经审核的负载均衡器。

实际 Caddy 回归：设置 CADDY_TEST_BINARY 为已验证二进制，执行 `go test ./deploy -run TestRealCaddyClientIP -count=1 -v`。覆盖直连伪造头、可信来源、IPv6、缺失/异常头和单值 XFF。可信边缘模拟只修改隔离测试配置，监听临时回环端口，不更改现网配置。

官方参考：[Cloudflare IPv4](https://www.cloudflare.com/ips-v4)、[IPv6](https://www.cloudflare.com/ips-v6)、[头部说明](https://developers.cloudflare.com/fundamentals/reference/http-headers/)、[Caddy 请求匹配](https://caddyserver.com/docs/caddyfile/matchers)。
