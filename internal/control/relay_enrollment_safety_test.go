package control

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestRelayEnrollmentOnlyAllowsUnregisteredNodesWithoutBusiness(t *testing.T) {
	cases := []struct {
		name        string
		confirmedV2 bool
		business    string
		wantAllowed bool
	}{
		{"empty initial setup", false, "", true},
		{"confirmed v2 without routes", true, "", false},
		{"legacy entry route", false, "entry", false},
		{"legacy hop route", false, "hop", false},
		{"legacy exit route", false, "exit", false},
		{"legacy user rule", false, "rule", false},
		{"malformed business record", false, "malformed", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, mux, _ := commerceTestApp(t)
			admin := &User{ID: "admin", Role: "admin", Status: "active"}
			agent := RelayAgent{ID: "relay-safety", Name: "Safety test", Address: "198.51.100.10", Enabled: true, Capability: "relay", EnrollmentHash: commerceHash("old-enrollment"), EnrollmentExpires: time.Now().Add(time.Minute).UnixMilli()}
			if tc.confirmedV2 {
				agent.ProtocolVersion = 2
				agent.OfflinePolicy = "keep_last"
				agent.KeepLastConfirmed = true
				agent.TokenHash = commerceHash("old-management-token")
				agent.LastSeen = time.Now().UnixMilli()
			}
			if err := a.Store.Update(func(s *State) error {
				if err := SaveDoc(s, "relay_agents", agent.ID, agent); err != nil {
					return err
				}
				if tc.business == "rule" {
					return SaveDoc(s, "user_rules", "buyer:route", UserRule{ID: "rule", UserID: "buyer", RouteID: "route", Segments: []RelaySegment{{AgentID: agent.ID}}})
				}
				if tc.business == "malformed" {
					if s.Docs["routes"] == nil {
						s.Docs["routes"] = map[string]json.RawMessage{}
					}
					s.Docs["routes"]["broken"] = json.RawMessage(`{"id":"broken","entryAgentId":"other","unexpected":true}`)
					return nil
				}
				if tc.business != "" {
					route := Route{ID: "business-route", EntryAgentID: "other"}
					switch tc.business {
					case "entry":
						route.EntryAgentID = agent.ID
					case "hop":
						route.Hops = []RouteStage{{AgentIDs: []string{agent.ID}}}
					case "exit":
						route.Exit.AgentIDs = []string{agent.ID}
					}
					return SaveDoc(s, "routes", route.ID, route)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			list := commerceTestRequest(mux, admin, http.MethodGet, "/api/admin/relay-agents", nil)
			var listed struct {
				Agents []struct {
					ID                      string `json:"id"`
					EnrollmentAllowed       bool   `json:"enrollmentAllowed"`
					EnrollmentBlockedReason string `json:"enrollmentBlockedReason"`
				} `json:"agents"`
			}
			if list.Code != 200 || json.Unmarshal(list.Body.Bytes(), &listed) != nil || len(listed.Agents) != 1 || listed.Agents[0].EnrollmentAllowed != tc.wantAllowed {
				t.Fatalf("wrong enrollment eligibility: %d %s", list.Code, list.Body.String())
			}
			if !tc.wantAllowed && listed.Agents[0].EnrollmentBlockedReason == "" {
				t.Fatal("blocked node lacks recovery guidance")
			}
			rotate := commerceTestRequest(mux, admin, http.MethodPost, "/api/admin/relay-agents/"+agent.ID+"/enrollment", nil)
			if tc.wantAllowed {
				var out struct {
					EnrollmentToken string `json:"enrollmentToken"`
				}
				if rotate.Code != 200 || json.Unmarshal(rotate.Body.Bytes(), &out) != nil || out.EnrollmentToken == "" {
					t.Fatalf("initial setup rejected: %d %s", rotate.Code, rotate.Body.String())
				}
				registered := commerceTestRequest(mux, nil, http.MethodPost, "/api/relay-agent/register", map[string]string{"enrollmentToken": out.EnrollmentToken})
				if registered.Code != 200 {
					t.Fatalf("fresh registration rejected: %d %s", registered.Code, registered.Body.String())
				}
				return
			}
			if rotate.Code != 409 || strings.Contains(rotate.Body.String(), "enrollmentToken") {
				t.Fatalf("unsafe enrollment permitted: %d %s", rotate.Code, rotate.Body.String())
			}
			if err := a.Store.View(func(s *State) error {
				after, _ := LoadDoc[RelayAgent](s, "relay_agents", agent.ID)
				if after.TokenHash != agent.TokenHash || after.EnrollmentHash != agent.EnrollmentHash || after.EnrollmentExpires != agent.EnrollmentExpires || after.LastSeen != agent.LastSeen || after.ReconcileState != agent.ReconcileState || after.KeepLastConfirmed != agent.KeepLastConfirmed {
					t.Fatal("blocked enrollment changed node credentials or control state")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
