import { test } from "node:test";
import assert from "node:assert/strict";
import { chromium, expect } from "@playwright/test";
import { build } from "esbuild";
import { fileURLToPath } from "node:url";

// Compiles the real application and answers every request from fixtures.
// Nothing here contacts a server or a VPS.
const base = "http://127.0.0.1:19875";
const now = Date.now();
const hour = 3600000;
const codes = [
  {
    id: "code-a",
    code: "MSB-7Q4K-M9XP-2F6H",
    masked: "MSB-••••-••••-2F6H",
    tail: "2F6H",
    serverHost: "203.0.113.24",
    serverPort: 22,
    fingerprint: "SHA256:" + "Q".repeat(43),
    authMethod: "password",
    os: "Debian 12",
    maxUses: 1,
    usedCount: 0,
    reserved: 0,
    rateMbps: 5,
    createdAt: now - hour,
    expiresAt: now + 23 * hour,
    maxExpiresAt: now + 71 * hour,
    state: "active",
    secretHeld: true,
    note: "给小明",
    relays: 0,
    failures: 0,
  },
  {
    id: "code-b",
    code: "MSB-H3TR-8WQE-K5NA",
    masked: "MSB-••••-••••-K5NA",
    tail: "K5NA",
    serverHost: "203.0.113.24",
    serverPort: 22,
    fingerprint: "SHA256:" + "R".repeat(43),
    authMethod: "key",
    keyType: "ED25519",
    os: "Debian 12",
    maxUses: 10,
    usedCount: 3,
    reserved: 0,
    rateMbps: 8,
    createdAt: now - 2 * hour,
    expiresAt: now + 66 * hour,
    maxExpiresAt: now + 70 * hour,
    state: "active",
    secretHeld: true,
    note: "公会群",
    relays: 3,
    failures: 1,
  },
  {
    id: "code-c",
    code: "MSB-9FBG-T7HY-3WMS",
    masked: "MSB-••••-••••-3WMS",
    tail: "3WMS",
    serverHost: "198.51.100.73",
    serverPort: 22,
    fingerprint: "SHA256:" + "S".repeat(43),
    authMethod: "password",
    maxUses: 1,
    usedCount: 1,
    reserved: 0,
    rateMbps: 5,
    createdAt: now - 50 * hour,
    expiresAt: now - hour,
    maxExpiresAt: now,
    state: "exhausted",
    secretHeld: false,
    secretDeletedAt: now - 40 * hour,
    deleteReason: "exhausted",
    note: "周末开荒",
    relays: 1,
    failures: 0,
  },
];
const relays = [
  {
    id: "aaar",
    role: "relay",
    source: "code",
    codeTail: "K5NA",
    user: "55661234@qq.com",
    maskedIp: "203.*.*.45",
    remark: "公会群",
    port: 41872,
    rateMbps: 8,
    state: "running",
    connections: 3,
    createdAt: now - 30 * hour,
    recorded: true,
  },
  {
    id: "bbbr",
    role: "relay",
    source: "manual",
    user: "10086123@qq.com",
    maskedIp: "192.*.*.41",
    remark: "东京中转",
    port: 30017,
    rateMbps: 5,
    state: "paused",
    connections: 0,
    createdAt: now - 60 * hour,
    recorded: true,
  },
  {
    id: "cccr",
    role: "relay",
    source: "legacy",
    maskedIp: "192.*.*.88",
    port: 52210,
    rateMbps: 5,
    state: "stopped",
    connections: 0,
    createdAt: now - 200 * hour,
    recorded: false,
  },
];

test(
  "relay codes in the actual application",
  { timeout: 120000 },
  async (t) => {
    const compiled = await build({
      entryPoints: [fileURLToPath(new URL("../src/main.tsx", import.meta.url))],
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
    async function fixture({ admin = false, path = "/#relay" } = {}) {
      const page = await browser.newPage({
        viewport: { width: 1440, height: 1000 },
      });
      page.setDefaultTimeout(7000);
      const errors = [],
        writes = [];
      page.on("pageerror", (error) => errors.push(error.message));
      await page.route("**/*", async (route) => {
        const request = route.request(),
          url = new URL(request.url());
        if (url.origin !== base) {
          errors.push("Unexpected external request " + url.href);
          return route.abort();
        }
        if (url.pathname === "/")
          return route.fulfill({
            contentType: "text/html",
            body: '<!doctype html><meta name="viewport" content="width=device-width,initial-scale=1"><div id="root"></div>',
          });
        if (request.method() !== "GET") {
          writes.push({ path: url.pathname, body: request.postDataJSON() });
          if (url.pathname === "/api/relay-codes/preview")
            return route.fulfill({
              json: {
                ownerMasked: "5566****@qq.com",
                serverMasked: "198.*.*.152",
                os: "Debian 12",
                maxUses: 3,
                remaining: 2,
                expiresAt: now + 26 * hour,
                rateMbps: 5,
                tail: "5RZH",
                usable: true,
              },
            });
          errors.push("Unexpected write " + url.pathname);
          return route.abort();
        }
        const user = {
          id: admin ? "synthetic-admin" : "synthetic-member",
          email: admin ? "20000001@qq.com" : "10086123@qq.com",
          role: admin ? "admin" : "member",
          status: "active",
          emailVerifiedAt: 1,
          expiresAt: 0,
        };
        const json = {
          "/api/me": { user },
          "/api/settings": {
            register: true,
            deploy: true,
            relay: true,
            dd: true,
            relayCodes: {
              enabled: true,
              manageWithSecret: true,
              maxTtlHours: 72,
              maxUses: 10,
              maxActive: 5,
              defaultRateMbps: 5,
              rateMinMbps: 1,
              rateMaxMbps: 10,
            },
          },
          "/api/tasks": {
            tasks: [],
            limits: {
              relay: { minutes: 15, count: 5, remaining: 5 },
              dd: { minutes: 15, count: 5, remaining: 5 },
              deploy: { minutes: 15, count: 5, remaining: 5 },
            },
          },
          "/api/tickets": { tickets: [], unreadCount: 0 },
          "/api/articles": { articles: [] },
          "/api/admin/articles": { articles: [] },
          "/api/relay-codes": {
            codes,
            stats: {
              active: 2,
              maxActive: 5,
              secrets: 2,
              earliestDeletion: now + 23 * hour,
              relays: 4,
              failures: 1,
              used: 1,
            },
            settings: {},
            manageLimit: { minutes: 15, count: 20, remaining: 20 },
            limit: { minutes: 15, count: 5, remaining: 5 },
          },
          "/api/relay-codes/used": {
            relays: [
              {
                id: "use-1",
                taskId: "task-1",
                serverMasked: "198.*.*.152",
                ownerMasked: "5566****@qq.com",
                entryPort: 38122,
                target: "192.0.2.41:41234",
                rateMbps: 5,
                codeTail: "Q8ZD",
                createdAt: now - 20 * hour,
              },
            ],
          },
          "/api/relay-codes/code-b": {
            code: codes[1],
            uses: [
              {
                id: "u1",
                userEmail: "55661234@qq.com",
                maskedIp: "203.*.*.45",
                listenPort: 41872,
                result: "succeeded",
                createdAt: now - 30 * hour,
              },
              {
                id: "u2",
                userEmail: "77665544@qq.com",
                maskedIp: "192.*.*.23",
                result: "failed",
                failReason: "节点配置缺少认证",
                createdAt: now - 31 * hour,
              },
            ],
          },
          "/api/admin/relay-codes": {
            settings: {
              enabled: true,
              manageWithSecret: true,
              notifyOwner: false,
              maxTtlHours: 72,
              maxUses: 10,
              maxActive: 5,
              defaultRateMbps: 5,
              rateMinMbps: 1,
              rateMaxMbps: 10,
              redeemPerAccount: 10,
              redeemPerIp: 30,
              manageMinutes: 15,
              manageCount: 20,
            },
            stats: {
              activeCodes: 41,
              activeOwners: 23,
              secrets: 41,
              earliestDeletion: now + 12 * 60000,
              latestDeletion: now + 70 * hour,
              uses7d: 186,
              failures7d: 9,
              deleted7d: 57,
              deletedByReason: { expired: 31, exhausted: 22, revoked: 4 },
            },
            users: [
              {
                userId: "u-1",
                email: "10086123@qq.com",
                activeCodes: 2,
                secrets: 2,
                longestExpiresAt: now + 66 * hour,
                usedByOthers7d: 6,
                usedOthers7d: 1,
                disabled: false,
                atLimit: false,
              },
              {
                userId: "u-2",
                email: "12340000@qq.com",
                activeCodes: 0,
                secrets: 0,
                usedByOthers7d: 0,
                usedOthers7d: 0,
                disabled: true,
                disabledReason: "公开出售中转码",
                disabledAt: now - 7 * 24 * hour,
                atLimit: false,
              },
            ],
            deletions: [
              {
                id: "d1",
                createdAt: now - hour,
                ownerEmail: "55661234@qq.com",
                codeTail: "P4QW",
                reason: "exhausted",
                actor: "系统",
              },
            ],
          },
          "/api/admin/settings": { register: true, limits: {} },
          "/api/health": { status: "ok" },
          "/api/public/launcher": { launcher: {}, localApp: {} },
        }[url.pathname];
        if (json) return route.fulfill({ json });
        errors.push("Unexpected read " + url.pathname);
        return route.abort();
      });
      await page.goto(base + path);
      await page.addStyleTag({ content: style });
      await page.addScriptTag({ content: script });
      await expect(page.locator(".sidebar")).toBeVisible();
      return { page, errors, writes };
    }
    async function noOverflow(page, label) {
      for (const width of [1440, 1024, 768, 375, 320]) {
        await page.setViewportSize({ width, height: 1000 });
        assert.equal(
          await page.evaluate(
            () => document.documentElement.scrollWidth > innerWidth,
          ),
          false,
          `${label} overflows at ${width}px`,
        );
      }
      await page.setViewportSize({ width: 1440, height: 1000 });
    }
    async function noForwardWording(page, label) {
      const text = await page.locator("main").innerText();
      assert.ok(!text.includes("转发"), `${label} must say 中转, not 转发`);
      assert.ok(!text.includes("新增"), `${label} must not show 新增 tags`);
    }
    try {
      await t.test(
        "relay page: three modes, password default, clamped inputs",
        async () => {
          const { page, errors, writes } = await fixture();
          try {
            const cards = page
              .getByRole("tablist", { name: "中转方式" })
              .getByRole("tab");
            await expect(cards.locator("strong")).toHaveText([
              "使用中转码",
              "生成中转码",
              "手动配置中转",
            ]);
            await expect(cards.locator("small")).toHaveText([
              "输入朋友的中转码，上传MSBOOST配置文件",
              "把自己的服务器线路分享给朋友，不用交出 SSH",
              "填写自己服务器的 SSH（原有功能）",
            ]);
            await expect(cards.nth(0)).toHaveAttribute("aria-selected", "true");
            await expect(page.locator(".page-heading p")).toHaveText(
              "上传MSBOOST配置文件，中转你的MSBOOST节点",
            );
            await expect(page.locator(".quota span").first()).toHaveText(
              "操作限额 每 15 分钟最多 5 次 · 剩余 5 次",
            );
            await expect(
              page.getByText(
                "中转服务器的拥有者在「生成中转码」里生成后发给你。你不需要对方的 SSH，对方也无法使用你的MSBOOST节点。",
              ),
            ).toBeVisible();
            await expect(page.locator(".aside-list")).toContainText(
              "服务器线路分享朋友用",
            );
            await expect(page.locator(".sidebar .pill")).toHaveCount(0);
            await expect(page.locator(".sidebar")).toContainText("中转码管理");
            // A front server can be used with a relay code.
            await page
              .getByRole("checkbox", { name: "使用自备前置机" })
              .check();
            await expect(page.getByLabel("前置机 IP 地址")).toBeVisible();
            await expect(page.locator(".flow")).toContainText("自备前置机");
            await expect(
              page
                .getByRole("tablist", { name: "前置机登录方式" })
                .getByRole("tab", { name: "密码" }),
            ).toHaveAttribute("aria-selected", "true");
            await noForwardWording(page, "使用中转码");
            await noOverflow(page, "使用中转码");

            await cards.nth(1).click();
            await expect(page.locator(".quota small")).toHaveText(
              "同时有效的中转码 2 / 5 个",
            );
            const login = page.getByRole("tablist", { name: "SSH 登录方式" });
            await expect(
              login.getByRole("tab", { name: "密码" }),
            ).toHaveAttribute("aria-selected", "true");
            await expect(page.getByLabel("SSH 密码")).toBeVisible();
            await expect(
              page.getByText("加密暂存到中转码失效为止，失效后立即删除。"),
            ).toBeVisible();
            await page.getByLabel("SSH 密码").fill("ui-only-password");
            await login.getByRole("tab", { name: "密钥" }).click();
            await expect(page.getByLabel("SSH 私钥")).toBeVisible();
            await expect(page.getByLabel("私钥口令（可选）")).toBeVisible();
            await expect(
              page.getByText("建议：为中转码单独生成一把密钥"),
            ).toBeVisible();
            await page
              .getByLabel("SSH 私钥")
              .fill("PuTTY-User-Key-File-3: ssh-ed25519");
            await expect(
              page.getByText(
                "PuTTY 的 .ppk 请先用 PuTTYgen 导出为 OpenSSH 格式。",
                { exact: true },
              ),
            ).toBeVisible();
            await login.getByRole("tab", { name: "密码" }).click();
            await expect(page.getByLabel("SSH 密码")).toHaveValue("");
            const uses = page.getByLabel("可使用次数");
            await expect(uses).toHaveValue("1");
            await uses.fill("99");
            await expect(uses).toHaveValue("10");
            await uses.fill("0");
            await expect(uses).toHaveValue("1");
            await expect(
              page.getByText("1–10 次；中转建立成功才计次。"),
            ).toBeVisible();
            const ttl = page.getByLabel("有效期", { exact: true });
            await expect(ttl).toHaveValue("24");
            await ttl.fill("100");
            await expect(ttl).toHaveValue("72");
            await expect(
              page
                .getByRole("group", { name: "常用有效期" })
                .getByRole("button"),
            ).toHaveText(["12 小时", "24 小时", "48 小时", "72 小时"]);
            await page.getByRole("button", { name: "48 小时" }).click();
            await expect(ttl).toHaveValue("48");
            await expect(
              page.getByText("最短 1 小时，最长 72 小时。"),
            ).toBeVisible();
            await expect(page.locator(".vault-note strong")).toHaveText(
              "SSH 最多暂存 48 小时，或使用 1 次后自动删除（以先到者为准）",
            );
            const rate = page.getByLabel("中转带宽");
            await expect(rate).toHaveValue("5");
            await expect(rate.locator("option")).toHaveCount(10);
            await expect(
              page.getByText(
                "中转码只发给你信任的人。使用者建立的中转长期有效",
                { exact: false },
              ),
            ).toBeVisible();
            await expect(
              page.getByText("禁止出售", { exact: false }),
            ).toHaveCount(0);
            await noForwardWording(page, "生成中转码");
            await noOverflow(page, "生成中转码");

            await cards.nth(2).click();
            await expect(page.getByLabel("中转带宽")).toHaveValue("5");
            await expect(
              page.getByLabel("中转带宽").locator("option"),
            ).toHaveText([
              "1 Mbps",
              "2 Mbps",
              "3 Mbps",
              "4 Mbps",
              "5 Mbps（默认）",
              "6 Mbps",
              "7 Mbps",
              "8 Mbps",
              "9 Mbps",
              "10 Mbps",
            ]);
            await expect(
              page.getByText(
                "入口使用实际中转服务器的 IP；监听端口随机分配，带宽按你的选择（默认 5 Mbps）。",
                { exact: true },
              ),
            ).toBeVisible();
            await expect(
              page
                .getByRole("tablist", { name: "SSH 登录方式" })
                .getByRole("tab", { name: "密码" }),
            ).toHaveAttribute("aria-selected", "true");
            await noForwardWording(page, "手动配置中转");
            await noOverflow(page, "手动配置中转");
            assert.deepEqual(writes, []);
            assert.deepEqual(errors, []);
          } finally {
            await page.close();
          }
        },
      );

      await t.test("shared link fills the code and queries it", async () => {
        const { page, errors, writes } = await fixture({
          path: "/?relayCode=MSB-C8VN-2KQT-5RZH#relay",
        });
        try {
          await expect(page.locator("#relay-code")).toHaveValue(
            "MSB-C8VN-2KQT-5RZH",
          );
          await expect(page.locator(".code-preview")).toContainText(
            "中转服务器 198.*.*.152",
          );
          await expect(page.locator(".code-preview")).toContainText("2 / 3 次");
          await expect(page.locator(".code-preview")).toContainText(
            "上下行各 5 Mbps",
          );
          await expect(page.locator(".code-preview")).toContainText(
            "长期，不随中转码失效",
          );
          assert.equal(writes.length, 1);
          assert.deepEqual(writes[0], {
            path: "/api/relay-codes/preview",
            body: { code: "MSB-C8VN-2KQT-5RZH" },
          });
          assert.equal(
            await page.evaluate(() => location.search),
            "",
            "the code leaves the address bar",
          );
          await noForwardWording(page, "分享链接");
          assert.deepEqual(errors, []);
        } finally {
          await page.close();
        }
      });

      await t.test("relay code management tabs and dialogs", async () => {
        const { page, errors } = await fixture({ path: "/#codes" });
        try {
          await expect(page.getByRole("heading", { level: 1 })).toHaveText(
            "中转码管理",
          );
          await expect(page.locator(".page-heading p")).toHaveText(
            "管理你生成的中转码；读取中转服务器上的全部中转，按使用者修改带宽、暂停或删除。",
          );
          await expect(page.getByRole("tab")).toHaveText([
            "我的中转码（3）",
            "中转服务器管理",
            "我使用的中转（1）",
          ]);
          const table = page.locator("table").first();
          await expect(table).toContainText("MSB-••••-••••-2F6H");
          await expect(table).toContainText("暂存中 · 密钥");
          await expect(table).toContainText("次数用完");
          await table
            .getByRole("button", { name: "显示中转码" })
            .first()
            .click();
          await expect(table).toContainText("MSB-7Q4K-M9XP-2F6H");
          await noForwardWording(page, "我的中转码");
          await noOverflow(page, "我的中转码");
          await table
            .locator("tr", { hasText: "公会群" })
            .getByRole("button", { name: "详情" })
            .click();
          const detail = page.getByRole("dialog");
          await expect(detail).toContainText("MSB-H3TR-8WQE-K5NA");
          await expect(detail).toContainText("使用记录（2）");
          await expect(detail).toContainText("节点配置缺少认证 · 不计次");
          await expect(
            detail.getByRole("button", { name: /管理这些中转/ }),
          ).toBeVisible();
          assert.ok(!(await detail.innerText()).includes("转发"));
          await detail.getByRole("button", { name: "调整" }).click();
          const adjust = page.getByRole("dialog");
          await expect(adjust).toContainText("调整中转码");
          await expect(
            adjust.getByRole("combobox", { name: "有效期" }).locator("option"),
          ).toHaveText([
            "保持不变",
            "延长 12 小时",
            "延长 24 小时",
            /延长到上限/,
          ]);
          await expect(adjust).toContainText("最多 10 次");
          await expect(adjust).toContainText(
            "从生成时算起总长不超过 72 小时。",
          );
          await adjust.getByRole("button", { name: "取消" }).click();
          await table
            .locator("tr", { hasText: "公会群" })
            .getByRole("button", { name: "作废" })
            .click();
          const revoke = page.getByRole("dialog");
          await expect(revoke).toContainText("保留已建立的 3 条中转");
          await expect(revoke).toContainText("同时删除这 3 条中转");
          await expect(
            revoke.getByRole("button", { name: /作废中转码/ }),
          ).toBeDisabled();
          assert.ok(!(await revoke.innerText()).includes("转发"));
          await revoke.getByRole("button", { name: "取消" }).click();

          await page.getByRole("tab", { name: "中转服务器管理" }).click();
          await expect(
            page
              .getByRole("tablist", { name: "SSH 来源" })
              .getByRole("tab", { selected: true }),
          ).toHaveText("使用中转码暂存的 SSH");
          await expect(page.getByLabel("选择中转码")).toContainText(
            "中转码 …2F6H「给小明」",
          );
          await expect(
            page.getByRole("button", { name: /读取中转列表/ }),
          ).toBeVisible();
          await page
            .getByRole("tablist", { name: "SSH 来源" })
            .getByRole("tab", { name: "输入 SSH" })
            .click();
          await expect(
            page
              .getByRole("tablist", { name: "SSH 登录方式" })
              .getByRole("tab", { name: "密码" }),
          ).toHaveAttribute("aria-selected", "true");
          await noForwardWording(page, "中转服务器管理");
          await noOverflow(page, "中转服务器管理");

          await page.getByRole("tab", { name: /我使用的中转/ }).click();
          await expect(page.locator("table")).toContainText("192.0.2.41:41234");
          await expect(
            page.getByRole("button", { name: /本机未保存/ }),
          ).toBeDisabled();
          await expect(
            page.getByText("连不上时请联系拥有者", { exact: false }),
          ).toBeVisible();
          await noForwardWording(page, "我使用的中转");
          await noOverflow(page, "我使用的中转");
          assert.deepEqual(errors, []);
        } finally {
          await page.close();
        }
      });

      await t.test(
        "DD with key login offers keep-key or new password",
        async () => {
          const { page, errors } = await fixture({ path: "/#dd" });
          try {
            await expect(page.locator(".quota span").first()).toHaveText(
              "操作限额 每 15 分钟最多 5 次 · 剩余 5 次",
            );
            await expect(
              page.getByRole("combobox", { name: "重装密码策略" }),
            ).toHaveValue("keep");
            await page
              .getByRole("tablist", { name: "SSH 登录方式" })
              .getByRole("tab", { name: "密钥" })
              .click();
            await expect(
              page.getByRole("combobox", { name: "重装密码策略" }),
            ).toHaveCount(0);
            await expect(
              page.getByRole("radio", { name: /继续使用当前密钥/ }),
            ).toBeChecked();
            await page.getByRole("radio", { name: /改用密码登录/ }).check();
            await expect(page.getByLabel("新 root 密码")).toBeVisible();
            await expect(page.getByText("新增")).toHaveCount(0);
            await noOverflow(page, "DD 密钥登录");
            assert.deepEqual(errors, []);
          } finally {
            await page.close();
          }
        },
      );

      await t.test("administrator relay code settings and audit", async () => {
        const { page, errors } = await fixture({
          admin: true,
          path: "/#settings",
        });
        try {
          await page
            .getByRole("button", { name: "中转码", exact: true })
            .click();
          await expect(page.getByLabel("有效期上限（小时）")).toHaveValue("72");
          await expect(
            page.getByText("最长 72 小时，只能调低。"),
          ).toBeVisible();
          await expect(page.getByLabel("每个中转码最多次数")).toHaveValue("10");
          await expect(page.getByText("AES-256-GCM（主密钥）")).toBeVisible();
          await expect(
            page.getByRole("button", { name: /立即删除全部暂存的 SSH/ }),
          ).toBeVisible();
          await noForwardWording(page, "系统设置 · 中转码");
          await noOverflow(page, "系统设置 · 中转码");
          await page.evaluate(() => (location.hash = "relayAudit"));
          await expect(page.getByRole("heading", { level: 1 })).toHaveText(
            "中转码审计",
          );
          await expect(page.locator("table")).toContainText("已停用中转码功能");
          await expect(
            page.getByRole("button", { name: "恢复功能" }),
          ).toBeVisible();
          await expect(page.locator(".sidebar .pill")).toHaveCount(0);
          await noForwardWording(page, "中转码审计");
          await page.getByRole("tab", { name: "SSH 删除记录" }).click();
          await expect(page.locator("table")).toContainText("MSB-…-P4QW");
          await expect(page.locator("table")).toContainText("次数用完");
          await noOverflow(page, "中转码审计");
          assert.deepEqual(errors, []);
        } finally {
          await page.close();
        }
      });
    } finally {
      await browser.close();
    }
  },
);
