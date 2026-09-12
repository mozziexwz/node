package control

import (
	"encoding/json"
	"github.com/mozziexwz/node/internal/relayruntime"
	"math"
	"strings"
	"testing"
	"time"
)

func TestRelayWeightedTrafficDirectionsRemainderAndOverflow(t *testing.T) {
	for _, tc := range []struct {
		mode             string
		multiplier, want int64
	}{{"both", 1500, 450}, {"upload", 2000, 200}, {"download", 500, 100}, {"", 0, 300}} {
		got, _, err := relayWeightedTraffic(100, 200, tc.mode, tc.multiplier, 0)
		if err != nil || got != tc.want {
			t.Fatalf("%+v got %d %v", tc, got, err)
		}
	}
	var total, remainder int64
	for i := 0; i < 1000; i++ {
		delta, next, err := relayWeightedTraffic(1, 999, "upload", 1, remainder)
		if err != nil {
			t.Fatal(err)
		}
		total += delta
		remainder = next
	}
	if total != 1 || remainder != 0 {
		t.Fatal("fractional traffic lost across samples")
	}
	if _, _, err := relayWeightedTraffic(math.MaxInt64, 0, "upload", 100000, 0); err == nil {
		t.Fatal("weighted overflow accepted")
	}
}

func TestRelayPortForwardTLSAndPrivateUserResponses(t *testing.T) {
	a, mux, user := commerceTestApp(t)
	admin := &User{ID: "admin", Role: "admin", Status: "active"}
	now := time.Now().UnixMilli()
	if err := a.Store.Update(func(s *State) error {
		s.Users[user.ID].ExpiresAt = now + commerceDay
		s.Users[user.ID].TrafficTotal = commerceGB
		for _, node := range []RelayAgent{{ID: "entry", Name: "入口", Address: "8.8.8.8", Addresses: []string{"2001:4860:4860::8888"}, Capability: "relay", Enabled: true, RequireFront: true, LastSeen: now, PortRanges: []PortRange{{22000, 22010}}}, {ID: "exit", Name: "出口", Address: "8.8.4.4", Addresses: []string{"2001:4860:4860::8844"}, Capability: "relay", Enabled: true, LastSeen: now, PortRanges: []PortRange{{23000, 23010}}}} {
			if err := SaveDoc(s, "relay_agents", node.ID, node); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"port_forward", "tunnel"} {
		route := Route{Name: kind, Type: kind, EntryAgentID: "entry", Enabled: true, RateMbps: 5, TrafficMode: "download", TrafficMultiplierPermille: 1500, AddressPreference: "ipv6"}
		if kind == "tunnel" {
			route.Exit = RouteStage{AgentIDs: []string{"exit"}, Protocol: "tls", Strategy: "round"}
		}
		response := commerceTestRequest(mux, admin, "POST", "/api/admin/routes", route)
		if response.Code != 200 {
			t.Fatal(response.Body.String())
		}
		if err := json.Unmarshal(response.Body.Bytes(), &route); err != nil {
			t.Fatal(err)
		}
		if route.EntryAddress != "2001:4860:4860::8888" {
			t.Fatal("automatic entry family ignored")
		}
		response = commerceTestRequest(mux, user, "POST", "/api/user/routes/"+route.ID+"/rules", map[string]any{"config": json.RawMessage(relayTestConfig), "requestId": "configure-" + kind})
		if response.Code != 202 {
			t.Fatal(response.Body.String())
		}
		if strings.Contains(response.Body.String(), "8.8.") || strings.Contains(response.Body.String(), "2001:") || strings.Contains(response.Body.String(), "segments") || strings.Contains(response.Body.String(), "PRIVATE KEY") {
			t.Fatal("user response exposes infrastructure")
		}
		if err := a.Store.View(func(s *State) error {
			rule, _ := LoadDoc[UserRule](s, "user_rules", user.ID+":"+route.ID)
			if rule.TrafficMode != "download" || rule.TrafficMultiplierPermille != 1500 {
				t.Error("tariff snapshot missing")
			}
			if kind == "port_forward" && len(rule.Segments) != 1 {
				t.Error("port forwarding created exit")
			}
			if kind == "tunnel" {
				if len(rule.Segments) != 2 || len(rule.Segments[0].Runtime.TargetTLS) != 1 || rule.Segments[1].SealedTLSKey == "" || rule.Segments[1].Runtime.TLSPrivateKey != "" {
					t.Error("TLS identity not sealed or chain not assigned")
				}
				if !strings.Contains(rule.Segments[0].Runtime.Targets[0], "2001:4860:4860::8844") {
					t.Error("IPv6 connection preference ignored")
				}
				key, err := a.Open(rule.Segments[1].SealedTLSKey)
				if err != nil || !strings.Contains(string(key), "PRIVATE KEY") {
					t.Error("TLS key unavailable")
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		response = commerceTestRequest(mux, user, "GET", "/api/user/routes/"+route.ID+"/config", nil)
		if response.Code != 200 || !strings.Contains(response.Body.String(), "2001:4860:4860::8888") {
			t.Fatal("download must retain usable entry")
		}
	}
	for _, path := range []string{"/api/routes", "/api/user/rules"} {
		response := commerceTestRequest(mux, user, "GET", path, nil)
		if strings.Contains(response.Body.String(), "8.8.") || strings.Contains(response.Body.String(), "2001:") || strings.Contains(response.Body.String(), "PRIVATE KEY") {
			t.Fatalf("address leak from %s", path)
		}
	}
}

func TestRelayTLSSecretsOnlySentToOwningAgentAndRotation(t *testing.T) {
	a, mux, user := commerceTestApp(t)
	now := time.Now().UnixMilli()
	token := commerceID() + commerceID()
	rule := UserRule{ID: "tls-rotation-rule", UserID: user.ID, RouteID: "route", Version: 1, State: "pending", Segments: []RelaySegment{{AgentID: "exit", Runtime: relayruntime.Rule{ID: "tls-rotation-rule", Version: 1, Protocol: "tls", RateMbps: 5}}}}
	stages := []RouteStage{{AgentIDs: []string{"exit"}, Protocol: "tls"}}
	if err := a.configureRelayTLS(&rule, stages, now+commerceDay); err != nil {
		t.Fatal(err)
	}
	certificate := rule.Segments[0].Runtime.TLSCertificate
	if err := a.configureRelayTLS(&rule, stages, now+commerceDay); err != nil {
		t.Fatal(err)
	}
	if certificate == rule.Segments[0].Runtime.TLSCertificate {
		t.Fatal("rotation reused identity")
	}
	if err := a.Store.Update(func(s *State) error {
		s.Users[user.ID].ExpiresAt = now + commerceDay
		s.Users[user.ID].TrafficTotal = commerceGB
		s.Users[user.ID].RateMbps = 5
		if err := SaveDoc(s, "relay_agents", "exit", RelayAgent{ID: "exit", Capability: "relay", TokenHash: commerceHash(token), Enabled: true, LastSeen: now}); err != nil {
			return err
		}
		if err := SaveDoc(s, "routes", "route", Route{ID: "route", EntryAgentID: "exit", Enabled: true, RateMbps: 5}); err != nil {
			return err
		}
		return SaveDoc(s, "user_rules", user.ID+":route", rule)
	}); err != nil {
		t.Fatal(err)
	}
	response := relayTestSync(t, mux, token, relayruntime.SyncRequest{BootID: "tls-agent-boot-unique", Sequence: 1})
	if len(response.Rules) != 1 || !strings.Contains(response.Rules[0].TLSPrivateKey, "PRIVATE KEY") {
		t.Fatal("TLS owner did not receive runtime key")
	}
	if err := a.Store.View(func(s *State) error {
		raw, _ := json.Marshal(s)
		if strings.Contains(string(raw), "PRIVATE KEY") {
			t.Error("runtime private key leaked to persistence")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
