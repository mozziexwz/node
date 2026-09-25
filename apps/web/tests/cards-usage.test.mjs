import { test } from "node:test";
import assert from "node:assert/strict";
import { chromium, expect } from "@playwright/test";
import { build } from "esbuild";
import { fileURLToPath } from "node:url";

test(
  "maple codes expose shared-use limits and per-user history",
  { timeout: 30000 },
  async () => {
    const compiled = await build({
      stdin: {
        contents: `
        import React from 'react';
        import {createRoot} from 'react-dom/client';
        import './app.css';
        import {CodesPage} from './admin';
        createRoot(document.getElementById('fixture')).render(React.createElement(CodesPage));
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
    const script = compiled.outputFiles.find((file) =>
      file.path.endsWith(".js"),
    ).text;
    const style = compiled.outputFiles.find((file) =>
      file.path.endsWith(".css"),
    ).text;
    const browser = await chromium.launch({
      headless: true,
      ...(process.platform === "win32" ? { channel: "chrome" } : {}),
    });
    const page = await browser.newPage();
    const writes = [],
      errors = [];
    page.on("pageerror", (error) => errors.push(error.message));
    await page.route("**/*", async (route) => {
      const request = route.request();
      const url = new URL(request.url());
      if (url.origin !== "http://127.0.0.1:19879") return route.abort();
      if (url.pathname === "/")
        return route.fulfill({
          contentType: "text/html",
          body: '<div id="fixture"></div>',
        });
      if (url.pathname === "/api/admin/cards" && request.method() === "GET")
        return route.fulfill({
          json: {
            cards: [
              {
                id: "card-1",
                code: "MAPLE-ONE",
                amountCents: 1000,
                maxUses: 3,
                usedCount: 1,
                status: "active",
                batch: "shared",
                uses: [
                  {
                    userId: "u1",
                    userEmail: "one@example.test",
                    usedAt: 1770000000000,
                  },
                ],
              },
            ],
          },
        });
      if (request.method() === "POST") {
        writes.push({ path: url.pathname, body: request.postDataJSON() });
        return route.fulfill({ json: { cards: [] } });
      }
      return route.fulfill({
        status: 404,
        json: { error: "unexpected request" },
      });
    });
    try {
      await page.goto("http://127.0.0.1:19879");
      await page.addStyleTag({ content: style });
      await page.addScriptTag({ content: script });
      await expect(
        page.getByRole("row").filter({ hasText: "MAPLE-ONE" }),
      ).toContainText("1 / 3");
      await expect(
        page.getByRole("row").filter({ hasText: "MAPLE-ONE" }),
      ).toContainText("可使用 / 启用");
      await page
        .getByRole("row")
        .filter({ hasText: "MAPLE-ONE" })
        .getByRole("button", { name: "次数与记录" })
        .click();
      const detail = page.getByRole("dialog");
      await expect(detail).toContainText("同一用户不能重复使用同一码");
      await expect(detail).toContainText("one@example.test");
      await expect(detail.getByRole("button", { name: "保存" })).toHaveCount(0);
      await detail.getByRole("button", { name: "关闭窗口" }).click();
      await page.getByRole("button", { name: "批量生成" }).click();
      const creation = page.getByRole("dialog");
      await expect(creation).toContainText("同一用户不能重复使用同一个兑换码");
      await creation
        .getByRole("spinbutton", { name: "每个兑换码允许使用次数（0 不限）" })
        .fill("3");
      await creation.getByRole("button", { name: "生成" }).click();
      await expect.poll(() => writes.length).toBe(1);
      assert.equal(writes[0].path, "/api/admin/cards");
      assert.equal(writes[0].body.maxUses, 3);
      assert.equal(writes[0].body.amountCents, 1000);
      assert.deepEqual(errors, []);
    } finally {
      await browser.close();
    }
  },
);
