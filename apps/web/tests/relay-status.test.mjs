import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import ts from "typescript";
import { chromium, expect } from "@playwright/test";
import { build } from "esbuild";
import { fileURLToPath } from "node:url";

const helperCode = ts.transpileModule(
  fs.readFileSync(new URL("../src/relay-status.ts", import.meta.url), "utf8"),
  {
    compilerOptions: {
      target: ts.ScriptTarget.ES2022,
      module: ts.ModuleKind.ESNext,
    },
  },
).outputText;
const {
  relayStatus,
  relayNeedsRecovery,
  relaySegmentStopText,
  relayAccountingVisible,
} = await import(
  "data:text/javascript;base64," + Buffer.from(helperCode).toString("base64")
);

test("relay status separates management loss from the last business report", () => {
  for (const runtimeStatus of [
    "last_reported_running",
    "last_reported_stopped",
    "partial_unknown",
    "unknown",
  ]) {
    const view = relayStatus({
      offlinePolicy: "keep_last",
      keepLastConfirmed: true,
      controlStatus: "offline",
      runtimeStatus,
      state: "active",
    });
    assert.equal(view.label, "业务状态待核实");
    assert.equal(view.controlLabel, "管理连接已中断");
    assert.doesNotMatch(view.label, /转发中|正常|已停止/);
  }
});
test("only complete keep_last ACK advertises confirmed offline retention; v2 never gets a 45 second deadline", () => {
  const confirmed = relayStatus({
    offlinePolicy: "keep_last",
    keepLastConfirmed: true,
  });
  assert.equal(confirmed.keepLastConfirmed, true);
  assert.match(confirmed.policyText, /已确认离线保留/);
  for (const value of [
    relayStatus({ offlinePolicy: "keep_last", keepLastConfirmed: false }),
    relayStatus({ offlinePolicy: "mixed", keepLastConfirmed: true }),
  ]) {
    assert.equal(value.keepLastConfirmed, false);
    assert.doesNotMatch(value.policyText, /已确认离线保留|45 秒/);
  }
  assert.doesNotMatch(confirmed.policyText, /45 秒|租约/);
  assert.match(relayStatus({ offlinePolicy: "lease" }).policyText, /v1.*45 秒/);
  assert.equal(
    relayStatus({ segments: [{ protocolVersion: 2 }] }).mode,
    "mixed",
  );
});
test("pending stop retains resources; recovery overrides optimistic confirmation; stale segment ACK is not a stop", () => {
  const rule = {
    offlinePolicy: "keep_last",
    keepLastConfirmed: true,
    state: "paused",
    stopStatus: "pending",
    reconcileState: "recovery_required",
  };
  assert.equal(relayNeedsRecovery(rule), true);
  const view = relayStatus(rule);
  assert.equal(view.label, "恢复核对中");
  assert.equal(view.keepLastConfirmed, false);
  assert.match(view.stopText, /待节点停止确认，端口及旧目标继续占用/);
  assert.match(view.recoveryText, /本机 root 受信恢复流程逐规则核对/);
  assert.match(view.recoveryText, /管理凭据失效不代表旧业务已停止/);
  const segment = {
    protocolVersion: 2,
    lastCommandAction: "pause",
    ackState: "stopped",
    stopConfirmed: true,
    configGeneration: 2,
    appliedGeneration: 2,
  };
  assert.equal(relaySegmentStopText(segment), "v2：明确停止已确认");
  assert.equal(
    relaySegmentStopText({ ...segment, appliedGeneration: 1 }),
    "v2：等待明确停止确认",
  );
  assert.equal(
    relaySegmentStopText({ ...segment, stopConfirmed: false }),
    "v2：等待明确停止确认",
  );
  assert.equal(relaySegmentStopText({ protocolVersion: 1 }), "v1：短租约截止");
});
test("accounting overview stays hidden only when there are no periods and no anomaly", () => {
  assert.equal(relayAccountingVisible(null), false);
  assert.equal(
    relayAccountingVisible({
      periods: [],
      reviewBytes: 0,
      reviewSamples: 0,
      degradedAgents: 0,
    }),
    false,
  );
  for (const data of [
    { periods: [{}] },
    { reviewBytes: 1 },
    { reviewSamples: 1 },
    { degradedAgents: 1 },
  ])
    assert.equal(relayAccountingVisible(data), true);
});

const base = "http://127.0.0.1:19875";
const sampleTime = Date.UTC(2026, 8, 15, 1, 2, 3);
const rules = [
  {
    id: "keep",
    routeName: "完整保留线路",
    offlinePolicy: "keep_last",
    keepLastConfirmed: true,
    state: "active",
  },
  {
    id: "mixed",
    routeName: "混合线路",
    offlinePolicy: "mixed",
    keepLastConfirmed: true,
    state: "active",
  },
  {
    id: "lease",
    routeName: "短租约线路",
    offlinePolicy: "lease",
    state: "active",
  },
  {
    id: "recovery",
    routeName: "恢复核对线路",
    offlinePolicy: "keep_last",
    keepLastConfirmed: false,
    state: "paused",
    syncState: "recovery_required",
    reconcileState: "recovery_required",
    stopStatus: "pending",
    accountingDegraded: true,
  },
].map((rule) => ({
  routeId: rule.id,
  userId: "synthetic-user",
  userEmail: "10000001@qq.com",
  targetHost: "198.51.100.10",
  targetPort: 12345,
  entryAddress: "198.51.100.11",
  entryPort: 12346,
  effectiveRateMbps: 5,
  readySegments: 0,
  totalSegments: 2,
  trafficBytes: 1234,
  inputBytes: 450e6,
  outputBytes: 1.25e9,
  controlStatus: "offline",
  runtimeStatus: "last_reported_running",
  runtimeObservedAt: sampleTime,
  version: 2,
  segments:
    rule.offlinePolicy === "lease"
      ? [
          {
            agentId: "v1-agent",
            protocolVersion: 1,
            runtime: { listenPort: 12346, rateMbps: 5 },
            ackState: "ready",
            lastLease: sampleTime + 45000,
          },
        ]
      : [
          {
            agentId: "pending-agent",
            runtime: { listenPort: 12346, rateMbps: 5 },
            protocolVersion: 2,
            lastCommandAction: "pause",
            configGeneration: 2,
            appliedGeneration: 1,
            stopConfirmed: false,
            ackState: "pending",
            ackAt: sampleTime,
            lastLease: Date.UTC(1999, 0, 1),
          },
          {
            agentId: "stopped-agent",
            runtime: { listenPort: 12347, rateMbps: 5 },
            protocolVersion: 2,
            lastCommandAction: "pause",
            configGeneration: 2,
            appliedGeneration: 2,
            stopConfirmed: true,
            ackState: "stopped",
            ackAt: sampleTime,
            lastLease: Date.UTC(1999, 0, 1),
          },
        ],
  ...rule,
}));

test(
  "relay and backup UI render truthful mixed-generation state with all network intercepted",
  { timeout: 60000 },
  async (t) => {
    const compiled = await build({
      stdin: {
        contents: `
    import React from 'react'; import {createRoot} from 'react-dom/client';
    import './app.css'; import {Routes} from './business'; import {AdminRules, ResourcePage} from './admin'; import {Backups} from './backups';
    const root=createRoot(document.getElementById('fixture'));
    window.renderFixture=mode=>root.render(mode==='member'?React.createElement(Routes,{user:{id:'synthetic-user',expiresAt:Date.now()+86400000,trafficTotal:1e9,trafficUsed:0,rateMbps:5}}):mode==='admin'?React.createElement(AdminRules):mode==='agents'?React.createElement(ResourcePage,{kind:'agents'}):React.createElement(Backups));
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
    async function fixture(mode) {
      const page = await browser.newPage({
        viewport: { width: 1440, height: 1000 },
      });
      page.setDefaultTimeout(7000);
      const requests = [],
        errors = [],
        model = { blockers: [] };
      page.on("pageerror", (error) => errors.push(error.message));
      await page.route("**/*", (route) => {
        const request = route.request(),
          url = new URL(request.url());
        if (url.origin !== base) {
          errors.push("Unexpected external request");
          return route.abort();
        }
        if (url.pathname === "/")
          return route.fulfill({
            contentType: "text/html",
            body: '<!doctype html><meta name="viewport" content="width=device-width,initial-scale=1"><div id="fixture"></div>',
          });
        requests.push({ path: url.pathname, method: request.method() });
        if (
          request.method() === "POST" &&
          url.pathname === "/api/admin/backups/preflight"
        )
          return route.fulfill({
            json: {
              users: 2,
              collections: 3,
              sha256: "synthetic-backup-hash",
              report: {
                currentUsers: 2,
                currentSha256: "synthetic-current-hash",
                recoveryRequired: true,
                recoveryRules: ["recovery"],
                warnings: ["合成预检警告：旧节点可能继续运行，必须独立核对。"],
                blockers: model.blockers,
                differences: [],
                preservedRoutes: ["recovery"],
                preservedNodes: ["old-node"],
                settingKeys: [],
              },
            },
          });
        if (
          request.method() === "POST" &&
          url.pathname === "/api/user/routes/keep/diagnose"
        )
          return route.fulfill({
            json: {
              routeName: "完整保留线路",
              status: "success",
              latencyMs: 33,
              packetLossPercent: 0,
              message: "仅为控制面视角的 TCP 路径样本；延迟是入口建连时间，不验证每跳/Mieru/游戏协议",
            },
          });
        if (request.method() !== "GET") {
          errors.push(`Unexpected write ${url.pathname}`);
          return route.abort();
        }
        if (url.pathname === "/api/routes")
          return route.fulfill({
            json: {
              routes: rules.map((rule) => ({
                id: rule.id,
                name: rule.routeName,
                online: false,
                rateMbps: 5,
              })),
              userRateMbps: 5,
            },
          });
        if (
          url.pathname === "/api/user/rules" ||
          url.pathname === "/api/admin/user-rules"
        )
          return route.fulfill({ json: { rules, userRateMbps: 5 } });
        if (url.pathname === "/api/admin/relay-accounting")
          return route.fulfill({
            json: {
              periods: [
                {
                  userId: "synthetic-user",
                  entitlementVersion: 3,
                  confirmedBytes: 1024,
                  reviewBytes: 2048,
                },
              ],
              reviewBytes: 2048,
              reviewSamples: 2,
              degradedAgents: 1,
              message: "历史周期独立核对",
            },
          });
        if (url.pathname === "/api/admin/backups")
          return route.fulfill({ json: { backups: [] } });
        if (url.pathname === "/api/admin/backup-plan")
          return route.fulfill({ json: { enabled: false } });
        if (url.pathname === "/api/admin/backup-targets")
          return route.fulfill({ json: { targets: [] } });
        if (url.pathname === "/api/admin/relay-agents")
          return route.fulfill({
            json: {
              agents: [
                {
                  id: "synthetic-agent",
                  name: "合成节点",
                  address: "198.51.100.11",
                  online: false,
                  enabled: true,
                },
              ],
            },
          });
        errors.push(`Unexpected read ${url.pathname}`);
        return route.abort();
      });
      await page.goto(base);
      await page.addStyleTag({ content: style });
      await page.addScriptTag({ content: script });
      await page.evaluate((value) => window.renderFixture(value), mode);
      return { page, requests, errors, model };
    }
    try {
      await t.test(
        "member sees per-rule traffic and real diagnosis; verbose policy copy is removed",
        async () => {
          const { page, requests, errors } = await fixture("member");
          try {
            const card = (name) =>
              page
                .locator(".route-card")
                .filter({
                  has: page.getByRole("heading", { name, exact: true }),
                });
            const keep = card("完整保留线路"),
              recovery = card("恢复核对线路");
            await expect(keep).toContainText("已用上行流量：450.00 MB");
            await expect(keep).toContainText("已用下行流量：1.25 GB");
            await expect(keep).not.toContainText("管理连接");
            await expect(keep).not.toContainText("离线保留");
            await expect(keep).not.toContainText("45 秒");
            await expect(keep).toContainText("线路离线");
            await keep.getByRole("button", { name: "诊断", exact: true }).click();
            const diagnosis = page.getByRole("dialog");
            await expect(diagnosis).toContainText(
              "入口(完整保留线路)->目标(MSBOOST)",
            );
            await expect(diagnosis).toContainText("成功");
            await expect(diagnosis).toContainText("33");
            await expect(diagnosis).toContainText("0.00%");
            await expect(diagnosis).toContainText("不验证每跳/Mieru/游戏协议");
            await diagnosis
              .getByRole("button", { name: "关闭", exact: true })
              .click();
            await expect(recovery).toContainText(
              "待节点停止确认，端口及旧目标继续占用",
            );
            await expect(recovery).toContainText("计量状态降级");
            await expect(
              recovery.getByRole("button", { name: "恢复", exact: true }),
            ).toBeDisabled();
            await expect(
              recovery.getByRole("button", { name: "删除", exact: true }),
            ).toBeDisabled();
            await expect(
              page.getByRole("button", { name: "更换统一目标", exact: true }),
            ).toBeDisabled();
            await expect(recovery).not.toContainText("已暂停");
            for (const width of [1440, 375, 320]) {
              await page.setViewportSize({ width, height: 1000 });
              assert.equal(
                await page.evaluate(
                  () => document.documentElement.scrollWidth > innerWidth,
                ),
                false,
              );
            }
            assert.equal(
              requests.filter((request) => request.method === "POST").length,
              1,
            );
            assert.deepEqual(errors, []);
          } finally {
            await page.close();
          }
        },
      );
      await t.test(
        "admin shows read-only historical accounting and v2 explicit stop ACK, never old lease as deadline",
        async () => {
          const { page, requests, errors } = await fixture("admin");
          try {
            const accounting = page.locator(
              'details[aria-label="账务核对概览"]',
            );
            await accounting.locator("summary").click();
            await expect(accounting).toContainText("不混扣新权益");
            await expect(accounting).toContainText("待核对流量未自动调账");
            await expect(accounting).toContainText("2,048 字节");
            await expect(
              accounting.getByRole("columnheader", {
                name: "权益周期版本",
                exact: true,
              }),
            ).toBeVisible();
            await expect(accounting.getByRole("button")).toHaveCount(0);
            const row = page
              .getByRole("row")
              .filter({ hasText: "恢复核对线路" });
            await expect(
              row.getByRole("button", { name: "恢复", exact: true }),
            ).toBeDisabled();
            await expect(
              row.getByRole("button", { name: "撤销 / 删除", exact: true }),
            ).toBeDisabled();
            await row
              .getByRole("button", { name: "详情", exact: true })
              .click();
            const dialog = page.getByRole("dialog");
            await expect(dialog).toContainText("已用上行流量：450.00 MB");
            await expect(dialog).toContainText("已用下行流量：1.25 GB");
            await expect(dialog).toContainText("v2：等待明确停止确认");
            await expect(dialog).toContainText("v2：明确停止已确认");
            await expect(dialog).not.toContainText("1999");
            await expect(dialog).not.toContainText("45 秒");
            await expect(dialog).not.toContainText("最迟租约截止");
            await expect(
              dialog.getByRole("button", { name: "恢复", exact: true }),
            ).toBeDisabled();
            await expect(
              dialog.getByRole("button", { name: "撤销 / 删除", exact: true }),
            ).toBeDisabled();
            assert.ok(requests.every((request) => request.method === "GET"));
            assert.deepEqual(errors, []);
          } finally {
            await page.close();
          }
        },
      );
      await t.test(
        "backup preflight displays recovery resources and warnings while retaining blockers",
        async () => {
          const { page, requests, errors, model } = await fixture("backup");
          try {
            await page
              .getByRole("button", { name: "恢复备份", exact: true })
              .click();
            await expect(
              page.getByText(/Token 失效不代表旧业务已停止/),
            ).toBeVisible();
            await expect(
              page.getByText(/本机 root 中转恢复入口逐规则核对/),
            ).toBeVisible();
            await page
              .getByLabel("选择加密备份")
              .setInputFiles({
                name: "synthetic.msb",
                mimeType: "application/octet-stream",
                buffer: Buffer.from("not a real backup"),
              });
            await page
              .getByRole("button", { name: "只预检，不恢复", exact: true })
              .click();
            const report = page.getByRole("region", {
              name: "恢复核对预检结果",
            });
            await expect(report).toContainText("recovery_required");
            await expect(report).toContainText("待核对规则：recovery");
            await expect(report).toContainText(
              "合成预检警告：旧节点可能继续运行，必须独立核对。",
            );
            await expect(
              page.getByRole("button", { name: "确认安全恢复", exact: true }),
            ).toBeEnabled();
            model.blockers = ["v1 仍有有效租约"];
            await page
              .getByRole("button", { name: "只预检，不恢复", exact: true })
              .click();
            await expect(
              page.getByText("v1 仍有有效租约。处理后请重新预检。", {
                exact: true,
              }),
            ).toBeVisible();
            await expect(
              page.getByRole("button", { name: "确认安全恢复", exact: true }),
            ).toBeDisabled();
            assert.equal(
              requests.filter((request) => request.method !== "GET").length,
              2,
            );
            assert.ok(
              requests.every(
                (request) =>
                  request.method === "GET" ||
                  request.path.endsWith("/preflight"),
              ),
            );
            assert.deepEqual(errors, []);
          } finally {
            await page.close();
          }
        },
      );
      await t.test(
        "node re-enrollment warns before any write; the table labels management connection",
        async () => {
          const { page, requests, errors } = await fixture("agents");
          try {
            await expect(
              page.getByRole("columnheader", { name: "管理连接", exact: true }),
            ).toBeVisible();
            let warning = "";
            page.once("dialog", async (dialog) => {
              warning = dialog.message();
              await dialog.dismiss();
            });
            await page
              .getByRole("button", { name: "部署 / 重装", exact: true })
              .click();
            assert.match(warning, /仅撤销管理凭据不保证旧转发停止/);
            assert.match(warning, /keep_last.*独立受信核对或隔离/);
            assert.ok(requests.every((request) => request.method === "GET"));
            assert.deepEqual(errors, []);
          } finally {
            await page.close();
          }
        },
      );
    } finally {
      await browser.close();
    }
  },
);
