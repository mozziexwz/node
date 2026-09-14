package control

import (
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/mozziexwz/node/internal/relayruntime"
)

func TestRelayTLSMaintenanceLeavesOtherRuleAndCommandBytesUnchanged(t *testing.T) {
	f := tlsMaintenanceFixture(t)
	otherID, otherRouteID := commerceID(), "independent-tls-rule-route"
	if err := f.app.Store.Update(func(s *State) error {
		route, _ := LoadDoc[Route](s, "routes", "route")
		route.ID = otherRouteID
		if err := SaveDoc(s, "routes", route.ID, route); err != nil {
			return err
		}
		row, _ := LoadDoc[UserRule](s, "user_rules", f.user.ID+":route")
		row.ID, row.RouteID = otherID, route.ID
		for i := range row.Segments {
			row.Segments[i].Runtime.ID = row.ID
			row.Segments[i].Runtime.ListenPort += 100
		}
		row.Segments[0].Runtime.Targets = []string{net.JoinHostPort(f.agents[1].Address, strconv.Itoa(row.Segments[1].Runtime.ListenPort))}
		if err := f.app.configureRelayTLS(&row, []RouteStage{{AgentIDs: []string{f.agents[0].ID}, Protocol: "tcp", Strategy: "round"}, route.Exit}, 0); err != nil {
			return err
		}
		for i := range row.Segments {
			agent, _ := LoadDoc[RelayAgent](s, "relay_agents", row.Segments[i].AgentID)
			catalog, _ := LoadDoc[RelayV2Catalog](s, "relay_v2_catalogs", agent.ID)
			if err := f.app.relayV2Issue(s, &catalog, agent, &row, i, "upsert", "authorized"); err != nil {
				return err
			}
			if err := SaveDoc(s, "relay_v2_catalogs", agent.ID, catalog); err != nil {
				return err
			}
		}
		return SaveDoc(s, "user_rules", row.UserID+":"+row.RouteID, row)
	}); err != nil {
		t.Fatal(err)
	}
	for index, agent := range f.agents {
		acks := []relayruntime.V2Ack{}
		if err := f.app.Store.View(func(s *State) error {
			for _, command := range ListDocs[RelayV2Command](s, "relay_v2_commands") {
				if command.AgentID == agent.ID {
					wire, err := f.app.recoveryWireCommand(command)
					if err != nil {
						return err
					}
					acks = append(acks, v2Ready(wire))
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		f.sync(t, index, acks...)
	}
	var report RelayTLSMaintenanceReport
	before := map[string]string{}
	if err := f.app.Store.View(func(s *State) error {
		var err error
		report, err = f.app.relayTLSMaintenanceReport(s, f.ruleID)
		before["rule"] = string(s.Docs["user_rules"][f.user.ID+":"+otherRouteID])
		for _, agent := range f.agents {
			before[agent.ID] = string(s.Docs["relay_v2_commands"][agent.ID+":"+otherID])
		}
		if preflight := backupPausePreflight(s, time.Now().UnixMilli()); !preflight.CanPauseControl {
			t.Fatalf("independent-rule fixture not ready: %+v", preflight)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.rotateRelayTLS(RelayTLSMaintenanceRequest{RuleID: f.ruleID, Fingerprint: report.Fingerprint, Confirmation: "ROTATE_TLS_INTERRUPTS_RULE " + f.ruleID}, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := f.app.Store.View(func(s *State) error {
		if string(s.Docs["user_rules"][f.user.ID+":"+otherRouteID]) != before["rule"] {
			t.Fatal("certificate maintenance changed an unrelated rule")
		}
		for _, agent := range f.agents {
			if string(s.Docs["relay_v2_commands"][agent.ID+":"+otherID]) != before[agent.ID] {
				t.Fatal("certificate maintenance reissued an unrelated command")
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRelayTLSMaintenanceReviewSurvivesUnchangedHeartbeat(t *testing.T) {
	f := tlsMaintenanceFixture(t)
	var report RelayTLSMaintenanceReport
	if err := f.app.Store.View(func(s *State) error {
		var err error
		report, err = f.app.relayTLSMaintenanceReport(s, f.ruleID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	for index, agent := range f.agents {
		var ack relayruntime.V2Ack
		if err := f.app.Store.View(func(s *State) error {
			command, _ := LoadDoc[RelayV2Command](s, "relay_v2_commands", agent.ID+":"+f.ruleID)
			wire, err := f.app.recoveryWireCommand(command)
			ack = v2Ready(wire)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		f.sync(t, index, ack)
	}
	if _, err := f.app.rotateRelayTLS(RelayTLSMaintenanceRequest{RuleID: f.ruleID, Fingerprint: report.Fingerprint, Confirmation: "ROTATE_TLS_INTERRUPTS_RULE " + f.ruleID}, time.Now().UnixMilli()); err != nil {
		t.Fatalf("unchanged healthy heartbeat invalidated the operator's configuration review: %v", err)
	}
}
