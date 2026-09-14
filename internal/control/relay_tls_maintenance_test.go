package control

import (
	"strings"
	"testing"
	"time"
)

func tlsMaintenanceFixture(t *testing.T) *relayV2Fixture {
	t.Helper()
	f := newRelayV2Fixture(t)
	if err := f.app.Store.Update(func(s *State) error {
		route, _ := LoadDoc[Route](s, "routes", "route")
		route.Exit.Protocol = "tls"
		if err := SaveDoc(s, "routes", "route", route); err != nil {
			return err
		}
		row, _ := LoadDoc[UserRule](s, "user_rules", f.user.ID+":route")
		row.Segments[1].Runtime.Protocol = "tls"
		row.Segments[0].Runtime.Targets = []string{"8.8.4.4:20001"}
		stages := []RouteStage{{AgentIDs: []string{f.agents[0].ID}, Protocol: "tcp", Strategy: "round"}, route.Exit}
		if err := f.app.configureRelayTLS(&row, stages, 0); err != nil {
			return err
		}
		return SaveDoc(s, "user_rules", f.user.ID+":route", row)
	}); err != nil {
		t.Fatal(err)
	}
	f.ready(t)
	if err := f.app.Store.Update(func(s *State) error { s.Settings["maintenance"] = true; return nil }); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestRelayTLSExplicitMaintenanceAndExactRuleScope(t *testing.T) {
	f := tlsMaintenanceFixture(t)
	var report RelayTLSMaintenanceReport
	before := map[string]RelayV2Command{}
	if err := f.app.Store.View(func(s *State) error {
		r, e := f.app.relayTLSMaintenanceReport(s, f.ruleID)
		report = r
		for _, row := range ListDocs[RelayV2Command](s, "relay_v2_commands") {
			before[row.AgentID] = row
		}
		if p := backupPausePreflight(s, time.Now().UnixMilli()); !p.CanPauseControl {
			t.Fatalf("fixture not ready: %+v", p)
		}
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.rotateRelayTLS(RelayTLSMaintenanceRequest{RuleID: f.ruleID, Fingerprint: report.Fingerprint, Confirmation: "ROTATE_TLS_INTERRUPTS_RULE " + f.ruleID}, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := f.app.Store.View(func(s *State) error {
		for _, command := range ListDocs[RelayV2Command](s, "relay_v2_commands") {
			old := before[command.AgentID]
			if command.RuleID != f.ruleID || command.Generation != old.Generation+1 || command.RuntimeHash == old.RuntimeHash || command.Reason != "root_tls_maintenance" || command.AckState != "" {
				t.Fatalf("wrong explicit TLS command: %+v", command)
			}
			wire, e := f.app.recoveryWireCommand(command)
			if e != nil {
				return e
			}
			if wire.Rule.Protocol == "tls" && wire.Rule.TLSPrivateKey == "" {
				t.Fatal("new TLS identity lacks private key")
			}
			if wire.Rule.Protocol == "tcp" && len(wire.Rule.TargetTLS) != 1 {
				t.Fatal("upstream lost pinned TLS verification")
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// The same old review must never silently rotate twice.
	if _, err := f.app.rotateRelayTLS(RelayTLSMaintenanceRequest{RuleID: f.ruleID, Fingerprint: report.Fingerprint, Confirmation: "ROTATE_TLS_INTERRUPTS_RULE " + f.ruleID}, time.Now().UnixMilli()); err == nil {
		t.Fatal("stale rotation review accepted")
	}
}

func TestRelayTLSMaintenanceRejectsWithoutChangingIdentity(t *testing.T) {
	for _, name := range []string{"confirmation", "fingerprint", "maintenance_off", "recovery", "legacy", "pending", "stale"} {
		t.Run(name, func(t *testing.T) {
			f := tlsMaintenanceFixture(t)
			var report RelayTLSMaintenanceReport
			if err := f.app.Store.View(func(s *State) error { r, e := f.app.relayTLSMaintenanceReport(s, f.ruleID); report = r; return e }); err != nil {
				t.Fatal(err)
			}
			in := RelayTLSMaintenanceRequest{RuleID: f.ruleID, Fingerprint: report.Fingerprint, Confirmation: "ROTATE_TLS_INTERRUPTS_RULE " + f.ruleID}
			switch name {
			case "confirmation":
				in.Confirmation = "yes"
			case "fingerprint":
				in.Fingerprint = strings.Repeat("0", 64)
			default:
				if err := f.app.Store.Update(func(s *State) error {
					row, _ := LoadDoc[UserRule](s, "user_rules", f.user.ID+":route")
					switch name {
					case "maintenance_off":
						s.Settings["maintenance"] = false
					case "recovery":
						row.ReconcileState = "recovery_required"
					case "legacy":
						row.Segments[0].ProtocolVersion = 1
					case "pending":
						row.Segments[0].AckState = "pending"
					case "stale":
						a, _ := LoadDoc[RelayAgent](s, "relay_agents", f.agents[0].ID)
						a.LastSeen = 1
						return SaveDoc(s, "relay_agents", a.ID, a)
					}
					return SaveDoc(s, "user_rules", f.user.ID+":route", row)
				}); err != nil {
					t.Fatal(err)
				}
			}
			before := recoveryState(t, f.app)
			if _, err := f.app.rotateRelayTLS(in, time.Now().UnixMilli()); err == nil {
				t.Fatal("unsafe certificate rotation accepted")
			}
			if recoveryState(t, f.app) != before {
				t.Fatal("failed rotation changed certificate/state")
			}
		})
	}
}
