import { test } from "node:test";
import assert from "node:assert/strict";
import { chromium, expect } from "@playwright/test";
import { build } from "esbuild";
import { fileURLToPath } from "node:url";

// Compiles the real application and answers every request from fixtures; no
// server or relay node is contacted.
const base = "http://127.0.0.1:19991";
const minute = 60000;
const now = Date.now();
const member = {
  id: "probe-member",
  role: "user",
  status: "active",
  email: "10000001@qq.com",
  expiresAt: now + 86400000 * 20,
  level: 2,
  trafficTotal: 1e11,
  trafficUsed: 0,
  rateMbps: 5,
};
const admin = { ...member, id: "probe-admin", role: "admin" };

// One result every two minutes for a day; failed lists minutes-ago ranges.
function samples(ms, failed = []) {
  const out = [];
  for (let ago = 1438; ago >= 0; ago -= 2)
    out.push([
      now - ago * minute,
      failed.some(([from, to]) => ago <= from && ago >= to) ? -1 : ms,
    ]);
  return out;
}
const healthy = {
  enabled: true,
  intervalSeconds: 120,
  retentionHours: 24,
  now,
  routes: [
    {
      routeId: "r1",
      routeName: "日本线路",
      level: 1,
      state: "active",
      samples: samples(102),
    },
    {
      routeId: "r2",
      routeName: "香港线路",
      level: 2,
      state: "active",
      // Nearly half an hour without a connection, seven hours ago: long enough
      // to cover a whole ten-minute period of the line wherever it falls.
      samples: samples(148, [[444, 420]]),
    },
    {
      routeId: "r3",
      routeName: "新加坡线路",
      level: 2,
      state: "paused",
      samples: [],
    },
    {
      routeId: "r4",
      routeName: "美西线路",
      level: 1,
      state: "active",
      samples: [],
    },
  ],
};
const down = {
  ...healthy,
  routes: healthy.routes.slice(0, 2).map((route) => ({
    ...route,
    samples: samples(100, [[12, 0]]),
  })),
};

async function probeBrowser(
  run,
  { user = member, probe = healthy, width = 1440 } = {},
) {
  const compiled = await build({
    entryPoints: [fileURLToPath(new URL("../src/main.tsx", import.meta.url))],
    bundle: true,
    write: false,
    outdir: "in-memory",
    format: "iife",
    platform: "browser",
  });
  const output = (suffix) =>
    compiled.outputFiles.find((file) => file.path.endsWith(suffix)).text;
  const browser = await chromium.launch({
    headless: true,
    ...(process.platform === "win32" ? { channel: "chrome" } : {}),
  });
  const page = await browser.newPage({ viewport: { width, height: 1000 } });
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
    if (url.pathname === "/")
      return route.fulfill({
        contentType: "text/html",
        body: `<style>${output(".css")}</style><div id="root"></div><script>${output(".js")}</script>`,
      });
    if (url.pathname === "/api/admin/settings" && request.method() === "PUT") {
      saves.push(request.postDataJSON());
      return route.fulfill({ json: { ok: true } });
    }
    const responses = {
      "/api/me": { user, csrfToken: "fixture" },
      "/api/settings": { deploy: true, relay: true, dd: true },
      "/api/public/launcher": { launcher: {}, localApp: {} },
      "/api/user/probe": probe,
      "/api/admin/settings": {
        relayProbe: { enabled: true, intervalMinutes: 2 },
      },
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
  "members with an entitlement see each route's probe history",
  { timeout: 60000 },
  async () => {
    await probeBrowser(async ({ page }) => {
      await page.goto(base + "/#traffic");
      const paid = page.locator(".nav-group", { hasText: "捐赠权益" });
      await expect(paid.locator(".nav-item")).toHaveText([
        "中转线路",
        "流量统计",
        "探针",
        "线路状态",
        "枫叶兑换",
        "我的枫叶",
        "兑换记录",
      ]);
      await paid.getByRole("button", { name: "探针" }).click();
      await expect(page).toHaveURL(/#probe$/);
      await expect(page.getByRole("heading", { level: 1 })).toHaveText("探针");
      await expect(
        page.getByText("每 2 分钟从各线路出口连接一次你的 MSBOOST 节点"),
      ).toBeVisible();
      const tiles = page.locator(".grid4 .stat");
      await expect(tiles.nth(0)).toContainText("监控线路");
      await expect(tiles.nth(0).locator(".stat-value")).toHaveText("3条");
      await expect(tiles.nth(1).locator(".stat-value")).toHaveText("2条");
      // 13 failures in 1440 results, rounded down.
      await expect(tiles.nth(2)).toContainText("近 24 小时连接成功率");
      await expect(tiles.nth(2).locator(".stat-value")).toHaveText("99.0%");

      const cards = page.locator(".probe-card");
      await expect(cards).toHaveCount(4);
      const japan = cards.nth(0);
      await expect(japan.locator(".badge.green")).toHaveText("连接正常");
      await expect(japan.locator(".probe-now .stat-value")).toHaveText("102ms");
      await expect(japan.locator(".probe-facts")).toContainText("100%");
      await expect(japan.locator(".probe-chart svg")).toBeVisible();
      // No failures: the result bar is one unbroken piece.
      await expect(japan.locator(".probe-chart svg rect")).toHaveCount(1);
      const hongKong = cards.nth(1);
      await expect(hongKong.locator(".probe-facts")).toContainText("13 次");
      await expect(hongKong.locator(".probe-facts")).toContainText("98.1%");
      await expect(hongKong.locator(".probe-chart svg rect")).toHaveCount(3);
      // The line is broken where nothing connected.
      await expect(
        hongKong.locator('.probe-chart svg path[fill="none"]'),
      ).toHaveCount(2);
      await expect(cards.nth(2)).toContainText("已暂停");
      await expect(cards.nth(2)).toContainText("暂停期间不探测");
      await expect(cards.nth(2).locator(".probe-chart")).toHaveCount(0);
      await expect(cards.nth(3)).toContainText("正在等待第一次探测结果");
      await expect(page.locator(".notice.orange")).toHaveCount(0);
      await expect(page.getByText("不等于游戏内延迟")).toBeVisible();

      // Hovering a period without a connection says so.
      const chart = hongKong.locator(".probe-chart");
      const box = await chart.boundingBox();
      const at = 46 + ((1440 - 432) / 1440) * (box.width - 56);
      await page.mouse.move(box.x + at, box.y + 60);
      await expect(chart.locator(".probe-tip")).toContainText("失败");
      await page.mouse.move(box.x + box.width - 60, box.y + 60);
      await expect(chart.locator(".probe-tip")).toContainText("148 ms");
      // The same details by keyboard.
      await chart.focus();
      await page.keyboard.press("ArrowLeft");
      await expect(chart.locator(".probe-tip")).toContainText("148 ms");
      await page.keyboard.press("Escape");
      await expect(chart.locator(".probe-tip")).toHaveCount(0);

      // The outage is outside the last six hours.
      await page.getByRole("button", { name: "6 小时" }).click();
      await expect(tiles.nth(2)).toContainText("近 6 小时连接成功率");
      await expect(tiles.nth(2).locator(".stat-value")).toHaveText("100%");
      await expect(hongKong.locator(".probe-chart svg rect")).toHaveCount(1);

      await page.getByRole("button", { name: "24 小时" }).click();
      await page.getByRole("button", { name: "表格" }).click();
      await expect(page.locator(".probe-legend")).toHaveCount(0);
      await expect(hongKong.locator(".probe-chart")).toHaveCount(0);
      await expect(hongKong.locator("thead th")).toHaveText([
        "时间段",
        "探测",
        "失败",
        "平均延迟",
        "最高延迟",
      ]);
      // Four-hour periods; together they hold every result and failure.
      const column = async (n) =>
        (await hongKong.locator(`tbody td:nth-child(${n})`).allTextContents())
          .map(Number)
          .reduce((sum, value) => sum + value, 0);
      assert.ok((await hongKong.locator("tbody tr").count()) >= 7);
      assert.equal(await column(2), 720);
      assert.equal(await column(3), 13);
    });
  },
);

test(
  "a node no route can reach is pointed out",
  { timeout: 60000 },
  async () => {
    await probeBrowser(
      async ({ page }) => {
        await page.goto(base + "/#probe");
        const cards = page.locator(".probe-card");
        await expect(cards).toHaveCount(2);
        await expect(page.locator(".notice.orange")).toContainText(
          "都连不上你的 MSBOOST 节点",
        );
        for (const index of [0, 1]) {
          await expect(cards.nth(index).locator(".badge.red")).toHaveText(
            "连接失败",
          );
          await expect(cards.nth(index).locator(".probe-state")).toContainText(
            "起",
          );
          await expect(
            cards.nth(index).locator(".probe-now .stat-value"),
          ).toHaveText("—");
        }
        await expect(
          page.locator(".grid4 .stat").nth(1).locator(".stat-value"),
        ).toHaveText("0条");
        // A phone shows the same page without sideways scrolling.
        assert.equal(
          await page.evaluate(
            () =>
              document.documentElement.scrollWidth -
              document.documentElement.clientWidth,
          ),
          0,
        );
      },
      { probe: down, width: 390 },
    );
  },
);

test(
  "without routes or when switched off the page explains itself",
  { timeout: 60000 },
  async () => {
    await probeBrowser(
      async ({ page }) => {
        await page.goto(base + "/#probe");
        await expect(
          page.getByText("配置中转线路后，这里会自动开始记录。"),
        ).toBeVisible();
        await expect(page.locator(".probe-card")).toHaveCount(0);
        await page.getByRole("button", { name: "去配置中转线路" }).click();
        await expect(page).toHaveURL(/#routes$/);
      },
      { probe: { ...healthy, routes: [] } },
    );
    await probeBrowser(
      async ({ page }) => {
        await page.goto(base + "/#probe");
        await expect(page.getByText("探针暂时关闭。")).toBeVisible();
        await expect(page.locator(".grid4")).toHaveCount(0);
      },
      { probe: { enabled: false, routes: [] } },
    );
    // Members without an entitlement have no menu item.
    await probeBrowser(
      async ({ page }) => {
        await page.goto(base + "/");
        await expect(
          page.getByRole("button", { name: "枫叶兑换" }),
        ).toBeVisible();
        await expect(page.getByRole("button", { name: "探针" })).toHaveCount(0);
      },
      { user: { ...member, expiresAt: 0 } },
    );
  },
);

test(
  "administrators switch the probe and set its interval",
  { timeout: 60000 },
  async () => {
    await probeBrowser(
      async ({ page, saves }) => {
        await page.goto(base + "/#settings");
        await page
          .locator(".tabs")
          .getByRole("button", { name: "探针" })
          .click();
        await expect(page.getByLabel("开启探针")).toBeChecked();
        await page.getByLabel(/^探测间隔/).fill("5");
        await page.getByLabel("开启探针").uncheck();
        await page.getByRole("button", { name: "保存", exact: true }).click();
        await expect(page.getByText("探针设置已保存")).toBeVisible();
        assert.deepEqual(saves, [
          { relayProbe: { enabled: false, intervalMinutes: 5 } },
        ]);
      },
      { user: admin },
    );
  },
);
