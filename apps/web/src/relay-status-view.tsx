import { RecordData } from "./api";
import { Notice } from "./ui";
import { relayStatus } from "./relay-status";

export function trafficAmount(value: unknown) {
  const bytes = Math.max(0, Number(value) || 0);
  return bytes < 1e9
    ? `${(bytes / 1e6).toFixed(2)} MB`
    : `${(bytes / 1e9).toFixed(2)} GB`;
}

export function RelayStatusDetail({
  rule,
  routeOnline,
  member = false,
}: {
  rule: RecordData;
  routeOnline?: boolean;
  member?: boolean;
}) {
  const status = relayStatus(rule, routeOnline);
  return (
    <section aria-label="中转流量" className="route-traffic mt16">
      <p>
        <span>已用上行流量：</span>
        <strong>{trafficAmount(rule.inputBytes)}</strong>
      </p>
      <p>
        <span>已用下行流量：</span>
        <strong>{trafficAmount(rule.outputBytes)}</strong>
      </p>
      {status.stopText && (
        <div className="mt8">
          <Notice tone="orange">
            {member
              ? "线路正在处理请求，完成前暂不能修改配置。"
              : status.stopText}
          </Notice>
        </div>
      )}
      {status.recoveryText && (
        <div className="mt8">
          <Notice tone="orange">
            {member
              ? "线路正在维护核对，暂不能操作。如持续出现，请联系客服。"
              : status.recoveryText}
          </Notice>
        </div>
      )}
      {status.accountingText && (
        <div className="mt8">
          <Notice tone="orange">
            {member
              ? "流量统计暂未同步，请稍后刷新；若持续异常请联系客服。"
              : status.accountingText}
          </Notice>
        </div>
      )}
    </section>
  );
}
