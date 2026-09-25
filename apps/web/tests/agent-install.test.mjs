import { test } from "node:test";
import assert from "node:assert/strict";
import { chromium, expect } from "@playwright/test";
import { build } from "esbuild";
import { fileURLToPath } from "node:url";

const base = "http://127.0.0.1:19879";

test("node reinstall makes fresh identity reset and connection interruption explicit", { timeout: 30000 }, async () => {
  const compiled = await build({
    stdin: {
      contents: `
        import React from 'react';
        import {createRoot} from 'react-dom/client';
        import './app.css';
        import {ResourcePage} from './admin';
        createRoot(document.getElementById('fixture')).render(React.createElement(ResourcePage, {kind: 'agents'}));
      `,
      resolveDir: fileURLToPath(new URL("../src/", import.meta.url)),
      loader: "jsx",
    },
    bundle: true,
    write: false,
    outdir: "in-memory",
    format: "iife",
    platform: "browser",
  });
  const script = compiled.outputFiles.find((file) => file.path.endsWith(".js")).text;
  const style = compiled.outputFiles.find((file) => file.path.endsWith(".css")).text;
  const browser = await chromium.launch({ headless: true, ...(process.platform === "win32" ? { channel: "chrome" } : {}) });
  const page = await browser.newPage();
  const errors = [], writes = [];
  page.on("pageerror", (error) => errors.push(error.message));
  await page.route("**/*", async (route) => {
    const request = route.request(), url = new URL(request.url());
    if (url.origin !== base) return route.abort();
    if (url.pathname === "/") return route.fulfill({ contentType: "text/html", body: '<div id="fixture"></div>' });
    if (url.pathname === "/api/admin/relay-agents" && request.method() === "GET") {
      return route.fulfill({ json: { agents: [
        { id: "relay-1", name: "未注册空节点", address: "198.51.100.10", portRanges: [{ start: 20000, end: 59999 }], enabled: true, online: false, enrollmentAllowed: true },
        { id: "relay-2", name: "已有业务节点", address: "198.51.100.11", portRanges: [{ start: 20000, end: 59999 }], enabled: true, online: true, protocolVersion: 2, enrollmentAllowed: false, enrollmentBlockedReason: "节点已确认 v2，禁止普通部署/重装" },
      ] } });
    }
    if (url.pathname === "/api/admin/relay-agents/relay-1/enrollment" && request.method() === "POST") {
      writes.push(url.pathname);
      return route.fulfill({ json: { enrollmentToken: "synthetic-private-token", expiresAt: Date.now() + 60000 } });
    }
    return route.fulfill({ status: 404, json: { error: "unexpected" } });
  });
  try {
    await page.goto(base);
    await page.addStyleTag({ content: style });
    await page.addScriptTag({ content: script });
    const row = page.getByRole("row").filter({ hasText: "未注册空节点" });
    const blocked = page.getByRole("row").filter({ hasText: "已有业务节点" });
    await expect(blocked.getByRole("button", { name: "部署 / 重装" })).toHaveCount(0);
    await expect(blocked.getByRole("note")).toContainText("禁止普通部署/重装");
    await expect(blocked.getByRole("link", { name: "查看受信恢复指引" })).toHaveAttribute("href", /\/v0\.3\.3\/docs\/relay-recovery\.md$/);
    page.once("dialog", (dialog) => void dialog.accept());
    await row.getByRole("button", { name: "部署 / 重装" }).click();
    const dialog = page.getByRole("dialog");
    await expect(dialog).toContainText("旧 keep_last 状态会优先使用旧身份");
    await expect(dialog).toContainText("旧转发与现有连接将中断");
    await expect(dialog).toContainText("禁止普通令牌轮换");
    const commands = dialog.locator("pre.code-panel");
    await expect(commands).toHaveCount(3);
    const first = await commands.nth(0).textContent();
    const reset = await commands.nth(1).textContent();
    assert.match(first, /\/v0\.3\.3\/agent\.sh/);
    assert.match(first, /--capability relay .*--offline-policy keep_last/);
    assert.doesNotMatch(first, /--fresh-reset/);
    assert.match(reset, /\/v0\.3\.3\/agent\.sh/);
    assert.match(reset, /--fresh-reset --acknowledge-relay-restart$/);
    await expect(dialog.getByRole("button", { name: "复制首次安装命令" })).toBeVisible();
    await expect(dialog.getByRole("button", { name: "复制全新重装命令" })).toBeVisible();
    assert.deepEqual(writes, ["/api/admin/relay-agents/relay-1/enrollment"]);
    assert.deepEqual(errors, []);
  } finally {
    await browser.close();
  }
});
