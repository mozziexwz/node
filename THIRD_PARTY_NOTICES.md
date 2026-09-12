# 第三方来源、脚本校验与许可说明

本文件记录当前源码导入及运行依赖的来源，不为整个仓库指定统一许可，也不是完整的供应链物料清单或法律意见。仓库所有者尚未选择本项目自有代码许可，因此未添加根 `LICENSE`；公开可见不表示获得任意再分发或再授权许可。第三方材料的权利和条件由各自上游许可决定。

## 1. 用户提供的 MSBOOST 脚本与需求

`installers/node/msboost.sh` 从用户提供的 `mita一键脚本/msboost.sh` 原样导入，没有在该副本中添加本项目许可声明。来源文件未提供可据以给整个工程授权的统一许可；使用或分发前应由权利人明确其授权范围。

UI 样式和产品需求来自用户提供的 `msboost-v4.1-ui` 文件夹；需求留档为 `docs/requirements/product-v4.1.md`。v4.1 是产品规划版本，不是本项目发布版本，也不是全部完成的声明。导入记录见 [source-manifest.json](docs/requirements/source-manifest.json)。

当前字节校验值：

| 文件 | SHA256 |
|---|---|
| `installers/node/msboost.sh` | `593b3f612e7537afe89444252883eb64f64bfa3ec4b0d367c56042d8b461ee24` |
| `docs/requirements/product-v4.1.md` | `a18e6dd69414aa0ce6275299fa53c6143e8a05d3ac0ed1468a2713dc0edb866c` |

MSBOOST 脚本运行时使用 [MetaCubeX/mihomo](https://github.com/MetaCubeX/mihomo)，默认版本 `v1.19.30`，可受 `MSBOOST_ENGINE_VERSION` 环境变量影响；上游 [该版本 LICENSE](https://github.com/MetaCubeX/mihomo/blob/v1.19.30/LICENSE) 为 GPLv3。相应下载、使用与分发应保留上游要求的许可及来源信息。脚本还读取 `meta-rules-dat` 的 [gfw.mrs](https://github.com/MetaCubeX/meta-rules-dat/blob/meta/geo/geosite/gfw.mrs)；该 URL 的资源会随上游分支变化，不能把脚本自身哈希误认为所有运行时资源均已冻结。

## 2. bin456789/reinstall

用户指定来源为 [reinstall.sh 的 main 链接](https://raw.githubusercontent.com/bin456789/reinstall/main/reinstall.sh)。本仓库没有在客户执行时直接追踪此可变链接，而是保存以下固定提交：

- 上游仓库：[bin456789/reinstall](https://github.com/bin456789/reinstall)
- 固定提交：[`b8eadd59123eccc7b591ef921f85f139316b1515`](https://github.com/bin456789/reinstall/commit/b8eadd59123eccc7b591ef921f85f139316b1515)
- 原始源文件：[固定提交 reinstall.sh](https://raw.githubusercontent.com/bin456789/reinstall/b8eadd59123eccc7b591ef921f85f139316b1515/reinstall.sh)
- 许可：GNU General Public License version 3，全文原样保存在 [installers/reinstall/LICENSE](installers/reinstall/LICENSE)，[上游同提交许可](https://github.com/bin456789/reinstall/blob/b8eadd59123eccc7b591ef921f85f139316b1515/LICENSE)。

| 文件 | SHA256 | 用途 |
|---|---|---|
| `installers/reinstall/reinstall.upstream.sh` | `da118210a149ec055aaa9b0c4d48a3c71ec71268976fda27e7cf549113898458` | 保存原始上游字节 |
| `installers/reinstall/reinstall.sh` | `5683838626eeda9236c3d119e497668c25b2db70d586c979100bcb6adc888a1c` | 实际执行副本 |

本项目的修改仅将脚本内部指向该仓库 `main` 的资源 URL 固定到同一上游提交，包括对应镜像/CDN地址。修改说明和机器可读来源见 [manifest.json](installers/reinstall/manifest.json)。这不表示所有外部操作系统镜像、软件仓库或下载资源都已固定或验收。

维护导入工具 `scripts/vendor-reinstall.mjs` 会访问上游、更新源文件与清单；只能作为有意审查的维护操作使用，不是客户运行时更新步骤。重新导入后应审查补丁、许可、哈希和可丢弃 VPS 实测结果。本项目的控制面适配不改变上游脚本的 GPLv3 许可，也不授予对上游材料另行再授权的权利。

## 3. GOST

Executor 的客户中转安装和 Relay Agent 使用独立的 [go-gost/gost](https://github.com/go-gost/gost) 进程。当前安装器固定 [v3.3.0](https://github.com/go-gost/gost/releases/tag/v3.3.0)，上游 [LICENSE](https://github.com/go-gost/gost/blob/v3.3.0/LICENSE) 为 MIT，版权声明包括 `Copyright (c) 2016 ginuerzh`。

安装器使用的官方 Linux 归档及其 SHA256 如下，来源为同版本 [checksums.txt](https://github.com/go-gost/gost/releases/download/v3.3.0/checksums.txt)：

| 架构 / 归档 | SHA256 |
|---|---|
| `gost_3.3.0_linux_amd64.tar.gz` | `676fb7f78d267b6ae73df719c0c7f2b565dde7147da935cfafbc1e1da558b6d5` |
| `gost_3.3.0_linux_arm64.tar.gz` | `d03699e3f385d4ff5dad68046712adfcc7515325a064d2ab046e0bece30f8f8f` |

准确下载链接及安装流程见 [Agent 安装说明](docs/agent-installation.md)。这些是上游 GOST 资产，不是本项目尚未发布的 Agent 二进制。分发 GOST 或包含它的交付物时应一并保留其许可与版权声明；校验摘要不能替代许可文本。

FLVX `2.2.0-alpha4` 在需求中作为参考方向；当前工程未声明完成 FLVX 全量代码迁入、兼容性适配或原项目授权的重新授予。现有实现边界见 [安全模型与限制](docs/security-and-limits.md)。

## 4. 应用与构建依赖

实际 npm 版本以 [apps/web/package-lock.json](apps/web/package-lock.json) 为准，Go 模块版本以 [go.mod](go.mod) 和 [go.sum](go.sum) 为准。以下列出主要组件，不穷举所有间接依赖：

| 组件 | 上游与许可位置 |
|---|---|
| React / React DOM | [facebook/react](https://github.com/facebook/react)，安装包 `LICENSE`（MIT） |
| Lucide React | [lucide-icons/lucide](https://github.com/lucide-icons/lucide)，安装包 `LICENSE`（ISC，并保留其中归属说明） |
| Marked | [markedjs/marked](https://github.com/markedjs/marked)，安装包 `LICENSE.md`（MIT） |
| DOMPurify | [cure53/DOMPurify](https://github.com/cure53/DOMPurify)，包声明 `MPL-2.0 OR Apache-2.0`，保留相应许可文件 |
| pgx / PostgreSQL 驱动 | [jackc/pgx](https://github.com/jackc/pgx)，以所用模块版本的许可为准 |
| SQLite Go 驱动 | [modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite)，以所用模块及其间接依赖许可为准 |
| SSH / 密码学、SFTP | [golang.org/x/crypto](https://pkg.go.dev/golang.org/x/crypto)、[pkg/sftp](https://github.com/pkg/sftp)，以所用模块版本许可为准 |
| TypeScript、Vite、Playwright 等构建测试工具 | 完整列表、版本与许可声明见 npm 锁文件及对应安装包 |

Docker 构建还使用 Node.js、Go、Debian 等基础镜像，Compose 使用 PostgreSQL 和 Caddy 镜像。它们及镜像内系统软件有独立的许可、版权与更新要求；镜像标签不等于不可变摘要。本仓库未提供完整镜像 SBOM 或所有传递依赖的法律审查结论。正式再分发前，应按所交付的精确代码、二进制和镜像版本归档全部适用的许可、通知及需要提供的对应源码。
