import { test } from "node:test";
import assert from "node:assert/strict";
import { chromium, expect } from "@playwright/test";
import { build } from "esbuild";
import { fileURLToPath } from "node:url";

test("SMTP alias uses mailbox login and saves only after a test send", { timeout: 30000 }, async () => {
  const compiled = await build({
    stdin: {
      contents: `
        import React from 'react';
        import {createRoot} from 'react-dom/client';
        import './app.css';
        import {Settings} from './admin';
        createRoot(document.getElementById('fixture')).render(React.createElement(Settings, {onRefresh: () => {}}));
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
  const base = "http://127.0.0.1:19880";
  const browser = await chromium.launch({ headless: true, ...(process.platform === "win32" ? { channel: "chrome" } : {}) });
  const page = await browser.newPage();
  const writes = [];
  await page.route("**/*", async (route) => {
    const request = route.request(), url = new URL(request.url());
    if (url.origin !== base) return route.abort();
    if (url.pathname === "/") return route.fulfill({ contentType: "text/html", body: '<div id="fixture"></div>' });
    if (url.pathname === "/api/admin/settings" && request.method() === "GET") {
      return route.fulfill({ json: { settings: {
        smtp: true,
        registrationEmailVerificationRequired: true,
        smtpConfig: { host: "mail.spacemail.com", port: 465, sender: "mailbox@example.com", name: "MSBOOST", encryption: "tls", credentialConfigured: true, testedAt: Date.now() },
      } } });
    }
    if (request.method() !== "GET") {
      writes.push({ path: url.pathname, method: request.method(), body: request.postDataJSON() });
      if (url.pathname === "/api/admin/smtp/test") return route.fulfill({ json: { ok: true, testedAt: Date.now() } });
    }
    return route.fulfill({ status: 404, json: { error: "unexpected request" } });
  });
  try {
    await page.goto(base);
    await page.addStyleTag({ content: style });
    await page.addScriptTag({ content: script });
    await page.getByRole("button", { name: "SMTP 邮箱" }).click();
    await expect(page.getByLabel("SMTP 登录账号")).toHaveValue("mailbox@example.com");
    await page.getByLabel("发件邮箱 From").fill("alias@example.com");
    await page.getByRole("button", { name: "发送测试邮件并保存" }).click();
    await expect(page.getByText("操作已完成")).toBeVisible();
    assert.equal(writes.length, 1);
    assert.equal(writes[0].path, "/api/admin/smtp/test");
    assert.equal(writes[0].method, "POST");
    assert.equal(writes[0].body.smtpConfig.username, "mailbox@example.com");
    assert.equal(writes[0].body.smtpConfig.sender, "alias@example.com");
    assert.equal(writes[0].body.smtpConfig.secret, "");
  } finally {
    await browser.close();
  }
});
