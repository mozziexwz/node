package control

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mozziexwz/node/internal/relayruntime"
)

func restoreRelayFixture(t *testing.T, s *State, userID string, version int) UserRule {
	t.Helper()
	now := time.Now().UnixMilli()
	s.Settings["maintenance"] = true
	agent := RelayAgent{ID: "old-segment-node", ProtocolVersion: version, Enabled: true, Online: true,
		TokenHash: "private-token-hash", EnrollmentHash: "private-enrollment", EnrollmentExpires: now + 60000,
		BootID: "old-agent-instance-0001", LastSeen: now, PortRanges: []PortRange{{Start: 24000, End: 24001}}}
	segment := RelaySegment{AgentID: agent.ID, LastLease: now - 60000, AckState: "ready", AckAt: now - 1000,
		Runtime:      relayruntime.Rule{ID: "preserved-rule-instance", Version: 7, ListenPort: 24000, Targets: []string{"8.8.4.4:443"}, Protocol: "tcp", RateMbps: 10},
		SealedTLSKey: "preserved-sealed-key"}
	if version == 2 {
		agent.OfflinePolicy, agent.KeepLastConfirmed = "keep_last", true
		agent.Capabilities = append([]string(nil), relayruntime.V2Capabilities...)
		segment.ProtocolVersion, segment.ConfigGeneration, segment.AppliedGeneration = 2, 5, 4
		segment.LastCommandID, segment.LastCommandAction, segment.RuntimeHash = "pending-revoke-command", "revoke", "preserved-runtime-hash"
		segment.EverReady, segment.RuntimeObservedAt = true, now-1000
	}
	rule := UserRule{ID: "preserved-rule-instance", UserID: userID, RouteID: "current-route", Version: 7, State: "revoking",
		TargetHash: "preserved-target-hash", TargetHost: "8.8.4.4", TargetPort: 443, EntryPort: 24000,
		DeleteAfter: now - 1000, SealedConfig: "preserved-sealed-config", Segments: []RelaySegment{segment}}
	rows := []struct {
		collection, id string
		value          any
	}{
		{"relay_agents", agent.ID, agent},
		{"relay_agents", "current-route-node", RelayAgent{ID: "current-route-node", Enabled: true}},
		{"routes", rule.RouteID, Route{ID: rule.RouteID, EntryAgentID: "current-route-node", Enabled: true}},
		{"user_rules", userID + ":" + rule.RouteID, rule},
		{"user_targets", userID, UserTarget{Hash: rule.TargetHash, Host: rule.TargetHost, Port: rule.TargetPort, ResetUntil: now - 1000}},
	}
	if version == 2 {
		command := RelayV2Command{AgentID: agent.ID, RuleID: rule.ID, CommandID: segment.LastCommandID, Generation: 5, Revision: 6,
			Action: "revoke", RuntimeHash: segment.RuntimeHash, AckState: "pending", SealedRule: "private-command-payload", BillingPeriodID: "historical-period"}
		rows = append(rows,
			struct {
				collection, id string
				value          any
			}{"relay_v2_control", "default", RelayV2Control{Epoch: "old-control-epoch-0001"}},
			struct {
				collection, id string
				value          any
			}{"relay_v2_catalogs", agent.ID, RelayV2Catalog{AgentID: agent.ID, ControlEpoch: "old-control-epoch-0001", Revision: 6, DispatchCursor: 3}},
			struct {
				collection, id string
				value          any
			}{"relay_v2_commands", agent.ID + ":" + rule.ID, command},
			struct {
				collection, id string
				value          any
			}{"relay_v2_command_history", command.CommandID, command})
	}
	for _, row := range rows {
		if err := SaveDoc(s, row.collection, row.id, row.value); err != nil {
			t.Fatal(err)
		}
	}
	return rule
}

func assertRestoredRelayFrozen(t *testing.T, s *State, original UserRule) {
	t.Helper()
	rule, ok := LoadDoc[UserRule](s, "user_rules", original.UserID+":"+original.RouteID)
	want := original
	want.ReconcileState = "recovery_required"
	if !ok || !reflect.DeepEqual(rule, want) {
		t.Fatal("restoration discarded resource identity or changed pending revocation intent")
	}
	control, _ := LoadDoc[RelayV2Control](s, "relay_v2_control", "default")
	if !control.RecoveryRequired || control.Epoch == "" || control.Epoch == "old-control-epoch-0001" {
		t.Fatal("restoration did not create an isolated frozen control identity")
	}
	agent, _ := LoadDoc[RelayAgent](s, "relay_agents", original.Segments[0].AgentID)
	if agent.Enabled || agent.Online || agent.TokenHash != "" || agent.EnrollmentHash != "" || agent.EnrollmentExpires != 0 || agent.LastSeen != 0 || agent.KeepLastConfirmed {
		t.Fatal("restored management identity remains authorized")
	}
	if agent.BootID != "old-agent-instance-0001" || agent.ReconcileState != "recovery_required" {
		t.Fatal("old installation identity lost or automatically trusted")
	}
	route, _ := LoadDoc[Route](s, "routes", original.RouteID)
	if route.Enabled || route.Online || !boolSetting(s, "maintenance") || len(s.Sessions) != 0 {
		t.Fatal("restoration isolation was weakened")
	}
	beforeRule, _ := json.Marshal(rule)
	beforeTarget := append([]byte(nil), s.Docs["user_targets"][original.UserID]...)
	if err := relayCleanup(s, time.Now().Add(30*24*time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	afterRule, _ := LoadDoc[UserRule](s, "user_rules", original.UserID+":"+original.RouteID)
	afterRaw, _ := json.Marshal(afterRule)
	if string(beforeRule) != string(afterRaw) || string(beforeTarget) != string(s.Docs["user_targets"][original.UserID]) {
		t.Fatal("cleanup released frozen resources on a timer")
	}
	port, err := relayReservePort(s, agent)
	if err != nil || port != 24001 {
		t.Fatal("frozen port became allocatable despite no trusted stop acknowledgement")
	}
}

func TestSafeRestoreKeepLastPreservesCurrentResourcesCommandsAndFinance(t *testing.T) {
	a, _, u := commerceTestApp(t)
	b := NewBackupService(a)
	snapshot := backupSnapshot(t, a, b)
	var original UserRule
	var preserved map[string]map[string]json.RawMessage
	if err := a.Store.Update(func(s *State) error {
		original = restoreRelayFixture(t, s, u.ID, 2)
		s.Users[u.ID].BalanceCents = 12345
		s.Sessions["prior-session"] = &Session{UserID: u.ID}
		for _, name := range []string{"relay_v2_billing_grants", "relay_v2_traffic_cursors", "relay_v2_billing_periods", "relay_v2_traffic_review"} {
			if err := SaveDoc(s, name, "retained", map[string]any{"sequence": 7, "userId": u.ID}); err != nil {
				return err
			}
		}
		cloned, err := cloneState(s)
		if err != nil {
			return err
		}
		preserved = cloned.Docs
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.Store.View(func(s *State) error {
		old, err := b.unpack(snapshot)
		if err != nil {
			return err
		}
		_, report, err := mergeSafeRestore(s, old)
		if err != nil {
			return err
		}
		if !report.RecoveryRequired || len(report.RecoveryRules) != 1 || len(report.Warnings) == 0 || !strings.Contains(strings.Join(report.PreservedNodes, ","), "old-segment-node") {
			t.Fatal("preflight omitted preserved old-topology node or recovery warning")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	response := backupRestoreRequest(t, b, &User{ID: "admin-context", Role: "admin", Status: "active"}, snapshot)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"recoveryRequired":true`) || !strings.Contains(response.Body.String(), "recovery_required") {
		t.Fatalf("safe restore did not report required recovery: %d", response.Code)
	}
	if err := a.Store.Update(func(s *State) error {
		assertRestoredRelayFrozen(t, s, original)
		if s.Users[u.ID].BalanceCents != 12345 {
			t.Fatal("safe restore rewound current money")
		}
		for _, name := range []string{"relay_v2_commands", "relay_v2_command_history", "relay_v2_billing_grants", "relay_v2_traffic_cursors", "relay_v2_billing_periods", "relay_v2_traffic_review"} {
			if !reflect.DeepEqual(s.Docs[name], preserved[name]) {
				t.Fatalf("safe restore replaced current %s", name)
			}
		}
		catalog, _ := LoadDoc[RelayV2Catalog](s, "relay_v2_catalogs", "old-segment-node")
		if catalog.ReconcileState != "recovery_required" || catalog.ControlEpoch != "old-control-epoch-0001" || catalog.Revision != 6 || catalog.DispatchCursor != 3 {
			t.Fatal("catalog reconciliation evidence lost")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestOfflineRestoreOldV1SnapshotCannotProveCurrentNodeStopped(t *testing.T) {
	a, _, u := commerceTestApp(t)
	b := NewBackupService(a)
	var original UserRule
	if err := a.Store.Update(func(s *State) error { original = restoreRelayFixture(t, s, u.ID, 1); return nil }); err != nil {
		t.Fatal(err)
	}
	parent := t.TempDir()
	input, output := filepath.Join(parent, "old-v1.msb"), filepath.Join(parent, "new-isolated")
	if err := os.WriteFile(input, backupSnapshot(t, a, b), 0600); err != nil {
		t.Fatal(err)
	}
	key, err := masterKey(a.Config)
	if err != nil {
		t.Fatal(err)
	}
	result, err := OfflineRestore(OfflineRestoreOptions{BackupPath: input, MasterKey: hex.EncodeToString(key), SQLiteOutputDir: output})
	if err != nil {
		t.Fatal(err)
	}
	if !result.RecoveryRequired || !strings.Contains(result.Message, "recovery_required") {
		t.Fatal("old snapshot was presented as already taken over")
	}
	recovered, err := openStore(Config{DataDir: output})
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if err := recovered.Update(func(s *State) error { assertRestoredRelayFrozen(t, s, original); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := a.Store.View(func(s *State) error {
		old, _ := LoadDoc[UserRule](s, "user_rules", u.ID+":"+original.RouteID)
		if !reflect.DeepEqual(old, original) || restoredRelayRecoveryRequired(s) {
			t.Fatal("offline restore changed the original database")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreRecoveryEvidenceDoesNotRequireFreshCapabilities(t *testing.T) {
	for _, evidence := range []string{"segment", "policy", "command", "history", "catalog", "already-frozen"} {
		t.Run(evidence, func(t *testing.T) {
			s := newState()
			rule := restoreRelayFixture(t, s, "user", 1)
			agent, _ := LoadDoc[RelayAgent](s, "relay_agents", "old-segment-node")
			switch evidence {
			case "segment":
				rule.Segments[0].ProtocolVersion = 2
			case "policy":
				agent.OfflinePolicy = "keep_last"
			case "command", "history":
				collection := "relay_v2_commands"
				if evidence == "history" {
					collection = "relay_v2_command_history"
				}
				if err := SaveDoc(s, collection, "evidence", RelayV2Command{AgentID: agent.ID, RuleID: rule.ID, Action: "revoke"}); err != nil {
					t.Fatal(err)
				}
			case "catalog":
				if err := SaveDoc(s, "relay_v2_catalogs", agent.ID, RelayV2Catalog{AgentID: agent.ID, ControlEpoch: "old-control-epoch-0001"}); err != nil {
					t.Fatal(err)
				}
			case "already-frozen":
				rule.ReconcileState = "recovery_required"
			}
			if err := SaveDoc(s, "user_rules", "user:"+rule.RouteID, rule); err != nil {
				t.Fatal(err)
			}
			if err := SaveDoc(s, "relay_agents", agent.ID, agent); err != nil {
				t.Fatal(err)
			}
			if err := isolateRestoredState(s, false); err != nil {
				t.Fatal(err)
			}
			assertRestoredRelayFrozen(t, s, rule)
		})
	}
}

func TestRestorePreflightBindsV2ResourceIdentityButNotObservations(t *testing.T) {
	s := newState()
	rule := restoreRelayFixture(t, s, "user", 2)
	before, err := restoreScopeFingerprint(s)
	if err != nil {
		t.Fatal(err)
	}
	rule.TrafficBytes, rule.Segments[0].AckAt, rule.Segments[0].RuntimeObservedAt = 999, 999, 999
	rule.Segments[0].AppliedGeneration, rule.Segments[0].StopConfirmed = 5, true
	if err := SaveDoc(s, "user_rules", "user:"+rule.RouteID, rule); err != nil {
		t.Fatal(err)
	}
	after, err := restoreScopeFingerprint(s)
	if err != nil || before != after {
		t.Fatal("observations invalidated preflight")
	}
	rule.Segments[0].Runtime.ListenPort++
	if err := SaveDoc(s, "user_rules", "user:"+rule.RouteID, rule); err != nil {
		t.Fatal(err)
	}
	changed, _ := restoreScopeFingerprint(s)
	if changed == before {
		t.Fatal("v2 retained port change did not invalidate preflight")
	}
}

func TestRestoreIdleDoesNotTreatV2LeaseAsStopProof(t *testing.T) {
	s := newState()
	rule := restoreRelayFixture(t, s, "user", 2)
	rule.Segments[0].LastLease = time.Now().Add(time.Hour).UnixMilli()
	if err := SaveDoc(s, "user_rules", "user:"+rule.RouteID, rule); err != nil {
		t.Fatal(err)
	}
	if err := validateRestoreIdle(s); err != nil {
		t.Fatal("obsolete v2 lease blocked frozen recovery")
	}
	s.Settings["maintenance"] = false
	if err := validateRestoreIdle(s); err == nil {
		t.Fatal("recovery bypassed maintenance")
	}
	s.Settings["maintenance"] = true
	if err := SaveDoc(s, "tasks", "active", map[string]any{"state": "running"}); err != nil {
		t.Fatal(err)
	}
	if err := validateRestoreIdle(s); err == nil {
		t.Fatal("recovery bypassed active task protection")
	}
}

func TestRestoreReferencesRequireKeptSegmentNode(t *testing.T) {
	s := newState()
	restoreRelayFixture(t, s, "user", 2)
	DeleteDoc(s, "relay_agents", "old-segment-node")
	if err := validateRestoreReferences(s); err == nil {
		t.Fatal("orphaned retained resource accepted")
	}
}

func TestRestorePreflightWarnsForRecoveredDeletedKeepLastNode(t *testing.T) {
	current, snapshot := newState(), newState()
	if err := SaveDoc(snapshot, "relay_agents", "deleted-node", RelayAgent{ID: "deleted-node", ProtocolVersion: 2, OfflinePolicy: "keep_last"}); err != nil {
		t.Fatal(err)
	}
	merged, report, err := mergeSafeRestore(current, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !report.RecoveryRequired || len(report.Warnings) == 0 {
		t.Fatal("preflight omitted recovery warning for a recovered node")
	}
	if err := isolateRestoredState(merged, false); err != nil {
		t.Fatal(err)
	}
	if !restoredRelayRecoveryRequired(merged) {
		t.Fatal("recovered node was treated as already taken over")
	}
}
