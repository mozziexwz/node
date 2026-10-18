import { test } from "node:test";
import assert from "node:assert/strict";
import { chromium, expect } from "@playwright/test";
import { build } from "esbuild";
import { fileURLToPath } from "node:url";

// Compiles the real application and answers every request from fixtures; no
// server, account or download host is contacted.
const base = "http://127.0.0.1:19987";
const published = {
  version: "v1.0.2",
  releasedAt: "2026-10-17",
  downloadUrl: "https://file.msboost.de/#/?code=AbC123",
  backupUrl: "",
  sha256: "",
  notes: "- 修复启动问题",
  updatedAt: 1760000000000,
};
const member = {
  id: "launcher-member",
  role: "user",
  status: "active",
  email: "10000001@qq.com",
};
const admin = { ...member, id: "launcher-admin", role: "admin" };

async function launcherBrowser(
  run,
  { user = member, launcher = published } = {},
) {
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
  const task = {
    id: "task-launcher",
    userId: user.id,
    kind: "deploy",
    state: "succeeded",
    configAvailable: true,
    createdAt: Date.now(),
    host: "8.8.8.8",
  };
  await page.route("**/*", (route) => {
    const request = route.request(),
      url = new URL(request.url());
    if (url.origin !== base) {
      errors.push("external request " + url.origin);
      return route.abort();
    }
    if (url.pathname === "/")
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
      "/api/public/launcher": { launcher },
      "/api/admin/settings": { launcher },
      "/api/tasks": { tasks: [task] },
      [`/api/tasks/${task.id}/config`]: { synthetic: "no real credentials" },
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
  "the launcher page comes first among the free tools and links to the download",
  { timeout: 60000 },
  async () => {
    await launcherBrowser(async ({ page }) => {
      await page.goto(base + "/#launcher");
      const tools = page.locator(".nav-group", { hasText: "免费部署工具" });
      await expect(tools.locator(".nav-item").first()).toHaveText(
        "MSBOOST 启动器",
      );
      await expect(page.getByRole("heading", { level: 1 })).toHaveText(
        "MSBOOST 启动器",
      );
      const download = page.getByRole("link", { name: "下载 v1.0.2" });
      await expect(download).toHaveAttribute("href", published.downloadUrl);
      await expect(download).toHaveAttribute("rel", "noopener noreferrer");
      await expect(page.getByRole("link", { name: "备用下载" })).toHaveCount(0);
      await expect(page.getByText("更新内容 · v1.0.2")).toBeVisible();
      await expect(page.locator(".launcher-notes li")).toHaveText(
        "修复启动问题",
      );
      await page.getByRole("button", { name: "去部署 MSBOOST" }).click();
      await expect(page).toHaveURL(/#deploy$/);
    });
  },
);

test(
  "an unpublished launcher sends administrators to its settings, which save the download",
  { timeout: 60000 },
  async () => {
    await launcherBrowser(
      async ({ page, saves }) => {
        await page.goto(base + "/#launcher");
        await expect(page.getByText("启动器下载地址尚未发布")).toBeVisible();
        await page.getByRole("button", { name: "去系统设置填写" }).click();
        await expect(page).toHaveURL(/#settings$/);
        await expect(page.locator(".tabs button.active")).toHaveText(
          "MSBOOST 启动器",
        );
        // Field hints are part of the label text.
        await page.getByLabel(/^当前版本/).fill("v1.0.3");
        await page.getByLabel("更新日期", { exact: true }).fill("2026-10-18");
        await page
          .getByLabel(/^下载地址/)
          .fill("https://file.msboost.de/#/?code=New");
        await page.getByLabel(/^更新内容/).fill("- 新版本");
        await page.getByRole("button", { name: "保存", exact: true }).click();
        await expect(page.getByText("启动器设置已保存")).toBeVisible();
        assert.deepEqual(saves, [
          {
            launcher: {
              version: "v1.0.3",
              releasedAt: "2026-10-18",
              downloadUrl: "https://file.msboost.de/#/?code=New",
              backupUrl: "",
              sha256: "",
              notes: "- 新版本",
            },
          },
        ]);
      },
      { user: admin, launcher: {} },
    );
  },
);

test(
  "a finished deployment points to the launcher next to its configuration",
  { timeout: 60000 },
  async () => {
    await launcherBrowser(async ({ page }) => {
      await page.goto(base + "/#tasks");
      await page.getByRole("button", { name: "详情", exact: true }).click();
      await expect(
        page.getByRole("button", { name: "下载配置", exact: true }),
      ).toBeVisible();
      await page.getByRole("button", { name: /获取 MSBOOST 启动器/ }).click();
      await expect(page).toHaveURL(/#launcher$/);
      await expect(
        page.getByRole("link", { name: "下载 v1.0.2" }),
      ).toBeVisible();
      await expect(page.getByRole("dialog")).toHaveCount(0);
    });
  },
);
