import { test } from "node:test";
import assert from "node:assert/strict";
import { chromium, expect } from "@playwright/test";
import { build } from "esbuild";
import { fileURLToPath } from "node:url";

test(
  "payment whitelist admin editing, filtering and member payment choices",
  { timeout: 30000 },
  async () => {
    const compiled = await build({
      stdin: {
        contents: `import React from 'react'; import {createRoot} from 'react-dom/client'; import './app.css'; import {UsersPage} from './users'; import {Plans} from './business'; createRoot(document.getElementById('fixture')).render(React.createElement(React.Fragment,null,React.createElement(UsersPage),React.createElement(Plans,{user:{balanceCents:1000},onRefresh:()=>{}})));`,
        resolveDir: fileURLToPath(new URL("../src/", import.meta.url)),
        loader: "jsx",
      },
      bundle: true,
      write: false,
      outdir: "in-memory",
      format: "iife",
      platform: "browser",
    });
    const browser = await chromium.launch({
      headless: true,
      ...(process.platform === "win32" ? { channel: "chrome" } : {}),
    });
    const page = await browser.newPage({
      viewport: { width: 1440, height: 1100 },
    });
    page.setDefaultTimeout(7000);
    const base = "http://127.0.0.1:19892",
      writes = [],
      errors = [];
    let allowed = false;
    const user = {
      id: "member",
      email: "12345678@qq.com",
      role: "member",
      status: "active",
      balanceCents: 1000,
      rateMbps: 1,
      level: 1,
    };
    page.on("pageerror", (e) => errors.push(e.message));
    await page.route("**/*", async (route) => {
      const req = route.request(),
        url = new URL(req.url());
      if (url.origin !== base) return route.abort();
      if (url.pathname === "/")
        return route.fulfill({
          contentType: "text/html",
          body: '<div id="fixture"></div>',
        });
      if (url.pathname === "/api/admin/users")
        return route.fulfill({
          json: { users: [{ ...user, onlinePaymentAllowed: allowed }] },
        });
      if (
        url.pathname === "/api/admin/users/member" &&
        req.method() === "PATCH"
      ) {
        const patch = req.postDataJSON();
        writes.push(patch);
        if (Object.hasOwn(patch, "onlinePaymentAllowed"))
          allowed = patch.onlinePaymentAllowed;
        return route.fulfill({
          json: { user: { ...user, onlinePaymentAllowed: allowed } },
        });
      }
      if (url.pathname === "/api/plans")
        return route.fulfill({
          json: {
            plans: [
              {
                id: "plan",
                name: "测试权益",
                enabled: true,
                priceCents: 100,
                days: 1,
                trafficBytes: 1000000000,
                rateMbps: 1,
                level: 1,
              },
            ],
          },
        });
      if (url.pathname === "/api/payment-channels")
        return route.fulfill({
          json: {
            channels: allowed
              ? [
                  { id: "ali", name: "支付宝" },
                  { id: "wx", name: "微信" },
                ]
              : [],
          },
        });
      return route.fulfill({
        status: 404,
        json: { error: "unexpected fixture request" },
      });
    });
    try {
      await page.goto(base);
      await page.addStyleTag({
        content: compiled.outputFiles.find((f) => f.path.endsWith(".css")).text,
      });
      await page.addScriptTag({
        content: compiled.outputFiles.find((f) => f.path.endsWith(".js")).text,
      });
      await page.getByRole("button", { name: "选择权益", exact: true }).click();
      let dialog = page.getByRole("dialog");
      await expect(dialog.getByLabel("兑换方式").locator("option")).toHaveText([
        "枫叶",
      ]);
      await dialog.getByRole("button", { name: "关闭窗口" }).click();
      await page.getByRole("button", { name: "编辑", exact: true }).click();
      dialog = page.getByRole("dialog");
      await expect(
        dialog.getByLabel("允许在线支付（支付宝／微信）"),
      ).toHaveValue("false");
      await dialog
        .getByLabel("允许在线支付（支付宝／微信）")
        .selectOption("true");
      await dialog.getByRole("button", { name: "保存", exact: true }).click();
      await expect(page.getByRole("dialog")).toHaveCount(0);
      assert.equal(writes[0].onlinePaymentAllowed, true);
      for (const field of ["balanceCents", "expiresAt", "trafficUsed"])
        assert.equal(Object.hasOwn(writes[0], field), false);
      await page.getByLabel("在线支付筛选").selectOption("false");
      await expect(
        page.locator("tbody tr").filter({ hasText: user.email }),
      ).toHaveCount(0);
      await page.getByLabel("在线支付筛选").selectOption("true");
      await expect(
        page.locator("tbody tr").filter({ hasText: user.email }),
      ).toHaveCount(1);
      await page.getByRole("button", { name: "选择权益", exact: true }).click();
      dialog = page.getByRole("dialog");
      await expect(dialog.getByLabel("兑换方式").locator("option")).toHaveText([
        "枫叶",
        "支付宝",
        "微信",
      ]);
      await dialog.getByRole("button", { name: "关闭窗口" }).click();
      await page.getByRole("button", { name: "编辑", exact: true }).click();
      dialog = page.getByRole("dialog");
      await dialog
        .getByLabel("允许在线支付（支付宝／微信）")
        .selectOption("false");
      await dialog.getByRole("button", { name: "保存", exact: true }).click();
      await expect(page.getByRole("dialog")).toHaveCount(0);
      await page.getByRole("button", { name: "选择权益", exact: true }).click();
      await expect(
        page.getByRole("dialog").getByLabel("兑换方式").locator("option"),
      ).toHaveText(["枫叶"]);
      assert.equal(writes[1].onlinePaymentAllowed, false);
      assert.deepEqual(errors, []);
    } finally {
      await browser.close();
    }
  },
);
