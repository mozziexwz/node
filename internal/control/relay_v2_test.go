package control

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mozziexwz/node/internal/relayruntime"
)

type relayV2Fixture struct {
	app      *App
	mux      http.Handler
	user     *User
	agents   []RelayAgent
	tokens   []string
	requests []relayruntime.V2SyncRequest
	ruleID   string
}

func TestRelayV2ObservationSequenceAndProcessReplacement(t *testing.T) {
	f := newRelayV2Fixture(t)
	commands := f.ready(t)
	old := f.requests[0]
	old.Acks = []relayruntime.V2Ack{v2Ready(commands[0])}
	fresh := old
	fresh.Sequence += 10
	fresh.Acks = append([]relayruntime.V2Ack(nil), old.Acks...)
	fresh.Acks[0].State = "persisted"
	if out := f.send(t, 0, fresh, 200); out.Status != "ready" || len(out.Commands) != 1 {
		t.Fatal("current crash observation failed to invalidate readiness")
	}
	if out := f.send(t, 0, old, 200); len(out.Commands) != 1 {
		t.Fatal("stale ready revived a crashed process")
	}
	assertPersisted := func() {
		t.Helper()
		if err := f.app.Store.View(func(s *State) error {
			rule, _ := LoadDoc[UserRule](s, "user_rules", f.user.ID+":route")
			if rule.Segments[0].AckState != "persisted" || relayRuleView(s, rule, false, time.Now().UnixMilli()).RuntimeStatus != "partial_unknown" {
				t.Fatal("stale ACK changed latest observation")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	assertPersisted()
	// A replacement process may first reach the control plane after several
	// failed connection attempts; its first received sequence need not be one.
	replacement := fresh
	replacement.AgentInstanceID, replacement.Sequence = commerceID(), 7
	replacement.Acks = nil
	if out := f.send(t, 0, replacement, 200); out.Status != "ready" || len(out.Commands) != 1 {
		t.Fatal("cold process could not reconcile the durable command")
	}
	assertPersisted()
	if out := f.send(t, 0, fresh, 200); out.Status != "recovery_required" || len(out.Commands) != 0 {
		t.Fatal("retired competing process was accepted")
	}
}

func TestRelayV2PauseProofIsNotTerminalRevoke(t *testing.T) {
	f := newRelayV2Fixture(t)
	f.ready(t)
	if err := f.app.Store.Update(func(s *State) error {
		rule, _ := LoadDoc[UserRule](s, "user_rules", f.user.ID+":route")
		rule.State = "paused"
		return SaveDoc(s, "user_rules", f.user.ID+":route", rule)
	}); err != nil {
		t.Fatal(err)
	}
	for i := range f.agents {
		out := f.sync(t, i)
		if len(out.Commands) != 1 || out.Commands[0].Action != "pause" {
			t.Fatal("missing explicit pause")
		}
		f.sync(t, i, v2Stopped(out.Commands[0]))
	}
	if err := f.app.Store.Update(func(s *State) error {
		rule, _ := LoadDoc[UserRule](s, "user_rules", f.user.ID+":route")
		relayRevoke(&rule, 0, true)
		if err := SaveDoc(s, "user_rules", f.user.ID+":route", rule); err != nil {
			return err
		}
		if err := relayCleanup(s, time.Now().UnixMilli()); err != nil {
			return err
		}
		if _, ok := LoadDoc[UserRule](s, "user_rules", f.user.ID+":route"); !ok {
			t.Fatal("pause ACK incorrectly replaced terminal revoke proof")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for i := range f.agents {
		out := f.sync(t, i)
		if len(out.Commands) != 1 || out.Commands[0].Action != "revoke" {
			t.Fatal("revoke tombstone not dispatched after confirmed pause")
		}
	}
}

func TestRelayV2MixedLeaseAndStopProof(t *testing.T) {
	f := newRelayV2Fixture(t)
	f.sync(t, 1)
	exit := f.sync(t, 1)
	f.sync(t, 1, v2Ready(exit.Commands[0]))
	now := time.Now().UnixMilli()
	if err := f.app.Store.Update(func(s *State) error {
		rule, _ := LoadDoc[UserRule](s, "user_rules", f.user.ID+":route")
		rule.Segments[0].AckState = "ready"
		rule.Segments[0].LastLease = now + relayLeaseMS
		view := relayRuleView(s, rule, false, now)
		if view.OfflinePolicy != "mixed" || view.KeepLastConfirmed {
			t.Fatal("mixed chain falsely confirmed keep-last")
		}
		relayRevoke(&rule, 0, true)
		return SaveDoc(s, "user_rules", f.user.ID+":route", rule)
	}); err != nil {
		t.Fatal(err)
	}
	stop := f.sync(t, 1)
	f.sync(t, 1, v2Stopped(stop.Commands[0]))
	if err := f.app.Store.Update(func(s *State) error {
		if err := relayCleanup(s, now); err != nil {
			return err
		}
		if _, ok := LoadDoc[UserRule](s, "user_rules", f.user.ID+":route"); !ok {
			t.Fatal("live v1 lease released early")
		}
		if err := relayCleanup(s, now+relayLeaseMS+1); err != nil {
			return err
		}
		if _, ok := LoadDoc[UserRule](s, "user_rules", f.user.ID+":route"); ok {
			t.Fatal("v1 expired lease plus exact v2 stop failed to release")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRelayV2BoundedCommandResendDoesNotDeleteOrStarve(t *testing.T) {
	f := newRelayV2Fixture(t)
	f.sync(t, 1)
	if err := f.app.Store.Update(func(s *State) error {
		base, _ := LoadDoc[UserRule](s, "user_rules", f.user.ID+":route")
		for i := 0; i < 40; i++ {
			routeID := fmt.Sprintf("route-%02d", i)
			if err := SaveDoc(s, "routes", routeID, Route{ID: routeID, EntryAgentID: f.agents[0].ID, Enabled: true, RateMbps: 5}); err != nil {
				return err
			}
			rule := base
			rule.ID, rule.RouteID = commerceID(), routeID
			rule.Segments = append([]RelaySegment(nil), base.Segments...)
			for j := range rule.Segments {
				rule.Segments[j].Runtime.ID = rule.ID
				rule.Segments[j].Runtime.ListenPort += i + 2
			}
			if err := SaveDoc(s, "user_rules", f.user.ID+":"+routeID, rule); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for i := 0; i < 3; i++ {
		out := f.sync(t, 1)
		if len(out.Commands) != 32 {
			t.Fatal("bounded resend window not enforced")
		}
		for _, command := range out.Commands {
			if old, ok := seen[command.RuleID]; ok && old != command.CommandID {
				t.Fatal("lost reply created a new command")
			}
			seen[command.RuleID] = command.CommandID
		}
	}
	if len(seen) != 41 {
		t.Fatal("unacknowledged command starved behind resend window")
	}
	if err := f.app.Store.View(func(s *State) error {
		if len(s.Docs["user_rules"]) != 41 {
			t.Fatal("omitted rules deleted")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRelayV2InvalidTrafficDoesNotRejectControlOrACKBadData(t *testing.T) {
	f := newRelayV2Fixture(t)
	commands := f.ready(t)
	now := time.Now().UnixMilli()
	valid := relayruntime.V2Traffic{Traffic: relayruntime.Traffic{ID: f.ruleID, Version: commands[0].Rule.Version, Epoch: commerceID(), Sequence: 1, InputBytes: 100, OutputBytes: 200, EntitlementVersion: commands[0].Rule.EntitlementVersion}, BillingPeriodID: commands[0].BillingPeriodID, CollectedFrom: now, CollectedUntil: now}
	bad := valid
	bad.Epoch = commerceID()
	bad.InputBytes = -1
	if err := f.app.Store.Update(func(s *State) error {
		rule, _ := LoadDoc[UserRule](s, "user_rules", f.user.ID+":route")
		rule.State = "paused"
		return SaveDoc(s, "user_rules", f.user.ID+":route", rule)
	}); err != nil {
		t.Fatal(err)
	}
	in := f.requests[0]
	in.Sequence++
	in.Acks = []relayruntime.V2Ack{v2Ready(commands[0])}
	in.Traffic = []relayruntime.V2Traffic{bad, valid}
	out := f.send(t, 0, in, 200)
	if out.Status != "ready" || len(out.Commands) != 1 || out.Commands[0].Action != "pause" || len(out.TrafficAcks) != 1 || out.TrafficAcks[0].Epoch != valid.Epoch {
		t.Fatal("bad sample blocked control or received an ACK")
	}
	if err := f.app.Store.View(func(s *State) error {
		rule, _ := LoadDoc[UserRule](s, "user_rules", f.user.ID+":route")
		if s.Users[f.user.ID].TrafficUsed != 300 || !relayRuleView(s, rule, false, now).AccountingDegraded {
			t.Fatal("traffic isolation or degradation visibility failed")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRelayV2PolicyViewWaitsForAppliedGenerationButRetainsLastRuntime(t *testing.T) {
	f := newRelayV2Fixture(t)
	commands := f.ready(t)
	if err := f.app.Store.Update(func(s *State) error {
		s.Users[f.user.ID].RateMbps = 2
		if err := relayCleanup(s, time.Now().UnixMilli()); err != nil {
			return err
		}
		rule, _ := LoadDoc[UserRule](s, "user_rules", f.user.ID+":route")
		view := relayRuleView(s, rule, false, time.Now().UnixMilli())
		if view.AppliedRateMbps != 0 || view.ReadySegments != 0 || view.RuntimeStatus != "last_reported_running" {
			t.Fatal("unissued policy falsely marked applied or erased last runtime observation")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	out := f.sync(t, 0, v2Ready(commands[0]))
	if len(out.Commands) != 1 || out.Commands[0].RuntimeHash == commands[0].RuntimeHash {
		t.Fatal("effective rate failed to change runtime hash")
	}
	f.sync(t, 0, v2Ready(out.Commands[0]))
	if err := f.app.Store.View(func(s *State) error {
		rule, _ := LoadDoc[UserRule](s, "user_rules", f.user.ID+":route")
		view := relayRuleView(s, rule, false, time.Now().UnixMilli())
		if view.AppliedRateMbps != 0 || view.ReadySegments != 1 {
			t.Fatal("unconfirmed downstream rate falsely applied")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRelayV2AdminEditsPreserveCapabilityAndResetDoesNotPromiseLeaseStop(t *testing.T) {
	f := newRelayV2Fixture(t)
	f.ready(t)
	admin := &User{ID: "operator", Role: "admin", Status: "active"}
	input := f.agents[0]
	input.Name = "Renamed"
	w := commerceTestRequest(f.mux, admin, http.MethodPut, "/api/admin/relay-agents/"+input.ID, input)
	if w.Code != 200 {
		t.Fatalf("admin rename: %d %s", w.Code, w.Body.String())
	}
	if err := f.app.Store.View(func(s *State) error {
		agent, _ := LoadDoc[RelayAgent](s, "relay_agents", input.ID)
		if agent.ProtocolVersion != 2 || agent.OfflinePolicy != "keep_last" || !agent.KeepLastConfirmed || len(agent.Capabilities) != len(relayruntime.V2Capabilities) {
			t.Fatal("ordinary PUT erased negotiated capabilities")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	w = commerceTestRequest(f.mux, f.user, http.MethodPut, "/api/admin/relay-agents/"+input.ID, input)
	if w.Code != 403 {
		t.Fatal("member could edit relay agent")
	}
	w = commerceTestRequest(f.mux, f.user, http.MethodDelete, "/api/user/routes/route/rules", nil)
	if w.Code != 202 || strings.Contains(w.Body.String(), "maximumLeaseSeconds") {
		t.Fatal("v2 deletion falsely promised a short lease stop")
	}
	w = commerceTestRequest(f.mux, f.user, http.MethodPost, "/api/user/target/reset", map[string]bool{"confirm": true})
	if w.Code != 202 || strings.Contains(w.Body.String(), "retryAfter") {
		t.Fatal("v2 target reset falsely promised a time-based release")
	}
	w = commerceTestRequest(f.mux, admin, http.MethodPost, "/api/admin/relay-agents/"+input.ID+"/enrollment", nil)
	if w.Code != 200 || strings.Contains(w.Body.String(), "previousLeaseExpiresAt") {
		t.Fatal("token rotation falsely promised old forwarding stopped")
	}
	in := f.requests[0]
	in.Sequence++
	f.send(t, 0, in, 401)
	if err := f.app.Store.View(func(s *State) error {
		rule, _ := LoadDoc[UserRule](s, "user_rules", f.user.ID+":route")
		if !relayRecoveryRequired(s, rule) {
			t.Fatal("token rotation dropped recovery resource lock")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRelayV2MaximumInventoryHasExplicitBoundedBody(t *testing.T) {
	f := newRelayV2Fixture(t)
	commands := f.ready(t)
	in := f.requests[0]
	in.Sequence++
	in.Acks = make([]relayruntime.V2Ack, 4096)
	now := time.Now().UnixMilli()
	for i := 0; i < relayruntime.MaxV2Traffic; i++ {
		in.Traffic = append(in.Traffic, relayruntime.V2Traffic{Traffic: relayruntime.Traffic{ID: f.ruleID, Version: commands[0].Rule.Version, Epoch: fmt.Sprintf("%0100d", i), Sequence: 1, InputBytes: 1, EntitlementVersion: commands[0].Rule.EntitlementVersion}, BillingPeriodID: commands[0].BillingPeriodID, CollectedFrom: now, CollectedUntil: now})
	}
	if err := f.app.Store.Update(func(s *State) error {
		for i := range in.Acks {
			command := RelayV2Command{AgentID: f.agents[0].ID, RuleID: fmt.Sprintf("%0100d", i), CommandID: fmt.Sprintf("%0100d", i+5000), Generation: 1, Action: "revoke", RuntimeHash: strings.Repeat("a", 64)}
			in.Acks[i] = relayruntime.V2Ack{RuleID: command.RuleID, CommandID: command.CommandID, Generation: 1, RuntimeHash: command.RuntimeHash, State: "stopped"}
			if err := SaveDoc(s, "relay_v2_command_history", command.CommandID, command); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(in)
	if len(raw) <= 1<<20 || len(raw) >= 2<<20 {
		t.Fatal("test inventory does not exercise full bound")
	}
	if out := f.send(t, 0, in, 200); out.Status != "ready" || len(out.TrafficAcks) != relayruntime.MaxV2Traffic {
		t.Fatal("valid complete inventory cannot synchronize")
	}
	in.Acks = nil
	raw, _ = json.Marshal(in)
	raw = append(raw, bytes.Repeat([]byte(" "), (2<<20)+1-len(raw))...)
	r := httptest.NewRequest(http.MethodPost, "/api/relay-agent/v2/sync", bytes.NewReader(raw))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+f.tokens[0])
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "2 MiB") {
		t.Fatal("oversized body was truncated and accepted")
	}
}

func newRelayV2Fixture(t *testing.T) *relayV2Fixture {
	t.Helper()
	a, mux, user := commerceTestApp(t)
	f := &relayV2Fixture{app: a, mux: mux, user: user, ruleID: commerceID()}
	f.tokens = []string{commerceID() + commerceID(), commerceID() + commerceID()}
	for i, address := range []string{"8.8.8.8", "8.8.4.4"} {
		agent := RelayAgent{ID: commerceID(), Name: address, Address: address, Capability: "relay", Enabled: true, TokenHash: commerceHash(f.tokens[i]), LastSeen: time.Now().UnixMilli(), PortRanges: []PortRange{{Start: 20000 + i, End: 20000 + i}}}
		f.agents = append(f.agents, agent)
		f.requests = append(f.requests, relayruntime.V2SyncRequest{ProtocolVersion: 2, AgentID: agent.ID, AgentInstanceID: commerceID(), Capabilities: append([]string(nil), relayruntime.V2Capabilities...)})
	}
	if err := a.Store.Update(func(s *State) error {
		s.Users[user.ID].ExpiresAt = time.Now().UnixMilli() + 10*commerceDay
		s.Users[user.ID].TrafficTotal = commerceGB
		for _, agent := range f.agents {
			if err := SaveDoc(s, "relay_agents", agent.ID, agent); err != nil {
				return err
			}
		}
		if err := SaveDoc(s, "routes", "route", Route{ID: "route", EntryAgentID: f.agents[0].ID, Enabled: true, RateMbps: 5, Exit: RouteStage{AgentIDs: []string{f.agents[1].ID}, Protocol: "tcp", Strategy: "round"}}); err != nil {
			return err
		}
		rule := UserRule{ID: f.ruleID, UserID: user.ID, RouteID: "route", Version: 1, State: "pending", CreatedAt: time.Now().UnixMilli()}
		for i, agent := range f.agents {
			rule.Segments = append(rule.Segments, RelaySegment{AgentID: agent.ID, AckState: "pending", Runtime: relayruntime.Rule{ID: f.ruleID, Version: 1, ListenPort: 20000 + i, Targets: []string{"1.1.1.1:443"}, Strategy: "round", Protocol: "tcp", RateMbps: 5, Billing: i == 0}})
		}
		return SaveDoc(s, "user_rules", user.ID+":route", rule)
	}); err != nil {
		t.Fatal(err)
	}
	return f
}
func (f *relayV2Fixture) send(t *testing.T, index int, in relayruntime.V2SyncRequest, status int) relayruntime.V2SyncResponse {
	t.Helper()
	if in.Sequence == 0 {
		f.requests[index].Sequence++
		in.Sequence = f.requests[index].Sequence
	}
	in.RequestID = commerceID()
	raw, _ := json.Marshal(in)
	r := httptest.NewRequest(http.MethodPost, "/api/relay-agent/v2/sync", bytes.NewReader(raw))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+f.tokens[index])
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	if w.Code != status {
		t.Fatalf("v2 HTTP %d want %d: %s", w.Code, status, w.Body.String())
	}
	var out relayruntime.V2SyncResponse
	if status == 200 {
		if json.Unmarshal(w.Body.Bytes(), &out) != nil || out.RequestID != in.RequestID || out.AgentID != in.AgentID {
			t.Fatal("unbound sync response")
		}
	}
	return out
}
func (f *relayV2Fixture) sync(t *testing.T, index int, acks ...relayruntime.V2Ack) relayruntime.V2SyncResponse {
	t.Helper()
	f.requests[index].Sequence++
	in := f.requests[index]
	in.Acks = acks
	out := f.send(t, index, in, 200)
	f.requests[index].ControlEpoch = out.ControlEpoch
	f.requests[index].AppliedRevision = out.Revision
	return out
}
func v2Ready(command relayruntime.V2Command) relayruntime.V2Ack {
	return relayruntime.V2Ack{CommandID: command.CommandID, RuleID: command.RuleID, Generation: command.Generation, RuntimeHash: command.RuntimeHash, State: "ready"}
}
func v2Stopped(command relayruntime.V2Command) relayruntime.V2Ack {
	ack := v2Ready(command)
	ack.State = "stopped"
	return ack
}
func (f *relayV2Fixture) ready(t *testing.T) []relayruntime.V2Command {
	t.Helper()
	if out := f.sync(t, 0); out.Status != "ready" || len(out.Commands) != 0 {
		t.Fatal("first handshake issued commands")
	}
	if out := f.sync(t, 0); len(out.Commands) != 0 {
		t.Fatal("ingress bypassed first downstream readiness")
	}
	f.sync(t, 1)
	exit := f.sync(t, 1)
	if len(exit.Commands) != 1 || exit.Commands[0].Action != "upsert" || exit.Commands[0].Rule.LeaseUntil != 0 {
		t.Fatal("downstream not explicitly provisioned")
	}
	f.sync(t, 1, v2Ready(exit.Commands[0]))
	entry := f.sync(t, 0)
	if len(entry.Commands) != 1 {
		t.Fatal("ready downstream did not release ingress")
	}
	f.sync(t, 0, v2Ready(entry.Commands[0]))
	return []relayruntime.V2Command{entry.Commands[0], exit.Commands[0]}
}

func TestRelayV2BootstrapAndMultiHopReconnect(t *testing.T) {
	f := newRelayV2Fixture(t)
	first := f.send(t, 0, f.requests[0], 200)
	retry := f.send(t, 0, f.requests[0], 200)
	if first.ControlEpoch == "" || first.ControlEpoch != retry.ControlEpoch || retry.Status != "ready" || len(retry.Commands) != 0 || retry.Revision != 0 {
		t.Fatal("lost bootstrap response cannot retry safely")
	}
	commands := f.ready(t)
	if err := f.app.Store.Update(func(s *State) error {
		for _, agent := range f.agents {
			row, _ := LoadDoc[RelayAgent](s, "relay_agents", agent.ID)
			row.LastSeen = 0
			if err := SaveDoc(s, "relay_agents", agent.ID, row); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	out := f.sync(t, 0, v2Ready(commands[0]))
	if out.Status != "ready" || len(out.Commands) != 0 {
		t.Fatal("ingress-first reconnect changed already applied config")
	}
	if err := f.app.Store.View(func(s *State) error {
		rule, _ := LoadDoc[UserRule](s, "user_rules", f.user.ID+":route")
		view := relayRuleView(s, rule, false, time.Now().UnixMilli())
		if view.ControlStatus != "offline" || view.RuntimeStatus != "last_reported_running" {
			t.Fatal("offline management reported fake real-time health")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if out := f.sync(t, 1, v2Ready(commands[1])); len(out.Commands) != 0 {
		t.Fatal("ordinary reconnect generated commands")
	}
}

func TestRelayV2MetadataAndExplicitStops(t *testing.T) {
	f := newRelayV2Fixture(t)
	commands := f.ready(t)
	if err := f.app.Store.Update(func(s *State) error { return SaveDoc(s, "entitlement_versions", f.user.ID, int64(1)) }); err != nil {
		t.Fatal(err)
	}
	metadata := f.sync(t, 0, v2Ready(commands[0]))
	if len(metadata.Commands) != 1 || metadata.Commands[0].Generation <= commands[0].Generation || metadata.Commands[0].RuntimeHash != commands[0].RuntimeHash || metadata.Commands[0].BillingPeriodID == commands[0].BillingPeriodID {
		t.Fatal("billing metadata not independent from runtime config")
	}
	// A known delayed ACK must not trigger recovery or acknowledge a newer command.
	retry := f.sync(t, 0, v2Ready(commands[0]))
	if retry.Status != "ready" || len(retry.Commands) != 1 || retry.Commands[0].CommandID != metadata.Commands[0].CommandID {
		t.Fatal("old ACK broke reliable command retry")
	}
	f.sync(t, 0, v2Ready(metadata.Commands[0]))
	if err := f.app.Store.Update(func(s *State) error {
		agent, _ := LoadDoc[RelayAgent](s, "relay_agents", f.agents[0].ID)
		agent.Enabled = false
		return SaveDoc(s, "relay_agents", agent.ID, agent)
	}); err != nil {
		t.Fatal(err)
	}
	stop := f.sync(t, 0, v2Ready(metadata.Commands[0]))
	if len(stop.Commands) != 1 || stop.Commands[0].Action != "pause" {
		t.Fatal("disabled node could not receive explicit stop")
	}
	f.sync(t, 0, v2Stopped(stop.Commands[0]))
	if err := f.app.Store.Update(func(s *State) error {
		agent, _ := LoadDoc[RelayAgent](s, "relay_agents", f.agents[0].ID)
		agent.Enabled = true
		return SaveDoc(s, "relay_agents", agent.ID, agent)
	}); err != nil {
		t.Fatal(err)
	}
	resume := f.sync(t, 0, v2Stopped(stop.Commands[0]))
	if len(resume.Commands) != 1 || resume.Commands[0].Action != "upsert" || resume.Commands[0].Generation <= stop.Commands[0].Generation {
		t.Fatal("resume did not supersede the exact paused instance")
	}
}

func TestRelayV2RevokeRetainsResourcesUntilEveryStopACK(t *testing.T) {
	f := newRelayV2Fixture(t)
	f.ready(t)
	now := time.Now().UnixMilli()
	if err := f.app.Store.Update(func(s *State) error {
		rule, _ := LoadDoc[UserRule](s, "user_rules", f.user.ID+":route")
		relayRevoke(&rule, now-relayLeaseMS-1, true)
		if err := SaveDoc(s, "user_rules", f.user.ID+":route", rule); err != nil {
			return err
		}
		return SaveDoc(s, "user_targets", f.user.ID, UserTarget{Hash: "old-target", ResetUntil: now - 1})
	}); err != nil {
		t.Fatal(err)
	}
	assertReserved := func() {
		t.Helper()
		if err := f.app.Store.Update(func(s *State) error {
			if err := relayCleanup(s, now); err != nil {
				return err
			}
			if _, ok := LoadDoc[UserRule](s, "user_rules", f.user.ID+":route"); !ok {
				t.Fatal("rule released without all stop ACKs")
			}
			if _, ok := LoadDoc[UserTarget](s, "user_targets", f.user.ID); !ok {
				t.Fatal("old target lock released before stop")
			}
			if _, err := relayReservePort(s, f.agents[0]); err == nil {
				t.Fatal("unconfirmed port reused")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	assertReserved()
	entry := f.sync(t, 0)
	exit := f.sync(t, 1)
	if len(entry.Commands) != 1 || len(exit.Commands) != 1 || entry.Commands[0].Action != "revoke" || exit.Commands[0].Action != "revoke" {
		t.Fatal("missing reliable revoke commands")
	}
	f.sync(t, 0, v2Stopped(entry.Commands[0]))
	assertReserved()
	f.sync(t, 1, v2Stopped(exit.Commands[0]))
	if err := f.app.Store.View(func(s *State) error {
		if _, ok := LoadDoc[UserRule](s, "user_rules", f.user.ID+":route"); ok {
			t.Fatal("fully stopped rule not archived")
		}
		if _, ok := LoadDoc[UserTarget](s, "user_targets", f.user.ID); ok {
			t.Fatal("confirmed target not released")
		}
		if port, err := relayReservePort(s, f.agents[0]); err != nil || port != 20000 {
			t.Fatal("confirmed port not reusable")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRelayV2UnknownInventoryAndDowngradeFreeze(t *testing.T) {
	for _, kind := range []string{"unknown_ack", "wrong_hash", "leading_revision", "lost_cache", "wrong_epoch"} {
		t.Run(kind, func(t *testing.T) {
			f := newRelayV2Fixture(t)
			commands := f.ready(t)
			in := f.requests[0]
			in.Acks = []relayruntime.V2Ack{v2Ready(commands[0])}
			switch kind {
			case "unknown_ack":
				in.Acks[0].CommandID = commerceID()
			case "wrong_hash":
				in.Acks[0].RuntimeHash = strings.Repeat("a", 64)
			case "leading_revision":
				in.AppliedRevision++
			case "lost_cache":
				in.ControlEpoch = ""
				in.AppliedRevision = 0
				in.Acks = nil
			case "wrong_epoch":
				in.ControlEpoch = commerceID()
			}
			out := f.send(t, 0, in, 200)
			if out.Status != "recovery_required" || len(out.Commands) != 0 {
				t.Fatal("untrusted inventory overwrote node")
			}
			if err := f.app.Store.Update(func(s *State) error {
				rule, _ := LoadDoc[UserRule](s, "user_rules", f.user.ID+":route")
				relayRevoke(&rule, 0, true)
				if err := SaveDoc(s, "user_rules", f.user.ID+":route", rule); err != nil {
					return err
				}
				if err := relayCleanup(s, time.Now().UnixMilli()+commerceDay); err != nil {
					return err
				}
				if _, ok := LoadDoc[UserRule](s, "user_rules", f.user.ID+":route"); !ok {
					t.Fatal("recovery freeze lost resources")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
	f := newRelayV2Fixture(t)
	f.ready(t)
	raw, _ := json.Marshal(relayruntime.SyncRequest{BootID: commerceID(), Sequence: 1})
	r := httptest.NewRequest(http.MethodPost, "/api/relay-agent/sync", bytes.NewReader(raw))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+f.tokens[0])
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	if w.Code != 409 {
		t.Fatal("v2 node silently downgraded to lease sync")
	}
}
