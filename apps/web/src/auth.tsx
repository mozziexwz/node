import { useState } from "react";
import {
  Server,
  Route,
  RefreshCw,
  Check as CheckIcon,
  ArrowRight,
  Download,
} from "lucide-react";
import { RecordData, post } from "./api";
import { Captcha } from "./captcha";
import {
  Brand,
  Maple,
  Field,
  Check,
  Button,
  Modal,
  Notice,
  AsyncForm,
} from "./ui";
const terms = [
  [
    "服务性质",
    "MSBOOST 提供 VPS 节点部署、系统重装、自备中转配置及增值线路中转等技术服务，服务器由用户自行购买和管理。",
  ],
  [
    "使用范围",
    "本站服务仅限 MapleStory 及相关游戏用途，禁止用于翻墙、公共代理、违法活动或其他与游戏无关的用途。",
  ],
  [
    "服务器操作风险",
    "部署、修改网络配置及 DD 重装可能导致服务中断或数据丢失。执行前请自行备份重要数据。",
  ],
  [
    "账号与服务器安全",
    "请妥善保管账号、SSH 密码、配置文件及其他凭据。因用户自行泄露、错误配置或第三方攻击造成的损失，由用户自行承担。",
  ],
  [
    "第三方服务",
    "VPS、网络线路、云服务商、游戏服务器及其他第三方服务不由 MSBOOST 控制，本站不保证其持续可用。",
  ],
  [
    "禁止滥用",
    "不得攻击、扫描、转售、共享滥用本站资源，也不得尝试绕过限速、流量限制或用途限制。",
  ],
  [
    "服务调整",
    "为保障安全和稳定，MSBOOST 有权对存在异常、滥用或违反规则的账号及服务进行限制、暂停或终止。",
  ],
];
const privacy = [
  "本站仅收集提供服务所必要的账号、任务、节点和使用记录。",
  "SSH 用户名、密码等敏感信息仅用于当前任务处理，不作为普通业务数据长期保存。",
  "为安全审计，本站可能记录操作时间、服务器 IP、目标 IP/端口、任务结果及来源 IP，但不会在普通日志中记录密码、令牌等秘密信息。",
  "本站不会主动出售用户个人信息。",
  "用户应避免向本站提交与服务无关的个人敏感信息。",
];
export function Agreements({
  kind,
  onClose,
}: {
  kind: string;
  onClose: () => void;
}) {
  return (
    <Modal title={kind === "terms" ? "用户协议" : "隐私政策"} onClose={onClose}>
      <div className="stack">
        {kind === "terms"
          ? terms.map(([h, p], i) => (
              <div key={h}>
                <h4>
                  {i + 1}. {h}
                </h4>
                <p className="muted mt8">{p}</p>
              </div>
            ))
          : privacy.map((p, i) => (
              <p key={i}>
                {i + 1}. {p}
              </p>
            ))}
      </div>
    </Modal>
  );
}
export function AuthPage({
  settings,
  onLogin,
}: {
  settings: RecordData;
  onLogin: () => void;
}) {
  const [mode, setMode] = useState("login"),
    [agreement, setAgreement] = useState(""),
    [email, setEmail] = useState(""),
    [codeStatus, setCodeStatus] = useState(""),
    [sending, setSending] = useState(false),
    [waitUntil, setWaitUntil] = useState(0),
    [turnstileToken, setTurnstileToken] = useState(""),
    [captchaReset, setCaptchaReset] = useState(0);
  async function send() {
    setSending(true);
    try {
      if (settings.turnstile && !turnstileToken)
        throw new Error("请先完成人机验证");
      await post("/api/auth/email/send", {
        email,
        purpose: "register",
        turnstileToken,
      });
      setCodeStatus("验证码已发送，请检查收件箱。");
      setWaitUntil(Date.now() + 60000);
    } catch (e) {
      setCodeStatus((e as Error).message);
    } finally {
      setSending(false);
      setTurnstileToken("");
      setCaptchaReset((n) => n + 1);
    }
  }
  return (
    <div className="login-body">
      <header className="public-top">
        <Brand />
        <small>专注你的游戏连接</small>
      </header>
      <section className="hero">
        <div>
          <div className="eyebrow">SELF-HOSTED · MAPLESTORY</div>
          <h1 className="v4-hero-title">
            <span className="v4-hero-line">一键部署你的</span>
            <span className="v4-hero-line orange">独立IP游戏节点</span>
          </h1>
          <p className="v4-hero-copy">
            部署节点、配置中转、重装系统。
            <br />
            把复杂的服务器操作，放进一个清晰的工作空间。
          </p>
          <div className="hero-points">
            {["三项免费核心工具", "需自备 VPS", "Windows 启动器"].map((x) => (
              <span key={x}>
                <CheckIcon size={13} /> {x}
              </span>
            ))}
          </div>
          <div className="hero-steps">
            <div>
              <Server size={24} />
              <strong>准备自己的 VPS</strong>
            </div>
            <ArrowRight size={14} />
            <div className="active">
              <Maple />
              <strong>部署 MSBOOST</strong>
            </div>
            <ArrowRight size={14} />
            <div>
              <Download size={24} />
              <strong>下载配置并启动</strong>
            </div>
          </div>
        </div>
        <div className="card auth-card">
          <div className="eyebrow">WELCOME TO MSBOOST</div>
          <h2>{mode === "login" ? "欢迎回来" : "创建你的账号"}</h2>
          <p>
            {mode === "login"
              ? "登录，管理你的节点与中转。"
              : "注册前请先准备自己的服务器。"}
          </p>
          <div className="auth-tabs">
            <button
              className={mode === "login" ? "active" : ""}
              onClick={() => setMode("login")}
            >
              登录
            </button>
            <button
              className={mode === "register" ? "active" : ""}
              onClick={() => setMode("register")}
            >
              注册
            </button>
          </div>
          {mode === "register" && settings.register === false ? (
            <Notice>管理员已关闭注册，请联系站点管理员。</Notice>
          ) : (
            <AsyncForm
              key={mode}
              label={mode === "login" ? "登录" : "注册并登录"}
              onSubmit={async (f) => {
                if (
                  mode === "register" &&
                  f.get("password") !== f.get("confirm")
                )
                  throw new Error("两次密码不一致");
                if (settings.turnstile && !turnstileToken)
                  throw new Error("请先完成人机验证");
                try {
                  await post("/api/auth/" + mode, {
                    email,
                    password: f.get("password"),
                    turnstileToken,
                    ...(mode === "register"
                      ? {
                          inviteCode: f.get("inviteCode") || "",
                          code: f.get("code") || "",
                          agree: true,
                        }
                      : {}),
                  });
                } finally {
                  setTurnstileToken("");
                  setCaptchaReset((n) => n + 1);
                }
                onLogin();
              }}
            >
              <Field
                label="QQ 邮箱"
                type="email"
                name="email"
                value={email}
                onChange={(e) => setEmail(e.target.value)}
                placeholder="请输入 QQ 邮箱"
                autoComplete="username"
                required
              />
              <Field
                label="密码"
                type="password"
                name="password"
                minLength={mode === "register" ? 12 : 1}
                autoComplete={
                  mode === "login" ? "current-password" : "new-password"
                }
                placeholder="请输入密码"
                required
              />
              {mode === "register" && (
                <>
                  <Field
                    label="确认密码"
                    type="password"
                    name="confirm"
                    autoComplete="new-password"
                    required
                  />
                  {settings.invite && (
                    <Field label="邀请码" name="inviteCode" required />
                  )}
                  {settings.registrationEmailVerificationRequired && (
                    <>
                      <div className="email-code-input41">
                        <Field
                          label="邮箱验证码"
                          name="code"
                          inputMode="numeric"
                          autoComplete="one-time-code"
                          required
                        />
                        <Button
                          disabled={sending}
                          onClick={() => {
                            if (Date.now() < waitUntil) {
                              setCodeStatus("请在 60 秒后重发");
                              return;
                            }
                            void send();
                          }}
                        >
                          获取验证码
                        </Button>
                      </div>
                      {codeStatus && <Notice>{codeStatus}</Notice>}
                    </>
                  )}
                </>
              )}
              {settings.turnstile && (
                <Captcha
                  siteKey={settings.turnstileSiteKey}
                  reset={captchaReset}
                  onToken={setTurnstileToken}
                />
              )}
              <Check
                required
                label={
                  <>
                    我已阅读并同意
                    <button type="button" onClick={() => setAgreement("terms")}>
                      《用户协议》
                    </button>
                    与
                    <button
                      type="button"
                      onClick={() => setAgreement("privacy")}
                    >
                      《隐私政策》
                    </button>
                  </>
                }
              />
            </AsyncForm>
          )}
        </div>
      </section>
      <section className="public-tools">
        <div className="grid3">
          {[
            [Server, "一键部署 MSBOOST", "部署你的专属 MSBOOST 节点"],
            [Route, "自备服务器中转", "一键中转你或朋友的 MSBOOST 节点"],
            [RefreshCw, "在线 DD 系统", "一键重装系统"],
          ].map(([Icon, title, sub]) => {
            const I = Icon as typeof Server;
            return (
              <button
                className="card tool-card"
                key={String(title)}
                onClick={() =>
                  document
                    .querySelector<HTMLInputElement>('input[name="email"]')
                    ?.focus()
                }
              >
                <span className="tool-icon">
                  <I size={22} />
                </span>
                <div>
                  <h3>{String(title)}</h3>
                  <p>{String(sub)}</p>
                </div>
              </button>
            );
          })}
        </div>
      </section>
      <footer className="public-footer">
        <span>© {new Date().getFullYear()} MSBOOST</span>
        <div className="flex">
          <button onClick={() => setAgreement("terms")}>用户协议</button>
          <button onClick={() => setAgreement("privacy")}>隐私政策</button>
        </div>
      </footer>
      {agreement && (
        <Agreements kind={agreement} onClose={() => setAgreement("")} />
      )}
    </div>
  );
}
