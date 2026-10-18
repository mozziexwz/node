import { test } from "node:test";
import assert from "node:assert/strict";
import { chromium, expect } from "@playwright/test";

// Only static assets come from the isolated localhost test server. Every API
// request is intercepted; no account, attachment or external host is mutated.
async function articleBrowser(run, { admin = true } = {}) {
  const base = process.env.MSBOOST_TEST_URL || "http://127.0.0.1:8080";
  assert.match(base, /^http:\/\/(127\.0\.0\.1|localhost):\d+$/);
  const browser = await chromium.launch({
    headless: true,
    timeout: 10000,
    ...(process.platform === "win32" ? { channel: "chrome" } : {}),
  });
  const page = await browser.newPage({
    baseURL: base,
    viewport: { width: 1440, height: 1000 },
  });
  page.setDefaultTimeout(7000);
  page.setDefaultNavigationTimeout(10000);
  const errors = [],
    unexpected = [];
  page.on("pageerror", (error) => errors.push(error.message));
  let handler;
  await page.route("**/*", async (route) => {
    const url = new URL(route.request().url());
    if (url.origin !== base) {
      unexpected.push("external request");
      return route.abort();
    }
    if (!url.pathname.startsWith("/api/")) return route.continue();
    if (url.pathname === "/api/me")
      return route.fulfill({
        json: {
          user: {
            id: "fixture-admin",
            email: "10000001@qq.com",
            role: admin ? "admin" : "member",
            status: "active",
          },
          csrfToken: "isolated-fixture",
        },
      });
    if (url.pathname === "/api/settings")
      return route.fulfill({
        json: { settings: { publicArticles: true, attachments: true } },
      });
    if (url.pathname === "/api/tickets" && route.request().method() === "GET")
      return route.fulfill({ json: { tickets: [], unreadCount: 0 } });
    if (await handler(route, url.pathname)) return;
    unexpected.push(route.request().method() + " " + url.pathname);
    return route.fulfill({
      status: 500,
      json: { error: "Unexpected fixture request" },
    });
  });
  try {
    await run({
      page,
      base,
      setHandler(value) {
        handler = value;
      },
    });
    assert.deepEqual(errors, []);
    assert.deepEqual(unexpected, []);
  } finally {
    await browser.close();
  }
}

test(
  "published article categories use brackets and pinned state uses an icon",
  { timeout: 30000 },
  async () => {
    await articleBrowser(
      async ({ page, base, setHandler }) => {
        setHandler(async (route, path) => {
          if (path !== "/api/articles") return false;
          await route.fulfill({
            json: {
              articles: [
                {
                  id: "fixed",
                  title: "固定文章",
                  category: "公告",
                  published: true,
                  pinned: true,
                  body: "正文",
                  attachments: [],
                },
              ],
            },
          });
          return true;
        });
        await page.goto(base + "/#tutorials");
        const item = page.locator(".article-item", { hasText: "固定文章" });
        await expect(item.locator("small")).toContainText("[公告]");
        await expect(item.locator(".article-pin-inline")).toBeVisible();
        await expect(item.locator("small")).not.toContainText("置顶");
        await expect(page.locator(".article-content .badge")).toHaveText(
          "[公告]",
        );
      },
      { admin: false },
    );
  },
);

test(
  "article management: group-boundary controls, top/bottom payload and explicit pin",
  { timeout: 30000 },
  async () => {
    await articleBrowser(async ({ page, base, setHandler }) => {
      let items = [
        {
          id: "pinned",
          title: "置顶旧文章",
          category: "公告",
          published: true,
          pinned: true,
          body: "old",
          attachments: [],
        },
        {
          id: "new",
          title: "新资讯",
          category: "资讯",
          published: true,
          pinned: false,
          body: "new",
          attachments: [],
        },
        {
          id: "old",
          title: "旧教程",
          category: "教程",
          published: false,
          pinned: false,
          body: "old",
          attachments: [],
        },
      ];
      const moves = [],
        pins = [];
      setHandler(async (route, path) => {
        if (path === "/api/admin/articles") {
          await route.fulfill({ json: { articles: items } });
          return true;
        }
        const match = path.match(
          /^\/api\/admin\/articles\/(old|new|pinned)\/(move|pin)$/,
        );
        if (!match) return false;
        const data = route.request().postDataJSON();
        assert.equal(route.request().method(), "POST");
        assert.equal(
          route.request().headers()["x-csrf-token"],
          "isolated-fixture",
        );
        if (match[2] === "move") {
          moves.push({ id: match[1], ...data });
          items = [items[0], items[2], items[1]];
        } else {
          pins.push({ id: match[1], ...data });
          items = items.map((item) =>
            item.id === match[1] ? { ...item, pinned: data.pinned } : item,
          );
        }
        await route.fulfill({ json: { articles: items } });
        return true;
      });
      await page.goto(base + "/#tutorials");
      await expect(
        page.getByRole("button", { name: "移到最顶 新资讯", exact: true }),
      ).toBeDisabled();
      await expect(
        page.getByRole("button", { name: "上移 新资讯", exact: true }),
      ).toBeDisabled();
      await expect(
        page.getByRole("button", { name: "移到最底 置顶旧文章", exact: true }),
      ).toBeDisabled();
      await expect(
        page.getByRole("button", { name: "移到最底 旧教程", exact: true }),
      ).toBeDisabled();
      await page
        .getByRole("button", { name: "移到最顶 旧教程", exact: true })
        .click();
      await expect(
        page.getByRole("button", { name: "移到最顶 旧教程", exact: true }),
      ).toBeDisabled();
      assert.deepEqual(moves, [{ id: "old", direction: "top" }]);
      await expect(page.locator("tbody tr td:first-child strong")).toHaveText([
        "置顶旧文章",
        "旧教程",
        "新资讯",
      ]);
      await page
        .getByRole("button", { name: "移到最底 旧教程", exact: true })
        .click();
      await expect(
        page.getByRole("button", { name: "移到最底 旧教程", exact: true }),
      ).toBeDisabled();
      assert.deepEqual(moves, [
        { id: "old", direction: "top" },
        { id: "old", direction: "bottom" },
      ]);
      await expect(page.locator("tbody tr td:first-child strong")).toHaveText([
        "置顶旧文章",
        "新资讯",
        "旧教程",
      ]);
      const row = page.getByRole("row").filter({ hasText: "新资讯" });
      await expect(row.getByRole("cell", { name: "[资讯]" })).toBeVisible();
      await row
        .getByRole("button", { name: "固定到顶部", exact: true })
        .click();
      await expect(
        row.getByRole("button", { name: "取消固定", exact: true }),
      ).toBeVisible();
      assert.deepEqual(pins, [{ id: "new", pinned: true }]);
      await row.getByRole("button", { name: "取消固定", exact: true }).click();
      await expect(
        row.getByRole("button", { name: "固定到顶部", exact: true }),
      ).toBeVisible();
      assert.deepEqual(pins, [
        { id: "new", pinned: true },
        { id: "new", pinned: false },
      ]);
      await page.screenshot({
        path: "../../.runtime/article-management-controls.png",
        fullPage: true,
      });
    });
  },
);

test(
  "new article: attachment auto-saves private draft, failed upload retries same draft, explicit publish only",
  { timeout: 30000 },
  async () => {
    await articleBrowser(async ({ page, base, setHandler }) => {
      let article = null,
        uploads = 0,
        deletes = 0;
      const creates = [],
        saves = [];
      setHandler(async (route, path) => {
        const method = route.request().method();
        if (path === "/api/admin/articles" && method === "GET") {
          await route.fulfill({ json: { articles: article ? [article] : [] } });
          return true;
        }
        if (path === "/api/admin/articles" && method === "POST") {
          const data = route.request().postDataJSON();
          creates.push(data);
          assert.equal(
            data.published,
            false,
            "upload must never publish even when the checkbox had been selected",
          );
          article = { ...data, id: "draft", attachments: [] };
          await route.fulfill({ json: article });
          return true;
        }
        if (
          path === "/api/admin/articles/draft/attachments" &&
          method === "POST"
        ) {
          uploads++;
          assert.equal(article.published, false);
          if (uploads === 1)
            await route.fulfill({
              status: 400,
              json: { error: "模拟附件上传失败" },
            });
          else {
            const file = { id: "file", name: "draft.txt", size: 7 };
            article.attachments = [file];
            await route.fulfill({ status: 201, json: file });
          }
          return true;
        }
        if (path === "/api/admin/articles/draft" && method === "PUT") {
          const data = route.request().postDataJSON();
          saves.push(data);
          article = { ...data, attachments: article.attachments };
          await route.fulfill({ json: article });
          return true;
        }
        if (
          path === "/api/admin/articles/draft/attachments/file" &&
          method === "DELETE"
        ) {
          deletes++;
          if (deletes === 1)
            await route.fulfill({
              status: 409,
              json: { error: "模拟附件删除失败" },
            });
          else {
            article.attachments = [];
            await route.fulfill({ json: { ok: true } });
          }
          return true;
        }
        return false;
      });
      await page.goto(base + "/#tutorials");
      await page.getByRole("button", { name: "新建文章", exact: true }).click();
      let dialog = page.getByRole("dialog");
      await expect(
        dialog.getByRole("heading", { name: "附件管理" }),
      ).toBeVisible();
      await dialog
        .getByRole("combobox", { name: /分类/ })
        .selectOption({ label: "资讯" });
      await dialog.getByLabel("发布给用户", { exact: true }).check();
      const file = {
        name: "draft.txt",
        mimeType: "text/plain",
        buffer: Buffer.from("fixture"),
      };
      await dialog.getByLabel("上传文章附件").setInputFiles(file);
      await expect(
        dialog.getByText("模拟附件上传失败", { exact: true }),
      ).toBeVisible();
      assert.equal(creates.length, 1);
      assert.equal(creates[0].title, "未命名草稿");
      assert.equal(creates[0].category, "资讯");
      await expect(
        dialog.getByRole("textbox", { name: "标题", exact: true }),
      ).toHaveValue("");
      await expect(
        dialog.getByLabel("发布给用户", { exact: true }),
      ).not.toBeChecked();
      await expect(
        dialog.getByText(/当前为草稿，文章及附件仅管理员可访问/),
      ).toBeVisible();
      await dialog.getByLabel("上传文章附件").setInputFiles(file);
      await expect(
        dialog.getByText("draft.txt", { exact: true }),
      ).toBeVisible();
      assert.equal(uploads, 2);
      assert.equal(creates.length, 1);
      assert.equal(saves.length, 0);
      await dialog
        .getByRole("textbox", { name: "标题", exact: true })
        .fill("带附件的资讯");
      await dialog.getByLabel("文章 Markdown 正文").fill("尚未公开正文");
      await dialog
        .getByRole("button", { name: "保存草稿", exact: true })
        .click();
      await expect(
        dialog.getByText("操作已完成", { exact: true }),
      ).toBeVisible();
      assert.equal(saves.length, 1);
      assert.equal(saves[0].published, false);
      await dialog.getByLabel("发布给用户", { exact: true }).check();
      await dialog
        .getByRole("button", { name: "发布 / 更新", exact: true })
        .click();
      await expect(
        dialog.getByRole("button", { name: "发布 / 更新", exact: true }),
      ).toBeEnabled();
      assert.equal(saves.length, 2);
      assert.equal(saves[1].published, true);
      page.on("dialog", (prompt) => prompt.accept());
      await dialog.getByRole("button", { name: "删除", exact: true }).click();
      await expect(
        dialog.getByText("模拟附件删除失败", { exact: true }),
      ).toBeVisible();
      await expect(
        dialog.getByText("draft.txt", { exact: true }),
      ).toBeVisible();
      await dialog.getByRole("button", { name: "删除", exact: true }).click();
      await expect(dialog.getByText("draft.txt", { exact: true })).toHaveCount(
        0,
      );
      assert.equal(deletes, 2);
      await page.screenshot({
        path: "../../.runtime/article-draft-attachments.png",
        fullPage: true,
      });
    });
  },
);

test(
  "article management: guest visibility toggles through the visibility endpoint",
  { timeout: 30000 },
  async () => {
    await articleBrowser(async ({ page, base, setHandler }) => {
      let article = {
        id: "guide",
        title: "部署教程",
        category: "教程",
        published: true,
        public: false,
        pinned: false,
        body: "正文",
        attachments: [],
      };
      const changes = [];
      setHandler(async (route, path) => {
        const method = route.request().method();
        if (path === "/api/admin/articles" && method === "GET") {
          await route.fulfill({ json: { articles: [article] } });
          return true;
        }
        if (
          path === "/api/admin/articles/guide/visibility" &&
          method === "POST"
        ) {
          const change = route.request().postDataJSON();
          changes.push(change);
          assert.equal(
            route.request().headers()["x-csrf-token"],
            "isolated-fixture",
          );
          article = { ...article, ...change };
          await route.fulfill({ json: { articles: [article] } });
          return true;
        }
        return false;
      });
      await page.goto(base + "/#tutorials");
      const row = page.locator("tbody tr", { hasText: "部署教程" });
      await expect(row.getByText("仅会员", { exact: true })).toBeVisible();
      await expect(row.getByRole("link", { name: /查看公开页/ })).toHaveCount(
        0,
      );
      await row
        .getByRole("button", { name: "公开到文章页", exact: true })
        .click();
      await expect(row.getByText("公开", { exact: true })).toBeVisible();
      await expect(
        row.getByRole("link", { name: /查看公开页/ }),
      ).toHaveAttribute("href", "/news/guide");
      await row.getByRole("button", { name: "取消发布", exact: true }).click();
      await expect(
        row.getByText("公开（未发布）", { exact: true }),
      ).toBeVisible();
      await expect(row.getByRole("link", { name: /查看公开页/ })).toHaveCount(
        0,
      );
      // Only the flags travel; a stale title or body can never be written back.
      assert.deepEqual(changes, [{ public: true }, { published: false }]);
      await row.getByRole("button", { name: "编辑", exact: true }).click();
      const dialog = page.getByRole("dialog");
      await expect(
        dialog.getByLabel("同时公开到主页和文章页（访客无需登录）"),
      ).toBeDisabled();
    });
  },
);

test(
  "article editor: toolbar and shortcuts format the line or selection, Ctrl+S saves, preview follows the display rules",
  { timeout: 40000 },
  async () => {
    await articleBrowser(async ({ page, base, setHandler }) => {
      const saves = [];
      setHandler(async (route, path) => {
        const method = route.request().method();
        if (path === "/api/admin/articles" && method === "GET") {
          await route.fulfill({ json: { articles: [] } });
          return true;
        }
        if (path === "/api/admin/articles" && method === "POST") {
          const data = route.request().postDataJSON();
          saves.push(data);
          await route.fulfill({
            json: { ...data, id: "fresh", attachments: [] },
          });
          return true;
        }
        return false;
      });
      await page.goto(base + "/#tutorials");
      await page.getByRole("button", { name: "新建文章", exact: true }).click();
      const dialog = page.getByRole("dialog");
      await dialog
        .getByRole("textbox", { name: "标题", exact: true })
        .fill("编辑器测试");
      const body = dialog.getByRole("textbox", { name: "文章 Markdown 正文" });
      await expect(body).toBeVisible();
      await expect(dialog.locator(".cm-lineNumbers")).toBeVisible();
      const tool = (name) =>
        dialog.getByRole("button", { name, exact: true }).click();
      const keys = page.keyboard;
      await expect(
        dialog.getByRole("button", { name: "撤销", exact: true }),
      ).toBeDisabled();

      await body.click();
      await keys.type("标题");
      // ## then ### then plain text, without adding blank lines.
      await tool("标题");
      await tool("标题");
      await tool("标题");
      await tool("标题");
      await keys.press("End");
      await keys.press("Enter");
      await keys.type("注意：");
      await keys.press("Shift+Home");
      await keys.press("Control+b");
      await keys.press("End");
      await keys.type("内容");
      await keys.press("Enter");
      await keys.type("网盘");
      await keys.press("Shift+Home");
      await tool("链接");
      // The address is selected, ready to be pasted over.
      await keys.type("https://file.msboost.de/#/?code=1");
      await keys.press("End");
      await keys.press("Enter");
      await keys.type("重点");
      await keys.press("Shift+Home");
      await tool("高亮");
      await keys.press("End");
      await keys.press("Enter");
      await keys.press("Enter");
      await keys.press("Enter");
      await keys.type("  两格缩进");
      await tool("表格");
      await keys.type("名称");
      await keys.press("Control+End");
      await keys.press("Enter");
      await keys.press("Enter");
      await keys.type("第一项");
      await keys.press("Enter");
      await keys.type("第二项");
      await keys.press("Shift+ArrowUp");
      await tool("无序列表");
      await tool("有序列表");
      await tool("撤销");
      await tool("重做");
      await expect(
        dialog.getByRole("button", { name: "重做", exact: true }),
      ).toBeDisabled();
      await keys.press("Control+s");
      await expect(
        dialog.getByText("操作已完成", { exact: true }),
      ).toBeVisible();
      assert.equal(saves.length, 1);
      assert.equal(
        saves[0].body,
        [
          "## 标题",
          "**注意：**内容",
          "[网盘](https://file.msboost.de/#/?code=1)",
          "==重点==",
          "",
          "",
          "  两格缩进",
          "",
          "| 名称 | 列2 |",
          "| --- | --- |",
          "| 内容 | 内容 |",
          "",
          "1. 第一项",
          "2. 第二项",
        ].join("\n"),
      );

      await dialog.getByRole("tab", { name: "对照", exact: true }).click();
      const preview = dialog.locator(".markdown-preview .markdown");
      await expect(preview.locator("h2")).toHaveText("标题");
      await expect(preview.locator("strong")).toHaveText("注意：");
      const link = preview.getByRole("link", { name: "网盘" });
      await expect(link).toHaveAttribute("target", "_blank");
      await expect(link).toHaveAttribute("rel", "noopener noreferrer");
      await expect(preview.locator("mark")).toHaveText("重点");
      await expect(preview.locator(".md-gap")).toHaveCount(1);
      await expect(preview.getByText("\u00a0\u00a0两格缩进")).toBeVisible();
      await expect(preview.locator(".md-table th").first()).toHaveText("名称");
      await expect(preview.locator("ol > li")).toHaveCount(2);
      await expect(dialog.locator(".markdown-status")).toContainText(
        "对照模式两侧同步滚动",
      );

      // Compare mode: scrolling the source moves the preview along.
      await body.fill(
        Array.from({ length: 120 }, (_, i) => `第 ${i + 1} 行`).join("\n"),
      );
      const scroller = dialog.locator(".cm-scroller");
      await scroller.evaluate((el) => {
        el.scrollTop = el.scrollHeight;
      });
      await expect
        .poll(() =>
          dialog.locator(".markdown-preview").evaluate((el) => el.scrollTop),
        )
        .toBeGreaterThan(100);

      await dialog.getByRole("tab", { name: "预览", exact: true }).click();
      await expect(body).toBeHidden();
      await expect(
        dialog.getByRole("button", { name: "加粗", exact: true }),
      ).toBeDisabled();
      await dialog.getByRole("tab", { name: "内容", exact: true }).click();
      await expect(body).toBeVisible();
      await expect(dialog.locator(".markdown-preview")).toHaveCount(0);
      await page.screenshot({
        path: "../../.runtime/article-editor.png",
        fullPage: true,
      });
    });
  },
);

test(
  "article editor: a plain text box keeps the article editable when the editor cannot load",
  { timeout: 30000 },
  async () => {
    await articleBrowser(async ({ page, base, setHandler }) => {
      const saves = [];
      setHandler(async (route, path) => {
        const method = route.request().method();
        if (path === "/api/admin/articles" && method === "GET") {
          await route.fulfill({ json: { articles: [] } });
          return true;
        }
        if (path === "/api/admin/articles" && method === "POST") {
          const data = route.request().postDataJSON();
          saves.push(data);
          await route.fulfill({
            json: { ...data, id: "fresh", attachments: [] },
          });
          return true;
        }
        return false;
      });
      await page.route("**/assets/markdown-editor-*.js", (route) =>
        route.abort(),
      );
      await page.goto(base + "/#tutorials");
      await page.getByRole("button", { name: "新建文章", exact: true }).click();
      const dialog = page.getByRole("dialog");
      await dialog
        .getByRole("textbox", { name: "标题", exact: true })
        .fill("备用编辑框");
      const body = dialog.locator("textarea.markdown-plain");
      await expect(body).toHaveAccessibleName("文章 Markdown 正文");
      await expect(
        dialog.getByRole("button", { name: "加粗", exact: true }),
      ).toBeDisabled();
      await body.fill("第一行\n第二行");
      await dialog
        .getByRole("button", { name: "保存草稿", exact: true })
        .click();
      await expect(
        dialog.getByText("操作已完成", { exact: true }),
      ).toBeVisible();
      assert.equal(saves[0].body, "第一行\n第二行");
    });
  },
);
