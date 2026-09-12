package control

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mozziexwz/node/internal/relayruntime"
)

func TestRelayEnrollmentInstructionsUseInstallerTokenFile(t *testing.T) {
	_, mux, _ := commerceTestApp(t)
	admin := &User{ID: "admin", Role: "admin", Status: "active"}
	w := commerceTestRequest(mux, admin, "POST", "/api/admin/relay-agents", RelayAgent{
		Name: "Relay test", Address: "8.8.8.8", Enabled: true, PortRanges: []PortRange{{Start: 20000, End: 20100}},
	})
	var created struct {
		Agent RelayAgent `json:"agent"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &created) != nil || created.Agent.ID == "" {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	responses := []*httptest.ResponseRecorder{w, commerceTestRequest(mux, admin, "POST", "/api/admin/relay-agents/"+created.Agent.ID+"/enrollment", map[string]any{})}
	for _, response := range responses {
		var out struct {
			EnrollmentToken string `json:"enrollmentToken"`
			InstallArgs     struct {
				Installer        string   `json:"installer"`
				Args             []string `json:"args"`
				TokenEnvironment string   `json:"tokenEnvironment"`
			} `json:"installArgs"`
		}
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &out) != nil {
			t.Fatalf("enrollment: %d %s", response.Code, response.Body.String())
		}
		args := strings.Join(out.InstallArgs.Args, " ")
		if out.EnrollmentToken == "" || out.InstallArgs.Installer != "deploy/install-agent.sh" || out.InstallArgs.TokenEnvironment != "MSBOOST_RELAY_ENROLLMENT_TOKEN" {
			t.Fatalf("invalid installation metadata: %s", response.Body.String())
		}
		if !strings.Contains(args, "--token-file /root/msboost-relay-token") || strings.Contains(args, "--enrollment-token") || strings.Contains(args, out.EnrollmentToken) {
			t.Fatalf("unsupported argument or credential in command: %s", args)
		}
	}
}

func relayTestSync(t *testing.T, mux http.Handler, token string, in relayruntime.SyncRequest) relayruntime.SyncResponse {
	t.Helper()
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/api/relay-agent/sync", bytes.NewReader(raw))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("sync %d %s", w.Code, w.Body.String())
	}
	var out relayruntime.SyncResponse
	if err = json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}
func TestRelayIngressWaitsForDownstreamAndInvalidatesRebootACK(t *testing.T) {
	a, mux, u := commerceTestApp(t)
	entryToken, exitToken := commerceID()+commerceID(), commerceID()+commerceID()
	now := time.Now().UnixMilli()
	ruleID := commerceID()
	err := a.Store.Update(func(s *State) error {
		s.Users[u.ID].ExpiresAt = now + commerceDay
		s.Users[u.ID].TrafficTotal = commerceGB
		for _, agent := range []RelayAgent{{ID: "entry", Address: "8.8.8.8", Enabled: true, Capability: "relay", TokenHash: commerceHash(entryToken), LastSeen: now}, {ID: "exit", Address: "8.8.4.4", Enabled: true, Capability: "relay", TokenHash: commerceHash(exitToken), LastSeen: now}} {
			if err := SaveDoc(s, "relay_agents", agent.ID, agent); err != nil {
				return err
			}
		}
		if err := SaveDoc(s, "routes", "route", Route{ID: "route", EntryAgentID: "entry", Enabled: true, RateMbps: 5, Exit: RouteStage{AgentIDs: []string{"exit"}, Protocol: "tcp", Strategy: "round"}}); err != nil {
			return err
		}
		rule := UserRule{ID: ruleID, UserID: u.ID, RouteID: "route", Version: 1, State: "pending", Segments: []RelaySegment{{AgentID: "entry", AckState: "pending", Runtime: relayruntime.Rule{ID: ruleID, Version: 1, ListenPort: 20000, Protocol: "tcp", RateMbps: 5, Billing: true}}, {AgentID: "exit", AckState: "pending", Runtime: relayruntime.Rule{ID: ruleID, Version: 1, ListenPort: 21000, Protocol: "tcp", RateMbps: 5}}}}
		return SaveDoc(s, "user_rules", u.ID+":route", rule)
	})
	if err != nil {
		t.Fatal(err)
	}
	entry := relayruntime.SyncRequest{BootID: "entry-boot-unique", Sequence: 1}
	exit := relayruntime.SyncRequest{BootID: "exit-boot-unique-1", Sequence: 1}
	if out := relayTestSync(t, mux, exitToken, exit); len(out.Rules) != 1 {
		t.Fatal("downstream not prepared")
	}
	if out := relayTestSync(t, mux, entryToken, entry); len(out.Rules) != 0 {
		t.Fatal("ingress activated before downstream bind ACK")
	}
	exit.Sequence++
	exit.Acks = []relayruntime.Ack{{ID: ruleID, Version: 1, State: "ready"}}
	relayTestSync(t, mux, exitToken, exit)
	entry.Sequence++
	if out := relayTestSync(t, mux, entryToken, entry); len(out.Rules) != 1 {
		t.Fatal("ready downstream did not activate ingress")
	}
	entry.Sequence++
	entry.Acks = []relayruntime.Ack{{ID: ruleID, Version: 1, State: "ready"}}
	relayTestSync(t, mux, entryToken, entry)
	exit.BootID = "exit-boot-unique-2"
	exit.Sequence = 1
	exit.Acks = nil
	relayTestSync(t, mux, exitToken, exit)
	entry.Sequence++
	if out := relayTestSync(t, mux, entryToken, entry); len(out.Rules) != 0 {
		t.Fatal("stale pre-reboot downstream ACK reactivated ingress")
	}
}
func TestRelayForceFrontOnlyUsesRoute(t *testing.T) {
	s := newState()
	route := Route{EntryAgentID: "agent", Exit: RouteStage{AgentIDs: []string{"agent"}}}
	agent := RelayAgent{ID: "agent", Enabled: true, LastSeen: time.Now().UnixMilli()}
	for _, tc := range []struct{ route, agent, want bool }{{false, false, false}, {true, false, true}, {false, true, false}, {true, true, true}} {
		route.RequireFront = tc.route
		agent.RequireFront = tc.agent
		if err := SaveDoc(s, "relay_agents", "agent", agent); err != nil {
			t.Fatal(err)
		}
		require, _ := relayEffective(s, route)
		if require != tc.want {
			t.Errorf("front inheritance %+v", tc)
		}
	}
}

func TestLateArchivedTrafficKeepsAuditWithoutChargingNewPackage(t *testing.T) {
	s := newState()
	s.Users["user"] = &User{ID: "user", Status: "active", TrafficTotal: 1000}
	if err := SaveDoc(s, "entitlement_versions", "user", int64(2)); err != nil {
		t.Fatal(err)
	}
	old := UserRule{ID: "old-rule", UserID: "user", RouteID: "route", Version: 1, EntitlementVersion: 1, State: "revoked", Segments: []RelaySegment{{AgentID: "entry", Runtime: relayruntime.Rule{Version: 1, Billing: true, EntitlementVersion: 1}}}}
	if err := SaveDoc(s, "relay_rule_archive", old.ID, old); err != nil {
		t.Fatal(err)
	}
	sample := relayruntime.Traffic{ID: old.ID, Version: 1, EntitlementVersion: 1, Epoch: "archived-process-epoch", Sequence: 1, InputBytes: 100, OutputBytes: 200}
	if err := applyRelayTraffic(s, RelayAgent{ID: "entry"}, sample, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if s.Users["user"].TrafficUsed != 0 {
		t.Fatal("late old package counters charged new package")
	}
	stored, _ := LoadDoc[UserRule](s, "relay_rule_archive", old.ID)
	if stored.TrafficBytes != 300 {
		t.Fatal("late traffic audit lost")
	}
	if _, ok := LoadDoc[UserRule](s, "user_rules", "user:route"); ok {
		t.Fatal("late metric resurrected deleted rule")
	}
}
