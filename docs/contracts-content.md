# 文章、公开页面与站点设置 API

浏览器使用会话 Cookie，写请求需要 `X-CSRF-Token`。JSON 使用 camelCase，时间为 Unix 毫秒，响应不缓存，错误为 `{ "error": "中文说明" }`。

## 页面与可见性

| 地址 | 访问者 | 说明 |
|---|---|---|
| `/` | 所有人 | 控制台。未登录显示登录页，登录后使用 `#视图` 切换页面 |
| `/home` | 访客 | 公开主页，内容来自「系统设置 → 主页与 SEO」 |
| `/news`、`/news?category=分类` | 访客 | 公开文章列表 |
| `/news/{id}` | 访客 | 公开文章详情；不存在或未公开时页面状态码为 404 |

访客能读到一篇文章的条件同时满足：

1. 系统设置「公开文章区（主页 / 文章页）」（`publicArticles`）开启；
2. 文章已发布（`published:true`）；
3. 文章已公开（`public:true`）。

`public` 默认是 `false`，升级前的文章也按未公开处理。会员在控制台「公告/教程」中仍能读到全部已发布文章，不受 `public` 和 `publicArticles` 影响。`/news` 对所有人只展示公开文章。

## 文章（控制台）

Article 字段：`id,title,category,summary,body,published,public,pinned,sort,createdAt,updatedAt,attachments`。`title` 1–240 字节，`category` ≤80 字节，`summary` ≤300 字且不含控制字符，`body` ≤512000 字节。正文按 Markdown 保存，只在浏览器经 DOMPurify 净化后渲染。

- `GET /api/articles`：登录会员读取已发布文章。**未登录一律 401**，公开阅读改用下文的 `/api/public/*`。
- `GET /api/admin/articles`：管理员读取全部文章（含草稿）。
- `POST /api/admin/articles`、`PUT /api/admin/articles/{id}`：管理员保存。更新时保留服务端的 `sort`、`pinned`、`createdAt` 与附件，避免旧编辑器覆盖新排序。
- `POST /api/admin/articles/{id}/visibility`：管理员只修改 `{published?,public?}`，至少提供一项，返回 `{articles}`。列表上的「发布 / 取消发布 / 公开 / 取消公开」使用此接口，不会把旧标题或正文写回。
- `POST /api/admin/articles/{id}/move`、`/pin`、`DELETE /api/admin/articles/{id}`、附件上传与删除：见控制台既有行为，成功后公开内容立即刷新。
- `GET /api/articles/{article}/attachments/{id}`：管理员可下载任意附件；会员需要「文章附件下载」开启且文章已发布；访客还需要文章满足上文三个公开条件。下载始终是 `application/octet-stream` 附件。

## 公开接口

公开接口不需要登录、不读取会话，也不经过整站备份闸门；数据来自 15 秒内存快照，文章、设置或备份恢复成功后立即失效。每个来源（IPv6 按 /64 归并）每分钟最多 300 次，超出返回 429。

- `GET /api/public/home` → `{home:HomeConfig,site,newsEnabled,articles:Summary[]}`。`articles` 为最新的 `home.latestCount` 篇；`home.showLatest` 为 `false` 或文章区关闭时为空数组。
- `GET /api/public/articles` → `{enabled,site,contactEmail,articles:Summary[]}`。按「固定在前、手动排序、新文章在前」排列；文章区关闭时 `enabled:false`、`articles:[]`。
- `GET /api/public/articles/{id}` → `{article,attachmentsEnabled,site,contactEmail}`；不可见时 404。

`site` 为 `{siteName,homeTitle,newsTitle}`。Summary 字段：`id,title,category,summary,pinned,createdAt,updatedAt,attachmentCount`；未填写摘要时由正文去除 Markdown 标记后截取前 120 字。详情 `article` 另含 `body,attachments`。

## 主页与 SEO 设置

- `GET /api/admin/site` → `{home,seo,defaults:{home,seo}}`，仅管理员。
- `PUT /api/admin/site` 接受 `{home?,seo?}`（至少一项），校验后保存并返回同上结构。保存记录写入内容审计。

HomeConfig：

| 字段 | 规则 |
|---|---|
| `eyebrow` | ≤60 字 |
| `titleLine1` / `titleLine2` | 必填 / 可选，各 ≤40 字；第二行橙色显示 |
| `subtitle` | ≤300 字，可换行 |
| `points` | 最多 4 项，每项 ≤20 字 |
| `primaryCta` / `secondaryCta` | `{text≤12,href}`；文字为空时不显示按钮，有文字时必须有链接 |
| `features` | 最多 6 项 `{title≤20,desc≤60}` |
| `showLatest`、`latestCount` | 是否显示最新文章；数量只能为 3 或 6 |
| `notice`、`noticeHref` | 公告横幅 ≤120 字，链接可选 |
| `contactEmail` | 可选，必须是有效邮箱 |

SEOConfig：`siteName`（必填 ≤40）、`homeTitle`、`newsTitle`（必填 ≤80）、`description`（≤200）、`keywords`（≤200）、`ogImage`、`allowIndexing`。

所有链接只接受以 `/` 开头的站内路径（不能以 `//` 开头）或 `https://` 地址，拒绝 `javascript:`、`data:`、`http://` 等。读取时再次校验；备份恢复或手工改动后不合规的值会回落为默认值。

## 页面元信息、robots 与 sitemap

- 访问 `/home`、`/news`、`/news/{id}` 时，服务端在 `index.html` 中替换标题与描述，并加入 `og:*`、`twitter:card` 与 canonical。所有值经 HTML 转义。canonical、`og:url` 与 sitemap 需要配置 `PUBLIC_URL`。
- 文章页标题为「文章标题 · 站点名称」，描述优先使用文章摘要。
- `allowIndexing:false` 时页面带 `noindex,nofollow`，`/robots.txt` 为 `Disallow: /`，`/sitemap.xml` 返回 404。
- 开启收录时，`/robots.txt` 允许 `/home`、`/news`，禁止 `/api/`，并给出 sitemap 地址；`/sitemap.xml` 列出主页、文章列表和全部公开文章。
