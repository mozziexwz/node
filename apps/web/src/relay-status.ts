import type { RecordData } from "./api";

export function relayNeedsRecovery(rule: RecordData) {
  return (
    rule.reconcileState === "recovery_required" ||
    rule.syncState === "recovery_required"
  );
}

export function relayStatus(rule: RecordData, routeOnline?: boolean) {
  const mode =
    rule.offlinePolicy === "keep_last" || rule.offlinePolicy === "mixed"
      ? rule.offlinePolicy
      : (rule.segments || []).some(
            (segment: RecordData) => segment.protocolVersion >= 2,
          )
        ? "mixed"
        : "unconfirmed";
  const recovery = relayNeedsRecovery(rule);
  const control =
    rule.controlStatus ||
    (routeOnline === true
      ? "online"
      : routeOnline === false
        ? "offline"
        : "unknown");
  const states: Record<string, string> = {
    pending: "等待各跳确认",
    syncing: "限速 / 配置同步中",
    active: "配置已确认",
    paused: "暂停请求已记录",
    pausing: "停止待确认",
    revoking: "撤销待确认",
    failed: "节点执行失败",
    suspended: "账号已停用",
    quota_exhausted: "流量耗尽",
    unavailable: "权益或线路不可用",
    awaiting_front: "等待前置机配置",
    provisioning: "前置机配置中",
    config_error: "配置异常，待核对",
    firewall_pending: "节点防火墙待处理，请联系管理员",
  };
  const runtime: Record<string, string> = {
    last_reported_running: "运行",
    last_reported_stopped: "停止",
    partial_unknown: "仅部分节点可核实",
    unknown: "状态未知",
  };
  const stopPending =
    rule.stopStatus === "pending" ||
    ["pausing", "revoking"].includes(rule.syncState || rule.state);
  const stopConfirmed = rule.stopStatus === "confirmed";
  const keepLastConfirmed =
    mode === "keep_last" && rule.keepLastConfirmed === true && !recovery;
  return {
    mode,
    recovery,
    control,
    stopPending,
    stopConfirmed,
    keepLastConfirmed,
    label: recovery
      ? "恢复核对中"
      : stopPending
        ? "待节点停止确认"
        : control === "offline"
          ? "业务状态待核实"
          : stopConfirmed
            ? "节点已报告停止"
            : states[rule.syncState || rule.state] || "状态待核对",
    controlLabel:
      control === "online"
        ? "管理连接在线"
        : control === "offline"
          ? "管理连接已中断"
          : "管理连接状态未知",
    runtimeLabel: runtime[rule.runtimeStatus] || "状态未知",
    policyText:
      mode === "unconfirmed"
        ? "等待节点连接并确认配置。"
        : mode === "mixed"
          ? "部分节点尚未确认配置，请检查相关节点连接及版本。"
          : keepLastConfirmed
            ? "节点已确认：面板离线时保留最后配置。"
            : "正在等待所有节点确认当前配置。",
    stopText: stopPending
      ? "正在等待节点停止转发，确认后释放端口。节点离线时请先恢复管理连接。"
      : stopConfirmed
        ? "当前停止请求已获节点确认；这是最后报告，不是实时业务探测。"
        : "",
    recoveryText: recovery
      ? "网站恢复后需要重新连接节点。请在节点管理中打开“恢复管理连接”向导；原有转发会保留。"
      : "",
    accountingText: rule.accountingDegraded
      ? "计量状态降级，流量需核对；不能据此判断业务已停止。"
      : "",
  };
}

// Members need an actionable summary, not the operator's lease/protocol
// reconciliation details. The underlying safety gates still use relayStatus.
export function relayMemberLabel(rule: RecordData, routeOnline?: boolean) {
  const status = relayStatus(rule, routeOnline);
  if (status.recovery) return "线路维护中";
  if (status.stopPending) return "处理中";
  if (status.control === "offline") return "线路状态待确认";
  if (status.stopConfirmed) return "已停止";
  return status.label;
}

export function relaySegmentStopText(segment: RecordData) {
  if (segment.protocolVersion >= 2) {
    return segment.stopConfirmed === true &&
      segment.ackState === "stopped" &&
      segment.appliedGeneration === segment.configGeneration &&
      ["pause", "revoke"].includes(segment.lastCommandAction)
      ? "已确认停止"
      : ["pause", "revoke"].includes(segment.lastCommandAction)
        ? "等待停止确认"
        : "尚未请求停止";
  }
  return "等待节点连接确认";
}

export function relayAccountingVisible(data: RecordData | null) {
  return (
    !!data &&
    ((Array.isArray(data.periods) && data.periods.length > 0) ||
      data.reviewBytes > 0 ||
      data.reviewSamples > 0 ||
      data.degradedAgents > 0)
  );
}
