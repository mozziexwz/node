import { test } from "node:test";
import assert from "node:assert/strict";
import { chromium, expect } from "@playwright/test";
import { build } from "esbuild";
import { fileURLToPath } from "node:url";

// Compile the actual application and intercept every browser request. SSH
// identity changes are simulated here; no test contacts a VPS.
const base = "http://127.0.0.1:19874";
const availability =
  "本站捐赠权益提供的中转服务不承诺 100% 可用性。如您对稳定性要求较高，建议使用本站免费工具搭配自备服务器部署中转，或选择专业游戏加速器。";
const purchaseTerms =
  "再次兑换条件：剩余时间少于30天，或剩余流量少于10GB。新权益覆盖旧的剩余时间与流量，不叠加。";

test(
  "approved copy in the actual application",
  { timeout: 60000 },
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
    async function fixture({
      signedIn = false,
      admin = false,
      view = "tutorials",
      unreadCount = 0,
      tickets = [],
      fingerprintChange = false,
    } = {}) {
      const page = await browser.newPage({
        viewport: { width: 1440, height: 1000 },
      });
      page.setDefaultTimeout(7000);
      const requests = [],
        errors = [];
      page.on("pageerror", (error) => errors.push(error.message));
      let ticketUnread = unreadCount;
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
            body: '<!doctype html><meta name="viewport" content="width=device-width,initial-scale=1"><div id="root"></div>',
          });
        requests.push({ path: url.pathname, method: request.method() });
        if (
          request.method() === "POST" &&
          /^\/api\/tickets\/[^/]+\/read$/.test(url.pathname)
        ) {
          ticketUnread = 0;
          return route.fulfill({ json: { ok: true, unreadCount: 0 } });
        }
        if (
          fingerprintChange &&
          request.method() === "POST" &&
          url.pathname === "/api/fingerprints"
        )
          return route.fulfill({
            json: {
              fingerprint: "SHA256:new-test-fingerprint",
              rememberedFingerprint: "SHA256:old-test-fingerprint",
            },
          });
        if (request.method() !== "GET") {
          errors.push(`Unexpected write ${url.pathname}`);
          return route.abort();
        }
        const user = {
          id: "synthetic-copy-member",
          email: "10000001@qq.com",
          role: admin ? "admin" : "member",
          status: "active",
          emailVerifiedAt: 1,
          balanceCents: 1200,
          expiresAt: Date.now() + 86400000,
          trafficTotal: 1e9,
          trafficUsed: 0,
          rateMbps: 5,
        };
        if (url.pathname === "/api/me")
          return route.fulfill(
            signedIn
              ? { json: { user } }
              : { status: 401, json: { error: "未登录" } },
          );
        if (url.pathname === "/api/settings")
          return route.fulfill({
            json: { register: true, deploy: true, relay: true, dd: true },
          });
        if (url.pathname === "/api/plans")
          return route.fulfill({
            json: {
              plans: [
                {
                  id: "synthetic-copy-plan",
                  name: "合成测试权益",
                  priceCents: 1200,
                  days: 1,
                  trafficBytes: 1e9,
                  rateMbps: 5,
                  level: 2,
                  enabled: true,
                },
              ],
            },
          });
        if (url.pathname === "/api/payment-channels")
          return route.fulfill({ json: { channels: [] } });
        if (url.pathname === "/api/routes")
          return route.fulfill({ json: { routes: [], userRateMbps: 5 } });
        if (url.pathname === "/api/user/rules")
          return route.fulfill({ json: { rules: [], userRateMbps: 5 } });
        if (url.pathname === "/api/tickets")
          return route.fulfill({
            json: { tickets, unreadCount: ticketUnread },
          });
        if (url.pathname === "/api/tasks")
          return route.fulfill({ json: { tasks: [], limits: {} } });
        if (
          url.pathname === "/api/articles" ||
          url.pathname === "/api/admin/articles"
        )
          return route.fulfill({ json: { articles: [] } });
        if (url.pathname === "/api/health")
          return route.fulfill({ json: { status: "ok" } });
        errors.push(`Unexpected read ${url.pathname}`);
        return route.abort();
      });
      await page.goto(base + "/#" + view);
      await page.addStyleTag({ content: style });
      await page.addScriptTag({ content: script });
      await expect(
        page.locator(signedIn ? ".sidebar" : ".auth-card"),
      ).toBeVisible();
      return { page, requests, errors };
    }
    async function navigate(page, view) {
      await page.evaluate((value) => {
        location.hash = value;
      }, view);
    }
    async function noOverflow(page) {
      assert.equal(
        await page.evaluate(
          () => document.documentElement.scrollWidth > innerWidth,
        ),
        false,
      );
    }
    try {
      await t.test(
        "public tool copy, plain support address below copyright and preserved authentication",
        async () => {
          const { page, requests, errors } = await fixture();
          try {
            const cards = page.locator(".public-tools .tool-card");
            await expect(cards.locator("h3")).toHaveText([
              "一键部署 MSBOOST",
              "一键配置中转服务器",
              "一键DD系统",
            ]);
            await expect(cards.locator("p")).toHaveText([
              "部署你的独立IP游戏节点",
              "与朋友共享你的服务器，但不共享你的IP",
              "在线重装系统",
            ]);
            const contact = page.locator(".public-footer .public-contact");
            await expect(contact.locator("span")).toHaveText([
              `© ${new Date().getFullYear()} MSBOOST`,
              "admin@msboost.de",
            ]);
            await expect(page.locator(".public-footer a")).toHaveCount(0);
            await expect(page.locator('[href^="mailto:"]')).toHaveCount(0);
            const bounds = await contact.locator("span").evaluateAll((items) =>
              items.map((item) => ({
                top: item.getBoundingClientRect().top,
                bottom: item.getBoundingClientRect().bottom,
              })),
            );
            assert.ok(
              bounds[1].top >= bounds[0].bottom,
              "support address is a separate line below copyright",
            );
            await expect(
              page.getByRole("button", { name: "忘记密码？", exact: true }),
            ).toBeVisible();
            await expect(
              page.getByRole("heading", { level: 1 }).locator(":scope > span"),
            ).toHaveText(["一键部署你的", "独立IP游戏节点"]);
            for (const width of [1440, 784, 375, 320]) {
              await page.setViewportSize({ width, height: 1000 });
              await noOverflow(page);
            }
            assert.ok(requests.every((request) => request.method === "GET"));
            assert.deepEqual(errors, []);
          } finally {
            await page.close();
          }
        },
      );
      await t.test(
        "member navigation and tool breadcrumbs; DD still only Debian 12 with keep defaults",
        async () => {
          const { page, requests, errors } = await fixture({
            signedIn: true,
            view: "relay",
          });
          try {
            const nav = page.getByRole("navigation", { name: "主导航" });
            await expect(
              nav.getByRole("button", { name: "配置中转服务器", exact: true }),
            ).toBeVisible();
            await expect(
              nav.getByRole("button", { name: "中转线路", exact: true }),
            ).toBeVisible();
            await expect(
              page.getByRole("button", { name: "工单消息，无未读", exact: true }),
            ).toBeVisible();
            await expect(page.locator(".crumb")).toContainText(
              "配置中转服务器",
            );
            await expect(page.getByRole("heading", { level: 1 })).toHaveText(
              "配置中转服务器",
            );
            await expect(page.locator(".page-heading p")).toHaveText(
              "上传MSBOOST配置文件，中转你或朋友的MSBOOST节点",
            );
            await expect(
              page.getByText(
                "入口使用实际中转服务器的 IP；监听端口随机分配，每端口固定 5 Mbps。",
                { exact: true },
              ),
            ).toBeVisible();
            await expect(
              page.getByText("仅支持 Debian 系统部署（Debian 11 或以上版本）。", { exact: true }),
            ).toBeVisible();
            await expect(
              page.getByRole("button", { name: "开始配置", exact: true }),
            ).toBeVisible();
            await expect(
              page.locator('[data-testid="cleanup-panel"] > summary'),
            ).toHaveText("清理中转服务器配置");
            await navigate(page, "deploy");
            await expect(page.getByRole("heading", { level: 1 })).toHaveText(
              "部署 MSBOOST",
            );
            await expect(page.locator(".page-heading p")).toHaveText(
              "在你的VPS上部署游戏节点，并下载MSBOOST配置文件。",
            );
            await expect(page.getByText("安全升级 / 修复", { exact: true })).toHaveCount(0);
            await expect(
              page.getByText(
                "每次均全新部署受管 MSBOOST 组件，重新生成认证和端口；不会重装操作系统。原配置部署成功后将失效。",
                { exact: true },
              ),
            ).toBeVisible();
            await expect(
              page.getByText("请先备份旧配置", { exact: false }),
            ).toHaveCount(0);
            await expect(
              page.getByText("仅支持 Debian 系统部署（Debian 11 或以上版本）。", { exact: true }),
            ).toBeVisible();
            await navigate(page, "dd");
            await expect(page.getByRole("heading", { level: 1 })).toHaveText(
              "DD 系统",
            );
            await expect(page.locator(".page-heading p")).toHaveText(
              "在线重装系统，默认保持当前SSH端口和密码。",
            );
            await expect(
              page.getByLabel("重装系统", { exact: true }),
            ).toHaveValue("Debian 12");
            await expect(
              page.getByLabel("重装系统", { exact: true }),
            ).toBeDisabled();
            await expect(
              page.getByRole("combobox", {
                name: "重装后 SSH 端口",
                exact: true,
              }),
            ).toHaveValue("keep");
            await expect(
              page.getByRole("combobox", { name: "重装密码策略", exact: true }),
            ).toHaveValue("keep");
            await expect(
              page.getByText(
                "DD系统会清除服务器原有系统和数据，操作前请先备份或确保无重要数据。操作提交后请等待15分钟以上，再执行部署 MSBOOST。",
                { exact: true },
              ),
            ).toBeVisible();
            await expect(
              page.getByRole("button", { name: "开始DD", exact: true }),
            ).toBeVisible();
            await expect(
              page.getByRole("checkbox", {
                name: "我已备份重要数据，并理解重装会清除原系统和数据",
              }),
            ).not.toBeChecked();
            for (const width of [1440, 375, 320]) {
              await page.setViewportSize({ width, height: 1000 });
              await noOverflow(page);
            }
            assert.ok(requests.every((request) => request.method === "GET"));
            assert.deepEqual(errors, []);
          } finally {
            await page.close();
          }
        },
      );
      await t.test(
        "changed SSH identity keeps detailed guidance below the identity warning",
        async () => {
          const { page, requests, errors } = await fixture({
            signedIn: true,
            view: "deploy",
            fingerprintChange: true,
          });
          try {
            const form = page.locator(".tool-layout > div > .card form").first();
            await form
              .getByLabel("服务器 IP 地址", { exact: true })
              .fill("198.51.100.10");
            await form.getByLabel(/SSH 密码/).fill("synthetic-password");
            await form.getByRole("button", { name: "开始部署" }).click();
            await expect(
              page.getByText("服务器身份已变化，请在高级设置中核对。", {
                exact: true,
              }),
            ).toBeVisible();
            const warning = page.getByText(
              "服务器身份已变化。若没有重装或更换服务器，请停止操作。",
              { exact: true },
            );
            const guidance = page.getByText(
              "这台服务器的身份与上次不同，操作已暂停。若刚重装过服务器，请到服务商控制台核对，再在高级设置中确认；没有重装过请先联系服务商。",
              { exact: true },
            );
            await expect(warning).toBeVisible();
            await expect(guidance).toBeVisible();
            assert.equal(
              await guidance.evaluate((node) =>
                node.previousElementSibling?.textContent?.trim(),
              ),
              "服务器身份已变化。若没有重装或更换服务器，请停止操作。",
            );
            assert.equal(await guidance.evaluate((node) => node.closest(".notice")), null);
            assert.deepEqual(
              requests
                .filter((request) => request.method === "POST")
                .map((request) => request.path),
              ["/api/fingerprints"],
            );
            for (const width of [1440, 375]) {
              await page.setViewportSize({ width, height: 1000 });
              await noOverflow(page);
            }
            await page.setViewportSize({ width: 320, height: 1000 });
            assert.equal(
              await guidance.evaluate(
                (node) => node.getBoundingClientRect().right > innerWidth,
              ),
              false,
            );
            assert.deepEqual(errors, []);
          } finally {
            await page.close();
          }
        },
      );
      await t.test(
        "admin receives unread ticket bell but cannot create a ticket, and opening marks it read",
        async () => {
          const ticket = {
            id: "synthetic-ticket",
            title: "合成工单",
            status: "open",
            createdAt: Date.now(),
            lastReplyAt: Date.now(),
            lastReplyRole: "user",
            replyCount: 1,
            replies: [],
          };
          const { page, requests, errors } = await fixture({
            signedIn: true,
            admin: true,
            view: "tickets",
            unreadCount: 2,
            tickets: [ticket],
          });
          try {
            await expect(page.locator(".page-heading p")).toHaveText(
              "请说明问题和贴出任务记录",
            );
            const bell = page.getByRole("button", {
              name: "工单消息，2条未读",
              exact: true,
            });
            await expect(bell).toBeVisible();
            await expect(bell).toHaveClass(/has-unread/);
            await expect(bell).toContainText("2");
            await expect(
              page.getByRole("button", { name: "新建工单", exact: true }),
            ).toHaveCount(0);
            await page
              .getByRole("button", { name: "查看 / 回复", exact: true })
              .click();
            await expect(page.getByRole("dialog")).toBeVisible();
            await expect(
              page.getByRole("button", { name: "工单消息，无未读", exact: true }),
            ).toBeVisible();
            assert.equal(
              requests.filter(
                (request) =>
                  request.method === "POST" &&
                  request.path === "/api/tickets/synthetic-ticket/read",
              ).length,
              1,
            );
            assert.deepEqual(errors, []);
          } finally {
            await page.close();
          }
        },
      );
      await t.test(
        "paid wording and info notice ordering preserve purchase disclosure and confirmation",
        async () => {
          const { page, requests, errors } = await fixture({
            signedIn: true,
            view: "routes",
          });
          try {
            await expect(page.locator(".crumb")).toContainText("中转线路");
            await expect(
              page.getByText(
                "每条中转规则独立限速，上下行分别计算；实际取用户限速与线路上限的较小值。",
                { exact: true },
              ),
            ).toBeVisible();
            await expect(
              page.getByText(
                "同一账号所有线路必须中转同一个 MSBOOST 节点配置。",
                { exact: true },
              ),
            ).toBeVisible();
            await expect(page.locator("main")).not.toContainText(
              "不是账号共享总带宽",
            );
            await navigate(page, "plans");
            await expect(page.getByRole("heading", { level: 1 })).toHaveText(
              "枫叶兑换",
            );
            const warning = page.getByText(availability, { exact: true }),
              terms = page.getByText(purchaseTerms, { exact: true });
            await expect(warning).toBeVisible();
            await expect(terms).toBeVisible();
            const warningBox = await warning.boundingBox(),
              termsBox = await terms.boundingBox();
            assert.ok(
              warningBox.y + warningBox.height <= termsBox.y,
              "availability statement appears above existing purchase terms",
            );
            const info = warning.locator("..");
            await expect(info).toHaveClass("notice ");
            await expect(info.locator("svg")).toHaveCount(1);
            for (const width of [1440, 375, 320]) {
              await page.setViewportSize({ width, height: 1000 });
              await noOverflow(page);
              await expect(warning).toBeVisible();
            }
            await page
              .getByRole("button", { name: "选择权益", exact: true })
              .click();
            const dialog = page.getByRole("dialog");
            await expect(
              dialog.getByRole("checkbox", {
                name: "我理解新权益将覆盖旧的剩余时间和流量",
              }),
            ).not.toBeChecked();
            await expect(dialog.getByRole("checkbox")).toHaveAttribute(
              "required",
              "",
            );
            await expect(
              dialog.getByRole("button", { name: "确认兑换", exact: true }),
            ).toBeVisible();
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
