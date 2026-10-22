import { test } from "node:test";
import assert from "node:assert/strict";
import { chromium, expect } from "@playwright/test";
import { build } from "esbuild";
import { fileURLToPath } from "node:url";

// Compiles the real application and answers every request from fixtures; no
// server, account or download host is contacted.
const base = "http://127.0.0.1:19988";
const published = {
  version: "v1.0.0",
  releasedAt: "2026-11-05",
  downloadUrl: "https://file.msboost.de/#/?code=Local",
  backupUrl: "",
  sha256: "1a".repeat(32),
  notes: "- 部署、升级 MSBOOST 节点",
  updatedAt: 1760000000000,
};
const member = {
  id: "local-member",
  role: "user",
  status: "active",
  email: "10000001@qq.com",
};
const admin = { ...member, id: "local-admin", role: "admin" };
const home = {
  showExplain: true,
  showLauncher: false,
  features: [{ title: "MSBOOST 本地版", desc: "不填 SSH", target: "#local" }],
  msboost: {
    title: "什么是 MSBOOST？",
    nodes: [{ title: "你的电脑" }, { title: "节点" }, { title: "游戏" }],
    points: [{ title: "你的 VPS" }],
  },
  relay: {},
};

async function localBrowser(run, { user = member, localApp = published } = {}) {
  const compiled = await build({
    entryPoints: [fileURLToPath(new URL("../src/main.tsx", import.meta.url))],
    bundle: true,
    write: false,
    outdir: "in-memory",
    format: "iife",
    platform: "browser",
  });
  const script = compiled.outputFiles.find((file) =>
    file.path.endsWith(".js"),
  ).text;
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
    if (url.pathname === "/" || url.pathname === "/home")
      return route.fulfill({
        contentType: "text/html",
        body: `<div id="root"></div><script>${script}</script>`,
      });
    if (url.pathname === "/api/admin/settings" && request.method() === "PUT") {
      saves.push(request.postDataJSON());
      return route.fulfill({ json: { ok: true } });
    }
    const responses = {
      "/api/me": { user, csrfToken: "fixture" },
      "/api/settings": { deploy: true, relay: true, dd: true },
      "/api/public/launcher": { launcher: {}, localApp },
      "/api/public/home": {
        home,
        site: {},
        articles: [],
        launcher: {},
        localApp,
      },
      "/api/admin/settings": { localApp },
      "/api/tasks": { tasks: [] },
      "/api/tickets": { tickets: [], unreadCount: 0 },
    };
    return route.fulfill({ json: responses[url.pathname] || {} });
  });
  try {
    await run({ page, saves });
    assert.deepEqual(errors, []);
  } finally {
    await browser.close();
  }
}

test(
  "a published local app has its own page and is offered beside the SSH forms",
  { timeout: 60000 },
  async () => {
    await localBrowser(async ({ page }) => {
      await page.goto(base + "/#deploy");
      const tools = page.locator(".nav-group", { hasText: "免费部署工具" });
      await expect(tools.locator(".nav-item").nth(1)).toHaveText(
        "MSBOOST 本地版",
      );
      const hint = page.locator(".lc-hint");
      await expect(hint).toContainText("不想在网站填写 SSH？");
      await hint.getByRole("button", { name: "了解 MSBOOST 本地版" }).click();
      await expect(page).toHaveURL(/#local$/);
      await expect(page.getByRole("heading", { level: 1 })).toHaveText(
        "MSBOOST 本地版",
      );
      const download = page.getByRole("link", { name: "下载 v1.0.0" });
      await expect(download).toHaveAttribute("href", published.downloadUrl);
      await expect(download).toHaveAttribute("rel", "noopener noreferrer");
      await expect(page.getByText("本地版和网站怎么选")).toBeVisible();
      await expect(page.getByText("只从本站下载")).toBeVisible();
      await page.getByRole("button", { name: "使用指南" }).click();
      await page.getByRole("button", { name: "用 MSBOOST 本地版" }).click();
      await expect(page).toHaveURL(/#local$/);
    });
  },
);

test(
  "members see nothing of an unpublished local app",
  { timeout: 60000 },
  async () => {
    await localBrowser(
      async ({ page }) => {
        await page.goto(base + "/#deploy");
        await expect(page.getByRole("heading", { level: 1 })).toHaveText(
          "部署 MSBOOST",
        );
        await expect(
          page.getByRole("button", { name: "MSBOOST 本地版" }),
        ).toHaveCount(0);
        await expect(page.locator(".lc-hint")).toHaveCount(0);
        await page.getByRole("button", { name: "使用指南" }).click();
        await expect(page.getByText("按步骤完成")).toBeVisible();
        await expect(
          page.getByRole("button", { name: "用 MSBOOST 本地版" }),
        ).toHaveCount(0);
      },
      { localApp: {} },
    );
  },
);

test(
  "administrators publish the local app from its own settings tab",
  { timeout: 60000 },
  async () => {
    await localBrowser(
      async ({ page, saves }) => {
        await page.goto(base + "/#local");
        await expect(page.getByText("本地版下载地址尚未发布")).toBeVisible();
        await page.getByRole("button", { name: "去系统设置填写" }).click();
        await expect(page).toHaveURL(/#settings$/);
        await expect(page.locator(".tabs button.active")).toHaveText(
          "MSBOOST 本地版",
        );
        await page.getByLabel(/^当前版本/).fill("v1.0.0");
        await page.getByLabel("更新日期", { exact: true }).fill("2026-11-05");
        await page
          .getByLabel(/^下载地址/)
          .fill("https://file.msboost.de/#/?code=Local");
        await page.getByRole("button", { name: "保存", exact: true }).click();
        await expect(page.getByText("本地版设置已保存")).toBeVisible();
        assert.deepEqual(saves, [
          {
            localApp: {
              version: "v1.0.0",
              releasedAt: "2026-11-05",
              downloadUrl: "https://file.msboost.de/#/?code=Local",
              backupUrl: "",
              sha256: "",
              notes: "",
            },
          },
        ]);
        await expect(
          page.getByRole("link", { name: "查看主页下载区" }),
        ).toHaveAttribute("href", "/home#local");
      },
      { user: admin, localApp: {} },
    );
  },
);

test(
  "the homepage offers the local app to visitors once it is published",
  { timeout: 60000 },
  async () => {
    await localBrowser(async ({ page }) => {
      await page.goto(base + "/home");
      const section = page.locator("#local");
      await expect(section.getByRole("heading", { level: 2 })).toHaveText(
        "不想在网站填写 SSH？用本地版",
      );
      await expect(
        section.getByRole("link", { name: "下载 v1.0.0" }),
      ).toHaveAttribute("href", published.downloadUrl);
      await page.getByRole("link", { name: /不想填 SSH？用本地版/ }).click();
      await expect(page).toHaveURL(/\/home#local$/);
      await expect(
        page.locator("a.tool-card", { hasText: "MSBOOST 本地版" }),
      ).toHaveAttribute("href", "#local");
    });
    await localBrowser(
      async ({ page }) => {
        await page.goto(base + "/home");
        await expect(page.getByText("什么是 MSBOOST？")).toBeVisible();
        await expect(page.locator("#local")).toHaveCount(0);
        await expect(
          page.getByRole("link", { name: /不想填 SSH？用本地版/ }),
        ).toHaveCount(0);
        // A card aimed at the hidden section is shown but not clickable.
        await expect(
          page.locator("a.tool-card", { hasText: "MSBOOST 本地版" }),
        ).toHaveCount(0);
      },
      { localApp: {} },
    );
  },
);
