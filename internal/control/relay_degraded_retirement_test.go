package control

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func degradedStoppedRecovery(t *testing.T, finalize bool) *relayV2Fixture {
	t.Helper()
	f, snapshots := recoveryFixture(t)
	for i := range snapshots {
		snapshots[i].AccountingDegraded = true
		plan, err := f.app.prepareRelayRecovery(recoveryInput(snapshots[i], "stop"), time.Now().UnixMilli())
		if err != nil {
			t.Fatal(err)
		}
		f.requests[i].AccountingDegraded = true
		if out := recoveryObserve(t, f, i, plan); out.Status != "ready" {
			t.Fatal("exact degraded stop ACK not accepted")
		}
	}
	if finalize {
		epoch := ""
		_ = f.app.Store.View(func(s *State) error {
			control, _ := LoadDoc[RelayV2Control](s, "relay_v2_control", "default")
			epoch = control.Epoch
			return nil
		})
		if err := f.app.finalizeRelayRecovery("OLD_CONTROL_ISOLATED_AND_FINANCES_REVIEWED "+epoch, time.Now().UnixMilli()); err != nil {
			t.Fatalf("explicit root terminal financial review blocked: %v", err)
		}
	}
	return f
}

func archiveDegradedStops(t *testing.T, f *relayV2Fixture) {
	t.Helper()
	if err := f.app.Store.Update(func(s *State) error {
		if err := relayCleanup(s, time.Now().UnixMilli()); err != nil {
			return err
		}
		if len(s.Docs["user_rules"]) != 0 {
			t.Fatal("confirmed stopped degraded rules did not archive")
		}
		DeleteDoc(s, "routes", "route")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRelayDegradedTerminalRootReviewThenRetirement(t *testing.T) {
	f := degradedStoppedRecovery(t, true)
	var evidence *State
	_ = f.app.Store.View(func(s *State) error {
		if len(s.Docs[relayDegradedReviewCollection]) != 2 {
			t.Fatal("root did not persist terminal financial review")
		}
		for _, agent := range ListDocs[RelayAgent](s, "relay_agents") {
			if !agent.AccountingDegraded || !relayDegradedReviewValid(s, agent, time.Now().UnixMilli()) {
				t.Fatal("root cleared degraded flag or wrote unusable proof")
			}
		}
		evidence, _ = cloneState(s)
		return nil
	})
	archiveDegradedStops(t, f)
	// Normal repeated terminal observations do not change reviewed intent. No
	// command, generation, ledger, instance or plan may otherwise change.
	if err := f.app.Store.Update(func(s *State) error {
		for _, p := range ListDocs[relayRecoveryPrepared](s, "relay_v2_recovery_prepared") {
			agent, _ := LoadDoc[RelayAgent](s, "relay_agents", p.AgentID)
			for _, ack := range p.Expected {
				if err := relayV2ApplyAck(s, agent, ack, time.Now().UnixMilli()); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	retireFixtureAgents(t, f)
	if err := f.app.Store.Update(func(s *State) error {
		for _, record := range ListDocs[relayAgentRetirement](s, relayRetirementCollection) {
			if !record.Agent.AccountingDegraded || record.DegradedReviewDigest == "" {
				t.Fatal("retirement erased degraded financial boundary")
			}
		}
		for _, col := range []string{relayDegradedReviewCollection, "relay_v2_recovery_snapshots", "relay_v2_traffic_review", "relay_v2_billing_grants", "relay_v2_traffic_cursors", "ledger"} {
			if !reflect.DeepEqual(evidence.Docs[col], s.Docs[col]) {
				t.Fatalf("retirement altered ledger/snapshot evidence %s", col)
			}
		}
		return isolateRestoredState(s, true)
	}); err != nil {
		t.Fatal(err)
	}
	epoch := ""
	_ = f.app.Store.View(func(s *State) error {
		control, _ := LoadDoc[RelayV2Control](s, "relay_v2_control", "default")
		epoch = control.Epoch
		return nil
	})
	if err := f.app.finalizeRelayRecovery("OLD_CONTROL_ISOLATED_AND_FINANCES_REVIEWED "+epoch, time.Now().UnixMilli()); err != nil {
		t.Fatalf("retired degraded proof failed future restore: %v", err)
	}
}

func TestRelayDegradedTerminalReviewRejectsMissingChangedAndRunning(t *testing.T) {
	for _, fault := range []string{"no-review", "bad-review", "stale-plan", "stale-epoch", "stale-instance", "changed-history", "changed-command"} {
		t.Run("retire/"+fault, func(t *testing.T) {
			f := degradedStoppedRecovery(t, true)
			archiveDegradedStops(t, f)
			if err := f.app.Store.Update(func(s *State) error {
				id := f.agents[0].ID
				switch fault {
				case "no-review":
					DeleteDoc(s, relayDegradedReviewCollection, id)
				case "bad-review":
					s.Docs[relayDegradedReviewCollection][id] = json.RawMessage(`{"version":1,"version":1}`)
				case "stale-plan":
					p, _ := LoadDoc[relayRecoveryPrepared](s, "relay_v2_recovery_prepared", id)
					p.PlanID = commerceID()
					return SaveDoc(s, "relay_v2_recovery_prepared", id, p)
				case "stale-epoch":
					c, _ := LoadDoc[RelayV2Catalog](s, "relay_v2_catalogs", id)
					c.ControlEpoch = commerceID()
					return SaveDoc(s, "relay_v2_catalogs", id, c)
				case "stale-instance":
					a, _ := LoadDoc[RelayAgent](s, "relay_agents", id)
					a.BootID = commerceID()
					return SaveDoc(s, "relay_agents", id, a)
				case "changed-history":
					for key, c := range s.Docs["relay_v2_command_history"] {
						var command RelayV2Command
						_ = json.Unmarshal(c, &command)
						if command.AgentID == id {
							command.Reason += "changed"
							return SaveDoc(s, "relay_v2_command_history", key, command)
						}
					}
				case "changed-command":
					c, _ := LoadDoc[RelayV2Command](s, "relay_v2_commands", id+":"+f.ruleID)
					c.Reason += "changed"
					return SaveDoc(s, "relay_v2_commands", id+":"+f.ruleID, c)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			before := recoveryState(t, f.app)
			if err := f.app.Store.Update(func(s *State) error { return retireRelayAgent(s, f.agents[0].ID, time.Now().UnixMilli()) }); err == nil {
				t.Fatal("ordinary deletion bypassed root financial review")
			}
			if before != recoveryState(t, f.app) {
				t.Fatal("failed retirement partially mutated state")
			}
		})
	}
	for _, fault := range []string{"running", "pause", "empty", "stale-ack", "future-ack"} {
		t.Run("finalize/"+fault, func(t *testing.T) {
			f := degradedStoppedRecovery(t, false)
			epoch := ""
			if err := f.app.Store.Update(func(s *State) error {
				control, _ := LoadDoc[RelayV2Control](s, "relay_v2_control", "default")
				epoch = control.Epoch
				id := f.agents[0].ID
				p, _ := LoadDoc[relayRecoveryPrepared](s, "relay_v2_recovery_prepared", id)
				if fault == "empty" {
					p.Expected = nil
				} else if fault == "stale-ack" {
					p.VerifiedAt = time.Now().Add(-time.Minute * 5).UnixMilli()
				} else if fault == "future-ack" {
					p.VerifiedAt = time.Now().Add(time.Hour).UnixMilli()
				} else {
					command, _ := LoadDoc[RelayV2Command](s, "relay_v2_commands", id+":"+f.ruleID)
					if fault == "running" {
						command.Action, command.AckState = "upsert", "ready"
					} else {
						command.Action = "pause"
					}
					if err := SaveDoc(s, "relay_v2_commands", id+":"+f.ruleID, command); err != nil {
						return err
					}
				}
				return SaveDoc(s, "relay_v2_recovery_prepared", id, p)
			}); err != nil {
				t.Fatal(err)
			}
			before := recoveryState(t, f.app)
			if err := f.app.finalizeRelayRecovery("OLD_CONTROL_ISOLATED_AND_FINANCES_REVIEWED "+epoch, time.Now().UnixMilli()); err == nil {
				t.Fatal("nonterminal/unproven degraded node finalized")
			}
			if before != recoveryState(t, f.app) {
				t.Fatal("failed financial review partially mutated state")
			}
		})
	}
}
