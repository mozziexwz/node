import React, { useEffect, useRef, useState } from "react";
import {
  ArrowRight,
  Copy,
  Eye,
  EyeOff,
  Link2,
  Server,
  Share2,
  ShieldCheck,
  Ticket,
  Clock,
  RefreshCw,
  Pause,
  Play,
  Trash2,
  Gauge,
  Plus,
  CircleCheck,
  KeyRound,
  Users,
  Route,
  RotateCw,
  ArrowLeftRight,
  Download,
} from "lucide-react";
import {
  api,
  post,
  requestID,
  copyText,
  download,
  RecordData,
  array,
} from "./api";
import {
  Field,
  Select,
  Check,
  Button,
  Notice,
  Header,
  Modal,
  Table,
  Badge,
  useData,
  ErrorNotice,
  Empty,
} from "./ui";
import {
  SSH,
  SSHFields,
  prepareSSH,
  newSSH,
  withoutSecrets,
  ConfigUpload,
  TaskDetail,
  ToolForm,
  QuotaBar,
  RateSelect,
  rateRange,
  EmailGate,
  needsEmailGate,
} from "./tools";
import { getLocal } from "./vault";

type Mode = "use" | "make" | "manual";
type CodesTab = "codes" | "servers" | "used";
// Set by another page right before navigating here.
let requestedMode: Mode | null = null;
let requestedTab: CodesTab | null = null;
let requestedCodeID = "";
export function openRelayMode(mode: Mode, navigate: (view: string) => void) {
  requestedMode = mode;
  navigate("relay");
}
export function openRelayCodes(
  tab: CodesTab,
  navigate: (view: string) => void,
  codeId = "",
) {
  requestedTab = tab;
  requestedCodeID = codeId;
  navigate("codes");
}
export function relayCodeFromURL() {
  try {
    return new URLSearchParams(location.search).get("relayCode") || "";
  } catch {
    return "";
  }
}

const pad = (n: number) => String(n).padStart(2, "0");
export function shortTime(ms: number) {
  const d = new Date(ms);
  return `${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
}
export function fullTime(ms: number) {
  return `${new Date(ms).getFullYear()}-${shortTime(ms)}`;
}
export function remainingText(ms: number, now = Date.now()) {
  const minutes = Math.max(0, Math.floor((ms - now) / 60000)),
    hours = Math.floor(minutes / 60),
    days = Math.floor(hours / 24);
  if (days > 0) return `${days} 天 ${hours % 24} 小时`;
  if (hours > 0) return `${hours} 小时 ${minutes % 60} 分`;
  return `${Math.max(1, minutes)} 分钟`;
}
export function shareLink(code: string) {
  return `${location.origin}/?relayCode=${encodeURIComponent(code)}#relay`;
}
function shareText(code: RecordData) {
  return [
    "我在 MSBOOST 给你开了一个中转：",
    `中转码：${code.code}`,
    `有效期：${fullTime(code.expiresAt)} 前，可使用 ${code.maxUses - code.usedCount} 次`,
    `带宽：上下行各 ${code.rateMbps} Mbps`,
    `使用方法：登录 ${location.host} → 配置中转服务器 → 使用中转码，上传你自己的 MSBOOST 节点配置即可。`,
    `链接：${shareLink(code.code)}`,
  ].join("\n");
}
const stateLabels: Record<string, [string, string]> = {
  active: ["有效", "green"],
  checking: ["检查中", "orange"],
  exhausted: ["已用完", ""],
  expired: ["已过期", ""],
  revoked: ["已作废", "red"],
};
const ownerReasons: Record<string, string> = {
  expired: "到期",
  exhausted: "次数用完",
  revoked: "你作废",
  admin_revoke: "管理员作废",
  admin_disabled: "管理员停用功能",
  admin_purge: "管理员删除全部",
  feature_disabled: "功能关闭",
  owner_disabled: "账号停用",
  restore: "备份恢复",
};
const adminReasons: Record<string, [string, string]> = {
  expired: ["到期", ""],
  exhausted: ["次数用完", ""],
  revoked: ["拥有者作废", "orange"],
  admin_revoke: ["管理员作废", "red"],
  admin_disabled: ["管理员停用功能", "red"],
  admin_purge: ["管理员删除全部", "red"],
  feature_disabled: ["功能关闭", "orange"],
  owner_disabled: ["账号停用", "orange"],
  restore: ["备份恢复", "orange"],
};
const manageErrors: Record<string, string> = {
  not_found: "没有找到这条中转",
  service_failed: "服务没有正常启动",
  paused: "已暂停，请先恢复",
  failed: "操作失败",
};
const actionNames: Record<string, string> = {
  rate: "修改带宽",
  pause: "暂停",
  resume: "恢复",
  restart: "重新启动",
  delete: "删除",
};
function taskFailure(task: RecordData) {
  return [task.message, task.nextStep].filter(Boolean).join(" ");
}
async function waitForTask(id: string): Promise<RecordData> {
  for (;;) {
    const task = await api(`/api/tasks/${id}`);
    if (!["queued", "running"].includes(task.state)) return task;
    await new Promise((resolve) => setTimeout(resolve, 2000));
  }
}
// Inputs never hold a value outside the allowed range.
function clampInput(raw: string, max: number): number | "" {
  if (raw === "") return "";
  const n = Math.trunc(Number(raw));
  if (!Number.isFinite(n)) return 1;
  return Math.min(max, Math.max(1, n));
}
function codeSettings(settings: RecordData) {
  const codes = settings.relayCodes || {};
  return {
    enabled: codes.enabled !== false,
    manageWithSecret: codes.manageWithSecret !== false,
    maxTtl: Number(codes.maxTtlHours) || 72,
    maxUses: Number(codes.maxUses) || 10,
    maxActive: Number(codes.maxActive) || 5,
  };
}
function CopyButton({
  value,
  label,
  icon = "copy",
  small = true,
  className = "",
}: {
  value: string;
  label: string;
  icon?: "copy" | "link";
  small?: boolean;
  className?: string;
}) {
  const [done, setDone] = useState(false);
  const Icon = icon === "link" ? Link2 : Copy;
  return (
    <Button
      className={`${small ? "small" : ""} ${className}`}
      onClick={async () => {
        try {
          await copyText(value);
          setDone(true);
          setTimeout(() => setDone(false), 1500);
        } catch {
          setDone(false);
        }
      }}
    >
      <Icon size={14} />
      {done ? "已复制" : label}
    </Button>
  );
}

// ===== 配置中转服务器 =====

export function RelayPage({
  user,
  settings,
  onNavigate,
}: {
  user: RecordData;
  settings: RecordData;
  onNavigate: (view: string) => void;
}) {
  // Initializers stay pure (StrictMode may run them twice); the hand-off
  // from another page is cleared once this page has mounted.
  const [mode, setMode] = useState<Mode>(() => requestedMode || "use");
  useEffect(() => {
    requestedMode = null;
  }, []);
  const linked = relayCodeFromURL();
  const codes = codeSettings(settings);
  const cards: [Mode, string, string, typeof Server][] = [
    ["use", "使用中转码", "输入朋友的中转码，上传MSBOOST配置文件", Ticket],
    [
      "make",
      "生成中转码",
      "把自己的服务器线路分享给朋友，不用交出 SSH",
      Share2,
    ],
    ["manual", "手动配置中转", "填写自己服务器的 SSH（原有功能）", Server],
  ];
  return (
    <>
      <Header
        title="配置中转服务器"
        sub="上传MSBOOST配置文件，中转你的MSBOOST节点"
      />
      {needsEmailGate(user, settings) ? (
        <EmailGate onNavigate={onNavigate} />
      ) : settings.relay === false ? (
        <Notice>该功能当前由管理员关闭，已有任务仍可查看。</Notice>
      ) : (
        <>
          <div className="mode-cards" role="tablist" aria-label="中转方式">
            {cards.map(([key, name, note, Icon]) => (
              <button
                type="button"
                role="tab"
                key={key}
                aria-selected={mode === key}
                className={"mode-card " + (mode === key ? "active" : "")}
                onClick={() => setMode(key)}
              >
                <span className="mode-icon">
                  <Icon size={18} />
                </span>
                <span>
                  <strong>{name}</strong>
                  <small>{note}</small>
                </span>
              </button>
            ))}
          </div>
          <div className="tool-layout">
            <div>
              {mode !== "manual" && !codes.enabled ? (
                <div className="card">
                  <Notice tone="orange">
                    中转码功能当前由管理员关闭。你仍可以用「手动配置中转」。
                  </Notice>
                </div>
              ) : mode === "use" ? (
                <UseCodeForm user={user} settings={settings} linked={linked} />
              ) : mode === "make" ? (
                <MakeCodeForm settings={settings} onNavigate={onNavigate} />
              ) : (
                <ToolForm kind="relay" user={user} settings={settings} />
              )}
            </div>
            <aside className="stack" style={{ alignContent: "start" }}>
              {mode === "use" && (
                <div className="card">
                  <h3>中转码从哪里来？</h3>
                  <p className="muted mt8">
                    中转服务器的拥有者在「生成中转码」里生成后发给你。你不需要对方的
                    SSH，对方也无法使用你的MSBOOST节点。
                  </p>
                  <Button
                    className="mt16"
                    onClick={() => openRelayCodes("used", onNavigate)}
                  >
                    我使用的中转
                  </Button>
                </div>
              )}
              {mode === "make" && (
                <div className="card">
                  <h3>SSH 会保存多久？</h3>
                  <p className="muted mt8">
                    只在中转码有效期内加密暂存，任何页面都不会显示。到期、次数用完或作废后立即删除，之后网站不再保存这台服务器的
                    SSH 信息。
                  </p>
                  <Button
                    className="mt16"
                    onClick={() => openRelayCodes("codes", onNavigate)}
                  >
                    中转码管理
                  </Button>
                </div>
              )}
              {mode === "manual" && (
                <div className="card">
                  <h3>配置文件在哪里？</h3>
                  <p className="muted mt8">
                    操作完成后下载配置，也可以在当前浏览器的任务记录里再次下载。
                  </p>
                  <Button className="mt16" onClick={() => onNavigate("tasks")}>
                    查看任务记录
                  </Button>
                </div>
              )}
              <div className="card">
                <h3>三种方式怎么选？</h3>
                <div className="aside-list">
                  <div>
                    <span>朋友给了你中转码</span>
                    <b>使用中转码</b>
                  </div>
                  <div>
                    <span>服务器线路分享朋友用</span>
                    <b>生成中转码</b>
                  </div>
                  <div>
                    <span>自己有服务器的 SSH</span>
                    <b>手动配置中转</b>
                  </div>
                </div>
              </div>
              <div className="card">
                <h3>操作须知</h3>
                <p className="muted mt8">
                  仅操作自己有权管理的服务器。密码、密钥和配置请妥善保管。
                </p>
              </div>
            </aside>
          </div>
        </>
      )}
    </>
  );
}

function UseCodeForm({
  user,
  settings,
  linked,
}: {
  user: RecordData;
  settings: RecordData;
  linked: string;
}) {
  const [code, setCode] = useState(linked),
    [preview, setPreview] = useState<RecordData | null>(null),
    [previewError, setPreviewError] = useState(""),
    [querying, setQuerying] = useState(false),
    [config, setConfig] = useState<RecordData | null>(null),
    [remark, setRemark] = useState(""),
    [useFront, setUseFront] = useState(false),
    [front, setFront] = useState(newSSH),
    [consent, setConsent] = useState(false),
    [busy, setBusy] = useState(false),
    [error, setError] = useState(""),
    [task, setTask] = useState<RecordData | null>(null);
  const { data: quotas, reload } = useData("/api/tasks");
  async function query(value = code) {
    if (!value.trim()) {
      setPreviewError("请填写中转码");
      return;
    }
    setQuerying(true);
    setPreviewError("");
    setPreview(null);
    try {
      setPreview(await post("/api/relay-codes/preview", { code: value }));
    } catch (e) {
      setPreviewError((e as Error).message);
    } finally {
      setQuerying(false);
    }
  }
  const linkedQueried = useRef(false);
  useEffect(() => {
    // Each lookup counts as an attempt, so a shared link is queried once.
    if (!linked || linkedQueried.current) return;
    linkedQueried.current = true;
    void query(linked);
    // The code is filled in; keep it out of the address bar and history.
    history.replaceState(null, "", location.pathname + location.hash);
  }, []);
  async function submit(e: React.FormEvent) {
    e.preventDefault();
    if (busy) return;
    setError("");
    if (!preview) {
      setError("请先查询中转码");
      return;
    }
    if (!config) {
      setError("请上传节点配置");
      return;
    }
    if (!consent) {
      setError("请先同意在中转服务器上记录你的 QQ 邮箱和节点 IP");
      return;
    }
    setBusy(true);
    try {
      const pinnedFront = useFront
        ? await prepareSSH(front, setFront)
        : undefined;
      const r = await post(
        "/api/relay-codes/redeem",
        {
          code,
          clientConfig: config,
          remark,
          consent: true,
          ...(pinnedFront ? { front: pinnedFront } : {}),
        },
        "POST",
        { "Idempotency-Key": requestID() },
      );
      setTask(r);
      setFront(withoutSecrets(front));
      setConfig(null);
      reload();
      void query(code);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }
  const server = preview ? `中转服务器 ${preview.serverMasked}` : "中转服务器";
  return (
    <div className="card">
      <QuotaBar q={quotas?.limits?.relay} note="与手动配置中转共用次数" />
      <form onSubmit={submit}>
        <div className="step-label">
          <b>1</b>输入中转码
        </div>
        <div className="code-input">
          <label className="sr-only" htmlFor="relay-code">
            中转码
          </label>
          <input
            id="relay-code"
            value={code}
            placeholder="MSB-XXXX-XXXX-XXXX"
            autoComplete="off"
            spellCheck={false}
            maxLength={40}
            onChange={(e) => {
              setCode(e.target.value);
              setPreview(null);
            }}
          />
          <Button disabled={querying} onClick={() => void query()}>
            {querying ? "正在查询…" : "查询"}
          </Button>
        </div>
        <small className="code-help">
          也可以直接打开朋友发来的分享链接，中转码会自动填入。不区分大小写。
        </small>
        <ErrorNotice error={previewError} />
        {preview && (
          <div className="code-preview" aria-live="polite">
            <div className="code-preview-head">
              <h4>
                <span className="server-icon">
                  <Server size={17} />
                </span>
                <span>
                  {server}
                  <small>
                    拥有者 {preview.ownerMasked}
                    {preview.os ? ` · ${preview.os}` : ""}
                  </small>
                </span>
              </h4>
              {preview.usable ? (
                <Badge tone="green">
                  <CircleCheck size={12} />
                  可以使用
                </Badge>
              ) : (
                <Badge tone="orange">次数已被占满，请稍后再试</Badge>
              )}
            </div>
            <div className="preview-grid">
              <div>
                <small>剩余次数</small>
                <strong>
                  {preview.remaining} / {preview.maxUses} 次
                </strong>
              </div>
              <div>
                <small>有效至</small>
                <strong>{fullTime(preview.expiresAt)}</strong>
              </div>
              <div>
                <small>剩余时间</small>
                <strong>
                  <Clock size={13} />
                  {remainingText(preview.expiresAt)}
                </strong>
              </div>
              <div>
                <small>中转带宽</small>
                <strong>上下行各 {preview.rateMbps} Mbps</strong>
              </div>
              <div>
                <small>中转期限</small>
                <strong>长期，不随中转码失效</strong>
              </div>
              <div>
                <small>服务器身份</small>
                <strong>
                  <ShieldCheck size={13} />
                  生成时已核对
                </strong>
              </div>
            </div>
          </div>
        )}
        <div className="step-label">
          <b>2</b>上传节点配置
        </div>
        <ConfigUpload onLoad={setConfig} />
        <Field
          label="配置备注"
          placeholder="例如：首尔中转（将用于配置名称和文件名）"
          value={remark}
          maxLength={60}
          onChange={(e) => setRemark(e.target.value)}
        />
        <Check
          checked={useFront}
          onChange={(e) => setUseFront(e.target.checked)}
          label="使用自备前置机"
        />
        {useFront && (
          <div className="mt16">
            <Notice tone="orange">
              客户电脑 → 自备前置机 → 中转服务器 → MSBOOST
              节点。前置机是你自己的服务器，请填写它的
              SSH；只用于本次任务，不保存。
            </Notice>
            <div className="mt16">
              <SSHFields
                title="前置机 IP 地址"
                label="前置机登录方式"
                value={front}
                onChange={setFront}
              />
            </div>
          </div>
        )}
        <div className="flow">
          {[
            ["客户电脑", ""],
            ...(useFront ? [["自备前置机", ""]] : []),
            [server, "main"],
            ["MSBOOST 节点", ""],
          ].map(([name, tone], i) => (
            <span className="flex" key={name}>
              {i > 0 && <ArrowRight size={13} />}
              <span className={"flow-node " + tone}>{name}</span>
            </span>
          ))}
        </div>
        <Notice>
          {useFront ? "入口使用自备前置机的 IP" : "入口使用中转服务器的 IP"}
          ；监听端口随机分配，带宽按中转码设定。中转建立后长期有效，与手动配置中转相同；中转码到期或用完不影响已建立的中转。
        </Notice>
        <Check
          className="consent"
          checked={consent}
          onChange={(e) => setConsent(e.target.checked)}
          label="我同意在这台中转服务器上记录我的 QQ 邮箱和节点 IP，拥有者可以查看、修改带宽、暂停或删除这条中转"
        />
        <Notice tone="orange">
          本功能仅限游戏用途，禁止用于翻墙、公共代理、违法活动或其他与游戏无关的用途。
        </Notice>
        {useFront && (
          <div className="mt16">
            <Notice tone="orange">
              前置机仅支持 Debian 11、12、13，不支持其他发行版或版本。
            </Notice>
          </div>
        )}
        <ErrorNotice error={error} />
        <div className="form-actions">
          <Button type="submit" primary disabled={busy}>
            {busy ? "正在提交…" : "使用中转码"}
            <ArrowRight size={15} />
          </Button>
        </div>
      </form>
      {task && (
        <TaskDetail
          task={task}
          owner={user.id}
          save={settings.localSave !== false}
          onClose={() => setTask(null)}
        />
      )}
    </div>
  );
}

function MakeCodeForm({
  settings,
  onNavigate,
}: {
  settings: RecordData;
  onNavigate: (view: string) => void;
}) {
  const limits = codeSettings(settings);
  const [ssh, setSSH] = useState(newSSH),
    [uses, setUses] = useState<number | "">(1),
    [ttl, setTtl] = useState<number | "">(Math.min(24, limits.maxTtl)),
    [rate, setRate] = useState(() => rateRange(settings).fallback),
    [note, setNote] = useState(""),
    [ack, setAck] = useState(false),
    [busy, setBusy] = useState(false),
    [checking, setChecking] = useState(false),
    [error, setError] = useState(""),
    [created, setCreated] = useState<RecordData | null>(null);
  const { data: quotas, reload: reloadQuotas } = useData("/api/tasks");
  const { data: codes, reload } = useData("/api/relay-codes");
  const stats = codes?.stats;
  async function submit(e: React.FormEvent) {
    e.preventDefault();
    if (busy) return;
    setError("");
    if (!ack) {
      setError("请先确认网站会在有效期内加密暂存这台服务器的 SSH 信息");
      return;
    }
    setBusy(true);
    try {
      const pinned = await prepareSSH(ssh, setSSH);
      const r = await post(
        "/api/relay-codes",
        {
          ssh: pinned,
          maxUses: Number(uses) || 1,
          ttlHours: Number(ttl) || 1,
          rateMbps: rate,
          note,
          acknowledge: true,
        },
        "POST",
        { "Idempotency-Key": requestID() },
      );
      setSSH(withoutSecrets(ssh));
      setChecking(true);
      const task = await waitForTask(r.task.id);
      if (task.state !== "succeeded")
        throw new Error(
          `${taskFailure(task)} 未生成中转码，也没有保存任何 SSH 信息。`,
        );
      const detail = await api(`/api/relay-codes/${r.code.id}`);
      setCreated(detail.code);
      setNote("");
      setAck(false);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
      setChecking(false);
      reload();
      reloadQuotas();
    }
  }
  const ttlValue = Number(ttl) || 1,
    usesValue = Number(uses) || 1;
  return (
    <div className="card">
      <QuotaBar
        q={quotas?.limits?.relay}
        aside={
          stats
            ? `同时有效的中转码 ${stats.active} / ${stats.maxActive} 个`
            : "中转码到期、用完或作废后释放名额"
        }
      />
      <form onSubmit={submit}>
        <div className="step-label">
          <b>1</b>填写中转服务器
        </div>
        <SSHFields
          title="中转服务器地址"
          value={ssh}
          onChange={setSSH}
          showHelp={false}
          passwordHint="加密暂存到中转码失效为止，失效后立即删除。"
          keyHint="支持 OpenSSH 格式的 ed25519、ECDSA、RSA 私钥，不超过 16 KB。PuTTY 的 .ppk 请先用 PuTTYgen 导出为 OpenSSH 格式。"
          keyNote="私钥只随本次提交加密暂存，不保存到浏览器。"
          keyHelp
          trustNote="“指纹”是服务器的身份标记，不是密码。生成中转码时会固定这台服务器的指纹；之后指纹变化（例如服务器重装），这个中转码会停止使用。验证方式与手动配置中转相同。"
        />
        <Notice>
          提交后先用这组 SSH 登录检查：系统是否为 Debian
          11–13、能否安装中转。检查通过才生成中转码并加密暂存
          SSH；检查不通过不会保存任何信息。
        </Notice>
        <div className="step-label mt24">
          <b>2</b>设置中转码
        </div>
        <div className="form-grid">
          <label className="field">
            <span>可使用次数</span>
            <input
              type="number"
              min={1}
              max={limits.maxUses}
              value={uses}
              onChange={(e) =>
                setUses(clampInput(e.target.value, limits.maxUses))
              }
              onBlur={() => uses === "" && setUses(1)}
              required
            />
            <small>{`1–${limits.maxUses} 次；中转建立成功才计次。`}</small>
          </label>
          <div className="field">
            <label htmlFor="relay-code-ttl">有效期</label>
            <div className="input-group">
              <input
                id="relay-code-ttl"
                type="number"
                min={1}
                max={limits.maxTtl}
                value={ttl}
                onChange={(e) =>
                  setTtl(clampInput(e.target.value, limits.maxTtl))
                }
                onBlur={() => ttl === "" && setTtl(1)}
                required
              />
              <span className="muted">小时</span>
            </div>
            <div className="chips mt8" role="group" aria-label="常用有效期">
              {[12, 24, 48, 72]
                .filter((hours) => hours <= limits.maxTtl)
                .map((hours) => (
                  <button
                    type="button"
                    key={hours}
                    className={ttl === hours ? "active" : ""}
                    onClick={() => setTtl(hours)}
                  >
                    {hours} 小时
                  </button>
                ))}
            </div>
            <small>{`最短 1 小时，最长 ${limits.maxTtl} 小时。`}</small>
          </div>
          <RateSelect
            label="中转带宽"
            value={rate}
            onChange={setRate}
            settings={settings}
            hint="每条中转的上行、下行各自限速。"
          />
          <Field
            label="备注（只有你自己可见）"
            maxLength={40}
            placeholder="例如：给小明、公会群"
            value={note}
            onChange={(e) => setNote(e.target.value)}
          />
        </div>
        <div className="vault-note">
          <KeyRound size={17} />
          <div>
            <strong>{`SSH 最多暂存 ${ttlValue} 小时，或使用 ${usesValue} 次后自动删除（以先到者为准）`}</strong>
            <small>
              你也可以随时在「中转码管理」作废，SSH
              会立即删除。删除后网站不再保存这台服务器的任何 SSH
              信息，已建立的中转继续长期运行。
            </small>
          </div>
        </div>
        <Check
          className="consent"
          checked={ack}
          onChange={(e) => setAck(e.target.checked)}
          label="我知道网站会在有效期内加密暂存这台服务器的 SSH 信息，到期或次数用完后自动删除"
        />
        <Notice tone="orange">
          中转码只发给你信任的人。使用者建立的中转长期有效，需要时到「中转码管理
          → 中转服务器管理」修改带宽、暂停或删除。
        </Notice>
        <div className="mt16">
          <Notice tone="orange">
            仅支持 Debian 11、12、13，不支持其他发行版或版本。
          </Notice>
        </div>
        <ErrorNotice error={error} />
        {stats && stats.active >= stats.maxActive && (
          <div className="mt16">
            <Notice tone="orange">
              {`同时有效的中转码最多 ${stats.maxActive} 个。作废不再使用的中转码，或等它们到期后再生成。`}{" "}
              <button
                type="button"
                className="btn link"
                onClick={() => openRelayCodes("codes", onNavigate)}
              >
                中转码管理
              </button>
            </Notice>
          </div>
        )}
        <div className="form-actions">
          <Button type="submit" primary disabled={busy}>
            {checking
              ? "正在检查服务器…"
              : busy
                ? "正在提交…"
                : "检查服务器并生成"}
            <ArrowRight size={15} />
          </Button>
        </div>
      </form>
      {created && (
        <CreatedCodeModal code={created} onClose={() => setCreated(null)} />
      )}
    </div>
  );
}

function CreatedCodeModal({
  code,
  onClose,
}: {
  code: RecordData;
  onClose: () => void;
}) {
  const text = shareText(code);
  return (
    <Modal title="中转码已生成" onClose={onClose}>
      <Notice tone="green">
        {`检查通过：${code.os || "Debian"}，服务器身份已固定。SSH 已加密暂存。`}
      </Notice>
      <div className="code-hero mt16">
        <span>{code.code}</span>
        <CopyButton value={code.code} label="复制" className="soft" />
      </div>
      <div className="detail-grid">
        <div className="kv">
          <span>可使用次数</span>
          <strong>{code.maxUses} 次</strong>
        </div>
        <div className="kv">
          <span>中转带宽</span>
          <strong>{code.rateMbps} Mbps</strong>
        </div>
        <div className="kv">
          <span>SSH 删除时间</span>
          <strong>{shortTime(code.expiresAt)} 或用完后</strong>
        </div>
      </div>
      <span className="field-label">分享链接</span>
      <div className="copy-row">
        <input
          className="copy-input"
          readOnly
          value={shareLink(code.code)}
          aria-label="分享链接"
        />
        <CopyButton value={shareLink(code.code)} label="复制" small={false} />
      </div>
      <span className="field-label">分享文案</span>
      <textarea
        className="share-text"
        readOnly
        value={text}
        aria-label="分享文案"
      />
      <div className="mt16">
        <Notice>
          之后可以在「中转码管理」再次查看和复制。请只发给你信任的人。
        </Notice>
      </div>
      <div className="form-actions">
        <Button onClick={onClose}>完成</Button>
        <CopyButton
          value={text}
          label="复制分享文案"
          className="primary"
          small={false}
        />
      </div>
    </Modal>
  );
}

// ===== 中转码管理 =====

export function RelayCodesPage({
  user,
  settings,
  onNavigate,
}: {
  user: RecordData;
  settings: RecordData;
  onNavigate: (view: string) => void;
}) {
  const { data, error, loading, reload } = useData("/api/relay-codes");
  const used = useData("/api/relay-codes/used");
  const [tab, setTab] = useState<CodesTab>(() => requestedTab || "codes");
  const [managed, setManaged] = useState(() => requestedCodeID);
  useEffect(() => {
    requestedTab = null;
    requestedCodeID = "";
  }, []);
  const [detail, setDetail] = useState<RecordData | null>(null),
    [adjust, setAdjust] = useState<RecordData | null>(null),
    [revoke, setRevoke] = useState<RecordData | null>(null);
  const codes = array(data, "codes"),
    relays = array(used.data, "relays");
  const stats = data?.stats || {};
  return (
    <>
      <Header
        title="中转码管理"
        sub="管理你生成的中转码；读取中转服务器上的全部中转，按使用者修改带宽、暂停或删除。"
      >
        <div className="actions">
          <Button primary onClick={() => openRelayMode("make", onNavigate)}>
            <Plus size={15} />
            生成中转码
          </Button>
        </div>
      </Header>
      <ErrorNotice error={error} />
      {data?.disabled && (
        <div className="mb16">
          <Notice tone="red">
            你的中转码功能已被管理员停用，不能生成或使用中转码；已建立的中转不受影响。
          </Notice>
        </div>
      )}
      <div className="grid4">
        <div className="card stat">
          <div className="stat-top">
            有效中转码
            <span className="stat-icon">
              <Ticket size={15} />
            </span>
          </div>
          <div className="stat-value">
            {stats.active ?? "—"}
            <span>个</span>
          </div>
          <small>上限 {stats.maxActive ?? 5} 个</small>
        </div>
        <div className="card stat">
          <div className="stat-top">
            暂存的 SSH
            <span className="stat-icon">
              <KeyRound size={15} />
            </span>
          </div>
          <div className="stat-value">
            {stats.secrets ?? "—"}
            <span>份</span>
          </div>
          <small>
            {stats.earliestDeletion
              ? `最早 ${remainingText(stats.earliestDeletion)}后删除`
              : "没有暂存的 SSH"}
          </small>
        </div>
        <div className="card stat">
          <div className="stat-top">
            通过中转码建立
            <span className="stat-icon">
              <Route size={15} />
            </span>
          </div>
          <div className="stat-value">
            {stats.relays ?? "—"}
            <span>条</span>
          </div>
          <small>
            {stats.failures
              ? `另有 ${stats.failures} 次失败，不计次`
              : "失败的不计次"}
          </small>
        </div>
        <div className="card stat">
          <div className="stat-top">
            我使用的中转
            <span className="stat-icon">
              <ArrowLeftRight size={15} />
            </span>
          </div>
          <div className="stat-value">
            {stats.used ?? "—"}
            <span>条</span>
          </div>
          <small>来自别人的中转码</small>
        </div>
      </div>
      <div className="tabs mt24" role="tablist">
        {(
          [
            ["codes", `我的中转码（${codes.length}）`],
            ["servers", "中转服务器管理"],
            ["used", `我使用的中转（${relays.length}）`],
          ] as [CodesTab, string][]
        ).map(([key, name]) => (
          <button
            type="button"
            role="tab"
            key={key}
            aria-selected={tab === key}
            className={tab === key ? "active" : ""}
            onClick={() => setTab(key)}
          >
            {name}
          </button>
        ))}
      </div>
      {tab === "codes" && (
        <MyCodes
          codes={codes}
          loading={loading}
          onDetail={setDetail}
          onAdjust={setAdjust}
          onRevoke={setRevoke}
        />
      )}
      {tab === "servers" && (
        <ServerManager
          codes={codes}
          settings={settings}
          initialCodeId={managed}
          manageLimit={data?.manageLimit}
          onUsed={reload}
        />
      )}
      {tab === "used" && (
        <UsedRelays
          relays={relays}
          loading={used.loading}
          error={used.error}
          user={user}
          onNavigate={onNavigate}
        />
      )}
      {detail && (
        <CodeDetail
          id={detail.id}
          onClose={() => setDetail(null)}
          onAdjust={(code) => {
            setDetail(null);
            setAdjust(code);
          }}
          onRevoke={(code) => {
            setDetail(null);
            setRevoke(code);
          }}
          onManage={(code) => {
            setDetail(null);
            setManaged(code.id);
            setTab("servers");
          }}
        />
      )}
      {adjust && (
        <AdjustCode
          code={adjust}
          settings={settings}
          onClose={() => setAdjust(null)}
          onDone={() => {
            setAdjust(null);
            reload();
          }}
        />
      )}
      {revoke && (
        <RevokeCode
          code={revoke}
          onClose={() => {
            setRevoke(null);
            reload();
          }}
        />
      )}
    </>
  );
}

function CodeCell({ code }: { code: RecordData }) {
  const [shown, setShown] = useState(false);
  return (
    <span className="code-cell">
      <span className="code-text">
        {shown && code.code ? code.code : code.masked}
      </span>
      {code.code && (
        <button
          type="button"
          className="icon-btn"
          title={shown ? "隐藏中转码" : "显示中转码"}
          aria-label={shown ? "隐藏中转码" : "显示中转码"}
          onClick={() => setShown(!shown)}
        >
          {shown ? <EyeOff size={14} /> : <Eye size={14} />}
        </button>
      )}
      {code.code && code.state === "active" && (
        <>
          <button
            type="button"
            className="icon-btn"
            title="复制中转码"
            aria-label="复制中转码"
            onClick={() => void copyText(code.code)}
          >
            <Copy size={14} />
          </button>
          <button
            type="button"
            className="icon-btn"
            title="复制分享链接"
            aria-label="复制分享链接"
            onClick={() => void copyText(shareLink(code.code))}
          >
            <Link2 size={14} />
          </button>
        </>
      )}
    </span>
  );
}

function SecretState({ code }: { code: RecordData }) {
  if (code.secretHeld)
    return (
      <>
        <span className="ssh-state held">
          <KeyRound size={13} />
          暂存中 · {code.authMethod === "key" ? "密钥" : "密码"}
        </span>
        <small>到期或用完即删除</small>
      </>
    );
  if (code.state === "checking")
    return <span className="ssh-state gone">正在检查</span>;
  return (
    <>
      <span className="ssh-state gone">已删除</span>
      {code.secretDeletedAt > 0 && (
        <small>
          {ownerReasons[code.deleteReason] || "已删除"} ·{" "}
          {shortTime(code.secretDeletedAt)}
        </small>
      )}
    </>
  );
}

function MyCodes({
  codes,
  loading,
  onDetail,
  onAdjust,
  onRevoke,
}: {
  codes: RecordData[];
  loading: boolean;
  onDetail: (c: RecordData) => void;
  onAdjust: (c: RecordData) => void;
  onRevoke: (c: RecordData) => void;
}) {
  const [search, setSearch] = useState(""),
    [state, setState] = useState("");
  const rows = codes.filter(
    (code) =>
      (!state || code.state === state) &&
      (!search.trim() ||
        `${code.note} ${code.code || ""} ${code.tail} ${code.serverHost}`
          .toLowerCase()
          .includes(search.trim().toLowerCase())),
  );
  return (
    <>
      <div className="filterbar">
        <input
          type="search"
          placeholder="搜索备注或中转码"
          aria-label="搜索备注或中转码"
          value={search}
          onChange={(e) => setSearch(e.target.value)}
        />
        <select
          aria-label="按状态筛选"
          value={state}
          onChange={(e) => setState(e.target.value)}
        >
          <option value="">全部状态</option>
          <option value="active">有效</option>
          <option value="exhausted">已用完</option>
          <option value="expired">已过期</option>
          <option value="revoked">已作废</option>
        </select>
      </div>
      <div className="mb16">
        <Notice>
          中转码到期、次数用完或作废时，暂存的 SSH
          立即删除，中转码随即失效；已建立的中转继续工作。管理这些中转请到「中转服务器管理」。
        </Notice>
      </div>
      <div className="card flush">
        {!loading && !codes.length ? (
          <Empty>还没有中转码。在「配置中转服务器 → 生成中转码」里生成。</Empty>
        ) : (
          <Table
            loading={loading}
            headers={[
              "中转码",
              "备注 / 服务器",
              "次数",
              "剩余时间",
              "带宽",
              "SSH 暂存",
              "状态",
              "操作",
            ]}
            rows={rows.map((code) => [
              <CodeCell code={code} />,
              <>
                {code.note || "—"}
                <small className="code-text">{code.serverHost}</small>
              </>,
              <div className="usage">
                <div className="progress">
                  <span
                    style={{
                      width: `${Math.min(100, (code.usedCount / Math.max(1, code.maxUses)) * 100)}%`,
                    }}
                  />
                </div>
                <small>
                  {code.usedCount} / {code.maxUses} 次
                </small>
              </div>,
              code.state === "active" ? (
                <>
                  {remainingText(code.expiresAt)}
                  <small>至 {shortTime(code.expiresAt)}</small>
                </>
              ) : (
                "—"
              ),
              `${code.rateMbps} Mbps`,
              <SecretState code={code} />,
              <Badge tone={stateLabels[code.state]?.[1] || ""}>
                {stateLabels[code.state]?.[0] || code.state}
              </Badge>,
              <div className="actions">
                <Button className="small" onClick={() => onDetail(code)}>
                  详情
                </Button>
                {code.secretHeld && (
                  <>
                    <Button className="small" onClick={() => onAdjust(code)}>
                      调整
                    </Button>
                    <Button
                      className="small danger"
                      onClick={() => onRevoke(code)}
                    >
                      作废
                    </Button>
                  </>
                )}
              </div>,
            ])}
          />
        )}
      </div>
    </>
  );
}

function CodeDetail({
  id,
  onClose,
  onAdjust,
  onRevoke,
  onManage,
}: {
  id: string;
  onClose: () => void;
  onAdjust: (c: RecordData) => void;
  onRevoke: (c: RecordData) => void;
  onManage: (c: RecordData) => void;
}) {
  const { data, error } = useData(`/api/relay-codes/${id}`);
  const code = data?.code,
    uses = array(data, "uses");
  return (
    <Modal title="中转码详情" onClose={onClose}>
      <ErrorNotice error={error} />
      {code && (
        <>
          <div className="detail-head">
            <div className="flex wrap">
              <span className="code-text">{code.code || code.masked}</span>
              <Badge tone={stateLabels[code.state]?.[1] || ""}>
                {stateLabels[code.state]?.[0] || code.state}
              </Badge>
              {code.secretHeld && (
                <Badge tone="green">
                  <KeyRound size={12} />
                  SSH 暂存中
                </Badge>
              )}
            </div>
            <div className="actions">
              {code.code && <CopyButton value={code.code} label="复制中转码" />}
              {code.code && code.state === "active" && (
                <CopyButton
                  value={shareLink(code.code)}
                  label="复制链接"
                  icon="link"
                />
              )}
              {code.secretHeld && (
                <>
                  <Button className="small" onClick={() => onAdjust(code)}>
                    调整
                  </Button>
                  <Button
                    className="small danger"
                    onClick={() => onRevoke(code)}
                  >
                    作废
                  </Button>
                </>
              )}
            </div>
          </div>
          <div className="detail-grid">
            <div className="kv">
              <span>中转服务器</span>
              <strong>
                {code.serverHost}
                {code.os ? ` · ${code.os}` : ""}
              </strong>
            </div>
            <div className="kv">
              <span>服务器指纹</span>
              <strong className="mono">
                {code.fingerprint.slice(0, 11)}…{code.fingerprint.slice(-4)}
              </strong>
            </div>
            <div className="kv">
              <span>登录方式</span>
              <strong>
                {code.authMethod === "key"
                  ? `密钥${code.keyType ? `（${code.keyType}）` : ""}`
                  : "密码"}
              </strong>
            </div>
            <div className="kv">
              <span>次数</span>
              <strong>
                已用 {code.usedCount} / {code.maxUses} 次
              </strong>
            </div>
            <div className="kv">
              <span>中转带宽</span>
              <strong>上下行各 {code.rateMbps} Mbps</strong>
            </div>
            <div className="kv">
              <span>备注</span>
              <strong>{code.note || "—"}</strong>
            </div>
            <div className="kv">
              <span>生成时间</span>
              <strong>{fullTime(code.createdAt)}</strong>
            </div>
            <div className="kv">
              <span>有效至</span>
              <strong>{fullTime(code.expiresAt)}</strong>
            </div>
            <div className="kv">
              <span>SSH 删除</span>
              <strong>
                {code.secretHeld
                  ? `最迟 ${shortTime(code.expiresAt)}`
                  : code.secretDeletedAt
                    ? `${ownerReasons[code.deleteReason] || "已删除"} · ${shortTime(code.secretDeletedAt)}`
                    : "—"}
              </strong>
            </div>
          </div>
          <div className="section-title">
            <h3>使用记录（{uses.length}）</h3>
            <Button className="small" onClick={() => onManage(code)}>
              <Server size={14} />
              管理这些中转
            </Button>
          </div>
          <div className="card flush">
            <Table
              headers={["使用者", "中转端口", "结果", "时间"]}
              rows={uses.map((use) => [
                <span className="user-cell">
                  <b>{use.userEmail}</b>
                  <span>（{use.maskedIp}）</span>
                </span>,
                use.listenPort ? (
                  <span className="code-text">{use.listenPort}</span>
                ) : (
                  "—"
                ),
                use.result === "succeeded" ? (
                  <Badge tone="green">成功</Badge>
                ) : use.result === "pending" ? (
                  <Badge tone="orange">建立中</Badge>
                ) : (
                  <>
                    <Badge tone="red">失败</Badge>
                    <small>{use.failReason || "建立失败"} · 不计次</small>
                  </>
                ),
                fullTime(use.createdAt),
              ])}
            />
          </div>
          <p className="muted mt16">
            这里只记录建立时的结果。中转当前是否运行、有几个连接，请到「中转服务器管理」读取。
          </p>
        </>
      )}
    </Modal>
  );
}

function AdjustCode({
  code,
  settings,
  onClose,
  onDone,
}: {
  code: RecordData;
  settings: RecordData;
  onClose: () => void;
  onDone: () => void;
}) {
  const limits = codeSettings(settings);
  const room = Math.max(0, code.maxExpiresAt - code.expiresAt);
  const [extend, setExtend] = useState(0),
    [uses, setUses] = useState<number | "">(code.maxUses),
    [note, setNote] = useState(code.note || ""),
    [busy, setBusy] = useState(false),
    [error, setError] = useState("");
  const toLimit = Math.ceil(room / 3600000);
  return (
    <Modal title="调整中转码" onClose={onClose}>
      <form
        onSubmit={async (e) => {
          e.preventDefault();
          if (busy) return;
          setBusy(true);
          setError("");
          try {
            await post(
              `/api/relay-codes/${code.id}`,
              {
                extendHours: extend,
                maxUses: Math.max(code.maxUses, Number(uses) || code.maxUses),
                note,
              },
              "PATCH",
            );
            onDone();
          } catch (err) {
            setError((err as Error).message);
          } finally {
            setBusy(false);
          }
        }}
      >
        <div className="kv">
          <span>中转码</span>
          <strong className="code-text">{code.masked}</strong>
        </div>
        <div className="kv">
          <span>已用次数</span>
          <strong>{code.usedCount} 次</strong>
        </div>
        <div className="kv">
          <span>当前有效至</span>
          <strong>{fullTime(code.expiresAt)}</strong>
        </div>
        <div className="form-grid mt16">
          <Select
            label="有效期"
            value={extend}
            onChange={(e) => setExtend(Number(e.target.value))}
          >
            <option value={0}>保持不变</option>
            {room > 0 && <option value={12}>延长 12 小时</option>}
            {room > 0 && <option value={24}>延长 24 小时</option>}
            {room > 0 && (
              <option
                value={toLimit}
              >{`延长到上限（${shortTime(code.maxExpiresAt)}）`}</option>
            )}
          </Select>
          <label className="field">
            <span>可使用次数</span>
            <input
              type="number"
              min={code.maxUses}
              max={limits.maxUses}
              value={uses}
              onChange={(e) =>
                setUses(clampInput(e.target.value, limits.maxUses))
              }
              onBlur={() =>
                setUses(Math.max(code.maxUses, Number(uses) || code.maxUses))
              }
            />
            <small>{`只能增加，不能少于已用次数，最多 ${limits.maxUses} 次。`}</small>
          </label>
          <label className="field span-all">
            <span>备注</span>
            <input
              value={note}
              maxLength={40}
              onChange={(e) => setNote(e.target.value)}
            />
          </label>
        </div>
        <p className="muted">{`从生成时算起总长不超过 ${limits.maxTtl} 小时。`}</p>
        <div className="mt16">
          <Notice>
            只有 SSH
            仍在暂存期的中转码可以调整。中转服务器、登录信息和带宽不能修改；需要更换时请作废后重新生成。
          </Notice>
        </div>
        <ErrorNotice error={error} />
        <div className="form-actions">
          <Button onClick={onClose}>取消</Button>
          <Button type="submit" primary disabled={busy}>
            {busy ? "正在保存…" : "保存"}
          </Button>
        </div>
      </form>
    </Modal>
  );
}

function RevokeCode({
  code,
  onClose,
}: {
  code: RecordData;
  onClose: () => void;
}) {
  const [remove, setRemove] = useState(false),
    [confirmed, setConfirmed] = useState(false),
    [busy, setBusy] = useState(false),
    [error, setError] = useState(""),
    [result, setResult] = useState("");
  const relays = Number(code.relays) || 0;
  async function submit() {
    if (busy || !confirmed) return;
    setBusy(true);
    setError("");
    try {
      const r = await post(`/api/relay-codes/${code.id}/revoke`, {
        deleteRelays: remove && relays > 0,
      });
      if (r.warning) {
        setResult(r.warning);
        return;
      }
      if (!r.task) {
        setResult("中转码已作废，暂存的 SSH 已删除。");
        return;
      }
      setResult("中转码已作废，暂存的 SSH 已删除，正在删除已建立的中转…");
      const task = await waitForTask(r.task.id);
      if (task.state !== "succeeded") {
        setResult(
          `中转码已作废，暂存的 SSH 已删除；但删除中转没有完成：${taskFailure(task)} 需要时请在「中转服务器管理」手动输入 SSH 处理。`,
        );
        return;
      }
      const report = await api(`/api/relay-servers/results/${r.task.id}`);
      const failed = array(report, "managed").filter((item) => !item.ok);
      setResult(
        failed.length
          ? `中转码已作废，暂存的 SSH 已删除。有 ${failed.length} 条中转没有删除成功（${failed
              .map((item) => manageErrors[item.error] || "操作失败")
              .join("、")}），需要时请在「中转服务器管理」手动输入 SSH 处理。`
          : `中转码已作废，暂存的 SSH 已删除，${array(report, "managed").length} 条中转已删除。`,
      );
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }
  return (
    <Modal title="作废中转码" onClose={onClose}>
      {result ? (
        <>
          <Notice tone={result.includes("没有") ? "orange" : "green"}>
            {result}
          </Notice>
          <div className="form-actions">
            <Button primary onClick={onClose}>
              完成
            </Button>
          </div>
        </>
      ) : (
        <>
          <Notice tone="red">
            作废后中转码立即失效，暂存的 SSH 立即删除，无法恢复。
          </Notice>
          <div className="mt16">
            <div className="kv">
              <span>中转码</span>
              <strong className="code-text">{code.masked}</strong>
            </div>
            <div className="kv">
              <span>备注</span>
              <strong>{code.note || "—"}</strong>
            </div>
            <div className="kv">
              <span>通过它建立的中转</span>
              <strong>{relays} 条</strong>
            </div>
          </div>
          {relays > 0 && (
            <div className="radio-cards mt16">
              <label className="radio-card">
                <input
                  type="radio"
                  name="revoke-mode"
                  checked={!remove}
                  onChange={() => setRemove(false)}
                />
                <span>
                  <strong>{`保留已建立的 ${relays} 条中转`}</strong>
                  <br />
                  <small className="muted">
                    使用者不受影响；之后需要时，在「中转服务器管理」输入 SSH
                    删除。
                  </small>
                </span>
              </label>
              <label className="radio-card">
                <input
                  type="radio"
                  name="revoke-mode"
                  checked={remove}
                  onChange={() => setRemove(true)}
                />
                <span>
                  <strong>{`同时删除这 ${relays} 条中转`}</strong>
                  <br />
                  <small className="muted">
                    先用暂存的 SSH 删除这些中转，再删除
                    SSH；使用者的中转配置随即失效。
                  </small>
                </span>
              </label>
            </div>
          )}
          <Check
            className="mt16"
            checked={confirmed}
            onChange={(e) => setConfirmed(e.target.checked)}
            label="我已了解作废的影响"
          />
          <ErrorNotice error={error} />
          <div className="form-actions">
            <Button onClick={onClose}>取消</Button>
            <Button
              className="danger"
              disabled={!confirmed || busy}
              onClick={() => void submit()}
            >
              <Trash2 size={14} />
              {busy ? "正在作废…" : "作废中转码"}
            </Button>
          </div>
        </>
      )}
    </Modal>
  );
}

const sourceNames: Record<string, string> = {
  manual: "手动配置",
  legacy: "升级前配置",
  route: "捐赠权益前置",
};

function ServerManager({
  codes,
  settings,
  initialCodeId,
  manageLimit,
  onUsed,
}: {
  codes: RecordData[];
  settings: RecordData;
  initialCodeId: string;
  manageLimit?: RecordData;
  onUsed: () => void;
}) {
  const limits = codeSettings(settings);
  const held = codes.filter((code) => code.secretHeld);
  const storedAllowed = limits.manageWithSecret && limits.enabled;
  const [source, setSource] = useState<"stored" | "input">(
    storedAllowed && held.length ? "stored" : "input",
  );
  const [codeId, setCodeId] = useState(
    held.some((code) => code.id === initialCodeId)
      ? initialCodeId
      : held[0]?.id || "",
  );
  const [ssh, setSSH] = useState<SSH>(newSSH),
    [prepared, setPrepared] = useState<{ ssh: SSH; at: number } | null>(null),
    [connection, setConnection] = useState<RecordData | null>(null),
    [rows, setRows] = useState<RecordData[]>([]),
    [showConnect, setShowConnect] = useState(true),
    [connecting, setConnecting] = useState(false),
    [working, setWorking] = useState(false),
    [search, setSearch] = useState(""),
    [sourceFilter, setSourceFilter] = useState(""),
    [stateFilter, setStateFilter] = useState(""),
    [selected, setSelected] = useState<string[]>([]),
    [dialog, setDialog] = useState<{
      kind: "rate" | "delete";
      items: RecordData[];
    } | null>(null),
    [message, setMessage] = useState(""),
    [error, setError] = useState("");
  // Typed SSH stays only in this page's memory. A fingerprint check is valid
  // for 30 minutes, so it is reused instead of repeated for every action.
  async function target(next = connection): Promise<RecordData> {
    const useStored = next ? next.source === "stored" : source === "stored";
    if (useStored) return { codeId: next ? next.codeId : codeId };
    if (prepared && Date.now() - prepared.at < 25 * 60000)
      return { ssh: prepared.ssh };
    const value = await prepareSSH(next?.ssh || ssh, setSSH);
    setPrepared({ ssh: value, at: Date.now() });
    return { ssh: value };
  }
  async function read(next?: RecordData) {
    const plan =
      next ||
      (source === "stored"
        ? (() => {
            const code = held.find((item) => item.id === codeId);
            if (!code) throw new Error("请选择 SSH 仍在暂存期的中转码");
            return {
              source,
              codeId,
              host: code.serverHost,
              label: `通过中转码 …${code.tail} 暂存的 SSH`,
            };
          })()
        : { source, ssh, host: ssh.host, label: "手动输入的 SSH" });
    const payload = await target(plan);
    const task = await post("/api/relay-servers/inventory", payload);
    const done = await waitForTask(task.id);
    if (done.state !== "succeeded") throw new Error(taskFailure(done));
    const result = await api(`/api/relay-servers/results/${task.id}`);
    setRows(array(result, "relays"));
    setConnection({ ...plan, os: result.os, at: Date.now() });
    setSelected([]);
    setShowConnect(false);
  }
  async function connect() {
    if (connecting) return;
    setConnecting(true);
    setError("");
    setMessage("");
    try {
      setPrepared(null);
      await read();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setConnecting(false);
      onUsed();
    }
  }
  async function act(action: string, items: RecordData[], rate?: number) {
    if (working || !connection) return;
    setWorking(true);
    setError("");
    setMessage("");
    try {
      const task = await post("/api/relay-servers/actions", {
        ...(await target()),
        action,
        items: items.map((item) => item.id),
        ...(action === "rate" ? { rateMbps: rate } : {}),
      });
      const done = await waitForTask(task.id);
      if (done.state !== "succeeded") throw new Error(taskFailure(done));
      const report = array(
        await api(`/api/relay-servers/results/${task.id}`),
        "managed",
      );
      const failed = report.filter((item) => !item.ok);
      setMessage(
        `${actionNames[action]}：成功 ${report.length - failed.length} 条` +
          (failed.length
            ? `，失败 ${failed.length} 条（${failed.map((item) => `端口 ${rows.find((row) => row.id === item.id)?.port ?? "?"} ${manageErrors[item.error] || "操作失败"}`).join("；")}）`
            : ""),
      );
      setDialog(null);
      await read(connection);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setWorking(false);
      onUsed();
    }
  }
  const visible = rows.filter(
    (row) =>
      (!sourceFilter || row.source === sourceFilter) &&
      (!stateFilter || row.state === stateFilter) &&
      (!search.trim() ||
        `${row.user || ""} ${row.maskedIp} ${row.remark || ""} ${row.port}`
          .toLowerCase()
          .includes(search.trim().toLowerCase())),
  );
  const chosen = rows.filter((row) => selected.includes(row.id));
  const count = (state: string) =>
    rows.filter((row) => row.state === state).length;
  return (
    <>
      {showConnect || !connection ? (
        <div className="card connect-card">
          <h3>连接中转服务器</h3>
          <p className="muted mt8">
            读取这台服务器上由 MSBOOST 安装的全部中转（包括手动配置的）。SSH
            只保存在当前页面，关闭页面即清除，网站不保存。
          </p>
          <span className="field-label">SSH 来源</span>
          <div className="segmented full" role="tablist" aria-label="SSH 来源">
            <button
              type="button"
              role="tab"
              aria-selected={source === "stored"}
              className={source === "stored" ? "active" : ""}
              disabled={!storedAllowed}
              onClick={() => setSource("stored")}
            >
              <KeyRound size={14} />
              使用中转码暂存的 SSH
            </button>
            <button
              type="button"
              role="tab"
              aria-selected={source === "input"}
              className={source === "input" ? "active" : ""}
              onClick={() => setSource("input")}
            >
              <Server size={14} />
              输入 SSH
            </button>
          </div>
          {source === "stored" ? (
            <div className="mt16">
              {held.length ? (
                <Select
                  label="选择中转码"
                  value={codeId}
                  onChange={(e) => setCodeId(e.target.value)}
                >
                  {held.map((code) => (
                    <option key={code.id} value={code.id}>
                      {`${code.serverHost} · 中转码 …${code.tail}${code.note ? `「${code.note}」` : ""} · SSH 还剩 ${remainingText(code.expiresAt)}`}
                    </option>
                  ))}
                </Select>
              ) : (
                <Notice>
                  没有 SSH 仍在暂存期的中转码，请改用「输入 SSH」。
                </Notice>
              )}
              <small className="muted">
                只列出 SSH 仍在暂存期的中转码；读取和管理不消耗中转码次数。
              </small>
            </div>
          ) : (
            <div className="mt16">
              <SSHFields
                title="中转服务器地址"
                value={ssh}
                onChange={(next) => {
                  setSSH(next);
                  setPrepared(null);
                }}
                showHelp={false}
                passwordHint=""
                keyHint=""
              />
            </div>
          )}
          {manageLimit && (
            <p className="muted mt16">
              {`中转服务器管理单独限额：每 ${manageLimit.minutes} 分钟最多 ${manageLimit.count} 次 · 剩余 ${manageLimit.remaining} 次`}
            </p>
          )}
          <ErrorNotice error={error} />
          <div className="form-actions">
            {connection && (
              <Button onClick={() => setShowConnect(false)}>取消</Button>
            )}
            <Button
              primary
              disabled={connecting || (source === "stored" && !held.length)}
              onClick={() => void connect()}
            >
              <RefreshCw size={15} />
              {connecting ? "正在读取…" : "读取中转列表"}
            </Button>
          </div>
        </div>
      ) : (
        <>
          <div className="conn-bar">
            <div className="server-title">
              <span className="server-icon">
                <Server size={17} />
              </span>
              <div>
                <h3>
                  {connection.host}
                  <span className="status online">
                    <span className="dot" />
                    已连接
                  </span>
                </h3>
                <small>
                  {[
                    connection.os,
                    connection.label,
                    `读取于 ${shortTime(connection.at).slice(6)}`,
                  ]
                    .filter(Boolean)
                    .join(" · ")}
                </small>
              </div>
            </div>
            <div className="actions">
              <Button
                className="small"
                disabled={working || connecting}
                onClick={async () => {
                  setConnecting(true);
                  setError("");
                  try {
                    await read(connection);
                  } catch (e) {
                    setError((e as Error).message);
                  } finally {
                    setConnecting(false);
                    onUsed();
                  }
                }}
              >
                <RotateCw size={14} />
                {connecting ? "正在读取…" : "刷新"}
              </Button>
              <Button className="small" onClick={() => setShowConnect(true)}>
                <ArrowLeftRight size={14} />
                更换服务器
              </Button>
            </div>
          </div>
          <div className="server-meta inv-meta">
            <div>
              <small>中转</small>
              <strong>{rows.length} 条</strong>
            </div>
            <div>
              <small>运行中</small>
              <strong>{count("running")} 条</strong>
            </div>
            <div>
              <small>已暂停 / 异常</small>
              <strong>
                {count("paused")} / {count("stopped")} 条
              </strong>
            </div>
            <div>
              <small>当前连接</small>
              <strong>
                {rows.reduce(
                  (sum, row) => sum + (Number(row.connections) || 0),
                  0,
                )}{" "}
                个
              </strong>
            </div>
          </div>
          <div className="filterbar mt16">
            <input
              type="search"
              placeholder="搜索邮箱、IP 或备注"
              aria-label="搜索邮箱、IP 或备注"
              value={search}
              onChange={(e) => setSearch(e.target.value)}
            />
            <select
              aria-label="按来源筛选"
              value={sourceFilter}
              onChange={(e) => setSourceFilter(e.target.value)}
            >
              <option value="">全部来源</option>
              <option value="code">中转码</option>
              <option value="manual">手动配置</option>
              <option value="legacy">升级前配置</option>
            </select>
            <select
              aria-label="按状态筛选"
              value={stateFilter}
              onChange={(e) => setStateFilter(e.target.value)}
            >
              <option value="">全部状态</option>
              <option value="running">运行中</option>
              <option value="paused">已暂停</option>
              <option value="stopped">异常</option>
            </select>
          </div>
          {message && (
            <div className="mb16">
              <Notice tone={message.includes("失败") ? "orange" : "green"}>
                {message}
              </Notice>
            </div>
          )}
          <ErrorNotice error={error} />
          {chosen.length > 0 && (
            <div className="batch-bar">
              <span>
                已选 <b>{chosen.length}</b> 条中转
              </span>
              <div className="actions">
                <Button
                  className="small"
                  disabled={working}
                  onClick={() => setDialog({ kind: "rate", items: chosen })}
                >
                  <Gauge size={14} />
                  修改带宽
                </Button>
                <Button
                  className="small"
                  disabled={working}
                  onClick={() => void act("pause", chosen)}
                >
                  <Pause size={14} />
                  暂停
                </Button>
                <Button
                  className="small"
                  disabled={working}
                  onClick={() => void act("resume", chosen)}
                >
                  <Play size={14} />
                  恢复
                </Button>
                <Button
                  className="small danger"
                  disabled={working}
                  onClick={() => setDialog({ kind: "delete", items: chosen })}
                >
                  <Trash2 size={14} />
                  删除
                </Button>
              </div>
            </div>
          )}
          <div className="card flush">
            {!rows.length ? (
              <Empty>这台服务器上没有由 MSBOOST 安装的中转。</Empty>
            ) : (
              <Table
                headers={[
                  <input
                    type="checkbox"
                    aria-label="全选"
                    checked={
                      visible.length > 0 &&
                      visible.every((row) => selected.includes(row.id))
                    }
                    onChange={(e) =>
                      setSelected(
                        e.target.checked ? visible.map((row) => row.id) : [],
                      )
                    }
                  />,
                  "使用者",
                  "来源 / 备注",
                  "中转端口",
                  "带宽",
                  "状态",
                  "建立时间",
                  "操作",
                ]}
                rows={visible.map((row) => [
                  <input
                    type="checkbox"
                    aria-label={`选择 ${row.user || "未记录使用者"} 的中转`}
                    checked={selected.includes(row.id)}
                    onChange={(e) =>
                      setSelected(
                        e.target.checked
                          ? [...selected, row.id]
                          : selected.filter((id) => id !== row.id),
                      )
                    }
                  />,
                  <span className={"user-cell " + (row.user ? "" : "unknown")}>
                    <b>{row.user || "未记录使用者"}</b>
                    <span>（{row.maskedIp}）</span>
                  </span>,
                  <>
                    <Badge tone={row.source === "code" ? "orange" : ""}>
                      {row.source === "code"
                        ? `中转码 …${row.codeTail || "????"}`
                        : sourceNames[row.source] || "升级前配置"}
                    </Badge>
                    {row.remark && <small>{row.remark}</small>}
                  </>,
                  <span className="code-text">{row.port}</span>,
                  row.rateMbps ? `${row.rateMbps} Mbps` : "不限速",
                  row.state === "running" ? (
                    <>
                      <span className="status online">
                        <span className="dot" />
                        运行中
                      </span>
                      <small>{row.connections} 个连接</small>
                    </>
                  ) : row.state === "paused" ? (
                    <span className="status paused">
                      <span className="dot" />
                      已暂停
                    </span>
                  ) : (
                    <span className="status offline">
                      <span className="dot" />
                      服务未运行
                    </span>
                  ),
                  <>
                    {shortTime(row.createdAt)}
                    {!row.recorded && <small>按服务创建时间</small>}
                  </>,
                  <div className="actions">
                    {row.state !== "stopped" && (
                      <Button
                        className="small"
                        disabled={working}
                        onClick={() =>
                          setDialog({ kind: "rate", items: [row] })
                        }
                      >
                        修改带宽
                      </Button>
                    )}
                    {row.state === "running" && (
                      <Button
                        className="small"
                        disabled={working}
                        onClick={() => void act("pause", [row])}
                      >
                        暂停
                      </Button>
                    )}
                    {row.state === "paused" && (
                      <Button
                        className="small"
                        disabled={working}
                        onClick={() => void act("resume", [row])}
                      >
                        恢复
                      </Button>
                    )}
                    {row.state === "stopped" && (
                      <Button
                        className="small"
                        disabled={working}
                        onClick={() => void act("restart", [row])}
                      >
                        <RotateCw size={13} />
                        重新启动
                      </Button>
                    )}
                    <Button
                      className="small danger"
                      disabled={working}
                      onClick={() =>
                        setDialog({ kind: "delete", items: [row] })
                      }
                    >
                      删除
                    </Button>
                  </div>,
                ])}
              />
            )}
          </div>
          <div className="mt16">
            <Notice>
              修改带宽会重启这条中转，使用者断开几秒后自动重连；暂停后使用者无法连接，恢复后继续使用原配置；删除会停止服务、删除配置并撤回本工具添加的防火墙规则，使用者的中转配置随即失效。每次操作都会记录审计（不含
              SSH）。
            </Notice>
          </div>
        </>
      )}
      {dialog?.kind === "rate" && (
        <RateDialog
          items={dialog.items}
          settings={settings}
          busy={working}
          onClose={() => setDialog(null)}
          onSubmit={(rate) => void act("rate", dialog.items, rate)}
        />
      )}
      {dialog?.kind === "delete" && (
        <RemoveDialog
          items={dialog.items}
          busy={working}
          onClose={() => setDialog(null)}
          onSubmit={() => void act("delete", dialog.items)}
        />
      )}
    </>
  );
}

function RelayItems({
  items,
  detail,
}: {
  items: RecordData[];
  detail: (row: RecordData) => string;
}) {
  return (
    <ul className="select-list">
      {items.map((row) => (
        <li key={row.id}>
          <span className={"user-cell " + (row.user ? "" : "unknown")}>
            <b>{row.user || "未记录使用者"}</b>
            <span>（{row.maskedIp}）</span>
          </span>
          <small>{detail(row)}</small>
        </li>
      ))}
    </ul>
  );
}

function RateDialog({
  items,
  settings,
  busy,
  onClose,
  onSubmit,
}: {
  items: RecordData[];
  settings: RecordData;
  busy: boolean;
  onClose: () => void;
  onSubmit: (rate: number) => void;
}) {
  const [rate, setRate] = useState(() => rateRange(settings).fallback);
  return (
    <Modal title="修改带宽" onClose={onClose}>
      <p className="muted">{`将修改以下 ${items.length} 条中转的带宽：`}</p>
      <RelayItems
        items={items}
        detail={(row) =>
          `端口 ${row.port} · 当前 ${row.rateMbps ? `${row.rateMbps} Mbps` : "不限速"}`
        }
      />
      <div className="mt16">
        <RateSelect
          label="新的带宽（上下行各）"
          value={rate}
          onChange={setRate}
          settings={settings}
        />
      </div>
      <Notice>修改后会重启这些中转，使用者的连接会中断几秒后自动恢复。</Notice>
      <div className="form-actions">
        <Button onClick={onClose}>取消</Button>
        <Button primary disabled={busy} onClick={() => onSubmit(rate)}>
          <Gauge size={14} />
          {busy ? "正在修改…" : "修改带宽"}
        </Button>
      </div>
    </Modal>
  );
}

function RemoveDialog({
  items,
  busy,
  onClose,
  onSubmit,
}: {
  items: RecordData[];
  busy: boolean;
  onClose: () => void;
  onSubmit: () => void;
}) {
  const [confirmed, setConfirmed] = useState(false);
  return (
    <Modal title="删除中转" onClose={onClose}>
      <Notice tone="red">
        删除会停止服务、删除配置并撤回本工具添加的防火墙规则。使用者的中转配置随即失效，无法恢复。
      </Notice>
      <p className="muted mt16">{`将删除以下 ${items.length} 条中转：`}</p>
      <RelayItems
        items={items}
        detail={(row) =>
          `端口 ${row.port} · ${row.source === "code" ? `中转码 …${row.codeTail || "????"}` : sourceNames[row.source] || "升级前配置"}`
        }
      />
      <Check
        className="mt16"
        checked={confirmed}
        onChange={(e) => setConfirmed(e.target.checked)}
        label="我已了解删除的影响"
      />
      <div className="form-actions">
        <Button onClick={onClose}>取消</Button>
        <Button
          className="danger"
          disabled={!confirmed || busy}
          onClick={onSubmit}
        >
          <Trash2 size={14} />
          {busy ? "正在删除…" : "删除中转"}
        </Button>
      </div>
    </Modal>
  );
}

function UsedRelays({
  relays,
  loading,
  error,
  user,
  onNavigate,
}: {
  relays: RecordData[];
  loading: boolean;
  error: string;
  user: RecordData;
  onNavigate: (view: string) => void;
}) {
  const [files, setFiles] = useState<Record<string, boolean>>({}),
    [message, setMessage] = useState("");
  useEffect(() => {
    let alive = true;
    Promise.all(
      relays.map(async (relay) => [
        relay.taskId,
        !!(await getLocal(user.id, relay.taskId).catch(() => null)),
      ]),
    ).then((values) => {
      if (alive) setFiles(Object.fromEntries(values));
    });
    return () => {
      alive = false;
    };
  }, [relays, user.id]);
  return (
    <>
      <div className="mb16">
        <Notice>
          这里是你用别人的中转码建立的中转。网站不保存拥有者的
          SSH，无法实时得知这条中转是否被拥有者暂停或删除；连不上时请联系拥有者。
        </Notice>
      </div>
      <ErrorNotice error={error || message} />
      <div className="card flush">
        {!loading && !relays.length ? (
          <Empty>还没有用别人的中转码建立中转。</Empty>
        ) : (
          <Table
            loading={loading}
            headers={[
              "中转服务器",
              "入口端口",
              "我的节点",
              "带宽",
              "建立时间",
              "来源中转码",
              "操作",
            ]}
            rows={relays.map((relay) => [
              <>
                <span className="code-text">
                  {relay.viaFront ? relay.frontHost : relay.serverMasked}
                </span>
                <small>
                  {relay.viaFront
                    ? `经自备前置机 · 中转服务器 ${relay.serverMasked} · `
                    : ""}
                  拥有者 {relay.ownerMasked}
                </small>
              </>,
              <span className="code-text">{relay.entryPort || "—"}</span>,
              <span className="code-text">{relay.target}</span>,
              `${relay.rateMbps} Mbps`,
              fullTime(relay.createdAt),
              <span className="code-text">…{relay.codeTail}</span>,
              <div className="actions">
                <Button
                  className="small"
                  disabled={!files[relay.taskId]}
                  title={
                    files[relay.taskId]
                      ? undefined
                      : "当前浏览器没有保存这份配置"
                  }
                  onClick={async () => {
                    try {
                      const file = await getLocal(user.id, relay.taskId);
                      if (file) download(file.data, file.name);
                    } catch (e) {
                      setMessage((e as Error).message);
                    }
                  }}
                >
                  <Download size={14} />
                  {files[relay.taskId] ? "下载配置" : "本机未保存"}
                </Button>
                <Button className="small" onClick={() => onNavigate("tasks")}>
                  任务记录
                </Button>
              </div>,
            ])}
          />
        )}
      </div>
    </>
  );
}

// ===== 管理员 =====

export function RelayCodeSettings({ onSaved }: { onSaved: () => void }) {
  const { data, error, loading, reload } = useData("/api/admin/relay-codes");
  const [values, setValues] = useState<RecordData | null>(null),
    [busy, setBusy] = useState(false),
    [message, setMessage] = useState(""),
    [failure, setFailure] = useState("");
  useEffect(() => {
    if (data) setValues({ ...data.settings });
  }, [data]);
  if (loading && !values) return <Notice>正在读取中转码设置…</Notice>;
  if (!values) return <ErrorNotice error={error || "中转码设置读取失败"} />;
  const stats = data?.stats || {};
  const number = (
    key: string,
    label: string,
    min: number,
    max: number,
    hint?: string,
  ) => (
    <Field
      label={label}
      type="number"
      min={min}
      max={max}
      value={values[key]}
      hint={hint}
      onChange={(e) => {
        const n = Math.trunc(Number(e.target.value));
        setValues({
          ...values,
          [key]: Number.isFinite(n) ? Math.min(max, Math.max(min, n)) : min,
        });
      }}
    />
  );
  const rates = [];
  for (let n = 1; n <= 10; n++) rates.push(n);
  return (
    <form
      onSubmit={async (e) => {
        e.preventDefault();
        if (busy) return;
        setBusy(true);
        setFailure("");
        setMessage("");
        try {
          if (
            data?.settings?.enabled &&
            !values.enabled &&
            !confirm(
              "关闭中转码功能会立即删除全部暂存的 SSH，所有中转码随即失效。确认关闭？",
            )
          )
            return;
          await post("/api/admin/settings", { relayCodes: values }, "PUT");
          setMessage("中转码设置已保存");
          reload();
          onSaved();
        } catch (err) {
          setFailure((err as Error).message);
        } finally {
          setBusy(false);
        }
      }}
    >
      <Notice tone="orange">
        关闭后，用户不能再生成或使用中转码，所有暂存的 SSH
        立即删除；已建立的中转不受影响。
      </Notice>
      <div className="setting-list mt16">
        {(
          [
            ["enabled", "允许用户生成和使用中转码"],
            [
              "manageWithSecret",
              "允许用中转码暂存的 SSH 管理中转服务器（读取列表、修改带宽、暂停、删除中转）",
            ],
            ["notifyOwner", "中转码被使用时，向拥有者发送邮件提醒"],
          ] as [string, string][]
        ).map(([key, label]) => (
          <Check
            key={key}
            label={label}
            checked={!!values[key]}
            onChange={(e) => setValues({ ...values, [key]: e.target.checked })}
          />
        ))}
      </div>
      <h3 className="mt24">上限</h3>
      <p className="muted mt8">
        调低上限只影响之后生成的中转码，已生成的按原设置到期。
      </p>
      <div className="form-grid mt16">
        {number(
          "maxTtlHours",
          "有效期上限（小时）",
          1,
          72,
          "最长 72 小时，只能调低。",
        )}
        {number(
          "maxUses",
          "每个中转码最多次数",
          1,
          10,
          "最多 10 次，只能调低。",
        )}
        {number("maxActive", "每人同时有效的中转码", 1, 20)}
        <Select
          label="默认带宽"
          value={values.defaultRateMbps}
          onChange={(e) =>
            setValues({ ...values, defaultRateMbps: Number(e.target.value) })
          }
        >
          {rates.map((n) => (
            <option key={n} value={n}>
              {n} Mbps
            </option>
          ))}
        </Select>
        <div className="field">
          <span>可选带宽范围（Mbps）</span>
          <div className="input-group">
            <input
              type="number"
              min={1}
              max={10}
              aria-label="最低带宽"
              value={values.rateMinMbps}
              onChange={(e) =>
                setValues({
                  ...values,
                  rateMinMbps: Math.min(
                    10,
                    Math.max(1, Math.trunc(Number(e.target.value)) || 1),
                  ),
                })
              }
            />
            <span className="muted">–</span>
            <input
              type="number"
              min={1}
              max={10}
              aria-label="最高带宽"
              value={values.rateMaxMbps}
              onChange={(e) =>
                setValues({
                  ...values,
                  rateMaxMbps: Math.min(
                    10,
                    Math.max(1, Math.trunc(Number(e.target.value)) || 1),
                  ),
                })
              }
            />
          </div>
          <small>同时作用于生成中转码和手动配置中转。</small>
        </div>
        {number("redeemPerAccount", "使用尝试（每账号每小时）", 1, 100)}
        {number("redeemPerIp", "使用尝试（每 IP 每小时）", 1, 500)}
        {number("manageMinutes", "中转服务器管理限额窗口（分钟）", 1, 1440)}
        {number(
          "manageCount",
          "中转服务器管理限额次数",
          1,
          100,
          "读取列表和每次操作都计入。",
        )}
      </div>
      <h3 className="mt24">暂存的 SSH</h3>
      <div className="detail-grid">
        <div className="kv">
          <span>加密方式</span>
          <strong>AES-256-GCM（主密钥）</strong>
        </div>
        <div className="kv">
          <span>自动删除检查</span>
          <strong>每分钟</strong>
        </div>
        <div className="kv">
          <span>整站备份</span>
          <strong>不包含</strong>
        </div>
        <div className="kv">
          <span>当前暂存</span>
          <strong>{stats.secrets ?? 0} 份</strong>
        </div>
        <div className="kv">
          <span>最早删除</span>
          <strong>
            {stats.earliestDeletion
              ? `${remainingText(stats.earliestDeletion)}后`
              : "—"}
          </strong>
        </div>
        <div className="kv">
          <span>最晚删除</span>
          <strong>
            {stats.latestDeletion
              ? `${remainingText(stats.latestDeletion)}后`
              : "—"}
          </strong>
        </div>
      </div>
      <div className="mt16">
        <Notice tone="red">
          发现安全问题时，可以立即删除全部暂存的
          SSH，所有中转码随即失效；已建立的中转不受影响。
        </Notice>
      </div>
      {message && (
        <div className="mt16">
          <Notice tone="green">{message}</Notice>
        </div>
      )}
      <ErrorNotice error={failure} />
      <div className="form-actions">
        <Button
          className="danger"
          disabled={busy}
          onClick={async () => {
            if (
              !confirm("立即删除全部暂存的 SSH？所有中转码随即失效，无法恢复。")
            )
              return;
            setBusy(true);
            setFailure("");
            try {
              const r = await post("/api/admin/relay-codes/purge", {
                confirm: "DELETE_ALL_RELAY_CODE_SSH",
              });
              setMessage(`已删除全部暂存的 SSH，${r.revoked} 个中转码已失效。`);
              reload();
            } catch (err) {
              setFailure((err as Error).message);
            } finally {
              setBusy(false);
            }
          }}
        >
          <Trash2 size={14} />
          立即删除全部暂存的 SSH
        </Button>
        <Button type="submit" primary disabled={busy}>
          {busy ? "正在保存…" : "保存"}
        </Button>
      </div>
    </form>
  );
}

export function RelayCodeAudit() {
  const { data, error, loading, reload } = useData("/api/admin/relay-codes");
  const [tab, setTab] = useState<"users" | "deletions">("users"),
    [search, setSearch] = useState(""),
    [state, setState] = useState(""),
    [viewing, setViewing] = useState<RecordData | null>(null),
    [disabling, setDisabling] = useState<RecordData | null>(null),
    [message, setMessage] = useState(""),
    [failure, setFailure] = useState("");
  const stats = data?.stats || {};
  const users = array(data, "users").filter(
    (row) =>
      (!search.trim() ||
        row.email.toLowerCase().includes(search.trim().toLowerCase())) &&
      (!state ||
        (state === "disabled" && row.disabled) ||
        (state === "limit" && row.atLimit && !row.disabled) ||
        (state === "normal" && !row.atLimit && !row.disabled)),
  );
  const deleted = stats.deletedByReason || {};
  async function revokeAll(row: RecordData) {
    if (
      !confirm(
        `作废 ${row.email} 的全部中转码？暂存的 SSH 会立即删除，已建立的中转不受影响。`,
      )
    )
      return;
    setFailure("");
    try {
      const r = await post(
        `/api/admin/relay-codes/users/${row.userId}/revoke`,
        {},
      );
      setMessage(`已作废 ${row.email} 的 ${r.revoked} 个中转码。`);
      reload();
    } catch (e) {
      setFailure((e as Error).message);
    }
  }
  async function feature(row: RecordData, enabled: boolean, reason = "") {
    setFailure("");
    try {
      const r = await post(
        `/api/admin/relay-codes/users/${row.userId}/feature`,
        { enabled, reason },
      );
      setMessage(
        enabled
          ? `已恢复 ${row.email} 的中转码功能。`
          : `已停用 ${row.email} 的中转码功能，${r.revoked} 个中转码已失效。`,
      );
      setDisabling(null);
      reload();
    } catch (e) {
      setFailure((e as Error).message);
    }
  }
  return (
    <>
      <Header
        title="中转码审计"
        sub="查看各用户的中转码和暂存的 SSH；发现滥用时可以作废或停用。"
      >
        <Button onClick={reload}>刷新</Button>
      </Header>
      <ErrorNotice error={error || failure} />
      {message && (
        <div className="mb16">
          <Notice tone="green">{message}</Notice>
        </div>
      )}
      <div className="grid4">
        <div className="card stat">
          <div className="stat-top">
            有效中转码
            <span className="stat-icon">
              <Ticket size={15} />
            </span>
          </div>
          <div className="stat-value">
            {stats.activeCodes ?? "—"}
            <span>个</span>
          </div>
          <small>涉及 {stats.activeOwners ?? 0} 位用户</small>
        </div>
        <div className="card stat">
          <div className="stat-top">
            暂存的 SSH
            <span className="stat-icon">
              <KeyRound size={15} />
            </span>
          </div>
          <div className="stat-value">
            {stats.secrets ?? "—"}
            <span>份</span>
          </div>
          <small>
            {stats.latestDeletion
              ? `最长剩余 ${remainingText(stats.latestDeletion)}`
              : "没有暂存的 SSH"}
          </small>
        </div>
        <div className="card stat">
          <div className="stat-top">
            近 7 天使用
            <span className="stat-icon">
              <Route size={15} />
            </span>
          </div>
          <div className="stat-value">
            {stats.uses7d ?? "—"}
            <span>次</span>
          </div>
          <small>另有 {stats.failures7d ?? 0} 次失败，不计次</small>
        </div>
        <div className="card stat">
          <div className="stat-top">
            近 7 天删除 SSH
            <span className="stat-icon">
              <Trash2 size={15} />
            </span>
          </div>
          <div className="stat-value">
            {stats.deleted7d ?? "—"}
            <span>份</span>
          </div>
          <small>
            到期 {deleted.expired || 0} · 用完 {deleted.exhausted || 0} · 作废{" "}
            {(deleted.revoked || 0) + (deleted.admin_revoke || 0)}
          </small>
        </div>
      </div>
      <div className="tabs mt24" role="tablist">
        {(
          [
            ["users", "按用户"],
            ["deletions", "SSH 删除记录"],
          ] as ["users" | "deletions", string][]
        ).map(([key, name]) => (
          <button
            type="button"
            role="tab"
            key={key}
            aria-selected={tab === key}
            className={tab === key ? "active" : ""}
            onClick={() => setTab(key)}
          >
            {name}
          </button>
        ))}
      </div>
      {tab === "users" ? (
        <>
          <div className="filterbar">
            <input
              type="search"
              placeholder="搜索邮箱"
              aria-label="搜索邮箱"
              value={search}
              onChange={(e) => setSearch(e.target.value)}
            />
            <select
              aria-label="按状态筛选"
              value={state}
              onChange={(e) => setState(e.target.value)}
            >
              <option value="">全部状态</option>
              <option value="normal">正常</option>
              <option value="limit">已达上限</option>
              <option value="disabled">已停用中转码功能</option>
            </select>
          </div>
          <div className="mb16">
            <Notice>
              「作废全部」会立即删除该用户暂存的
              SSH；「停用功能」后该用户不能再生成或使用中转码。已建立的中转不受影响（网站不持有这些服务器的
              SSH，无法远程删除）。
            </Notice>
          </div>
          <div className="card flush">
            <Table
              loading={loading}
              headers={[
                "用户",
                "有效中转码",
                "暂存 SSH",
                "最长剩余",
                "近 7 天被使用",
                "近 7 天使用别人的",
                "状态",
                "操作",
              ]}
              rows={users.map((row) => [
                row.email,
                row.activeCodes,
                row.secrets,
                row.longestExpiresAt
                  ? remainingText(row.longestExpiresAt)
                  : "—",
                `${row.usedByOthers7d} 次`,
                `${row.usedOthers7d} 次`,
                row.disabled ? (
                  <>
                    <Badge tone="red">已停用中转码功能</Badge>
                    <small>
                      {shortTime(row.disabledAt).slice(0, 5)}
                      {row.disabledReason ? ` · ${row.disabledReason}` : ""}
                    </small>
                  </>
                ) : row.atLimit ? (
                  <Badge tone="orange">已达上限</Badge>
                ) : (
                  <Badge tone="green">正常</Badge>
                ),
                <div className="actions">
                  <Button className="small" onClick={() => setViewing(row)}>
                    查看
                  </Button>
                  {row.activeCodes > 0 && (
                    <Button
                      className="small"
                      onClick={() => void revokeAll(row)}
                    >
                      作废全部
                    </Button>
                  )}
                  {row.disabled ? (
                    <Button
                      className="small"
                      onClick={() => void feature(row, true)}
                    >
                      恢复功能
                    </Button>
                  ) : (
                    <Button
                      className="small danger"
                      onClick={() => setDisabling(row)}
                    >
                      停用功能
                    </Button>
                  )}
                </div>,
              ])}
            />
          </div>
        </>
      ) : (
        <>
          <div className="mb16">
            <Notice>只记录删除事件，不记录任何 SSH 内容。</Notice>
          </div>
          <div className="card flush">
            <Table
              loading={loading}
              headers={["时间", "拥有者", "中转码", "原因", "操作人"]}
              rows={array(data, "deletions").map((row) => [
                fullTime(row.createdAt),
                row.ownerEmail,
                <span className="code-text">MSB-…-{row.codeTail}</span>,
                <Badge tone={adminReasons[row.reason]?.[1] || ""}>
                  {adminReasons[row.reason]?.[0] || row.reason}
                </Badge>,
                row.actor,
              ])}
            />
          </div>
        </>
      )}
      {viewing && <AuditUser row={viewing} onClose={() => setViewing(null)} />}
      {disabling && (
        <DisableFeature
          row={disabling}
          onClose={() => setDisabling(null)}
          onSubmit={(reason) => void feature(disabling, false, reason)}
        />
      )}
    </>
  );
}

function AuditUser({ row, onClose }: { row: RecordData; onClose: () => void }) {
  const { data, error, loading } = useData(
    `/api/admin/relay-codes/users/${row.userId}`,
  );
  return (
    <Modal title={`${row.email} 的中转码`} onClose={onClose}>
      <ErrorNotice error={error} />
      <div className="card flush">
        <Table
          loading={loading}
          headers={[
            "中转码",
            "备注 / 服务器",
            "次数",
            "带宽",
            "SSH 暂存",
            "状态",
          ]}
          rows={array(data, "codes").map((code) => [
            <span className="code-text">{code.masked}</span>,
            <>
              {code.note || "—"}
              <small className="code-text">{code.serverHost}</small>
            </>,
            `${code.usedCount} / ${code.maxUses} 次`,
            `${code.rateMbps} Mbps`,
            <SecretState code={code} />,
            <Badge tone={stateLabels[code.state]?.[1] || ""}>
              {stateLabels[code.state]?.[0] || code.state}
            </Badge>,
          ])}
        />
      </div>
    </Modal>
  );
}

function DisableFeature({
  row,
  onClose,
  onSubmit,
}: {
  row: RecordData;
  onClose: () => void;
  onSubmit: (reason: string) => void;
}) {
  const [reason, setReason] = useState("");
  return (
    <Modal title="停用中转码功能" onClose={onClose}>
      <Notice tone="red">
        {`停用后 ${row.email} 不能再生成或使用中转码，其全部中转码立即作废，暂存的 SSH 立即删除；已建立的中转不受影响。`}
      </Notice>
      <div className="mt16">
        <Field
          label="原因（仅管理员可见）"
          maxLength={60}
          placeholder="例如：公开出售中转码"
          value={reason}
          onChange={(e) => setReason(e.target.value)}
        />
      </div>
      <div className="form-actions">
        <Button onClick={onClose}>取消</Button>
        <Button className="danger" onClick={() => onSubmit(reason.trim())}>
          <Users size={14} />
          停用功能
        </Button>
      </div>
    </Modal>
  );
}
