package control

import (
	"encoding/json"
	"testing"
	"time"
)

// Independently reviewed finalization boundaries: a previously valid prepared
// plan must not turn a later incomplete catalogue or rotated identity into a
// blanket authorization to clear global recovery isolation.
func TestRelayRecoveryFinalizeRechecksCompleteCurrentInventory(t *testing.T) {
	for _, mutation := range []string{"malformed agent", "missing agent", "orphan command", "extra command", "changed credential", "changed catalogue revision", "missing segment agent", "malformed orphan prepared"} {
		t.Run(mutation, func(t *testing.T) {
			f, snapshots := recoveryFixture(t)
			epoch := ""
			for index, snapshot := range snapshots {
				plan, err := f.app.prepareRelayRecovery(recoveryInput(snapshot, "adopt"), time.Now().UnixMilli())
				if err != nil {
					t.Fatal(err)
				}
				if response := recoveryObserve(t, f, index, plan); response.Status != "ready" {
					t.Fatal("fixture did not verify prepared plan")
				}
				epoch = plan.ControlEpoch
			}
			if err := f.app.Store.Update(func(s *State) error {
				agentID := f.agents[0].ID
				switch mutation {
				case "malformed agent":
					s.Docs["relay_agents"][agentID] = json.RawMessage(`[]`)
				case "missing agent":
					DeleteDoc(s, "relay_agents", agentID)
				case "changed credential":
					agent, _ := LoadDoc[RelayAgent](s, "relay_agents", agentID)
					agent.TokenHash = commerceHash("a-different-private-current-credential")
					return SaveDoc(s, "relay_agents", agentID, agent)
				case "changed catalogue revision":
					catalog, _ := LoadDoc[RelayV2Catalog](s, "relay_v2_catalogs", agentID)
					catalog.Revision++
					return SaveDoc(s, "relay_v2_catalogs", agentID, catalog)
				case "orphan command", "extra command":
					for _, command := range ListDocs[RelayV2Command](s, "relay_v2_commands") {
						if command.AgentID != agentID {
							continue
						}
						command.CommandID, command.RuleID = commerceID(), commerceID()
						command.Action, command.SealedRule, command.AckState = "revoke", "", "pending"
						if mutation == "orphan command" {
							command.AgentID = commerceID()
						}
						return SaveDoc(s, "relay_v2_commands", command.AgentID+":"+command.RuleID, command)
					}
				case "missing segment agent":
					for key := range s.Docs["user_rules"] {
						rule, _ := LoadDoc[UserRule](s, "user_rules", key)
						if len(rule.Segments) == 0 {
							continue
						}
						rule.Segments[0].AgentID = commerceID()
						return SaveDoc(s, "user_rules", key, rule)
					}
				case "malformed orphan prepared":
					s.Docs["relay_v2_recovery_prepared"][commerceID()] = json.RawMessage(`[]`)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			before := recoveryState(t, f.app)
			if err := f.app.finalizeRelayRecovery("OLD_CONTROL_ISOLATED_AND_FINANCES_REVIEWED "+epoch, time.Now().UnixMilli()); err == nil {
				t.Fatalf("finalize cleared recovery despite %s after verification", mutation)
			}
			if after := recoveryState(t, f.app); after != before {
				t.Fatal("rejected finalization partially mutated recovery/financial state")
			}
		})
	}
}

func TestRelayRecoveryFinalizeRejectsCommandHistoryCorruption(t *testing.T) {
	for _, mutation := range []string{"malformed", "missing current", "orphan rule", "orphan agent", "higher generation", "higher revision", "duplicate generation", "changed current intent", "revoke before running"} {
		t.Run(mutation, func(t *testing.T) {
			f, snapshots := recoveryFixture(t)
			// Include a legitimate historical pause followed by a newer upsert.
			// It must remain recoverable; a pause is not a terminal tombstone.
			var pauseID string
			if err := f.app.Store.Update(func(s *State) error {
				key := f.agents[0].ID + ":" + snapshots[0].Records[0].RuleID
				current, _ := LoadDoc[RelayV2Command](s, "relay_v2_commands", key)
				pause := current
				pauseID = commerceID()
				pause.CommandID, pause.Action, pause.SealedRule = pauseID, "pause", ""
				pause.Generation, pause.Revision = current.Generation+1, current.Revision+1
				if err := SaveDoc(s, "relay_v2_command_history", pauseID, pause); err != nil {
					return err
				}
				current.CommandID, current.Generation, current.Revision = commerceID(), pause.Generation+1, pause.Revision+1
				if err := SaveDoc(s, "relay_v2_commands", key, current); err != nil {
					return err
				}
				history := current
				history.SealedRule = ""
				if err := SaveDoc(s, "relay_v2_command_history", current.CommandID, history); err != nil {
					return err
				}
				catalog, _ := LoadDoc[RelayV2Catalog](s, "relay_v2_catalogs", current.AgentID)
				catalog.Revision = current.Revision
				if err := SaveDoc(s, "relay_v2_catalogs", current.AgentID, catalog); err != nil {
					return err
				}
				wire, err := f.app.recoveryWireCommand(current)
				if err != nil {
					return err
				}
				wire.Rule.TLSPrivateKey = ""
				last := wire
				snapshots[0].Revision = current.Revision
				snapshots[0].Records[0].Command, snapshots[0].Records[0].LastApplied = wire, &last
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			epoch := ""
			for i, snapshot := range snapshots {
				plan, err := f.app.prepareRelayRecovery(recoveryInput(snapshot, "adopt"), time.Now().UnixMilli())
				if err != nil {
					t.Fatal(err)
				}
				if out := recoveryObserve(t, f, i, plan); out.Status != "ready" {
					t.Fatal("fixture plan was not verified")
				}
				epoch = plan.ControlEpoch
			}
			if err := f.app.Store.View(f.app.validateRelayRecoveryFinalization); err != nil {
				t.Fatalf("valid pause-to-upsert history rejected before corruption: %v", err)
			}
			if err := f.app.Store.Update(func(s *State) error {
				current, _ := LoadDoc[RelayV2Command](s, "relay_v2_commands", f.agents[0].ID+":"+snapshots[0].Records[0].RuleID)
				old := current
				old.SealedRule = ""
				switch mutation {
				case "malformed":
					s.Docs["relay_v2_command_history"][commerceID()] = json.RawMessage(`[]`)
					return nil
				case "missing current":
					DeleteDoc(s, "relay_v2_command_history", current.CommandID)
					return nil
				case "orphan rule":
					old.CommandID, old.RuleID = commerceID(), commerceID()
				case "orphan agent":
					old.CommandID, old.AgentID = commerceID(), commerceID()
				case "higher generation":
					old.CommandID, old.Generation = commerceID(), current.Generation+1
				case "higher revision":
					old.CommandID, old.Revision = commerceID(), current.Revision+1
				case "duplicate generation":
					old.CommandID = commerceID()
				case "changed current intent":
					old.BillingPeriodID = commerceID()
				case "revoke before running":
					old, _ = LoadDoc[RelayV2Command](s, "relay_v2_command_history", pauseID)
					old.Action = "revoke"
				}
				return SaveDoc(s, "relay_v2_command_history", old.CommandID, old)
			}); err != nil {
				t.Fatal(err)
			}
			before := recoveryState(t, f.app)
			if err := f.app.finalizeRelayRecovery("OLD_CONTROL_ISOLATED_AND_FINANCES_REVIEWED "+epoch, time.Now().UnixMilli()); err == nil {
				t.Fatalf("finalize released despite %s history", mutation)
			}
			if recoveryState(t, f.app) != before {
				t.Fatal("failed history check partially changed persistent state")
			}
		})
	}
}

func TestRelayRecoveryRepeatedStopsPreserveBackupAdmission(t *testing.T) {
	for _, nodeAhead := range []bool{false, true} {
		t.Run(map[bool]string{false: "reuse exact revoke", true: "monotonic newer revoke"}[nodeAhead], func(t *testing.T) {
			f, snapshots := recoveryFixture(t)
			epoch := ""
			for i, snapshot := range snapshots {
				first, err := f.app.prepareRelayRecovery(recoveryInput(snapshot, "stop"), time.Now().UnixMilli())
				if err != nil {
					t.Fatal(err)
				}
				if out := recoveryObserve(t, f, i, first); out.Status != "ready" {
					t.Fatal("first explicit stop was not verified")
				}
				stopped := *first.Decisions[0].Command
				snapshot.PlanSequence, snapshot.ServerURL, snapshot.ControlEpoch, snapshot.Revision = first.PlanSequence, first.ServerURL, first.ControlEpoch, first.Revision
				snapshot.Records[0].Command = stopped
				snapshot.Records[0].State, snapshot.Records[0].Running, snapshot.Records[0].RuntimeHash, snapshot.Records[0].ProcessEpoch = "stopped", false, "", ""
				if nodeAhead {
					snapshot.Records[0].Command.CommandID = commerceID()
					snapshot.Records[0].Command.Generation++
				}
				second, err := f.app.prepareRelayRecovery(recoveryInput(snapshot, "stop"), time.Now().UnixMilli())
				if err != nil {
					t.Fatal(err)
				}
				command := *second.Decisions[0].Command
				if command.Action != "revoke" || !nodeAhead && command != stopped || nodeAhead && command.Generation <= snapshot.Records[0].Command.Generation {
					t.Fatal("repeated stop failed exact reuse or monotonic terminal transition")
				}
				if out := recoveryObserve(t, f, i, second); out.Status != "ready" {
					t.Fatal("second explicit stop was not verified")
				}
				epoch = second.ControlEpoch
			}
			if err := f.app.finalizeRelayRecovery("OLD_CONTROL_ISOLATED_AND_FINANCES_REVIEWED "+epoch, time.Now().UnixMilli()); err != nil {
				t.Fatal(err)
			}
			if err := f.app.Store.View(func(s *State) error {
				if report := backupPausePreflight(s, time.Now().UnixMilli()); !report.CanPauseControl {
					t.Fatalf("confirmed terminal recovery blocked subsequent backup: %+v", report)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRelayRecoveryHistoryMonotonicTerminalGuard(t *testing.T) {
	makeCommand := func(id string, generation int64, action string) RelayV2Command {
		return RelayV2Command{AgentID: "agent", RuleID: "rule", CommandID: id, Generation: generation, Revision: generation, Action: action}
	}
	for _, tc := range []struct {
		name    string
		actions []string
		bad     bool
	}{
		{"pause then resume", []string{"upsert", "pause", "upsert"}, false},
		{"repeated pause", []string{"upsert", "pause", "pause"}, false},
		{"repeated revoke", []string{"upsert", "revoke", "revoke"}, false},
		{"revoke then pause", []string{"upsert", "revoke", "pause"}, true},
		{"revoke then upsert", []string{"upsert", "revoke", "upsert"}, true},
		{"intermediate resurrection", []string{"revoke", "upsert", "revoke"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			history := map[string]RelayV2Command{}
			var current RelayV2Command
			for i, action := range tc.actions {
				current = makeCommand(commerceID(), int64(i+1), action)
				history[current.CommandID] = current
			}
			if bad := len(backupPauseHistoryConflicts(map[string]RelayV2Command{"agent:rule": current}, history)) != 0; bad != tc.bad {
				t.Fatalf("history rejected=%v, want=%v", bad, tc.bad)
			}
		})
	}
	old := makeCommand("old", 1, "pause")
	current := makeCommand("new", 2, "upsert")
	current.Revision = old.Revision
	if backupPauseHistoryPrecedes(old, current) {
		t.Fatal("replacement without strictly advancing revision accepted")
	}
	current.Revision++
	current.CommandID = old.CommandID
	if backupPauseHistoryPrecedes(old, current) {
		t.Fatal("replacement reusing command identity accepted")
	}
}
