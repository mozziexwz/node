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

const published = {
  version: "v1.0.2",
  releasedAt: "2026-10-17",
  downloadUrl: "https://file.msboost.de/#/?code=AbC123",
  backupUrl: "https://file.msboost.de/#/?code=Backup",
  sha256: "ab".repeat(32),
  notes: ["- 修复启动问题", "- 补充日志"].join("\n"),
  updatedAt: 1760000000000,
};

// realHome serves the isolated server's own default home page, with the
// fixture articles and, when given, a published launcher.
async function publicBrowser(run, { enabled = true, viewport = { width: 1440, height: 1000 }, realHome = false, launcher } = {}) {
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
    if (url.pathname === "/api/public/home" && realHome) {
      const response = await route.fetch();
      const json = await response.json();
      return route.fulfill({ response, json: { ...json, articles, ...(launcher ? { launcher } : {}) } });
    }
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

test("home explains MSBOOST and relay servers, cards scroll to them and articles come last", { timeout: 30000 }, async () => {
  await publicBrowser(
    async ({ page }) => {
      await page.goto("/home");
      await expect(page.getByRole("heading", { level: 1 }).locator(":scope > span")).toHaveText(["一键部署你的MapleStory", "独立IP游戏节点"]);
      await expect(page.locator(".v4-hero-copy")).toContainText("部署节点、配置中转、一键加速。");
      const cards = page.locator(".public-tools .tool-card");
      await expect(cards.locator("h3")).toHaveText(["一键部署 MSBOOST", "一键配置中转服务器", "MSBOOST 启动器"]);
      await expect(cards.locator("p")).toHaveText(["部署你的VPS独立IP游戏节点", "与朋友共享你的VPS精品线路，但不共享你的IP", "一键加速 MapleStory"]);
      await expect(cards.locator(".card-jump")).toHaveText(["了解 MSBOOST", "了解中转服务器", "了解 MSBOOST 启动器"]);
      const order = await page.evaluate(() =>
        [...["msboost", "relay", "launcher"].map((id) => document.getElementById(id)), document.querySelector(".public-latest")].map((section) => (section ? section.getBoundingClientRect().top + scrollY : null)),
      );
      assert.ok(order.every((top, i) => top !== null && (i === 0 || top > order[i - 1])), `sections in order, latest articles last: ${order}`);
      for (const [card, id] of [[1, "relay"], [0, "msboost"], [2, "launcher"]]) {
        await cards.nth(card).click();
        await expect(page).toHaveURL(new RegExp(`/home#${id}$`));
        await expect
          .poll(() => page.evaluate((id) => { const top = document.getElementById(id).getBoundingClientRect().top; return top >= 0 && top < innerHeight / 2; }, id))
          .toBe(true);
      }
      const msboost = page.locator("#msboost");
      await expect(msboost.getByRole("heading", { level: 2 })).toHaveText("什么是 MSBOOST？");
      await expect(msboost.locator(".route-node.exit")).toContainText("游戏出口：你自己的VPS · IP");
      await expect(msboost.getByRole("link", { name: "去部署 MSBOOST" })).toHaveAttribute("href", "/#deploy");
      const relay = page.locator("#relay");
      await expect(relay.locator(".route-node.relay")).toContainText("自己或朋友分享的精品线路中转");
      await expect(relay.locator(".myth-card.is-wrong")).toContainText("就是在用朋友的 IP 玩游戏");
      await expect(relay.locator(".myth-card.is-right b")).toHaveText("分享的是精品线路，不是 IP。");
      await expect(relay.getByRole("link", { name: "去配置中转服务器" })).toHaveAttribute("href", "/#relay");
      // Not yet published: the section stays and says so.
      await expect(page.locator("#launcher")).toContainText("启动器下载地址尚未发布");
      assert.doesNotMatch(await page.locator("body").innerText(), /冒险岛|转发|github/i);
    },
    { realHome: true },
  );
});

test("a shared /home#relay link opens at its section and a published launcher offers its download", { timeout: 30000 }, async () => {
  await publicBrowser(
    async ({ page }) => {
      await page.goto("/home#relay");
      // The section opens right under the header and the table of contents.
      await expect
        .poll(() =>
          page.evaluate(() => {
            const section = document.getElementById("relay"),
              header = document.querySelector(".public-header"),
              bar = document.querySelector(".hp-secnav");
            if (!section || !header || !bar) return 9999;
            return Math.abs(Math.round(section.getBoundingClientRect().top - header.getBoundingClientRect().bottom - bar.offsetHeight));
          }),
        )
        .toBeLessThan(3);
      const section = page.locator("#launcher");
      const download = section.getByRole("link", { name: "下载 v1.0.2" });
      await expect(download).toHaveAttribute("href", published.downloadUrl);
      await expect(download).toHaveAttribute("target", "_blank");
      await expect(download).toHaveAttribute("rel", "noopener noreferrer");
      await expect(section.getByRole("link", { name: "备用下载" })).toHaveAttribute("href", published.backupUrl);
      await expect(section).toContainText("将在新窗口打开 file.msboost.de");
      await expect(section).toContainText("更新于 2026-10-17");
      await expect(section.locator(".hash-row code")).toHaveText(published.sha256);
      await section.locator(".notes-details summary").click();
      await expect(section.locator(".notes-details li")).toHaveText(["修复启动问题", "补充日志"]);
      await page.setViewportSize({ width: 375, height: 812 });
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false, "no horizontal overflow on a phone");
    },
    { realHome: true, launcher: published },
  );
});
