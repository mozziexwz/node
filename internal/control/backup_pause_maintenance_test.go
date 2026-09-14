package control

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func backupPauseLegacyFixture(t *testing.T, allLegacy bool) (*State, int64, string) {
	t.Helper()
	s, now, key := backupPauseFixture(t)
	rule, _ := LoadDoc[UserRule](s, "user_rules", key)
	for i := range rule.Segments {
		if i > 0 && !allLegacy {
			continue
		}
		seg := &rule.Segments[i]
		seg.ProtocolVersion = 1
		seg.ConfigGeneration, seg.AppliedGeneration = 0, 0
		seg.LastCommandID, seg.LastCommandAction, seg.RuntimeHash = "", "", ""
		agent, _ := LoadDoc[RelayAgent](s, "relay_agents", seg.AgentID)
		agent.ProtocolVersion, agent.OfflinePolicy, agent.KeepLastConfirmed = 1, "lease", false
		agent.Capabilities = nil
		backupPauseSave(t, s, "relay_agents", agent.ID, agent)
		DeleteDoc(s, "relay_v2_catalogs", agent.ID)
		DeleteDoc(s, "relay_v2_commands", agent.ID+":"+rule.ID)
		for historyKey := range s.Docs["relay_v2_command_history"] {
			command, _ := LoadDoc[RelayV2Command](s, "relay_v2_command_history", historyKey)
			if command.AgentID == agent.ID {
				DeleteDoc(s, "relay_v2_command_history", historyKey)
			}
		}
	}
	backupPauseSave(t, s, "user_rules", key, rule)
	return s, now, key
}

func TestBackupPauseMaintenanceLegacyAndMixedAreExplicit(t *testing.T) {
	for _, allLegacy := range []bool{true, false} {
		name := "mixed"
		if allLegacy {
			name = "legacy"
		}
		t.Run(name, func(t *testing.T) {
			s, now, _ := backupPauseLegacyFixture(t, allLegacy)
			report := backupPausePreflight(s, now)
			if report.CanPauseControl || !report.CanPauseMaintenance || !backupPauseHasCode(report, "offline_unsupported") {
				t.Fatalf("wrong maintenance classification: %+v", report)
			}
			store := backupPauseTestStore(t)
			if err := store.Update(func(target *State) error { *target = *s; return nil }); err != nil {
				t.Fatal(err)
			}
			token := strings.Repeat("ab", 32)
			if _, err := backupPauseAcquire(store, token, now); !errors.Is(err, errBackupPauseBlocked) {
				t.Fatal("strict/automatic path waived continuity", err)
			}
			result, err := backupPauseAcquireMode(store, token, now, "maintenance")
			if err != nil || !result.GateActive || result.CanPauseControl || !result.CanPauseMaintenance || result.Mode != "maintenance" || len(result.AcceptedRisks) != 1 || result.AcceptedRisks[0] != "offline_unsupported" {
				t.Fatal(result, err)
			}
			encoded, _ := json.Marshal(result)
			if bytes.Contains(encoded, []byte(token)) || bytes.Contains(encoded, []byte(tokenHash(token))) {
				t.Fatal("maintenance response leaked owner proof")
			}
			if err := store.Update(func(*State) error { t.Error("maintenance gate allowed concurrent write"); return nil }); !errors.Is(err, ErrBackupPauseActive) {
				t.Fatal(err)
			}
			if _, err = backupPauseAcquire(store, token, now); !errors.Is(err, errBackupPauseMode) {
				t.Fatal("strict retry hid maintenance risk", err)
			}
			if _, err = backupPauseAcquireMode(store, token, now+24*60*60*1000, "maintenance"); err != nil {
				t.Fatal("same mode lost-response retry failed", err)
			}
			status, err := backupPauseStatus(store, now)
			if err != nil || status.CanPauseControl || status.CanPauseMaintenance || !status.GateActive || status.Mode != "maintenance" {
				t.Fatal(status, err)
			}
			if _, err = backupPauseRelease(store, token); err != nil {
				t.Fatal(err)
			}
			if err = store.View(func(after *State) error {
				beforeRules, _ := json.Marshal(s.Docs["user_rules"])
				afterRules, _ := json.Marshal(after.Docs["user_rules"])
				if !bytes.Equal(beforeRules, afterRules) || len(after.Docs["content_audit"]) < 2 || backupPauseGatePresent(after) {
					t.Error("maintenance changed forwarding data or lost audit")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestBackupPauseMaintenanceNeverWaivesHardBlockers(t *testing.T) {
	base, now, key := backupPauseLegacyFixture(t, false)
	for _, name := range []string{"task-running", "task-unknown", "web-backup", "backup-unknown", "recovery", "rule-recovery", "agent-recovery", "accounting", "bad-json", "missing-agent", "missing-user", "missing-route", "bad-runtime", "v2-conflict", "unknown-rule", "missing-segments", "stale-v2", "pending-v2", "pending-legacy", "legacy-no-ack", "legacy-version", "legacy-stop-pending"} {
		t.Run(name, func(t *testing.T) {
			s, _ := cloneState(base)
			rule, _ := LoadDoc[UserRule](s, "user_rules", key)
			agent, _ := LoadDoc[RelayAgent](s, "relay_agents", rule.Segments[0].AgentID)
			switch name {
			case "task-running", "task-unknown":
				state := "running"
				if name == "task-unknown" {
					state = "unknown"
				}
				backupPauseSave(t, s, "tasks", "job", Task{ID: "job", State: state})
			case "web-backup":
				backupPauseSave(t, s, "backup_operations", "web", map[string]any{"state": "running"})
			case "backup-unknown":
				backupPauseSave(t, s, "backups", "unknown", BackupRecord{Status: "unknown"})
			case "recovery":
				backupPauseSave(t, s, "relay_v2_control", "default", RelayV2Control{Epoch: "recovery", RecoveryRequired: true})
			case "rule-recovery":
				rule.ReconcileState = "recovery_required"
				backupPauseSave(t, s, "user_rules", key, rule)
			case "agent-recovery":
				agent.ReconcileState = "recovery_required"
				backupPauseSave(t, s, "relay_agents", agent.ID, agent)
			case "accounting":
				agent.AccountingDegraded = true
				backupPauseSave(t, s, "relay_agents", agent.ID, agent)
			case "bad-json":
				s.Docs["relay_agents"][agent.ID] = json.RawMessage(`{"id":"bad","id":"duplicate"}`)
			case "missing-agent":
				DeleteDoc(s, "relay_agents", agent.ID)
			case "missing-user":
				delete(s.Users, rule.UserID)
			case "missing-route":
				DeleteDoc(s, "routes", rule.RouteID)
			case "bad-runtime":
				rule.Segments[0].Runtime.ListenPort = -1
				backupPauseSave(t, s, "user_rules", key, rule)
			case "v2-conflict":
				rule.Segments[0].ConfigGeneration = 5
				backupPauseSave(t, s, "user_rules", key, rule)
			case "unknown-rule":
				rule.State = "unknown"
				backupPauseSave(t, s, "user_rules", key, rule)
			case "missing-segments":
				rule.Segments = nil
				backupPauseSave(t, s, "user_rules", key, rule)
			case "stale-v2":
				other, _ := LoadDoc[RelayAgent](s, "relay_agents", rule.Segments[1].AgentID)
				other.LastSeen = now - backupPauseFreshnessMS
				backupPauseSave(t, s, "relay_agents", other.ID, other)
			case "pending-v2":
				rule.Segments[1].AckState = "persisted"
				backupPauseSave(t, s, "user_rules", key, rule)
			case "pending-legacy":
				rule.State = "pending"
				backupPauseSave(t, s, "user_rules", key, rule)
			case "legacy-no-ack":
				rule.Segments[0].AckAt = 0
				backupPauseSave(t, s, "user_rules", key, rule)
			case "legacy-version":
				rule.Segments[0].Runtime.Version++
				backupPauseSave(t, s, "user_rules", key, rule)
			case "legacy-stop-pending":
				agent.Enabled = false
				backupPauseSave(t, s, "relay_agents", agent.ID, agent)
			}
			report := backupPausePreflight(s, now)
			if report.CanPauseMaintenance {
				t.Fatalf("hard blocker waived: %+v", report)
			}
			store := backupPauseTestStore(t)
			if err := store.Update(func(target *State) error { *target = *s; return nil }); err != nil {
				t.Fatal(err)
			}
			if _, err := backupPauseAcquireMode(store, strings.Repeat("ab", 32), now, "maintenance"); !errors.Is(err, errBackupPauseBlocked) {
				t.Fatal("maintenance acquired unsafe state", err)
			}
			if err := store.View(func(after *State) error {
				if backupPauseGatePresent(after) {
					t.Error("rejected maintenance created gate")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
	if backupPauseMaintenanceRisk("future-new-blocker") || backupPauseMaintenanceRisk("") {
		t.Fatal("unknown safety check became bypassable")
	}
}

func TestBackupPauseMaintenanceConfirmationFrame(t *testing.T) {
	token := strings.Repeat("ab", 32)
	valid := token + "\n" + backupPauseMaintenanceConfirmation + "\n"
	if got, err := backupPauseReadMaintenanceToken(strings.NewReader(valid)); err != nil || got != token {
		t.Fatal(err)
	}
	for _, input := range []string{token + "\n", valid + "extra", strings.TrimSuffix(valid, "\n"), strings.ReplaceAll(valid, "\n", "\r\n"), strings.ReplaceAll(valid, backupPauseMaintenanceConfirmation, "YES"), strings.ToUpper(valid), strings.Repeat("a", 129)} {
		if _, err := backupPauseReadMaintenanceToken(strings.NewReader(input)); err == nil {
			t.Fatal("missing/incorrect/extra risk acknowledgement accepted")
		}
	}
	store := backupPauseTestStore(t)
	if _, err := backupPauseAcquireMode(store, token, 1, "force"); err == nil {
		t.Fatal("unknown mode accepted")
	}
	if _, err := backupPauseAcquire(store, token, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := backupPauseAcquireMode(store, token, 1, "maintenance"); !errors.Is(err, errBackupPauseMode) {
		t.Fatal("maintenance changed an existing strict gate")
	}
}
