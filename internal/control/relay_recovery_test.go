package control

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mozziexwz/node/internal/relayruntime"
)

func recoveryFixture(t *testing.T) (*relayV2Fixture, []relayruntime.V2RecoverySnapshot) {
	t.Helper()
	f := newRelayV2Fixture(t)
	if err := f.app.Store.Update(func(s *State) error {
		row, _ := LoadDoc[UserRule](s, "user_rules", f.user.ID+":route")
		row.Segments[0].Runtime.Targets = []string{"8.8.4.4:20001"}
		return SaveDoc(s, "user_rules", f.user.ID+":route", row)
	}); err != nil {
		t.Fatal(err)
	}
	f.ready(t)
	f.app.Config.PublicURL = "https://new-panel.example.test"
	snapshots := make([]relayruntime.V2RecoverySnapshot, len(f.agents))
	err := f.app.Store.Update(func(s *State) error {
		s.Settings["maintenance"] = true
		for i, agent := range f.agents {
			catalog, _ := LoadDoc[RelayV2Catalog](s, "relay_v2_catalogs", agent.ID)
			snapshot := relayruntime.V2RecoverySnapshot{ProtocolVersion: 2, RecoveryID: commerceID(), AgentID: agent.ID, ServerURL: "https://old-panel.example.test", ControlEpoch: catalog.ControlEpoch, Revision: catalog.Revision, Records: []relayruntime.V2RecoveryRecord{}, Traffic: []relayruntime.V2Traffic{}, HistoryIncomplete: true, CapturedAt: time.Now().UnixMilli()}
			for _, command := range ListDocs[RelayV2Command](s, "relay_v2_commands") {
				if command.AgentID != agent.ID {
					continue
				}
				wire, e := f.app.recoveryWireCommand(command)
				if e != nil {
					return e
				}
				if wire.Rule != nil {
					wire.Rule.TLSPrivateKey = ""
				}
				last := wire
				snapshot.Records = append(snapshot.Records, relayruntime.V2RecoveryRecord{RuleID: command.RuleID, Command: wire, LastApplied: &last, State: "ready", Running: true, RuntimeHash: command.RuntimeHash, ProcessEpoch: commerceID()})
			}
			snapshots[i] = snapshot
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return f, snapshots
}
func recoveryInput(snapshot relayruntime.V2RecoverySnapshot, action string) RelayRecoveryRequest {
	input := RelayRecoveryRequest{Snapshot: snapshot, Fingerprint: recoveryDigest(snapshot), Confirmation: "OLD_CONTROL_ISOLATED " + snapshot.RecoveryID}
	for _, row := range snapshot.Records {
		input.Decisions = append(input.Decisions, RelayRecoverySelection{RuleID: row.RuleID, Action: action})
	}
	return input
}
func recoveryObserve(t *testing.T, f *relayV2Fixture, index int, plan relayruntime.V2RecoveryPlan) relayruntime.V2SyncResponse {
	t.Helper()
	in := f.requests[index]
	in.Sequence += 100
	in.ControlEpoch, in.AppliedRevision = plan.ControlEpoch, plan.Revision
	in.Acks = []relayruntime.V2Ack{}
	for _, d := range plan.Decisions {
		if d.Command == nil {
			continue
		}
		if d.Command.Action == "upsert" {
			in.Acks = append(in.Acks, v2Ready(*d.Command))
		} else {
			in.Acks = append(in.Acks, v2Stopped(*d.Command))
		}
	}
	f.tokens[index] = plan.Token
	f.requests[index] = in
	return f.send(t, index, in, 200)
}
func recoveryState(t *testing.T, a *App) string {
	t.Helper()
	var raw []byte
	if e := a.Store.View(func(s *State) error { raw, _ = json.Marshal(s); return nil }); e != nil {
		t.Fatal(e)
	}
	return string(raw)
}

func TestRelayRecoveryReportPrepareAndExplicitFinalize(t *testing.T) {
	f, snapshots := recoveryFixture(t)
	before := recoveryState(t, f.app)
	for _, snapshot := range snapshots {
		if err := f.app.Store.View(func(s *State) error {
			report, e := f.app.relayRecoveryReport(s, snapshot)
			if len(report.Differences) != 1 || !report.Differences[0].CanAdopt || !report.HistoryIncomplete {
				t.Fatal("same runtime not available for review")
			}
			return e
		}); err != nil {
			t.Fatal(err)
		}
	}
	if recoveryState(t, f.app) != before {
		t.Fatal("report mutated state")
	}
	plans := []relayruntime.V2RecoveryPlan{}
	for i, snapshot := range snapshots {
		input := recoveryInput(snapshot, "adopt")
		plan, err := f.app.prepareRelayRecovery(input, time.Now().UnixMilli())
		if err != nil {
			t.Fatal(err)
		}
		retry, err := f.app.prepareRelayRecovery(input, time.Now().UnixMilli())
		if err != nil || recoveryDigest(plan) != recoveryDigest(retry) {
			t.Fatal("uncertain output retry changed token/plan")
		}
		if out := recoveryObserve(t, f, i, plan); out.Status != "ready" || len(out.Commands) != 0 || len(out.TrafficAcks) != 0 {
			t.Fatal("recovery confirmation mutated runtime or acknowledged uncommitted accounting")
		}
		plans = append(plans, plan)
	}
	if err := f.app.finalizeRelayRecovery("wrong", time.Now().UnixMilli()); err == nil {
		t.Fatal("recovery released without isolation/finance confirmation")
	}
	confirmation := "OLD_CONTROL_ISOLATED_AND_FINANCES_REVIEWED " + plans[0].ControlEpoch
	if err := f.app.finalizeRelayRecovery(confirmation, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := f.app.Store.View(func(s *State) error {
		c, _ := LoadDoc[RelayV2Control](s, "relay_v2_control", "default")
		if c.RecoveryRequired || !boolSetting(s, "maintenance") {
			t.Fatal("finalize did not retain independent maintenance")
		}
		if s.Users[f.user.ID].TrafficUsed != f.user.TrafficUsed || s.Users[f.user.ID].BalanceCents != f.user.BalanceCents {
			t.Fatal("recovery altered money/traffic")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for i, plan := range plans {
		if out := recoveryObserve(t, f, i, plan); out.Status != "ready" || len(out.Commands) != 0 {
			t.Fatal("same configuration redeployed after takeover")
		}
	}
}

func TestRelayRecoveryRejectsUnsafeAndChangedInputsAtomically(t *testing.T) {
	cases := []string{"fingerprint", "confirmation", "duplicate", "missing_decision", "bad_action", "wrong_hash", "new_generation", "revoked", "unknown_rule", "missing_node", "wrong_agent", "tls_secret", "accounting_degraded", "maintenance_off", "backup_active", "pause_gate"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			f, snaps := recoveryFixture(t)
			in := recoveryInput(snaps[0], "adopt")
			switch name {
			case "fingerprint":
				in.Fingerprint = strings.Repeat("0", 64)
			case "confirmation":
				in.Confirmation = "yes"
			case "duplicate":
				in.Decisions = append(in.Decisions, in.Decisions[0])
			case "missing_decision":
				in.Decisions = nil
			case "bad_action":
				in.Decisions[0].Action = "force"
			case "wrong_hash":
				in.Snapshot.Records[0].RuntimeHash = strings.Repeat("0", 64)
			case "new_generation":
				in.Snapshot.Records[0].Command.Generation++
			case "revoked":
				in.Snapshot.Records[0].Command.Action = "revoke"
			case "unknown_rule":
				in.Snapshot.Records[0].RuleID = commerceID()
				in.Snapshot.Records[0].Command.RuleID = in.Snapshot.Records[0].RuleID
				in.Snapshot.Records[0].LastApplied.RuleID = in.Snapshot.Records[0].RuleID
			case "missing_node":
				in.Snapshot.Records = []relayruntime.V2RecoveryRecord{}
				in.Decisions = nil
			case "wrong_agent":
				in.Snapshot.AgentID = commerceID()
			case "tls_secret":
				in.Snapshot.Records[0].Command.Rule.TLSPrivateKey = "secret"
			case "accounting_degraded":
				in.Snapshot.AccountingDegraded = true
			case "maintenance_off", "backup_active", "pause_gate":
				if err := f.app.Store.Update(func(s *State) error {
					switch name {
					case "maintenance_off":
						s.Settings["maintenance"] = false
					case "backup_active":
						return SaveDoc(s, "backup_operations", "unknown", true)
					case "pause_gate":
						return SaveDoc(s, backupPauseCollection, "default", true)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			if name != "fingerprint" {
				in.Fingerprint = recoveryDigest(in.Snapshot)
			}
			before := recoveryState(t, f.app)
			if _, err := f.app.prepareRelayRecovery(in, time.Now().UnixMilli()); err == nil {
				t.Fatal("unsafe recovery prepared")
			}
			if recoveryState(t, f.app) != before {
				t.Fatal("failed recovery changed state")
			}
		})
	}
}

func TestRelayRecoveryHoldAndStopRemainIsolated(t *testing.T) {
	for _, action := range []string{"hold", "stop"} {
		t.Run(action, func(t *testing.T) {
			f, snaps := recoveryFixture(t)
			plan, err := f.app.prepareRelayRecovery(recoveryInput(snaps[0], action), time.Now().UnixMilli())
			if err != nil {
				t.Fatal(err)
			}
			out := recoveryObserve(t, f, 0, plan)
			if len(out.Commands) != 0 {
				t.Fatal("recovery issued normal configuration")
			}
			if action == "hold" && out.Status != "recovery_required" {
				t.Fatal("unreviewed rule released")
			}
			if action == "stop" && (plan.Decisions[0].Command.Action != "revoke" || plan.Decisions[0].Command.Generation <= snaps[0].Records[0].Command.Generation) {
				t.Fatal("missing monotonic explicit revoke")
			}
			if err := f.app.finalizeRelayRecovery("OLD_CONTROL_ISOLATED_AND_FINANCES_REVIEWED "+plan.ControlEpoch, time.Now().UnixMilli()); err == nil {
				t.Fatal("unreviewed other node released")
			}
			if err := f.app.Store.Update(func(s *State) error {
				if err := relayCleanup(s, time.Now().UnixMilli()+10*commerceDay); err != nil {
					return err
				}
				if _, ok := LoadDoc[UserRule](s, "user_rules", f.user.ID+":route"); !ok {
					t.Fatal("recovery freed port by elapsed time")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRelayRecoveryUnknownStopAndSpoofedObservations(t *testing.T) {
	f, snaps := recoveryFixture(t)
	in := recoveryInput(snaps[0], "adopt")
	extra := in.Snapshot.Records[0]
	extra.RuleID = commerceID()
	extra.Command.RuleID = extra.RuleID
	copy := *extra.LastApplied
	copy.RuleID = extra.RuleID
	extra.LastApplied = &copy
	in.Snapshot.Records = append(in.Snapshot.Records, extra)
	in.Fingerprint = recoveryDigest(in.Snapshot)
	in.Decisions = append(in.Decisions, RelayRecoverySelection{RuleID: extra.RuleID, Action: "stop"})
	plan, err := f.app.prepareRelayRecovery(in, time.Now().UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if out := recoveryObserve(t, f, 0, plan); out.Status != "ready" {
		t.Fatal("explicit unknown stop not confirmable")
	}
	request := f.requests[0]
	request.Sequence++
	request.Acks[0].Generation++
	if out := f.send(t, 0, request, 200); out.Status != "recovery_required" || len(out.Commands) != 0 {
		t.Fatal("changed ACK accepted")
	}
}
