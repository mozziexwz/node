import { RecordData } from "./api";
import { date, Notice } from "./ui";
import { relayStatus } from "./relay-status";

export function RelayStatusDetail({
  rule,
  routeOnline,
}: {
  rule: RecordData;
  routeOnline?: boolean;
}) {
  const status = relayStatus(rule, routeOnline);
  return (
    <section aria-label="中转状态说明" className="mt16">
      <p className="muted">
        {status.controlLabel}；最后报告：{status.runtimeLabel}（
        {rule.runtimeObservedAt ? date(rule.runtimeObservedAt) : "无报告时间"}）
        {status.control !== "online"
          ? "；当前业务待核实。"
          : "；报告不等于实时业务探测。"}
      </p>
      <p className="muted mt8">{status.policyText}</p>
      <p className="muted mt8">
        最近全链路配置确认：{rule.readySegments || 0} /{" "}
        {rule.totalSegments || 0} 个节点。
        {rule.appliedRateMbps > 0
          ? `最近确认限速：上下行各 ${rule.appliedRateMbps} Mbps。`
          : "尚未确认全部节点应用当前配置。"}
      </p>
      {status.stopText && (
        <div className="mt8">
          <Notice tone="orange">{status.stopText}</Notice>
        </div>
      )}
      {status.recoveryText && (
        <div className="mt8">
          <Notice tone="orange">{status.recoveryText}</Notice>
        </div>
      )}
      {status.accountingText && (
        <div className="mt8">
          <Notice tone="orange">{status.accountingText}</Notice>
        </div>
      )}
    </section>
  );
}
