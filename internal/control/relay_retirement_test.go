package control

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mozziexwz/node/internal/relayruntime"
)

func retirementFixture(t *testing.T) (*relayV2Fixture, *State) {
	t.Helper()
	f := newRelayV2Fixture(t)
	f.ready(t)
	var old *State
	if err := f.app.Store.Update(func(s *State) error {
		var e error
		old, e = cloneState(s)
		if e != nil {
			return e
		}
		row, _ := LoadDoc[UserRule](s, "user_rules", f.user.ID+":route")
		relayRevoke(&row, 0, true)
		return SaveDoc(s, "user_rules", f.user.ID+":route", row)
	}); err != nil {
		t.Fatal(err)
	}
	for i := range f.agents {
		out := f.sync(t, i)
		if len(out.Commands) != 1 || out.Commands[0].Action != "revoke" {
			t.Fatal("missing terminal revoke")
		}
		f.sync(t, i, v2Stopped(out.Commands[0]))
	}
	if err := f.app.Store.Update(func(s *State) error {
		if err := relayCleanup(s, time.Now().UnixMilli()); err != nil {
			return err
		}
		if len(s.Docs["user_rules"]) != 0 || len(s.Docs["relay_rule_archive"]) != 1 {
			t.Fatal("rule not terminally archived")
		}
		DeleteDoc(s, "routes", "route")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return f, old
}

func retireFixtureAgents(t *testing.T, f *relayV2Fixture) {
	t.Helper()
	admin := &User{ID: "retirement-admin", Role: "admin", Status: "active"}
	for _, agent := range f.agents {
		w := commerceTestRequest(f.mux, admin, http.MethodDelete, "/api/admin/relay-agents/"+agent.ID, nil)
		if w.Code != 200 {
			t.Fatalf("retirement HTTP %d: %s", w.Code, w.Body.String())
		}
	}
}

func TestRelayRetirementDeleteBackupRestoreAndFinalize(t *testing.T) {
	f, old := retirementFixture(t)
	// A repeated stopped ACK after rule archival legitimately advances the
	// command observation, not the archived segment. Retirement must accept it.
	var command RelayV2Command
	if err := f.app.Store.View(func(s *State) error {
		command, _ = LoadDoc[RelayV2Command](s, "relay_v2_commands", f.agents[0].ID+":"+f.ruleID)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.app.Store.Update(func(s *State) error {
		return relayV2ApplyAck(s, f.agents[0], relayruntime.V2Ack{RuleID: command.RuleID, CommandID: command.CommandID, Generation: command.Generation, RuntimeHash: command.RuntimeHash, State: "stopped"}, time.Now().UnixMilli())
	}); err != nil {
		t.Fatal(err)
	}
	var before, archived *State
	_ = f.app.Store.View(func(s *State) error { before, _ = cloneState(s); return nil })
	retireFixtureAgents(t, f)
	_ = f.app.Store.View(func(s *State) error { archived, _ = cloneState(s); return nil })
	if len(archived.Docs["relay_agents"]) != 0 || len(archived.Docs[relayRetirementCollection]) != len(f.agents) {
		t.Fatal("active identity not replaced by explicit retirement")
	}
	for _, col := range []string{"relay_v2_catalogs", "relay_v2_commands", "relay_v2_command_history", "relay_rule_archive", "relay_v2_billing_grants", "relay_v2_traffic_cursors", "ledger"} {
		if !reflect.DeepEqual(before.Docs[col], archived.Docs[col]) {
			t.Fatalf("retirement modified evidence/accounting %s", col)
		}
	}
	if p := backupPausePreflight(archived, time.Now().UnixMilli()); !p.CanPauseControl {
		t.Fatalf("legal terminal retirement blocked backup: %+v", p)
	}
	f.send(t, 0, f.requests[0], 401)
	// Safe restore may not reintroduce an identity from before its retirement.
	if _, _, err := mergeSafeRestore(archived, old); err == nil {
		t.Fatal("safe restore resurrected retired identity")
	}
	if err := f.app.Store.Update(func(s *State) error { return isolateRestoredState(s, true) }); err != nil {
		t.Fatal(err)
	}
	epoch := ""
	_ = f.app.Store.View(func(s *State) error {
		control, _ := LoadDoc[RelayV2Control](s, "relay_v2_control", "default")
		epoch = control.Epoch
		if !control.RecoveryRequired || !boolSetting(s, "maintenance") {
			t.Fatal("restore bypassed isolation")
		}
		for _, col := range []string{"relay_v2_catalogs", "relay_v2_commands", "relay_v2_command_history", relayRetirementCollection} {
			if !reflect.DeepEqual(archived.Docs[col], s.Docs[col]) {
				t.Fatalf("restore rewrote terminal evidence %s", col)
			}
		}
		return nil
	})
	if err := f.app.finalizeRelayRecovery("OLD_CONTROL_ISOLATED_AND_FINANCES_REVIEWED "+epoch, time.Now().UnixMilli()); err != nil {
		t.Fatalf("legal historical retirement made finalization unreachable: %v", err)
	}
	_ = f.app.Store.View(func(s *State) error {
		if restoredRelayRecoveryRequired(s) || !boolSetting(s, "maintenance") {
			t.Fatal("finalize did not preserve maintenance-only boundary")
		}
		if p := backupPausePreflight(s, time.Now().UnixMilli()); !p.CanPauseControl {
			t.Fatalf("old retired epoch blocked subsequent backup: %+v", p)
		}
		return nil
	})
}

func TestRelayRetirementRejectsUnprovenAndChangedEvidence(t *testing.T) {
	for _, mutation := range []string{"running", "pause", "pending", "future-stop", "missing-history", "orphan-history", "revive-history", "catalog-instance", "active-route", "active-rule", "archive-missing", "archive-not-stopped", "archive-id", "archive-future", "bad-history", "recovery", "accounting"} {
		t.Run("before-delete/"+mutation, func(t *testing.T) {
			f, _ := retirementFixture(t)
			if err := f.app.Store.Update(func(s *State) error { mutateRetirementEvidence(t, s, f, mutation); return nil }); err != nil {
				t.Fatal(err)
			}
			before := recoveryState(t, f.app)
			if err := f.app.Store.Update(func(s *State) error { return retireRelayAgent(s, f.agents[0].ID, time.Now().UnixMilli()) }); err == nil {
				t.Fatal("retired unproven node")
			}
			if recoveryState(t, f.app) != before {
				t.Fatal("rejected retirement partially changed state")
			}
		})
	}
	for _, mutation := range []string{"missing-proof", "bad-proof", "identity-reappeared", "enabled-proof", "capabilities-proof", "changed-digest", "running", "pause", "pending", "missing-history", "orphan-history", "catalog-instance", "active-route", "active-rule", "archive-missing"} {
		t.Run("after-delete/"+mutation, func(t *testing.T) {
			f, _ := retirementFixture(t)
			retireFixtureAgents(t, f)
			if err := f.app.Store.Update(func(s *State) error {
				id := f.agents[0].ID
				switch mutation {
				case "missing-proof":
					DeleteDoc(s, relayRetirementCollection, id)
				case "bad-proof":
					s.Docs[relayRetirementCollection][id] = json.RawMessage(`{"version":1,"version":1}`)
				case "identity-reappeared":
					return SaveDoc(s, "relay_agents", id, f.agents[0])
				case "enabled-proof", "capabilities-proof", "changed-digest":
					record, _ := LoadDoc[relayAgentRetirement](s, relayRetirementCollection, id)
					if mutation == "enabled-proof" {
						record.Agent.Enabled = true
					} else if mutation == "capabilities-proof" {
						record.Agent.Capabilities = nil
					} else {
						record.HistoryDigest = strings.Repeat("0", 64)
					}
					return SaveDoc(s, relayRetirementCollection, id, record)
				default:
					mutateRetirementEvidence(t, s, f, mutation)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			_ = f.app.Store.View(func(s *State) error {
				if p := backupPausePreflight(s, time.Now().UnixMilli()); p.CanPauseControl || p.CanPauseMaintenance {
					t.Fatal("corrupt/orphan retirement passed backup admission")
				}
				if err := f.app.validateRelayRecoveryFinalization(s); err == nil {
					t.Fatal("corrupt/orphan retirement passed finalization")
				}
				return nil
			})
		})
	}
}

func TestRelayRetirementCannotMistakeLostInventoryForEmptyNode(t *testing.T) {
	for _, keepArchive := range []bool{true, false} {
		f, _ := retirementFixture(t)
		if err := f.app.Store.Update(func(s *State) error {
			id := f.agents[0].ID
			for _, name := range []string{"relay_v2_commands", "relay_v2_command_history"} {
				for key, raw := range s.Docs[name] {
					var command RelayV2Command
					_ = json.Unmarshal(raw, &command)
					if command.AgentID == id {
						DeleteDoc(s, name, key)
					}
				}
			}
			if !keepArchive {
				DeleteDoc(s, "relay_rule_archive", f.ruleID)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		before := recoveryState(t, f.app)
		if err := f.app.Store.Update(func(s *State) error { return retireRelayAgent(s, f.agents[0].ID, time.Now().UnixMilli()) }); err == nil {
			t.Fatal("nonzero revision with missing complete inventory retired as empty")
		}
		if recoveryState(t, f.app) != before {
			t.Fatal("lost inventory rejection mutated state")
		}
	}
	f := newRelayV2Fixture(t)
	if err := f.app.Store.Update(func(s *State) error {
		DeleteDoc(s, "user_rules", f.user.ID+":route")
		DeleteDoc(s, "routes", "route")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.sync(t, 0)
	f.sync(t, 0)
	if err := f.app.Store.Update(func(s *State) error { return retireRelayAgent(s, f.agents[0].ID, time.Now().UnixMilli()) }); err != nil {
		t.Fatalf("never-provisioned revision-zero node cannot retire: %v", err)
	}
}

func TestRelayRetirementAcceptsExplicitlyRecoveredEmptyInventory(t *testing.T) {
	f := newRelayV2Fixture(t)
	f.app.Config.PublicURL = "https://new-panel.example.test"
	if err := f.app.Store.Update(func(s *State) error {
		DeleteDoc(s, "user_rules", f.user.ID+":route")
		DeleteDoc(s, "routes", "route")
		s.Settings["maintenance"] = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for i := range f.agents {
		f.sync(t, i)
		f.sync(t, i)
	}
	epoch := ""
	for i, agent := range f.agents {
		var catalog RelayV2Catalog
		_ = f.app.Store.View(func(s *State) error {
			catalog, _ = LoadDoc[RelayV2Catalog](s, "relay_v2_catalogs", agent.ID)
			return nil
		})
		snapshot := relayruntime.V2RecoverySnapshot{ProtocolVersion: 2, RecoveryID: commerceID(), AgentID: agent.ID, ServerURL: "https://old-panel.example.test", ControlEpoch: catalog.ControlEpoch, Revision: 1, Records: []relayruntime.V2RecoveryRecord{}, Traffic: []relayruntime.V2Traffic{}, HistoryIncomplete: true, CapturedAt: time.Now().UnixMilli()}
		plan, err := f.app.prepareRelayRecovery(recoveryInput(snapshot, "adopt"), time.Now().UnixMilli())
		if err != nil {
			t.Fatal(err)
		}
		if plan.Revision <= 0 {
			t.Fatal("fixture did not get root recovery revision")
		}
		epoch = plan.ControlEpoch
		recoveryObserve(t, f, i, plan)
	}
	if err := f.app.finalizeRelayRecovery("OLD_CONTROL_ISOLATED_AND_FINANCES_REVIEWED "+epoch, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	retireFixtureAgents(t, f)
	_ = f.app.Store.View(func(s *State) error {
		if _, err := validateRelayRetirements(s, time.Now().UnixMilli()); err != nil {
			t.Fatal(err)
		}
		return nil
	})
}

func mutateRetirementEvidence(t *testing.T, s *State, f *relayV2Fixture, mutation string) {
	t.Helper()
	id, key := f.agents[0].ID, f.agents[0].ID+":"+f.ruleID
	command, _ := LoadDoc[RelayV2Command](s, "relay_v2_commands", key)
	put := func(c, k string, value any) {
		if err := SaveDoc(s, c, k, value); err != nil {
			t.Fatal(err)
		}
	}
	switch mutation {
	case "running":
		command.Action, command.AckState = "upsert", "ready"
		put("relay_v2_commands", key, command)
	case "pause":
		command.Action = "pause"
		put("relay_v2_commands", key, command)
	case "pending":
		command.AckState = "persisted"
		put("relay_v2_commands", key, command)
	case "future-stop":
		command.AppliedAt = time.Now().Add(time.Hour).UnixMilli()
		put("relay_v2_commands", key, command)
	case "missing-history":
		DeleteDoc(s, "relay_v2_command_history", command.CommandID)
	case "orphan-history":
		command.RuleID, command.CommandID = commerceID(), commerceID()
		put("relay_v2_command_history", command.CommandID, command)
	case "revive-history":
		command.Generation++
		command.Revision++
		command.CommandID = commerceID()
		command.Action = "upsert"
		put("relay_v2_command_history", command.CommandID, command)
	case "catalog-instance":
		catalog, _ := LoadDoc[RelayV2Catalog](s, "relay_v2_catalogs", id)
		catalog.InstanceID = commerceID()
		put("relay_v2_catalogs", id, catalog)
	case "active-route":
		put("routes", "reintroduced", Route{ID: "reintroduced", EntryAgentID: id})
	case "active-rule":
		archive, _ := LoadDoc[UserRule](s, "relay_rule_archive", f.ruleID)
		put("user_rules", archive.UserID+":"+archive.RouteID, archive)
	case "archive-missing":
		DeleteDoc(s, "relay_rule_archive", f.ruleID)
	case "archive-not-stopped", "archive-id", "archive-future":
		archive, _ := LoadDoc[UserRule](s, "relay_rule_archive", f.ruleID)
		for i := range archive.Segments {
			if archive.Segments[i].AgentID == id {
				if mutation == "archive-not-stopped" {
					archive.Segments[i].StopConfirmed = false
				} else if mutation == "archive-id" {
					archive.Segments[i].Runtime.ID = commerceID()
				} else {
					archive.Segments[i].AckAt = time.Now().Add(time.Hour).UnixMilli()
					archive.Segments[i].RuntimeObservedAt = archive.Segments[i].AckAt
				}
			}
		}
		put("relay_rule_archive", f.ruleID, archive)
	case "bad-history":
		s.Docs["relay_v2_command_history"][commerceID()] = json.RawMessage(`[]`)
	case "recovery", "accounting":
		agent, _ := LoadDoc[RelayAgent](s, "relay_agents", id)
		if mutation == "recovery" {
			agent.ReconcileState = "recovery_required"
		} else {
			agent.AccountingDegraded = true
		}
		put("relay_agents", id, agent)
	default:
		t.Fatalf("unknown mutation %s", mutation)
	}
}
