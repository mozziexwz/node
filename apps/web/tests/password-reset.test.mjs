import { test } from "node:test";
import assert from "node:assert/strict";
import { chromium, expect } from "@playwright/test";
import { build } from "esbuild";
import { fileURLToPath } from "node:url";

// In-memory component fixture only: every request is intercepted. No local
// server, real account, SMTP service, CAPTCHA service or VPS is contacted.
const base = "http://127.0.0.1:19873";
const genericMessage =
  "如果该邮箱对应可找回的会员账号，我们将发送验证码，请留意收件箱。";
const requestPath = "/api/auth/password/reset/request";
const confirmPath = "/api/auth/password/reset/confirm";

test(
  "member password recovery UI and auth regression",
  { timeout: 90000 },
  async (t) => {
    const compiled = await build({
      stdin: {
        contents: `
        import React from 'react';
        import {createRoot} from 'react-dom/client';
        import './app.css';
        import {AuthPage} from './auth';
        window.loginCount = 0;
        createRoot(document.getElementById('fixture')).render(
          React.createElement(AuthPage, {settings: window.fixtureSettings, onLogin: () => window.loginCount++})
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
    async function fixture({ captcha = false, register = true } = {}) {
      const page = await browser.newPage({
        viewport: { width: 1440, height: 1000 },
      });
      page.setDefaultTimeout(7000);
      const requests = [],
        errors = [];
      const model = {
        requestStatus: 200,
        confirmStatus: 200,
        retryAfter: 60,
        hold: false,
        held: undefined,
      };
      page.on("pageerror", (error) => errors.push(error.message));
      await page.clock.install();
      await page.route("**/*", async (route) => {
        const request = route.request(),
          url = new URL(request.url());
        if (url.origin !== base) return route.abort();
        if (url.pathname === "/")
          return route.fulfill({
            contentType: "text/html",
            body: '<!doctype html><meta name="viewport" content="width=device-width,initial-scale=1"><div id="fixture"></div>',
          });
        const body = request.postDataJSON();
        requests.push({ path: url.pathname, body });
        if (
          url.pathname === requestPath ||
          url.pathname === "/api/auth/email/send"
        ) {
          if (model.hold) {
            model.held = route;
            return;
          }
          return route.fulfill(
            model.requestStatus === 200
              ? {
                  json: {
                    ok: true,
                    message: genericMessage,
                    retryAfter: model.retryAfter,
                    expiresIn: 600,
                  },
                }
              : {
                  status: model.requestStatus,
                  json: { error: "请求过于频繁，请稍后重试" },
                },
          );
        }
        if (url.pathname === confirmPath)
          return route.fulfill(
            model.confirmStatus === 200
              ? { json: { ok: true, message: "密码已重置" } }
              : {
                  status: model.confirmStatus,
                  json: { error: "验证码无效或已过期，请重新获取" },
                },
          );
        if (
          url.pathname === "/api/auth/login" ||
          url.pathname === "/api/auth/register"
        )
          return route.fulfill({ json: { ok: true } });
        errors.push(`Unexpected request ${url.pathname}`);
        return route.abort();
      });
      await page.goto(base);
      await page.evaluate(
        ({ captcha, register }) => {
          window.fixtureSettings = {
            register,
            registrationEmailVerificationRequired: true,
            turnstile: captcha,
            turnstileSiteKey: "synthetic-site-key",
          };
          window.captchaRenders = 0;
          window.captchaRemovals = 0;
          const widgets = new Map();
          window.turnstile = {
            render(element, options) {
              const id = String(++window.captchaRenders);
              const button = document.createElement("button");
              button.type = "button";
              button.textContent = "完成测试人机验证";
              button.onclick = () => {
                options.callback("synthetic-token-" + id);
                button.disabled = true;
              };
              element.appendChild(button);
              widgets.set(id, element);
              return id;
            },
            remove(id) {
              widgets.get(id)?.replaceChildren();
              widgets.delete(id);
              window.captchaRemovals++;
            },
          };
        },
        { captcha, register },
      );
      await page.addStyleTag({ content: style });
      await page.addScriptTag({ content: script });
      await expect(
        page.getByRole("heading", { name: "欢迎回来", exact: true }),
      ).toBeVisible();
      return { page, requests, errors, model };
    }
    async function openReset(page) {
      await page
        .getByRole("button", { name: "忘记密码？", exact: true })
        .click();
      await expect(
        page.getByRole("heading", { name: "找回密码", exact: true }),
      ).toBeVisible();
    }
    async function fillNewPassword(
      page,
      password = "Synthetic-password-123",
      code = "028461",
    ) {
      await page.getByLabel("邮箱验证码", { exact: true }).fill(code);
      await page.getByLabel("新密码", { exact: true }).fill(password);
      await page.getByLabel("确认新密码", { exact: true }).fill(password);
    }
    try {
      await t.test(
        "send state and server-driven cooldown survive auth mode/email changes; responsive colors",
        async () => {
          const { page, requests, errors, model } = await fixture();
          try {
            await openReset(page);
            await expect(
              page.getByText(/管理员账号不支持邮件找回/),
            ).toBeVisible();
            await page.getByLabel("QQ 邮箱").fill("10000001@qq.com");
            const send = page.getByRole("button", {
              name: "获取验证码",
              exact: true,
            });
            assert.equal(
              await send.evaluate(
                (node) => getComputedStyle(node).backgroundColor,
              ),
              "rgb(231, 101, 39)",
            );
            model.hold = true;
            await send.click();
            await expect.poll(() => !!model.held).toBe(true);
            await expect(
              page.getByRole("button", { name: "正在发送…", exact: true }),
            ).toBeDisabled();
            await expect(page.getByLabel("QQ 邮箱")).toBeDisabled();
            await expect(
              page
                .locator(".auth-tabs")
                .getByRole("button", { name: "登录", exact: true }),
            ).toBeDisabled();
            await model.held.fulfill({
              json: {
                ok: true,
                message: genericMessage,
                retryAfter: 17,
                expiresIn: 600,
              },
            });
            await expect(page.getByRole("status")).toContainText(
              genericMessage,
            );
            const resend = page.getByRole("button", { name: /秒后重发/ });
            await expect(resend).toBeDisabled();
            await expect(resend).toHaveText(/1[67] 秒后重发/);
            await expect
              .poll(() =>
                resend.evaluate(
                  (node) => getComputedStyle(node).backgroundColor,
                ),
              )
              .toBe("rgb(229, 229, 229)");
            await page.getByLabel("QQ 邮箱").fill("10000002@qq.com");
            await expect(resend).toBeDisabled();
            await page
              .locator(".auth-tabs")
              .getByRole("button", { name: "注册", exact: true })
              .click();
            await expect(
              page.getByRole("button", { name: /秒后重发/ }),
            ).toBeDisabled();
            await page
              .locator(".auth-tabs")
              .getByRole("button", { name: "登录", exact: true })
              .click();
            await openReset(page);
            await expect(
              page.getByRole("button", { name: /秒后重发/ }),
            ).toBeDisabled();
            await page.clock.runFor(18000);
            await expect(
              page.getByRole("button", { name: "获取验证码", exact: true }),
            ).toBeEnabled();
            assert.equal(requests.length, 1);
            for (const width of [1440, 784, 375, 320]) {
              await page.setViewportSize({ width, height: 1000 });
              assert.equal(
                await page.evaluate(
                  () => document.documentElement.scrollWidth > innerWidth,
                ),
                false,
                `no overflow at ${width}`,
              );
              await expect(
                page.getByRole("button", { name: "获取验证码", exact: true }),
              ).toBeVisible();
              await expect(
                page.getByRole("heading", { level: 1 }).locator("span"),
              ).toHaveText(["一键部署你的", "独立IP游戏节点"]);
            }
            assert.deepEqual(errors, []);
          } finally {
            await page.close();
          }
        },
      );

      await t.test(
        "request/confirmation require separate CAPTCHA; leading zero and Unicode password; no automatic login",
        async () => {
          const { page, requests, errors } = await fixture({
            captcha: true,
            register: false,
          });
          try {
            await openReset(page);
            await page.getByLabel("QQ 邮箱").fill("10000001@qq.com");
            await page
              .getByRole("button", { name: "获取验证码", exact: true })
              .click();
            await expect(page.getByRole("alert")).toContainText(
              "请先完成人机验证",
            );
            assert.equal(requests.length, 0);
            await page
              .getByRole("button", { name: "完成测试人机验证", exact: true })
              .click();
            await page
              .getByRole("button", { name: "获取验证码", exact: true })
              .click();
            await expect(page.getByRole("status")).toContainText(
              genericMessage,
            );
            const firstToken = requests[0].body.turnstileToken;
            assert.ok(firstToken.startsWith("synthetic-token-"));
            await fillNewPassword(page, "一二三四五六七八");
            await page
              .getByRole("button", { name: "重置密码", exact: true })
              .click();
            await expect(page.getByRole("alert")).toContainText(
              "请再次完成人机验证",
            );
            assert.equal(requests.length, 1);
            await page
              .getByRole("button", { name: "完成测试人机验证", exact: true })
              .click();
            await page
              .getByRole("button", { name: "重置密码", exact: true })
              .click();
            await expect(
              page.getByRole("heading", { name: "欢迎回来", exact: true }),
            ).toBeVisible();
            await expect(page.getByRole("status")).toContainText(
              "密码已重置，请使用新密码重新登录",
            );
            assert.deepEqual(
              requests.map((r) => r.path),
              [requestPath, confirmPath],
            );
            assert.equal(requests[1].body.code, "028461");
            assert.equal(requests[1].body.newPassword, "一二三四五六七八");
            assert.notEqual(requests[1].body.turnstileToken, firstToken);
            assert.equal(await page.evaluate(() => window.loginCount), 0);
            await expect(page.getByLabel("密码", { exact: true })).toHaveValue(
              "",
            );
            await expect(page.getByLabel("QQ 邮箱")).toHaveValue(
              "10000001@qq.com",
            );
            await expect(page.getByRole("checkbox")).not.toBeChecked();
            assert.deepEqual(errors, []);
          } finally {
            await page.close();
          }
        },
      );

      await t.test(
        "invalid QQ/password/code rejected; rate-limit and invalid-code errors do not claim success",
        async () => {
          const { page, requests, model, errors } = await fixture();
          try {
            await openReset(page);
            await page.getByLabel("QQ 邮箱").fill("named@qq.com");
            await page
              .getByRole("button", { name: "获取验证码", exact: true })
              .click();
            await expect(page.getByRole("alert")).toContainText(
              "纯数字 QQ 邮箱",
            );
            assert.equal(requests.length, 0);
            await page.getByLabel("QQ 邮箱").fill("10000001@qq.com");
            await fillNewPassword(page, "短密码");
            await page
              .getByRole("button", { name: "重置密码", exact: true })
              .click();
            await expect(page.getByRole("alert")).toContainText("至少8字符");
            for (const [password, expected] of [
              ["10000001", "不能与邮箱或邮箱前缀相同"],
              ["10000001@qq.com", "不能与邮箱或邮箱前缀相同"],
              ["secure123456x", "不能包含连续6位或以上数字"],
            ]) {
              await fillNewPassword(page, password);
              await page
                .getByRole("button", { name: "重置密码", exact: true })
                .click();
              await expect(page.getByRole("alert")).toContainText(expected);
            }
            await fillNewPassword(page);
            await page
              .getByLabel("确认新密码", { exact: true })
              .fill("different-password");
            await page
              .getByRole("button", { name: "重置密码", exact: true })
              .click();
            await expect(page.getByRole("alert")).toContainText(
              "两次密码不一致",
            );
            assert.equal(requests.length, 0);
            await fillNewPassword(page, "Synthetic-password-123", "123");
            await page
              .getByRole("button", { name: "重置密码", exact: true })
              .click();
            assert.equal(requests.length, 0);
            model.requestStatus = 429;
            await page
              .getByRole("button", { name: "获取验证码", exact: true })
              .click();
            await expect(
              page.getByText("请求过于频繁，请稍后重试", { exact: true }),
            ).toBeVisible();
            await expect(
              page.getByRole("button", { name: "获取验证码", exact: true }),
            ).toBeEnabled();
            model.confirmStatus = 400;
            await fillNewPassword(page);
            await page
              .getByRole("button", { name: "重置密码", exact: true })
              .click();
            await expect(
              page.getByText("验证码无效或已过期，请重新获取", { exact: true }),
            ).toBeVisible();
            assert.equal(await page.evaluate(() => window.loginCount), 0);
            await expect(
              page.getByRole("heading", { name: "找回密码", exact: true }),
            ).toBeVisible();
            assert.deepEqual(errors, []);
          } finally {
            await page.close();
          }
        },
      );

      await t.test(
        "registration retains existing payload, consumes fresh CAPTCHA and shares resend guard; login unchanged",
        async () => {
          const { page, requests, errors } = await fixture({ captcha: true });
          try {
            await page
              .locator(".auth-tabs")
              .getByRole("button", { name: "注册", exact: true })
              .click();
            await page.getByLabel("QQ 邮箱").fill("10000001@qq.com");
            await page
              .getByRole("button", { name: "完成测试人机验证", exact: true })
              .click();
            await page
              .getByRole("button", { name: "获取验证码", exact: true })
              .click();
            await expect(
              page.getByText("验证码已发送，请检查收件箱。", { exact: true }),
            ).toBeVisible();
            assert.equal(requests[0].path, "/api/auth/email/send");
            assert.equal(requests[0].body.purpose, "register");
            await page
              .getByLabel("密码", { exact: true })
              .fill("Synthetic-password-123");
            await page
              .getByLabel("确认密码", { exact: true })
              .fill("Synthetic-password-123");
            await page.getByLabel("邮箱验证码", { exact: true }).fill("028461");
            await page.getByRole("checkbox").check();
            await page
              .getByRole("button", { name: "注册并登录", exact: true })
              .click();
            await expect(page.getByRole("alert")).toContainText(
              "请先完成人机验证",
            );
            await page
              .getByRole("button", { name: "完成测试人机验证", exact: true })
              .click();
            await page
              .getByRole("button", { name: "注册并登录", exact: true })
              .click();
            await expect.poll(() => requests.length).toBe(2);
            assert.equal(requests[1].path, "/api/auth/register");
            assert.equal(requests[1].body.agree, true);
            assert.equal(requests[1].body.code, "028461");
            assert.notEqual(
              requests[1].body.turnstileToken,
              requests[0].body.turnstileToken,
            );
            await expect
              .poll(() => page.evaluate(() => window.loginCount))
              .toBe(1);
            await page
              .locator(".auth-tabs")
              .getByRole("button", { name: "登录", exact: true })
              .click();
            await page
              .getByLabel("密码", { exact: true })
              .fill("Synthetic-password-123");
            await page.getByRole("checkbox").check();
            await page
              .getByRole("button", { name: "完成测试人机验证", exact: true })
              .click();
            await page
              .locator("form")
              .getByRole("button", { name: "登录", exact: true })
              .click();
            await expect
              .poll(() => page.evaluate(() => window.loginCount))
              .toBe(2);
            assert.equal(requests[2].path, "/api/auth/login");
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
