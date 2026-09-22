package control

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"mime"
	"net/http"
	"sort"
	"time"

	"github.com/mozziexwz/node/internal/relayruntime"
)

// These collections are part of the recovery boundary. Restoring an older
// database must freeze them, not clear node occupancy or accept a new epoch as
// an implicit command to replace the last configuration held by a node.
type RelayV2Control struct {
	Epoch            string `json:"epoch"`
	RecoveryRequired bool   `json:"recoveryRequired"`
}
type RelayV2Catalog struct {
	AgentID        string `json:"agentId"`
	ControlEpoch   string `json:"controlEpoch"`
	Revision       int64  `json:"revision"`
	DispatchCursor int64  `json:"dispatchCursor"`
	ReconcileState string `json:"reconcileState,omitempty"`
	InstanceID     string `json:"instanceId,omitempty"`
	Sequence       int64  `json:"sequence,omitempty"`
}
type RelayV2Command struct {
	AgentID         string `json:"agentId"`
	CommandID       string `json:"commandId"`
	RuleID          string `json:"ruleId"`
	Generation      int64  `json:"generation"`
	Revision        int64  `json:"revision"`
	Action          string `json:"action"`
	RuntimeHash     string `json:"runtimeHash,omitempty"`
	BillingPeriodID string `json:"billingPeriodId,omitempty"`
	Reason          string `json:"reason,omitempty"`
	SealedRule      string `json:"sealedRule,omitempty"`
	AckState        string `json:"ackState,omitempty"`
	AppliedAt       int64  `json:"appliedAt,omitempty"`
}

func relayRuleUsesV2(s *State, rule UserRule) bool {
	for _, seg := range rule.Segments {
		if seg.ProtocolVersion == relayruntime.ProtocolV2 {
			return true
		}
		if agent, ok := LoadDoc[RelayAgent](s, "relay_agents", seg.AgentID); ok && agent.ProtocolVersion == relayruntime.ProtocolV2 {
			return true
		}
	}
	return false
}
func relayRecoveryRequired(s *State, rule UserRule) bool {
	if rule.ReconcileState == "recovery_required" {
		return true
	}
	control, _ := LoadDoc[RelayV2Control](s, "relay_v2_control", "default")
	if control.RecoveryRequired && relayRuleUsesV2(s, rule) {
		return true
	}
	for _, seg := range rule.Segments {
		agent, _ := LoadDoc[RelayAgent](s, "relay_agents", seg.AgentID)
		if agent.ReconcileState == "recovery_required" {
			return true
		}
	}
	return false
}
func relaySegmentStopped(seg RelaySegment, now int64) bool {
	if seg.ProtocolVersion == relayruntime.ProtocolV2 {
		return seg.StopConfirmed && (seg.LastCommandAction == "pause" || seg.LastCommandAction == "revoke") && seg.AppliedGeneration == seg.ConfigGeneration && seg.AckState == "stopped"
	}
	return seg.AckState == "stopped" || seg.LastLease <= now
}
func relayRuleStopped(rule UserRule, now int64) bool {
	for _, seg := range rule.Segments {
		if !relaySegmentStopped(seg, now) {
			return false
		}
	}
	return true
}
func relayRuleRevoked(rule UserRule, now int64) bool {
	for _, seg := range rule.Segments {
		if !relaySegmentStopped(seg, now) || seg.ProtocolVersion == relayruntime.ProtocolV2 && seg.LastCommandAction != "revoke" {
			return false
		}
	}
	return true
}
func relayUserV2Pending(s *State, userID string, now int64) bool {
	control, _ := LoadDoc[RelayV2Control](s, "relay_v2_control", "default")
	if control.RecoveryRequired {
		return true
	}
	for _, rule := range ListDocs[UserRule](s, "user_rules") {
		if rule.UserID == userID && (relayRecoveryRequired(s, rule) || relayRuleUsesV2(s, rule) && !relayRuleRevoked(rule, now)) {
			return true
		}
	}
	return false
}

func relayV2Capabilities(capabilities []string) bool {
	if len(capabilities) > 16 {
		return false
	}
	seen := map[string]bool{}
	for _, item := range capabilities {
		if len(item) > 64 || seen[item] {
			return false
		}
		seen[item] = true
	}
	for _, item := range relayruntime.V2Capabilities {
		if !seen[item] {
			return false
		}
	}
	return true
}
func relayV2Identifier(value string) bool {
	if len(value) < 16 || len(value) > 100 {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}
func relayV2AckMatches(command RelayV2Command, ack relayruntime.V2Ack) bool {
	return ack.CommandID == command.CommandID && ack.RuleID == command.RuleID && ack.Generation == command.Generation && ack.RuntimeHash == command.RuntimeHash
}
func relayV2KnownAck(s *State, agent RelayAgent, ack relayruntime.V2Ack) bool {
	command, ok := LoadDoc[RelayV2Command](s, "relay_v2_command_history", ack.CommandID)
	if !ok || command.AgentID != agent.ID || !relayV2AckMatches(command, ack) {
		return false
	}
	switch ack.State {
	case "persisted", "failed":
		return true
	case "ready":
		return command.Action == "upsert"
	case "stopping", "stopped":
		return command.Action == "pause" || command.Action == "revoke" || command.Action == "upsert"
	default:
		return false
	}
}
func relayV2ApplyAck(s *State, agent RelayAgent, ack relayruntime.V2Ack, now int64) error {
	key := agent.ID + ":" + ack.RuleID
	command, ok := LoadDoc[RelayV2Command](s, "relay_v2_commands", key)
	if !ok || !relayV2AckMatches(command, ack) {
		return nil
	} // known superseded intent
	// A persisted/failed retry cannot erase a terminal matching stop proof.
	if command.AckState == "stopped" && ack.State != "stopped" && (command.Action == "pause" || command.Action == "revoke") {
		return nil
	}
	command.AckState = ack.State
	if ack.State == "ready" || ack.State == "stopped" {
		command.AppliedAt = now
	}
	if err := SaveDoc(s, "relay_v2_commands", key, command); err != nil {
		return err
	}
	for _, rule := range ListDocs[UserRule](s, "user_rules") {
		if rule.ID != ack.RuleID {
			continue
		}
		for i := range rule.Segments {
			seg := &rule.Segments[i]
			if seg.AgentID != agent.ID || seg.LastCommandID != command.CommandID || seg.ConfigGeneration != ack.Generation {
				continue
			}
			seg.AckState, seg.RuntimeState, seg.AckAt, seg.RuntimeObservedAt = ack.State, ack.State, now, now
			if ack.State == "ready" {
				seg.AppliedGeneration, seg.EverReady = ack.Generation, true
			}
			if ack.State == "stopped" && (command.Action == "pause" || command.Action == "revoke") {
				seg.AppliedGeneration, seg.StopConfirmed = ack.Generation, true
			}
		}
		return SaveDoc(s, "user_rules", rule.UserID+":"+rule.RouteID, rule)
	}
	return nil // a retained tombstone can be re-ACKed after archival
}

// A fresh process cannot inherit an older process's live-ready observation.
// Keep the same durable command and generation; the new instance must report
// its own readiness. Explicit, matching stop tombstones remain terminal.
func relayV2NewInstance(s *State, agentID string) error {
	for key, raw := range s.Docs["relay_v2_commands"] {
		var command RelayV2Command
		if json.Unmarshal(raw, &command) != nil || command.AgentID != agentID || command.Action != "upsert" || command.AckState != "ready" {
			continue
		}
		command.AckState = "persisted"
		if err := SaveDoc(s, "relay_v2_commands", key, command); err != nil {
			return err
		}
	}
	for _, rule := range ListDocs[UserRule](s, "user_rules") {
		changed := false
		for i := range rule.Segments {
			seg := &rule.Segments[i]
			if seg.AgentID == agentID && seg.LastCommandAction == "upsert" && seg.AckState == "ready" {
				seg.AckState, seg.RuntimeState, seg.RuntimeObservedAt = "persisted", "unknown", 0
				changed = true
			}
		}
		if changed {
			if err := SaveDoc(s, "user_rules", rule.UserID+":"+rule.RouteID, rule); err != nil {
				return err
			}
		}
	}
	return nil
}

func relayV2Desired(s *State, agent RelayAgent, rule UserRule, now int64) (string, string) {
	u := s.Users[rule.UserID]
	if rule.State == "revoking" || u == nil || u.ExpiresAt <= now {
		return "revoke", "expired_or_revoked"
	}
	route, exists := LoadDoc[Route](s, "routes", rule.RouteID)
	if !agent.Enabled {
		return "pause", "node_disabled"
	}
	if !exists || !route.Enabled {
		return "pause", "route_disabled"
	}
	if !userCanUseRoute(u, route) {
		return "pause", "entitlement_level"
	}
	requiredFront, _ := relayEffective(s, route)
	if requiredFront && !rule.HasFront && rule.State != "awaiting_front" {
		return "pause", "front_unavailable"
	}
	if rule.State == "paused" {
		return "pause", "paused"
	}
	if rule.State == "failed" {
		return "pause", "deployment_failed"
	}
	if u.Status != "active" {
		return "pause", "account_suspended"
	}
	if u.TrafficUsed >= u.TrafficTotal {
		return "pause", "quota_exhausted"
	}
	return "upsert", "authorized"
}

func (a *App) relayV2Issue(s *State, catalog *RelayV2Catalog, agent RelayAgent, rule *UserRule, index int, action, reason string) error {
	seg := &rule.Segments[index]
	key := agent.ID + ":" + rule.ID
	previous, exists := LoadDoc[RelayV2Command](s, "relay_v2_commands", key)
	if exists && previous.Action == "revoke" {
		return nil
	} // globally unique ID is terminal
	command := RelayV2Command{AgentID: agent.ID, RuleID: rule.ID, Action: action, Reason: reason, RuntimeHash: previous.RuntimeHash}
	var wireRule relayruntime.Rule
	if action == "upsert" {
		wireRule = seg.Runtime
		wireRule.LeaseUntil = 0
		if wireRule.Protocol == "tls" {
			plain, err := a.Open(seg.SealedTLSKey)
			if err != nil {
				return errors.New("invalid sealed TLS identity")
			}
			wireRule.TLSPrivateKey = string(plain)
		}
		hash, err := relayruntime.RuntimeHash(wireRule)
		if err != nil {
			return err
		}
		command.RuntimeHash = hash
		period, err := grantRelayBillingV2(s, agent.ID, *rule, wireRule)
		if err != nil {
			return err
		}
		command.BillingPeriodID = period
	}
	if exists && previous.Action == action && previous.RuntimeHash == command.RuntimeHash && previous.BillingPeriodID == command.BillingPeriodID {
		return nil
	}
	if catalog.Revision == math.MaxInt64 || previous.Generation == math.MaxInt64 {
		return errors.New("command generation exhausted")
	}
	command.CommandID, command.Generation, command.Revision = commerceID(), previous.Generation+1, catalog.Revision+1
	if action == "upsert" {
		raw, err := json.Marshal(wireRule)
		if err != nil {
			return err
		}
		sealed, err := a.Seal(raw)
		if err != nil {
			return err
		}
		command.SealedRule = sealed
	}
	if err := SaveDoc(s, "relay_v2_commands", key, command); err != nil {
		return err
	}
	history := command
	history.SealedRule = ""
	if err := SaveDoc(s, "relay_v2_command_history", command.CommandID, history); err != nil {
		return err
	}
	catalog.Revision = command.Revision
	// Resource protection begins at durable issue, even if the HTTP response
	// carrying this command is lost. It must not wait for a ready ACK.
	seg.ProtocolVersion, seg.ConfigGeneration = relayruntime.ProtocolV2, command.Generation
	seg.LastCommandID, seg.LastCommandAction, seg.RuntimeHash = command.CommandID, command.Action, command.RuntimeHash
	if action == "upsert" {
		seg.IssuedRateMbps, seg.IssuedEntitlementVersion = wireRule.RateMbps, wireRule.EntitlementVersion
	}
	seg.StopConfirmed, seg.AckState = false, "pending"
	return nil
}

func (a *App) relayV2Commands(s *State, catalog *RelayV2Catalog, agent RelayAgent) ([]relayruntime.V2Command, error) {
	rows := []RelayV2Command{}
	for _, command := range ListDocs[RelayV2Command](s, "relay_v2_commands") {
		if command.AgentID != agent.ID || command.Action == "upsert" && command.AckState == "ready" || command.Action != "upsert" && command.AckState == "stopped" {
			continue
		}
		rows = append(rows, command)
	}
	// Rotate the bounded resend window so one failed rule cannot starve all
	// other commands, while a lost response remains retriable and idempotent.
	sort.Slice(rows, func(i, j int) bool {
		iAfter, jAfter := rows[i].Revision > catalog.DispatchCursor, rows[j].Revision > catalog.DispatchCursor
		if iAfter != jAfter {
			return iAfter
		}
		return rows[i].Revision < rows[j].Revision
	})
	if len(rows) > relayruntime.MaxV2Commands {
		rows = rows[:relayruntime.MaxV2Commands]
	}
	out := make([]relayruntime.V2Command, 0, len(rows))
	for _, row := range rows {
		command := relayruntime.V2Command{CommandID: row.CommandID, RuleID: row.RuleID, Generation: row.Generation, Action: row.Action, RuntimeHash: row.RuntimeHash, BillingPeriodID: row.BillingPeriodID, Reason: row.Reason}
		if row.Action == "upsert" {
			plain, err := a.Open(row.SealedRule)
			if err != nil {
				return nil, errors.New("invalid protected command")
			}
			var rule relayruntime.Rule
			if json.Unmarshal(plain, &rule) != nil {
				return nil, errors.New("invalid protected rule")
			}
			command.Rule = &rule
		}
		out = append(out, command)
		catalog.DispatchCursor = row.Revision
	}
	return out, nil
}

func decodeRelayV2(r *http.Request, in *relayruntime.V2SyncRequest) error {
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || contentType != "application/json" {
		return errors.New("请求必须使用 application/json")
	}
	const limit = 2 << 20
	raw, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil || len(raw) > limit {
		return errors.New("v2 同步请求超过 2 MiB 上限或无法读取")
	}
	if !backupPauseDecode(raw, in) {
		return errors.New("请求 JSON 格式或字段无效")
	}
	return nil
}

func (a *App) relaySyncAgentV2(w http.ResponseWriter, r *http.Request) {
	var in relayruntime.V2SyncRequest
	if err := decodeRelayV2(r, &in); err != nil {
		commerceError(w, 400, err)
		return
	}
	if in.ProtocolVersion != relayruntime.ProtocolV2 || !relayV2Identifier(in.AgentID) || !relayV2Identifier(in.AgentInstanceID) || !relayV2Identifier(in.RequestID) || in.Sequence <= 0 || in.AppliedRevision < 0 || len(in.Acks) > 4096 || len(in.Traffic) > relayruntime.MaxV2Traffic || !relayV2Capabilities(in.Capabilities) {
		commerceError(w, 400, errors.New("invalid v2 sync envelope"))
		return
	}
	now := time.Now().UnixMilli()
	out := relayruntime.V2SyncResponse{ProtocolVersion: relayruntime.ProtocolV2, AgentID: in.AgentID, RequestID: in.RequestID, PreviousRevision: in.AppliedRevision, OfflinePolicy: relayruntime.KeepLast, Status: "ready", Commands: []relayruntime.V2Command{}, TrafficAcks: []relayruntime.V2TrafficAck{}}
	authFailure := false
	err := a.Store.Update(func(s *State) error {
		agent, err := a.relayAgentIdentity(s, r)
		if err != nil || agent.ID != in.AgentID {
			authFailure = true
			return errors.New("relay authentication failed")
		}
		if handled, err := relayRecoverySync(s, &agent, in, &out, now); handled || err != nil {
			return err
		}
		control, found := LoadDoc[RelayV2Control](s, "relay_v2_control", "default")
		if !found {
			control = RelayV2Control{Epoch: commerceID()}
			if err := SaveDoc(s, "relay_v2_control", "default", control); err != nil {
				return err
			}
		}
		catalog, exists := LoadDoc[RelayV2Catalog](s, "relay_v2_catalogs", agent.ID)
		if !exists {
			catalog = RelayV2Catalog{AgentID: agent.ID, ControlEpoch: control.Epoch}
		}
		out.ControlEpoch, out.Revision = control.Epoch, catalog.Revision
		bootstrap := in.ControlEpoch == "" && catalog.Revision == 0 && !agent.KeepLastConfirmed && len(in.Acks) == 0 && in.AppliedRevision == 0
		conflict := control.RecoveryRequired || agent.ReconcileState == "recovery_required" || catalog.ReconcileState == "recovery_required" || catalog.ControlEpoch != control.Epoch || (!bootstrap && in.ControlEpoch != control.Epoch) || in.AppliedRevision > catalog.Revision
		newInstance := catalog.InstanceID != "" && catalog.InstanceID != in.AgentInstanceID
		_, retired := s.Docs["relay_v2_retired_instances"][agent.ID+":"+in.AgentInstanceID]
		if retired {
			conflict = true
		}
		freshObservation := catalog.InstanceID != in.AgentInstanceID || in.Sequence > catalog.Sequence
		seen := map[string]bool{}
		for _, ack := range in.Acks {
			if seen[ack.RuleID] || !relayV2KnownAck(s, agent, ack) {
				conflict = true
			}
			seen[ack.RuleID] = true
		}
		agent.ProtocolVersion, agent.OfflinePolicy, agent.Capabilities = relayruntime.ProtocolV2, relayruntime.KeepLast, append([]string(nil), in.Capabilities...)
		if !conflict && freshObservation {
			if newInstance {
				if err := SaveDoc(s, "relay_v2_retired_instances", agent.ID+":"+catalog.InstanceID, true); err != nil {
					return err
				}
				if err := relayV2NewInstance(s, agent.ID); err != nil {
					return err
				}
			}
			catalog.InstanceID, catalog.Sequence = in.AgentInstanceID, in.Sequence
			agent.LastSeen, agent.BootID, agent.AccountingDegraded = now, in.AgentInstanceID, in.AccountingDegraded
			agent.ControlStatus = "online"
		}
		for _, rule := range ListDocs[UserRule](s, "user_rules") {
			changed := false
			for i := range rule.Segments {
				if rule.Segments[i].AgentID == agent.ID && rule.Segments[i].ProtocolVersion != relayruntime.ProtocolV2 {
					rule.Segments[i].ProtocolVersion = relayruntime.ProtocolV2
					rule.Segments[i].StopConfirmed = false
					changed = true
				}
			}
			if changed {
				if err := SaveDoc(s, "user_rules", rule.UserID+":"+rule.RouteID, rule); err != nil {
					return err
				}
			}
		}
		if conflict {
			out.Status = "recovery_required"
			agent.ReconcileState, catalog.ReconcileState, agent.KeepLastConfirmed = "recovery_required", "recovery_required", false
		} else if !bootstrap {
			// Each invalid sample is isolated by the accounting function before
			// mutation. Successful ACKs leave this handler only after COMMIT.
			for _, sample := range in.Traffic {
				ack, err := applyRelayTrafficV2(s, agent, sample, now)
				if err != nil {
					agent.AccountingDegraded = true
					continue
				}
				out.TrafficAcks = append(out.TrafficAcks, ack)
			}
			if freshObservation {
				for _, ack := range in.Acks {
					if err := relayV2ApplyAck(s, agent, ack, now); err != nil {
						return err
					}
				}
			}
			// Persist capability before cleanup, so previously unissued segments
			// on an opted-in node cannot still be freed by a v1 wall-clock path.
			if err := SaveDoc(s, "relay_agents", agent.ID, agent); err != nil {
				return err
			}
			if err := relayCleanup(s, now); err != nil {
				return err
			}
			for _, rule := range ListDocs[UserRule](s, "user_rules") {
				if relayRecoveryRequired(s, rule) {
					continue
				}
				action, reason := relayV2Desired(s, agent, rule, now)
				for i, seg := range rule.Segments {
					if seg.AgentID != agent.ID {
						continue
					}
					if action == "upsert" && agent.AccountingDegraded {
						continue
					}
					if action == "upsert" && seg.Runtime.Billing && !seg.EverReady {
						ready := true
						for _, downstream := range rule.Segments {
							if !downstream.Runtime.Billing && !relaySegmentReady(s, downstream, now) {
								ready = false
							}
						}
						if !ready {
							continue
						}
					}
					if err := a.relayV2Issue(s, &catalog, agent, &rule, i, action, reason); err != nil {
						rule.Segments[i].ConfigError = true
						continue
					}
					rule.Segments[i].ConfigError = false
				}
				configError := false
				for _, seg := range rule.Segments {
					configError = configError || seg.ConfigError
				}
				if configError {
					rule.ReconcileState = "config_error"
				} else if rule.ReconcileState == "config_error" {
					rule.ReconcileState = ""
				}
				allReady := len(rule.Segments) > 0
				for _, seg := range rule.Segments {
					allReady = allReady && relaySegmentReady(s, seg, now)
				}
				if allReady && rule.State == "pending" {
					rule.State = "active"
				}
				if err := SaveDoc(s, "user_rules", rule.UserID+":"+rule.RouteID, rule); err != nil {
					return err
				}
			}
			out.Commands, err = a.relayV2Commands(s, &catalog, agent)
			if err != nil {
				return err
			}
			agent.KeepLastConfirmed = true
			for _, rule := range ListDocs[UserRule](s, "user_rules") {
				for _, seg := range rule.Segments {
					if seg.AgentID == agent.ID && (seg.ProtocolVersion != relayruntime.ProtocolV2 || seg.ConfigGeneration <= 0 || seg.AppliedGeneration != seg.ConfigGeneration || seg.AckState != "ready" && seg.AckState != "stopped") {
						agent.KeepLastConfirmed = false
					}
				}
			}
			agent.ReconcileState = "in_sync"
			if len(out.Commands) > 0 {
				agent.ReconcileState = "pending"
			}
		}
		out.Revision = catalog.Revision
		if err := SaveDoc(s, "relay_v2_catalogs", agent.ID, catalog); err != nil {
			return err
		}
		return SaveDoc(s, "relay_agents", agent.ID, agent)
	})
	if err != nil {
		if authFailure {
			commerceError(w, 401, errors.New("relay authentication failed"))
		} else {
			commerceError(w, 503, errors.New("控制面同步未提交，保留最后配置并重试"))
		}
		return
	}
	WriteJSON(w, 200, out)
}
