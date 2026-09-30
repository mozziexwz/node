import { useEffect } from "react";
import { array, post } from "./api";
import { Button, Notice, Table, Badge, ErrorNotice, useData, date, bytes, AsyncForm, Check, Loading } from "./ui";

export function WholeSiteBackups() {
  const { data, error, loading, reload } = useData("/api/admin/disaster-backups");
  const { data: settings, reload: reloadSettings } = useData("/api/admin/settings");
  useEffect(() => { const timer = setInterval(reload, 15000); return () => clearInterval(timer); }, []);
  const schedule = data?.schedule;
  const runs = array(data, "runs");
  const localAt = Math.max(0, ...runs.map((r) => r.localVerifiedAt || 0));
  const remoteAt = Math.max(0, ...runs.map((r) => r.remoteVerifiedAt || 0));
  if (!data) return <div className="card mt16">
    <div className="actions"><h3>整站备份</h3><Button onClick={reload}>刷新状态</Button></div>
    <ErrorNotice error={error} />
    {loading && <Loading />}
  </div>;
  return <div className="card mt16">
    <div className="actions"><h3>整站备份</h3><Button onClick={reload}>刷新状态</Button></div>
    <ErrorNotice error={error} />
    <p>每日计划：{schedule?.enabled ? "已启用" : "未启用或尚无记录"}
      {schedule?.time ? ` · ${schedule.time}（服务器时区 ${schedule.zone}）` : ""}</p>
    {schedule?.nextRunAt > 0 && <p>下次计划：{date(schedule.nextRunAt)}，实际执行可能延后 60 秒。</p>}
    <p>最近本地校验成功：{localAt ? date(localAt) : "尚无成功记录"}；最近异地校验成功：{remoteAt ? date(remoteAt) : "尚无成功记录"}</p>
    <Notice>整站备份包含用户、订单、权益、文章附件、网站配置和原密钥。网站保持运行。启用计划或连接测试通过，不代表已产生成功备份。</Notice>
    {settings && <AsyncForm label="保存提醒设置" key={String(settings.disasterBackupEmailEnabled)} onSubmit={async (f) => { await post("/api/admin/settings", {disasterBackupEmailEnabled:f.get("disasterBackupEmailEnabled")==="on"}); reloadSettings(); }}>
      <Check name="disasterBackupEmailEnabled" defaultChecked={!!settings.disasterBackupEmailEnabled} label="备份异常时发邮件给管理员（需已配置 SMTP，默认关闭）" />
    </AsyncForm>}
    <p className="mt16">在面板服务器运行 <code>msboost</code>：菜单 8 立即备份，9 配置并验证计划，13 查看记录，14 测试异地连接，15 重传已有备份。</p>
    <p className="muted">恢复时 HTTPS 证书会自动重新签发；历史备份文件、客户 VPS 磁盘和浏览器配置不重复收录。下面记录对应服务器安装脚本的整站备份，与其他页签中的业务备份分别记录。</p>
    <Table headers={["更新时间", "结果", "本地", "异地", "文件"]} rows={runs.map((r) => [
      date(r.updatedAt), <span>{r.message}{r.detail && <small className="muted">{r.detail}</small>}</span>,
      <Badge tone={r.localOK ? "green" : "orange"}>{r.localOK ? "校验通过" : "尚未完成"}</Badge>,
      <Badge tone={r.remoteOK ? "green" : "orange"}>{r.remoteOK ? "校验通过" : r.remoteConfigured ? "尚未成功" : "未配置"}</Badge>,
      r.archive ? `${r.archive}（${bytes(r.size)}）` : "尚未生成",
    ])} />
  </div>;
}
