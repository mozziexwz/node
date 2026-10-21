import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import { chromium, expect } from "@playwright/test";
import { build } from "esbuild";
import { fileURLToPath } from "node:url";

// Compiles the real console and answers every request from fixtures; no
// server or SMTP service is contacted.
const base = "http://127.0.0.1:19990";
const screenshots = fileURLToPath(
  new URL("../../../.runtime/email-log-ui/", import.meta.url),
);
fs.mkdirSync(screenshots, { recursive: true });
const now = Date.now();
const admin = {
  id: "mail-admin",
  role: "admin",
  status: "active",
  email: "10000001@qq.com",
  expiresAt: 0,
};
const member = {
  ...admin,
  id: "mail-member",
  role: "user",
  email: "20000001@qq.com",
};
const kinds = [
  ["register", "注册验证码"],
  ["verify", "邮箱验证"],
  ["password_reset", "找回密码"],
  ["password_changed", "密码已修改"],
  ["relay_code_used", "中转码使用提醒"],
  ["relay_verify_failed", "中转线路校验未通过"],
  ["relay_verify_refused", "新配置的中转线路已撤销"],
  ["relay_verify_paused", "中转线路已暂停"],
  ["smtp_test", "SMTP 测试"],
  ["backup_alert", "整站备份提醒"],
].map(([kind, label]) => ({ kind, label }));
const named = (record) => ({
  ...record,
  label: kinds.find((k) => k.kind === record.kind).label,
});
const records = [
  {
    id: "r1",
    kind: "relay_verify_refused",
    recipient: "20000001@qq.com",
    userId: member.id,
    subject: "[MSBOOST] 新配置的中转线路未通过校验",
    status: "sent",
    createdAt: now - 60000,
  },
  {
    id: "r2",
    kind: "password_reset",
    recipient: "20000001@qq.com",
    userId: member.id,
    subject: "[MSBOOST] 找回密码验证码",
    status: "failed",
    failure: "authentication_failure",
    createdAt: now - 120000,
  },
  {
    id: "r3",
    kind: "smtp_test",
    recipient: "10000001@qq.com",
    subject: "[MSBOOST] 邮件服务测试",
    status: "failed",
    failure: "network_timeout",
    createdAt: now - 180000,
  },
  {
    id: "r4",
    kind: "backup_alert",
    recipient: "10000001@qq.com",
    userId: admin.id,
    subject: "MSBOOST 整站备份提醒",
    status: "sent",
    createdAt: now - 240000,
  },
  // More than one page of older registration codes.
  ...Array.from({ length: 226 }, (_, i) => ({
    id: `bulk-${i}`,
    kind: "register",
    recipient: `${30000000 + i}@qq.com`,
    subject: "[MSBOOST] 注册验证码",
    status: "sent",
    createdAt: now - 3600000 - i * 1000,
  })),
].map(named);

// Mirrors the server's filters so the page's query is what decides the rows.
function emailLog(url) {
  const kind = url.searchParams.get("kind") || "",
    status = url.searchParams.get("status") || "",
    q = (url.searchParams.get("q") || "").trim().toLowerCase(),
    limit = Number(url.searchParams.get("limit") || 200);
  const matched = records.filter(
    (r) =>
      (!kind || r.kind === kind) &&
      (!status || r.status === status) &&
      (!q ||
        r.recipient.toLowerCase().includes(q) ||
        r.subject.toLowerCase().includes(q)),
  );
  return {
    records: matched.slice(0, limit),
    total: matched.length,
    kinds,
    retentionDays: 180,
    maxRecords: 5000,
  };
}

let compiled;
async function app() {
  if (compiled) return compiled;
  const output = await build({
    entryPoints: [fileURLToPath(new URL("../src/main.tsx", import.meta.url))],
    bundle: true,
    write: false,
    outdir: "in-memory",
    format: "iife",
    platform: "browser",
  });
  const file = (ext) =>
    output.outputFiles.find((f) => f.path.endsWith(ext))?.text || "";
  compiled = { script: file(".js"), style: file(".css") };
  return compiled;
}

async function mailBrowser(
  run,
  { user = admin, viewport = { width: 1440, height: 1000 } } = {},
) {
  const { script, style } = await app();
  const browser = await chromium.launch({
    headless: true,
    ...(process.platform === "win32" ? { channel: "chrome" } : {}),
  });
  const page = await browser.newPage({ viewport });
  page.setDefaultTimeout(7000);
  const errors = [],
    queries = [];
  page.on("pageerror", (error) => errors.push(error.message));
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
        body: `<style>${style}</style><div id="root"></div><script>${script}</script>`,
      });
    if (request.method() !== "GET") {
      errors.push("unexpected write " + url.pathname);
      return route.fulfill({ status: 405, json: { error: "fixture" } });
    }
    if (url.pathname === "/api/admin/email-log") {
      queries.push(url.searchParams);
      return route.fulfill({ json: emailLog(url) });
    }
    const responses = {
      "/api/me": { user, csrfToken: "fixture" },
      "/api/settings": { deploy: true, relay: true, dd: true },
      "/api/tickets": { tickets: [], unreadCount: 0 },
    };
    return route.fulfill({ json: responses[url.pathname] || {} });
  });
  try {
    await run(page, queries);
    assert.deepEqual(errors, []);
  } finally {
    await browser.close();
  }
}

test(
  "administrators open the email log from platform management",
  { timeout: 60000 },
  async () => {
    await mailBrowser(async (page) => {
      await page.goto(base + "/#tutorials");
      await page
        .getByRole("navigation", { name: "主导航" })
        .getByRole("region", { name: "平台管理" })
        .getByRole("button", { name: "邮件记录" })
        .click();
      await expect(page).toHaveURL(/#emailLog$/);
      await expect(
        page.getByRole("heading", { level: 1, name: "邮件记录" }),
      ).toBeVisible();
      assert.deepEqual(await page.locator("thead th").allInnerTexts(), [
        "时间",
        "类型",
        "收件人",
        "主题",
        "结果",
      ]);
      const rows = page.locator("tbody tr");
      await expect(rows).toHaveCount(200);
      await expect(page.getByText("共 230 条，显示最近 200 条")).toBeVisible();
      // Newest first, the full address, and a failure's category in words.
      await expect(rows.nth(0)).toContainText("新配置的中转线路已撤销");
      await expect(rows.nth(0)).toContainText("20000001@qq.com");
      await expect(rows.nth(0)).toContainText(
        "[MSBOOST] 新配置的中转线路未通过校验",
      );
      await expect(rows.nth(0)).toContainText("已发送");
      await expect(rows.nth(1)).toContainText("发送失败");
      await expect(rows.nth(1)).toContainText(
        "SMTP 登录失败，请检查登录账号和授权码",
      );
      await expect(rows.nth(2)).toContainText("连接 SMTP 服务器超时");
      await expect(rows.nth(3)).toContainText("整站备份提醒");
      const notice = page.locator(".notice").filter({ hasText: "已发送" });
      await expect(notice).toContainText("不代表已进入收件箱");
      await expect(notice).toContainText("不保存邮件正文、验证码、链接");
      await expect(notice).toContainText("保留 180 天");
      await page.screenshot({
        path: screenshots + "desktop.png",
        fullPage: false,
      });
      const text = await page.locator("main").innerText();
      assert.doesNotMatch(text, /转发|冒险岛|GitHub/);
    });
  },
);

test(
  "the log filters by kind and result and searches recipients",
  { timeout: 60000 },
  async () => {
    await mailBrowser(async (page, queries) => {
      await page.goto(base + "/#emailLog");
      const rows = page.locator("tbody tr");
      await expect(rows).toHaveCount(200);
      const kind = page.getByLabel("按类型筛选");
      await expect(kind.locator("option")).toHaveText([
        "全部类型",
        ...kinds.map((k) => k.label),
      ]);
      await kind.selectOption({ label: "找回密码" });
      await expect(rows).toHaveCount(1);
      await expect(rows.first()).toContainText("SMTP 登录失败");
      await kind.selectOption({ label: "全部类型" });
      await page.getByLabel("按结果筛选").selectOption({ label: "发送失败" });
      await expect(rows).toHaveCount(2);
      await expect(page.getByText(/显示最近/)).toHaveCount(0);
      await page.getByLabel("按结果筛选").selectOption({ label: "全部结果" });
      await page.getByLabel("搜索收件人或主题").fill(" 20000001@QQ.com ");
      await expect(rows).toHaveCount(2);
      await expect(rows.nth(0)).toContainText("新配置的中转线路已撤销");
      await expect(rows.nth(1)).toContainText("找回密码");
      await page.getByLabel("搜索收件人或主题").fill("没有这封邮件");
      await expect(page.getByText("暂无记录")).toBeVisible();
      // The filters reach the server instead of hiding rows in the browser.
      const sent = queries.map((q) => Object.fromEntries(q));
      assert.ok(sent.some((q) => q.kind === "password_reset"));
      assert.ok(sent.some((q) => q.status === "failed" && !q.kind));
      assert.ok(sent.some((q) => q.q === "20000001@QQ.com"));
      assert.ok(sent.every((q) => q.limit === "200"));
    });
  },
);

test(
  "members neither see nor load the email log",
  { timeout: 60000 },
  async () => {
    await mailBrowser(
      async (page, queries) => {
        await page.goto(base + "/#emailLog");
        await expect(page.getByText("暂无记录")).toBeVisible();
        await expect(
          page
            .getByRole("navigation", { name: "主导航" })
            .getByRole("button", { name: "邮件记录" }),
        ).toHaveCount(0);
        assert.equal(queries.length, 0);
      },
      { user: member },
    );
  },
);

test(
  "the log fits a phone screen without sideways page scrolling",
  { timeout: 60000 },
  async () => {
    await mailBrowser(
      async (page) => {
        await page.goto(base + "/#emailLog");
        await expect(page.locator("tbody tr")).toHaveCount(200);
        const overflow = await page.evaluate(
          () => document.documentElement.scrollWidth - window.innerWidth,
        );
        assert.ok(overflow <= 0, `page scrolls sideways by ${overflow}px`);
        await page.screenshot({
          path: screenshots + "phone.png",
          fullPage: false,
        });
      },
      { viewport: { width: 375, height: 812 } },
    );
  },
);
