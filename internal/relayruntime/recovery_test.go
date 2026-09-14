package relayruntime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func recoveryTestPlan(s *runtimeState, snapshot V2RecoverySnapshot) V2RecoveryPlan {
	plan := V2RecoveryPlan{ProtocolVersion: ProtocolV2, RecoveryID: snapshot.RecoveryID, PlanID: randomID(), PlanSequence: snapshot.PlanSequence + 1, AgentID: snapshot.AgentID, ExpectedServerURL: snapshot.ServerURL, ExpectedControlEpoch: snapshot.ControlEpoch, ServerURL: "https://recovered.example.test", ControlEpoch: "root-explicit-recovery-epoch", Token: "root-private-new-management-credential", Revision: snapshot.Revision, Decisions: []V2RecoveryDecision{}}
	for _, entry := range snapshot.Records {
		record := s.v2.disk.Records[entry.RuleID]
		command := record.Command
		decision := V2RecoveryDecision{RuleID: entry.RuleID, Action: "adopt", ExpectedCommand: RecoveryCommandRef(record.Command), ExpectedRuntimeHash: entry.RuntimeHash, Command: &command}
		if record.LastApplied != nil {
			ref := RecoveryCommandRef(*record.LastApplied)
			decision.ExpectedLastApplied = &ref
		}
		if !v2RunningAction(command.Action) {
			decision.Action = "stop"
		}
		plan.Decisions = append(plan.Decisions, decision)
	}
	return plan
}

func cloneRecoveryPlan(plan V2RecoveryPlan) V2RecoveryPlan {
	raw, _ := json.Marshal(plan)
	var copy V2RecoveryPlan
	_ = json.Unmarshal(raw, &copy)
	return copy
}

func recoveryReadyResponse(request V2SyncRequest) V2SyncResponse {
	return V2SyncResponse{ProtocolVersion: ProtocolV2, AgentID: request.AgentID, RequestID: request.RequestID, ControlEpoch: request.ControlEpoch, PreviousRevision: request.AppliedRevision, Revision: request.AppliedRevision, Status: "ready", OfflinePolicy: KeepLast, Commands: []V2Command{}, TrafficAcks: []V2TrafficAck{}}
}

func TestV2RecoveryAdoptionPreservesOriginalTCPAndRejectsOldAuthority(t *testing.T) {
	s, ctx, target := v2TestFixture(t)
	command := v2TestCommand(t, "recovery-live-rule-one", target)
	command.Rule.TLSPrivateKey = "test-private-key-never-exported"
	request, response := v2TestResponse(s, []V2Command{command})
	if err := s.applyV2Response(ctx, request, response); err != nil {
		t.Fatal(err)
	}
	v2WaitState(t, s, ctx, command.RuleID, "ready")
	conn := v2Connect(t, command)
	v2Echo(t, conn, 1)
	before := s.processes[command.RuleID]
	oldRequest := s.v2Request("original-live-instance")
	snapshot, err := s.recoverySnapshot()
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.recoverySnapshot()
	if err != nil || snapshot.RecoveryID != again.RecoveryID || !snapshot.HistoryIncomplete {
		t.Fatalf("snapshot retry or accounting honesty failed: %v", err)
	}
	raw, _ := json.Marshal(snapshot)
	if strings.Contains(string(raw), "test-private-key-never-exported") || snapshot.Records[0].Command.Rule.TLSPrivateKey != "" {
		t.Fatal("snapshot leaked TLS private material")
	}
	plan := recoveryTestPlan(s, snapshot)
	if _, err := s.applyRecoveryPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if s.processes[command.RuleID] != before || s.v2.token != plan.Token || !s.v2.disk.RecoveryRequired {
		t.Fatal("adoption restarted child or prematurely unfroze recovery")
	}
	v2Echo(t, conn, 2)
	if err := s.applyV2Response(ctx, oldRequest, recoveryReadyResponse(oldRequest)); err == nil {
		t.Fatal("in-flight old authority response survived hot credential change")
	}
	if s.v2.disk.Recovery.HasHolds {
		t.Fatal("stale old exchange poisoned new trusted recovery")
	}
	if _, err := s.applyRecoveryPlan(ctx, plan); err != nil {
		t.Fatalf("identical recovery retry was not idempotent: %v", err)
	}
	changed := cloneRecoveryPlan(plan)
	changed.Token = "same-id-but-different-private-credential"
	if _, err := s.applyRecoveryPlan(ctx, changed); err == nil {
		t.Fatal("same plan identity accepted changed contents")
	}
	request = s.v2Request("original-live-instance")
	if err := s.applyV2Response(ctx, request, recoveryReadyResponse(request)); err != nil {
		t.Fatal(err)
	}
	if s.v2.disk.RecoveryRequired {
		t.Fatal("matching trusted empty catalogue confirmation did not complete local recovery")
	}
	v2Echo(t, conn, 3)
	if _, err := s.applyRecoveryPlan(ctx, plan); err != nil {
		t.Fatalf("lost result retry after confirmation was not idempotent: %v", err)
	}
	cfg := s.cfg
	loaded, err := loadV2State(cfg, snapshot.AgentID)
	if err != nil || loaded.ManagementToken != plan.Token || loaded.ControlEpoch != plan.ControlEpoch || loaded.Recovery.PlanID != plan.PlanID {
		t.Fatalf("trusted credential/epoch/idempotency checkpoint was not atomic and durable: %v", err)
	}
	request = s.v2Request("original-live-instance")
	wrongEpoch := recoveryReadyResponse(request)
	wrongEpoch.ControlEpoch = "another-untrusted-disaster-epoch"
	if err := s.applyV2Response(ctx, request, wrongEpoch); err == nil {
		t.Fatal("unsolicited epoch change accepted")
	}
	if s.v2.disk.Recovery != nil {
		t.Fatal("completed old root plan remained authority for a future incident")
	}
	request = s.v2Request("original-live-instance")
	if err := s.applyV2Response(ctx, request, recoveryReadyResponse(request)); err == nil {
		t.Fatal("a later incident automatically reused old root authorization")
	}
	v2Echo(t, conn, 4)
}

func TestV2RecoveryInvalidPlansAndDiskFailureDoNotMutateLiveRules(t *testing.T) {
	s, ctx, target := v2TestFixture(t)
	command := v2TestCommand(t, "recovery-reject-rule-one", target)
	request, response := v2TestResponse(s, []V2Command{command})
	if err := s.applyV2Response(ctx, request, response); err != nil {
		t.Fatal(err)
	}
	v2WaitState(t, s, ctx, command.RuleID, "ready")
	conn := v2Connect(t, command)
	snapshot, err := s.recoverySnapshot()
	if err != nil {
		t.Fatal(err)
	}
	plan := recoveryTestPlan(s, snapshot)
	before := s.processes[command.RuleID]
	mutations := map[string]func(*V2RecoveryPlan){
		"agent identity":       func(p *V2RecoveryPlan) { p.AgentID = "different-agent" },
		"untrusted origin":     func(p *V2RecoveryPlan) { p.ServerURL = "http://public.example.test" },
		"origin path":          func(p *V2RecoveryPlan) { p.ServerURL += "/unexpected" },
		"header injection":     func(p *V2RecoveryPlan) { p.Token += "\r\nInjected: token" },
		"missing decision":     func(p *V2RecoveryPlan) { p.Decisions = nil },
		"old generation ref":   func(p *V2RecoveryPlan) { p.Decisions[0].ExpectedCommand.Generation++ },
		"missing last applied": func(p *V2RecoveryPlan) { p.Decisions[0].ExpectedLastApplied = nil },
		"different real config": func(p *V2RecoveryPlan) {
			p.Decisions[0].Command.Rule.RateMbps++
			p.Decisions[0].Command.RuntimeHash, _ = RuntimeHash(*p.Decisions[0].Command.Rule)
			p.Decisions[0].Command.Generation++
			p.Decisions[0].Command.CommandID = randomID()
		},
		"rollback generation": func(p *V2RecoveryPlan) { p.Decisions[0].Command.Generation = 0 },
		"forged running hash": func(p *V2RecoveryPlan) { p.Decisions[0].ExpectedRuntimeHash = strings.Repeat("a", 64) },
		"hold with command":   func(p *V2RecoveryPlan) { p.Decisions[0].Action = "hold" },
		"invalid sequence":    func(p *V2RecoveryPlan) { p.PlanSequence++ },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			bad := cloneRecoveryPlan(plan)
			mutate(&bad)
			if _, err := s.applyRecoveryPlan(ctx, bad); err == nil {
				t.Fatal("bad recovery plan accepted")
			}
			if s.processes[command.RuleID] != before || s.v2.disk.ControlEpoch != snapshot.ControlEpoch || s.v2.disk.Recovery.PlanSequence != 0 {
				t.Fatal("rejected plan changed live process/identity")
			}
			v2Echo(t, conn, 20)
		})
	}
	statePath := s.v2.path
	s.v2.path = filepath.Join(s.cfg.StateDir, "nonexistent", v2StateFile)
	if _, err := s.applyRecoveryPlan(ctx, plan); err == nil {
		t.Fatal("plan proceeded without durable intent")
	}
	s.v2.path = statePath
	if s.v2.disk.ControlEpoch != snapshot.ControlEpoch || s.processes[command.RuleID] != before {
		t.Fatal("failed persistence changed active authority or process")
	}
	v2Echo(t, conn, 21)
}

func TestV2RecoveryHoldAndExplicitStopRetainIsolationAndTombstones(t *testing.T) {
	s, ctx, target := v2TestFixture(t)
	first := v2TestCommand(t, "recovery-held-rule-one", target)
	second := v2TestCommand(t, "recovery-stop-rule-two", target)
	request, response := v2TestResponse(s, []V2Command{first, second})
	if err := s.applyV2Response(ctx, request, response); err != nil {
		t.Fatal(err)
	}
	v2WaitState(t, s, ctx, first.RuleID, "ready")
	v2WaitState(t, s, ctx, second.RuleID, "ready")
	conn := v2Connect(t, first)
	stopped := s.processes[second.RuleID]
	snapshot, err := s.recoverySnapshot()
	if err != nil {
		t.Fatal(err)
	}
	plan := recoveryTestPlan(s, snapshot)
	for i := range plan.Decisions {
		d := &plan.Decisions[i]
		if d.RuleID == first.RuleID {
			d.Action, d.Command = "hold", nil
		} else {
			d.Action = "stop"
			d.Command = &V2Command{CommandID: randomID(), RuleID: d.RuleID, Generation: d.ExpectedCommand.Generation + 1, Action: "revoke"}
		}
	}
	if _, err := s.applyRecoveryPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	v2WaitState(t, s, ctx, second.RuleID, "stopped")
	if !processDone(stopped) || !s.v2.disk.Recovery.HasHolds {
		t.Fatal("stop did not await process exit or hold was lost")
	}
	request = s.v2Request("same-instance")
	if err := s.applyV2Response(ctx, request, recoveryReadyResponse(request)); err == nil {
		t.Fatal("empty control response cleared a held recovery rule")
	}
	v2Echo(t, conn, 30)
	snapshot, err = s.recoverySnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.PlanSequence != plan.PlanSequence || snapshot.RecoveryID != plan.RecoveryID {
		t.Fatal("partial recovery snapshot lost original retry identity")
	}
	finish := recoveryTestPlan(s, snapshot)
	if _, err := s.applyRecoveryPlan(ctx, finish); err != nil {
		t.Fatal(err)
	}
	v2WaitState(t, s, ctx, second.RuleID, "stopped")
	request = s.v2Request("same-instance")
	if err := s.applyV2Response(ctx, request, recoveryReadyResponse(request)); err != nil {
		t.Fatal(err)
	}
	if s.v2.disk.RecoveryRequired || s.v2.disk.Records[second.RuleID].Command.Action != "revoke" {
		t.Fatal("confirmed recovery lost terminal tombstone")
	}
	v2Echo(t, conn, 31)
	// A fresh root recovery still cannot turn a tombstone back into a listener.
	snapshot, err = s.recoverySnapshot()
	if err != nil {
		t.Fatal(err)
	}
	resurrect := recoveryTestPlan(s, snapshot)
	for i := range resurrect.Decisions {
		if resurrect.Decisions[i].RuleID == second.RuleID {
			command := second
			command.Generation += 10
			command.CommandID = randomID()
			resurrect.Decisions[i].Action, resurrect.Decisions[i].Command = "adopt", &command
		}
	}
	if _, err := s.applyRecoveryPlan(ctx, resurrect); err == nil {
		t.Fatal("root recovery resurrected terminal revoked rule ID")
	}
}

func TestV2RecoveryEmptyInventoryAndSnapshotPersistenceFailure(t *testing.T) {
	s, ctx, _ := v2TestFixture(t)
	statePath := s.v2.path
	s.v2.path = filepath.Join(s.cfg.StateDir, "missing", v2StateFile)
	if _, err := s.recoverySnapshot(); err == nil {
		t.Fatal("snapshot authorized without durable freeze")
	}
	if s.v2.disk.RecoveryRequired {
		t.Fatal("failed initial snapshot changed in-memory authority")
	}
	s.v2.path = statePath
	snapshot, err := s.recoverySnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loadV2State(s.cfg, s.v2.disk.AgentID); err != nil {
		t.Fatalf("empty unbootstrapped frozen state did not reload: %v", err)
	}
	plan := recoveryTestPlan(s, snapshot)
	if _, err := s.applyRecoveryPlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	request := s.v2Request("empty-instance")
	if err := s.applyV2Response(ctx, request, recoveryReadyResponse(request)); err != nil {
		t.Fatal(err)
	}
	if s.v2.disk.RecoveryRequired {
		t.Fatal("empty explicitly trusted inventory could not complete recovery")
	}
	if _, err := os.Stat(s.v2.path); err != nil {
		t.Fatal(err)
	}
}

func TestV2RecoveryClientNotAvailableOutsideLinux(t *testing.T) {
	// The non-Linux runtime exposes no simulated credential socket. Linux peer
	// credential/permissions behavior is exercised in platform-specific tests.
	if strings.Contains(os.Getenv("OS"), "Windows") {
		if err := RunV2RecoveryClient(context.Background(), t.TempDir(), "snapshot", "unused"); err == nil {
			t.Fatal("non-Linux exposed an insecure recovery endpoint")
		}
	}
}

func TestV2RecoverySnapshotPreservesBillingCursorsAndRejectsNewDegradation(t *testing.T) {
	s, ctx, target := v2TestFixture(t)
	command := v2TestCommand(t, "recovery-billing-rule-one", target)
	command.Rule.Billing, command.Rule.EntitlementVersion = true, 7
	command.BillingPeriodID = "original-immutable-billing-grant"
	request, response := v2TestResponse(s, []V2Command{command})
	if err := s.applyV2Response(ctx, request, response); err != nil {
		t.Fatal(err)
	}
	v2WaitState(t, s, ctx, command.RuleID, "ready")
	p := s.processes[command.RuleID]
	s.mu.Lock()
	s.recordV2TrafficLocked(p, 100, 200, 1, time.Now().UnixMilli())
	batch := s.v2TrafficBatchLocked()
	if len(batch) != 1 {
		s.mu.Unlock()
		t.Fatal("missing test billing sample")
	}
	if err := s.acknowledgeV2TrafficLocked(batch, []V2TrafficAck{{RuleID: command.RuleID, Epoch: batch[0].Epoch, Sequence: batch[0].Sequence}}); err != nil {
		s.mu.Unlock()
		t.Fatal(err)
	}
	s.mu.Unlock()
	snapshot, err := s.recoverySnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(s.v2Traffic.Pending) != 0 || len(snapshot.Traffic) != 1 || snapshot.Traffic[0] != batch[0] || !snapshot.HistoryIncomplete {
		t.Fatal("snapshot lost active ACKed cursor or fabricated complete historical accounting")
	}
	plan := recoveryTestPlan(s, snapshot)
	s.v2Traffic.Degraded = true
	if _, err := s.applyRecoveryPlan(ctx, plan); err == nil {
		t.Fatal("adoption ignored accounting degradation after snapshot")
	}
	if s.v2.disk.ControlEpoch != snapshot.ControlEpoch || s.processes[command.RuleID] != p {
		t.Fatal("rejected accounting-degraded adoption changed authority or process")
	}
}

func TestV2RecoveryJSONRejectsCaseFoldedDuplicateFields(t *testing.T) {
	for _, raw := range []string{`{"protocolVersion":2,"ProtocolVersion":2}`, `{"decisions":[{"action":"hold","Action":"stop"}]}`, `{"token":"first-private-token","TOKEN":"second-private-token"}`} {
		var plan V2RecoveryPlan
		if strictV2JSON([]byte(raw), &plan) == nil {
			t.Fatal("case-insensitive duplicate recovery field was accepted")
		}
	}
}
