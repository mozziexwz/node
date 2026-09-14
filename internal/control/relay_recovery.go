package control

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"sort"

	"github.com/mozziexwz/node/internal/relayruntime"
)

// Recovery is deliberately a root-local, two-sided operation. Ordinary sync
// never grants itself permission to adopt a restored catalogue or rotate its
// credential. No financial balances are changed by these operations.
type RelayRecoveryDifference struct {
	RuleID             string `json:"ruleId"`
	Difference         string `json:"difference"`
	CanAdopt           bool   `json:"canAdopt"`
	NodeGeneration     int64  `json:"nodeGeneration"`
	DatabaseGeneration int64  `json:"databaseGeneration"`
}
type RelayRecoveryReport struct {
	AgentID           string                    `json:"agentId"`
	RecoveryID        string                    `json:"recoveryId"`
	Fingerprint       string                    `json:"fingerprint"`
	Differences       []RelayRecoveryDifference `json:"differences"`
	MissingOnNode     []string                  `json:"missingOnNode"`
	HistoryIncomplete bool                      `json:"historyIncomplete"`
	Message           string                    `json:"message"`
}
type RelayRecoverySelection struct {
	RuleID string `json:"ruleId"`
	Action string `json:"action"`
}
type RelayRecoveryRequest struct {
	Snapshot     relayruntime.V2RecoverySnapshot `json:"snapshot"`
	Fingerprint  string                          `json:"fingerprint"`
	Decisions    []RelayRecoverySelection        `json:"decisions"`
	Confirmation string                          `json:"confirmation"`
	Review       *RelayRecoveryReport            `json:"review,omitempty"`
}
type relayRecoveryPrepared struct {
	AgentID      string               `json:"agentId"`
	RecoveryID   string               `json:"recoveryId"`
	PlanID       string               `json:"planId"`
	PlanSequence int64                `json:"planSequence"`
	RequestHash  string               `json:"requestHash"`
	SealedPlan   string               `json:"sealedPlan"`
	Expected     []relayruntime.V2Ack `json:"expected"`
	AllDecided   bool                 `json:"allDecided"`
	VerifiedAt   int64                `json:"verifiedAt"`
	InstanceID   string               `json:"instanceId,omitempty"`
	Sequence     int64                `json:"sequence,omitempty"`
	PreparedAt   int64                `json:"preparedAt"`
	FinalizedAt  int64                `json:"finalizedAt,omitempty"`
}

var errRelayRecovery = errors.New("恢复证据不完整、已变化或未被明确确认；保持冻结，不覆盖节点或财务数据")

func recoveryDigest(value any) string {
	raw, _ := json.Marshal(value)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func validateRelayRecoverySnapshot(snapshot relayruntime.V2RecoverySnapshot) error {
	emptyBootstrap := snapshot.ControlEpoch == "" && len(snapshot.Records) == 0 && snapshot.Revision == 0
	if snapshot.ProtocolVersion != relayruntime.ProtocolV2 || !relayV2Identifier(snapshot.AgentID) || !relayV2Identifier(snapshot.RecoveryID) || !relayV2Identifier(snapshot.ControlEpoch) && !emptyBootstrap || snapshot.Revision < 0 || snapshot.PlanSequence < 0 || snapshot.PlanSequence == math.MaxInt64 || snapshot.CapturedAt <= 0 || snapshot.Records == nil || len(snapshot.Records) > 4096 || len(snapshot.Traffic) > 8192 || !relayRecoveryOrigin(snapshot.ServerURL) {
		return errRelayRecovery
	}
	seen := map[string]bool{}
	for _, row := range snapshot.Records {
		if !relayV2Identifier(row.RuleID) || row.Command.RuleID != row.RuleID || !relayV2Identifier(row.Command.CommandID) || row.Command.Generation < 1 || row.Command.Generation == math.MaxInt64 || seen[row.RuleID] {
			return errRelayRecovery
		}
		seen[row.RuleID] = true
		switch row.Command.Action {
		case "upsert", "resume", "pause", "revoke":
		default:
			return errRelayRecovery
		}
		switch row.State {
		case "persisted", "ready", "stopping", "stopped", "failed":
		default:
			return errRelayRecovery
		}
		if row.Command.RuntimeHash != "" && !backupPauseValidToken(row.Command.RuntimeHash) || row.RuntimeHash != "" && !backupPauseValidToken(row.RuntimeHash) {
			return errRelayRecovery
		}
		if row.Command.Rule != nil && row.Command.Rule.TLSPrivateKey != "" {
			return errRelayRecovery
		}
		if row.LastApplied != nil && (row.LastApplied.RuleID != row.RuleID || !relayV2Identifier(row.LastApplied.CommandID) || row.LastApplied.Generation < 1 || row.LastApplied.Generation > row.Command.Generation || row.LastApplied.Rule != nil && row.LastApplied.Rule.TLSPrivateKey != "") {
			return errRelayRecovery
		}
	}
	return nil
}

func recoveryCommandDigest(command relayruntime.V2Command) string {
	if command.Rule != nil {
		rule := *command.Rule
		rule.LeaseUntil, rule.TLSPrivateKey = 0, ""
		command.Rule = &rule
	}
	return recoveryDigest(command)
}

func (a *App) recoveryWireCommand(row RelayV2Command) (relayruntime.V2Command, error) {
	out := relayruntime.V2Command{CommandID: row.CommandID, RuleID: row.RuleID, Generation: row.Generation, Action: row.Action, RuntimeHash: row.RuntimeHash, BillingPeriodID: row.BillingPeriodID, Reason: row.Reason}
	if row.Action == "upsert" {
		plain, err := a.Open(row.SealedRule)
		if err != nil {
			return out, errRelayRecovery
		}
		var rule relayruntime.Rule
		if !backupPauseDecode(plain, &rule) {
			return out, errRelayRecovery
		}
		hash, err := relayruntime.RuntimeHash(rule)
		if err != nil || hash != row.RuntimeHash || rule.ID != row.RuleID {
			return out, errRelayRecovery
		}
		out.Rule = &rule
	}
	return out, nil
}

func (a *App) relayRecoveryReport(s *State, snapshot relayruntime.V2RecoverySnapshot) (RelayRecoveryReport, error) {
	report := RelayRecoveryReport{AgentID: snapshot.AgentID, RecoveryID: snapshot.RecoveryID, Fingerprint: recoveryDigest(snapshot), Differences: []RelayRecoveryDifference{}, MissingOnNode: []string{}, HistoryIncomplete: true, Message: "仅比较原节点本机快照与当前数据库。已确认后被节点清除的旧用量不可据此重建；必须核对外部账本。相同配置可接管，差异只能保留冻结或明确停止，不会恢复营业。"}
	if err := validateRelayRecoverySnapshot(snapshot); err != nil {
		return report, err
	}
	var agent RelayAgent
	if !backupPauseDecode(s.Docs["relay_agents"][snapshot.AgentID], &agent) || agent.ID != snapshot.AgentID || agent.Capability != "relay" {
		return report, errRelayRecovery
	}
	seen := map[string]bool{}
	for _, row := range snapshot.Records {
		seen[row.RuleID] = true
		diff := RelayRecoveryDifference{RuleID: row.RuleID, Difference: "node_only_or_unknown", NodeGeneration: row.Command.Generation}
		var current RelayV2Command
		if raw, exists := s.Docs["relay_v2_commands"][snapshot.AgentID+":"+row.RuleID]; exists {
			if !backupPauseDecode(raw, &current) || !backupPauseCommandValid(current) || current.AgentID != snapshot.AgentID || current.RuleID != row.RuleID {
				return report, errRelayRecovery
			}
			diff.DatabaseGeneration = current.Generation
			diff.Difference = "configuration_or_generation_changed"
			wire, err := a.recoveryWireCommand(current)
			if err != nil {
				return report, err
			}
			ref := relayruntime.RecoveryCommandRef(wire)
			if ref == relayruntime.RecoveryCommandRef(row.Command) && recoveryCommandDigest(wire) == recoveryCommandDigest(row.Command) {
				if current.Action == "upsert" && row.LastApplied != nil && recoveryCommandDigest(*row.LastApplied) == recoveryCommandDigest(wire) && row.Running && row.State == "ready" && row.RuntimeHash == current.RuntimeHash {
					diff.CanAdopt, diff.Difference = true, "same_applied_configuration"
				} else if (current.Action == "revoke" || current.Action == "pause") && !row.Running && row.State == "stopped" {
					diff.CanAdopt, diff.Difference = true, "same_stopped_tombstone"
				}
			}
			// Preserve the strongest explicit revoke. It can never become an upsert.
			if current.Action == "revoke" && row.Command.Action != "revoke" {
				diff.CanAdopt, diff.Difference = false, "database_revoked_node_still_retained"
			}
		}
		if row.Command.Action == "revoke" && !diff.CanAdopt {
			diff.Difference = "node_revoked_never_resurrect"
		}
		report.Differences = append(report.Differences, diff)
	}
	for key, raw := range s.Docs["relay_v2_commands"] {
		var command RelayV2Command
		if !backupPauseDecode(raw, &command) || key != command.AgentID+":"+command.RuleID {
			return report, errRelayRecovery
		}
		if command.AgentID == snapshot.AgentID && !seen[command.RuleID] {
			report.MissingOnNode = append(report.MissingOnNode, command.RuleID)
		}
	}
	sort.Slice(report.Differences, func(i, j int) bool { return report.Differences[i].RuleID < report.Differences[j].RuleID })
	sort.Strings(report.MissingOnNode)
	return report, nil
}

func (a *App) prepareRelayRecovery(input RelayRecoveryRequest, now int64) (plan relayruntime.V2RecoveryPlan, err error) {
	if now <= 0 || input.Confirmation != "OLD_CONTROL_ISOLATED "+input.Snapshot.RecoveryID || input.Fingerprint != recoveryDigest(input.Snapshot) || !relayRecoveryOrigin(a.Config.PublicURL) {
		return plan, errRelayRecovery
	}
	err = a.Store.Update(func(s *State) error {
		if !boolSetting(s, "maintenance") || backupOperationPending(s) {
			return errRelayRecovery
		}
		// Stable retry returns the same encrypted plan/token after an uncertain
		// output or commit, rather than silently revoking a plan already delivered.
		canonicalInput := input
		canonicalInput.Review = nil // display-only, never an authorization input
		requestHash := recoveryDigest(canonicalInput)
		var prior relayRecoveryPrepared
		if raw, ok := s.Docs["relay_v2_recovery_prepared"][input.Snapshot.AgentID]; ok {
			if !backupPauseDecode(raw, &prior) || prior.AgentID != input.Snapshot.AgentID {
				return errRelayRecovery
			}
			if prior.RequestHash == requestHash && prior.FinalizedAt == 0 {
				raw, err := a.Open(prior.SealedPlan)
				if err != nil || !backupPauseDecode(raw, &plan) {
					return errRelayRecovery
				}
				currentControl, _ := LoadDoc[RelayV2Control](s, "relay_v2_control", "default")
				currentAgent, _ := LoadDoc[RelayAgent](s, "relay_agents", plan.AgentID)
				if plan.ControlEpoch != currentControl.Epoch || plan.ServerURL != a.Config.PublicURL || commerceHash(plan.Token) != currentAgent.TokenHash {
					return errRelayRecovery
				}
				return nil
			}
			if input.Snapshot.RecoveryID == prior.RecoveryID && input.Snapshot.PlanSequence != prior.PlanSequence {
				return errRelayRecovery
			}
		}
		report, err := a.relayRecoveryReport(s, input.Snapshot)
		if err != nil {
			return err
		}
		if len(report.MissingOnNode) != 0 || len(input.Decisions) != len(report.Differences) {
			return errRelayRecovery
		}
		decisions := map[string]string{}
		for _, d := range input.Decisions {
			if _, ok := decisions[d.RuleID]; ok || d.Action != "adopt" && d.Action != "stop" && d.Action != "hold" {
				return errRelayRecovery
			}
			decisions[d.RuleID] = d.Action
		}
		control, ok := LoadDoc[RelayV2Control](s, "relay_v2_control", "default")
		if !ok && input.Snapshot.ControlEpoch == "" && len(input.Snapshot.Records) == 0 {
			control = RelayV2Control{Epoch: commerceID()}
		} else if !ok || !relayV2Identifier(control.Epoch) {
			return errRelayRecovery
		}
		agent, _ := LoadDoc[RelayAgent](s, "relay_agents", input.Snapshot.AgentID)
		catalog, _ := LoadDoc[RelayV2Catalog](s, "relay_v2_catalogs", agent.ID)
		revision := max(catalog.Revision, input.Snapshot.Revision)
		if revision >= math.MaxInt64-int64(len(input.Decisions))-1 {
			return errRelayRecovery
		}
		plan = relayruntime.V2RecoveryPlan{ProtocolVersion: 2, RecoveryID: input.Snapshot.RecoveryID, PlanID: commerceID(), PlanSequence: input.Snapshot.PlanSequence + 1, AgentID: agent.ID, ExpectedServerURL: input.Snapshot.ServerURL, ExpectedControlEpoch: input.Snapshot.ControlEpoch, ServerURL: a.Config.PublicURL, ControlEpoch: control.Epoch, Token: commerceID() + commerceID(), Decisions: []relayruntime.V2RecoveryDecision{}}
		prepared := relayRecoveryPrepared{AgentID: agent.ID, RecoveryID: plan.RecoveryID, PlanID: plan.PlanID, PlanSequence: plan.PlanSequence, RequestHash: requestHash, Expected: []relayruntime.V2Ack{}, AllDecided: true, PreparedAt: now}
		admissible := map[string]bool{}
		for _, d := range report.Differences {
			admissible[d.RuleID] = d.CanAdopt
		}
		for _, row := range input.Snapshot.Records {
			action, exists := decisions[row.RuleID]
			if !exists {
				return errRelayRecovery
			}
			d := relayruntime.V2RecoveryDecision{RuleID: row.RuleID, Action: action, ExpectedCommand: relayruntime.RecoveryCommandRef(row.Command), ExpectedRuntimeHash: row.RuntimeHash}
			if row.LastApplied != nil {
				v := relayruntime.RecoveryCommandRef(*row.LastApplied)
				d.ExpectedLastApplied = &v
			}
			key := agent.ID + ":" + row.RuleID
			command, _ := LoadDoc[RelayV2Command](s, "relay_v2_commands", key)
			switch action {
			case "hold":
				prepared.AllDecided = false
			case "adopt":
				if !admissible[row.RuleID] || input.Snapshot.AccountingDegraded {
					return errRelayRecovery
				}
				wire, e := a.recoveryWireCommand(command)
				if e != nil {
					return e
				}
				d.Command = &wire
				if command.Action != "upsert" {
					d.Action = "stop" // retain an already-confirmed terminal tombstone
				}
			case "stop":
				// A stop carries no replacement configuration and is bound to this exact
				// node's original record. Existing tombstones remain terminal.
				// Reuse a byte-for-byte matching, already stopped revoke. For any
				// difference issue a strictly newer revoke; never turn it into pause.
				if command.Action != "revoke" || !admissible[row.RuleID] {
					generation := max(command.Generation, row.Command.Generation)
					if generation == math.MaxInt64 {
						return errRelayRecovery
					}
					command = RelayV2Command{AgentID: agent.ID, CommandID: commerceID(), RuleID: row.RuleID, Generation: generation + 1, Action: "revoke", RuntimeHash: row.Command.RuntimeHash, Reason: "root_recovery_stop"}
				}
				wire, e := a.recoveryWireCommand(command)
				if e != nil {
					return e
				}
				d.Command = &wire
			}
			if d.Command != nil {
				revision++
				command.Revision, command.AckState, command.AppliedAt = revision, "pending", 0
				if err := SaveDoc(s, "relay_v2_commands", key, command); err != nil {
					return err
				}
				history := command
				history.SealedRule = ""
				if err := SaveDoc(s, "relay_v2_command_history", command.CommandID, history); err != nil {
					return err
				}
				state := "stopped"
				if command.Action == "upsert" {
					state = "ready"
				}
				prepared.Expected = append(prepared.Expected, relayruntime.V2Ack{CommandID: command.CommandID, RuleID: row.RuleID, Generation: command.Generation, RuntimeHash: command.RuntimeHash, State: state})
				if err := bindRelayRecoverySegment(s, command); err != nil {
					return err
				}
			}
			plan.Decisions = append(plan.Decisions, d)
		}
		plan.Revision = revision
		raw, err := json.Marshal(plan)
		if err != nil {
			return err
		}
		prepared.SealedPlan, err = a.Seal(raw)
		if err != nil {
			return err
		}
		control.RecoveryRequired = true
		agent.TokenHash, agent.EnrollmentHash, agent.EnrollmentExpires = commerceHash(plan.Token), "", 0
		agent.ReconcileState, agent.KeepLastConfirmed, agent.LastSeen = "recovery_required", false, 0
		catalog = RelayV2Catalog{AgentID: agent.ID, ControlEpoch: control.Epoch, Revision: revision, ReconcileState: "recovery_required"}
		for _, save := range []struct {
			collection, id string
			value          any
		}{{"relay_v2_control", "default", control}, {"relay_agents", agent.ID, agent}, {"relay_v2_catalogs", agent.ID, catalog}, {"relay_v2_recovery_prepared", agent.ID, prepared}, {"relay_v2_recovery_snapshots", agent.ID, input.Snapshot}} {
			if err := SaveDoc(s, save.collection, save.id, save.value); err != nil {
				return err
			}
		}
		return commerceAudit(s, "local-root", "relay.recovery.prepare", plan.PlanID)
	})
	return plan, err
}

func bindRelayRecoverySegment(s *State, command RelayV2Command) error {
	for key, raw := range s.Docs["user_rules"] {
		var rule UserRule
		if !backupPauseDecode(raw, &rule) {
			return errRelayRecovery
		}
		if rule.ID != command.RuleID {
			continue
		}
		for i := range rule.Segments {
			seg := &rule.Segments[i]
			if seg.AgentID != command.AgentID {
				continue
			}
			seg.ProtocolVersion = 2
			seg.LastCommandID, seg.LastCommandAction = command.CommandID, command.Action
			seg.ConfigGeneration, seg.RuntimeHash = command.Generation, command.RuntimeHash
			seg.StopConfirmed = false
			seg.AckState = "pending"
		}
		rule.ReconcileState = "recovery_required"
		return SaveDoc(s, "user_rules", key, rule)
	}
	return nil // an unknown post-snapshot rule stays reserved in the catalogue
}

// Called only after bearer authentication, ahead of ordinary command issuance.
// A root-prepared plan accepts observations but never performs desired-state
// reconciliation, resource cleanup or automatic financial settlement.
func relayRecoverySync(s *State, agent *RelayAgent, in relayruntime.V2SyncRequest, out *relayruntime.V2SyncResponse, now int64) (bool, error) {
	raw, exists := s.Docs["relay_v2_recovery_prepared"][agent.ID]
	if !exists {
		return false, nil
	}
	var prepared relayRecoveryPrepared
	if !backupPauseDecode(raw, &prepared) || prepared.AgentID != agent.ID {
		return true, errRelayRecovery
	}
	if prepared.FinalizedAt > 0 {
		return false, nil
	}
	control, _ := LoadDoc[RelayV2Control](s, "relay_v2_control", "default")
	catalog, _ := LoadDoc[RelayV2Catalog](s, "relay_v2_catalogs", agent.ID)
	out.ControlEpoch, out.Revision, out.Status = control.Epoch, catalog.Revision, "recovery_required"
	matched := prepared.AllDecided && in.ControlEpoch == control.Epoch && catalog.ControlEpoch == control.Epoch && in.AppliedRevision == catalog.Revision && len(in.Acks) == len(prepared.Expected)
	expected := map[string]relayruntime.V2Ack{}
	for _, ack := range prepared.Expected {
		expected[ack.RuleID] = ack
	}
	for _, ack := range in.Acks {
		want, ok := expected[ack.RuleID]
		if !ok || want != ack {
			matched = false
		}
		delete(expected, ack.RuleID)
	}
	if len(expected) != 0 {
		matched = false
	}
	if prepared.InstanceID != "" && prepared.InstanceID != in.AgentInstanceID {
		matched = false
	}
	if _, retired := s.Docs["relay_v2_retired_instances"][agent.ID+":"+in.AgentInstanceID]; retired {
		matched = false
	}
	if !matched {
		prepared.VerifiedAt = 0
		if err := SaveDoc(s, "relay_v2_recovery_prepared", agent.ID, prepared); err != nil {
			return true, err
		}
	}
	if matched && (prepared.InstanceID == "" || in.Sequence > prepared.Sequence) {
		prepared.VerifiedAt, prepared.InstanceID, prepared.Sequence = now, in.AgentInstanceID, in.Sequence
		agent.LastSeen, agent.BootID, agent.ControlStatus = now, in.AgentInstanceID, "online"
		agent.AccountingDegraded = in.AccountingDegraded
		for _, ack := range in.Acks {
			if err := relayV2ApplyAck(s, *agent, ack, now); err != nil {
				return true, err
			}
		}
		// Preserve raw samples for explicit later reconciliation. Do not ACK data
		// that has not been applied to its historical ledger in this transaction.
		if len(in.Traffic) > 0 {
			if err := SaveDoc(s, "relay_v2_recovery_traffic", agent.ID, in.Traffic); err != nil {
				return true, err
			}
		}
		if err := SaveDoc(s, "relay_v2_recovery_prepared", agent.ID, prepared); err != nil {
			return true, err
		}
	}
	if matched {
		out.Status = "ready"
	} // empty commands; local node can trust its new epoch
	if err := SaveDoc(s, "relay_agents", agent.ID, *agent); err != nil {
		return true, err
	}
	return true, nil
}

func (a *App) finalizeRelayRecovery(confirmation string, now int64) error {
	return a.Store.Update(func(s *State) error {
		if err := a.validateRelayRecoveryFinalization(s); err != nil {
			return err
		}
		control, ok := LoadDoc[RelayV2Control](s, "relay_v2_control", "default")
		if !ok || !control.RecoveryRequired || !boolSetting(s, "maintenance") || confirmation != "OLD_CONTROL_ISOLATED_AND_FINANCES_REVIEWED "+control.Epoch {
			return errRelayRecovery
		}
		for _, rule := range ListDocs[UserRule](s, "user_rules") {
			for _, seg := range rule.Segments {
				if seg.ProtocolVersion != 2 {
					return errRelayRecovery
				}
			}
		}
		for _, agent := range ListDocs[RelayAgent](s, "relay_agents") {
			if !restoreAgentNeedsRecovery(s, agent) {
				continue
			}
			var p relayRecoveryPrepared
			if !backupPauseDecode(s.Docs["relay_v2_recovery_prepared"][agent.ID], &p) || !p.AllDecided || p.VerifiedAt <= 0 || p.VerifiedAt > now || now-p.VerifiedAt >= 90_000 {
				return errRelayRecovery
			}
			if agent.AccountingDegraded {
				review, err := relayDegradedTerminalEvidence(s, agent, p, now, true)
				if err != nil {
					return err
				}
				if err := SaveDoc(s, relayDegradedReviewCollection, agent.ID, review); err != nil {
					return err
				}
				if err := commerceAudit(s, "local-root", "relay.accounting.degraded_terminal_review", agent.ID); err != nil {
					return err
				}
			}
			catalog, _ := LoadDoc[RelayV2Catalog](s, "relay_v2_catalogs", agent.ID)
			if catalog.ControlEpoch != control.Epoch {
				return errRelayRecovery
			}
			for _, ack := range p.Expected {
				command, ok := LoadDoc[RelayV2Command](s, "relay_v2_commands", agent.ID+":"+ack.RuleID)
				if !ok || !relayV2AckMatches(command, ack) || command.AckState != ack.State {
					return errRelayRecovery
				}
				if command.Action == "upsert" {
					found := false
					for _, rule := range ListDocs[UserRule](s, "user_rules") {
						if rule.ID != command.RuleID {
							continue
						}
						found = true
						action, _ := relayV2Desired(s, agent, rule, now)
						if action != "upsert" {
							return errRelayRecovery
						}
						for _, seg := range rule.Segments {
							if seg.AgentID == agent.ID && (seg.Runtime.RateMbps != seg.IssuedRateMbps || seg.Runtime.EntitlementVersion != seg.IssuedEntitlementVersion) {
								return errRelayRecovery
							}
						}
					}
					if !found {
						return errRelayRecovery
					}
				}
			}
			p.FinalizedAt = now
			agent.ReconcileState = "in_sync"
			agent.KeepLastConfirmed = true
			catalog.InstanceID, catalog.Sequence, catalog.ReconcileState = p.InstanceID, p.Sequence, "in_sync"
			for _, save := range []struct {
				collection, id string
				value          any
			}{{"relay_v2_recovery_prepared", agent.ID, p}, {"relay_agents", agent.ID, agent}, {"relay_v2_catalogs", agent.ID, catalog}} {
				if err := SaveDoc(s, save.collection, save.id, save.value); err != nil {
					return err
				}
			}
		}
		for key, rule := range s.Docs["user_rules"] {
			var row UserRule
			if !backupPauseDecode(rule, &row) {
				return errRelayRecovery
			}
			row.ReconcileState = ""
			if relayRuleRevoked(row, now) {
				row.State = "revoking"
			}
			if err := SaveDoc(s, "user_rules", key, row); err != nil {
				return err
			}
		}
		control.RecoveryRequired = false
		if err := SaveDoc(s, "relay_v2_control", "default", control); err != nil {
			return err
		}
		return commerceAudit(s, "local-root", "relay.recovery.finalize", control.Epoch)
	})
}

// Lists intended for UI display skip malformed documents. A release gate must
// instead enumerate the complete raw catalogue and prove that nothing was lost.
func (a *App) validateRelayRecoveryFinalization(s *State) error {
	retired, retirementErr := relayRetirementNow(s)
	if retirementErr != nil {
		return retirementErr
	}
	bad := false
	malformed := func(string) { bad = true }
	agents := backupPauseReadDocs[RelayAgent](s, "relay_agents", malformed, func(k string, v RelayAgent) bool { return k == v.ID && relayV2Identifier(k) && v.Capability == "relay" })
	catalogs := backupPauseReadDocs[RelayV2Catalog](s, "relay_v2_catalogs", malformed, func(k string, v RelayV2Catalog) bool { return k == v.AgentID && v.Revision >= 0 && v.Sequence >= 0 })
	commands := backupPauseReadDocs[RelayV2Command](s, "relay_v2_commands", malformed, func(k string, v RelayV2Command) bool {
		return k == v.AgentID+":"+v.RuleID && backupPauseCommandValid(v)
	})
	history := backupPauseReadDocs[RelayV2Command](s, "relay_v2_command_history", malformed, func(k string, v RelayV2Command) bool {
		return k == v.CommandID && backupPauseCommandValid(v)
	})
	prepared := backupPauseReadDocs[relayRecoveryPrepared](s, "relay_v2_recovery_prepared", malformed, func(k string, v relayRecoveryPrepared) bool {
		return k == v.AgentID && v.PlanSequence > 0 && v.PlanID != "" && v.SealedPlan != "" && v.Expected != nil
	})
	rules := backupPauseReadDocs[UserRule](s, "user_rules", malformed, func(k string, v UserRule) bool { return k == v.UserID+":"+v.RouteID && v.ID != "" })
	if bad {
		return errRelayRecovery
	}
	var control RelayV2Control
	if !backupPauseDecode(s.Docs["relay_v2_control"]["default"], &control) || len(s.Docs["relay_v2_control"]) != 1 || !relayV2Identifier(control.Epoch) {
		return errRelayRecovery
	}
	for id := range catalogs {
		_, terminal := retired[id]
		if _, ok := agents[id]; !ok && !terminal {
			return errRelayRecovery
		}
	}
	for id := range prepared {
		_, terminal := retired[id]
		if _, ok := agents[id]; !ok && !terminal {
			return errRelayRecovery
		}
	}
	for _, command := range commands {
		_, terminal := retired[command.AgentID]
		if _, ok := agents[command.AgentID]; !ok && !terminal {
			return errRelayRecovery
		}
		if _, ok := catalogs[command.AgentID]; !ok {
			return errRelayRecovery
		}
		old, ok := history[command.CommandID]
		if !ok || !backupPauseSameIntent(old, command) {
			return errRelayRecovery
		}
	}
	if len(backupPauseHistoryConflicts(commands, history)) != 0 {
		return errRelayRecovery
	}
	for _, rule := range rules {
		running := false
		for _, seg := range rule.Segments {
			if _, ok := agents[seg.AgentID]; !ok {
				return errRelayRecovery
			}
			command, ok := commands[seg.AgentID+":"+rule.ID]
			if !ok || seg.ProtocolVersion != 2 || seg.LastCommandID != command.CommandID || seg.LastCommandAction != command.Action || seg.ConfigGeneration != command.Generation || seg.RuntimeHash != command.RuntimeHash || seg.AppliedGeneration != command.Generation || seg.AckState != command.AckState {
				return errRelayRecovery
			}
			if command.Action == "upsert" {
				running = true
				wire, e := a.recoveryWireCommand(command)
				if e != nil {
					return e
				}
				current := seg.Runtime
				if current.Protocol == "tls" {
					key, e := a.Open(seg.SealedTLSKey)
					if e != nil {
						return errRelayRecovery
					}
					current.TLSPrivateKey = string(key)
				}
				hash, e := relayruntime.RuntimeHash(current)
				if e != nil || hash != wire.RuntimeHash || current.Version != wire.Rule.Version || current.Billing != wire.Rule.Billing || current.EntitlementVersion != wire.Rule.EntitlementVersion {
					return errRelayRecovery
				}
			} else if !seg.StopConfirmed {
				return errRelayRecovery
			}
		}
		if running && !relayRetainedTopologyMatches(s, rule) {
			return errRelayRecovery
		}
	}
	for id, agent := range agents {
		if !restoreAgentNeedsRecovery(s, agent) {
			continue
		}
		p, ok := prepared[id]
		if !ok || !p.AllDecided {
			return errRelayRecovery
		}
		raw, e := a.Open(p.SealedPlan)
		var plan relayruntime.V2RecoveryPlan
		if e != nil || !backupPauseDecode(raw, &plan) || plan.AgentID != id || plan.RecoveryID != p.RecoveryID || plan.PlanID != p.PlanID || plan.PlanSequence != p.PlanSequence || plan.ControlEpoch != control.Epoch || commerceHash(plan.Token) != agent.TokenHash || plan.ServerURL != a.Config.PublicURL {
			return errRelayRecovery
		}
		catalog, ok := catalogs[id]
		if !ok || catalog.ControlEpoch != plan.ControlEpoch || catalog.Revision != plan.Revision {
			return errRelayRecovery
		}
		expected := map[string]relayruntime.V2Ack{}
		for _, ack := range p.Expected {
			if _, dup := expected[ack.RuleID]; dup {
				return errRelayRecovery
			}
			expected[ack.RuleID] = ack
		}
		if len(plan.Decisions) != len(expected) {
			return errRelayRecovery
		}
		decided := map[string]bool{}
		for _, d := range plan.Decisions {
			if decided[d.RuleID] {
				return errRelayRecovery
			}
			decided[d.RuleID] = true
			ack, ok := expected[d.RuleID]
			if !ok || d.Action == "hold" || d.Command == nil || ack.CommandID != d.Command.CommandID || ack.Generation != d.Command.Generation || ack.RuntimeHash != d.Command.RuntimeHash {
				return errRelayRecovery
			}
			want := "stopped"
			if d.Command.Action == "upsert" {
				want = "ready"
			}
			if ack.State != want {
				return errRelayRecovery
			}
		}
		count := 0
		for _, command := range commands {
			if command.AgentID != id {
				continue
			}
			count++
			ack, ok := expected[command.RuleID]
			if !ok || !relayV2AckMatches(command, ack) || command.AckState != ack.State {
				return errRelayRecovery
			}
		}
		if count != len(expected) {
			return errRelayRecovery
		}
	}
	return nil
}
