import { useEffect, useState } from "react";
import { ArrowRight, ShieldCheck, Upload, Download } from "lucide-react";
import { api, post, requestID, download, RecordData, array } from "./api";
import { saveLocal, getLocal, deleteLocal } from "./vault";
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
  date,
  useData,
  ErrorNotice,
  Empty,
} from "./ui";
export type SSH = {
  host: string;
  port: number;
  user: string;
  password: string;
  fingerprint: string;
};
const newSSH = (): SSH => ({
  host: "",
  port: 22,
  user: "root",
  password: "",
  fingerprint: "",
});
export function SSHFields({
  value,
  onChange,
  title,
}: {
  value: SSH;
  onChange: (s: SSH) => void;
  title: string;
}) {
  const [checking, setChecking] = useState(false),
    [observed, setObserved] = useState(""),
    [error, setError] = useState("");
  function change(k: keyof SSH, v: string | number) {
    onChange({
      ...value,
      [k]: v,
      ...(["host", "port"].includes(k) ? { fingerprint: "" } : {}),
    });
    if (["host", "port"].includes(k)) setObserved("");
  }
  return (
    <>
      <div className="form-grid">
        <Field
          label={title}
          value={value.host}
          onChange={(e) => change("host", e.target.value)}
          placeholder="填写公网 IP 地址"
          disabled={checking}
          required
        />
        <Field
          label="SSH 端口"
          type="number"
          min={1}
          max={65535}
          value={value.port}
          disabled={checking}
          onChange={(e) => change("port", Number(e.target.value))}
          required
        />
        <Field
          label="SSH 用户名"
          value={value.user}
          onChange={(e) => change("user", e.target.value)}
          required
        />
        <Field
          label="SSH 密码"
          value={value.password}
          onChange={(e) => change("password", e.target.value)}
          type="password"
          autoComplete="off"
          hint="仅用于本次任务，不进入任务记录。"
          required
        />
      </div>
      <div className="mt8">
        <Button
          disabled={checking || !value.host}
          onClick={async () => {
            setChecking(true);
            setError("");
            try {
              const result = await post("/api/fingerprints", {
                host: value.host,
                port: value.port,
              });
              setObserved(result.fingerprint);
            } catch (e) {
              setError((e as Error).message);
            } finally {
              setChecking(false);
            }
          }}
        >
          <ShieldCheck size={16} />
          {checking ? "正在检查…" : "检查 SSH 主机指纹"}
        </Button>
        {observed && (
          <div className="mt16">
            <div className="code-panel">{observed}</div>
            <Check
              checked={value.fingerprint === observed}
              onChange={(e) =>
                onChange({
                  ...value,
                  fingerprint: e.target.checked ? observed : "",
                })
              }
              label="我已与 VPS 控制台提供的主机指纹核对一致"
            />
          </div>
        )}
        <ErrorNotice error={error} />
      </div>
    </>
  );
}
export function ConfigUpload({
  onLoad,
}: {
  onLoad: (d: RecordData | null) => void;
}) {
  const [error, setError] = useState(""),
    [name, setName] = useState("");
  return (
    <div className="upload-zone">
      <Upload size={24} className="orange" />
      <h4 className="mt8">上传 MSBOOST 配置 JSON</h4>
      <input
        type="file"
        accept=".json,application/json"
        aria-label="上传节点配置 JSON"
        onChange={async (e) => {
          onLoad(null);
          const f = e.target.files?.[0];
          setError("");
          setName("");
          if (!f) return;
          try {
            if (f.size > 128 * 1024) throw new Error("配置文件不能大于 128 KB");
            const d = JSON.parse(await f.text());
            if (
              !Array.isArray(d.profiles) ||
              d.profiles.length !== 1 ||
              d.profiles[0]?.servers?.length !== 1 ||
              d.profiles[0]?.servers[0]?.portBindings?.length !== 1
            )
              throw new Error(
                "请选择仅含一个 profile、一个服务器和一个端口的配置",
              );
            if (!d.profiles[0].user?.name || !d.profiles[0].user?.password)
              throw new Error("配置缺少节点认证");
            onLoad(d);
            setName(f.name);
          } catch (e) {
            setError((e as Error).message);
          }
        }}
      />
      <small>{name || "只用于当前任务；免费工具配置保存在当前浏览器。"}</small>
      <ErrorNotice error={error} />
    </div>
  );
}
const states: Record<string, string> = {
  queued: "等待执行机",
  running: "执行中",
  succeeded: "已完成",
  failed: "失败",
  unknown: "提交结果不明确",
  interrupted: "已中断",
  executed: "已执行 DD 操作",
};
export function TaskDetail({
  task,
  onClose,
  owner,
  save = true,
}: {
  task: RecordData;
  onClose: () => void;
  owner: string;
  save?: boolean;
}) {
  const [current, setCurrent] = useState(task),
    [config, setConfig] = useState<unknown>(null),
    [status, setStatus] = useState(""),
    [error, setError] = useState("");
  useEffect(() => {
    let live = true;
    const timer = setInterval(() => {
      if (!["queued", "running"].includes(current.state)) return;
      api("/api/tasks/" + task.id)
        .then((r) => {
          if (live) setCurrent(r.task || r);
        })
        .catch((e) => {
          if (live) setError(e.message);
        });
    }, 2000);
    return () => {
      live = false;
      clearInterval(timer);
    };
  }, [task.id, current.state]);
  useEffect(() => {
    if (
      !current.configAvailable ||
      current.kind === "dd" ||
      current.userId !== owner
    )
      return;
    let live = true;
    api("/api/tasks/" + current.id + "/config")
      .then(async (d) => {
        if (!live) return;
        setConfig(d);
        if (!save) {
          setStatus("仅本次可下载");
          return;
        }
        try {
          await saveLocal(
            owner,
            current.id,
            d,
            current.kind === "deploy" ? "直连.json" : "自备中转.json",
          );
          if (live) setStatus("已存当前浏览器");
        } catch {
          if (live) setStatus("仅本次可下载：浏览器存储不可用，请立即下载。");
        }
      })
      .catch((e) => {
        if (live) setError(e.message);
      });
    return () => {
      live = false;
    };
  }, [current.configAvailable, current.id, current.kind, owner, save]);
  return (
    <Modal title="任务详情" onClose={onClose}>
      <div className="between">
        <Badge
          tone={
            ["failed", "unknown", "interrupted"].includes(current.state)
              ? "red"
              : "orange"
          }
        >
          {states[current.state] || current.state}
        </Badge>
        <small>{date(current.createdAt)}</small>
      </div>
      <div className="kv">
        <span>任务编号</span>
        <strong className="mono">{current.id}</strong>
      </div>
      <div className="kv">
        <span>服务器</span>
        <strong>{current.host}</strong>
      </div>
      <p className="mt16">
        {current.message || current.phase || "等待执行机接收任务"}
      </p>
      {current.health && (
        <Table
          headers={["检查项目", "结果"]}
          rows={Object.entries(current.health).map(([k, v]) => [k, String(v)])}
        />
      )}
      <div className="mt16">
        {current.kind === "dd" && current.state === "executed" ? (
          <Notice tone="orange">
            已执行 DD 操作，请等待15分钟以上，再执行部署
            MSBOOST。此状态仅表示操作已提交，不表示系统安装完成。
          </Notice>
        ) : current.kind === "dd" && current.state === "unknown" ? (
          <Notice tone="red">
            重装提交结果不明确。请先使用 VPS 控制台检查状态，不要直接重复提交。
          </Notice>
        ) : null}
      </div>
      {current.hops && (
        <pre className="code-panel mt16">
          {JSON.stringify(current.hops, null, 2)}
        </pre>
      )}
      {config != null && (
        <div className="mt24">
          <Notice tone="orange">
            {status || "配置已就绪"}
            。配置已保存在当前浏览器时，请下载备份。清除网站数据或更换设备后，本机配置可能无法恢复。
          </Notice>
          <Button
            primary
            className="mt16"
            onClick={() =>
              download(
                config,
                current.kind === "deploy" ? "直连.json" : "自备中转.json",
              )
            }
          >
            <Download size={16} />
            下载配置
          </Button>
        </div>
      )}
      <ErrorNotice error={error} />
    </Modal>
  );
}
export function ToolPage({
  kind,
  user,
  settings,
  onNavigate,
}: {
  kind: string;
  user: RecordData;
  settings: RecordData;
  onNavigate: (p: string) => void;
}) {
  const [ssh, setSSH] = useState(newSSH),
    [front, setFront] = useState(newSSH),
    [useFront, setUseFront] = useState(false),
    [mode, setMode] = useState("fresh"),
    [portMode, setPortMode] = useState("keep"),
    [passwordMode, setPasswordMode] = useState("keep"),
    [newPort, setNewPort] = useState(""),
    [newPassword, setNewPassword] = useState(""),
    [config, setConfig] = useState<RecordData | null>(null),
    [erase, setErase] = useState(false),
    [busy, setBusy] = useState(false),
    [error, setError] = useState(""),
    [task, setTask] = useState<RecordData | null>(null);
  const { data: quotas, reload } = useData("/api/tasks");
  const title =
    { deploy: "部署 MSBOOST", relay: "配置自备中转", dd: "DD 系统" }[kind] ||
    "";
  const gate =
    user.role !== "admin" &&
    settings.freeToolsRequireVerifiedEmail &&
    !user.emailVerifiedAt;
  const q = quotas?.limits?.[kind];
  async function submit(e: React.FormEvent) {
    e.preventDefault();
    if (busy) return;
    setError("");
    if (
      !ssh.fingerprint ||
      (kind === "relay" && useFront && !front.fingerprint)
    ) {
      setError("请先检查并核对每台服务器的 SSH 主机指纹");
      return;
    }
    if (kind === "relay" && !config) {
      setError("请上传节点配置");
      return;
    }
    setBusy(true);
    try {
      const r = await post(
        "/api/tasks",
        {
          kind,
          ssh,
          ...(kind === "deploy" ? { mode } : {}),
          ...(kind === "relay"
            ? { clientConfig: config, ...(useFront ? { front } : {}) }
            : {}),
          ...(kind === "dd"
            ? {
                dd: {
                  confirmErase: erase,
                  portMode,
                  newPort: Number(newPort) || undefined,
                  passwordMode,
                  newPassword: passwordMode === "new" ? newPassword : undefined,
                },
              }
            : {}),
        },
        "POST",
        { "Idempotency-Key": requestID() },
      );
      setTask(r.task || r);
      setErase(false);
      setSSH({ ...ssh, password: "" });
      setFront({ ...front, password: "" });
      setNewPassword("");
      setConfig(null);
      reload();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }
  return (
    <>
      <Header
        title={title}
        sub={
          kind === "deploy"
            ? "在自备 VPS 上部署，并下载直连配置。"
            : kind === "relay"
              ? "上传配置并填写中转服务器信息，系统自动分配端口。"
              : "仅提供 Debian 12，默认保留当前 SSH 端口和密码。"
        }
      />
      {gate ? (
        <div className="card email-gate41">
          <ShieldCheck size={30} className="orange" />
          <h2>请先验证注册邮箱</h2>
          <p>请先验证注册邮箱，再使用部署 MSBOOST、自备中转和 DD 系统。</p>
          <div className="actions">
            <Button primary onClick={() => onNavigate("account")}>
              前往验证邮箱
            </Button>
            <Button onClick={() => onNavigate("tasks")}>查看历史任务</Button>
          </div>
        </div>
      ) : settings[kind] === false ? (
        <Notice>该功能当前由管理员关闭，已有任务仍可查看。</Notice>
      ) : (
        <div className="tool-layout">
          <div className="card">
            {q && (
              <div className="quota">
                <span>
                  每 <b>{q.minutes} 分钟</b>最多 <b>{q.count} 次</b> · 剩余{" "}
                  <b>{q.remaining} 次</b>
                </span>
                <small>
                  {q.remaining ? "当前可操作" : `下次可用：${date(q.nextAt)}`} ·
                  失败操作也计次
                </small>
              </div>
            )}
            <form onSubmit={submit}>
              {kind === "relay" && (
                <>
                  <div className="step-label">
                    <b>1</b>上传节点配置
                  </div>
                  <ConfigUpload onLoad={setConfig} />
                  <div className="step-label">
                    <b>2</b>填写中转服务器
                  </div>
                </>
              )}
              <SSHFields
                title={kind === "relay" ? "中转服务器地址" : "服务器 IP 地址"}
                value={ssh}
                onChange={setSSH}
              />
              {kind === "deploy" && (
                <>
                  <fieldset className="fieldset">
                    <legend>执行策略</legend>
                    <div className="radio-cards">
                      {[
                        [
                          "fresh",
                          "全新安装",
                          "重新部署受管的 MSBOOST 组件，不重装操作系统。",
                        ],
                        [
                          "repair",
                          "安全升级 / 修复",
                          "优先保留已有认证与端口，失败可回滚受管变更。",
                        ],
                      ].map(([v, t, d]) => (
                        <label className="radio-card" key={v}>
                          <input
                            type="radio"
                            checked={mode === v}
                            onChange={() => setMode(v)}
                          />
                          <span>
                            <strong>{t}</strong>
                            <small>{d}</small>
                          </span>
                        </label>
                      ))}
                    </div>
                  </fieldset>
                  <Notice tone="orange">
                    本功能仅限游戏用途，禁止用于翻墙、公共代理、违法活动或其他与游戏无关的用途。(MSBOOST
                    将屏蔽部分网站)
                  </Notice>
                </>
              )}
              {kind === "relay" && (
                <>
                  <Check
                    checked={useFront}
                    onChange={(e) => setUseFront(e.target.checked)}
                    label="使用自备前置机"
                  />
                  {useFront && (
                    <div className="mt16">
                      <Notice tone="orange">
                        前置机 → 自备中转机 → MSBOOST
                        节点。请同时填写两台服务器信息。
                      </Notice>
                      <div className="mt16">
                        <SSHFields
                          title="前置机 IP 地址"
                          value={front}
                          onChange={setFront}
                        />
                      </div>
                    </div>
                  )}
                  <div className="flow">
                    {[
                      "客户电脑",
                      ...(useFront ? ["自备前置机"] : []),
                      "自备中转机",
                      "MSBOOST 节点",
                    ].map((x, i) => (
                      <span className="flex" key={x}>
                        {i > 0 && <ArrowRight size={13} />}
                        <span className="flow-node">{x}</span>
                      </span>
                    ))}
                  </div>
                  <Notice>
                    入口使用实际转发服务器的 IP；监听端口随机分配，每端口固定 5
                    Mbps。
                  </Notice>
                </>
              )}
              {kind === "dd" && (
                <>
                  <fieldset className="fieldset">
                    <legend>重装参数</legend>
                    <div className="form-grid">
                      <Field label="重装系统" value="Debian 12" disabled />
                      <Select
                        label="重装后 SSH 端口"
                        value={portMode}
                        onChange={(e) => setPortMode(e.target.value)}
                      >
                        <option value="keep">保持不变</option>
                        <option value="new">输入新端口</option>
                      </Select>
                      {portMode === "new" && (
                        <Field
                          label="新 SSH 端口"
                          type="number"
                          min={1}
                          max={65535}
                          value={newPort}
                          onChange={(e) => setNewPort(e.target.value)}
                          required
                        />
                      )}
                      <Select
                        label="重装密码策略"
                        value={passwordMode}
                        onChange={(e) => setPasswordMode(e.target.value)}
                      >
                        <option value="keep">保持不变</option>
                        <option value="new">输入新密码</option>
                      </Select>
                      {passwordMode === "new" && (
                        <Field
                          label="新 root 密码"
                          type="password"
                          value={newPassword}
                          onChange={(e) => setNewPassword(e.target.value)}
                          required
                        />
                      )}
                    </div>
                  </fieldset>
                  <Notice tone="red">
                    <strong>DD 会清除服务器原有系统和数据。</strong>
                    请先备份。操作提交后请等待15分钟以上，再执行部署 MSBOOST。
                  </Notice>
                  <Check
                    checked={erase}
                    onChange={(e) => setErase(e.target.checked)}
                    required
                    label="我已备份重要数据，并理解重装会清除原系统和数据"
                  />
                </>
              )}
              <ErrorNotice error={error} />
              <div className="form-actions">
                <Button type="submit" primary disabled={busy}>
                  {busy
                    ? "正在提交…"
                    : kind === "deploy"
                      ? "开始部署"
                      : kind === "relay"
                        ? "配置转发"
                        : "执行 DD"}
                  <ArrowRight size={15} />
                </Button>
              </div>
            </form>
          </div>
          <aside className="stack" style={{ alignContent: "start" }}>
            <div className="card">
              <h3>{kind === "dd" ? "DD 提交后" : "配置文件在哪里？"}</h3>
              <p className="muted mt8">
                {kind === "dd"
                  ? "“已执行 DD 操作”不表示操作系统已安装完成，请等待 15 分钟以上再部署。"
                  : "操作完成后下载配置，也可以在当前浏览器的任务记录里再次下载。"}
              </p>
              <Button className="mt16" onClick={() => onNavigate("tasks")}>
                查看任务记录
              </Button>
            </div>
            <div className="card">
              <h3>操作须知</h3>
              <p className="muted mt8">
                仅操作自己有权管理的服务器。密码和配置请妥善保管。
              </p>
              <Button className="mt16" onClick={() => onNavigate("tutorials")}>
                公告/教程
              </Button>
            </div>
          </aside>
        </div>
      )}
      {task && (
        <TaskDetail
          task={task}
          owner={user.id}
          save={settings.localSave !== false}
          onClose={() => setTask(null)}
        />
      )}
    </>
  );
}
export function TasksPage({ user }: { user: RecordData }) {
  const { data, error, reload } = useData(
    user.role === "admin" ? "/api/admin/tasks" : "/api/tasks",
  );
  const [selected, setSelected] = useState<RecordData | null>(null),
    [message, setMessage] = useState(""),
    [files, setFiles] = useState<Record<string, boolean>>({}),
    [restoring, setRestoring] = useState<RecordData | null>(null),
    [restoreConfig, setRestoreConfig] = useState<RecordData | null>(null);
  const tasks = array(data, "tasks");
  useEffect(() => {
    let alive = true;
    Promise.all(
      tasks.map(async (t) => [
        t.id,
        !!(await getLocal(user.id, t.id).catch(() => null)),
      ]),
    ).then((values) => {
      if (alive) setFiles(Object.fromEntries(values));
    });
    return () => {
      alive = false;
    };
  }, [data, user.id]);
  return (
    <>
      <Header
        title={user.role === "admin" ? "任务审计" : "任务记录"}
        sub="任务结果保存在服务器；免费配置只保存在生成时的浏览器。"
      >
        <Button onClick={reload}>刷新</Button>
      </Header>
      <ErrorNotice error={error || message} />
      <Notice tone="orange">
        免费配置请下载备份。清除网站数据或更换设备后，本机配置可能无法恢复；增值线路文件请到线路页从服务器下载。
      </Notice>
      <div className="card flush mt24">
        <Table
          headers={["任务 / 时间", "服务器", "状态", "配置", "操作"]}
          rows={tasks.map((t) => [
            <>
              {(
                {
                  deploy: "部署 MSBOOST",
                  relay: "自备中转",
                  dd: "DD 系统",
                } as Record<string, string>
              )[t.kind] || t.kind}
              <small>{date(t.createdAt)}</small>
            </>,
            t.host,
            <Badge
              tone={
                ["failed", "unknown", "interrupted"].includes(t.state)
                  ? "red"
                  : "orange"
              }
            >
              {states[t.state] || t.state}
            </Badge>,
            t.kind === "dd"
              ? "不生成配置"
              : files[t.id]
                ? "已存当前浏览器"
                : t.configAvailable
                  ? "临时交付可用"
                  : "本机无文件",
            <div className="actions">
              <Button onClick={() => setSelected(t)}>详情</Button>
              {!files[t.id] &&
                t.kind !== "dd" &&
                t.configHost &&
                user.role !== "admin" && (
                  <Button
                    onClick={() => {
                      setRestoring(t);
                      setRestoreConfig(null);
                    }}
                  >
                    导入恢复
                  </Button>
                )}
              {files[t.id] && (
                <>
                  <Button
                    onClick={async () => {
                      try {
                        const f = await getLocal(user.id, t.id);
                        if (f) download(f.data, f.name);
                      } catch (e) {
                        setMessage((e as Error).message);
                      }
                    }}
                  >
                    下载
                  </Button>
                  <Button
                    onClick={async () => {
                      if (
                        !confirm(
                          "删除当前浏览器中的这份配置？请确认已有下载备份。",
                        )
                      )
                        return;
                      await deleteLocal(user.id, t.id);
                      setFiles({ ...files, [t.id]: false });
                    }}
                  >
                    删除本地
                  </Button>
                </>
              )}
            </div>,
          ])}
        />
      </div>
      {selected && (
        <TaskDetail
          owner={user.id}
          task={selected}
          onClose={() => {
            setSelected(null);
            reload();
          }}
        />
      )}
      {restoring && (
        <Modal title="导入本机配置" onClose={() => setRestoring(null)}>
          <ConfigUpload onLoad={setRestoreConfig} />
          <Notice>
            只恢复当前浏览器中的副本，不会修改服务器。文件入口必须与原任务一致。
          </Notice>
          <Button
            primary
            className="mt16"
            onClick={async () => {
              try {
                if (!restoreConfig) throw new Error("请选择配置文件");
                const server = restoreConfig.profiles[0].servers[0];
                const host = (server.ipAddress || server.domainName || "")
                  .toLowerCase()
                  .replace(/\.$/, "");
                if (
                  host !==
                    String(restoring.configHost)
                      .toLowerCase()
                      .replace(/\.$/, "") ||
                  Number(server.portBindings[0].port) !==
                    Number(restoring.configPort) ||
                  String(server.portBindings[0].protocol).toLowerCase() !==
                    "tcp"
                )
                  throw new Error("文件入口与该任务不一致");
                restoreConfig.socks5Port = 10086;
                await saveLocal(
                  user.id,
                  restoring.id,
                  restoreConfig,
                  restoring.kind === "deploy" ? "直连.json" : "自备中转.json",
                );
                setFiles({ ...files, [restoring.id]: true });
                setRestoring(null);
                setRestoreConfig(null);
              } catch (e) {
                setMessage((e as Error).message);
              }
            }}
          >
            保存到当前浏览器
          </Button>
        </Modal>
      )}
      {!tasks.length && !error && (
        <Empty>还没有任务。请从一项免费工具开始。</Empty>
      )}
    </>
  );
}
