package control

import (
	"errors"
	"time"

	"github.com/mozziexwz/node/internal/relayruntime"
)

const relayRetirementCollection = "relay_v2_agent_retirements"

// A terminal administrative retirement is not an orphan exemption. The live
// identity is removed only after proof, while every original command, history,
// catalogue and accounting record remains. Digests bind that complete evidence
// against later loss/editing; a restored control epoch never rewrites it.
type relayAgentRetirement struct {
	Version              int            `json:"version"`
	Agent                RelayAgent     `json:"agent"`
	RetiredAt            int64          `json:"retiredAt"`
	Catalog              RelayV2Catalog `json:"catalog"`
	CommandsDigest       string         `json:"commandsDigest"`
	HistoryDigest        string         `json:"historyDigest"`
	RecoveryDigest       string         `json:"recoveryDigest"`
	DegradedReviewDigest string         `json:"degradedReviewDigest,omitempty"`
}

var errRelayRetirement = errors.New("节点缺少完整终态退役证明或原证据发生变化；保留身份与全部历史，不释放未知节点")

func relayRetirementReconciled(value string) bool { return value == "" || value == "in_sync" }

func relayRetirementEvidence(s *State, id string, now int64) (RelayV2Catalog, string, string, string, error) {
	var catalog RelayV2Catalog
	if !backupPauseDecode(s.Docs["relay_v2_catalogs"][id], &catalog) || catalog.AgentID != id || !relayV2Identifier(catalog.ControlEpoch) || !relayV2Identifier(catalog.InstanceID) || catalog.Revision < 0 || catalog.Sequence <= 0 || !relayRetirementReconciled(catalog.ReconcileState) {
		return catalog, "", "", "", errRelayRetirement
	}
	if _, retired := s.Docs["relay_v2_retired_instances"][id+":"+catalog.InstanceID]; retired {
		return catalog, "", "", "", errRelayRetirement
	}
	bad := false
	malformed := func(string) { bad = true }
	allCommands := backupPauseReadDocs[RelayV2Command](s, "relay_v2_commands", malformed, func(k string, c RelayV2Command) bool {
		return k == c.AgentID+":"+c.RuleID && backupPauseCommandValid(c)
	})
	allHistory := backupPauseReadDocs[RelayV2Command](s, "relay_v2_command_history", malformed, func(k string, c RelayV2Command) bool { return k == c.CommandID && backupPauseCommandValid(c) })
	rules := backupPauseReadDocs[UserRule](s, "user_rules", malformed, func(k string, r UserRule) bool { return k == r.UserID+":"+r.RouteID && r.ID != "" })
	routes := backupPauseReadDocs[Route](s, "routes", malformed, func(k string, r Route) bool { return k == r.ID && k != "" })
	archives := backupPauseReadDocs[UserRule](s, "relay_rule_archive", malformed, func(k string, r UserRule) bool { return k == r.ID && k != "" })
	if bad {
		return catalog, "", "", "", errRelayRetirement
	}
	for _, rule := range rules {
		for _, segment := range rule.Segments {
			if segment.AgentID == id {
				return catalog, "", "", "", errRelayRetirement
			}
		}
	}
	for _, route := range routes {
		for _, agentID := range relayRouteAgents(route) {
			if agentID == id {
				return catalog, "", "", "", errRelayRetirement
			}
		}
	}
	commands, history := map[string]RelayV2Command{}, map[string]RelayV2Command{}
	for key, command := range allCommands {
		if command.AgentID != id {
			continue
		}
		if command.Action != "revoke" || command.AckState != "stopped" || command.AppliedAt <= 0 || command.AppliedAt > now || command.Revision > catalog.Revision {
			return catalog, "", "", "", errRelayRetirement
		}
		old, found := allHistory[command.CommandID]
		if !found || !backupPauseSameIntent(old, command) {
			return catalog, "", "", "", errRelayRetirement
		}
		archive, found := archives[command.RuleID]
		if !found || archive.State != "revoked" {
			return catalog, "", "", "", errRelayRetirement
		}
		matching := 0
		for _, seg := range archive.Segments {
			if seg.AgentID == id {
				if !backupPauseSegmentMatches(seg, command) || seg.Runtime.ID != command.RuleID || seg.AckAt <= 0 || seg.RuntimeObservedAt != seg.AckAt || seg.AckAt > command.AppliedAt {
					return catalog, "", "", "", errRelayRetirement
				}
				matching++
			}
		}
		if matching != 1 {
			return catalog, "", "", "", errRelayRetirement
		}
		commands[key] = command
	}
	for key, old := range allHistory {
		if old.AgentID == id {
			history[key] = old
		}
	}
	for _, archive := range archives {
		for _, seg := range archive.Segments {
			if seg.AgentID == id {
				if _, found := commands[id+":"+archive.ID]; !found {
					return catalog, "", "", "", errRelayRetirement
				}
			}
		}
	}
	if len(backupPauseHistoryConflicts(commands, history)) != 0 {
		return catalog, "", "", "", errRelayRetirement
	}
	// Completed recovery artefacts remain forensic history. An unfinished plan
	// cannot be converted to retirement, and missing/replaced artefacts fail.
	var prepared *relayRecoveryPrepared
	var snapshot *relayruntime.V2RecoverySnapshot
	if raw, exists := s.Docs["relay_v2_recovery_prepared"][id]; exists {
		var p relayRecoveryPrepared
		if !backupPauseDecode(raw, &p) || p.AgentID != id || !p.AllDecided || p.FinalizedAt <= 0 || p.FinalizedAt > now || p.PlanSequence <= 0 || p.SealedPlan == "" || !relayV2Identifier(p.PlanID) || !relayV2Identifier(p.RecoveryID) || p.InstanceID != catalog.InstanceID || p.Sequence <= 0 || p.Sequence > catalog.Sequence || p.VerifiedAt <= 0 || p.VerifiedAt > p.FinalizedAt || p.Expected == nil {
			return catalog, "", "", "", errRelayRetirement
		}
		prepared = &p
	}
	if raw, exists := s.Docs["relay_v2_recovery_snapshots"][id]; exists {
		var snap relayruntime.V2RecoverySnapshot
		if !backupPauseDecode(raw, &snap) || snap.AgentID != id || prepared == nil || snap.RecoveryID != prepared.RecoveryID || validateRelayRecoverySnapshot(snap) != nil {
			return catalog, "", "", "", errRelayRetirement
		}
		snapshot = &snap
	}
	if prepared != nil && snapshot == nil {
		return catalog, "", "", "", errRelayRetirement
	}
	if len(commands) == 0 && catalog.Revision > 0 {
		// A nonzero catalogue is evidence of prior inventory, not permission to
		// treat missing collections as an empty node. Only an already finalized
		// root recovery of an explicitly empty original snapshot can prove this.
		if prepared == nil || prepared.Expected == nil || len(prepared.Expected) != 0 || snapshot == nil || len(snapshot.Records) != 0 || len(history) != 0 || validateRelayRecoverySnapshot(*snapshot) != nil {
			return catalog, "", "", "", errRelayRetirement
		}
	}
	return catalog, recoveryDigest(commands), recoveryDigest(history), recoveryDigest(struct {
		Prepared *relayRecoveryPrepared
		Snapshot *relayruntime.V2RecoverySnapshot
	}{prepared, snapshot}), nil
}

func validateRelayRetirements(s *State, now int64) (map[string]relayAgentRetirement, error) {
	retired := map[string]relayAgentRetirement{}
	if len(s.Docs[relayRetirementCollection]) > 0 {
		var control RelayV2Control
		if len(s.Docs["relay_v2_control"]) != 1 || !backupPauseDecode(s.Docs["relay_v2_control"]["default"], &control) || !relayV2Identifier(control.Epoch) {
			return nil, errRelayRetirement
		}
	}
	for id, raw := range s.Docs[relayRetirementCollection] {
		var record relayAgentRetirement
		if !backupPauseDecode(raw, &record) || record.Version != 1 || record.Agent.ID != id || !relayV2Identifier(id) || record.RetiredAt <= 0 || record.RetiredAt > now || record.Agent.Capability != "relay" || record.Agent.ProtocolVersion != 2 || record.Agent.OfflinePolicy != "keep_last" || !record.Agent.KeepLastConfirmed || !relayV2Capabilities(record.Agent.Capabilities) || record.Agent.Enabled || record.Agent.Online || !relayRetirementReconciled(record.Agent.ReconcileState) || record.Agent.TokenHash != "" || record.Agent.EnrollmentHash != "" || record.Agent.EnrollmentExpires != 0 {
			return nil, errRelayRetirement
		}
		if _, alive := s.Docs["relay_agents"][id]; alive {
			return nil, errRelayRetirement
		}
		if record.Agent.AccountingDegraded {
			if !relayDegradedReviewValid(s, record.Agent, record.RetiredAt) || record.DegradedReviewDigest != recoveryDigest(s.Docs[relayDegradedReviewCollection][id]) {
				return nil, errRelayRetirement
			}
		} else if record.DegradedReviewDigest != "" {
			return nil, errRelayRetirement
		}
		catalog, commands, history, recovery, err := relayRetirementEvidence(s, id, record.RetiredAt)
		if err != nil || record.Agent.BootID != catalog.InstanceID || recoveryDigest(catalog) != recoveryDigest(record.Catalog) || commands != record.CommandsDigest || history != record.HistoryDigest || recovery != record.RecoveryDigest {
			return nil, errRelayRetirement
		}
		retired[id] = record
	}
	return retired, nil
}

// Called only inside the same Store.Update transaction as DELETE. Legacy-only
// identities retain their established deletion behaviour; v2 traces require a
// complete irreversible-stop proof and a non-registerable retirement record.
func retireRelayAgent(s *State, id string, now int64) error {
	if _, err := validateRelayRetirements(s, now); err != nil {
		return err
	}
	var agent RelayAgent
	raw, exists := s.Docs["relay_agents"][id]
	if !exists {
		if _, retired := s.Docs[relayRetirementCollection][id]; retired {
			return nil
		}
		if restoreAgentNeedsRecovery(s, RelayAgent{ID: id}) {
			return errRelayRetirement
		}
		return nil
	}
	if !backupPauseDecode(raw, &agent) || agent.ID != id {
		return errRelayRetirement
	}
	// Even legacy removal cannot use a display parser that silently skips a
	// damaged route/rule and mistakes unknown references for no references.
	bad := false
	rules := backupPauseReadDocs[UserRule](s, "user_rules", func(string) { bad = true }, func(k string, v UserRule) bool { return k == v.UserID+":"+v.RouteID && v.ID != "" })
	routes := backupPauseReadDocs[Route](s, "routes", func(string) { bad = true }, func(k string, v Route) bool { return k == v.ID && k != "" })
	if bad {
		return errRelayRetirement
	}
	for _, rule := range rules {
		for _, seg := range rule.Segments {
			if seg.AgentID == id {
				return errRelayRetirement
			}
		}
	}
	for _, route := range routes {
		for _, agentID := range relayRouteAgents(route) {
			if agentID == id {
				return errRelayRetirement
			}
		}
	}
	if !restoreAgentNeedsRecovery(s, agent) {
		DeleteDoc(s, "relay_agents", id)
		return nil
	}
	var control RelayV2Control
	if !backupPauseDecode(s.Docs["relay_v2_control"]["default"], &control) || len(s.Docs["relay_v2_control"]) != 1 || control.RecoveryRequired || !relayV2Identifier(control.Epoch) || agent.ProtocolVersion != 2 || agent.OfflinePolicy != "keep_last" || !agent.KeepLastConfirmed || !relayRetirementReconciled(agent.ReconcileState) || !relayV2Capabilities(agent.Capabilities) {
		return errRelayRetirement
	}
	catalog, commands, history, recovery, err := relayRetirementEvidence(s, id, now)
	if err != nil || catalog.ControlEpoch != control.Epoch || catalog.InstanceID != agent.BootID {
		return errRelayRetirement
	}
	agent.TokenHash, agent.EnrollmentHash, agent.EnrollmentExpires = "", "", 0
	agent.Enabled, agent.Online = false, false
	record := relayAgentRetirement{Version: 1, Agent: agent, RetiredAt: now, Catalog: catalog, CommandsDigest: commands, HistoryDigest: history, RecoveryDigest: recovery}
	if agent.AccountingDegraded {
		if !relayDegradedReviewValid(s, agent, now) {
			return errRelayRetirement
		}
		record.DegradedReviewDigest = recoveryDigest(s.Docs[relayDegradedReviewCollection][id])
	}
	if err := SaveDoc(s, relayRetirementCollection, id, record); err != nil {
		return err
	}
	DeleteDoc(s, "relay_agents", id)
	return commerceAudit(s, "system", "relay.agent.terminal_retirement", id)
}

func relayRetirementNow(s *State) (map[string]relayAgentRetirement, error) {
	return validateRelayRetirements(s, time.Now().UnixMilli())
}
