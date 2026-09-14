package control

import (
	"testing"
	"time"

	"github.com/mozziexwz/node/internal/relayruntime"
)

// Two customers share both physical nodes. Reconnection after a changed
// entitlement must issue a stop for only the affected customer's rule, not
// regenerate the healthy customer's same-hash configuration or identity.
func TestRelayV2ReconnectedPolicyStopsOnlyAffectedCustomer(t *testing.T) {
	for _, policy := range []string{"expired", "quota", "suspended"} {
		t.Run(policy, func(t *testing.T) {
			f := newRelayV2Fixture(t)
			firstCommands := f.ready(t)
			otherID, otherRuleID := commerceID(), commerceID()
			if err := f.app.Store.Update(func(s *State) error {
				other := *s.Users[f.user.ID]
				other.ID, other.Email = otherID, "isolated-healthy@example.test"
				s.Users[other.ID] = &other
				row, _ := LoadDoc[UserRule](s, "user_rules", f.user.ID+":route")
				row.ID, row.UserID = otherRuleID, otherID
				for i := range row.Segments {
					row.Segments[i].Runtime.ID = otherRuleID
					row.Segments[i].Runtime.ListenPort += 100
					agent, _ := LoadDoc[RelayAgent](s, "relay_agents", row.Segments[i].AgentID)
					catalog, _ := LoadDoc[RelayV2Catalog](s, "relay_v2_catalogs", agent.ID)
					if err := f.app.relayV2Issue(s, &catalog, agent, &row, i, "upsert", "authorized"); err != nil {
						return err
					}
					if err := SaveDoc(s, "relay_v2_catalogs", agent.ID, catalog); err != nil {
						return err
					}
				}
				return SaveDoc(s, "user_rules", otherID+":route", row)
			}); err != nil {
				t.Fatal(err)
			}
			otherCommands := make([]relayruntime.V2Command, len(f.agents))
			before := make([]RelayV2Command, len(f.agents))
			for i := range f.agents {
				if err := f.app.Store.View(func(s *State) error {
					command, _ := LoadDoc[RelayV2Command](s, "relay_v2_commands", f.agents[i].ID+":"+otherRuleID)
					var err error
					otherCommands[i], err = f.app.recoveryWireCommand(command)
					before[i] = command
					return err
				}); err != nil {
					t.Fatal(err)
				}
				f.sync(t, i, v2Ready(firstCommands[i]), v2Ready(otherCommands[i]))
			}
			if err := f.app.Store.Update(func(s *State) error {
				u := s.Users[f.user.ID]
				switch policy {
				case "expired":
					u.ExpiresAt = time.Now().UnixMilli() - 1
				case "quota":
					u.TrafficUsed = u.TrafficTotal
				case "suspended":
					u.Status = "suspended"
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			for i := range f.agents {
				out := f.sync(t, i, v2Ready(firstCommands[i]), v2Ready(otherCommands[i]))
				want := "pause"
				if policy == "expired" {
					want = "revoke"
				}
				if out.Status != "ready" || len(out.Commands) != 1 || out.Commands[0].RuleID != f.ruleID || out.Commands[0].Action != want {
					t.Fatal("changed policy did not issue only the affected customer's explicit stop")
				}
				if err := f.app.Store.View(func(s *State) error {
					current, _ := LoadDoc[RelayV2Command](s, "relay_v2_commands", f.agents[i].ID+":"+otherRuleID)
					if !backupPauseSameIntent(before[i], current) || before[i].SealedRule != current.SealedRule || current.AckState != "ready" {
						t.Fatal("unrelated customer's configuration was replaced or stopped")
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
