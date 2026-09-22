import { test } from "node:test";
import assert from "node:assert/strict";
import { chromium, expect } from "@playwright/test";
import { build } from "esbuild";
import { fileURLToPath } from "node:url";

const base = "http://127.0.0.1:19877";

test(
  "route management exposes entitlement level and explicit customer-rule purge",
  { timeout: 30000 },
  async () => {
    const compiled = await build({
      stdin: {
        contents: `
          import React from 'react';
          import {createRoot} from 'react-dom/client';
          import './app.css';
          import {RouteBuilder} from './tunnels';
          createRoot(document.getElementById('fixture')).render(React.createElement(RouteBuilder));
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
    const page = await browser.newPage({
      viewport: { width: 1440, height: 1000 },
    });
    const writes = [],
      errors = [];
    page.on("pageerror", (error) => errors.push(error.message));
    const routeRecord = {
      id: "route-jp",
      name: "日本线路",
      order: 1,
      level: 2,
      type: "port_forward",
      trafficMode: "both",
      trafficMultiplierPermille: 1000,
      entryAgentId: "agent-entry",
      entryAddresses: [],
      addressPreference: "auto",
      hops: [],
      exit: { agentIds: [] },
      requireFront: false,
      enabled: true,
      online: true,
      rateMbps: 10,
    };
    const secondRoute = {
      ...routeRecord,
      id: "route-us",
      name: "美国线路",
      order: 2,
      level: 3,
    };
    await page.route("**/*", async (requestRoute) => {
      const request = requestRoute.request(),
        url = new URL(request.url());
      if (url.origin !== base) return requestRoute.abort();
      if (url.pathname === "/")
        return requestRoute.fulfill({
          contentType: "text/html",
          body: '<div id="fixture"></div>',
        });
      if (url.pathname === "/api/admin/routes" && request.method() === "GET")
        return requestRoute.fulfill({
          json: { routes: [routeRecord, secondRoute] },
        });
      if (
        url.pathname === "/api/admin/relay-agents" &&
        request.method() === "GET"
      )
        return requestRoute.fulfill({
          json: {
            agents: [
              {
                id: "agent-entry",
                name: "入口节点",
                address: "198.51.100.10",
                online: true,
              },
            ],
          },
        });
      if (request.method() !== "GET") {
        writes.push({
          path: url.pathname,
          method: request.method(),
          body: request.postDataJSON(),
        });
        return requestRoute.fulfill({ json: { ok: true } });
      }
      return requestRoute.fulfill({
        status: 404,
        json: { error: "unexpected" },
      });
    });
    try {
      await page.goto(base);
      await page.addStyleTag({ content: style });
      await page.addScriptTag({ content: script });
      const row = page.getByRole("row").filter({ hasText: "日本线路" });
      await expect(row).toContainText("L2");
      const secondRow = page.getByRole("row").filter({ hasText: "美国线路" });
      await expect(
        row.getByRole("button", { name: "上移 日本线路" }),
      ).toBeDisabled();
      await expect(
        row.getByRole("button", { name: "下移 日本线路" }),
      ).toBeEnabled();
      await expect(
        secondRow.getByRole("button", { name: "上移 美国线路" }),
      ).toBeEnabled();
      await expect(
        secondRow.getByRole("button", { name: "下移 美国线路" }),
      ).toBeDisabled();
      page.once("dialog", (dialog) => {
        assert.match(dialog.message(), /全部客户中转规则/);
        assert.match(dialog.message(), /不可撤销/);
        void dialog.accept();
      });
      await row
        .getByRole("button", { name: "删除全部客户规则", exact: true })
        .click();
      await expect
        .poll(
          () =>
            writes.filter((item) => item.path.endsWith("purge-rules")).length,
        )
        .toBe(1);
      assert.deepEqual(writes.slice(0, 1), [
        {
          path: "/api/admin/routes/route-jp/purge-rules",
          method: "POST",
          body: { confirm: true },
        },
      ]);
      await row.getByRole("button", { name: "下移 日本线路" }).click();
      await expect
        .poll(() => writes.filter((item) => item.path.endsWith("/move")).length)
        .toBe(1);
      assert.deepEqual(
        writes.find((item) => item.path.endsWith("/move")),
        {
          path: "/api/admin/routes/route-jp/move",
          method: "POST",
          body: { direction: "down" },
        },
      );
      await row.getByRole("button", { name: "编辑", exact: true }).click();
      const dialog = page.getByRole("dialog");
      await expect(dialog.getByLabel("权益等级")).toHaveValue("2");
      await dialog.getByLabel("权益等级").selectOption("3");
      await dialog
        .getByRole("button", { name: "保存隧道", exact: true })
        .click();
      await expect
        .poll(() => writes.filter((item) => item.method === "PUT").length)
        .toBe(1);
      const update = writes.find((item) => item.method === "PUT");
      assert.equal(update.path, "/api/admin/routes/route-jp");
      assert.equal(update.body.level, 3);
      assert.deepEqual(errors, []);
    } finally {
      await browser.close();
    }
  },
);
