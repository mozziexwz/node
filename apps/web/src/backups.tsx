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
    [targetAuthMode, setTargetAuthMode] = useState("password"),
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
        sub="加密快照支持保留财务的安全恢复，或离线导入全新数据库的完整灾难恢复。客户 VPS 磁盘和浏览器本地配置不在备份范围内。"
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
          <Notice>
            本机目录：{data?.localDirectory || "数据目录 / backups"}
            。删除只影响本机文件，远程副本与历史记录保留；恢复前回滚副本受保护。
          </Notice>
          <Table
            headers={["时间", "大小", "状态", "保存目标", "操作"]}
            rows={array(data, "backups").map((b) => [
              date(b.createdAt),
              (b.size / 1024 / 1024).toFixed(2) + " MB",
              <Badge tone={b.status === "verified" ? "green" : "orange"}>
                {(
                  {
                    verified: "校验通过",
                    partial: "远程部分失败",
                    failed: "失败",
                    local_removed: "本机已删除",
                    local_missing: "本机未迁移",
                  } as Record<string, string>
                )[b.status] || b.status}
                {b.protected ? " · 回滚保护" : ""}
              </Badge>,
              Object.entries(b.targets || {}).map(([k, v]) => (
                <small key={k}>
                  {k === "local"
                    ? "本机"
                    : targets.find((t) => t.id === k)?.name || k}
                  ：{String(v)}
                </small>
              )),
              <div className="actions">
                <Button
                  disabled={
                    b.targets?.local === "removed" ||
                    b.targets?.local === "not_restored"
                  }
                  onClick={() =>
                    void downloadFile(
                      `/api/admin/backups/${b.id}/download`,
                      `msboost-${b.id}.msb`,
                    ).catch((e) => setMessage(e.message))
                  }
                >
                  下载加密备份
                </Button>
                <Button
                  disabled={
                    busy || b.protected || b.targets?.local === "removed"
                  }
                  onClick={async () => {
                    if (
                      !confirm(
                        `仅删除这份 ${date(b.createdAt)} 的本机备份（${(b.size / 1024 / 1024).toFixed(2)} MB）？远程副本和历史记录不删除。本机文件删除后不能撤销，最少有效副本保护仍会检查。`,
                      )
                    )
                      return;
                    try {
                      const result = await post(
                        `/api/admin/backups/${b.id}`,
                        { confirm: "DELETE_LOCAL" },
                        "DELETE",
                      );
                      setMessage(result.message);
                      reload();
                    } catch (e) {
                      setMessage((e as Error).message);
                    }
                  }}
                >
                  删除本机副本
                </Button>
              </div>,
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
              onClick={() => {
                setTargetAuthMode("password");
                setTarget({
                  name: "",
                  host: "",
                  port: 22,
                  user: "root",
                  path: "/root/msboost-backup",
                  authMode: "password",
                  fingerprint: "",
                  enabled: true,
                });
              }}
            >
              新增目标
            </Button>
          </div>
          <Table
            headers={["目标", "服务器", "认证方式", "目录", "操作"]}
            rows={targets.map((t) => [
              t.name,
              t.host,
              t.authMode === "password" ? "SSH 密码" : "SSH 私钥",
              t.path,
              <div className="actions">
                <Button
                  onClick={() => {
                    setTargetAuthMode(t.authMode || "private_key");
                    setTarget(t);
                  }}
                >
                  编辑
                </Button>
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
          <h3>安全恢复（网页）</h3>
          <Notice tone="red">
            只恢复站点设置、文章附件、线路及节点定义。完整保留当前用户身份、枫叶与权益、兑换记录与兑换码、支付去重、工单审计、流量记录及用户规则。
            当前用户规则正在关联的线路和节点保留当前版本，预检会列出。先开启维护模式并结束任务；v1
            规则暂停后需等待节点确认或短租约失效。 keep_last
            或混合模式不能用旧租约推定停止，恢复会保留相关资源并进入恢复核对。请先独立受信核对或隔离旧节点，再按预检结果操作。
            恢复前保存受保护的私有回滚副本；恢复后退出全部会话，撤销 Agent
            凭据，停用控制面的线路定义，保持维护。Token
            失效不代表旧业务已停止。支持恢复协议的 v2 节点须通过服务器本机 root
            中转恢复入口逐规则核对；其他节点须独立确认停止，不能直接解除恢复冻结。
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
                快照校验通过：{preflight.users} 个用户、{preflight.collections}{" "}
                组数据。安全恢复保留当前 {preflight.report?.currentUsers}{" "}
                个用户，不回滚财务与流量。
              </Notice>
              <Table
                headers={["恢复范围", "新增", "替换", "移除"]}
                rows={(preflight.report?.differences || []).map(
                  (d: RecordData) => [
                    (
                      {
                        articles: "公告教程",
                        attachments: "文章附件",
                        routes: "线路",
                        relay_agents: "中转节点",
                        executors: "执行机",
                      } as Record<string, string>
                    )[d.collection] || d.collection,
                    d.added,
                    d.changed,
                    d.removed,
                  ],
                )}
              />
              <Notice>
                变更的站点设置键：
                {preflight.report?.settingKeys?.join("、") || "无"}
                。支付与兑换开关保留当前值；维护模式强制保持开启。
              </Notice>
              <Notice>
                为保护当前用户规则而保留的线路：
                {preflight.report?.preservedRoutes?.join("、") || "无"}；节点：
                {preflight.report?.preservedNodes?.join("、") || "无"}。
              </Notice>
              <section aria-label="恢复核对预检结果" className="mt16">
                {preflight.report?.recoveryRequired && (
                  <Notice tone="orange">
                    本次恢复需要人工核对（recovery_required）。端口、旧目标及关联节点资源继续保留；不能因为管理凭据被撤销就重新分配端口或认定旧业务停止。
                    待核对规则：
                    {preflight.report?.recoveryRules?.join("、") ||
                      "暂无关联规则；仍需核对旧节点"}
                    。
                  </Notice>
                )}
                {(preflight.report?.warnings || []).map((warning: string) => (
                  <div className="mt8" key={warning}>
                    <Notice tone="orange">{warning}</Notice>
                  </div>
                ))}
              </section>
              {(preflight.report?.blockers || []).map((reason: string) => (
                <Notice tone="red" key={reason}>
                  {reason}。处理后请重新预检。
                </Notice>
              ))}
              <pre className="code-panel mt16">SHA256：{preflight.sha256}</pre>
              <Button
                className="mt16 danger"
                disabled={!!preflight.report?.blockers?.length}
                onClick={async () => {
                  if (
                    !file ||
                    !confirm(
                      "确认按预检范围安全恢复？当前用户、财务与流量保留；全部会话退出、Agent 凭据失效、控制面线路定义停用。keep_last 旧业务可能继续运行，相关资源保留并进入人工恢复核对；Token 失效不代表节点已停止。",
                    )
                  )
                    return;
                  const form = new FormData();
                  form.append("file", file);
                  form.append("sha256", preflight.sha256);
                  form.append("currentSha256", preflight.report.currentSha256);
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
                确认安全恢复
              </Button>
            </div>
          )}
          <h3 className="mt24">完整灾难恢复（离线）</h3>
          <Notice>
            适用于原数据库损坏或整站迁移。使用原 MASTER_KEY 和 msboost-restore
            命令，将完整快照导入全新隔离 PostgreSQL 数据库或 SQLite
            目录；绝不在线覆写原库。
            备份之后的用户、付款及流量不会进入恢复库。旧库保留，恢复后默认维护、关闭支付，需停止旧服务后手动切换并人工核对兑换记录，再恢复营业。
            离线保留节点可能仍在运行，须保留端口及旧目标并独立受信核对或隔离；恢复数据库和撤销
            Token 不构成旧业务停止或无损接管的证明。
          </Notice>
          <p className="mt16">
            <a
              href="https://github.com/mozziexwz/node/blob/main/docs/backup-recovery.md"
              target="_blank"
              rel="noreferrer"
            >
              查看 Debian 12 / Docker Compose 完整恢复操作说明
            </a>
          </p>
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
                  authMode: targetAuthMode,
                  privateKey: f.get("privateKey") || "",
                  password: f.get("password") || "",
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
                ["user", "SSH 用户名"],
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
            <Select
              label="SSH 认证方式"
              value={targetAuthMode}
              onChange={(e) => setTargetAuthMode(e.target.value)}
            >
              <option value="password">SSH 密码</option>
              <option value="private_key">SSH 私钥</option>
            </Select>
            {targetAuthMode === "password" ? (
              <Field
                key="password"
                label="SSH 密码"
                name="password"
                type="password"
                autoComplete="new-password"
                maxLength={512}
                required={
                  !target.id ||
                  targetAuthMode !== (target.authMode || "private_key")
                }
                placeholder={
                  target.id && targetAuthMode === target.authMode
                    ? "留空保留已保存的密码"
                    : "请输入 SSH 登录密码"
                }
              />
            ) : (
              <label className="field" key="private_key">
                <span>备份专用 SSH 私钥</span>
                <textarea
                  name="privateKey"
                  rows={6}
                  autoComplete="off"
                  required={
                    !target.id ||
                    targetAuthMode !== (target.authMode || "private_key")
                  }
                  placeholder={
                    target.id &&
                    targetAuthMode === (target.authMode || "private_key")
                      ? "留空保留已保存的私钥"
                      : "请输入备份专用 SSH 私钥"
                  }
                />
              </label>
            )}
            <Check
              label="启用"
              name="enabled"
              defaultChecked={target.enabled}
            />
            <Notice>
              可使用 root
              或备份专用账号。凭据加密保存，不会回显；编辑时留空仅保留同一认证方式的凭据，切换方式必须重新填写。
              缺失目录会以 0700 权限创建，备份文件权限为
              0600；路径不能包含符号链接或其他账号可写的目录，账号须有相应权限。远端重新读取校验后才记为成功。
            </Notice>
            <details className="mt16">
              <summary>如何核对 SSH 主机指纹？</summary>
              <p className="mt8">
                请通过 VPS
                服务商控制台等可信通道，在目标服务器查看主机公钥指纹。常见
                Debian 12 的 ECDSA 主机公钥可使用：
              </p>
              <pre className="code-panel mt8">
                ssh-keygen -lf /etc/ssh/ssh_host_ecdsa_key.pub -E sha256
              </pre>
              <p className="mt8">
                复制输出中的完整
                SHA256:…，不是私钥。若服务器使用其他主机密钥，请核对实际协商密钥对应的指纹。指纹不符会在发送密码前中止连接；不要未经独立核对就信任网络扫描结果。
              </p>
            </details>
          </AsyncForm>
        </Modal>
      )}
    </>
  );
}
