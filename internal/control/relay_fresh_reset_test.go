package control

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const relayFreshResetConfirmation = "INTERRUPT_AND_REPLACE_RELAY"

func emptyOnlineRelayFixture(t *testing.T) *relayV2Fixture {
	t.Helper()
	f := newRelayV2Fixture(t)
	if err := f.app.Store.Update(func(s *State) error {
		DeleteDoc(s, "user_rules", f.user.ID+":route")
		DeleteDoc(s, "routes", "route")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.sync(t, 0)
	f.sync(t, 0)
	return f
}

func TestRelayFreshResetReplacesOnlineEmptyNodeAtomically(t *testing.T) {
	f := emptyOnlineRelayFixture(t)
	admin := &User{ID: "admin", Role: "admin", Status: "active"}
	path := "/api/admin/relay-agents/" + f.agents[0].ID + "/fresh-reset"
	for _, tc := range []struct {
		name string
		user *User
		body any
		want int
	}{
		{"anonymous", nil, map[string]string{"confirm": relayFreshResetConfirmation}, 403},
		{"member", f.user, map[string]string{"confirm": relayFreshResetConfirmation}, 403},
		{"missing explicit confirmation", admin, map[string]string{}, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := commerceTestRequest(f.mux, tc.user, http.MethodPost, path, tc.body)
			if w.Code != tc.want || strings.Contains(w.Body.String(), "enrollmentToken") {
				t.Fatalf("unsafe reset: %d %s", w.Code, w.Body.String())
			}
		})
	}
	w := commerceTestRequest(f.mux, admin, http.MethodPost, path, map[string]string{"confirm": relayFreshResetConfirmation})
	if w.Code != 200 {
		t.Fatalf("online empty v2 relay cannot be reset: %d %s", w.Code, w.Body.String())
	}
	var out struct {
		OldAgentID      string     `json:"oldAgentId"`
		Agent           RelayAgent `json:"agent"`
		EnrollmentToken string     `json:"enrollmentToken"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.OldAgentID != f.agents[0].ID || out.Agent.ID == "" || out.Agent.ID == out.OldAgentID || out.EnrollmentToken == "" || out.Agent.Address != f.agents[0].Address || out.Agent.TokenHash != "" || out.Agent.EnrollmentHash != "" {
		t.Fatalf("bad replacement response: %+v", out.Agent)
	}
	if err := f.app.Store.View(func(s *State) error {
		if _, alive := LoadDoc[RelayAgent](s, "relay_agents", out.OldAgentID); alive {
			t.Fatal("old identity remains active")
		}
		if _, retired := s.Docs[relayRetirementCollection][out.OldAgentID]; !retired {
			t.Fatal("old v2 identity lacks retirement proof")
		}
		newAgent, found := LoadDoc[RelayAgent](s, "relay_agents", out.Agent.ID)
		if !found || newAgent.EnrollmentHash != commerceHash(out.EnrollmentToken) || newAgent.TokenHash != "" || newAgent.LastSeen != 0 {
			t.Fatal("replacement identity or one-time credential not persisted")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.send(t, 0, f.requests[0], 401)
	registration := commerceTestRequest(f.mux, nil, http.MethodPost, "/api/relay-agent/register", map[string]string{"enrollmentToken": out.EnrollmentToken})
	if registration.Code != 200 || !strings.Contains(registration.Body.String(), out.Agent.ID) {
		t.Fatalf("replacement registration failed: %d %s", registration.Code, registration.Body.String())
	}
	second := commerceTestRequest(f.mux, admin, http.MethodPost, path, map[string]string{"confirm": relayFreshResetConfirmation})
	if second.Code != 409 || strings.Contains(second.Body.String(), "enrollmentToken") {
		t.Fatalf("repeated reset permitted: %d %s", second.Code, second.Body.String())
	}
}

func TestRelayFreshResetReplacesNodeAfterEveryOldRuleStopped(t *testing.T) {
	f, _ := retirementFixture(t)
	admin := &User{ID: "admin", Role: "admin", Status: "active"}
	w := commerceTestRequest(f.mux, admin, http.MethodPost,
		"/api/admin/relay-agents/"+f.agents[0].ID+"/fresh-reset",
		map[string]string{"confirm": relayFreshResetConfirmation})
	if w.Code != 200 {
		t.Fatalf("terminally stopped former business node cannot be reset: %d %s", w.Code, w.Body.String())
	}
}

func TestRelayFreshResetRejectsLinkedAndUnprovenNodeWithoutPartialMutation(t *testing.T) {
	admin := &User{ID: "admin", Role: "admin", Status: "active"}
	for _, scenario := range []string{"linked", "unproven", "disabled"} {
		t.Run(scenario, func(t *testing.T) {
			f := newRelayV2Fixture(t)
			switch scenario {
			case "linked":
				f.sync(t, 0)
				f.sync(t, 0)
			case "unproven":
				f.ready(t)
				if err := f.app.Store.Update(func(s *State) error {
					DeleteDoc(s, "user_rules", f.user.ID+":route")
					DeleteDoc(s, "routes", "route")
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			case "disabled":
				if err := f.app.Store.Update(func(s *State) error {
					DeleteDoc(s, "user_rules", f.user.ID+":route")
					DeleteDoc(s, "routes", "route")
					row, _ := LoadDoc[RelayAgent](s, "relay_agents", f.agents[0].ID)
					row.Enabled = false
					return SaveDoc(s, "relay_agents", row.ID, row)
				}); err != nil {
					t.Fatal(err)
				}
			}
			before := recoveryState(t, f.app)
			w := commerceTestRequest(f.mux, admin, http.MethodPost, "/api/admin/relay-agents/"+f.agents[0].ID+"/fresh-reset", map[string]string{"confirm": relayFreshResetConfirmation})
			if w.Code != 409 || strings.Contains(w.Body.String(), "enrollmentToken") {
				t.Fatalf("unsafe reset accepted: %d %s", w.Code, w.Body.String())
			}
			if recoveryState(t, f.app) != before {
				t.Fatal("rejected reset changed state")
			}
		})
	}
}

func TestRelayFreshResetConcurrentRequestsOnlyOneWins(t *testing.T) {
	f := emptyOnlineRelayFixture(t)
	admin := &User{ID: "admin", Role: "admin", Status: "active"}
	path := "/api/admin/relay-agents/" + f.agents[0].ID + "/fresh-reset"
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- commerceTestRequest(f.mux, admin, http.MethodPost, path, map[string]string{"confirm": relayFreshResetConfirmation}).Code
		}()
	}
	wg.Wait()
	close(codes)
	success, rejected := 0, 0
	for code := range codes {
		if code == 200 {
			success++
		} else if code == 409 {
			rejected++
		} else {
			t.Fatalf("unexpected HTTP %d", code)
		}
	}
	if success != 1 || rejected != 1 {
		t.Fatalf("success %d, rejected %d", success, rejected)
	}
}

func TestRelayFreshResetUsesSessionCSRFBoundary(t *testing.T) {
	a, err := New(Config{DataDir: t.TempDir(), PublicURL: "https://identity.test", AdminEmail: "99999999@qq.com", AdminPassword: "test-admin-password-123"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	mux := http.NewServeMux()
	a.RegisterIdentity(mux)
	a.RegisterRelay(mux)
	h := a.Authenticate(mux)
	cookie, csrf := identityLoginAdmin(t, h)
	id := commerceID()
	if err := a.Store.Update(func(s *State) error {
		return SaveDoc(s, "relay_agents", id, RelayAgent{ID: id, Name: "new", Address: "8.8.8.8", Enabled: true, Capability: "relay"})
	}); err != nil {
		t.Fatal(err)
	}
	path := "/api/admin/relay-agents/" + id + "/fresh-reset"
	body := map[string]string{"confirm": relayFreshResetConfirmation}
	if w := identityRequest(h, http.MethodPost, path, body, cookie, ""); w.Code != 403 {
		t.Fatalf("missing CSRF: %d", w.Code)
	}
	if w := identityRequest(h, http.MethodPost, path, body, cookie, "wrong"); w.Code != 403 {
		t.Fatalf("wrong CSRF: %d", w.Code)
	}
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	r.Header.Set("Origin", "https://evil.test")
	r.Header.Set("X-CSRF-Token", csrf)
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("wrong origin: %d", w.Code)
	}
	// No affected service is required: the valid request exercises the same
	// authenticated/CSRF-protected route and atomic replacement path.
	if w := identityRequest(h, http.MethodPost, path, body, cookie, csrf); w.Code != 200 {
		t.Fatalf("valid session: %d %s", w.Code, w.Body.String())
	}
}
