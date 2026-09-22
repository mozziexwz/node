import { test } from "node:test";
import assert from "node:assert/strict";
import { chromium, expect } from "@playwright/test";
import { build } from "esbuild";
import { fileURLToPath } from "node:url";

const base = "http://127.0.0.1:19878";

test(
  "entitlement policy controls and member eligibility explanations stay explicit",
  { timeout: 30000 },
  async () => {
    const compiled = await build({
      stdin: {
        contents: `
          import React from 'react';
          import {createRoot} from 'react-dom/client';
          import './app.css';
          import {ResourcePage} from './admin';
          import {Plans} from './business';
          createRoot(document.getElementById('fixture')).render(
            React.createElement(React.Fragment, null,
              React.createElement('section', {className: 'admin-fixture'}, React.createElement(ResourcePage, {kind: 'plans'})),
              React.createElement('section', {className: 'member-fixture'}, React.createElement(Plans, {
                user: {balanceCents: 500000, emailVerifiedAt: 1},
                onRefresh: () => {},
              })),
            ),
          );
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
      viewport: { width: 1440, height: 1200 },
    });
    const writes = [],
      errors = [];
    page.on("pageerror", (error) => errors.push(error.message));
    const basePlan = {
      id: "holders-plan",
      name: "老用户权益",
      level: 2,
      priceCents: 1000,
      days: 30,
      trafficBytes: 100000000000,
      rateMbps: 20,
      enabled: true,
      maxPurchasesPerUser: 0,
      currentHoldersOnly: true,
      trial: false,
    };
    const memberPlans = [
      {
        ...basePlan,
        eligible: false,
        reason: "此权益仅允许当前有效持有者续兑",
      },
      {
        ...basePlan,
        id: "trial-plan",
        name: "体验权益",
        currentHoldersOnly: false,
        trial: true,
        eligible: false,
        reason: "体验权益每位会员终身只能兑换一次",
      },
      {
        ...basePlan,
        id: "open-plan",
        name: "开放权益",
        currentHoldersOnly: false,
        eligible: true,
        reason: "",
      },
    ];
    await page.route("**/*", async (requestRoute) => {
      const request = requestRoute.request(),
        url = new URL(request.url());
      if (url.origin !== base) return requestRoute.abort();
      if (url.pathname === "/")
        return requestRoute.fulfill({
          contentType: "text/html",
          body: '<div id="fixture"></div>',
        });
      if (url.pathname === "/api/admin/plans" && request.method() === "GET")
        return requestRoute.fulfill({ json: { plans: [basePlan] } });
      if (url.pathname === "/api/plans" && request.method() === "GET")
        return requestRoute.fulfill({
          json: {
            plans: memberPlans,
            purchasedCounts: { "trial-plan": 1 },
            purchaseRequireVerifiedEmail: false,
          },
        });
      if (
        url.pathname === "/api/payment-channels" &&
        request.method() === "GET"
      )
        return requestRoute.fulfill({ json: { channels: [] } });
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

      const member = page.locator(".member-fixture");
      const holderCard = member
        .locator(".card")
        .filter({ hasText: "老用户权益" });
      await expect(holderCard).toContainText("仅限当前持有人续兑");
      await expect(holderCard).toContainText("此权益仅允许当前有效持有者续兑");
      await expect(
        holderCard.getByRole("button", { name: "暂不可兑换" }),
      ).toBeDisabled();
      const trialCard = member.locator(".card").filter({ hasText: "体验权益" });
      await expect(trialCard).toContainText("每位用户终身仅可兑换一次");
      await expect(trialCard).toContainText("体验权益每位会员终身只能兑换一次");
      await expect(
        trialCard.getByRole("button", { name: "暂不可兑换" }),
      ).toBeDisabled();
      await expect(
        member
          .locator(".card")
          .filter({ hasText: "开放权益" })
          .getByRole("button", { name: "选择权益" }),
      ).toBeEnabled();

      const admin = page.locator(".admin-fixture");
      await admin
        .getByRole("row")
        .filter({ hasText: "老用户权益" })
        .getByRole("button", { name: "编辑" })
        .click();
      const dialog = page.getByRole("dialog");
      const holders = dialog.getByRole("checkbox", {
        name: "仅允许当前该权益有效用户兑换",
      });
      const trial = dialog.getByRole("checkbox", {
        name: "体验权益（每位用户仅可兑换一次）",
      });
      await expect(holders).toBeChecked();
      await expect(trial).not.toBeChecked();
      await trial.check();
      await expect(holders).not.toBeChecked();
      await dialog.getByRole("button", { name: "保存" }).click();
      await expect.poll(() => writes.length).toBe(1);
      assert.equal(writes[0].path, "/api/admin/plans/holders-plan");
      assert.equal(writes[0].method, "PUT");
      assert.equal(writes[0].body.currentHoldersOnly, false);
      assert.equal(writes[0].body.trial, true);
      assert.deepEqual(errors, []);
    } finally {
      await browser.close();
    }
  },
);
