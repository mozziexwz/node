package control

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mozziexwz/node/internal/executor"
	"github.com/mozziexwz/node/internal/relayruntime"
)

// relayTestConfigUDP is relayTestConfig from a node that also serves UDP.
var relayTestConfigUDP = strings.Replace(relayTestConfig, `{"port":12345,"protocol":"TCP"}`, `{"port":12345,"protocol":"TCP"},{"port":12345,"protocol":"UDP"}`, 1)

func TestNewRelayRuleWaitsForACheckBeforeUDP(t *testing.T) {
	guarded := []string{relayruntime.SocksGuardCapability, relayruntime.UDPForwardCapability}
	for _, c := range []struct {
		name       string
		config     string
		verify     string
		exitCanUDP bool
		want       string
	}{
		{"udp node", relayTestConfigUDP, "log", true, "pending"},
		{"tcp node", relayTestConfig, "log", true, ""},
		{"no checks", relayTestConfigUDP, "off", true, ""},
		{"old exit", relayTestConfigUDP, "enforce", false, ""},
		{"well-known port", strings.ReplaceAll(relayTestConfigUDP, "12345", "443"), "enforce", true, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, mux, user := commerceTestApp(t)
			now := time.Now().UnixMilli()
			exit := []string{relayruntime.SocksGuardCapability}
			if c.exitCanUDP {
				exit = guarded
			}
			if err := a.Store.Update(func(s *State) error {
				s.Users[user.ID].ExpiresAt = now + commerceDay
				s.Users[user.ID].TrafficTotal = commerceGB
				s.Settings["relayVerify"] = RelayVerifySettings{Free: "selftest", Code: "log", Donation: c.verify, RecheckHours: 24, GraceHours: 48, BreakerPercent: 20}
				for _, agent := range []RelayAgent{
					{ID: "entry", Address: "8.8.8.8", Enabled: true, Capability: "relay", LastSeen: now, PortRanges: []PortRange{{Start: 22000, End: 22010}}, Capabilities: guarded},
					{ID: "exit", Address: "8.8.4.4", Enabled: true, Capability: "relay", LastSeen: now, PortRanges: []PortRange{{Start: 23000, End: 23010}}, Capabilities: exit},
				} {
					if err := SaveDoc(s, "relay_agents", agent.ID, agent); err != nil {
						return err
					}
				}
				return SaveDoc(s, "routes", "route", Route{ID: "route", Name: "two hops", Type: "tunnel", EntryAgentID: "entry", EntryAddress: "8.8.8.8", Exit: RouteStage{AgentIDs: []string{"exit"}, Protocol: "tls", Strategy: "round"}, Enabled: true, RateMbps: 5, Level: 1})
			}); err != nil {
				t.Fatal(err)
			}
			w := commerceTestRequest(mux, user, "POST", "/api/user/routes/route/rules", map[string]any{"config": json.RawMessage(c.config), "requestId": "udp-route-0001"})
			if w.Code != http.StatusAccepted {
				t.Fatalf("create: %d %s", w.Code, w.Body.String())
			}
			_ = a.Store.View(func(s *State) error {
				rule, _ := LoadDoc[UserRule](s, "user_rules", user.ID+":route")
				if rule.UDP != c.want {
					t.Fatalf("udp %q, want %q", rule.UDP, c.want)
				}
				for _, seg := range rule.Segments {
					if seg.Runtime.UDP {
						t.Fatal("a new rule must start with TCP only")
					}
				}
				raw, err := a.Open(rule.SealedConfig)
				if err != nil {
					t.Fatal(err)
				}
				if info, err := executor.ParseClientConfig(raw); err != nil || info.UDP {
					t.Fatal("the configuration offers UDP before the relay forwards it")
				}
				return nil
			})
		})
	}
}

func TestRelayRuleOpensUDPOnEveryHopAfterPassing(t *testing.T) {
	f := newRelayV2Fixture(t)
	for i := range f.requests {
		f.requests[i].Capabilities = append(f.requests[i].Capabilities, relayruntime.UDPForwardCapability)
	}
	previous := f.ready(t)
	stub := &stubRouteVerifier{}
	f.app.SetRouteVerifier(stub.verify)
	key := f.verifiedRule(t, &RuleVerify{State: "pending"})
	waiting := func(front, frontUDP bool) {
		t.Helper()
		if err := f.app.Store.Update(func(s *State) error {
			s.Settings["relayVerify"] = RelayVerifySettings{Free: "selftest", Code: "log", Donation: "enforce", RecheckHours: 24, GraceHours: 48, BreakerPercent: 20}
			rule, _ := LoadDoc[UserRule](s, "user_rules", key)
			config, err := executor.RewriteClientConfig([]byte(relayTestConfigUDP), rule.EntryAddress, rule.EntryPort)
			if err != nil {
				return err
			}
			if rule.SealedConfig, err = f.app.Seal(config); err != nil {
				return err
			}
			rule.UDP, rule.HasFront, rule.FrontUDP = "pending", front, frontUDP
			rule.Verify = &RuleVerify{State: "pending"}
			return SaveDoc(s, "user_rules", key, rule)
		}); err != nil {
			t.Fatal(err)
		}
	}
	// A front that cannot forward UDP keeps the whole rule on TCP.
	waiting(true, false)
	f.runVerify(t, key)
	if rule := f.rule(t, key); rule.Verify.State != "ok" || rule.UDP != "pending" || rule.Segments[0].Runtime.UDP {
		t.Fatalf("UDP opened behind a TCP-only front: %+v %s", rule.Verify, rule.UDP)
	}
	waiting(false, false)
	f.runVerify(t, key)
	rule := f.rule(t, key)
	if rule.Verify.State != "ok" || rule.UDP != "on" {
		t.Fatalf("passing did not open UDP: %+v %q", rule.Verify, rule.UDP)
	}
	for _, seg := range rule.Segments {
		if !seg.Runtime.UDP {
			t.Fatal("every hop must forward UDP")
		}
	}
	raw, err := f.app.Open(rule.SealedConfig)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := executor.ParseClientConfig(raw); err != nil || !info.UDP || info.TargetPort != rule.EntryPort {
		t.Fatalf("the downloaded configuration needs UDP on the entry port: %s", raw)
	}
	// The next sync restarts the hop with UDP: a new runtime, a new command.
	entry := f.sync(t, 0)
	if len(entry.Commands) != 1 || entry.Commands[0].Rule == nil || !entry.Commands[0].Rule.UDP || entry.Commands[0].RuntimeHash == previous[0].RuntimeHash {
		t.Fatalf("entry not reissued with UDP: %+v", entry.Commands)
	}
	if hash, err := relayruntime.RuntimeHash(*entry.Commands[0].Rule); err != nil || hash != entry.Commands[0].RuntimeHash {
		t.Fatal("reissued command hash does not match its rule")
	}
	ready := func() (ok bool) {
		_ = f.app.Store.View(func(s *State) error {
			rule, _ := LoadDoc[UserRule](s, "user_rules", key)
			ok = relaySegmentReady(s, rule.Segments[0], time.Now().UnixMilli())
			return nil
		})
		return ok
	}
	if ready() {
		t.Fatal("a hop is not ready with UDP before its Agent applied it")
	}
	f.sync(t, 0, v2Ready(entry.Commands[0]))
	if !ready() {
		t.Fatal("the applied UDP command did not make the hop ready")
	}
}
