import { useEffect, useRef, useState } from "react";
import { Notice } from "./ui";
type Turnstile = {
  render(element: HTMLElement, options: Record<string, unknown>): string;
  remove(id: string): void;
};
declare global {
  interface Window {
    turnstile?: Turnstile;
  }
}
let script: Promise<void> | undefined;
function load() {
  if (window.turnstile) return Promise.resolve();
  return (script ||= new Promise<void>((resolve, reject) => {
    const el = document.createElement("script");
    el.src =
      "https://challenges.cloudflare.com/turnstile/v0/api.js?render=explicit";
    el.async = true;
    el.onload = () => resolve();
    el.onerror = () => {
      script = undefined;
      el.remove();
      reject(new Error("人机验证加载失败，请检查网络后刷新页面。"));
    };
    document.head.append(el);
  }));
}
// Cloudflare explicit rendering; each server submission consumes the token.
export function Captcha({
  siteKey,
  reset,
  onToken,
}: {
  siteKey: string;
  reset: number;
  onToken: (token: string) => void;
}) {
  const element = useRef<HTMLDivElement>(null),
    callback = useRef(onToken);
  callback.current = onToken;
  const [error, setError] = useState("");
  useEffect(() => {
    let disposed = false,
      id: string | undefined;
    callback.current("");
    setError("");
    load()
      .then(() => {
        if (disposed || !element.current) return;
        id = window.turnstile!.render(element.current, {
          sitekey: siteKey,
          theme: "light",
          size: "flexible",
          callback: (token: string) => callback.current(token),
          "expired-callback": () => callback.current(""),
          "error-callback": () => {
            callback.current("");
            setError("人机验证未完成，请刷新后重试。");
          },
        });
      })
      .catch((e) => {
        if (!disposed) setError(e.message);
      });
    return () => {
      disposed = true;
      if (id) window.turnstile?.remove(id);
    };
  }, [siteKey, reset]);
  return (
    <div className="mt16">
      <div ref={element} />
      {error && <Notice tone="red">{error}</Notice>}
    </div>
  );
}
