import { test } from "node:test";
import assert from "node:assert/strict";
import { chromium, expect } from "@playwright/test";

// Static assets come from the isolated localhost test server; every API call
// is answered by fixtures, so no account or article is created or changed.
const home = {
  eyebrow: "SELF-HOSTED · MAPLESTORY",
  titleLine1: "测试主页标题",
  titleLine2: "橙色第二行",
  subtitle: "第一行副标题\n第二行副标题",
  points: ["要点一", "要点二"],
  primaryCta: { text: "进入控制台", href: "/" },
  secondaryCta: { text: "阅读教程", href: "/news" },
  features: [{ title: "功能一", desc: "说明一" }],
  showLatest: true,
  latestCount: 3,
  notice: "测试公告横幅",
  noticeHref: "/news",
  contactEmail: "admin@example.test",
};
const site = { siteName: "MSBOOST", homeTitle: "测试主页 · MSBOOST", newsTitle: "公告与教程 · MSBOOST" };
const articles = [
  { id: "pinned-notice", title: "固定公告", category: "公告", summary: "公告摘要", pinned: true, createdAt: 1760000000000, updatedAt: 1760000000000, attachmentCount: 0 },
  { id: "guide", title: "部署教程", category: "教程", summary: "教程摘要", pinned: false, createdAt: 1759000000000, updatedAt: 1759500000000, attachmentCount: 1 },
];

async function publicBrowser(run, { enabled = true, viewport = { width: 1440, height: 1000 } } = {}) {
  const base = process.env.MSBOOST_TEST_URL || "http://127.0.0.1:8080";
  assert.match(base, /^http:\/\/(127\.0\.0\.1|localhost):\d+$/);
  const browser = await chromium.launch({
    headless: true,
    timeout: 10000,
    ...(process.platform === "win32" ? { channel: "chrome" } : {}),
  });
  const page = await browser.newPage({ baseURL: base, viewport });
  page.setDefaultTimeout(7000);
  const errors = [],
    unexpected = [];
  page.on("pageerror", (error) => errors.push(error.message));
  await page.route("**/*", async (route) => {
    const url = new URL(route.request().url());
    if (url.origin !== base) {
      unexpected.push("external request " + url.origin);
      return route.abort();
    }
    if (!url.pathname.startsWith("/api/")) return route.continue();
    if (url.pathname === "/api/public/home")
      return route.fulfill({ json: { home, site, newsEnabled: enabled, articles: enabled ? articles : [] } });
    if (url.pathname === "/api/public/articles")
      return route.fulfill({ json: { enabled, site, contactEmail: home.contactEmail, articles: enabled ? articles : [] } });
    if (url.pathname === "/api/public/articles/guide")
      return route.fulfill({
        json: {
          site,
          attachmentsEnabled: true,
          article: {
            ...articles[1],
            body: "## 准备\n\n正文段落<script>window.injected=1</script><img src=x onerror=\"window.injected=2\">",
            attachments: [{ id: "file", name: "guide.pdf", size: 2048 }],
          },
        },
      });
    if (url.pathname.startsWith("/api/public/articles/"))
      return route.fulfill({ status: 404, json: { error: "文章不存在或未公开" } });
    unexpected.push(route.request().method() + " " + url.pathname);
    return route.fulfill({ status: 500, json: { error: "Unexpected fixture request" } });
  });
  try {
    await run({ page, base });
    assert.deepEqual(errors, []);
    assert.deepEqual(unexpected, []);
  } finally {
    await browser.close();
  }
}

test("public home renders admin content, switcher and latest articles", { timeout: 30000 }, async () => {
  await publicBrowser(async ({ page }) => {
    await page.goto("/home");
    await expect(page.getByRole("heading", { level: 1 })).toContainText("测试主页标题");
    await expect(page.locator(".v4-hero-line.orange")).toHaveText("橙色第二行");
    await expect(page.locator(".public-notice")).toContainText("测试公告横幅");
    await expect(page.getByRole("link", { name: "进入控制台" }).first()).toHaveAttribute("href", "/");
    await expect(page.locator(".news-card")).toHaveCount(2);
    await expect(page).toHaveTitle("测试主页 · MSBOOST");
    const switcher = page.getByRole("navigation", { name: "站点切换" });
    await expect(switcher.getByRole("link", { name: "主页" })).toHaveAttribute("aria-current", "page");
    await expect(switcher.getByRole("link", { name: "控制台" })).toHaveAttribute("href", "/");
    await expect(page.locator(".public-footer")).toContainText("admin@example.test");
  });
});

test("news list filters by category and opens a sanitized article without reloading", { timeout: 30000 }, async () => {
  await publicBrowser(async ({ page }) => {
    await page.goto("/home");
    await page.evaluate(() => (window.sameDocument = true));
    await page.getByRole("navigation", { name: "站点切换" }).getByRole("link", { name: "文章" }).click();
    await expect(page).toHaveURL(/\/news$/);
    await expect(page.locator(".news-row")).toHaveCount(2);
    await expect(page.locator(".news-row").first()).toContainText("固定公告");
    await page.getByRole("link", { name: "教程", exact: true }).click();
    await expect(page).toHaveURL(/\/news\?category=/);
    await expect(page.locator(".news-row")).toHaveCount(1);
    await expect(page.getByRole("link", { name: "教程", exact: true })).toHaveAttribute("aria-current", "page");
    await page.locator(".news-row").first().click();
    await expect(page).toHaveURL(/\/news\/guide$/);
    await expect(page.getByRole("heading", { level: 1 })).toHaveText("部署教程");
    await expect(page.locator(".news-body")).toContainText("正文段落");
    await expect(page.getByRole("button", { name: "下载" })).toBeVisible();
    await expect(page).toHaveTitle("部署教程 · MSBOOST");
    assert.equal(await page.evaluate(() => window.sameDocument), true, "public navigation reloaded the page");
    assert.equal(await page.evaluate(() => window.injected), undefined, "article markup executed script");
    await page.goBack();
    await expect(page).toHaveURL(/\/news\?category=/);
    await expect(page.locator(".news-row")).toHaveCount(1);
  });
});

test("missing articles and a closed article area show clear states", { timeout: 30000 }, async () => {
  await publicBrowser(async ({ page }) => {
    await page.goto("/news/missing-article");
    await expect(page.getByText("文章不存在或未公开")).toBeVisible();
    await expect(page.getByRole("link", { name: "返回文章列表" })).toHaveAttribute("href", "/news");
  });
  await publicBrowser(
    async ({ page }) => {
      await page.goto("/news");
      await expect(page.getByText("文章区暂未开放")).toBeVisible();
      await page.goto("/home");
      await expect(page.getByRole("heading", { level: 1 })).toContainText("测试主页标题");
      await expect(page.locator(".public-latest")).toHaveCount(0);
    },
    { enabled: false },
  );
});

test("public pages have no horizontal overflow from 320px to desktop", { timeout: 60000 }, async () => {
  for (const width of [320, 390, 768, 1440]) {
    await publicBrowser(
      async ({ page }) => {
        for (const path of ["/home", "/news", "/news/guide"]) {
          await page.goto(path);
          await expect(page.locator(".public-main")).not.toBeEmpty();
          await expect(page.locator(".empty[role=status]")).toHaveCount(0);
          assert.equal(
            await page.evaluate(() => document.documentElement.scrollWidth > innerWidth),
            false,
            `overflow at ${width}px on ${path}`,
          );
        }
      },
      { viewport: { width, height: 900 } },
    );
  }
});
