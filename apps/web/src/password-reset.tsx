import { useState } from "react";
import { post, RecordData } from "./api";
import { Captcha } from "./captcha";
import { AsyncForm, Button, Field, Notice } from "./ui";
import { CodeSendButton, qqEmailInput, validateNewPassword } from "./auth-code";

const requestMessage =
  "如果该邮箱对应可找回的会员账号，我们将发送验证码，请留意收件箱。";

export function PasswordReset({
  settings,
  email,
  onEmail,
  seconds,
  startCooldown,
  working,
  onWorking,
  onBack,
  onSuccess,
}: {
  settings: RecordData;
  email: string;
  onEmail: (email: string) => void;
  seconds: number;
  startCooldown: (retryAfter: unknown) => void;
  working: boolean;
  onWorking: (working: boolean) => void;
  onBack: () => void;
  onSuccess: () => void;
}) {
  const [sending, setSending] = useState(false),
    [status, setStatus] = useState(""),
    [sendError, setSendError] = useState(""),
    [turnstileToken, setTurnstileToken] = useState(""),
    [captchaReset, setCaptchaReset] = useState(0);
  function resetCaptcha() {
    setTurnstileToken("");
    setCaptchaReset((n) => n + 1);
  }
  async function send() {
    if (working || seconds > 0) return;
    setSending(true);
    onWorking(true);
    setStatus("");
    setSendError("");
    try {
      const recipient = qqEmailInput(email);
      if (settings.turnstile && !turnstileToken)
        throw new Error("请先完成人机验证");
      const result = await post("/api/auth/password/reset/request", {
        email: recipient,
        turnstileToken,
      });
      // Do not infer account existence, verification status or role here.
      setStatus(requestMessage);
      startCooldown(result.retryAfter);
    } catch (e) {
      setSendError((e as Error).message);
    } finally {
      resetCaptcha();
      setSending(false);
      onWorking(false);
    }
  }
  return (
    <section aria-label="会员找回密码" className="password-reset">
      <p className="auth-reset-help">
        普通会员（包括尚未验证邮箱的会员）可通过注册邮箱找回密码。
        管理员账号不支持邮件找回，请在控制面服务器本机处理。
      </p>
      <AsyncForm
        label="重置密码"
        onSubmit={async (form) => {
          if (working) throw new Error("请等待当前请求完成");
          const recipient = qqEmailInput(email);
          const password = String(form.get("newPassword") || "");
          validateNewPassword(
            password,
            String(form.get("confirm") || ""),
            recipient,
          );
          const code = String(form.get("code") || "").trim();
          if (!/^[0-9]{6}$/.test(code)) throw new Error("请输入六位数字验证码");
          if (settings.turnstile && !turnstileToken)
            throw new Error("请再次完成人机验证后提交新密码");
          onWorking(true);
          try {
            await post("/api/auth/password/reset/confirm", {
              email: recipient,
              code,
              newPassword: password,
              turnstileToken,
            });
            onSuccess();
          } finally {
            resetCaptcha();
            onWorking(false);
          }
        }}
      >
        <Field
          label="QQ 邮箱"
          type="email"
          name="email"
          autoComplete="username"
          placeholder="请输入注册时使用的 QQ 邮箱"
          value={email}
          onChange={(e) => {
            onEmail(e.target.value);
            setStatus("");
            setSendError("");
          }}
          disabled={working}
          required
        />
        <div className="email-code-input41 auth-code-row">
          <Field
            label="邮箱验证码"
            name="code"
            inputMode="numeric"
            autoComplete="one-time-code"
            pattern="[0-9]{6}"
            maxLength={6}
            placeholder="六位数字验证码"
            disabled={working}
            required
          />
          <CodeSendButton
            seconds={seconds}
            sending={sending}
            disabled={working}
            onClick={() => void send()}
          />
        </div>
        {status && (
          <div role="status">
            <Notice>{status}</Notice>
          </div>
        )}
        {sendError && <Notice tone="red">{sendError}</Notice>}
        <Field
          label="新密码"
          type="password"
          name="newPassword"
          autoComplete="new-password"
          aria-describedby="reset-password-requirements"
          disabled={working}
          required
        />
        <p id="reset-password-requirements" className="auth-reset-help">
          密码至少8字符。
        </p>
        <Field
          label="确认新密码"
          type="password"
          name="confirm"
          autoComplete="new-password"
          disabled={working}
          required
        />
        {settings.turnstile && (
          <>
            <p className="auth-reset-help">
              获取验证码与提交新密码均需完成人机验证。
            </p>
            <Captcha
              siteKey={settings.turnstileSiteKey}
              reset={captchaReset}
              onToken={setTurnstileToken}
            />
          </>
        )}
      </AsyncForm>
      <Button className="link auth-back" disabled={working} onClick={onBack}>
        返回登录
      </Button>
    </section>
  );
}
