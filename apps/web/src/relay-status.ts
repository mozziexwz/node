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
        : "lease";
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
  };
  const runtime: Record<string, string> = {
    last_reported_running: "运行",
    last_reported_stopped: "停止",
    partial_unknown: "仅部分节点可核实",
    unknown: "状态未知",
  };
  const stopPending =
    rule.stopStatus === "pending" ||
    (mode === "lease" &&
      ["pausing", "revoking"].includes(rule.syncState || rule.state));
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
      mode === "lease"
        ? "短租约模式（v1）：失联后按有效租约停止，旧配置最长保留 45 秒；实际停止情况仍需核实。"
        : mode === "mixed"
          ? "混合模式：含短租约节点，未确认全链路离线保留，不能保证失联期间连续运行。"
          : keepLastConfirmed
            ? "已确认离线保留（全链路 ACK）；这不代表失联期间业务可用性已核实。"
            : "离线保留模式尚未获得全链路确认，不能据此判断失联后的业务可用性。",
    stopText: stopPending
      ? "待节点停止确认，端口及旧目标继续占用。" +
        (mode === "lease"
          ? "v1 将同时依据短租约状态核对。"
          : "不能用管理心跳或旧租约时间替代停止确认。")
      : stopConfirmed
        ? "当前停止请求已获节点确认；这是最后报告，不是实时业务探测。"
        : "",
    recoveryText: recovery
      ? "恢复核对期间保留端口及旧目标。管理凭据失效不代表旧业务已停止；请由运维通过本机 root 受信恢复流程逐规则核对，或独立确认旧节点已停止。"
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
      ? "v2：明确停止已确认"
      : ["pause", "revoke"].includes(segment.lastCommandAction)
        ? "v2：等待明确停止确认"
        : "v2：尚无明确停止确认";
  }
  return "v1：短租约截止";
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
