import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import { fileURLToPath } from "node:url";
import { chromium, expect } from "@playwright/test";
import { build } from "esbuild";

const base = "http://127.0.0.1:19987";
const screenshots = fileURLToPath(
  new URL("../../../.runtime/task-audit-ui/", import.meta.url),
);
fs.mkdirSync(screenshots, { recursive: true });
async function mount(browser, component, handle, beforeRender = "") {
  const compiled = await build({
    stdin: {
      contents: `import React from 'react';import {createRoot} from 'react-dom/client';import {ToolPage,TasksPage} from './tools';import {saveLocal} from './vault';import './app.css';(async()=>{${beforeRender};createRoot(document.getElementById('app')).render(<main className="content">${component}</main>);})();`,
      resolveDir: fileURLToPath(new URL("../src/", import.meta.url)),
      loader: "jsx",
    },
    bundle: true,
    write: false,
    outdir: "in-memory",
    format: "iife",
    platform: "browser",
  }).catch(async (error) => {
    await browser.close();
    throw error;
  });
  const page = await browser.newPage({
    viewport: { width: 1440, height: 1100 },
  });
  const errors = [],
    external = [];
  page.on("pageerror", (e) => errors.push(e.message));
  await page.route("**/*", (route) => {
    const url = new URL(route.request().url());
    if (url.origin !== base) {
      external.push(url.origin);
      return route.abort();
    }
    if (url.pathname === "/")
      return route.fulfill({
        contentType: "text/html",
        body: '<div id="app"></div>',
      });
    return handle(route, url);
  });
  await page.goto(base);
  await page.addStyleTag({
    content: compiled.outputFiles.find((f) => f.path.endsWith(".css")).text,
  });
  await page.addScriptTag({
    content: compiled.outputFiles.find((f) => f.path.endsWith(".js")).text,
  });
  return { page, errors, external };
}
const launch = () =>
  chromium.launch({
    headless: true,
    ...(process.platform === "win32" ? { channel: "chrome" } : {}),
  });

test(
  "deployment errors follow SSH advanced settings and precede three consecutive notices",
  { timeout: 60000 },
  async () => {
    const browser = await launch();
    try {
      for (const mode of ["identity", "fingerprint-502", "deploy-502"]) {
        const writes = [];
        const { page, errors, external } = await mount(
          browser,
          '<ToolPage kind="deploy" user={{id:"fixture-user",role:"user"}} settings={{}} onNavigate={()=>{}}/>',
          (route, url) => {
            if (
              url.pathname === "/api/tasks" &&
              route.request().method() === "GET"
            )
              return route.fulfill({ json: { tasks: [] } });
            writes.push(url.pathname);
            if (url.pathname === "/api/fingerprints") {
              if (mode === "fingerprint-502")
                return route.fulfill({
                  status: 502,
                  json: { error: "请求失败 (502)" },
                });
              return route.fulfill({
                json: {
                  fingerprint: "SHA256:new-fixture",
                  rememberedFingerprint:
                    mode === "identity" ? "SHA256:old-fixture" : "",
                },
              });
            }
            if (
              url.pathname === "/api/tasks" &&
              route.request().method() === "POST"
            )
              return route.fulfill({
                status: 502,
                json: { error: "请求失败 (502)" },
              });
            throw new Error(
              "Unexpected synthetic deployment request: " + url.pathname,
            );
          },
        );
        try {
          const form = page.locator(".tool-layout > div > .card form").first();
          await form
            .getByLabel("服务器 IP 地址", { exact: true })
            .fill("198.51.100.10");
          await form.getByLabel(/SSH 密码/).fill("synthetic-password");
          await form
            .getByRole("button", { name: "开始部署", exact: true })
            .click();
          const message =
            mode === "identity"
              ? "服务器身份已变化，请在高级设置中核对。"
              : "请求失败 (502)";
          await expect(form.getByText(message, { exact: true })).toBeVisible();
          await expect(form.locator(":scope > .notice.orange")).toHaveCount(3);
          const order = await form.evaluate((node) => {
            const error = [...node.children].find((c) =>
              c.matches(".notice.red"),
            );
            const warnings = [...node.children].filter((c) =>
              c.matches(".notice.orange"),
            );
            return {
              previous: error?.previousElementSibling?.tagName,
              next: error?.nextElementSibling === warnings[0],
              consecutive: warnings.every(
                (w, i) => i === 0 || warnings[i - 1].nextElementSibling === w,
              ),
            };
          });
          assert.deepEqual(order, {
            previous: "DETAILS",
            next: true,
            consecutive: true,
          });
          assert.deepEqual(
            writes,
            mode === "deploy-502"
              ? ["/api/fingerprints", "/api/tasks"]
              : ["/api/fingerprints"],
          );
          for (const width of [1440, 375]) {
            await page.setViewportSize({ width, height: 1100 });
            await expect(
              form.getByText(message, { exact: true }),
            ).toBeVisible();
            assert.equal(
              await page.evaluate(
                () => document.documentElement.scrollWidth > innerWidth,
              ),
              false,
            );
            await page.screenshot({
              path: `${screenshots}/deploy-${mode}-${width}.png`,
              fullPage: true,
            });
          }
          assert.deepEqual(errors, []);
          assert.deepEqual(external, []);
        } finally {
          await page.close();
        }
      }
    } finally {
      await browser.close();
    }
  },
);

test(
  "admin task audit searches and filters remotely with loading, failure, empty and stale response protection",
  { timeout: 60000 },
  async () => {
    const browser = await launch();
    const requests = [],
      pending = [];
    let mode = "ready";
    const tasks = [
      {
        id: "fixture-deploy",
        userId: "u-alice",
        userName: "Alice@example.test",
        kind: "deploy",
        host: "198.51.100.10",
        state: "succeeded",
        createdAt: 1700000000000,
      },
      {
        id: "fixture-front",
        userId: "u-bob",
        userName: "Bob@example.test",
        kind: "front",
        host: "203.0.113.20",
        state: "failed",
        createdAt: 1700000000100,
      },
      {
        id: "fixture-deleted",
        userId: "u-deleted",
        userName: "已删除用户",
        kind: "cleanup",
        host: "203.0.113.30",
        state: "succeeded",
        createdAt: 1700000000200,
      },
    ];
    const { page, errors, external } = await mount(
      browser,
      '<TasksPage user={{id:"fixture-admin",role:"admin"}} settings={{}}/>',
      async (route, url) => {
        assert.equal(url.pathname, "/api/admin/tasks");
        assert.equal(route.request().method(), "GET");
        requests.push({
          q: url.searchParams.get("q") || "",
          kind: url.searchParams.get("kind") || "",
        });
        if (mode === "pending" || mode === "stale") {
          const captured = mode;
          await new Promise((resolve) => pending.push(resolve));
          return route.fulfill({
            json: { tasks: captured === "stale" ? tasks : [] },
          });
        }
        if (mode === "failed")
          return route.fulfill({
            status: 503,
            json: { error: "任务读取失败，请重试" },
          });
        const q = (url.searchParams.get("q") || "").toLowerCase();
        const kind = url.searchParams.get("kind");
        return route.fulfill({
          json: {
            tasks:
              mode === "empty"
                ? []
                : tasks.filter(
                    (t) =>
                      (!kind || t.kind === kind) &&
                      (!q ||
                        [t.userName, t.userId, t.id, t.host].some((value) =>
                          value.toLowerCase().includes(q),
                        )),
                  ),
          },
        });
      },
    );
    try {
      const search = page.getByRole("textbox", {
        name: "搜索任务",
        exact: true,
      });
      const kind = page.getByRole("combobox", {
        name: "任务类型",
        exact: true,
      });
      await expect(
        page.getByRole("columnheader", { name: "操作用户名", exact: true }),
      ).toBeVisible();
      await expect(
        page.getByText("Alice@example.test", { exact: true }),
      ).toBeVisible();
      await expect(page.getByText("已删除用户", { exact: true })).toBeVisible();
      assert.deepEqual(
        await kind
          .locator("option")
          .evaluateAll((nodes) => nodes.map((n) => n.value)),
        [
          "",
          "deploy",
          "relay",
          "dd",
          "fingerprint",
          "front",
          "cleanup-preview",
          "cleanup",
        ],
      );
      for (const width of [1440, 375]) {
        await page.setViewportSize({ width, height: 1100 });
        assert.equal(
          await page.evaluate(
            () => document.documentElement.scrollWidth > innerWidth,
          ),
          false,
        );
        await page.screenshot({
          path: `${screenshots}/admin-audit-${width}.png`,
          fullPage: true,
        });
      }
      await search.fill("  Alice@EXAMPLE.test  ");
      await search.press("Enter");
      await expect
        .poll(() => requests.at(-1))
        .toEqual({ q: "Alice@EXAMPLE.test", kind: "" });
      await expect(page.locator("tbody tr")).toHaveCount(1);
      await expect(
        page.getByText("Bob@example.test", { exact: true }),
      ).toHaveCount(0);
      await kind.selectOption("front");
      await expect
        .poll(() => requests.at(-1))
        .toEqual({ q: "Alice@EXAMPLE.test", kind: "front" });
      await expect(
        page.getByText("没有符合条件的任务。请调整搜索或任务类型。", {
          exact: true,
        }),
      ).toBeVisible();
      await expect(
        page.getByText("暂无任务记录。", { exact: true }),
      ).toHaveCount(0);
      await search.fill("203.0.113.20");
      await search.press("Enter");
      await expect(page.locator("tbody tr")).toHaveCount(1);
      await expect(
        page.getByText("Bob@example.test", { exact: true }),
      ).toBeVisible();
      await expect(page.locator("tbody tr").first()).toContainText("前置机");
      await expect(page.locator("tbody tr").first()).toContainText(
        "不生成配置",
      );
      mode = "pending";
      await page.getByRole("button", { name: "刷新", exact: true }).click();
      await expect(page.getByText("正在读取…", { exact: true })).toBeVisible();
      await expect(
        page.getByText("Bob@example.test", { exact: true }),
      ).toHaveCount(0);
      await expect(
        page.getByText("没有符合条件的任务。请调整搜索或任务类型。", {
          exact: true,
        }),
      ).toHaveCount(0);
      pending.shift()();
      await expect(
        page.getByText("没有符合条件的任务。请调整搜索或任务类型。", {
          exact: true,
        }),
      ).toBeVisible();
      mode = "failed";
      await page.getByRole("button", { name: "刷新", exact: true }).click();
      await expect(
        page.getByText("任务读取失败，请重试", { exact: true }),
      ).toBeVisible();
      await expect(
        page.getByText("没有符合条件的任务。请调整搜索或任务类型。", {
          exact: true,
        }),
      ).toHaveCount(0);
      await expect(page.getByText("暂无记录", { exact: true })).toHaveCount(0);
      mode = "ready";
      await page.getByRole("button", { name: "清空筛选", exact: true }).click();
      await expect(search).toHaveValue("");
      await expect(kind).toHaveValue("");
      await expect(page.locator("tbody tr")).toHaveCount(3);
      mode = "stale";
      await search.fill("old-query");
      await search.press("Enter");
      await expect.poll(() => pending.length).toBe(1);
      mode = "ready";
      await search.fill("missing-query");
      await search.press("Enter");
      await expect(
        page.getByText("没有符合条件的任务。请调整搜索或任务类型。", {
          exact: true,
        }),
      ).toBeVisible();
      const staleResponse = page.waitForResponse((response) =>
        response.url().includes("q=old-query"),
      );
      pending.shift()();
      await staleResponse;
      await page.evaluate(
        () =>
          new Promise((resolve) =>
            requestAnimationFrame(() => requestAnimationFrame(resolve)),
          ),
      );
      await expect(
        page.getByText("Alice@example.test", { exact: true }),
      ).toHaveCount(0);
      mode = "empty";
      await page.getByRole("button", { name: "清空筛选", exact: true }).click();
      await expect(
        page.getByText("暂无任务记录。", { exact: true }),
      ).toBeVisible();
      assert.deepEqual(errors, []);
      assert.deepEqual(external, []);
    } finally {
      pending.splice(0).forEach((resolve) => resolve());
      await browser.close();
    }
  },
);

test(
  "ordinary task records expose no admin audit controls or account column",
  { timeout: 30000 },
  async () => {
    const browser = await launch();
    const requests = [];
    const { page, errors, external } = await mount(
      browser,
      '<TasksPage user={{id:"fixture-user",role:"user"}} settings={{}}/>',
      (route, url) => {
        requests.push(url.pathname + url.search);
        assert.equal(url.pathname, "/api/tasks");
        return route.fulfill({
          json: {
            tasks: [
              {
                id: "own-task",
                kind: "deploy",
                host: "198.51.100.50",
                state: "succeeded",
                createdAt: 1700000000000,
                userName: "should-not-be-displayed@example.test",
              },
            ],
          },
        });
      },
      "await saveLocal('fixture-user','own-task',{profiles:[]},'fixture.json')",
    );
    try {
      await expect(
        page.getByRole("heading", { name: "任务记录", exact: true }),
      ).toBeVisible();
      await expect(page.locator("tbody tr")).toHaveCount(1);
      await expect(
        page.getByText("已存当前浏览器", { exact: true }),
      ).toBeVisible();
      await expect(
        page.getByRole("columnheader", { name: "操作用户名", exact: true }),
      ).toHaveCount(0);
      await expect(
        page.getByRole("textbox", { name: "搜索任务", exact: true }),
      ).toHaveCount(0);
      await expect(
        page.getByRole("combobox", { name: "任务类型", exact: true }),
      ).toHaveCount(0);
      await expect(
        page.getByText("should-not-be-displayed@example.test", { exact: true }),
      ).toHaveCount(0);
      assert.deepEqual(requests, ["/api/tasks"]);
      assert.deepEqual(errors, []);
      assert.deepEqual(external, []);
    } finally {
      await browser.close();
    }
  },
);
