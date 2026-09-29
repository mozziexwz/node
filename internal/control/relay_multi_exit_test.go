package control

import (
	"encoding/json"
	"github.com/mozziexwz/node/internal/relayruntime"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRelayDiagnosisSelectsSecondExitWithoutEntryProbe(t *testing.T) {
	f := newRelayV2Fixture(t)
	token := commerceID() + commerceID()
	agent := RelayAgent{ID: commerceID(), Name: "second-exit", Address: "9.9.9.9", Capability: "relay", Enabled: true, TokenHash: commerceHash(token), LastSeen: time.Now().UnixMilli(), PortRanges: []PortRange{{Start: 20002, End: 20002}}}
	f.agents = append(f.agents, agent)
	f.tokens = append(f.tokens, token)
	f.requests = append(f.requests, relayruntime.V2SyncRequest{ProtocolVersion: 2, AgentID: agent.ID, AgentInstanceID: commerceID(), Capabilities: append([]string(nil), relayruntime.V2Capabilities...)})
	for i := range f.requests {
		f.requests[i].Capabilities = append(f.requests[i].Capabilities, relayruntime.TargetProbeCapability)
	}
	if err := f.app.Store.Update(func(s *State) error {
		if err := SaveDoc(s, "relay_agents", agent.ID, agent); err != nil {
			return err
		}
		route, _ := LoadDoc[Route](s, "routes", "route")
		route.Exit.AgentIDs = append(route.Exit.AgentIDs, agent.ID)
		if err := SaveDoc(s, "routes", route.ID, route); err != nil {
			return err
		}
		rule, _ := LoadDoc[UserRule](s, "user_rules", f.user.ID+":route")
		exit := rule.Segments[1]
		exit.AgentID = agent.ID
		exit.Runtime.ListenPort = 20002
		rule.Segments = append(rule.Segments, exit)
		rule.TargetHost = "1.1.1.1"
		rule.TargetPort = 443
		rule.EntryAddress = f.agents[0].Address
		rule.EntryPort = 20000
		return SaveDoc(s, "user_rules", f.user.ID+":route", rule)
	}); err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 6; round++ {
		for i := range f.agents {
			out := f.sync(t, i)
			for _, cmd := range out.Commands {
				if cmd.Action == "upsert" {
					f.sync(t, i, v2Ready(cmd))
				}
			}
		}
	}
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		result <- commerceTestRequest(f.mux, f.user, http.MethodPost, "/api/user/routes/route/diagnose?exit=1", nil)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		f.app.relayDiagnostics.mu.Lock()
		count := len(f.app.relayDiagnostics.jobs)
		f.app.relayDiagnostics.mu.Unlock()
		if count > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("probe was not queued")
		}
		time.Sleep(5 * time.Millisecond)
	}
	for _, i := range []int{0, 1} {
		if len(f.sync(t, i).TargetProbes) != 0 {
			t.Fatal("non-selected node received probe")
		}
	}
	challenge := f.sync(t, 2)
	if len(challenge.TargetProbes) != 1 || challenge.TargetProbes[0].Target != "1.1.1.1:443" {
		t.Fatal("wrong exit or target")
	}
	f.requests[2].TargetProbeResults = []relayruntime.TargetProbeResult{{ID: challenge.TargetProbes[0].ID, RuleID: f.ruleID, Status: "success", LatencyMS: 11}}
	f.sync(t, 2)
	select {
	case w := <-result:
		var body struct {
			Status    string `json:"status"`
			ExitIndex int    `json:"exitIndex"`
			ExitCount int    `json:"exitCount"`
			LatencyMS int    `json:"latencyMs"`
		}
		if json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Status != "success" || body.ExitIndex != 1 || body.ExitCount != 2 || body.LatencyMS != 11 {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("diagnosis did not finish")
	}
	for _, query := range []string{"-1", "8", "wrong", "2"} {
		w := commerceTestRequest(f.mux, f.user, http.MethodPost, "/api/user/routes/route/diagnose?exit="+query, nil)
		if w.Code < 400 {
			t.Fatal("invalid exit accepted")
		}
	}
}
