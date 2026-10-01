import { test } from "node:test";
import assert from "node:assert/strict";
import { chromium, expect } from "@playwright/test";
import { build } from "esbuild";
import { fileURLToPath } from "node:url";

test("SMTP alias saves after a test send and preserves feedback through the protected refresh", { timeout: 30000 }, async () => {
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
  let reads = 0, releaseSend, releaseRefresh, failSend = false;
  const settings = {
    smtp: true,
    registrationEmailVerificationRequired: true,
    smtpConfig: { host: "mail.spacemail.com", port: 465, sender: "mailbox@example.com", name: "MSBOOST", encryption: "tls", credentialConfigured: true, testedAt: Date.now() },
  };
  // Every request is fulfilled here or aborted; no SMTP endpoint or external
  // network is contacted, including a failure/retry of the form.
  await page.route("**/*", async (route) => {
    const request = route.request(), url = new URL(request.url());
    if (url.origin !== base) return route.abort();
    if (url.pathname === "/") return route.fulfill({ contentType: "text/html", body: '<div id="fixture"></div>' });
    if (url.pathname === "/api/admin/settings" && request.method() === "GET") {
      reads++;
      if (reads > 1) await new Promise(resolve => { releaseRefresh = resolve; });
      return route.fulfill({ json: { settings } });
    }
    if (request.method() !== "GET") {
      writes.push({ path: url.pathname, method: request.method(), body: request.postDataJSON() });
      if (url.pathname === "/api/admin/smtp/test") {
        await new Promise(resolve => { releaseSend = resolve; });
        if (failSend) return route.fulfill({ status: 503, json: { error: "SMTP 测试失败，配置未保存" } });
        settings.smtpConfig = { ...settings.smtpConfig, ...request.postDataJSON().smtpConfig, credentialConfigured: true, testedAt: Date.now() };
        return route.fulfill({ json: { ok: true, testedAt: Date.now() } });
      }
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
    await expect.poll(() => typeof releaseSend).toBe("function");
    assert.equal(reads, 1, "a pending test send must not trigger a saved-settings refresh");
    await expect(page.getByText("设置已保存", { exact: true })).toHaveCount(0);
    assert.equal(writes.length, 1);
    assert.equal(writes[0].path, "/api/admin/smtp/test");
    assert.equal(writes[0].method, "POST");
    assert.equal(writes[0].body.smtpConfig.username, "mailbox@example.com");
    assert.equal(writes[0].body.smtpConfig.sender, "alias@example.com");
    assert.equal(writes[0].body.smtpConfig.secret, "");
    releaseSend();
    await expect.poll(() => typeof releaseRefresh).toBe("function");
    await expect(page.getByText("设置已保存", { exact: true })).toBeVisible();
    await expect(page.getByText("正在读取系统设置…", { exact: true })).toBeVisible();
    await expect(page.getByRole("button", { name: "发送测试邮件并保存" })).toHaveCount(0);
    await expect(page.getByLabel("发件邮箱 From")).toHaveCount(0);
    releaseRefresh();
    await expect(page.getByLabel("发件邮箱 From")).toHaveValue("alias@example.com");
    await expect(page.getByText("设置已保存", { exact: true })).toBeVisible();
    await page.getByRole("button", { name: "功能开关", exact: true }).click();
    await expect(page.getByText("设置已保存", { exact: true })).toHaveCount(0);
    await page.getByRole("button", { name: "SMTP 邮箱", exact: true }).click();
    // A failed retry cannot reuse the previous success or bypass test-send
    // acceptance with a separate settings PUT.
    failSend = true;
    releaseSend = undefined;
    await page.getByLabel("发件邮箱 From").fill("rejected-alias@example.com");
    await page.getByRole("button", { name: "发送测试邮件并保存" }).click();
    await expect.poll(() => typeof releaseSend).toBe("function");
    await expect(page.getByText("设置已保存", { exact: true })).toHaveCount(0);
    releaseSend();
    await expect(page.getByText("SMTP 测试失败，配置未保存", { exact: true })).toBeVisible();
    await expect(page.getByText("设置已保存", { exact: true })).toHaveCount(0);
    await expect(page.getByText("操作已完成", { exact: true })).toHaveCount(0);
    assert.equal(reads, 2, "a failed SMTP send must not reload or save settings");
    assert.equal(settings.smtpConfig.sender, "alias@example.com");
    assert.deepEqual(writes.map(write => [write.path, write.method]), [
      ["/api/admin/smtp/test", "POST"], ["/api/admin/smtp/test", "POST"],
    ]);
  } finally {
    releaseSend?.();
    releaseRefresh?.();
    await browser.close();
  }
});
