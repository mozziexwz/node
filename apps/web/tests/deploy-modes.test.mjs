import { test } from "node:test";
import assert from "node:assert/strict";
import { fileURLToPath } from "node:url";
import { chromium, expect } from "@playwright/test";
import { build } from "esbuild";

// The deploy page's three modes: a fresh deploy, an upgrade that keeps the
// node's configuration, and fetching the configuration back.
const base = "http://127.0.0.1:19989";
const fingerprint = "SHA256:" + "A".repeat(43);

async function mount(browser, imports, component, handle) {
  const compiled = await build({
    stdin: {
      contents: `import React from 'react';import {createRoot} from 'react-dom/client';${imports};import './app.css';createRoot(document.getElementById('app')).render(<main className="content">${component}</main>);`,
      resolveDir: fileURLToPath(new URL("../src/", import.meta.url)),
      loader: "jsx",
    },
    bundle: true,
    write: false,
    outdir: "in-memory",
    format: "iife",
    platform: "browser",
  });
  const page = await browser.newPage({
    viewport: { width: 1440, height: 1100 },
  });
  page.setDefaultTimeout(7000);
  const errors = [];
  page.on("pageerror", (e) => errors.push(e.message));
  await page.route("**/*", (route) => {
    const url = new URL(route.request().url());
    if (url.origin !== base) {
      errors.push("external request " + url.origin);
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
  return { page, errors };
}
const launch = () =>
  chromium.launch({
    headless: true,
    ...(process.platform === "win32" ? { channel: "chrome" } : {}),
  });

test(
  "upgrade and fetch reuse the SSH form and send only SSH",
  { timeout: 60000 },
  async () => {
    const browser = await launch();
    try {
      const posts = [];
      const { page, errors } = await mount(
        browser,
        "import {ToolPage} from './tools'",
        '<ToolPage kind="deploy" user={{id:"member",role:"user"}} settings={{nodeUpgrade:{releasedAt:"2026-10-24",notes:"- 节点组件更新到新版"}}} onNavigate={()=>{}}/>',
        (route, url) => {
          const method = route.request().method();
          if (url.pathname === "/api/tasks" && method === "GET")
            return route.fulfill({
              json: {
                tasks: [],
                limits: {
                  deploy: { minutes: 15, count: 5, remaining: 4, nextAt: 0 },
                },
              },
            });
          if (url.pathname === "/api/public/launcher")
            return route.fulfill({ json: { launcher: {}, localApp: {} } });
          if (url.pathname === "/api/fingerprints")
            return route.fulfill({
              json: { fingerprint, rememberedFingerprint: "" },
            });
          if (url.pathname === "/api/tasks" && method === "POST") {
            const body = route.request().postDataJSON();
            posts.push(body);
            return route.fulfill({
              status: 202,
              json: {
                id: "task-" + posts.length,
                kind: body.kind,
                host: body.ssh.host,
                state: "queued",
                phase: "queued",
              },
            });
          }
          if (url.pathname.startsWith("/api/tasks/"))
            return route.fulfill({
              json: {
                id: url.pathname.split("/").pop(),
                state: "queued",
                phase: "queued",
              },
            });
          throw new Error("unexpected request " + method + " " + url.pathname);
        },
      );
      const tab = (name) =>
        page.getByRole("tab", { name: new RegExp("^" + name) });
      await expect(tab("全新部署")).toHaveAttribute("aria-selected", "true");
      await expect(page.getByText("节点更新内容")).toHaveCount(0);
      await expect(page.getByText("卸载 MSBOOST")).toBeVisible();

      await tab("升级节点").click();
      await expect(
        page.getByRole("heading", { name: "节点更新内容" }),
      ).toBeVisible();
      await expect(page.getByText("更新于 2026-10-24")).toBeVisible();
      await expect(page.getByText("节点组件更新到新版")).toBeVisible();
      await expect(
        page.getByText("与全新部署共用次数 · 失败操作也计次"),
      ).toBeVisible();
      await expect(page.getByText("卸载 MSBOOST")).toHaveCount(0);
      const form = page.locator(".tool-layout > div > .card form").first();
      await form
        .getByLabel("服务器 IP 地址", { exact: true })
        .fill("198.51.100.10");
      await form.getByLabel(/SSH 密码/).fill("synthetic-password");
      await form.getByRole("button", { name: "开始升级" }).click();
      await expect(
        page.getByRole("heading", { name: "任务详情" }),
      ).toBeVisible();
      await page.getByRole("button", { name: "关闭窗口" }).click();

      await tab("取回配置文件").click();
      await expect(page.getByText("节点更新内容")).toHaveCount(0);
      await expect(
        form.getByLabel("服务器 IP 地址", { exact: true }),
      ).toHaveValue("198.51.100.10");
      await expect(form.locator(":scope > .notice.orange")).toHaveCount(2);
      await form.getByLabel(/SSH 密码/).fill("synthetic-password");
      await form.getByRole("button", { name: "取回配置" }).click();
      await expect(
        page.getByRole("heading", { name: "任务详情" }),
      ).toBeVisible();

      assert.deepEqual(
        posts.map((p) => [
          p.kind,
          p.ssh.host,
          "mode" in p,
          Object.keys(p).sort().join(","),
        ]),
        [
          ["upgrade", "198.51.100.10", false, "kind,ssh"],
          ["fetch-config", "198.51.100.10", false, "kind,ssh"],
        ],
      );
      assert.deepEqual(errors, []);
    } finally {
      await browser.close();
    }
  },
);

test(
  "a fetched configuration shows only the checks that ran",
  { timeout: 60000 },
  async () => {
    const browser = await launch();
    try {
      const { page, errors } = await mount(
        browser,
        "import {TaskDetail} from './tools'",
        '<TaskDetail owner="member" onClose={()=>{}} task={{id:"fetched",userId:"member",kind:"fetch-config",host:"203.0.113.10",state:"succeeded",phase:"complete",message:"已从节点取回配置，服务器没有改动",health:{service:"stopped",localSelfTest:"not_tested",publicTCP:"reachable",bbr:"unknown"}}}/>',
        (route) => route.fulfill({ json: {} }),
      );
      await expect(
        page.getByText("已从节点取回配置，服务器没有改动"),
      ).toBeVisible();
      const rows = await page
        .locator("dialog tbody tr")
        .evaluateAll((trs) =>
          trs.map((tr) => tr.innerText.replace(/\s+/g, " ").trim()),
        );
      assert.deepEqual(rows, ["服务运行 未运行", "公网 TCP 可达性 可达"]);
      assert.deepEqual(errors, []);
    } finally {
      await browser.close();
    }
  },
);

test(
  "a node that serves UDP shows its UDP check and the launcher it needs",
  { timeout: 60000 },
  async () => {
    const browser = await launch();
    try {
      const config = {
        profiles: [
          {
            profileName: "msboost",
            user: { name: "node-user", password: "node-password-1234" },
            servers: [
              {
                ipAddress: "203.0.113.10",
                domainName: "",
                portBindings: [
                  { port: 23456, protocol: "TCP" },
                  { port: 23456, protocol: "UDP" },
                ],
              },
            ],
            mtu: 1400,
          },
        ],
        activeProfile: "msboost",
      };
      const { page, errors } = await mount(
        browser,
        "import {TaskDetail} from './tools'",
        '<TaskDetail owner="member" onClose={()=>{}} task={{id:"deployed",userId:"member",kind:"deploy",host:"203.0.113.10",state:"succeeded",phase:"complete",configAvailable:true,message:"部署完成",health:{service:"running",localSelfTest:"passed",publicTCP:"reachable",publicUDP:"unreachable",bbr:"enabled"}}}/>',
        (route) =>
          route.fulfill({
            json: route.request().url().endsWith("/api/tasks/deployed/config")
              ? config
              : {},
          }),
      );
      await expect(
        page.getByText("这份配置同时支持 TCP 和 UDP", { exact: false }),
      ).toBeVisible();
      const rows = await page
        .locator("dialog tbody tr")
        .evaluateAll((trs) =>
          trs.map((tr) => tr.innerText.replace(/\s+/g, " ").trim()),
        );
      assert.deepEqual(rows, [
        "BBR 网络优化 已启用",
        "服务运行 运行中",
        "本地自测 通过",
        "公网 TCP 可达性 可达",
        "公网 UDP 可达性 不可达",
      ]);
      assert.deepEqual(errors, []);
    } finally {
      await browser.close();
    }
  },
);

test("administrators write the upgrade notes", { timeout: 60000 }, async () => {
  const browser = await launch();
  try {
    const saves = [];
    const { page, errors } = await mount(
      browser,
      "import {NodeUpgradeSettings} from './node-upgrade'",
      "<NodeUpgradeSettings onSaved={()=>{}}/>",
      (route, url) => {
        if (route.request().method() === "PUT") {
          saves.push(route.request().postDataJSON());
          return route.fulfill({ json: { ok: true } });
        }
        if (url.pathname === "/api/admin/settings")
          return route.fulfill({ json: { nodeUpgrade: {} } });
        throw new Error("unexpected request " + url.pathname);
      },
    );
    await expect(page.getByText("还没有保存过")).toBeVisible();
    await page.getByLabel("更新日期", { exact: true }).fill("2026-10-24");
    await page.getByLabel(/^更新内容/).fill("- 节点组件更新到新版");
    await page.getByRole("button", { name: "保存", exact: true }).click();
    await expect(page.getByText("升级节点设置已保存")).toBeVisible();
    assert.deepEqual(saves, [
      {
        nodeUpgrade: {
          releasedAt: "2026-10-24",
          notes: "- 节点组件更新到新版",
        },
      },
    ]);
    assert.deepEqual(errors, []);
  } finally {
    await browser.close();
  }
});
