package control

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"

	"github.com/mozziexwz/node/internal/relayruntime"
)

// This is an observation freshness bound, not a lease or a promise that a
// connection is still alive. Ninety seconds accommodates three maximum v2
// retry delays; a stale node must reconnect before a planned control outage.
const backupPauseFreshnessMS int64 = 90_000

type BackupPauseBlocker struct {
	RuleID  string `json:"ruleId,omitempty"`
	AgentID string `json:"agentId,omitempty"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type BackupPauseReport struct {
	CanPauseControl     bool                 `json:"canPauseControl"`
	CanPauseMaintenance bool                 `json:"canPauseMaintenance"`
	CheckedRules        int                  `json:"checkedRules"`
	Blockers            []BackupPauseBlocker `json:"blockers"`
}

// backupPausePreflight never mutates State and never performs network work.
// The caller must acquire its durable admission gate in the SAME transaction;
// using this report alone followed by a later stop has a TOCTOU race.
func backupPausePreflight(s *State, now int64) BackupPauseReport {
	report := BackupPauseReport{Blockers: []BackupPauseBlocker{}}
	seen := map[BackupPauseBlocker]bool{}
	block := func(rule, agent, code, message string) {
		b := BackupPauseBlocker{RuleID: rule, AgentID: agent, Code: code, Message: message}
		if !seen[b] {
			seen[b] = true
			report.Blockers = append(report.Blockers, b)
		}
	}
	if s == nil || s.Docs == nil || s.Users == nil || s.Settings == nil || now <= 0 {
		block("", "", "invalid_state", "持久状态或检查时间无效，不能安全停站")
		return report
	}
	fresh := func(at int64) bool { return at > 0 && at <= now && now-at < backupPauseFreshnessMS }
	malformed := func(collection string) {
		block("", "", "invalid_record", "持久记录无效或身份关系不完整："+collection)
	}
	agents := backupPauseReadDocs[RelayAgent](s, "relay_agents", malformed, func(k string, a RelayAgent) bool { return k != "" && a.ID == k })
	catalogs := backupPauseReadDocs[RelayV2Catalog](s, "relay_v2_catalogs", malformed, func(k string, c RelayV2Catalog) bool {
		return k != "" && c.AgentID == k && c.ControlEpoch != "" && c.Revision >= 0 && c.Sequence >= 0
	})
	commands := backupPauseReadDocs[RelayV2Command](s, "relay_v2_commands", malformed, func(k string, c RelayV2Command) bool {
		return backupPauseCommandValid(c) && k == c.AgentID+":"+c.RuleID && (c.Action != "upsert" || c.SealedRule != "")
	})
	history := backupPauseReadDocs[RelayV2Command](s, "relay_v2_command_history", malformed, func(k string, c RelayV2Command) bool {
		return backupPauseCommandValid(c) && k == c.CommandID
	})
	rules := backupPauseReadDocs[UserRule](s, "user_rules", malformed, func(k string, r UserRule) bool {
		return r.ID != "" && r.UserID != "" && r.RouteID != "" && k == r.UserID+":"+r.RouteID
	})
	report.CheckedRules = len(s.Docs["user_rules"])
	controls := backupPauseReadDocs[RelayV2Control](s, "relay_v2_control", malformed, func(k string, c RelayV2Control) bool { return k == "default" && c.Epoch != "" })
	control, controlFound := controls["default"]
	retired, retirementErr := validateRelayRetirements(s, now)
	if retirementErr != nil {
		malformed(relayRetirementCollection)
	}
	if control.RecoveryRequired {
		block("", "", "recovery_required", "控制面处于恢复冻结，不能承诺停站连续性")
	}
	for _, agent := range agents {
		if agent.ReconcileState == "recovery_required" {
			block("", agent.ID, "recovery_required", "节点需要受信恢复核对")
		}
		if agent.AccountingDegraded {
			block("", agent.ID, "accounting_degraded", "节点流量持久化或补报异常，需先处理")
		}
	}
	for _, catalog := range catalogs {
		if _, terminal := retired[catalog.AgentID]; terminal {
			continue
		}
		if _, present := agents[catalog.AgentID]; !present {
			malformed("relay_v2_catalogs")
		}
		if !controlFound || catalog.ControlEpoch != control.Epoch || catalog.ReconcileState == "recovery_required" {
			block("", catalog.AgentID, "recovery_required", "节点命令目录与控制面恢复代次不一致")
		}
	}
	// Execution tasks are checked without validateRestoreIdle's unrelated
	// maintenance prerequisite or wall-clock dependency.
	for key, raw := range s.Docs["tasks"] {
		var task Task
		if !backupPauseDecode(raw, &task) || task.ID != key || task.ID == "" {
			malformed("tasks")
			continue
		}
		switch task.State {
		case "succeeded", "failed", "cancelled":
		default:
			block("", "", "execution_busy", "存在未结束或结果不明的执行任务，不能安全停站")
		}
	}
	if len(s.Docs["backup_operations"]) > 0 {
		block("", "", "operation_in_progress", "存在进行中或崩溃后未核实的外部备份操作，需先核对")
	}
	// Existing backup records are normally written only after completion. The
	// durable admission gate must ALSO account for in-flight filesystem work.
	for _, raw := range s.Docs["backups"] {
		var record BackupRecord
		if !backupPauseDecode(raw, &record) {
			malformed("backups")
			continue
		}
		switch record.Status {
		case "verified", "partial", "failed", "local_missing", "local_removed":
		default:
			block("", "", "backup_busy", "存在未完成或状态不明的备份记录")
		}
	}
	byID := map[string]UserRule{}
	segments := map[string]bool{}
	for _, rule := range rules {
		if _, duplicate := byID[rule.ID]; duplicate {
			block(rule.ID, "", "invalid_record", "规则身份重复")
		}
		byID[rule.ID] = rule
		if rule.ReconcileState != "" && rule.ReconcileState != "in_sync" {
			block(rule.ID, "", "reconcile_required", "规则配置或恢复状态尚未核对完成")
		}
		switch rule.State {
		case "active", "pending", "paused", "failed", "suspended", "quota_exhausted", "awaiting_front", "revoking":
		default:
			block(rule.ID, "", "unknown_rule_state", "规则状态未知，不能推断进程已停止")
		}
		if len(rule.Segments) == 0 {
			block(rule.ID, "", "missing_segments", "现存规则缺少逐段运行或停止证明")
		}
		for _, seg := range rule.Segments {
			key := seg.AgentID + ":" + rule.ID
			if seg.AgentID == "" || seg.Runtime.ID != rule.ID || segments[key] {
				block(rule.ID, seg.AgentID, "invalid_record", "转发段身份缺失或重复")
			}
			segments[key] = true
			command, found := commands[key]
			exact := found && backupPauseSegmentMatches(seg, command)
			// Only a terminal revoke is exempt from current capability/freshness
			// checks. A paused rule can resume and must retain its durable stop.
			if rule.State == "revoking" && exact && command.Action == "revoke" && command.AckState == "stopped" && command.AppliedAt > 0 && command.AppliedAt <= now && seg.AckAt == command.AppliedAt && seg.RuntimeObservedAt == command.AppliedAt && seg.RuntimeState == "stopped" {
				continue
			}
			if seg.ProtocolVersion != relayruntime.ProtocolV2 {
				agent, agentExists := agents[seg.AgentID]
				// Legacy explicit stop plus expired lease is terminal only when
				// there is no v2 evidence for the segment or its node.
				if rule.State == "revoking" && seg.AckState == "stopped" && seg.AckAt > 0 && seg.AckAt <= now && seg.LastLease <= now && !found && !restoreSegmentMayKeepLast(s, seg) && agent.ProtocolVersion < 2 {
					continue
				}
				// A risk confirmation may waive legacy lease continuity only;
				// it cannot turn missing ownership or contradictory v2 evidence
				// into a trusted legacy chain.
				var route Route
				user, userExists := s.Users[rule.UserID]
				if !agentExists || !userExists || user.ID != rule.UserID || !backupPauseDecode(s.Docs["routes"][rule.RouteID], &route) || route.ID != rule.RouteID || seg.ProtocolVersion < 0 || seg.ProtocolVersion > 1 {
					block(rule.ID, seg.AgentID, "invalid_record", "旧转发段缺少一致的用户、线路或节点身份")
				}
				if found || agent.ProtocolVersion >= relayruntime.ProtocolV2 || seg.ConfigGeneration != 0 || seg.AppliedGeneration != 0 || seg.LastCommandID != "" {
					block(rule.ID, seg.AgentID, "command_unconfirmed", "旧协议标记与已存在的 v2 命令证据冲突")
				}
				if seg.ConfigError || seg.Runtime.Version <= 0 || seg.Runtime.Version != rule.Version || seg.AckAt <= 0 || seg.AckAt > now {
					block(rule.ID, seg.AgentID, "command_unconfirmed", "旧转发段缺少当前配置的有效确认")
				}
				action, _ := relayV2Desired(s, agent, rule, now)
				if rule.State == "pending" || rule.State == "awaiting_front" || action == "revoke" || action == "upsert" && seg.AckState != "ready" || action == "pause" && seg.AckState != "stopped" {
					block(rule.ID, seg.AgentID, "intent_unconfirmed", "旧链路仍有待完成的运行或停止意图，维护风险确认不能替代执行结果")
				}
				if seg.Runtime.Protocol == "tls" {
					if seg.Runtime.TLSCertificate == "" || seg.SealedTLSKey == "" {
						block(rule.ID, seg.AgentID, "invalid_record", "旧转发段的持久 TLS 身份不完整")
					}
				} else if _, err := relayruntime.RuntimeHash(seg.Runtime); err != nil {
					block(rule.ID, seg.AgentID, "invalid_record", "旧转发段的运行配置无效")
				}
				block(rule.ID, seg.AgentID, "offline_unsupported", "存在 v1 或未确认协议的转发段，停站后可能触发短租约中断")
				continue
			}
			agent, exists := agents[seg.AgentID]
			catalog, catalogExists := catalogs[seg.AgentID]
			if !exists || agent.ProtocolVersion != relayruntime.ProtocolV2 || agent.OfflinePolicy != relayruntime.KeepLast || !relayV2Capabilities(agent.Capabilities) || !agent.KeepLastConfirmed {
				block(rule.ID, seg.AgentID, "offline_unconfirmed", "节点尚未完整确认 v2 keep_last、持久配置、显式停止及流量 ACK 能力")
			}
			if !fresh(agent.LastSeen) || !fresh(seg.AckAt) || !fresh(seg.RuntimeObservedAt) || !fresh(command.AppliedAt) {
				block(rule.ID, seg.AgentID, "stale_observation", "节点心跳或逐段运行证明已超过 90 秒或时间异常")
			}
			if !catalogExists || !controlFound || catalog.ControlEpoch != control.Epoch || catalog.InstanceID == "" || catalog.InstanceID != agent.BootID || catalog.Sequence <= 0 || catalog.Revision < command.Revision {
				block(rule.ID, seg.AgentID, "catalog_unconfirmed", "当前节点进程身份或命令目录版本未确认")
			}
			if _, retired := s.Docs["relay_v2_retired_instances"][seg.AgentID+":"+catalog.InstanceID]; retired {
				block(rule.ID, seg.AgentID, "recovery_required", "当前节点进程身份已退役")
			}
			if !exact || seg.ConfigError || command.AppliedAt != seg.AckAt || command.AppliedAt != seg.RuntimeObservedAt || seg.RuntimeState != seg.AckState || seg.AckAt > agent.LastSeen {
				block(rule.ID, seg.AgentID, "command_unconfirmed", "当前配置代次、命令或实际运行状态尚未得到一致 ACK")
			}
			action, _ := relayV2Desired(s, agent, rule, now)
			if command.Action != action || action == "upsert" && (command.AckState != "ready" || seg.StopConfirmed) || action != "upsert" && (command.AckState != "stopped" || !seg.StopConfirmed) {
				block(rule.ID, seg.AgentID, "intent_unconfirmed", "当前运行或停止意图尚未确认，不能在此时停站")
			}
			if command.Action == "upsert" {
				var grant RelayBillingGrantV2
				if !backupPauseDecode(s.Docs["relay_v2_billing_grants"][command.BillingPeriodID], &grant) || grant.ID != command.BillingPeriodID || grant.AgentID != seg.AgentID || grant.RuleID != rule.ID || grant.UserID != rule.UserID || grant.EntitlementVersion != rule.EntitlementVersion || grant.Billing != seg.Runtime.Billing || grant.TrafficMode != rule.TrafficMode || grant.Multiplier != rule.TrafficMultiplierPermille || seg.Runtime.Version < grant.FirstVersion || seg.Runtime.Version > grant.LastVersion {
					block(rule.ID, seg.AgentID, "accounting_unconfirmed", "当前命令缺少一致的历史流量记账授权")
				}
				route, routeOK := LoadDoc[Route](s, "routes", rule.RouteID)
				if !routeOK || rule.EntitlementVersion != entitlementVersion(s, rule.UserID) || seg.Runtime.ID != rule.ID || seg.Runtime.Version != rule.Version || seg.IssuedRateMbps != seg.Runtime.RateMbps || seg.Runtime.RateMbps != relayRate(s.Users[rule.UserID], route) || seg.IssuedEntitlementVersion != rule.EntitlementVersion || seg.Runtime.EntitlementVersion != rule.EntitlementVersion {
					block(rule.ID, seg.AgentID, "policy_unconfirmed", "当前权益、限速或配置版本尚未在节点确认")
				}
				// TLS private material is sealed and unavailable to this pure
				// State check. Identity/hash/generation/ACK equality establishes
				// offline-policy readiness, NOT a GOST/TLS connectivity test.
				if seg.Runtime.Protocol == "tcp" {
					hash, err := relayruntime.RuntimeHash(seg.Runtime)
					if err != nil || hash != command.RuntimeHash {
						block(rule.ID, seg.AgentID, "config_unconfirmed", "当前运行配置与已确认内容不一致")
					}
				} else if seg.Runtime.Protocol != "tls" || seg.Runtime.TLSCertificate == "" || seg.SealedTLSKey == "" {
					block(rule.ID, seg.AgentID, "config_unconfirmed", "当前运行协议或持久 TLS 身份无效")
				}
			}
		}
	}
	for key, command := range commands {
		_, terminal := retired[command.AgentID]
		if _, present := agents[command.AgentID]; !present && !terminal {
			malformed("relay_v2_commands")
		}
		old, found := history[command.CommandID]
		if !found || !backupPauseSameIntent(old, command) {
			block(command.RuleID, command.AgentID, "command_history_invalid", "当前命令缺少一致的持久历史记录")
		}
		if !segments[key] && !(command.Action == "revoke" && command.AckState == "stopped" && command.AppliedAt > 0 && command.AppliedAt <= now) {
			block(command.RuleID, command.AgentID, "orphan_command", "存在脱离规则拓扑的命令，缺少终态停止证明")
		}
	}
	for _, old := range backupPauseHistoryConflicts(commands, history) {
		block(old.RuleID, old.AgentID, "orphan_history", "历史命令不能由当前目录证明已安全取代或终止")
	}
	sort.Slice(report.Blockers, func(i, j int) bool {
		a, b := report.Blockers[i], report.Blockers[j]
		if a.RuleID != b.RuleID {
			return a.RuleID < b.RuleID
		}
		if a.AgentID != b.AgentID {
			return a.AgentID < b.AgentID
		}
		if a.Code != b.Code {
			return a.Code < b.Code
		}
		return a.Message < b.Message
	})
	report.CanPauseControl = len(report.Blockers) == 0
	report.CanPauseMaintenance = true
	for _, blocker := range report.Blockers {
		if !backupPauseMaintenanceRisk(blocker.Code) {
			report.CanPauseMaintenance = false
		}
	}
	return report
}

// Deliberately a tiny allowlist, not a blacklist: new checks remain mandatory.
// Stale observations, pending intent/configuration, accounting problems,
// recovery, unknown jobs and malformed state never become maintenance risks.
func backupPauseMaintenanceRisk(code string) bool {
	return code == "offline_unsupported" || code == "offline_unconfirmed"
}

func backupPauseCommandValid(c RelayV2Command) bool {
	return c.AgentID != "" && c.RuleID != "" && c.CommandID != "" && c.Generation > 0 && c.Revision > 0 && (c.Action == "upsert" && c.RuntimeHash != "" || c.Action == "pause" || c.Action == "revoke")
}

func backupPauseSameIntent(a, b RelayV2Command) bool {
	return a.AgentID == b.AgentID && a.RuleID == b.RuleID && a.CommandID == b.CommandID && a.Generation == b.Generation && a.Revision == b.Revision && a.Action == b.Action && a.RuntimeHash == b.RuntimeHash && a.BillingPeriodID == b.BillingPeriodID
}

// A revoke is terminal for this rule identity, not for only one command ID.
// Trusted recovery may explicitly stop it again with a newer revoke. A pause,
// unlike a revoke, can be superseded by a newer running configuration. Require
// both counters and IDs to advance together for every replacement, including
// intermediate history: a later revoke must not hide an earlier resurrection.
func backupPauseHistoryPrecedes(old, current RelayV2Command) bool {
	if old.AgentID != current.AgentID || old.RuleID != current.RuleID {
		return false
	}
	if old.Generation == current.Generation {
		return backupPauseSameIntent(old, current)
	}
	return old.Generation < current.Generation && old.Revision < current.Revision && old.CommandID != current.CommandID && (old.Action != "revoke" || current.Action == "revoke")
}

func backupPauseHistoryConflicts(commands, history map[string]RelayV2Command) []RelayV2Command {
	conflicts := []RelayV2Command{}
	groups := map[string][]RelayV2Command{}
	for _, old := range history {
		key := old.AgentID + ":" + old.RuleID
		current, found := commands[key]
		if !found || !backupPauseHistoryPrecedes(old, current) {
			conflicts = append(conflicts, old)
		}
		groups[key] = append(groups[key], old)
	}
	for _, rows := range groups {
		sort.Slice(rows, func(i, j int) bool { return rows[i].Generation < rows[j].Generation })
		for i := 1; i < len(rows); i++ {
			if !backupPauseHistoryPrecedes(rows[i-1], rows[i]) {
				conflicts = append(conflicts, rows[i])
			}
		}
	}
	return conflicts
}

func backupPauseSegmentMatches(seg RelaySegment, c RelayV2Command) bool {
	return seg.ProtocolVersion == relayruntime.ProtocolV2 && seg.AgentID == c.AgentID && seg.LastCommandID == c.CommandID && seg.ConfigGeneration == c.Generation && seg.AppliedGeneration == c.Generation && seg.LastCommandAction == c.Action && seg.RuntimeHash == c.RuntimeHash && seg.AckState == c.AckState && (c.Action == "upsert" && c.AckState == "ready" && !seg.StopConfirmed || c.Action != "upsert" && c.AckState == "stopped" && seg.StopConfirmed)
}

func backupPauseReadDocs[T any](s *State, collection string, malformed func(string), valid func(string, T) bool) map[string]T {
	out := map[string]T{}
	for key, raw := range s.Docs[collection] {
		var value T
		if !backupPauseDecode(raw, &value) || !valid(key, value) {
			malformed(collection)
			continue
		}
		out[key] = value
	}
	return out
}

// Reject null, unknown fields and duplicate object keys, including nested
// runtime fields. Do not let a malformed raw row disappear through ListDocs.
func backupPauseDecode(raw json.RawMessage, out any) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	if backupPauseUniqueJSON(decoder) != nil {
		return false
	}
	if _, err := decoder.Token(); err != io.EOF {
		return false
	}
	decoder = json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	return decoder.Decode(out) == nil
}

func backupPauseUniqueJSON(d *json.Decoder) error {
	token, err := d.Token()
	if err != nil {
		return err
	}
	delim, container := token.(json.Delim)
	if !container {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			keyToken, err := d.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			// encoding/json matches struct fields case-insensitively too.
			folded := strings.ToLower(key)
			if !ok || seen[folded] {
				return errors.New("duplicate or invalid object key")
			}
			seen[folded] = true
			if err := backupPauseUniqueJSON(d); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := backupPauseUniqueJSON(d); err != nil {
				return err
			}
		}
	default:
		return errors.New("invalid JSON delimiter")
	}
	_, err = d.Token()
	return err
}
