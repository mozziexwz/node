import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import { fileURLToPath } from "node:url";
import { chromium, expect } from "@playwright/test";
import { build } from "esbuild";

// The usage guides are static pages published with the site, offered from the
// download cards; 公告/教程 and the public news page open with an announcement
// that administrators edit in place. Every request is answered by fixtures.
const guides = fileURLToPath(new URL("../public/guides/", import.meta.url));
const base = "http://127.0.0.1:19990";

test("the usage guides run only the shared script file", () => {
  assert.match(fs.readFileSync(guides + "guide.js", "utf8"), /\.toc a/);
  for (const name of ["launcher.html", "local.html"]) {
    const html = fs.readFileSync(guides + name, "utf8");
    // The site's Content-Security-Policy refuses inline scripts.
    assert.deepEqual(
      [...html.matchAll(/<script\b([^>]*)>/g)].map((m) => m[1]),
      [' src="guide.js"'],
      name,
    );
    assert.doesNotMatch(html, /\son[a-z]+\s*=\s*["']/i, name);
    assert.doesNotMatch(html, /javascript:/i, name);
    assert.doesNotMatch(html, /冒险岛|转发/, name);
    assert.doesNotMatch(html, /github\.com/i, name);
  }
});

const published = {
  version: "v1.1.0",
  releasedAt: "2026-10-24",
  downloadUrl: "https://file.msboost.de/#/?code=Launcher",
  backupUrl: "",
  sha256: "",
  notes: "",
  updatedAt: 1761300000000,
};
const notice = {
  enabled: true,
  title: "MSBOOST 本地版 v1.0.0 已发布",
  body: "不想在网站填写 SSH 的话，可以用**本地版**在自己的电脑上部署。",
  linkText: "去下载本地版",
  linkHref: "/#local",
  public: true,
  updatedAt: 1761300000000,
};
const member = {
  id: "member",
  role: "user",
  status: "active",
  email: "10000001@qq.com",
  emailVerifiedAt: 1,
};
const admin = { ...member, id: "admin", role: "admin" };

let script;
async function site(run, responses, user = member) {
  script ??= (
    await build({
      entryPoints: [fileURLToPath(new URL("../src/main.tsx", import.meta.url))],
      bundle: true,
      write: false,
      outdir: "in-memory",
      format: "iife",
      platform: "browser",
      loader: { ".css": "empty" },
    })
  ).outputFiles.find((file) => file.path.endsWith(".js")).text;
  const browser = await chromium.launch({
    headless: true,
    ...(process.platform === "win32" ? { channel: "chrome" } : {}),
  });
  const page = await browser.newPage({
    viewport: { width: 1440, height: 1000 },
  });
  page.setDefaultTimeout(7000);
  const errors = [],
    saves = [];
  page.on("pageerror", (error) => errors.push(error.message));
  await page.route("**/*", (route) => {
    const request = route.request(),
      url = new URL(request.url());
    if (url.origin !== base) {
      errors.push("external request " + url.origin);
      return route.abort();
    }
    if (["/", "/home", "/news"].includes(url.pathname))
      return route.fulfill({
        contentType: "text/html",
        body: `<div id="root"></div><script>${script}</script>`,
      });
    if (url.pathname === "/api/admin/settings" && request.method() === "PUT") {
      saves.push(request.postDataJSON());
      return route.fulfill({ json: { ok: true } });
    }
    const fixed = {
      "/api/me": { user, csrfToken: "fixture" },
      "/api/settings": { deploy: true, relay: true, dd: true },
      "/api/public/launcher": {
        launcher: published,
        localApp: {
          ...published,
          version: "v1.0.0",
          downloadUrl: "https://file.msboost.de/#/?code=Local",
        },
      },
      "/api/tasks": { tasks: [] },
      "/api/tickets": { tickets: [], unreadCount: 0 },
      ...responses,
    };
    return route.fulfill({ json: fixed[url.pathname] || {} });
  });
  try {
    await run({ page, saves });
    assert.deepEqual(errors, []);
  } finally {
    await browser.close();
  }
}

test(
  "download cards open the usage guides in a new window",
  { timeout: 60000 },
  async () => {
    await site(async ({ page }) => {
      for (const [view, guide] of [
        ["launcher", "/guides/launcher.html"],
        ["local", "/guides/local.html"],
      ]) {
        await page.goto(base + "/#" + view);
        const link = page.getByRole("link", { name: "使用指南", exact: true });
        await expect(link).toHaveAttribute("href", guide);
        await expect(link).toHaveAttribute("target", "_blank");
      }
      await page.goto(base + "/#home");
      await expect(
        page.getByRole("link", { name: /启动器使用指南/ }),
      ).toHaveAttribute("href", "/guides/launcher.html");
    });
  },
);

test(
  "members read the page notice; nothing shows while it is off",
  { timeout: 60000 },
  async () => {
    const articles = [
      {
        id: "a1",
        title: "第一次使用 MSBOOST",
        category: "教程",
        body: "正文",
        published: true,
        updatedAt: 1761300000000,
        attachments: [],
      },
    ];
    await site(
      async ({ page }) => {
        await page.goto(base + "/#tutorials");
        const card = page.locator(".na-card");
        await expect(
          card.getByRole("heading", { name: notice.title }),
        ).toBeVisible();
        await expect(card.locator("strong")).toHaveText("本地版");
        await expect(
          card.getByRole("link", { name: "去下载本地版" }),
        ).toHaveAttribute("href", "/#local");
        await expect(
          page.getByRole("button", { name: "编辑公告" }),
        ).toHaveCount(0);
      },
      { "/api/articles": { articles, notice } },
    );
    await site(
      async ({ page }) => {
        await page.goto(base + "/#tutorials");
        await expect(page.locator(".article-content h2")).toHaveText(
          "第一次使用 MSBOOST",
        );
        await expect(page.locator(".na-card, .na-empty")).toHaveCount(0);
      },
      { "/api/articles": { articles, notice: null } },
    );
  },
);

test(
  "administrators edit the notice on 公告/教程",
  { timeout: 60000 },
  async () => {
    await site(
      async ({ page, saves }) => {
        await page.goto(base + "/#tutorials");
        const card = page.locator(".na-card");
        await expect(card.getByText("已隐藏", { exact: true })).toBeVisible();
        await card.getByRole("button", { name: "编辑公告" }).click();
        const dialog = page.getByRole("dialog");
        await expect(
          dialog.getByRole("heading", { name: "编辑页面公告" }),
        ).toBeVisible();
        await expect(dialog.getByLabel(/^标题/)).toHaveValue(notice.title);
        await dialog.getByRole("checkbox", { name: "显示公告" }).check();
        await dialog
          .getByRole("checkbox", { name: /同时显示在公开的文章页/ })
          .check();
        await dialog.getByRole("button", { name: "保存公告" }).click();
        await expect(page.getByRole("dialog")).toHaveCount(0);
        assert.deepEqual(
          saves,
          [
            {
              newsNotice: {
                ...notice,
                enabled: true,
                public: true,
                updatedAt: undefined,
              },
            },
          ].map((s) => JSON.parse(JSON.stringify(s))),
        );
      },
      {
        "/api/admin/articles": {
          articles: [],
          notice: { ...notice, enabled: false, public: false },
        },
      },
      admin,
    );
    await site(
      async ({ page }) => {
        await page.goto(base + "/#tutorials");
        await expect(page.getByText("还没有页面公告")).toBeVisible();
        await page.getByRole("button", { name: "写公告" }).click();
        await expect(
          page.getByRole("dialog").getByRole("checkbox", { name: "显示公告" }),
        ).toBeChecked();
      },
      {
        "/api/admin/articles": {
          articles: [],
          notice: {
            enabled: false,
            title: "",
            body: "",
            linkText: "",
            linkHref: "",
            public: false,
            updatedAt: 0,
          },
        },
      },
      admin,
    );
  },
);

test(
  "the public news page opens with a public notice",
  { timeout: 60000 },
  async () => {
    const shell = { "/api/public/home": { home: {}, site: {}, articles: [] } };
    await site(
      async ({ page }) => {
        await page.goto(base + "/news");
        await expect(
          page.locator(".na-card").getByRole("heading", { name: notice.title }),
        ).toBeVisible();
      },
      {
        ...shell,
        "/api/public/articles": {
          enabled: true,
          site: {},
          articles: [],
          notice,
        },
      },
      null,
    );
    await site(
      async ({ page }) => {
        await page.goto(base + "/news");
        await expect(
          page.getByRole("heading", { name: "公告与教程", level: 1 }),
        ).toBeVisible();
        await expect(page.locator(".na-card")).toHaveCount(0);
      },
      {
        ...shell,
        "/api/public/articles": {
          enabled: true,
          site: {},
          articles: [],
          notice: null,
        },
      },
      null,
    );
  },
);
