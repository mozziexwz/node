package control

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mozziexwz/node/internal/relayruntime"
)

func relayManagementFixture(t *testing.T) (*App, *http.ServeMux, *User, *User, string, string) {
	t.Helper()
	a, mux, user := commerceTestApp(t)
	a.RegisterIdentity(mux)
	admin := &User{ID: "manager", Email: "admin@example.com", Role: "admin", Status: "active"}
	entryToken, exitToken := commerceID()+commerceID(), commerceID()+commerceID()
	now := time.Now().UnixMilli()
	err := a.Store.Update(func(s *State) error {
		s.Users[admin.ID] = admin
		s.Users[user.ID].ExpiresAt = now + commerceDay
		s.Users[user.ID].TrafficTotal = commerceGB
		for _, agent := range []RelayAgent{
			{ID: "entry", Name: "入口", Address: "8.8.8.8", Enabled: true, Capability: "relay", TokenHash: commerceHash(entryToken), LastSeen: now, BootID: "entry-boot-unique", PortRanges: []PortRange{{20000, 20100}}},
			{ID: "exit", Name: "出口", Address: "8.8.4.4", Enabled: true, Capability: "relay", TokenHash: commerceHash(exitToken), LastSeen: now, BootID: "exit-boot-unique-1", PortRanges: []PortRange{{21000, 21100}}},
		} {
			if err := SaveDoc(s, "relay_agents", agent.ID, agent); err != nil {
				return err
			}
		}
		for i, cap := range []int64{100000, 3} {
			id := fmt.Sprintf("route%d", i)
			route := Route{ID: id, Name: "线路" + id, Type: "tunnel", EntryAgentID: "entry", EntryAddress: "8.8.8.8", Enabled: true, RateMbps: cap, Exit: RouteStage{AgentIDs: []string{"exit"}, Protocol: "tcp", Strategy: "round"}}
			if err := SaveDoc(s, "routes", id, route); err != nil {
				return err
			}
			rule := UserRule{ID: "rule" + id, UserID: user.ID, RouteID: id, RouteName: route.Name, Version: 1, State: "active", TargetHash: "secret-target-hash", TargetHost: "1.1.1.1", TargetPort: 443, EntryAddress: "8.8.8.8", EntryPort: 20000 + i, SealedConfig: "sealed-client-config", CreatedAt: now}
			for j, node := range []string{"entry", "exit"} {
				rule.Segments = append(rule.Segments, RelaySegment{AgentID: node, AckState: "ready", AckAt: now, LastLease: now + relayLeaseMS, SealedTLSKey: "encrypted-private-key", Runtime: relayruntime.Rule{ID: rule.ID, Version: 1, ListenPort: 20000 + i + j*1000, RateMbps: min(5, cap), Protocol: "tcp", Billing: j == 0, Targets: []string{"secret-internal-target:443"}, TLSPrivateKey: "secret-private-key"}})
			}
			if err := SaveDoc(s, "user_rules", user.ID+":"+id, rule); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return a, mux, user, admin, entryToken, exitToken
}

func managementRules(t *testing.T, mux http.Handler, user *User, path string) []RelayRuleView {
	t.Helper()
	w := commerceTestRequest(mux, user, "GET", path, nil)
	var out struct {
		Rules []RelayRuleView `json:"rules"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	return out.Rules
}

func TestRelayRateChangeAllRulesAllHopsRequireCurrentACK(t *testing.T) {
	a, mux, user, admin, entryToken, exitToken := relayManagementFixture(t)
	w := commerceTestRequest(mux, admin, "PATCH", "/api/admin/users/"+user.ID, map[string]any{"rateMbps": 20, "reason": "rate test"})
	if w.Code != 200 {
		t.Fatalf("admin rate: %d %s", w.Code, w.Body.String())
	}
	// Before the cleanup ticker/sync, old ACKs must not confirm the new policy.
	for _, rule := range managementRules(t, mux, user, "/api/user/rules") {
		if rule.SyncState != "syncing" || rule.AppliedRateMbps != 0 {
			t.Fatalf("stale active projection: %+v", rule)
		}
	}
	entry := relayruntime.SyncRequest{BootID: "entry-boot-unique", Sequence: 1, Acks: []relayruntime.Ack{{ID: "ruleroute0", Version: 1, State: "failed"}}}
	if out := relayTestSync(t, mux, entryToken, entry); len(out.Rules) != 0 {
		t.Fatal("ingress published before downstream policy ACK")
	}
	if err := a.Store.View(func(s *State) error {
		for _, rule := range ListDocs[UserRule](s, "user_rules") {
			want := int64(20)
			if rule.RouteID == "route1" {
				want = 3
			}
			if rule.Version != 2 || rule.State != "pending" {
				t.Fatalf("old ACK accepted or other rule not invalidated: %+v", rule)
			}
			for _, seg := range rule.Segments {
				if seg.Runtime.Version != 2 || seg.Runtime.RateMbps != want || seg.AckState != "pending" {
					t.Fatalf("hop not updated: %+v", seg)
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	exit := relayruntime.SyncRequest{BootID: "exit-boot-unique-1", Sequence: 1}
	if out := relayTestSync(t, mux, exitToken, exit); len(out.Rules) != 2 {
		t.Fatal("all rules not delivered to downstream")
	}
	exit.Sequence++
	exit.Acks = []relayruntime.Ack{{ID: "ruleroute0", Version: 2, State: "ready"}}
	relayTestSync(t, mux, exitToken, exit)
	entry.Sequence++
	entry.Acks = nil
	if out := relayTestSync(t, mux, entryToken, entry); len(out.Rules) != 1 || out.Rules[0].ID != "ruleroute0" || out.Rules[0].RateMbps != 20 {
		t.Fatalf("per-rule downstream gate: %+v", out.Rules)
	}
	entry.Sequence++
	entry.Acks = []relayruntime.Ack{{ID: "ruleroute0", Version: 2, State: "ready"}}
	relayTestSync(t, mux, entryToken, entry)
	for _, rule := range managementRules(t, mux, user, "/api/user/rules") {
		if rule.RouteID == "route0" && (rule.AppliedRateMbps != 20 || rule.ReadySegments != 2) {
			t.Fatalf("rule0 not confirmed: %+v", rule)
		}
		if rule.RouteID == "route1" && rule.AppliedRateMbps != 0 {
			t.Fatal("rule1 claimed active before its own ACK")
		}
	}
	exit.Sequence++
	exit.Acks = []relayruntime.Ack{{ID: "ruleroute1", Version: 2, State: "ready"}}
	relayTestSync(t, mux, exitToken, exit)
	entry.Sequence++
	entry.Acks = nil
	relayTestSync(t, mux, entryToken, entry)
	entry.Sequence++
	entry.Acks = []relayruntime.Ack{{ID: "ruleroute1", Version: 2, State: "ready"}}
	relayTestSync(t, mux, entryToken, entry)
	for _, rule := range managementRules(t, mux, user, "/api/user/rules") {
		want := int64(20)
		if rule.RouteID == "route1" {
			want = 3
		}
		if rule.SyncState != "active" || rule.AppliedRateMbps != want {
			t.Fatalf("all ACKs did not activate per-rule rate: %+v", rule)
		}
	}
}

func TestRelayAdminManagementAuthorizationSanitizationAndLeaseArchive(t *testing.T) {
	a, mux, user, admin, entryToken, _ := relayManagementFixture(t)
	path := "/api/admin/user-rules/ruleroute0"
	for _, method := range []string{"GET", "PATCH", "DELETE"} {
		for _, actor := range []*User{nil, user} {
			w := commerceTestRequest(mux, actor, method, path, map[string]any{"paused": true})
			if w.Code != 403 {
				t.Fatalf("unauthorized %s: %d %s", method, w.Code, w.Body.String())
			}
		}
	}
	w := commerceTestRequest(mux, admin, "GET", path, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), user.Email) {
		t.Fatalf("detail missing email: %s", w.Body.String())
	}
	if empty := commerceTestRequest(mux, admin, "PATCH", path, map[string]any{}); empty.Code != 400 {
		t.Fatal("ambiguous update must not resume a rule")
	}
	for _, secret := range []string{"sealed-client-config", "secret-target-hash", "secret-internal-target", "encrypted-private-key", "secret-private-key"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatalf("secret exposed: %s", secret)
		}
	}
	if got := managementRules(t, mux, admin, "/api/admin/user-rules?q=buyer%40example.com&routeId=route0&state=active"); len(got) != 1 {
		t.Fatalf("filters: %+v", got)
	}
	if got := managementRules(t, mux, admin, "/api/admin/user-rules?q=missing"); len(got) != 0 {
		t.Fatal("email filter ignored")
	}
	w = commerceTestRequest(mux, admin, "PATCH", path, map[string]any{"paused": true})
	var view RelayRuleView
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &view) != nil || view.SyncState != "pausing" {
		t.Fatalf("pause: %d %s", w.Code, w.Body.String())
	}
	entry := relayruntime.SyncRequest{BootID: "entry-boot-unique", Sequence: 1}
	for _, rule := range relayTestSync(t, mux, entryToken, entry).Rules {
		if rule.ID == "ruleroute0" {
			t.Fatal("paused rule still leased")
		}
	}
	w = commerceTestRequest(mux, admin, "PATCH", path, map[string]any{"paused": false})
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &view) != nil || view.SyncState != "syncing" {
		t.Fatalf("resume not pending ACK: %d %s", w.Code, w.Body.String())
	}
	w = commerceTestRequest(mux, admin, "DELETE", path, nil)
	if w.Code != 202 {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	var deadline int64
	if err := a.Store.View(func(s *State) error {
		rule, ok := LoadDoc[UserRule](s, "user_rules", user.ID+":route0")
		if !ok || rule.State != "revoking" || rule.SealedConfig != "" || len(rule.Segments) != 2 {
			t.Fatalf("live rule removed before lease expiry: %+v", rule)
		}
		deadline = rule.DeleteAfter
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	commerceTestRequest(mux, admin, "DELETE", path, nil)
	if err := a.Store.Update(func(s *State) error {
		rule, _ := LoadDoc[UserRule](s, "user_rules", user.ID+":route0")
		if rule.DeleteAfter != deadline {
			t.Fatal("repeated delete extended revoke lease")
		}
		return relayCleanup(s, deadline)
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.Store.View(func(s *State) error {
		if _, ok := LoadDoc[UserRule](s, "user_rules", user.ID+":route0"); ok {
			t.Fatal("rule not cleaned after deadline")
		}
		rule, ok := LoadDoc[UserRule](s, "relay_rule_archive", "ruleroute0")
		if !ok || rule.State != "revoked" || rule.SealedConfig != "" {
			t.Fatal("audit archive missing")
		}
		audited := map[string]bool{}
		for _, event := range ListDocs[map[string]any](s, "commerce_audit") {
			if event["userId"] == admin.ID && event["objectId"] == "ruleroute0" {
				audited[event["action"].(string)] = true
			}
		}
		for _, action := range []string{"relay_rule.pause", "relay_rule.resume", "relay_rule.delete"} {
			if !audited[action] {
				t.Fatalf("missing admin audit: %s", action)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRelayRouteRatesAndDeleteConflictIdentifyAssociations(t *testing.T) {
	_, mux, user, admin, _, _ := relayManagementFixture(t)
	w := commerceTestRequest(mux, user, "GET", "/api/routes", nil)
	var out struct {
		Routes   []Route `json:"routes"`
		UserRate int64   `json:"userRateMbps"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.UserRate != 5 || len(out.Routes) != 2 {
		t.Fatalf("routes: %d %s", w.Code, w.Body.String())
	}
	for _, route := range out.Routes {
		want := int64(5)
		if route.ID == "route1" {
			want = 3
		}
		if route.RateMbps != want || route.EntryAddress != "" {
			t.Fatalf("wrong user rate/secret route: %+v", route)
		}
	}
	for _, path := range []string{"/api/admin/routes/route0", "/api/admin/relay-agents/entry"} {
		w = commerceTestRequest(mux, admin, "DELETE", path, nil)
		if w.Code != 409 || !strings.Contains(w.Body.String(), user.Email) || !strings.Contains(w.Body.String(), "ruleroute0") || !strings.Contains(w.Body.String(), "用户中转") {
			t.Fatalf("opaque conflict: %d %s", w.Code, w.Body.String())
		}
	}
}
