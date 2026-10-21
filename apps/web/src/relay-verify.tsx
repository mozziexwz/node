import { useEffect, useState } from "react";
import {
  CircleCheck,
  CircleX,
  Clock,
  RefreshCw,
  ShieldCheck,
} from "lucide-react";
import { post, RecordData } from "./api";
import {
  Badge,
  Button,
  ErrorNotice,
  Field,
  Notice,
  Select,
  Table,
  useData,
} from "./ui";

// Relay target verification: the site checks, through the real relay entry,
// that a relay leads to a real MSBOOST node.

const reasons: Record<string, string> = {
  ok: "已校验：MSBOOST 节点",
  entry_unreachable: "中转还没准备好，请稍后重试",
  target_down: "节点没有运行，或节点防火墙没有放行中转服务器的 IP",
  tunnel_failed:
    "配置文件与节点不匹配，或目标不是 mieru，请重新上传节点的配置文件",
  not_msboost: "不是 MSBOOST 节点，或节点版本较低，请重新部署MSBOOST节点。",
  unavailable: "暂时未能校验，稍后会自动重试",
};

export function verifyReason(code?: string) {
  return (code && reasons[code]) || "尚未校验";
}

const games: Record<string, string> = {
  ok: "可连接",
  refused: "连接被拒绝",
  timeout: "连接超时",
  unreachable: "无法到达",
  failed: "连接失败",
};

const filters: Record<string, string> = {
  ok: "正常",
  missing: "被改动（本应拒绝的网站可以访问）",
  inconclusive: "无法判断（对照网站也连不上）",
  skipped: "未检测",
};

const shortTime = (ms?: number) =>
  ms
    ? new Date(ms).toLocaleString("zh-CN", {
        month: "2-digit",
        day: "2-digit",
        hour: "2-digit",
        minute: "2-digit",
        hour12: false,
      })
    : "";

// A node that was redeployed has a new configuration, so a rule or relay has
// to be rebuilt with it; say so wherever a check failed.
const redeployHint =
  "重新部署会生成新的配置文件，旧配置随之失效：部署完成后，请删除这条线路并用新的配置文件重新配置。";

/** A one-line badge for tables: rules, relay-code uses and relay lists. */
export function VerifyBadge({
  verify,
  at,
}: {
  verify?: RecordData | null;
  at?: number;
}) {
  if (!verify) return <Badge>未校验</Badge>;
  const time = shortTime(at ?? verify.checkedAt);
  switch (verify.state ?? (verify.code === "ok" ? "ok" : "failed")) {
    case "ok":
      return (
        <Badge tone="green">
          已校验 MSBOOST 节点{time ? ` · ${time}` : ""}
        </Badge>
      );
    case "exempt":
      return <Badge>免校验</Badge>;
    case "paused":
      return <Badge tone="red">校验未通过 · 已暂停</Badge>;
    case "pending":
    case "unchecked":
      return <Badge tone="orange">待校验</Badge>;
  }
  if (verify.code && !["tunnel_failed", "not_msboost"].includes(verify.code))
    return <Badge tone="orange">未能确认 · {time}</Badge>;
  return <Badge tone="red">校验未通过{time ? ` · ${time}` : ""}</Badge>;
}

/** The self-test of a free relay or relay-code task, on its result page. */
export function SelfTest({ verify }: { verify: RecordData }) {
  const ok = verify.code === "ok";
  return (
    <div className="mt16" aria-label="中转自测">
      <Table
        headers={["自测项目", "结果"]}
        rows={[
          [
            "MSBOOST 节点",
            <span className={ok ? "green" : "red"}>
              {ok ? (
                <CircleCheck size={14} aria-hidden="true" />
              ) : (
                <CircleX size={14} aria-hidden="true" />
              )}{" "}
              {ok ? "已确认是 MSBOOST 节点" : verifyReason(verify.code)}
            </span>,
          ],
          [
            "游戏服务器",
            ok && verify.game && verify.game !== "skipped"
              ? verify.game === "ok"
                ? "节点可以连接游戏服务器"
                : `${games[verify.game] || "连接失败"}：可能是游戏维护或 VPS 网络问题`
              : "未检测",
          ],
        ]}
      />
      <p className="muted mt8">
        自测经过刚建好的中转入口连接你的节点，与你实际使用的路径一致。
        {!ok && ["tunnel_failed", "not_msboost"].includes(verify.code)
          ? " 节点重新部署过的话，请用新的配置文件重新配置中转。"
          : ""}
      </p>
    </div>
  );
}

/** A paid-route rule's verification on the member's route card. */
export function RuleVerify({
  rule,
  routeId,
  onDone,
}: {
  rule: RecordData;
  routeId: string;
  onDone: () => void;
}) {
  const verify = rule.verify;
  const [busy, setBusy] = useState(false),
    [asked, setAsked] = useState(0),
    [error, setError] = useState("");
  useEffect(() => {
    if (asked && verify?.checkedAt > asked) setAsked(0);
  }, [verify?.checkedAt, asked]);
  if (!verify || rule.state === "revoking") return null;
  const enforce = rule.verifyMode === "enforce";
  const failed = ["tunnel_failed", "not_msboost"].includes(verify.code);
  let tone = "",
    text = "";
  switch (verify.state) {
    case "ok":
      tone = "green";
      text = `目标已校验为 MSBOOST 节点（${shortTime(verify.passedAt)}）。`;
      break;
    case "pending":
      tone = "orange";
      text = enforce
        ? "正在校验目标节点，通过后才能下载配置；通常一两分钟内完成。"
        : "正在校验目标节点。";
      if (verify.code && failed)
        text = `校验未通过：${verifyReason(verify.code)}`;
      break;
    case "unchecked":
      text = "目标节点等待例行校验。";
      break;
    case "failed":
      tone = "red";
      text = `校验未通过：${verifyReason(verify.code)}`;
      if (rule.verifyPauseAt)
        text += ` 如果 ${shortTime(rule.verifyPauseAt)} 前仍未通过，这条线路会暂停。`;
      break;
    case "paused":
      tone = "red";
      text = `线路已暂停：${verifyReason(verify.code)} 修好节点后点「重新校验」，通过后自动恢复。`;
      break;
    case "exempt":
      return (
        <p className="muted mt8">
          <ShieldCheck size={14} aria-hidden="true" />{" "}
          这条线路由管理员核查，免于例行校验。
        </p>
      );
  }
  if (verify.state === "ok" && verify.code && verify.code !== "ok" && !failed) {
    text += ` 最近一次未能确认：${verifyReason(verify.code)}，会自动重试。`;
  }
  return (
    <div className="mt16" aria-label="目标校验">
      <Notice tone={tone}>
        <span>
          {text}
          {failed && verify.state !== "ok" ? ` ${redeployHint}` : ""}
          {asked ? " 已安排校验，结果会自动刷新。" : ""}
        </span>
      </Notice>
      <ErrorNotice error={error} />
      {verify.state !== "ok" || verify.code !== "ok" ? (
        <Button
          className="small mt8"
          disabled={busy || !!asked}
          onClick={async () => {
            setBusy(true);
            setError("");
            try {
              await post(`/api/user/routes/${routeId}/verify`, {});
              setAsked(Date.now());
              onDone();
            } catch (e) {
              setError((e as Error).message);
            } finally {
              setBusy(false);
            }
          }}
        >
          {asked ? <Clock size={14} /> : <RefreshCw size={14} />}
          {asked ? "校验中…" : "重新校验"}
        </Button>
      ) : null}
    </div>
  );
}

/** Administrator view of one rule's verification, with its actions. */
export function AdminRuleVerify({
  rule,
  onChanged,
}: {
  rule: RecordData;
  onChanged: () => void;
}) {
  const verify = rule.verify;
  const [reason, setReason] = useState(""),
    [busy, setBusy] = useState(false),
    [error, setError] = useState("");
  async function act(action: string) {
    setBusy(true);
    setError("");
    try {
      await post(
        `/api/admin/user-rules/${encodeURIComponent(rule.id)}/verify`,
        { action, reason },
      );
      setReason("");
      onChanged();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }
  return (
    <div className="card mt16" aria-label="目标校验">
      <h3>目标校验</h3>
      <div className="mt8">
        <VerifyBadge verify={verify} />
      </div>
      {verify ? (
        <Table
          headers={["项目", "结果"]}
          rows={[
            ["最近结果", verifyReason(verify.code)],
            ["最近校验", shortTime(verify.checkedAt) || "—"],
            ["最近通过", shortTime(verify.passedAt) || "—"],
            ["连续失败起始", shortTime(verify.failedSince) || "—"],
            ["游戏服务器", games[verify.game] || "未检测"],
            ["节点过滤规则（只记录）", filters[verify.filter] || "未检测"],
            ...(rule.verifyPauseAt
              ? [["预计暂停", shortTime(rule.verifyPauseAt)]]
              : []),
            ...(verify.exemptReason ? [["豁免原因", verify.exemptReason]] : []),
          ]}
        />
      ) : (
        <p className="muted mt8">这条规则还没有校验记录。</p>
      )}
      <ErrorNotice error={error} />
      <div className="actions mt16">
        {verify?.state !== "exempt" && (
          <Button disabled={busy} onClick={() => void act("recheck")}>
            <RefreshCw size={14} />
            重新校验
          </Button>
        )}
        {verify?.state === "exempt" && (
          <Button disabled={busy} onClick={() => void act("unexempt")}>
            取消豁免
          </Button>
        )}
      </div>
      {verify?.state !== "exempt" && (
        <div className="mt16">
          <Field
            label="豁免原因（豁免后不再例行校验，会写入审计）"
            value={reason}
            maxLength={200}
            onChange={(e) => setReason(e.target.value)}
          />
          <Button
            disabled={busy || reason.trim().length < 2}
            onClick={() => void act("exempt")}
          >
            <ShieldCheck size={14} />
            豁免这条规则
          </Button>
        </div>
      )}
    </div>
  );
}

export const verifyFilterOptions: [string, string][] = [
  ["", "全部校验状态"],
  ["failed", "校验未通过"],
  ["paused", "因校验暂停"],
  ["waiting", "待校验"],
  ["ok", "已校验"],
  ["exempt", "免校验"],
];

export function matchesVerifyFilter(rule: RecordData, filter: string) {
  const state = rule.verify?.state || "";
  switch (filter) {
    case "":
      return true;
    case "waiting":
      return !state || state === "pending" || state === "unchecked";
    default:
      return state === filter;
  }
}

const modes: [string, string][] = [
  ["off", "关闭"],
  ["log", "只记录"],
  ["enforce", "强制"],
];

/** Settings → 中转校验. */
export function RelayVerifySettings({ onSaved }: { onSaved: () => void }) {
  const { data, error, loading } = useData("/api/admin/settings");
  const [values, setValues] = useState<RecordData | null>(null),
    [busy, setBusy] = useState(false),
    [message, setMessage] = useState(""),
    [failure, setFailure] = useState("");
  useEffect(() => {
    if (data) setValues({ ...(data.settings || data).relayVerify });
  }, [data]);
  if (loading && !values) return <Notice>正在读取中转校验设置…</Notice>;
  if (!values) return <ErrorNotice error={error || "中转校验设置读取失败"} />;
  const set = (key: string, value: unknown) =>
    setValues({ ...values, [key]: value });
  const number = (
    key: string,
    label: string,
    min: number,
    max: number,
    hint: string,
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
        set(key, Number.isFinite(n) ? Math.min(max, Math.max(min, n)) : min);
      }}
    />
  );
  return (
    <form
      onSubmit={async (e) => {
        e.preventDefault();
        if (busy) return;
        setBusy(true);
        setFailure("");
        setMessage("");
        try {
          await post("/api/admin/settings", { relayVerify: values }, "PUT");
          setMessage("中转校验设置已保存");
          onSaved();
        } catch (err) {
          setFailure((err as Error).message);
        } finally {
          setBusy(false);
        }
      }}
    >
      <Notice>
        执行机用使用者自己的配置，经真实的中转入口连接节点，访问 MSBOOST
        节点自带的测速服务完成一次握手，确认中转的是真正的 MSBOOST
        节点。建议先「只记录」观察一两周，确认误判接近零后再改为「强制」。
      </Notice>
      <div className="form-grid mt24">
        <Select
          label="免费自备中转"
          value={values.free}
          onChange={(e) => set("free", e.target.value)}
        >
          <option value="selftest">显示自测结果（不阻止）</option>
          <option value="off">关闭</option>
        </Select>
        <Select
          label="中转码"
          value={values.code}
          onChange={(e) => set("code", e.target.value)}
        >
          {modes.map(([value, name]) => (
            <option key={value} value={value}>
              {name}
              {value === "enforce" ? "（不通过就撤回刚建的中转）" : ""}
            </option>
          ))}
        </Select>
        <Select
          label="捐赠权益线路"
          value={values.donation}
          onChange={(e) => set("donation", e.target.value)}
        >
          {modes.map(([value, name]) => (
            <option key={value} value={value}>
              {name}
              {value === "enforce" ? "（新建须通过；复查失败宽限后暂停）" : ""}
            </option>
          ))}
        </Select>
        {number(
          "recheckHours",
          "复查间隔（小时）",
          1,
          168,
          "捐赠权益线路的例行复查，时间会随机分散",
        )}
        {number(
          "graceHours",
          "宽限时长（小时）",
          1,
          336,
          "强制模式下，复查失败持续这么久才暂停",
        )}
        {number(
          "breakerPercent",
          "熔断比例（%）",
          5,
          100,
          "一小时内失败比例超过它时，多半是执行机或网络问题，不计入用户失败",
        )}
        <Field
          label="游戏检测目标"
          value={values.gameTarget}
          hint="公网 IPv4 和端口，只作提示"
          onChange={(e) => set("gameTarget", e.target.value.trim())}
        />
        <Field
          label="过滤检查：应被拒绝的网站"
          value={values.filterBlocked}
          hint="只记录，不阻止"
          onChange={(e) => set("filterBlocked", e.target.value.trim())}
        />
        <Field
          label="过滤检查：对照网站（应能访问）"
          value={values.filterAllowed}
          hint="对照网站也连不上时，过滤检查记为无法判断"
          onChange={(e) => set("filterAllowed", e.target.value.trim())}
        />
      </div>
      {failure && <Notice tone="red">{failure}</Notice>}
      {message && <Notice tone="green">{message}</Notice>}
      <div className="form-actions">
        <Button type="submit" primary disabled={busy}>
          {busy ? "正在保存…" : "保存"}
        </Button>
      </div>
    </form>
  );
}
