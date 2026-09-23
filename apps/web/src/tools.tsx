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
  trustMode?: "tofu" | "strict";
  replaceFingerprint?: string;
  _rememberedFingerprint?: string;
};
const newSSH = (): SSH => ({
  host: "",
  port: 22,
  user: "root",
  password: "",
  fingerprint: "",
  trustMode: "tofu",
});
export async function prepareSSH(
  value: SSH,
  onChange?: (s: SSH) => void,
): Promise<SSH> {
  const { _rememberedFingerprint: _unused, ...input } = value;
  if (value.trustMode === "strict") {
    if (!value.fingerprint)
      throw new Error("请展开高级设置，核对这台服务器的身份后再继续。");
    return input;
  }
  const probe = await post("/api/fingerprints", {
    host: value.host,
    port: value.port,
  });
  const previous = String(probe.rememberedFingerprint || "");
  if (
    previous &&
    previous !== probe.fingerprint &&
    (value.replaceFingerprint !== previous ||
      value.fingerprint !== probe.fingerprint)
  ) {
    onChange?.({
      ...value,
      fingerprint: probe.fingerprint,
      replaceFingerprint: "",
      _rememberedFingerprint: previous,
    });
    throw new Error(
      "这台服务器的身份与上次不同，操作已暂停。若刚重装过服务器，请到服务商控制台核对，再在高级设置中确认；没有重装过请先联系服务商。",
    );
  }
  return {
    ...input,
    trustMode: "tofu",
    fingerprint: probe.fingerprint,
    replaceFingerprint:
      previous !== probe.fingerprint ? value.replaceFingerprint : "",
  };
}
function configName(task: RecordData) {
  const base = String(
    task.remark || (task.kind === "deploy" ? "直连" : "自备中转"),
  )
    .replace(/[<>:"/\\|?*\u0000-\u001f\u007f]/g, "_")
    .replace(/[. ]+$/g, "")
    .slice(0, 60);
  return (
    (base && !/^(con|prn|aux|nul|com[1-9]|lpt[1-9])(?:\.|$)/i.test(base)
      ? base
      : "MSBOOST配置") + ".json"
  );
}
export function SSHFields({
  value,
  onChange,
  title,
  showHelp = true,
}: {
  value: SSH;
  onChange: (s: SSH) => void;
  title: string;
  showHelp?: boolean;
}) {
  const [checking, setChecking] = useState(false),
    [observed, setObserved] = useState(""),
    [error, setError] = useState("");
  function change(k: keyof SSH, v: string | number) {
    onChange({
      ...value,
      [k]: v,
      ...(["host", "port"].includes(k)
        ? {
            fingerprint: "",
            replaceFingerprint: "",
            _rememberedFingerprint: "",
          }
        : {}),
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
      {showHelp && (
        <Notice>
          填写服务器信息即可，提交时会自动核对服务器身份。首次连接会记住这台服务器，以后身份变化会暂停操作。密码会在连接时验证。
        </Notice>
      )}
      <details
        className="mt8"
        open={
          !!value._rememberedFingerprint &&
          value._rememberedFingerprint !== value.fingerprint
        }
      >
        <summary>高级设置：核对服务器身份</summary>
        <p className="muted mt8">
          “指纹”是服务器的身份标记，不是密码。首次自动记住的身份还未经过服务商确认；需要更严格的检查时，可与服务商控制台提供的指纹核对。重装后指纹可能变化，没有重装却发生变化时请停止操作。
        </p>
        <Select
          label="验证方式"
          value={value.trustMode || "tofu"}
          onChange={(e) =>
            onChange({
              ...value,
              trustMode: e.target.value as "tofu" | "strict",
              fingerprint: "",
              replaceFingerprint: "",
            })
          }
        >
          <option value="tofu">自动记住服务器，之后自动核对</option>
          <option value="strict">与服务商控制台的指纹手动核对</option>
        </Select>
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
              onChange({
                ...value,
                fingerprint: "",
                _rememberedFingerprint: result.rememberedFingerprint || "",
                replaceFingerprint: "",
              });
            } catch (e) {
              setError((e as Error).message);
            } finally {
              setChecking(false);
            }
          }}
        >
          <ShieldCheck size={16} />
          {checking ? "正在检查…" : "验证指纹"}
        </Button>
        <p className="muted mt8">
          这里仅检查服务器身份；密码是否正确，要等实际连接时才能确认。
        </p>
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
              label="我已与服务商控制台提供的指纹核对一致"
            />
          </div>
        )}
        {value._rememberedFingerprint &&
          value._rememberedFingerprint !== (value.fingerprint || observed) && (
            <div className="mt16">
              <Notice tone="red">
                服务器身份已变化。若没有重装或更换服务器，请停止操作。
              </Notice>
              <p className="mono">原指纹：{value._rememberedFingerprint}</p>
              <p className="mono">新指纹：{value.fingerprint || observed}</p>
              <Check
                checked={
                  value.replaceFingerprint === value._rememberedFingerprint
                }
                onChange={(e) =>
                  onChange({
                    ...value,
                    fingerprint: value.fingerprint || observed,
                    replaceFingerprint: e.target.checked
                      ? value._rememberedFingerprint
                      : "",
                  })
                }
                label="我已在服务商控制台核对新指纹，确认是自己的服务器，允许更新记录"
              />
            </div>
          )}
        <ErrorNotice error={error} />
      </details>
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
const phases: Record<string, string> = {
  queued: "等待执行机",
  executing: "执行中",
  complete: "完成",
  fingerprint: "验证指纹",
  validate: "参数校验",
  install: "安装",
  config: "配置交付",
  prepare: "重装准备（未提交重启）",
  submitted: "重装重启已提交",
  submission_unknown: "重启提交待核实",
  preflight: "运行环境检查",
  target: "目标连接检查",
  relay: "中转部署",
  front: "前置机部署",
  integrity: "完整性校验",
  extract: "资源解压",
  binary: "程序架构 / 启动验证",
  dependencies: "系统依赖",
  download: "资源下载",
  service: "服务启动",
  self_test: "本地自测",
  execution: "远端执行",
  ssh_connect: "SSH 网络连接",
  ssh_host_key: "SSH 主机校验",
  ssh_auth: "SSH 认证",
  ssh_handshake: "SSH 握手",
  ssh_session: "SSH 会话",
  executor: "执行机通信",
  executor_timeout: "执行机通信超时",
  server_restart: "服务重启中断",
  ownership: "受管所有权检查",
  cleanup: "清理",
};
const healthLabels: Record<string, string> = {
  service: "服务运行",
  localSelfTest: "本地自测",
  publicTCP: "公网 TCP 可达性",
};
const healthValues: Record<string, string> = {
  running: "运行中",
  passed: "通过",
  reachable: "可达",
  unreachable: "不可达",
  not_tested: "未检测",
  target_tcp_reachable: "目标 TCP 可达",
  unknown: "未知",
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
          await saveLocal(owner, current.id, d, configName(current));
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
        {current.message || phases[current.phase] || "等待执行机接收任务"}
      </p>
      <div className="kv">
        <span>执行阶段</span>
        <strong>{phases[current.phase] || "等待结果"}</strong>
      </div>
      {current.errorCode && <small>诊断编号：{current.errorCode}</small>}
      {current.nextStep && (
        <Notice tone="orange">下一步：{current.nextStep}</Notice>
      )}
      {current.health && (
        <Table
          headers={["检查项目", "结果"]}
          rows={Object.entries(healthLabels).map(([k, label]) => [
            label,
            healthValues[current.health[k]] || "未知",
          ])}
        />
      )}
      {current.health && (
        <p className="muted mt8">
          游戏实际连接需由客户端自行验证；TCP 可达不代表游戏可正常使用。
        </p>
      )}
      {current.cleanup && (
        <Table
          headers={["清理对象", "类型"]}
          rows={(current.cleanup.items || []).map((item: RecordData) => [
            item.path,
            item.kind === "firewall"
              ? "本项目创建的防火墙规则"
              : item.kind === "directory"
                ? "目录"
                : "文件",
          ])}
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
      {Array.isArray(current.hops) && current.hops.length > 0 && (
        <Table
          headers={["中转顺序", "连接路径"]}
          rows={[...current.hops].reverse().map((hop: RecordData, index) => [
            `第 ${index + 1} 段`,
            `${hop.fromHost}:${hop.fromPort} → ${hop.toHost}:${hop.toPort}`,
          ])}
        />
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
            onClick={() => download(config, configName(current))}
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
    [remark, setRemark] = useState(""),
    [erase, setErase] = useState(false),
    [busy, setBusy] = useState(false),
    [error, setError] = useState(""),
    [task, setTask] = useState<RecordData | null>(null);
  const { data: quotas, reload } = useData("/api/tasks");
  const title =
    { deploy: "部署 MSBOOST", relay: "配置中转服务器", dd: "DD 系统" }[kind] ||
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
    if (kind === "relay" && !config) {
      setError("请上传节点配置");
      return;
    }
    setBusy(true);
    try {
      const pinnedSSH = await prepareSSH(ssh, setSSH);
      const pinnedFront =
        kind === "relay" && useFront
          ? await prepareSSH(front, setFront)
          : undefined;
      const r = await post(
        "/api/tasks",
        {
          kind,
          ssh: pinnedSSH,
          ...(kind === "deploy" ? { mode } : {}),
          ...(kind === "relay"
            ? {
                clientConfig: config,
                remark,
                ...(pinnedFront ? { front: pinnedFront } : {}),
              }
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
            ? "在你的VPS上部署游戏节点，并下载MSBOOST配置文件。"
            : kind === "relay"
              ? "上传MSBOOST配置文件，中转你或朋友的MSBOOST节点"
              : "在线重装系统，默认保持当前SSH端口和密码。"
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
          <div>
            <div className="card">
              {q && (
                <div className="quota">
                  <span>
                    每 <b>{q.minutes} 分钟</b>最多 <b>{q.count} 次</b> · 剩余{" "}
                    <b>{q.remaining} 次</b>
                  </span>
                  <small>
                    {q.remaining ? "当前可操作" : `下次可用：${date(q.nextAt)}`}{" "}
                    · 失败操作也计次
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
                    <Field
                      label="配置备注"
                      placeholder="例如：东京中转（将用于配置名称和文件名）"
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
                      入口使用实际中转服务器的 IP；监听端口随机分配，每端口固定
                      5 Mbps。
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
            {["deploy", "relay"].includes(kind) && (
              <CleanupPanel
                scope={kind === "deploy" ? "msboost" : "relay"}
                user={user}
              />
            )}
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
function CleanupPanel({
  scope,
  user,
}: {
  scope: "msboost" | "relay";
  user: RecordData;
}) {
  const [ssh, setSSH] = useState(newSSH),
    [preview, setPreview] = useState<RecordData | null>(null),
    [task, setTask] = useState<RecordData | null>(null),
    [busy, setBusy] = useState(false),
    [error, setError] = useState("");
  const checking =
    busy || (!!preview && ["queued", "running"].includes(preview.state));
  const ready =
    preview?.state === "succeeded" &&
    !!preview.cleanup?.digest &&
    Array.isArray(preview.cleanup?.items) &&
    preview.cleanup.items.length > 0;
  useEffect(() => {
    if (!preview || !["queued", "running"].includes(preview.state)) return;
    let alive = true;
    const timer = setInterval(
      () =>
        api("/api/tasks/" + preview.id)
          .then((r) => {
            if (alive) setPreview(r.task || r);
          })
          .catch((e) => {
            if (alive) setError(e.message);
          }),
      2000,
    );
    return () => {
      alive = false;
      clearInterval(timer);
    };
  }, [preview?.id, preview?.state]);
  async function submit(remove: boolean) {
    if (checking || (remove && !ready)) return;
    const checkedPreview = preview;
    // A final click consumes this UI confirmation, including an uncertain HTTP
    // result. The server still enforces its single-use token, digest and host.
    setPreview(null);
    setBusy(true);
    setError("");
    try {
      const connection = await prepareSSH(ssh, setSSH);
      const result = await post(
        "/api/tasks",
        {
          kind: remove ? "cleanup" : "cleanup-preview",
          ssh: connection,
          cleanup: {
            scope,
            ...(remove
              ? {
                  previewId: checkedPreview!.id,
                  digest: checkedPreview!.cleanup.digest,
                  confirm: true,
                }
              : { confirm: false }),
          },
        },
        "POST",
        { "Idempotency-Key": requestID() },
      );
      if (remove) {
        setTask(result.task || result);
        setSSH(newSSH());
      } else setPreview(result.task || result);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }
  return (
    <details className="card mt24" data-testid="cleanup-panel">
      <summary>
        {scope === "msboost" ? "卸载 MSBOOST" : "清理中转服务器配置"}
      </summary>
      <Notice tone="red">清理会停止相关服务并删除清单内配置。</Notice>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          submit(false);
        }}
      >
        <fieldset
          style={{ border: 0, padding: 0, margin: "16px 0 0", minWidth: 0 }}
          disabled={checking}
        >
          <SSHFields
            title="服务器 IP 地址"
            showHelp={false}
            value={ssh}
            onChange={(next) => {
              setSSH(next);
              setPreview(null);
              setError("");
            }}
          />
          {!ready && (
            <Button type="submit" className="mt16" disabled={checking}>
              {checking ? "正在检查…" : "继续"}
            </Button>
          )}
        </fieldset>
      </form>
      {ready && (
        <Button
          className="mt16 danger"
          disabled={checking}
          onClick={() => void submit(true)}
        >
          确认删除
        </Button>
      )}
      {preview?.state === "succeeded" &&
        preview.cleanup?.items?.length === 0 && (
          <p className="muted mt16">未发现需要卸载的组件。</p>
        )}
      {preview &&
        ["failed", "unknown", "interrupted"].includes(preview.state) && (
          <ErrorNotice
            error={[preview.message, preview.nextStep]
              .filter(Boolean)
              .join(" ")}
          />
        )}
      <ErrorNotice error={error} />
      {task && (
        <TaskDetail task={task} owner={user.id} onClose={() => setTask(null)} />
      )}
    </details>
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
        免费配置请下载备份。清除网站数据或更换设备后，本机配置可能无法恢复；捐赠权益线路文件请到线路页使用“中转配置下载”。
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
                  fingerprint: "验证指纹",
                  "cleanup-preview": "卸载检查",
                  cleanup: "卸载组件",
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
            ["dd", "fingerprint", "cleanup", "cleanup-preview"].includes(t.kind)
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
                  configName(restoring),
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
