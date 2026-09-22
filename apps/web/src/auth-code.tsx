import { useEffect, useState } from "react";
import { Button } from "./ui";

// One conservative cooldown survives changing email or auth mode. The server
// remains authoritative; this only prevents misleading resend UI in this page.
export function useEmailCodeCooldown() {
  const [until, setUntil] = useState(0),
    [now, setNow] = useState(Date.now);
  useEffect(() => {
    if (!until) return;
    const tick = () => {
      const time = Date.now();
      setNow(time);
      if (time >= until) setUntil(0);
    };
    tick();
    const timer = window.setInterval(tick, 250);
    return () => window.clearInterval(timer);
  }, [until]);
  return {
    seconds: Math.max(0, Math.ceil((until - now) / 1000)),
    start(retryAfter: unknown) {
      const seconds =
        typeof retryAfter === "number" && Number.isFinite(retryAfter)
          ? Math.min(3600, Math.max(1, Math.ceil(retryAfter)))
          : 60;
      const time = Date.now();
      setNow(time);
      setUntil((old) => Math.max(old, time + seconds * 1000));
    },
  };
}

export function CodeSendButton({
  seconds,
  sending,
  disabled,
  onClick,
}: {
  seconds: number;
  sending: boolean;
  disabled?: boolean;
  onClick: () => void;
}) {
  return (
    <Button
      primary
      className="auth-code-button"
      disabled={disabled || sending || seconds > 0}
      onClick={onClick}
    >
      {sending
        ? "正在发送…"
        : seconds > 0
          ? `${seconds} 秒后重发`
          : "获取验证码"}
    </Button>
  );
}

export function qqEmailInput(value: string) {
  const email = value.trim().toLowerCase();
  if (!/^[0-9]{5,12}@qq\.com$/.test(email))
    throw new Error("请填写 5–12 位纯数字 QQ 邮箱");
  return email;
}

export function validateNewPassword(
  password: string,
  confirmation: string,
  email = "",
) {
  if (Array.from(password).length < 8) throw new Error("密码至少8字符");
  if (new TextEncoder().encode(password).length > 72)
    throw new Error("密码不能超过72字节");
  const normalizedEmail = email.trim().toLowerCase();
  const normalizedPassword = password.toLowerCase();
  const emailLocalPart = normalizedEmail.split("@", 1)[0];
  if (
    normalizedEmail &&
    (normalizedPassword === normalizedEmail ||
      normalizedPassword === emailLocalPart)
  )
    throw new Error("密码不能与邮箱或邮箱前缀相同");
  if (/\d{6,}/u.test(password))
    throw new Error("密码不能包含连续6位或以上数字");
  if (password !== confirmation) throw new Error("两次密码不一致");
}
