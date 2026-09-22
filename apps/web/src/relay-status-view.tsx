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
}: {
  rule: RecordData;
  routeOnline?: boolean;
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
