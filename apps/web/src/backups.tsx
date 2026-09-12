import { useState } from "react";
import { api, post, array, downloadFile, RecordData } from "./api";
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
  ErrorNotice,
  AsyncForm,
  useData,
  date,
} from "./ui";
export function Backups() {
  const { data, error, reload } = useData("/api/admin/backups"),
    { data: plan, reload: reloadPlan } = useData("/api/admin/backup-plan"),
    { data: targetData, reload: reloadTargets } = useData(
      "/api/admin/backup-targets",
    );
  const [tab, setTab] = useState("history"),
    [busy, setBusy] = useState(false),
    [message, setMessage] = useState(""),
    [target, setTarget] = useState<RecordData | null>(null),
    [file, setFile] = useState<File | null>(null),
    [preflight, setPreflight] = useState<RecordData | null>(null);
  const targets = array(targetData, "targets");
  async function backup() {
    setBusy(true);
    setMessage("");
    try {
      const r = await post("/api/admin/backups", {});
      setMessage(
        r.status === "verified"
          ? "加密备份已完成并校验。"
          : "本地备份已完成，部分远程目标未成功，请检查记录。",
      );
      reload();
    } catch (e) {
      setMessage((e as Error).message);
    } finally {
      setBusy(false);
    }
  }
  return (
    <>
      <Header
        title="备份与恢复"
        sub="完整平台数据的一致性加密快照。客户 VPS 磁盘和浏览器本地配置不在备份范围内。"
      >
        <Button primary disabled={busy} onClick={() => void backup()}>
          {busy ? "正在备份…" : "立即备份"}
        </Button>
      </Header>
      <ErrorNotice error={error} />
      {message && <Notice>{message}</Notice>}
      <div className="tabs mt16">
        {[
          ["history", "备份历史"],
          ["schedule", "自动计划"],
          ["targets", "远程目标"],
          ["restore", "恢复备份"],
        ].map(([k, n]) => (
          <button
            className={k === tab ? "active" : ""}
            key={k}
            onClick={() => setTab(k)}
          >
            {n}
          </button>
        ))}
      </div>
      {tab === "history" && (
        <div className="card flush mt16">
          <Table
            headers={["时间", "大小", "状态", "保存目标", "操作"]}
            rows={array(data, "backups").map((b) => [
              date(b.createdAt),
              (b.size / 1024 / 1024).toFixed(2) + " MB",
              <Badge tone={b.status === "verified" ? "green" : "orange"}>
                {b.status}
              </Badge>,
              Object.entries(b.targets || {}).map(([k, v]) => (
                <small key={k}>
                  {k === "local"
                    ? "本机"
                    : targets.find((t) => t.id === k)?.name || k}
                  ：{String(v)}
                </small>
              )),
              <Button
                onClick={() =>
                  void downloadFile(
                    `/api/admin/backups/${b.id}/download`,
                    `msboost-${b.id}.msb`,
                  ).catch((e) => setMessage(e.message))
                }
              >
                下载加密备份
              </Button>,
            ])}
          />
          <div className="card">
            <Button
              onClick={async () => {
                try {
                  const r = await post("/api/admin/backups/retention", {
                    confirm: false,
                  });
                  if (!r.ids.length) {
                    setMessage("没有满足保留策略的待清理本地副本");
                    return;
                  }
                  if (
                    confirm(
                      `将清理 ${r.ids.length} 份超过保留期且不在最少成功副本保护范围内的本地备份，确认删除？`,
                    )
                  ) {
                    await post("/api/admin/backups/retention", {
                      confirm: true,
                    });
                    reload();
                  }
                } catch (e) {
                  setMessage((e as Error).message);
                }
              }}
            >
              预览本机保留清理
            </Button>
          </div>
        </div>
      )}
      {tab === "schedule" && plan && (
        <div className="card mt16">
          <AsyncForm
            onSubmit={(f) =>
              post(
                "/api/admin/backup-plan",
                {
                  enabled: f.get("enabled") === "on",
                  mode: f.get("mode"),
                  time: f.get("time"),
                  hours: Number(f.get("hours")),
                  weekday: Number(f.get("weekday")),
                  timezone: f.get("timezone"),
                  retentionDays: Number(f.get("retentionDays")),
                  minCopies: Number(f.get("minCopies")),
                  targets: f.getAll("targets"),
                },
                "PUT",
              )
            }
            onDone={reloadPlan}
          >
            <Check
              label="开启自动备份"
              name="enabled"
              defaultChecked={plan.enabled}
            />
            <div className="form-grid">
              <Select label="频率" name="mode" defaultValue={plan.mode}>
                <option value="daily">每天</option>
                <option value="hours">每隔 N 小时</option>
                <option value="weekly">每周</option>
              </Select>
              <Field
                label="执行时间"
                name="time"
                type="time"
                defaultValue={plan.time}
              />
              <Field
                label="小时间隔"
                name="hours"
                type="number"
                min={1}
                max={168}
                defaultValue={plan.hours}
              />
              <Select
                label="每周指定日"
                name="weekday"
                defaultValue={plan.weekday || 0}
              >
                {["周日", "周一", "周二", "周三", "周四", "周五", "周六"].map(
                  (t, i) => (
                    <option value={i} key={i}>
                      {t}
                    </option>
                  ),
                )}
              </Select>
              <Select label="时区" name="timezone" defaultValue={plan.timezone}>
                <option>Asia/Shanghai</option>
                <option>UTC</option>
              </Select>
              <Field
                label="本地保留天数"
                type="number"
                min={1}
                name="retentionDays"
                defaultValue={plan.retentionDays}
              />
              <Field
                label="最少成功副本数"
                type="number"
                min={1}
                name="minCopies"
                defaultValue={plan.minCopies}
              />
            </div>
            <h3>远程目标</h3>
            {targets.map((t) => (
              <Check
                key={t.id}
                name="targets"
                value={t.id}
                defaultChecked={plan.targets?.includes(t.id)}
                label={t.name}
              />
            ))}
            <Notice>
              每次包含数据库业务数据、站点设置、文章与附件、审计及流量记录。主密钥必须独立备份；恢复时需要原密钥。
            </Notice>
            {plan.enabled && (
              <p className="mt16">下次执行：{date(plan.nextAt)}</p>
            )}
          </AsyncForm>
        </div>
      )}
      {tab === "targets" && (
        <div className="card mt16">
          <div className="between">
            <h3>SFTP 异地备份</h3>
            <Button
              primary
              onClick={() =>
                setTarget({
                  name: "",
                  host: "",
                  port: 22,
                  user: "backup",
                  path: "/srv/msboost-backups",
                  fingerprint: "",
                  enabled: true,
                })
              }
            >
              新增目标
            </Button>
          </div>
          <Table
            headers={["目标", "服务器", "目录", "操作"]}
            rows={targets.map((t) => [
              t.name,
              t.host,
              t.path,
              <div className="actions">
                <Button onClick={() => setTarget(t)}>编辑</Button>
                <Button
                  onClick={async () => {
                    if (
                      !confirm("移除这个备份目标？不会删除服务器上的已有副本。")
                    )
                      return;
                    try {
                      await api("/api/admin/backup-targets/" + t.id, {
                        method: "DELETE",
                      });
                      reloadTargets();
                    } catch (e) {
                      setMessage((e as Error).message);
                    }
                  }}
                >
                  删除
                </Button>
              </div>,
            ])}
          />
        </div>
      )}
      {tab === "restore" && (
        <div className="card mt16">
          <Notice tone="red">
            恢复会覆盖平台业务数据。请先开启维护模式、结束运行任务、暂停本站线路并等待租约失效。恢复前会自动保存当前站点；恢复后须重新登录、关联
            Agent 并核对支付流水。
          </Notice>
          <div className="mt24">
            <input
              type="file"
              accept=".msb"
              aria-label="选择加密备份"
              onChange={(e) => {
                setFile(e.target.files?.[0] || null);
                setPreflight(null);
              }}
            />
          </div>
          <Button
            className="mt16"
            onClick={async () => {
              if (!file) return;
              const form = new FormData();
              form.append("file", file);
              try {
                setPreflight(
                  await api("/api/admin/backups/preflight", {
                    method: "POST",
                    body: form,
                  }),
                );
              } catch (e) {
                setMessage((e as Error).message);
              }
            }}
          >
            只预检，不恢复
          </Button>
          {preflight && (
            <div className="mt24">
              <Notice tone="green">
                校验通过：{preflight.users} 个用户、{preflight.collections}{" "}
                组数据。
              </Notice>
              <pre className="code-panel mt16">SHA256：{preflight.sha256}</pre>
              <Button
                className="mt16 danger"
                onClick={async () => {
                  if (
                    !file ||
                    !confirm("确认用这份已校验备份覆盖平台数据并退出全部会话？")
                  )
                    return;
                  const form = new FormData();
                  form.append("file", file);
                  form.append("sha256", preflight.sha256);
                  form.append("confirm", "RESTORE");
                  try {
                    const result = await api("/api/admin/backups/restore", {
                      method: "POST",
                      body: form,
                    });
                    alert(result.message);
                    location.reload();
                  } catch (e) {
                    setMessage((e as Error).message);
                  }
                }}
              >
                确认覆盖并恢复
              </Button>
            </div>
          )}
        </div>
      )}
      {target && (
        <Modal title="远程备份目标" onClose={() => setTarget(null)}>
          <AsyncForm
            onSubmit={async (f) => {
              await post(
                "/api/admin/backup-targets" +
                  (target.id ? "/" + target.id : ""),
                {
                  name: f.get("name"),
                  host: f.get("host"),
                  port: Number(f.get("port")),
                  user: f.get("user"),
                  path: f.get("path"),
                  fingerprint: f.get("fingerprint"),
                  privateKey: f.get("privateKey") || "",
                  enabled: f.get("enabled") === "on",
                },
                target.id ? "PUT" : "POST",
              );
              setTarget(null);
              reloadTargets();
            }}
          >
            <div className="form-grid">
              {[
                ["name", "名称"],
                ["host", "公网 IP"],
                ["port", "SSH 端口"],
                ["user", "备份专用用户"],
                ["path", "远程目录"],
                ["fingerprint", "SSH 主机指纹（SHA256:…）"],
              ].map(([k, n]) => (
                <Field
                  key={k}
                  label={n}
                  name={k}
                  defaultValue={target[k]}
                  type={k === "port" ? "number" : "text"}
                  required
                />
              ))}
            </div>
            <label className="field">
              <span>备份专用 SSH 私钥（留空保留）</span>
              <textarea name="privateKey" rows={6} />
            </label>
            <Check
              label="启用"
              name="enabled"
              defaultChecked={target.enabled}
            />
            <Notice>
              请预先创建目录并授予此备份账号权限。上传使用临时文件，远端重新读取校验后才记为成功。
            </Notice>
          </AsyncForm>
        </Modal>
      )}
    </>
  );
}
