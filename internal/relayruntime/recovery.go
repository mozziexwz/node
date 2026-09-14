package relayruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"sort"
	"strings"
	"time"
)

type v2RecoveryCheckpoint struct {
	RecoveryID   string `json:"recoveryId"`
	PlanID       string `json:"planId,omitempty"`
	PlanSequence int64  `json:"planSequence"`
	PlanHash     string `json:"planHash,omitempty"`
	HasHolds     bool   `json:"hasHolds"`
}

func validRecoveryOrigin(value string) bool {
	u, err := url.Parse(value)
	return err == nil && u.Host != "" && u.User == nil && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && u.Opaque == "" && u.RawPath == "" && (u.Path == "" || u.Path == "/") && (u.Scheme == "https" || u.Scheme == "http" && (u.Hostname() == "localhost" || net.ParseIP(u.Hostname()) != nil && net.ParseIP(u.Hostname()).IsLoopback()))
}

func validRecoveryToken(token string) bool {
	if len(token) < 1 || len(token) > 4096 {
		return false
	}
	for _, c := range token {
		if c < 33 || c > 126 {
			return false
		}
	}
	return true
}

func validateV2RecoveryCheckpoint(state v2DiskState) error {
	if state.ManagementToken != "" && !validRecoveryToken(state.ManagementToken) {
		return errors.New("invalid persisted management credential")
	}
	r := state.Recovery
	if r == nil {
		return nil
	}
	if !validV2ID(r.RecoveryID) || r.PlanSequence < 0 || r.PlanSequence == 0 && (r.PlanID != "" || r.PlanHash != "") || r.PlanSequence > 0 && (!validV2ID(r.PlanID) || len(r.PlanHash) != 64 || state.ManagementToken == "") {
		return errors.New("invalid trusted recovery checkpoint")
	}
	if r.PlanSequence > 0 {
		if _, err := hex.DecodeString(r.PlanHash); err != nil || strings.ToLower(r.PlanHash) != r.PlanHash {
			return errors.New("invalid trusted recovery plan fingerprint")
		}
	}
	if r.HasHolds && !state.RecoveryRequired {
		return errors.New("unresolved recovery holds cannot appear outside recovery isolation")
	}
	return nil
}

func redactedRecoveryCommand(command V2Command) V2Command {
	if command.Rule != nil {
		rule := *command.Rule
		rule.TLSPrivateKey = ""
		rule.Targets = append([]string(nil), rule.Targets...)
		rule.AllowedSources = append([]string(nil), rule.AllowedSources...)
		rule.TargetTLS = append([]TLSClient(nil), rule.TargetTLS...)
		command.Rule = &rule
	}
	return command
}

func (s *runtimeState) recoverySnapshot() (V2RecoverySnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out V2RecoverySnapshot
	if s.v2 == nil {
		return out, errors.New("protected recovery requires an active keep_last Agent")
	}
	next := s.v2.disk
	if next.Recovery == nil || !next.RecoveryRequired {
		next.Recovery = &v2RecoveryCheckpoint{RecoveryID: randomID()}
	}
	next.RecoveryRequired = true
	if err := writeV2PrivateJSON(s.v2.path, next); err != nil {
		return out, errors.New("cannot persist recovery freeze; no recovery snapshot authorized")
	}
	s.v2.disk = next
	out = V2RecoverySnapshot{ProtocolVersion: ProtocolV2, RecoveryID: next.Recovery.RecoveryID, AgentID: next.AgentID, ServerURL: next.ServerURL, ControlEpoch: next.ControlEpoch, Revision: next.Revision, PlanSequence: next.Recovery.PlanSequence, Records: []V2RecoveryRecord{}, Traffic: []V2Traffic{}, AccountingDegraded: s.v2AccountingDegradedLocked(), HistoryIncomplete: true, CapturedAt: time.Now().UnixMilli()}
	for id, record := range next.Records {
		entry := V2RecoveryRecord{RuleID: id, Command: redactedRecoveryCommand(record.Command), State: record.State}
		if record.LastApplied != nil {
			last := redactedRecoveryCommand(*record.LastApplied)
			entry.LastApplied = &last
		}
		if p := s.processes[id]; !processDone(p) {
			entry.Running, entry.ProcessEpoch = true, p.epoch
			entry.RuntimeHash, _ = RuntimeHash(p.rule)
			if p.stopping {
				entry.State = "stopping"
			} else if p.ack.State != "ready" {
				entry.State = "persisted"
			}
		} else if entry.State == "ready" {
			entry.State = "persisted"
		}
		out.Records = append(out.Records, entry)
	}
	// Include retained unacknowledged samples and current meter cursors. Past
	// ACKed epochs were not retained by older Agents: never invent completeness
	// or charge replacement entitlements from this recovery evidence.
	traffic := map[string]V2Traffic{}
	if s.v2Traffic != nil {
		for epoch, sample := range s.v2Traffic.Pending {
			traffic[epoch] = sample
		}
		for _, meter := range s.v2Traffic.meters {
			if sample := meter.sample; sample.Epoch != "" && sample.Sequence > traffic[sample.Epoch].Sequence {
				traffic[sample.Epoch] = sample
			}
		}
	}
	for _, sample := range traffic {
		out.Traffic = append(out.Traffic, sample)
	}
	sort.Slice(out.Records, func(i, j int) bool { return out.Records[i].RuleID < out.Records[j].RuleID })
	sort.Slice(out.Traffic, func(i, j int) bool { return out.Traffic[i].Epoch < out.Traffic[j].Epoch })
	return out, nil
}

func sameRecoveryRef(expected *V2RecoveryCommandRef, command *V2Command) bool {
	if expected == nil || command == nil {
		return expected == nil && command == nil
	}
	return *expected == RecoveryCommandRef(*command)
}

func (s *runtimeState) applyRecoveryPlan(ctx context.Context, plan V2RecoveryPlan) (V2RecoveryResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out V2RecoveryResult
	if s.v2 == nil || s.v2.disk.Recovery == nil {
		return out, errors.New("take a protected local recovery snapshot before preparing a plan")
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	current := s.v2.disk
	checkpoint := current.Recovery
	raw, err := json.Marshal(plan)
	if err != nil || len(raw) > maxV2StateBytes {
		return out, errors.New("invalid or oversized recovery plan")
	}
	digest := sha256.Sum256(raw)
	planHash := hex.EncodeToString(digest[:])
	result := func() V2RecoveryResult {
		return V2RecoveryResult{RecoveryID: plan.RecoveryID, PlanID: plan.PlanID, PlanSequence: plan.PlanSequence, RecoveryRequired: s.v2.disk.RecoveryRequired, Message: "管理凭据已由本机 root 明确信任；转发未整体重启，仍等待逐规则停止/运行确认及控制面恢复核对"}
	}
	if plan.RecoveryID == checkpoint.RecoveryID && plan.PlanID == checkpoint.PlanID && plan.PlanSequence == checkpoint.PlanSequence && planHash == checkpoint.PlanHash {
		return result(), nil
	}
	if plan.ProtocolVersion != ProtocolV2 || !current.RecoveryRequired || plan.RecoveryID != checkpoint.RecoveryID || !validV2ID(plan.PlanID) || plan.PlanID == checkpoint.PlanID || plan.PlanSequence < 1 || plan.PlanSequence != checkpoint.PlanSequence+1 || plan.AgentID != current.AgentID || strings.TrimRight(plan.ExpectedServerURL, "/") != current.ServerURL || plan.ExpectedControlEpoch != current.ControlEpoch || !validRecoveryOrigin(plan.ServerURL) || !validV2ID(plan.ControlEpoch) || !validRecoveryToken(plan.Token) || len(plan.Token) < 16 || plan.Revision < 0 || len(plan.Decisions) != len(current.Records) {
		return out, errors.New("recovery plan identity, sequence, origin, credential, or complete rule inventory mismatch")
	}
	next := current
	next.Records = make(map[string]v2Record, len(current.Records))
	for id, record := range current.Records {
		next.Records[id] = record
	}
	hasHolds := false
	seen := map[string]bool{}
	for _, decision := range plan.Decisions {
		record, exists := current.Records[decision.RuleID]
		if !exists || seen[decision.RuleID] || decision.ExpectedCommand != RecoveryCommandRef(record.Command) || !sameRecoveryRef(decision.ExpectedLastApplied, record.LastApplied) {
			return out, errors.New("recovery decision does not match the frozen original command and last-applied identities")
		}
		seen[decision.RuleID] = true
		if decision.Action == "hold" {
			if decision.Command != nil {
				return out, errors.New("held recovery rules cannot carry replacement commands")
			}
			hasHolds = true
			continue
		}
		if decision.Command == nil || validateV2Command(*decision.Command) != nil || decision.Command.RuleID != decision.RuleID {
			return out, errors.New("invalid explicit recovery command")
		}
		command := normalizedV2Command(*decision.Command)
		if command.Generation < record.Command.Generation || command.Generation == record.Command.Generation && !sameV2Command(command, record.Command) || command.Generation > record.Command.Generation && command.CommandID == record.Command.CommandID {
			return out, errors.New("recovery cannot roll back or conflict with an existing command generation")
		}
		if decision.Action == "stop" {
			if v2RunningAction(command.Action) || record.Command.Action == "revoke" && command.Action != "revoke" {
				return out, errors.New("explicit recovery stop must preserve terminal revoke semantics")
			}
			next.Records[decision.RuleID] = v2Record{Command: command, State: "persisted", LastApplied: record.LastApplied}
			continue
		}
		if decision.Action != "adopt" || !v2RunningAction(command.Action) || !v2RunningAction(record.Command.Action) || record.LastApplied == nil || record.Command.RuntimeHash != record.LastApplied.RuntimeHash || command.RuntimeHash != record.LastApplied.RuntimeHash || decision.ExpectedRuntimeHash != command.RuntimeHash {
			return out, errors.New("only an unchanged running last-applied configuration can be adopted; differences must be held or explicitly stopped")
		}
		if s.v2AccountingDegradedLocked() {
			return out, errors.New("accounting degraded after snapshot; adoption remains frozen pending explicit accounting review")
		}
		p := s.processes[decision.RuleID]
		if processDone(p) || p.stopping || p.ack.State != "ready" {
			return out, errors.New("same-connection adoption requires the original ready process to remain alive")
		}
		actualHash, err := RuntimeHash(p.rule)
		if err != nil || actualHash != command.RuntimeHash {
			return out, errors.New("actual running configuration differs from the explicit adoption plan")
		}
		copy := command
		next.Records[decision.RuleID] = v2Record{Command: command, State: "ready", LastApplied: &copy}
	}
	commandIDs := map[string]bool{}
	for _, record := range next.Records {
		if commandIDs[record.Command.CommandID] {
			return out, errors.New("recovery plan reused a command identity")
		}
		commandIDs[record.Command.CommandID] = true
	}
	next.ServerURL, next.ControlEpoch, next.Revision = strings.TrimRight(plan.ServerURL, "/"), plan.ControlEpoch, plan.Revision
	next.ManagementToken = plan.Token
	next.RecoveryRequired = true
	next.Recovery = &v2RecoveryCheckpoint{RecoveryID: checkpoint.RecoveryID, PlanID: plan.PlanID, PlanSequence: plan.PlanSequence, PlanHash: planHash, HasHolds: hasHolds}
	// Credentials, trusted epoch, selected commands and idempotency identity
	// share one durable commit. Do not alter any process or live credential
	// before it completes. A retry after an unknown write outcome is safe.
	if err := writeV2PrivateJSON(s.v2.path, next); err != nil {
		return out, errors.New("cannot persist trusted recovery plan; live credentials and forwarding left unchanged")
	}
	s.v2.disk, s.v2.token = next, plan.Token
	s.cfg.ServerURL = next.ServerURL
	s.v2.credentialGeneration++
	for _, decision := range plan.Decisions {
		if decision.Action == "adopt" {
			p := s.processes[decision.RuleID]
			command := next.Records[decision.RuleID].Command
			if p.rule.EntitlementVersion != command.Rule.EntitlementVersion || p.v2BillingPeriod != command.BillingPeriodID {
				s.recordV2TrafficLocked(p, p.traffic.InputBytes, p.traffic.OutputBytes, p.traffic.Connections, time.Now().UnixMilli())
			}
			p.rule, p.v2BillingPeriod, p.ack.Version = *command.Rule, command.BillingPeriodID, command.Rule.Version
		}
	}
	// Frozen reconciliation retains running adopts/holds and executes only
	// explicitly durable pause/revoke decisions, waiting for actual child exit.
	_ = s.reconcileFrozenV2Locked(ctx)
	if s.v2.wake != nil {
		select {
		case s.v2.wake <- struct{}{}:
		default:
		}
	}
	return result(), nil
}

func (s *runtimeState) recoveryLocallyConfirmedLocked() bool {
	for id, record := range s.v2.disk.Records {
		p := s.processes[id]
		if v2RunningAction(record.Command.Action) {
			if record.State != "ready" || record.LastApplied == nil || !sameV2Command(record.Command, *record.LastApplied) || processDone(p) || p.stopping || p.ack.State != "ready" {
				return false
			}
			hash, err := RuntimeHash(p.rule)
			if err != nil || hash != record.Command.RuntimeHash {
				return false
			}
		} else if record.State != "stopped" || !processDone(p) {
			return false
		}
	}
	return true
}
