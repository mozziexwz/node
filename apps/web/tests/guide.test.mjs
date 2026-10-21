import { test } from "node:test";
import assert from "node:assert/strict";
import { chromium, expect } from "@playwright/test";
import { build } from "esbuild";
import { fileURLToPath } from "node:url";

// Compiles the real application and answers every request from fixtures; no
// server or account is touched.
const base = "http://127.0.0.1:19988";
const member = {
  id: "guide-member",
  role: "user",
  status: "active",
  email: "10000001@qq.com",
};
const day = (time) =>
  new Date(time).toLocaleDateString("zh-CN", {
    month: "2-digit",
    day: "2-digit",
  });
const now = Date.UTC(2026, 9, 20, 4);
const articles = [
  {
    id: "deploy-guide",
    title: "[部署MSBOOST节点]",
    category: "教程",
    body: "部署正文",
    createdAt: now,
    pinned: true,
    attachments: [],
  },
  {
    id: "relay-guide",
    title: "[配置中转服务器]",
    category: "教程",
    body: "中转正文",
    createdAt: now - 86400000,
    attachments: [],
  },
  {
    id: "vps-news",
    title: "[线路机VPS资讯]",
    category: "资讯",
    body: "资讯正文",
    createdAt: now - 2 * 86400000,
    attachments: [],
  },
  {
    id: "older",
    title: "更早的公告",
    category: "公告",
    body: "旧正文",
    createdAt: now - 9 * 86400000,
    attachments: [],
  },
];

let script;
async function app() {
  if (script) return script;
  const compiled = await build({
    entryPoints: [fileURLToPath(new URL("../src/main.tsx", import.meta.url))],
    bundle: true,
    write: false,
    outdir: "in-memory",
    format: "iife",
    platform: "browser",
  });
  script = compiled.outputFiles.find((file) => file.path.endsWith(".js")).text;
  return script;
}

async function guideBrowser(
  run,
  { user = member, tasks = [], viewport = { width: 1440, height: 1000 } } = {},
) {
  const code = await app();
  const browser = await chromium.launch({
    headless: true,
    ...(process.platform === "win32" ? { channel: "chrome" } : {}),
  });
  const page = await browser.newPage({ viewport });
  page.setDefaultTimeout(7000);
  const errors = [];
  page.on("pageerror", (error) => errors.push(error.message));
  await page.route("**/*", (route) => {
    const url = new URL(route.request().url());
    if (url.origin !== base) {
      errors.push("external request " + url.origin);
      return route.abort();
    }
    if (url.pathname === "/")
      return route.fulfill({
        contentType: "text/html",
        body: `<div id="root"></div><script>${code}</script>`,
      });
    const responses = {
      "/api/me": { user, csrfToken: "fixture" },
      "/api/settings": { deploy: true, relay: true, dd: true },
      "/api/tasks": { tasks },
      "/api/articles": { articles },
      "/api/admin/overview": { users: 3, tasks: 7, routes: 2, orders: 1 },
      "/api/tickets": { tickets: [], unreadCount: 0 },
    };
    return route.fulfill({ json: responses[url.pathname] || {} });
  });
  try {
    await run(page);
    assert.deepEqual(errors, []);
  } finally {
    await browser.close();
  }
}

const progress = (page) => page.locator(".g-progress li");
const task = (kind, state, host, createdAt = now) => ({
  id: `${kind}-${host}-${state}-${createdAt}`,
  userId: member.id,
  kind,
  state,
  host,
  createdAt,
});

test(
  "a new member gets the guide with step 1 as the next step",
  { timeout: 60000 },
  async () => {
    await guideBrowser(async (page) => {
      await page.goto(base + "/#home");
      const nav = page.locator(".nav-group", { hasText: "开始使用" });
      await expect(nav.locator(".nav-item")).toHaveText([
        "公告/教程",
        "使用指南",
      ]);
      await expect(page.locator(".crumb b")).toHaveText("使用指南");
      await expect(page.getByRole("heading", { level: 1 })).toHaveText(
        "使用指南",
      );
      await expect(page.locator(".g-hero-main .badge")).toHaveText(
        "0 / 4 已完成",
      );
      await expect(progress(page).locator("small")).toHaveText([
        "下一步",
        "待完成",
        "可选 · 未配置",
        "待完成",
      ]);
      await expect(progress(page).nth(0)).toHaveClass(/current/);
      await expect(page.locator(".g-step")).toHaveCount(4);
      await expect(page.locator(".g-step.current h3")).toHaveText(
        "准备一台 VPS",
      );
      const next = page.locator(".g-next");
      await expect(next.locator("h3")).toHaveText(
        "准备一台 VPS，然后部署 MSBOOST 节点",
      );
      await next.getByRole("button", { name: "部署 MSBOOST" }).click();
      await expect(page).toHaveURL(/#deploy$/);
    });
  },
);

test(
  "progress follows the task records: deploying, deployed nodes and relays",
  { timeout: 60000 },
  async () => {
    await guideBrowser(
      async (page) => {
        await page.goto(base + "/#home");
        await expect(progress(page).locator("small")).toHaveText([
          "已完成",
          "正在部署",
          "可选 · 未配置",
          "待完成",
        ]);
        await expect(page.locator(".g-next h3")).toHaveText("节点正在部署");
        await page
          .locator(".g-next")
          .getByRole("button", { name: "查看任务记录" })
          .click();
        await expect(page).toHaveURL(/#tasks$/);
      },
      {
        tasks: [
          task("deploy", "running", "203.0.113.5"),
          task("deploy", "failed", "203.0.113.5"),
        ],
      },
    );
    await guideBrowser(
      async (page) => {
        await page.goto(base + "/#home");
        await expect(page.locator(".g-hero-main .badge")).toHaveText(
          "2 / 4 已完成",
        );
        await expect(progress(page).locator("small")).toHaveText([
          "已完成",
          `已部署 2 台 · ${day(now)}`,
          "可选 · 未配置",
          "下一步",
        ]);
        await expect(
          page.locator(".g-step").nth(1).locator(".badge"),
        ).toHaveText("已部署 2 台");
        await expect(page.locator(".g-step.current h3")).toHaveText(
          "下载启动器，开始加速",
        );
        await page
          .locator(".g-next")
          .getByRole("button", { name: "下载 MSBOOST 启动器" })
          .click();
        await expect(page).toHaveURL(/#launcher$/);
      },
      {
        tasks: [
          task("deploy", "succeeded", "203.0.113.5", now - 86400000),
          task("deploy", "succeeded", "203.0.113.5", now),
          task("deploy", "succeeded", "198.51.100.7", now - 3 * 86400000),
          task("dd", "executed", "198.51.100.7"),
        ],
      },
    );
    await guideBrowser(
      async (page) => {
        await page.goto(base + "/#home");
        await expect(page.locator(".g-hero-main .badge")).toHaveText(
          "3 / 4 已完成",
        );
        await expect(progress(page).nth(2).locator("small")).toHaveText(
          "已配置 2 条",
        );
        await expect(page.locator(".g-step").nth(2)).toHaveClass(/done/);
      },
      {
        tasks: [
          task("deploy", "succeeded", "203.0.113.5"),
          task("relay", "succeeded", "192.0.2.10"),
          task("relay-code", "succeeded", "192.0.2.11"),
          task("relay", "failed", "192.0.2.12"),
        ],
      },
    );
  },
);

test(
  "guide links open tools and a chosen article; administrators keep their overview",
  { timeout: 60000 },
  async () => {
    await guideBrowser(async (page) => {
      await page.goto(base + "/#home");
      const latest = page.locator(".g-news button");
      await expect(latest.locator("span")).toHaveText([
        "[部署MSBOOST节点]",
        "[配置中转服务器]",
        "[线路机VPS资讯]",
      ]);
      await latest.nth(1).click();
      await expect(page).toHaveURL(/#tutorials$/);
      await expect(page.locator(".article-content h2")).toHaveText(
        "[配置中转服务器]",
      );
      await page.goto(base + "/#home");
      await expect(page.locator(".g-links button b")).toHaveText([
        "MSBOOST 启动器",
        "部署 MSBOOST",
        "配置中转服务器",
        "中转码管理",
        "DD 系统",
        "任务记录",
      ]);
      await page.locator(".g-links button", { hasText: "中转码管理" }).click();
      await expect(page).toHaveURL(/#codes$/);
    });
    await guideBrowser(
      async (page) => {
        await page.goto(base + "/#home");
        await expect(page.getByRole("heading", { level: 1 })).toHaveText(
          "运营概览",
        );
        await expect(page.locator(".crumb b")).toHaveText("运营概览");
        await expect(
          page
            .locator(".nav-group", { hasText: "开始使用" })
            .locator(".nav-item"),
        ).toHaveText(["公告/教程", "运营概览"]);
        await expect(page.locator(".g-hero")).toHaveCount(0);
      },
      { user: { ...member, id: "guide-admin", role: "admin" } },
    );
  },
);

test("the guide fits a phone screen", { timeout: 60000 }, async () => {
  await guideBrowser(
    async (page) => {
      await page.goto(base + "/#home");
      await expect(page.locator(".g-step")).toHaveCount(4);
      assert.equal(
        await page.evaluate(
          () => document.documentElement.scrollWidth > innerWidth,
        ),
        false,
        "no horizontal overflow at 390px",
      );
      await page.screenshot({
        path: "../../.runtime/guide-mobile.png",
        fullPage: true,
      });
    },
    {
      tasks: [task("deploy", "succeeded", "203.0.113.5")],
      viewport: { width: 390, height: 844 },
    },
  );
});
