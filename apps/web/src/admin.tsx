import { useEffect, useState } from "react";
import { Plus } from "lucide-react";
import { api, post, array, download, copyText, RecordData } from "./api";
import {
  Header,
  Button,
  Field,
  Select,
  Check,
  Notice,
  Table,
  Modal,
  Badge,
  Empty,
  ErrorNotice,
  AsyncForm,
  useData,
  money,
  date,
} from "./ui";

type Input = {
  key: string;
  label: string;
  type?: string;
  default?: any;
  options?: [string, string][];
  optional?: boolean;
  convert?: (s: any) => any;
};
type Resource = {
  title: string;
  url: string;
  key: string;
  fields: Input[];
  columns: [string, string][];
  method?: string;
  noDelete?: boolean;
};
const schemas: Record<string, Resource> = {
  plans: {
    title: "套餐管理",
    url: "/api/admin/plans",
    key: "plans",
    fields: [
      { key: "name", label: "套餐名称" },
      {
        key: "priceCents",
        label: "售价（元）",
        type: "number",
        default: 0,
        convert: (v) => Math.round(Number(v) * 100),
      },
      { key: "days", label: "天数（1–31）", type: "number", default: 30 },
      {
        key: "trafficBytes",
        label: "流量（GB）",
        type: "number",
        default: 100,
        convert: (v) => Math.round(Number(v) * 1e9),
      },
      {
        key: "rateMbps",
        label: "每规则速率（Mbps）",
        type: "number",
        default: 1,
      },
      { key: "enabled", label: "上架", type: "checkbox", default: false },
      {
        key: "maxPurchasesPerUser",
        label: "每人限购次数（0 不限购）",
        type: "number",
        default: 0,
      },
    ],
    columns: [
      ["name", "套餐"],
      ["priceCents", "售价（元）"],
      ["days", "天数"],
      ["maxPurchasesPerUser", "每人限购"],
      ["enabled", "已上架"],
    ],
  },
  payments: {
    title: "支付渠道",
    url: "/api/admin/payment-channels",
    key: "channels",
    fields: [
      { key: "name", label: "渠道名称" },
      {
        key: "version",
        label: "协议版本",
        options: [
          ["v1", "易支付 v1 / MD5"],
          ["v2", "易支付 v2 / RSA"],
        ],
      },
      {
        key: "type",
        label: "支付类型",
        options: [
          ["wxpay", "微信"],
          ["alipay", "支付宝"],
        ],
      },
      { key: "gateway", label: "网关 HTTPS 地址", type: "url" },
      { key: "merchantId", label: "商户 ID" },
      {
        key: "merchantKey",
        label: "商户密钥",
        type: "password",
        optional: true,
      },
      {
        key: "privateKey",
        label: "商户 RSA 私钥",
        type: "textarea",
        optional: true,
      },
      {
        key: "platformPublicKey",
        label: "平台 RSA 公钥",
        type: "textarea",
        optional: true,
      },
      { key: "enabled", label: "启用", type: "checkbox", default: false },
    ],
    columns: [
      ["name", "渠道"],
      ["version", "版本"],
      ["type", "类型"],
      ["enabled", "启用"],
    ],
  },
  executors: {
    title: "控制执行机",
    url: "/api/admin/executors",
    key: "executors",
    method: "PATCH",
    fields: [
      { key: "name", label: "执行机名称" },
      {
        key: "status",
        label: "状态",
        options: [
          ["active", "启用"],
          ["disabled", "停用"],
        ],
      },
    ],
    columns: [
      ["name", "名称"],
      ["status", "启用状态"],
      ["lastSeenAt", "最后心跳"],
    ],
  },
  agents: {
    title: "节点",
    url: "/api/admin/relay-agents",
    key: "agents",
    fields: [
      { key: "name", label: "名称" },
      { key: "address", label: "公网 IP" },
      {
        key: "addresses",
        label: "其他公网 IP（IPv4 / IPv6，每行一个；可留空）",
        type: "textarea",
        optional: true,
        convert: (v: string) =>
          v
            .split(/\r?\n/)
            .map((x) => x.trim())
            .filter(Boolean),
      },
      {
        key: "portRanges",
        label: "可用端口范围（多段用逗号分开）",
        default: "20000-59999",
        convert: (v: string) =>
          v.split(",").map((x) => {
            const [start, end] = x.trim().split("-").map(Number);
            return { start, end: end || start };
          }),
      },
      { key: "enabled", label: "启用", type: "checkbox", default: true },
    ],
    columns: [
      ["name", "名称"],
      ["address", "公网 IP"],
      ["online", "在线状态"],
      ["enabled", "启用"],
    ],
  },
};
function show(value: any, key: string) {
  if (key === "priceCents") return `¥ ${money(value)}`;
  if (key === "maxPurchasesPerUser") return value ? `${value} 次` : "不限购";
  if (key.endsWith("At") || key === "lastSeen") return date(value);
  if (typeof value === "boolean")
    return <Badge tone={value ? "green" : ""}>{value ? "是" : "否"}</Badge>;
  if (value == null || value === "") return "—";
  return String(value);
}
export function ResourcePage({ kind }: { kind: string }) {
  const s = schemas[kind],
    { data, error, reload } = useData(s.url);
  const [editing, setEditing] = useState<RecordData | null>(null),
    [values, setValues] = useState<RecordData>({}),
    [message, setMessage] = useState(""),
    [token, setToken] = useState<RecordData | null>(null);
  function open(row: RecordData = {}) {
    const initial: RecordData = {};
    for (const f of s.fields) {
      let v = row[f.key] ?? f.default ?? f.options?.[0][0] ?? "";
      if (
        f.type === "password" ||
        f.key === "privateKey" ||
        f.key === "platformPublicKey"
      )
        v = "";
      if (kind === "plans" && row.id) {
        if (f.key === "priceCents") v = row.priceCents / 100;
        if (f.key === "trafficBytes") v = row.trafficBytes / 1e9;
      }
      if (f.key === "portRanges" && Array.isArray(v))
        v = v.map((p: RecordData) => `${p.start}-${p.end}`).join(",");
      if (f.key === "addresses" && Array.isArray(v)) v = v.join("\n");
      initial[f.key] = v;
    }
    setValues(initial);
    setEditing(row);
  }
  const rows = array(data, s.key);
  return (
    <>
      <Header title={s.title}>
        <Button primary onClick={() => open()}>
          <Plus size={16} />
          新增
        </Button>
      </Header>
      <ErrorNotice error={error || message} />
      {kind === "payments" && (
        <Notice>
          保存后的密钥不回显。当前使用真实收银台跳转；回调地址须公网 HTTPS
          可达。
        </Notice>
      )}
      <div className="card flush mt16">
        <Table
          headers={[...s.columns.map((c) => c[1]), "操作"]}
          rows={rows.map((row) => [
            ...s.columns.map(([key]) => show(row[key], key)),
            <div className="actions">
              <Button onClick={() => open(row)}>编辑</Button>
              {["executors", "agents"].includes(kind) && (
                <Button
                  onClick={async () => {
                    if (
                      !confirm(
                        kind === "executors"
                          ? "生成新的安装令牌？旧令牌将立即失效，已有执行机需要重新安装。"
                          : "生成新的节点注册令牌？仅在目标节点上运行安装脚本。",
                      )
                    )
                      return;
                    try {
                      setToken(await post(`${s.url}/${row.id}/enrollment`, {}));
                      reload();
                    } catch (e) {
                      setMessage((e as Error).message);
                    }
                  }}
                >
                  部署 / 重装
                </Button>
              )}
              {!s.noDelete && (
                <Button
                  onClick={async () => {
                    if (
                      !confirm(
                        `删除“${row.name || row.email}”？有关联业务的记录会被保护。`,
                      )
                    )
                      return;
                    try {
                      await api(s.url + "/" + row.id, { method: "DELETE" });
                      reload();
                    } catch (e) {
                      setMessage((e as Error).message);
                    }
                  }}
                >
                  删除
                </Button>
              )}
            </div>,
          ])}
        />
      </div>
      {editing && (
        <Modal
          title={(editing.id ? "编辑" : "新增") + s.title}
          onClose={() => setEditing(null)}
        >
          <AsyncForm
            onSubmit={async () => {
              let body: RecordData = {};
              for (const f of s.fields) {
                if (
                  kind === "payments" &&
                  (values.version === "v1"
                    ? ["privateKey", "platformPublicKey"].includes(f.key)
                    : f.key === "merchantKey")
                )
                  continue;
                if (kind === "executors" && !editing.id && f.key === "status")
                  continue;
                const val = values[f.key];
                if (f.optional && !val) continue;
                body[f.key] = f.convert
                  ? f.convert(val)
                  : f.type === "number"
                    ? Number(val)
                    : val;
              }
              const response = await post(
                s.url + (editing.id ? "/" + editing.id : ""),
                body,
                editing.id ? s.method || "PUT" : "POST",
              );
              if (response.token || response.enrollmentToken)
                setToken(response);
              setEditing(null);
              reload();
            }}
          >
            <div className="form-grid">
              {s.fields
                .filter(
                  (f) =>
                    !(
                      kind === "payments" &&
                      (values.version === "v1"
                        ? ["privateKey", "platformPublicKey"].includes(f.key)
                        : f.key === "merchantKey")
                    ) &&
                    !(
                      kind === "executors" &&
                      !editing.id &&
                      f.key === "status"
                    ),
                )
                .map((f) =>
                  f.type === "checkbox" ? (
                    <Check
                      key={f.key}
                      checked={!!values[f.key]}
                      onChange={(e) =>
                        setValues({ ...values, [f.key]: e.target.checked })
                      }
                      label={f.label}
                    />
                  ) : f.options ? (
                    <Select
                      key={f.key}
                      label={f.label}
                      value={values[f.key]}
                      onChange={(e) =>
                        setValues({ ...values, [f.key]: e.target.value })
                      }
                    >
                      {f.options.map(([v, n]) => (
                        <option key={v} value={v}>
                          {n}
                        </option>
                      ))}
                    </Select>
                  ) : f.type === "textarea" ? (
                    <label className="field full" key={f.key}>
                      <span>{f.label}</span>
                      <textarea
                        value={values[f.key]}
                        onChange={(e) =>
                          setValues({ ...values, [f.key]: e.target.value })
                        }
                        rows={5}
                      />
                    </label>
                  ) : (
                    <Field
                      key={f.key}
                      label={f.label}
                      type={f.type || "text"}
                      value={values[f.key]}
                      onChange={(e) =>
                        setValues({ ...values, [f.key]: e.target.value })
                      }
                      required={!f.optional}
                      autoComplete={
                        f.type === "password" ? "new-password" : undefined
                      }
                      step={f.type === "number" ? "any" : undefined}
                    />
                  ),
                )}
            </div>
          </AsyncForm>
        </Modal>
      )}
      {token && (
        <Modal
          title="一键部署（令牌仅显示一次）"
          onClose={() => setToken(null)}
        >
          <Notice tone="orange">
            在目标 Debian VPS 的 root
            终端运行下面命令，再粘贴令牌。令牌不会进入命令行历史；不要发给客户。节点注册令牌有有效期，过期可重新生成。
          </Notice>
          <pre className="code-panel mt16">
            {`curl -fsSL --proto '=https' --tlsv1.2 https://raw.githubusercontent.com/mozziexwz/node/v0.1.0/agent.sh -o /tmp/msboost-agent-install.sh && bash /tmp/msboost-agent-install.sh --capability ${kind === "executors" ? "executor" : "relay"} --server '${location.origin}'`}
          </pre>
          {location.protocol !== "https:" && (
            <Notice tone="orange">
              此站点尚未配置 HTTPS。请先使用域名和 TLS
              部署网站，再复制安装命令；Agent 不接受公网明文 HTTP。
            </Notice>
          )}
          <Button
            onClick={() =>
              void copyText(
                `curl -fsSL --proto '=https' --tlsv1.2 https://raw.githubusercontent.com/mozziexwz/node/v0.1.0/agent.sh -o /tmp/msboost-agent-install.sh && bash /tmp/msboost-agent-install.sh --capability ${kind === "executors" ? "executor" : "relay"} --server '${location.origin}'`,
              ).catch((e) => setMessage(e.message))
            }
          >
            复制安装命令
          </Button>
          <pre className="code-panel mt16">
            {token.token || token.enrollmentToken}
          </pre>
          <Button
            className="mt16"
            onClick={() =>
              void copyText(token.token || token.enrollmentToken).catch((e) =>
                setMessage(e.message),
              )
            }
          >
            复制令牌
          </Button>
        </Modal>
      )}
    </>
  );
}

export function CodesPage({ invitations = false }: { invitations?: boolean }) {
  const url = "/api/admin/" + (invitations ? "invitations" : "cards"),
    key = invitations ? "invitations" : "cards";
  const { data, error, reload } = useData(
      url + (invitations ? "" : "?status=all"),
    ),
    [selected, setSelected] = useState<string[]>([]),
    [query, setQuery] = useState(""),
    [status, setStatus] = useState(""),
    [creating, setCreating] = useState(false),
    [message, setMessage] = useState(""),
    [editing, setEditing] = useState<RecordData | null>(null);
  const rows = array(data, key)
    .map((x) =>
      invitations
        ? {
            ...x,
            status: x.archived
              ? "archived"
              : !x.enabled
                ? "disabled"
                : (x.uses?.length || 0) >= x.maxUses
                  ? "used"
                  : "active",
          }
        : x,
    )
    .filter(
      (x) =>
        (!query ||
          JSON.stringify(x).toLowerCase().includes(query.toLowerCase())) &&
        (status ? x.status === status : x.status !== "archived"),
    );
  async function batch(action: string) {
    if (!selected.length) return;
    if (
      action === "delete" &&
      !confirm("删除所选未使用代码；已使用代码将保留使用记录并归档。")
    )
      return;
    try {
      const r = await post(url + "/batch", { ids: selected, action });
      if (action === "export") download(r, `${key}-export.json`);
      reload();
      setSelected([]);
    } catch (e) {
      setMessage((e as Error).message);
    }
  }
  return (
    <>
      <Header
        title={invitations ? "邀请码管理" : "卡密管理"}
        sub={
          invitations
            ? "邀请码仅用于注册；每码使用次数独立计算。"
            : "卡密只兑换账户余额，不直接兑换套餐。"
        }
      >
        <Button primary onClick={() => setCreating(true)}>
          批量生成
        </Button>
      </Header>
      <ErrorNotice error={error || message} />
      <div className="filterbar">
        <input
          aria-label="搜索代码"
          placeholder="搜索完整代码 / 批次"
          value={query}
          onChange={(e) => setQuery(e.target.value)}
        />
        <select
          aria-label="状态筛选"
          value={status}
          onChange={(e) => setStatus(e.target.value)}
        >
          <option value="">未归档记录</option>
          {[
            ["active", "未使用 / 启用"],
            ["disabled", "停用"],
            ["used", "已使用"],
            ["archived", "已删除（归档）"],
          ].map(([x, label]) => (
            <option key={x} value={x}>
              {label}
            </option>
          ))}
        </select>
      </div>
      <div className="actions mt16">
        <span>已选择 {selected.length} 项</span>
        {[
          ["enable", "启用"],
          ["disable", "停用"],
          ["export", "导出所选"],
          ["delete", "删除"],
        ].map(([v, n]) => (
          <Button
            disabled={!selected.length}
            key={v}
            onClick={() => void batch(v)}
          >
            {n}
          </Button>
        ))}
        <Button onClick={() => setSelected([])}>清除选择</Button>
        <Button onClick={() => download({ [key]: rows }, `${key}-all.json`)}>
          导出当前筛选
        </Button>
      </div>
      <div className="card flush mt16">
        <Table
          headers={[
            <input
              type="checkbox"
              aria-label="全选筛选结果"
              checked={
                rows.length > 0 && rows.every((x) => selected.includes(x.id))
              }
              onChange={(e) =>
                setSelected(e.target.checked ? rows.map((x) => x.id) : [])
              }
            />,
            invitations ? "完整邀请码" : "完整卡密",
            invitations ? "使用次数" : "面额",
            "状态",
            "批次",
            ...(!invitations ? ["使用用户", "使用时间"] : []),
            "操作",
          ]}
          rows={rows.map((c) => [
            <input
              type="checkbox"
              aria-label={"选择 " + c.code}
              checked={selected.includes(c.id)}
              onChange={(e) =>
                setSelected(
                  e.target.checked
                    ? [...selected, c.id]
                    : selected.filter((x) => x !== c.id),
                )
              }
            />,
            <span className="mono">{c.code}</span>,
            invitations
              ? `${c.usedCount ?? c.uses?.length ?? 0} / ${c.maxUses}`
              : `¥ ${money(c.amountCents)}`,
            (
              {
                active: "未使用 / 启用",
                disabled: "停用",
                used: "已使用",
                archived: "已归档",
              } as Record<string, string>
            )[c.status] || c.status,
            c.batch || c.note || "—",
            ...(!invitations
              ? [c.usedByEmail || c.usedBy || "—", date(c.usedAt)]
              : []),
            <div className="actions">
              <Button
                onClick={() =>
                  void copyText(c.code).catch((e) => setMessage(e.message))
                }
              >
                复制
              </Button>
              {invitations && (
                <Button onClick={() => setEditing(c)}>次数与记录</Button>
              )}
            </div>,
          ])}
        />
      </div>
      {creating && (
        <Modal
          title={invitations ? "批量生成邀请码" : "批量生成卡密"}
          onClose={() => setCreating(false)}
        >
          <AsyncForm
            label="生成"
            onSubmit={(f) =>
              post(url, {
                count: Number(f.get("count")),
                ...(invitations
                  ? {
                      maxUses: Number(f.get("maxUses")),
                      note: String(f.get("batch")),
                    }
                  : { amountCents: Math.round(Number(f.get("amount")) * 100) }),
                batch: String(f.get("batch")),
              })
            }
            onDone={() => {
              setCreating(false);
              reload();
            }}
          >
            <Field
              label="生成数量"
              name="count"
              type="number"
              min={1}
              max={1000}
              defaultValue={5}
              required
            />
            {invitations ? (
              <Field
                label="每个码允许使用次数"
                name="maxUses"
                type="number"
                min={1}
                defaultValue={1}
                required
              />
            ) : (
              <Field
                label="每张面额（元）"
                name="amount"
                type="number"
                min={0.01}
                step="0.01"
                defaultValue={10}
                required
              />
            )}
            <Field label="批次 / 备注" name="batch" />
          </AsyncForm>
        </Modal>
      )}
      {editing && (
        <Modal title="邀请码使用次数与记录" onClose={() => setEditing(null)}>
          <p className="mono">{editing.code}</p>
          <AsyncForm
            onSubmit={(f) =>
              post(
                url + "/" + editing.id,
                {
                  maxUses: Number(f.get("maxUses")),
                  enabled: editing.enabled,
                  note: editing.note || "",
                },
                "PUT",
              )
            }
            onDone={() => {
              setEditing(null);
              reload();
            }}
          >
            <Field
              label="允许使用次数"
              type="number"
              name="maxUses"
              defaultValue={editing.maxUses}
              min={1}
            />
          </AsyncForm>
          <Table
            headers={["用户", "使用时间"]}
            rows={(editing.uses || []).map((v: RecordData) => [
              v.email || v.userId,
              date(v.createdAt || v.at),
            ])}
          />
        </Modal>
      )}
    </>
  );
}

const featureNames: Record<string, string> = {
  register: "公开注册",
  invite: "注册需要邀请码",
  deploy: "部署 MSBOOST",
  relay: "自备中转配置",
  dd: "DD 系统",
  paidCreate: "新增增值转发",
  planSale: "套餐销售",
  cards: "卡密兑换",
  paywx: "微信支付",
  payali: "支付宝支付",
  monitor: "用户线路监控",
  localSave: "免费配置本机自动保存",
  publicArticles: "公开阅读文章",
  attachments: "文章附件下载",
  maintenance: "维护模式",
  registrationEmailVerificationRequired: "注册时必须验证邮箱",
  freeToolsRequireVerifiedEmail: "免费工具仅限已验证邮箱用户",
  purchaseRequireVerifiedEmail: "购买套餐必须验证邮箱",
};
export function Settings({ onRefresh }: { onRefresh: () => void }) {
  const { data, error, reload } = useData("/api/admin/settings");
  const [tab, setTab] = useState("features"),
    [values, setValues] = useState<RecordData>({});
  useEffect(() => {
    if (data) setValues(data.settings || data);
  }, [data]);
  const keys =
    tab === "registration"
      ? [
          "register",
          "invite",
          "registrationEmailVerificationRequired",
          "freeToolsRequireVerifiedEmail",
          "purchaseRequireVerifiedEmail",
        ]
      : Object.keys(featureNames).filter(
          (k) =>
            ![
              "register",
              "invite",
              "registrationEmailVerificationRequired",
              "freeToolsRequireVerifiedEmail",
              "purchaseRequireVerifiedEmail",
            ].includes(k),
        );
  return (
    <>
      <Header title="系统设置" sub="功能开关由服务端即时校验。" />
      <ErrorNotice error={error} />
      <div className="tabs">
        {[
          ["features", "功能开关"],
          ["registration", "注册与验证"],
          ["captcha", "人机验证"],
          ["smtp", "SMTP 邮箱"],
          ["limits", "操作限额"],
        ].map(([k, t]) => (
          <button
            className={tab === k ? "active" : ""}
            key={k}
            onClick={() => setTab(k)}
          >
            {t}
          </button>
        ))}
      </div>
      <div className="card mt16">
        {tab === "captcha" ? (
          <AsyncForm
            onSubmit={async (f) => {
              await post(
                "/api/admin/settings",
                {
                  turnstile: f.get("enabled") === "on",
                  turnstileSiteKey: f.get("siteKey"),
                  turnstileSecret: f.get("secret"),
                },
                "PUT",
              );
              reload();
              onRefresh();
            }}
          >
            <Field
              label="Turnstile Site Key"
              name="siteKey"
              defaultValue={values.turnstileSiteKey}
            />
            <Field
              label="Turnstile Secret Key（留空保留）"
              type="password"
              name="secret"
              autoComplete="new-password"
            />
            <Check
              label="启用 Cloudflare Turnstile"
              name="enabled"
              defaultChecked={values.turnstile}
            />
            <Notice>
              请先在 Cloudflare
              配置本站域名。启用后，登录、注册与注册邮件发送均需真实验证。
            </Notice>
          </AsyncForm>
        ) : tab === "smtp" ? (
          <AsyncForm
            onSubmit={async (f) => {
              await post(
                "/api/admin/settings",
                {
                  smtp: true,
                  smtpConfig: {
                    host: f.get("host"),
                    port: Number(f.get("port")),
                    sender: f.get("sender"),
                    name: f.get("name"),
                    secret: f.get("secret"),
                    encryption: f.get("encryption"),
                  },
                },
                "PUT",
              );
              reload();
              onRefresh();
            }}
          >
            <div className="form-grid">
              <Field
                label="SMTP 主机"
                name="host"
                defaultValue={values.smtpConfig?.host}
                required
              />
              <Field
                label="端口"
                name="port"
                type="number"
                defaultValue={values.smtpConfig?.port || 465}
                required
              />
              <Field
                label="发件邮箱"
                type="email"
                name="sender"
                defaultValue={values.smtpConfig?.sender}
                required
              />
              <Field
                label="发件人显示名称"
                name="name"
                defaultValue={values.smtpConfig?.name}
              />
              <Field
                label="授权码 / 密码（留空保留）"
                type="password"
                name="secret"
                autoComplete="new-password"
              />
              <Select
                label="加密方式"
                name="encryption"
                defaultValue={values.smtpConfig?.encryption || "tls"}
              >
                <option value="tls">TLS</option>
                <option value="starttls">STARTTLS</option>
              </Select>
            </div>
            <Notice>
              发信配置保存后，请发送测试邮件；测试通过后才能开启任一邮箱验证策略。
            </Notice>
            <Button
              className="mt16"
              onClick={async () => {
                try {
                  await post("/api/admin/smtp/test", {});
                  alert("测试邮件已发送");
                  reload();
                } catch (e) {
                  alert((e as Error).message);
                }
              }}
            >
              发送测试邮件
            </Button>
          </AsyncForm>
        ) : tab === "limits" ? (
          <AsyncForm
            onSubmit={(f) =>
              post(
                "/api/admin/settings",
                {
                  limits: Object.fromEntries(
                    ["deploy", "relay", "dd", "fingerprint"].map((k) => [
                      k,
                      {
                        minutes: Number(f.get(k + "Minutes")),
                        count: Number(f.get(k + "Count")),
                      },
                    ]),
                  ),
                },
                "PUT",
              )
            }
            onDone={reload}
          >
            <div className="grid2">
              {["deploy", "relay", "dd", "fingerprint"].map((k) => (
                <div key={k}>
                  <h3>
                    {
                      (
                        {
                          deploy: "部署 MSBOOST",
                          relay: "自备中转",
                          dd: "DD 系统",
                          fingerprint: "SSH 指纹检查",
                        } as Record<string, string>
                      )[k]
                    }
                  </h3>
                  <div className="form-grid mt16">
                    <Field
                      label="窗口（分钟）"
                      type="number"
                      name={k + "Minutes"}
                      min={1}
                      defaultValue={
                        values.limits?.[k]?.minutes ||
                        (k === "fingerprint" ? 30 : 15)
                      }
                    />
                    <Field
                      label="允许次数"
                      type="number"
                      name={k + "Count"}
                      min={1}
                      defaultValue={
                        values.limits?.[k]?.count ||
                        (k === "fingerprint" ? 10 : 5)
                      }
                    />
                  </div>
                </div>
              ))}
            </div>
          </AsyncForm>
        ) : (
          <AsyncForm
            onSubmit={() =>
              post(
                "/api/admin/settings",
                Object.fromEntries(keys.map((k) => [k, !!values[k]])),
                "PUT",
              )
            }
            onDone={() => {
              reload();
              onRefresh();
            }}
          >
            {tab === "registration" && (
              <Notice tone="orange">
                注册验证与免费工具验证相互独立。关闭验证策略不会把未验证用户标记为已验证。
              </Notice>
            )}
            <div className="setting-list">
              {keys.map((k) => (
                <Check
                  key={k}
                  label={featureNames[k]}
                  checked={!!values[k]}
                  onChange={(e) =>
                    setValues({ ...values, [k]: e.target.checked })
                  }
                />
              ))}
            </div>
          </AsyncForm>
        )}
      </div>
    </>
  );
}
export function AdminRules() {
  const { data, error } = useData("/api/admin/user-rules");
  return (
    <>
      <Header title="用户中转" sub="查看每位用户的站内转发、目标及执行状态。" />
      <ErrorNotice error={error} />
      <div className="card flush">
        <Table
          headers={["用户", "线路", "入口", "目标", "状态", "计费流量"]}
          rows={array(data, "rules").map((r) => [
            <span className="mono">{r.userId}</span>,
            r.routeName,
            `${r.entryAddress}:${r.entryPort}`,
            `${r.targetHost}:${r.targetPort}`,
            r.state,
            (r.trafficBytes / 1e9).toFixed(2) + " GB",
          ])}
        />
      </div>
    </>
  );
}
