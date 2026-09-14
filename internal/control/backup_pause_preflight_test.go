package control

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func backupPauseFixture(t *testing.T) (*State, int64, string) {
	t.Helper()
	f := newRelayV2Fixture(t)
	f.ready(t)
	var snapshot *State
	if err := f.app.Store.View(func(s *State) error {
		var err error
		snapshot, err = cloneState(s)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return snapshot, time.Now().UnixMilli(), f.user.ID + ":route"
}

func backupPauseSave(t *testing.T, s *State, collection, key string, value any) {
	t.Helper()
	if err := SaveDoc(s, collection, key, value); err != nil {
		t.Fatal(err)
	}
}

func backupPauseHasCode(report BackupPauseReport, code string) bool {
	for _, blocker := range report.Blockers {
		if blocker.Code == code {
			return true
		}
	}
	return false
}

func TestBackupPausePreflightHealthyAndReadOnly(t *testing.T) {
	s, now, _ := backupPauseFixture(t)
	before, _ := json.Marshal(s)
	for i := 0; i < 3; i++ {
		report := backupPausePreflight(s, now)
		if !report.CanPauseControl || report.CheckedRules != 1 || len(report.Blockers) != 0 {
			t.Fatalf("healthy v2 chain blocked: %+v", report)
		}
	}
	after, _ := json.Marshal(s)
	if !bytes.Equal(before, after) {
		t.Fatal("preflight mutated persistent state")
	}
	empty := backupPausePreflight(newState(), now)
	if !empty.CanPauseControl || empty.CheckedRules != 0 || empty.Blockers == nil {
		t.Fatalf("empty site requires no maintenance toggle: %+v", empty)
	}
}

func TestBackupPausePreflightRejectsUnsafeChains(t *testing.T) {
	base, now, ruleKey := backupPauseFixture(t)
	tests := []struct {
		name, code string
		mutate     func(*State, *UserRule, *RelayAgent, *RelayV2Catalog, *RelayV2Command)
	}{
		{"mixed v1 chain", "offline_unsupported", func(s *State, r *UserRule, a *RelayAgent, c *RelayV2Catalog, cmd *RelayV2Command) {
			r.Segments[0].ProtocolVersion = 1
		}},
		{"false capability claim", "offline_unconfirmed", func(s *State, r *UserRule, a *RelayAgent, c *RelayV2Catalog, cmd *RelayV2Command) {
			a.Capabilities = []string{"keep_last"}
		}},
		{"duplicate capability", "offline_unconfirmed", func(s *State, r *UserRule, a *RelayAgent, c *RelayV2Catalog, cmd *RelayV2Command) {
			a.Capabilities = append(a.Capabilities, "keep_last")
		}},
		{"unconfirmed persistence", "offline_unconfirmed", func(s *State, r *UserRule, a *RelayAgent, c *RelayV2Catalog, cmd *RelayV2Command) {
			a.KeepLastConfirmed = false
		}},
		{"v1 node flag", "offline_unconfirmed", func(s *State, r *UserRule, a *RelayAgent, c *RelayV2Catalog, cmd *RelayV2Command) {
			a.ProtocolVersion = 1
		}},
		{"lease node flag", "offline_unconfirmed", func(s *State, r *UserRule, a *RelayAgent, c *RelayV2Catalog, cmd *RelayV2Command) {
			a.OfflinePolicy = "lease"
		}},
		{"stale heartbeat boundary", "stale_observation", func(s *State, r *UserRule, a *RelayAgent, c *RelayV2Catalog, cmd *RelayV2Command) {
			a.LastSeen = now - backupPauseFreshnessMS
		}},
		{"future heartbeat", "stale_observation", func(s *State, r *UserRule, a *RelayAgent, c *RelayV2Catalog, cmd *RelayV2Command) {
			a.LastSeen = now + 1
		}},
		{"fresh heartbeat stale ready", "stale_observation", func(s *State, r *UserRule, a *RelayAgent, c *RelayV2Catalog, cmd *RelayV2Command) {
			r.Segments[0].AckAt = now - backupPauseFreshnessMS
		}},
		{"fresh heartbeat stale runtime", "stale_observation", func(s *State, r *UserRule, a *RelayAgent, c *RelayV2Catalog, cmd *RelayV2Command) {
			r.Segments[0].RuntimeObservedAt = now - backupPauseFreshnessMS
		}},
		{"stop before ACK", "intent_unconfirmed", func(s *State, r *UserRule, a *RelayAgent, c *RelayV2Catalog, cmd *RelayV2Command) {
			r.State = "revoking"
		}},
		{"disabled node running", "intent_unconfirmed", func(s *State, r *UserRule, a *RelayAgent, c *RelayV2Catalog, cmd *RelayV2Command) { a.Enabled = false }},
		{"wrong generation", "command_unconfirmed", func(s *State, r *UserRule, a *RelayAgent, c *RelayV2Catalog, cmd *RelayV2Command) {
			r.Segments[0].AppliedGeneration++
		}},
		{"wrong command", "command_unconfirmed", func(s *State, r *UserRule, a *RelayAgent, c *RelayV2Catalog, cmd *RelayV2Command) {
			r.Segments[0].LastCommandID = "another-command"
		}},
		{"wrong hash", "command_unconfirmed", func(s *State, r *UserRule, a *RelayAgent, c *RelayV2Catalog, cmd *RelayV2Command) {
			r.Segments[0].RuntimeHash = "changed"
		}},
		{"pending process", "command_unconfirmed", func(s *State, r *UserRule, a *RelayAgent, c *RelayV2Catalog, cmd *RelayV2Command) {
			r.Segments[0].AckState = "persisted"
		}},
		{"stale runtime field", "command_unconfirmed", func(s *State, r *UserRule, a *RelayAgent, c *RelayV2Catalog, cmd *RelayV2Command) {
			r.Segments[0].RuntimeState = "unknown"
		}},
		{"pending new rate", "policy_unconfirmed", func(s *State, r *UserRule, a *RelayAgent, c *RelayV2Catalog, cmd *RelayV2Command) {
			s.Users[r.UserID].RateMbps = 1
		}},
		{"pending renewal", "policy_unconfirmed", func(s *State, r *UserRule, a *RelayAgent, c *RelayV2Catalog, cmd *RelayV2Command) {
			backupPauseSave(t, s, "entitlement_versions", r.UserID, 5)
		}},
		{"changed target same stale hash", "config_unconfirmed", func(s *State, r *UserRule, a *RelayAgent, c *RelayV2Catalog, cmd *RelayV2Command) {
			r.Segments[0].Runtime.Targets = []string{"9.9.9.9:443"}
		}},
		{"candidate config error", "command_unconfirmed", func(s *State, r *UserRule, a *RelayAgent, c *RelayV2Catalog, cmd *RelayV2Command) {
			r.Segments[0].ConfigError = true
		}},
		{"accounting degraded", "accounting_degraded", func(s *State, r *UserRule, a *RelayAgent, c *RelayV2Catalog, cmd *RelayV2Command) {
			a.AccountingDegraded = true
		}},
		{"missing traffic grant", "accounting_unconfirmed", func(s *State, r *UserRule, a *RelayAgent, c *RelayV2Catalog, cmd *RelayV2Command) {
			DeleteDoc(s, "relay_v2_billing_grants", cmd.BillingPeriodID)
		}},
		{"node recovery", "recovery_required", func(s *State, r *UserRule, a *RelayAgent, c *RelayV2Catalog, cmd *RelayV2Command) {
			a.ReconcileState = "recovery_required"
		}},
		{"rule recovery", "reconcile_required", func(s *State, r *UserRule, a *RelayAgent, c *RelayV2Catalog, cmd *RelayV2Command) {
			r.ReconcileState = "recovery_required"
		}},
		{"catalog recovery", "recovery_required", func(s *State, r *UserRule, a *RelayAgent, c *RelayV2Catalog, cmd *RelayV2Command) {
			c.ReconcileState = "recovery_required"
		}},
		{"epoch mismatch", "recovery_required", func(s *State, r *UserRule, a *RelayAgent, c *RelayV2Catalog, cmd *RelayV2Command) {
			c.ControlEpoch = "other-control"
		}},
		{"missing process identity", "catalog_unconfirmed", func(s *State, r *UserRule, a *RelayAgent, c *RelayV2Catalog, cmd *RelayV2Command) { c.InstanceID = "" }},
		{"replayed process identity", "recovery_required", func(s *State, r *UserRule, a *RelayAgent, c *RelayV2Catalog, cmd *RelayV2Command) {
			backupPauseSave(t, s, "relay_v2_retired_instances", a.ID+":"+c.InstanceID, true)
		}},
		{"catalog behind command", "catalog_unconfirmed", func(s *State, r *UserRule, a *RelayAgent, c *RelayV2Catalog, cmd *RelayV2Command) { c.Revision = 0 }},
		{"unknown rule state", "unknown_rule_state", func(s *State, r *UserRule, a *RelayAgent, c *RelayV2Catalog, cmd *RelayV2Command) {
			r.State = "unknown"
		}},
		{"missing segments", "missing_segments", func(s *State, r *UserRule, a *RelayAgent, c *RelayV2Catalog, cmd *RelayV2Command) { r.Segments = nil }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, err := cloneState(base)
			if err != nil {
				t.Fatal(err)
			}
			rule, _ := LoadDoc[UserRule](s, "user_rules", ruleKey)
			agent, _ := LoadDoc[RelayAgent](s, "relay_agents", rule.Segments[0].AgentID)
			catalog, _ := LoadDoc[RelayV2Catalog](s, "relay_v2_catalogs", agent.ID)
			command, _ := LoadDoc[RelayV2Command](s, "relay_v2_commands", agent.ID+":"+rule.ID)
			tc.mutate(s, &rule, &agent, &catalog, &command)
			backupPauseSave(t, s, "user_rules", ruleKey, rule)
			backupPauseSave(t, s, "relay_agents", agent.ID, agent)
			backupPauseSave(t, s, "relay_v2_catalogs", agent.ID, catalog)
			backupPauseSave(t, s, "relay_v2_commands", agent.ID+":"+rule.ID, command)
			report := backupPausePreflight(s, now)
			if report.CanPauseControl || !backupPauseHasCode(report, tc.code) {
				t.Fatalf("unsafe chain accepted or wrong reason: %+v", report)
			}
		})
	}
}

func TestBackupPausePreflightExplicitStopsAndOrphans(t *testing.T) {
	s, now, ruleKey := backupPauseFixture(t)
	rule, _ := LoadDoc[UserRule](s, "user_rules", ruleKey)
	for i := range rule.Segments {
		seg := &rule.Segments[i]
		command, _ := LoadDoc[RelayV2Command](s, "relay_v2_commands", seg.AgentID+":"+rule.ID)
		command.CommandID += "-stop"
		command.Generation++
		command.Revision++
		command.Action, command.AckState, command.AppliedAt = "pause", "stopped", now
		seg.LastCommandID, seg.LastCommandAction = command.CommandID, command.Action
		seg.ConfigGeneration, seg.AppliedGeneration = command.Generation, command.Generation
		seg.StopConfirmed, seg.AckState, seg.RuntimeState = true, "stopped", "stopped"
		seg.AckAt, seg.RuntimeObservedAt = now, now
		backupPauseSave(t, s, "relay_v2_commands", seg.AgentID+":"+rule.ID, command)
		backupPauseSave(t, s, "relay_v2_command_history", command.CommandID, command)
		catalog, _ := LoadDoc[RelayV2Catalog](s, "relay_v2_catalogs", seg.AgentID)
		catalog.Revision = command.Revision
		backupPauseSave(t, s, "relay_v2_catalogs", seg.AgentID, catalog)
		agent, _ := LoadDoc[RelayAgent](s, "relay_agents", seg.AgentID)
		agent.LastSeen = now
		backupPauseSave(t, s, "relay_agents", agent.ID, agent)
	}
	rule.State = "paused"
	backupPauseSave(t, s, "user_rules", ruleKey, rule)
	if report := backupPausePreflight(s, now); !report.CanPauseControl {
		t.Fatalf("current exact persistent pause blocked: %+v", report)
	}
	if report := backupPausePreflight(s, now+backupPauseFreshnessMS); report.CanPauseControl {
		t.Fatal("nonterminal pause bypassed freshness")
	}
	DeleteDoc(s, "user_rules", ruleKey)
	if report := backupPausePreflight(s, now); report.CanPauseControl || !backupPauseHasCode(report, "orphan_command") {
		t.Fatal("orphan pause accepted")
	}
	rule.State = "revoking"
	for i := range rule.Segments {
		seg := &rule.Segments[i]
		command, _ := LoadDoc[RelayV2Command](s, "relay_v2_commands", seg.AgentID+":"+rule.ID)
		command.CommandID += "-revoke"
		command.Generation++
		command.Revision++
		command.Action = "revoke"
		seg.LastCommandID, seg.LastCommandAction = command.CommandID, command.Action
		seg.ConfigGeneration, seg.AppliedGeneration = command.Generation, command.Generation
		backupPauseSave(t, s, "relay_v2_commands", seg.AgentID+":"+rule.ID, command)
		backupPauseSave(t, s, "relay_v2_command_history", command.CommandID, command)
	}
	backupPauseSave(t, s, "user_rules", ruleKey, rule)
	if report := backupPausePreflight(s, now+backupPauseFreshnessMS); !report.CanPauseControl {
		t.Fatalf("terminal revoke requires no live capability: %+v", report)
	}
	DeleteDoc(s, "user_rules", ruleKey)
	if report := backupPausePreflight(s, now+backupPauseFreshnessMS); !report.CanPauseControl {
		t.Fatalf("retained terminal tombstones blocked: %+v", report)
	}
	for _, seg := range rule.Segments {
		DeleteDoc(s, "relay_agents", seg.AgentID)
	}
	if report := backupPausePreflight(s, now+backupPauseFreshnessMS); report.CanPauseControl {
		t.Fatal("unknown orphan identity bypassed explicit terminal retirement proof")
	}
	for key := range s.Docs["relay_v2_commands"] {
		DeleteDoc(s, "relay_v2_commands", key)
	}
	if report := backupPausePreflight(s, now); report.CanPauseControl || !backupPauseHasCode(report, "orphan_history") {
		t.Fatal("lost current command concealed retained history")
	}
}

func TestBackupPausePreflightRawCorruptionAndBusyOperations(t *testing.T) {
	now := time.Now().UnixMilli()
	for _, collection := range []string{"user_rules", "relay_agents", "relay_v2_catalogs", "relay_v2_commands", "relay_v2_command_history", "relay_v2_control", "tasks", "backups"} {
		for _, raw := range []string{"null", "{}", "[]", "{", `{"id":"ignored","id":"overwritten"}`, `{"id":"ignored","ID":"overwritten"}`} {
			t.Run(collection+"/"+raw, func(t *testing.T) {
				s := newState()
				s.Docs[collection] = map[string]json.RawMessage{"invalid": json.RawMessage(raw)}
				report := backupPausePreflight(s, now)
				if report.CanPauseControl {
					t.Fatalf("malformed raw %s was ignored: %+v", collection, report)
				}
			})
		}
	}
	for _, state := range []string{"queued", "running", "unknown", "interrupted", "executed", ""} {
		s := newState()
		backupPauseSave(t, s, "tasks", "task", Task{ID: "task", State: state})
		if report := backupPausePreflight(s, now); !backupPauseHasCode(report, "execution_busy") {
			t.Fatalf("unsafe task %q ignored: %+v", state, report)
		}
	}
	for _, raw := range []string{`{"id":"operation","kind":"site-backup","startedAt":1}`, "null", "{"} {
		s := newState()
		s.Docs["backup_operations"] = map[string]json.RawMessage{"operation": json.RawMessage(raw)}
		if report := backupPausePreflight(s, now); !backupPauseHasCode(report, "operation_in_progress") {
			t.Fatalf("crash marker ignored: %+v", report)
		}
	}
	for _, status := range []string{"verified", "partial", "failed", "local_missing", "local_removed"} {
		s := newState()
		backupPauseSave(t, s, "backups", "backup", BackupRecord{ID: "backup", Status: status})
		if report := backupPausePreflight(s, now); !report.CanPauseControl {
			t.Fatalf("completed backup status %q blocked: %+v", status, report)
		}
	}
	if report := backupPausePreflight(nil, now); report.CanPauseControl {
		t.Fatal("nil state accepted")
	}
}

func TestBackupPausePreflightLegacyTerminalStopOnly(t *testing.T) {
	s, now, ruleKey := backupPauseFixture(t)
	for _, collection := range []string{"relay_v2_control", "relay_v2_catalogs", "relay_v2_commands", "relay_v2_command_history"} {
		delete(s.Docs, collection)
	}
	rule, _ := LoadDoc[UserRule](s, "user_rules", ruleKey)
	rule.State = "revoking"
	for i := range rule.Segments {
		seg := &rule.Segments[i]
		seg.ProtocolVersion, seg.ConfigGeneration, seg.AppliedGeneration = 1, 0, 0
		seg.LastCommandID, seg.LastCommandAction, seg.RuntimeHash = "", "", ""
		seg.AckState, seg.AckAt, seg.LastLease = "stopped", now, now-1
		agent, _ := LoadDoc[RelayAgent](s, "relay_agents", seg.AgentID)
		agent.ProtocolVersion, agent.OfflinePolicy, agent.KeepLastConfirmed = 1, "lease", false
		agent.Capabilities, agent.ReconcileState = nil, ""
		backupPauseSave(t, s, "relay_agents", agent.ID, agent)
	}
	backupPauseSave(t, s, "user_rules", ruleKey, rule)
	if report := backupPausePreflight(s, now); !report.CanPauseControl {
		t.Fatalf("proven terminal v1 stop blocked: %+v", report)
	}
	rule.Segments[0].LastLease = now + 1
	backupPauseSave(t, s, "user_rules", ruleKey, rule)
	if report := backupPausePreflight(s, now); report.CanPauseControl {
		t.Fatal("live legacy lease accepted")
	}
	rule.Segments[0].LastLease = now - 1
	rule.Segments[0].AckState = "ready"
	backupPauseSave(t, s, "user_rules", ruleKey, rule)
	if report := backupPausePreflight(s, now); report.CanPauseControl {
		t.Fatal("expired lease substituted for explicit terminal stop ACK")
	}
}

func TestBackupPausePreflightNoSecretsAndDeterministicBlockers(t *testing.T) {
	s, now, ruleKey := backupPauseFixture(t)
	rule, _ := LoadDoc[UserRule](s, "user_rules", ruleKey)
	rule.TargetHost, rule.SealedConfig = "sensitive-target.example", "sensitive-sealed-rule"
	backupPauseSave(t, s, "user_rules", ruleKey, rule)
	for key, raw := range s.Docs["relay_agents"] {
		var agent RelayAgent
		_ = json.Unmarshal(raw, &agent)
		agent.TokenHash, agent.EnrollmentHash, agent.Address = "sensitive-token", "sensitive-enrollment", "sensitive-address"
		agent.AccountingDegraded = true
		backupPauseSave(t, s, "relay_agents", key, agent)
	}
	first, _ := json.Marshal(backupPausePreflight(s, now))
	if strings.Contains(string(first), "sensitive-") {
		t.Fatal("preflight exposed a credential, target or sealed payload")
	}
	for i := 0; i < 10; i++ {
		next, _ := json.Marshal(backupPausePreflight(s, now))
		if !bytes.Equal(first, next) {
			t.Fatal("nondeterministic report")
		}
	}
}
