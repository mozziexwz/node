package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mozziexwz/node/internal/executor"
)

func TestRelayLifecycleEmptyRestoreCanEnroll(t *testing.T) {
	f := emptyOnlineRelayFixture(t)
	// Start from an actually empty relay domain, not a lost-inventory fixture.
	if err := f.app.Store.Update(func(s *State) error {
		for _, name := range []string{"relay_agents", "relay_v2_catalogs", "relay_v2_control"} {
			delete(s.Docs, name)
		}
		if err := prepareRestoredRelay(s, time.Now().UnixMilli(), true); err != nil {
			return err
		}
		if restoredRelayRecoveryRequired(s) {
			t.Fatal("empty restore froze relay domain")
		}
		return SaveDoc(s, "relay_agents", f.agents[0].ID, f.agents[0])
	}); err != nil {
		t.Fatal(err)
	}
	f.requests[0].ControlEpoch = ""
	f.requests[0].AppliedRevision = 0
	if out := f.sync(t, 0); out.Status != "ready" {
		t.Fatal(out.Status)
	}
}

func TestRelayLifecycleRejectedRegistrationCanBeCancelledOnlyWithoutHistory(t *testing.T) {
	for _, scenario := range []string{"empty", "issued", "unknown_history", "route"} {
		t.Run(scenario, func(t *testing.T) {
			f := newRelayV2Fixture(t)
			if err := f.app.Store.Update(func(s *State) error {
				delete(s.Docs, "user_rules")
				delete(s.Docs, "routes")
				for _, agent := range f.agents {
					agent.LastSeen = 0
					if err := SaveDoc(s, "relay_agents", agent.ID, agent); err != nil {
						return err
					}
				}
				return SaveDoc(s, "relay_v2_control", "default", RelayV2Control{Epoch: commerceID(), RecoveryRequired: true})
			}); err != nil {
				t.Fatal(err)
			}
			if out := f.sync(t, 0); out.Status != "recovery_required" {
				t.Fatal(out.Status)
			}
			if err := f.app.Store.Update(func(s *State) error {
				switch scenario {
				case "issued":
					c, _ := LoadDoc[RelayV2Catalog](s, "relay_v2_catalogs", f.agents[0].ID)
					c.Revision = 1
					return SaveDoc(s, "relay_v2_catalogs", c.AgentID, c)
				case "unknown_history":
					return SaveDoc(s, "relay_v2_command_history", "broken", map[string]string{"unknown": "value"})
				case "route":
					return SaveDoc(s, "routes", "linked", Route{ID: "linked", EntryAgentID: f.agents[0].ID})
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			admin := &User{ID: "admin", Role: "admin", Status: "active"}
			w := commerceTestRequest(f.mux, admin, http.MethodDelete, "/api/admin/relay-agents/"+f.agents[0].ID, nil)
			if scenario != "empty" {
				if w.Code != 409 {
					t.Fatalf("unsafe cancellation %d", w.Code)
				}
				return
			}
			if w.Code != 200 {
				t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
			}
			f.send(t, 0, f.requests[0], 401)
			if err := f.app.Store.View(func(s *State) error {
				if restoredRelayRecoveryRequired(s) {
					t.Fatal("empty latch not released")
				}
				if _, err := validateRelayRetirements(s, time.Now().UnixMilli()); err != nil {
					t.Fatal(err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := f.app.Store.Update(func(s *State) error {
				if err := prepareRestoredRelay(s, time.Now().UnixMilli(), true); err != nil {
					return err
				}
				if restoredRelayRecoveryRequired(s) {
					t.Fatal("cancelled empty registration relocked restore")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRelayLifecycleFrontUsesCurrentReadyV2(t *testing.T) {
	f := newRelayV2Fixture(t)
	f.ready(t)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := f.app.relayFrontReady(ctx, f.user.ID+":route"); err != nil {
		t.Fatal(err)
	}
}

func TestRelayLifecycleArchivePreservesRuntimeAndRevokesCredentials(t *testing.T) {
	f := newRelayV2Fixture(t)
	f.ready(t)
	admin := &User{ID: "admin", Role: "admin", Status: "active"}
	path := "/api/admin/relay-agents/" + f.agents[0].ID + "/archive"
	body := map[string]any{"archived": true, "confirm": "KEEP_REMOTE_STATE_AND_RESERVATIONS"}
	if w := commerceTestRequest(f.mux, f.user, http.MethodPost, path, body); w.Code != 403 {
		t.Fatal("member archive accepted")
	}
	if w := commerceTestRequest(f.mux, admin, http.MethodPost, path, body); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if err := f.app.Store.View(func(s *State) error {
		a, _ := LoadDoc[RelayAgent](s, "relay_agents", f.agents[0].ID)
		rule, _ := LoadDoc[UserRule](s, "user_rules", f.user.ID+":route")
		route, _ := LoadDoc[Route](s, "routes", "route")
		if !a.Archived || a.TokenHash != "" || a.Enabled || len(rule.Segments) != 2 || rule.State != "active" || !route.AdmissionsClosed {
			t.Fatal("archive lost history or claims stopped")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.send(t, 0, f.requests[0], 401)
	for query, want := range map[string]bool{"": false, "?includeArchived=1": true} {
		w := commerceTestRequest(f.mux, admin, http.MethodGet, "/api/admin/relay-agents"+query, nil)
		if strings.Contains(w.Body.String(), f.agents[0].ID) != want {
			t.Fatal("archive list visibility")
		}
	}
	body["archived"] = false
	if w := commerceTestRequest(f.mux, admin, http.MethodPost, path, body); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	f.send(t, 0, f.requests[0], 401) // Restoring visibility cannot resurrect credentials.
}

func TestRelayLifecycleAsyncFrontFailureRecorded(t *testing.T) {
	f := newRelayV2Fixture(t)
	f.ready(t)
	var rule UserRule
	if err := f.app.Store.Update(func(s *State) error {
		rule, _ = LoadDoc[UserRule](s, "user_rules", f.user.ID+":route")
		rule.State = "awaiting_front"
		rule.FrontTaskID = "front-test-task"
		return SaveDoc(s, "user_rules", f.user.ID+":route", rule)
	}); err != nil {
		t.Fatal(err)
	}
	called := make(chan struct{})
	f.app.SetFrontProvisioner(func(ctx context.Context, user string, ssh executor.SSH, host string, port int) (executor.Hop, error) {
		close(called)
		return executor.Hop{}, errors.New("simulated partial failure")
	})
	f.app.startRelayFront(rule, executor.SSH{}, []byte("private-client-config"))
	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("background provision did not start")
	}
	f.app.relayWorkWG.Wait()
	if err := f.app.Store.View(func(s *State) error {
		current, _ := LoadDoc[UserRule](s, "user_rules", f.user.ID+":route")
		if current.State != "revoking" || current.FrontStatus != "check_customer_vps" || current.SealedConfig != "" {
			t.Fatal("partial failure hidden")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRelayLifecycleMetadataAndAdmissionDoNotChangeRuntime(t *testing.T) {
	f := newRelayV2Fixture(t)
	commands := f.ready(t)
	var route Route
	if err := f.app.Store.View(func(s *State) error { route, _ = LoadDoc[Route](s, "routes", "route"); return nil }); err != nil {
		t.Fatal(err)
	}
	route.Name = "Renamed"
	route.AdmissionsClosed = true
	w := commerceTestRequest(f.mux, &User{ID: "admin", Role: "admin", Status: "active"}, http.MethodPut, "/api/admin/routes/route", route)
	if w.Code != 200 {
		t.Fatalf("rename: %d %s", w.Code, w.Body.String())
	}
	if out := f.sync(t, 0, v2Ready(commands[0])); len(out.Commands) != 0 {
		t.Fatal("metadata stopped existing forwarding")
	}
	route.EntryAgentID = f.agents[1].ID
	w = commerceTestRequest(f.mux, &User{ID: "admin", Role: "admin", Status: "active"}, http.MethodPut, "/api/admin/routes/route", route)
	if w.Code != 409 {
		t.Fatal("unsafe topology edit accepted")
	}
}

func TestRelayLifecycleDuplicatePrimaryAddressRejected(t *testing.T) {
	_, mux, _ := commerceTestApp(t)
	admin := &User{ID: "admin", Role: "admin", Status: "active"}
	in := RelayAgent{Name: "Primary", Address: "8.8.8.8", Enabled: true, PortRanges: []PortRange{{Start: 23456, End: 23456}}}
	for _, want := range []int{200, 409} {
		w := commerceTestRequest(mux, admin, http.MethodPost, "/api/admin/relay-agents", in)
		if w.Code != want {
			t.Fatalf("duplicate: %d %s", w.Code, w.Body.String())
		}
	}
}

func TestRelayLifecycleOverlappingAddressPortPoolReserved(t *testing.T) {
	s := newState()
	first := RelayAgent{ID: "first", Address: "8.8.8.8", Addresses: []string{"9.9.9.9"}, PortRanges: []PortRange{{Start: 25000, End: 25000}}}
	second := RelayAgent{ID: "second", Address: "1.1.1.1", Addresses: []string{"9.9.9.9"}, PortRanges: first.PortRanges}
	SaveDoc(s, "relay_agents", first.ID, first)
	SaveDoc(s, "relay_agents", second.ID, second)
	if _, err := relayReservePortWithReader(s, second, strings.NewReader(""), map[string]int{first.ID: 25000}); err == nil || err.Error() != "Agent端口池已耗尽" {
		t.Fatal("same-operation shared bind address was not reserved before random selection")
	}
	rule := UserRule{ID: "rule", UserID: "u", RouteID: "r", Segments: []RelaySegment{{AgentID: first.ID}}}
	rule.Segments[0].Runtime.ListenPort = 25000
	SaveDoc(s, "user_rules", "u:r", rule)
	if _, err := relayReservePort(s, second); err == nil {
		t.Fatal("shared bind address port reused")
	}
}

func TestRelayLifecycleRecoveryCannotAdvertiseRouteOnline(t *testing.T) {
	f := newRelayV2Fixture(t)
	f.ready(t)
	if err := f.app.Store.Update(func(s *State) error {
		agent, _ := LoadDoc[RelayAgent](s, "relay_agents", f.agents[0].ID)
		agent.ReconcileState = "recovery_required"
		return SaveDoc(s, "relay_agents", agent.ID, agent)
	}); err != nil {
		t.Fatal(err)
	}
	w := commerceTestRequest(f.mux, &User{ID: "admin", Role: "admin", Status: "active"}, http.MethodGet, "/api/admin/routes", nil)
	var out struct {
		Routes []Route `json:"routes"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || len(out.Routes) != 1 || out.Routes[0].Online {
		t.Fatalf("false online: %s", w.Body.String())
	}
}

func TestRelayLifecycleEnrollmentRetryAndRestoreRevocation(t *testing.T) {
	app, mux, _ := commerceTestApp(t)
	admin := &User{ID: "admin", Role: "admin", Status: "active"}
	w := commerceTestRequest(mux, admin, http.MethodPost, "/api/admin/relay-agents", RelayAgent{Name: "fresh", Address: "8.8.4.4", Enabled: true, PortRanges: []PortRange{{Start: 25000, End: 25010}}})
	var created struct {
		Agent RelayAgent `json:"agent"`
		Token string     `json:"enrollmentToken"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &created) != nil || created.Token == "" {
		t.Fatalf("create %s", w.Body.String())
	}
	check := func(want string) {
		t.Helper()
		res := commerceTestRequest(mux, nil, http.MethodPost, "/api/relay-agent/enrollment-check", map[string]string{"enrollmentToken": created.Token})
		var out struct {
			Status string `json:"status"`
		}
		if res.Code != 200 || json.Unmarshal(res.Body.Bytes(), &out) != nil || out.Status != want {
			t.Fatalf("preflight %d %s", res.Code, res.Body.String())
		}
	}
	check("ready")
	check("ready") // A preflight must not consume the credential.
	request := map[string]string{"enrollmentToken": created.Token, "requestId": commerceID()}
	first := commerceTestRequest(mux, nil, http.MethodPost, "/api/relay-agent/register", request)
	retry := commerceTestRequest(mux, nil, http.MethodPost, "/api/relay-agent/register", request)
	if first.Code != 200 || retry.Code != 200 || first.Body.String() != retry.Body.String() {
		t.Fatalf("lost response retry %d %d", first.Code, retry.Code)
	}
	check("invalid_token")
	other := map[string]string{"enrollmentToken": created.Token, "requestId": commerceID()}
	if res := commerceTestRequest(mux, nil, http.MethodPost, "/api/relay-agent/register", other); res.Code != 401 {
		t.Fatal("different installation stole credential")
	}
	list := commerceTestRequest(mux, admin, http.MethodGet, "/api/admin/relay-agents", nil)
	var view struct {
		Agents []RelayAgent `json:"agents"`
	}
	if json.Unmarshal(list.Body.Bytes(), &view) != nil || len(view.Agents) != 1 {
		t.Fatal("list")
	}
	if view.Agents[0].EnrollmentSealedToken != "" || view.Agents[0].EnrollmentAttemptID != "" || view.Agents[0].EnrollmentRetryHash != "" {
		t.Fatal("retry secrets disclosed")
	}
	if err := app.Store.Update(func(s *State) error { return isolateRestoredState(s, true) }); err != nil {
		t.Fatal(err)
	}
	if err := app.Store.View(func(s *State) error {
		a, _ := LoadDoc[RelayAgent](s, "relay_agents", created.Agent.ID)
		if a.TokenHash != "" || a.EnrollmentSealedToken != "" || a.EnrollmentRetryHash != "" {
			t.Fatal("restore retained credentials")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if res := commerceTestRequest(mux, nil, http.MethodPost, "/api/relay-agent/register", request); res.Code != 401 {
		t.Fatal("restored retry credential accepted")
	}
}

func TestRelayLifecycleNewNodeNotBlockedByOtherRestoredNode(t *testing.T) {
	f := emptyOnlineRelayFixture(t)
	epoch := commerceID()
	if err := f.app.Store.Update(func(s *State) error {
		if err := SaveDoc(s, "relay_v2_control", "default", RelayV2Control{Epoch: epoch, RecoveryRequired: true}); err != nil {
			return err
		}
		for i, original := range f.agents {
			a, _ := LoadDoc[RelayAgent](s, "relay_agents", original.ID)
			if i == 0 {
				a.EnrollmentEpoch = epoch
				a.LastSeen, a.BootID, a.KeepLastConfirmed = 0, "", false
				DeleteDoc(s, "relay_v2_catalogs", a.ID)
			} else {
				a.ReconcileState = "recovery_required"
			}
			if err := SaveDoc(s, "relay_agents", a.ID, a); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.requests[0].ControlEpoch = ""
	f.requests[0].AppliedRevision = 0
	if out := f.sync(t, 0); out.Status != "ready" {
		t.Fatalf("independent new node: %s", out.Status)
	}
	if out := f.sync(t, 1); out.Status != "recovery_required" {
		t.Fatalf("old identity lost isolation: %s", out.Status)
	}
}
