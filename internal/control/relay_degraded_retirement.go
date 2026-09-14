package control

import "github.com/mozziexwz/node/internal/relayruntime"

const relayDegradedReviewCollection = "relay_v2_degraded_terminal_reviews"

// This root financial review authorizes retirement of an already stopped
// identity, not clearing degraded accounting or restoring a running route.
type relayDegradedTerminalReview struct {
	Version        int    `json:"version"`
	AgentID        string `json:"agentId"`
	InstanceID     string `json:"instanceId"`
	ControlEpoch   string `json:"controlEpoch"`
	PlanID         string `json:"planId"`
	PlanSequence   int64  `json:"planSequence"`
	Revision       int64  `json:"revision"`
	ReviewedAt     int64  `json:"reviewedAt"`
	CommandsDigest string `json:"commandsDigest"`
	HistoryDigest  string `json:"historyDigest"`
	SnapshotDigest string `json:"snapshotDigest"`
}

func relayDegradedTerminalEvidence(s *State, agent RelayAgent, p relayRecoveryPrepared, now int64, fresh bool) (relayDegradedTerminalReview, error) {
	var out relayDegradedTerminalReview
	if !agent.AccountingDegraded || !p.AllDecided || p.AgentID != agent.ID || p.InstanceID != agent.BootID || !relayV2Identifier(p.InstanceID) || p.PlanSequence <= 0 || !relayV2Identifier(p.PlanID) || p.VerifiedAt <= 0 || p.VerifiedAt > now || fresh && now-p.VerifiedAt >= backupPauseFreshnessMS || len(p.Expected) == 0 {
		return out, errRelayRetirement
	}
	var catalog RelayV2Catalog
	if !backupPauseDecode(s.Docs["relay_v2_catalogs"][agent.ID], &catalog) || catalog.AgentID != agent.ID || !relayV2Identifier(catalog.ControlEpoch) {
		return out, errRelayRetirement
	}
	var snapshot relayruntime.V2RecoverySnapshot
	if !backupPauseDecode(s.Docs["relay_v2_recovery_snapshots"][agent.ID], &snapshot) || snapshot.AgentID != agent.ID || snapshot.RecoveryID != p.RecoveryID || validateRelayRecoverySnapshot(snapshot) != nil {
		return out, errRelayRetirement
	}
	expected := map[string]relayruntime.V2Ack{}
	for _, ack := range p.Expected {
		if _, dup := expected[ack.RuleID]; dup || ack.State != "stopped" {
			return out, errRelayRetirement
		}
		expected[ack.RuleID] = ack
	}
	bad := false
	malformed := func(string) { bad = true }
	allCommands := backupPauseReadDocs[RelayV2Command](s, "relay_v2_commands", malformed, func(k string, c RelayV2Command) bool {
		return k == c.AgentID+":"+c.RuleID && backupPauseCommandValid(c)
	})
	allHistory := backupPauseReadDocs[RelayV2Command](s, "relay_v2_command_history", malformed, func(k string, c RelayV2Command) bool { return k == c.CommandID && backupPauseCommandValid(c) })
	if bad {
		return out, errRelayRetirement
	}
	commands, history := map[string]RelayV2Command{}, map[string]RelayV2Command{}
	for key, command := range allCommands {
		if command.AgentID != agent.ID {
			continue
		}
		ack, exists := expected[command.RuleID]
		old, found := allHistory[command.CommandID]
		if !exists || !found || !backupPauseSameIntent(old, command) || !relayV2AckMatches(command, ack) || command.Action != "revoke" || command.AckState != "stopped" || command.AppliedAt < p.VerifiedAt || command.AppliedAt > now || command.Revision > catalog.Revision || fresh && now-command.AppliedAt >= backupPauseFreshnessMS {
			return out, errRelayRetirement
		}
		// Repeating the SAME irreversible stopped ACK advances only observation
		// time. It must not invalidate root's reviewed intent/ledger boundary.
		command.AppliedAt = 0
		commands[key] = command
	}
	if len(commands) != len(expected) {
		return out, errRelayRetirement
	}
	for key, old := range allHistory {
		if old.AgentID == agent.ID {
			history[key] = old
		}
	}
	if len(backupPauseHistoryConflicts(commands, history)) != 0 {
		return out, errRelayRetirement
	}
	out = relayDegradedTerminalReview{Version: 1, AgentID: agent.ID, InstanceID: p.InstanceID, ControlEpoch: catalog.ControlEpoch, PlanID: p.PlanID, PlanSequence: p.PlanSequence, Revision: catalog.Revision, ReviewedAt: now, CommandsDigest: recoveryDigest(commands), HistoryDigest: recoveryDigest(history), SnapshotDigest: recoveryDigest(snapshot)}
	return out, nil
}

func relayDegradedReviewValid(s *State, agent RelayAgent, now int64) bool {
	var review relayDegradedTerminalReview
	var prepared relayRecoveryPrepared
	if !backupPauseDecode(s.Docs[relayDegradedReviewCollection][agent.ID], &review) || review.Version != 1 || review.AgentID != agent.ID || review.ReviewedAt <= 0 || review.ReviewedAt > now || !backupPauseDecode(s.Docs["relay_v2_recovery_prepared"][agent.ID], &prepared) || prepared.FinalizedAt != review.ReviewedAt {
		return false
	}
	current, err := relayDegradedTerminalEvidence(s, agent, prepared, now, false)
	if err != nil {
		return false
	}
	current.ReviewedAt = review.ReviewedAt
	return current == review
}
