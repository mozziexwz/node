package control

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/mozziexwz/node/internal/relayruntime"
)

type probeResponse struct {
	Enabled         bool  `json:"enabled"`
	IntervalSeconds int   `json:"intervalSeconds"`
	RetentionHours  int   `json:"retentionHours"`
	Now             int64 `json:"now"`
	Routes          []struct {
		RouteID   string     `json:"routeId"`
		RouteName string     `json:"routeName"`
		Level     int        `json:"level"`
		State     string     `json:"state"`
		Samples   [][2]int64 `json:"samples"`
	} `json:"routes"`
}

// probeFixture is a ready two-node route whose exit node can probe.
func probeFixture(t *testing.T) *relayV2Fixture {
	t.Helper()
	f := newRelayV2Fixture(t)
	f.requests[1].Capabilities = append(f.requests[1].Capabilities, relayruntime.TargetProbeCapability)
	f.ready(t)
	if err := f.app.Store.Update(func(s *State) error {
		rule, _ := LoadDoc[UserRule](s, "user_rules", f.user.ID+":route")
		rule.RouteName, rule.TargetHost, rule.TargetPort = "测试线路", "1.1.1.1", 443
		return SaveDoc(s, "user_rules", f.user.ID+":route", rule)
	}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *relayV2Fixture) probeView(t *testing.T, user *User) probeResponse {
	t.Helper()
	w := commerceTestRequest(f.mux, user, http.MethodGet, "/api/user/probe", nil)
	var out probeResponse
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatalf("probe view: %d %s", w.Code, w.Body.String())
	}
	return out
}

// answerProbe delivers the exit node's result for the one scheduled probe.
func (f *relayV2Fixture) answerProbe(t *testing.T, status string, latency int64, want int) {
	t.Helper()
	if out := f.sync(t, 0); len(out.TargetProbes) != 0 {
		t.Fatal("entry node received a probe meant for the exit")
	}
	challenge := f.sync(t, 1)
	if len(challenge.TargetProbes) != 1 || challenge.TargetProbes[0].RuleID != f.ruleID || challenge.TargetProbes[0].Target != "1.1.1.1:443" {
		t.Fatalf("exit node did not receive the rule's own target: %+v", challenge.TargetProbes)
	}
	f.requests[1].TargetProbeResults = []relayruntime.TargetProbeResult{{ID: challenge.TargetProbes[0].ID, RuleID: f.ruleID, Status: status, LatencyMS: latency}}
	f.sync(t, 1)
	f.requests[1].TargetProbeResults = nil
	deadline := time.Now().Add(2 * time.Second)
	for len(f.app.relayProbe.series(f.ruleID, time.Now().UnixMilli())) != want {
		if time.Now().After(deadline) {
			t.Fatalf("result was not recorded: %v", f.app.relayProbe.series(f.ruleID, time.Now().UnixMilli()))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// tickAt runs the scheduler at a later moment. The nodes last synchronised at
// the real time, so they are first marked as seen at that moment too.
func (f *relayV2Fixture) tickAt(t *testing.T, at time.Time) {
	t.Helper()
	if err := f.app.Store.Update(func(s *State) error {
		for _, node := range f.agents {
			agent, _ := LoadDoc[RelayAgent](s, "relay_agents", node.ID)
			agent.LastSeen = max(agent.LastSeen, at.UnixMilli())
			if err := SaveDoc(s, "relay_agents", agent.ID, agent); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.app.relayProbeTick(context.Background(), at)
}

func probeJobs(a *App) int {
	a.relayDiagnostics.mu.Lock()
	defer a.relayDiagnostics.mu.Unlock()
	return len(a.relayDiagnostics.jobs)
}

func TestRelayProbeSchedulesExitAndKeepsHistory(t *testing.T) {
	f := probeFixture(t)
	ctx := context.Background()
	start := time.Now()
	f.app.relayProbeTick(ctx, start)
	if probeJobs(f.app) != 1 {
		t.Fatal("a ready rule was not probed")
	}
	f.answerProbe(t, "success", 23, 1)

	// Not due again until the interval has passed.
	f.tickAt(t, start.Add(time.Minute))
	if probeJobs(f.app) != 0 {
		t.Fatal("rule was probed again before its interval")
	}
	f.tickAt(t, start.Add(2*time.Minute+time.Second))
	if probeJobs(f.app) != 1 {
		t.Fatal("rule was not probed after its interval")
	}
	f.answerProbe(t, "failed", 0, 2)

	// A node that is not running the rule says nothing about the member's node.
	f.tickAt(t, start.Add(5*time.Minute))
	challenge := f.sync(t, 1)
	if len(challenge.TargetProbes) != 1 {
		t.Fatal("third probe was not issued")
	}
	f.requests[1].TargetProbeResults = []relayruntime.TargetProbeResult{{ID: challenge.TargetProbes[0].ID, RuleID: f.ruleID, Status: "unavailable"}}
	f.sync(t, 1)
	f.requests[1].TargetProbeResults = nil
	deadline := time.Now().Add(2 * time.Second)
	for probeJobs(f.app) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("answered probe was not released")
		}
		time.Sleep(5 * time.Millisecond)
	}

	view := f.probeView(t, f.user)
	if !view.Enabled || view.IntervalSeconds != 120 || view.RetentionHours != 24 || len(view.Routes) != 1 {
		t.Fatalf("unexpected view: %+v", view)
	}
	route := view.Routes[0]
	if route.RouteID != "route" || route.RouteName != "测试线路" || route.State != "active" || route.Level != 1 {
		t.Fatalf("unexpected route: %+v", route)
	}
	if len(route.Samples) != 2 || route.Samples[0][1] != 23 || route.Samples[1][1] != -1 || route.Samples[0][0] > route.Samples[1][0] {
		t.Fatalf("unexpected samples: %v", route.Samples)
	}
	// Another member never sees this rule or its history.
	if other := f.probeView(t, &User{ID: "someone-else", Role: "member", Status: "active"}); len(other.Routes) != 0 {
		t.Fatalf("history leaked to another member: %+v", other.Routes)
	}
	if w := commerceTestRequest(f.mux, nil, http.MethodGet, "/api/user/probe", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous request was answered: %d", w.Code)
	}
}

func TestRelayProbeLeavesSlotsForDiagnosis(t *testing.T) {
	f := probeFixture(t)
	exit := f.agents[1].ID
	var held []*relayDiagnosticJob
	for _, member := range []string{"member-a", "member-b"} {
		job, err := f.app.relayDiagnostics.begin(member, f.ruleID, exit, "1.1.1.1:443")
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, job)
	}
	start := time.Now()
	f.app.relayProbeTick(context.Background(), start)
	if probeJobs(f.app) != 2 {
		t.Fatal("a scheduled probe took a slot while two diagnoses were waiting")
	}
	// The members' own diagnoses still have room.
	job, err := f.app.relayDiagnostics.begin("member-c", f.ruleID, exit, "1.1.1.1:443")
	if err != nil {
		t.Fatalf("diagnosis was refused: %v", err)
	}
	f.app.relayDiagnostics.end(job.id)
	for _, job := range held {
		f.app.relayDiagnostics.end(job.id)
	}
	// Once they are done the rule is probed, after the per-node gap.
	f.tickAt(t, start.Add(time.Second))
	if probeJobs(f.app) != 0 {
		t.Fatal("node was asked again inside its gap")
	}
	f.tickAt(t, start.Add(relayProbeAgentGap+time.Second))
	if probeJobs(f.app) != 1 {
		t.Fatal("rule was not probed once the slots were free")
	}
	f.answerProbe(t, "success", 9, 1)
}

func TestRelayProbeFollowsRuleStateAndSwitch(t *testing.T) {
	f := probeFixture(t)
	key := f.user.ID + ":route"
	setState := func(state string) {
		t.Helper()
		if err := f.app.Store.Update(func(s *State) error {
			rule, _ := LoadDoc[UserRule](s, "user_rules", key)
			rule.State = state
			return SaveDoc(s, "user_rules", key, rule)
		}); err != nil {
			t.Fatal(err)
		}
	}
	setState("paused")
	start := time.Now()
	f.app.relayProbeTick(context.Background(), start)
	if probeJobs(f.app) != 0 {
		t.Fatal("a paused rule was probed")
	}
	if view := f.probeView(t, f.user); len(view.Routes) != 1 || view.Routes[0].State != "paused" {
		t.Fatalf("paused rule not reported: %+v", view.Routes)
	}
	setState("active")
	// An exit node that never offered the probe is reported as such.
	if err := f.app.Store.Update(func(s *State) error {
		agent, _ := LoadDoc[RelayAgent](s, "relay_agents", f.agents[1].ID)
		agent.Capabilities = append([]string(nil), relayruntime.V2Capabilities...)
		return SaveDoc(s, "relay_agents", agent.ID, agent)
	}); err != nil {
		t.Fatal(err)
	}
	if view := f.probeView(t, f.user); view.Routes[0].State != "unsupported" {
		t.Fatalf("old exit node not reported: %+v", view.Routes)
	}
	f.sync(t, 1) // the node reports its capabilities again
	if view := f.probeView(t, f.user); view.Routes[0].State != "active" {
		t.Fatalf("ready rule not reported: %+v", view.Routes)
	}

	if err := f.app.Store.Update(func(s *State) error {
		s.Settings["relayProbe"] = RelayProbeSettings{Enabled: false, IntervalMinutes: 5}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.tickAt(t, start.Add(time.Minute))
	if probeJobs(f.app) != 0 {
		t.Fatal("probing continued after it was switched off")
	}
	if view := f.probeView(t, f.user); view.Enabled || len(view.Routes) != 0 {
		t.Fatalf("switched-off view still lists routes: %+v", view)
	}
}

func TestRelayProbeSettingsAreValidated(t *testing.T) {
	a, _, _ := commerceTestApp(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/admin/settings", a.adminSettings)
	admin := &User{ID: "admin-context", Role: "admin", Status: "active"}
	for _, minutes := range []int{0, 31} {
		if w := commerceTestRequest(mux, admin, http.MethodPut, "/api/admin/settings", map[string]any{"relayProbe": map[string]any{"enabled": true, "intervalMinutes": minutes}}); w.Code != http.StatusBadRequest {
			t.Fatalf("interval %d accepted: %d", minutes, w.Code)
		}
	}
	w := commerceTestRequest(mux, admin, http.MethodPut, "/api/admin/settings", map[string]any{"relayProbe": map[string]any{"enabled": false, "intervalMinutes": 10}})
	var out struct {
		RelayProbe RelayProbeSettings `json:"relayProbe"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.RelayProbe != (RelayProbeSettings{Enabled: false, IntervalMinutes: 10}) {
		t.Fatalf("settings not saved: %d %s", w.Code, w.Body.String())
	}
	if w := commerceTestRequest(mux, &User{ID: "buyer", Role: "member", Status: "active"}, http.MethodPut, "/api/admin/settings", map[string]any{"relayProbe": map[string]any{"enabled": true, "intervalMinutes": 2}}); w.Code != http.StatusForbidden {
		t.Fatalf("member changed the settings: %d", w.Code)
	}
	// A stored value outside the bounds falls back to the defaults.
	_ = a.Store.Update(func(s *State) error {
		s.Settings["relayProbe"] = map[string]any{"enabled": true, "intervalMinutes": 999}
		return nil
	})
	_ = a.Store.View(func(s *State) error {
		if relayProbeSettings(s) != defaultRelayProbeSettings() {
			t.Fatalf("out-of-range setting used: %+v", relayProbeSettings(s))
		}
		return nil
	})
}

func TestRelayProbeHistorySurvivesRestartAndAgesOut(t *testing.T) {
	path := filepath.Join(t.TempDir(), relayProbeFile)
	now := time.Now().UnixMilli()
	hour := time.Hour.Milliseconds()
	var first relayProbeMonitor
	first.mu.Lock()
	first.initLocked()
	first.addLocked("rule-kept-000000", probeSample{At: now - 30*hour, Latency: 40})
	first.addLocked("rule-kept-000000", probeSample{At: now - 2*hour, Latency: 41})
	first.addLocked("rule-kept-000000", probeSample{At: now - hour, Latency: -1})
	first.samples["rule-old-0000000"] = []probeSample{{At: now - 30*hour, Latency: 7}}
	first.mu.Unlock()
	if got := first.series("rule-kept-000000", now); len(got) != 2 || got[0][1] != 41 || got[1][1] != -1 {
		t.Fatalf("day-old result was kept: %v", got)
	}
	first.save(path)

	var second relayProbeMonitor
	second.load(path, now)
	if got := second.series("rule-kept-000000", now); len(got) != 2 || got[0][1] != 41 || got[1][1] != -1 {
		t.Fatalf("history lost across restart: %v", got)
	}
	if got := second.series("rule-old-0000000", now); len(got) != 0 {
		t.Fatalf("aged-out rule restored: %v", got)
	}
	// A missing or damaged snapshot only means an empty history.
	var empty relayProbeMonitor
	empty.load(filepath.Join(t.TempDir(), "missing.gob"), now)
	if got := empty.series("rule-kept-000000", now); len(got) != 0 {
		t.Fatalf("history from nowhere: %v", got)
	}
	second.mu.Lock()
	second.pruneLocked(now + 30*hour)
	left := len(second.samples)
	second.mu.Unlock()
	if left != 0 {
		t.Fatal("aged-out rules were not forgotten")
	}
}
