package control

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRelayVersionHeaderIsAuthenticatedAndBounded(t *testing.T) {
	f := newRelayV2Fixture(t)
	f.ready(t)
	for i, version := range []string{"v2.2.0", "", "bad version", strings.Repeat("x", 65), ""} {
		in := f.requests[0]
		in.Sequence += int64(10 + i)
		in.RequestID = commerceID()
		expected := "v2.2.0"
		if i == 4 {
			in.AgentInstanceID, expected = commerceID(), ""
		}
		raw, _ := json.Marshal(in)
		r := httptest.NewRequest(http.MethodPost, "/api/relay-agent/v2/sync", bytes.NewReader(raw))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+f.tokens[0])
		r.Header.Set("X-MSBOOST-Agent-Version", version)
		w := httptest.NewRecorder()
		f.mux.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		if err := f.app.Store.View(func(s *State) error {
			agent, _ := LoadDoc[RelayAgent](s, "relay_agents", f.agents[0].ID)
			if agent.Version != expected {
				t.Fatal("incorrect version for current process", agent.Version, expected)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRelayFirewallFailureDoesNotAdvertiseReady(t *testing.T) {
	f := newRelayV2Fixture(t)
	commands := f.ready(t)
	ack := v2Ready(commands[0])
	ack.State = "persisted"
	ack.FirewallStatus = "error"
	f.sync(t, 0, ack)
	check := func(expected string) {
		t.Helper()
		if err := f.app.Store.View(func(s *State) error {
			rule, _ := LoadDoc[UserRule](s, "user_rules", f.user.ID+":route")
			view := relayRuleView(s, rule, false, time.Now().UnixMilli())
			if view.SyncState != expected {
				t.Fatalf("want %s got %s", expected, view.SyncState)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	check("firewall_pending")
	ack.State = "ready"
	ack.FirewallStatus = "ready"
	f.sync(t, 0, ack)
	check("active")
	if err := f.app.Store.View(func(s *State) error {
		for _, status := range []string{"error", "pending", "arbitrary text"} {
			bad := ack
			bad.FirewallStatus = status
			if relayV2KnownAck(s, f.agents[0], bad) {
				t.Fatal("accepted false ready", status)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
