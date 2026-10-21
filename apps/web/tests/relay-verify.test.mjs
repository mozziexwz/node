import { test } from "node:test";
import assert from "node:assert/strict";
import { chromium, expect } from "@playwright/test";
import { build } from "esbuild";
import { fileURLToPath } from "node:url";

// Compiles the real application and answers every request from fixtures; no
// server, executor or node is contacted.
const base = "http://127.0.0.1:19989";
const now = Date.now();
const member = {
  id: "verify-member",
  role: "user",
  status: "active",
  email: "10000001@qq.com",
  expiresAt: now + 86400000 * 20,
  trafficTotal: 1e11,
  trafficUsed: 0,
  rateMbps: 5,
};
const admin = { ...member, id: "verify-admin", role: "admin" };
const routes = ["r1", "r2", "r3"].map((id, i) => ({
  id,
  name: `线路${i + 1}`,
  online: true,
  level: 1,
  rateMbps: 5,
}));
const ruleBase = {
  state: "active",
  syncState: "active",
  readySegments: 2,
  totalSegments: 2,
  effectiveRateMbps: 5,
  appliedRateMbps: 5,
  controlStatus: "online",
  runtimeStatus: "last_reported_running",
  offlinePolicy: "keep_last",
  keepLastConfirmed: true,
  trafficBytes: 0,
  targetHost: "203.0.113.9",
  targetPort: 31234,
  userEmail: member.email,
  userId: member.id,
};
const rules = [
  {
    ...ruleBase,
    id: "rule-ok",
    routeId: "r1",
    routeName: "线路1",
    verifyMode: "enforce",
    verify: {
      state: "ok",
      code: "ok",
      game: "ok",
      filter: "ok",
      checkedAt: now - 3600000,
      passedAt: now - 3600000,
    },
  },
  {
    ...ruleBase,
    id: "rule-failed",
    routeId: "r2",
    routeName: "线路2",
    verifyMode: "enforce",
    verifyPauseAt: now + 40 * 3600000,
    verify: {
      state: "failed",
      code: "not_msboost",
      checkedAt: now - 600000,
      failedSince: now - 8 * 3600000,
      passedAt: now - 86400000,
    },
  },
  {
    ...ruleBase,
    id: "rule-new",
    routeId: "r3",
    routeName: "线路3",
    state: "active",
    verifyMode: "enforce",
    verify: { state: "pending", checkedAt: 0 },
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

async function verifyBrowser(run, { user = member } = {}) {
  const code = await app();
  const browser = await chromium.launch({
    headless: true,
    ...(process.platform === "win32" ? { channel: "chrome" } : {}),
  });
  const page = await browser.newPage({
    viewport: { width: 1440, height: 1000 },
  });
  page.setDefaultTimeout(7000);
  const errors = [],
    posts = [];
  page.on("pageerror", (error) => errors.push(error.message));
  const settings = {
    relayVerify: {
      free: "selftest",
      code: "log",
      donation: "log",
      recheckHours: 24,
      graceHours: 48,
      breakerPercent: 20,
      gameTarget: "35.155.204.207:8585",
      filterBlocked: "www.youtube.com",
      filterAllowed: "www.nexon.com",
    },
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
        body: `<div id="root"></div><script>${code}</script>`,
      });
    if (request.method() !== "GET") {
      posts.push({
        method: request.method(),
        path: url.pathname,
        body: request.postDataJSON(),
      });
      if (url.pathname === "/api/admin/settings")
        Object.assign(settings, request.postDataJSON());
      if (
        url.pathname.endsWith("/verify") &&
        url.pathname.startsWith("/api/admin/")
      )
        return route.fulfill({
          json: {
            ...rules[1],
            verify: {
              ...rules[1].verify,
              state: "exempt",
              exemptReason: request.postDataJSON().reason,
            },
          },
        });
      return route.fulfill({ status: 202, json: { ok: true } });
    }
    const task = {
      id: "relay-task",
      userId: user.id,
      kind: "relay",
      state: "succeeded",
      phase: "complete",
      host: "8.8.8.8",
      createdAt: now,
      configAvailable: false,
      health: {
        service: "running",
        localSelfTest: "target_tcp_reachable",
        publicTCP: "reachable",
        game: "not_tested",
        bbr: "enabled",
      },
      verify: { code: "not_msboost", elapsedMs: 1200 },
    };
    const responses = {
      "/api/me": { user, csrfToken: "fixture" },
      "/api/settings": { deploy: true, relay: true, dd: true },
      "/api/routes": { routes, userRateMbps: 5 },
      "/api/user/rules": { rules, userRateMbps: 5 },
      "/api/admin/user-rules": { rules },
      "/api/admin/relay-accounting": {},
      "/api/admin/settings": settings,
      "/api/tasks": { tasks: [task] },
      "/api/tasks/relay-task": task,
      "/api/tickets": { tickets: [], unreadCount: 0 },
    };
    return route.fulfill({ json: responses[url.pathname] || {} });
  });
  try {
    await run(page, posts, settings);
    assert.deepEqual(errors, []);
  } finally {
    await browser.close();
  }
}

test(
  "route cards explain each verification state and let the owner recheck",
  { timeout: 60000 },
  async () => {
    await verifyBrowser(async (page, posts) => {
      await page.goto(base + "/#routes");
      const cards = page.locator(".route-card");
      await expect(cards).toHaveCount(3);
      await expect(cards.nth(0).getByLabel("目标校验")).toContainText(
        "目标已校验为 MSBOOST 节点",
      );
      await expect(
        cards.nth(0).getByRole("button", { name: "重新校验" }),
      ).toHaveCount(0);
      const failed = cards.nth(1).getByLabel("目标校验");
      await expect(failed).toContainText(
        "不是 MSBOOST 节点，或节点版本较低，请重新部署MSBOOST节点。",
      );
      await expect(failed).toContainText("这条线路会暂停");
      await expect(failed).toContainText("用新的配置文件重新配置");
      // A new rule under enforcement cannot be downloaded before it passes.
      await expect(
        cards.nth(2).getByRole("button", { name: "校验通过后可下载" }),
      ).toBeDisabled();
      await expect(cards.nth(2).getByLabel("目标校验")).toContainText(
        "通过后才能下载配置",
      );
      await failed.getByRole("button", { name: "重新校验" }).click();
      await expect(
        failed.getByRole("button", { name: "校验中…" }),
      ).toBeDisabled();
      assert.deepEqual(
        posts.map((p) => p.path),
        ["/api/user/routes/r2/verify"],
      );
      const text = await page.locator("main").innerText();
      assert.doesNotMatch(text, /转发|冒险岛|root 运行/);
    });
  },
);

test(
  "a free relay result shows its self-test",
  { timeout: 60000 },
  async () => {
    await verifyBrowser(async (page) => {
      await page.goto(base + "/#tasks");
      await page.getByRole("button", { name: "详情", exact: true }).click();
      const selfTest = page.getByLabel("中转自测");
      await expect(selfTest).toContainText("MSBOOST 节点");
      await expect(selfTest).toContainText("不是 MSBOOST 节点，或节点版本较低");
      await expect(selfTest).toContainText("用新的配置文件重新配置中转");
    });
  },
);

test(
  "administrators filter by verification, exempt with a reason and set the policy",
  { timeout: 60000 },
  async () => {
    await verifyBrowser(
      async (page, posts, settings) => {
        await page.goto(base + "/#rules");
        await expect(page.locator("tbody tr")).toHaveCount(3);
        await page.getByLabel("按目标校验筛选").selectOption("failed");
        await expect(page.locator("tbody tr")).toHaveCount(1);
        await expect(page.locator("tbody tr")).toContainText("校验未通过");
        await page
          .locator("tbody tr")
          .getByRole("button", { name: "详情" })
          .click();
        const panel = page.getByRole("dialog").getByLabel("目标校验");
        await expect(panel).toContainText("不是 MSBOOST 节点");
        const exempt = panel.getByRole("button", { name: "豁免这条规则" });
        await expect(exempt).toBeDisabled();
        await panel.getByLabel(/豁免原因/).fill("客户自建节点，已人工核查");
        await exempt.click();
        await expect.poll(() => posts.length).toBe(1);
        assert.deepEqual(posts[0], {
          method: "POST",
          path: "/api/admin/user-rules/rule-failed/verify",
          body: { action: "exempt", reason: "客户自建节点，已人工核查" },
        });
        await page.goto(base + "/#settings");
        await page
          .getByRole("button", { name: "中转校验", exact: true })
          .click();
        await page.getByLabel(/^捐赠权益线路/).selectOption("enforce");
        await page.getByLabel(/^中转码/).selectOption("enforce");
        await page.getByRole("button", { name: "保存", exact: true }).click();
        await expect(page.getByText("中转校验设置已保存")).toBeVisible();
        assert.equal(settings.relayVerify.donation, "enforce");
        assert.equal(settings.relayVerify.code, "enforce");
        assert.equal(settings.relayVerify.free, "selftest");
      },
      { user: admin },
    );
  },
);
