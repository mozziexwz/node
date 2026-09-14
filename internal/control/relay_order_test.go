package control

import (
	"bytes"
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"
)

func TestRelayRoutesStableIdentityOrder(t *testing.T) {
	a, mux, member := commerceTestApp(t)
	admin := &User{ID: "admin", Role: "admin", Status: "active"}
	if err := a.Store.Update(func(s *State) error {
		// Deliberately unsorted insertions and identical names. A name-only or
		// online-first sort would not provide a stable periodic-refresh order.
		for _, id := range []string{"route-z", "route-a", "route-m", "route-b", "route-c"} {
			if err := SaveDoc(s, "relay_agents", id, RelayAgent{ID: id, Address: "8.8.8.8", Capability: "relay", Enabled: true, LastSeen: time.Now().UnixMilli()}); err != nil {
				return err
			}
			route := Route{ID: id, Name: "同名中转线路", Type: "port_forward", EntryAgentID: id, EntryAddress: "8.8.8.8", Enabled: id != "route-c", Version: 7, RateMbps: 20}
			if err := SaveDoc(s, "routes", id, route); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	allIDs := []string{"route-a", "route-b", "route-c", "route-m", "route-z"}
	publicIDs := []string{"route-a", "route-b", "route-m", "route-z"}
	assertRefreshes := func(wantPublic []string, offline bool) {
		t.Helper()
		var before []byte
		if err := a.Store.View(func(s *State) error { before, _ = json.Marshal(s); return nil }); err != nil {
			t.Fatal(err)
		}
		for refresh := 0; refresh < 30; refresh++ {
			for _, audience := range []struct {
				user *User
				path string
				want []string
			}{{admin, "/api/admin/routes", allIDs}, {member, "/api/routes", wantPublic}} {
				response := commerceTestRequest(mux, audience.user, http.MethodGet, audience.path, nil)
				var body struct {
					Routes       []Route `json:"routes"`
					RateScope    string  `json:"rateScope"`
					LeaseSeconds int64   `json:"leaseSeconds"`
				}
				if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &body) != nil {
					t.Fatalf("list failed: HTTP %d", response.Code)
				}
				ids := make([]string, len(body.Routes))
				for i, route := range body.Routes {
					ids[i] = route.ID
					if route.Version != 7 {
						t.Fatal("listing changed route version")
					}
					if route.ID == "route-a" && route.Online == offline {
						t.Fatal("fixture heartbeat change was not reflected")
					}
					if audience.user == member {
						if !route.Enabled || route.EntryAgentID != "" || route.EntryAddress != "" || len(route.EntryAddresses) != 0 || len(route.Hops) != 0 || len(route.Exit.AgentIDs) != 0 || route.RateMbps != member.RateMbps {
							t.Fatal("member visibility, infrastructure redaction or rate semantics changed")
						}
					} else if route.EntryAgentID != route.ID || route.RateMbps != 20 {
						t.Fatal("administrator route view changed")
					}
				}
				if !slices.Equal(ids, audience.want) {
					t.Fatalf("unstable order at refresh %d for %s: got %v, want %v", refresh, audience.path, ids, audience.want)
				}
				if body.RateScope != "per_rule_per_direction" || body.LeaseSeconds != relayLeaseMS/1000 {
					t.Fatal("listing changed runtime/commerce response semantics")
				}
			}
		}
		if err := a.Store.View(func(s *State) error {
			after, _ := json.Marshal(s)
			if !bytes.Equal(before, after) {
				t.Fatal("sorting route responses mutated persisted state")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	assertRefreshes(publicIDs, false)
	if err := a.Store.Update(func(s *State) error {
		for id, name := range map[string]string{"route-a": "ZZZ 改名", "route-z": "AAA 改名"} {
			route, _ := LoadDoc[Route](s, "routes", id)
			route.Name = name
			if err := SaveDoc(s, "routes", id, route); err != nil {
				return err
			}
		}
		agent, _ := LoadDoc[RelayAgent](s, "relay_agents", "route-a")
		agent.LastSeen = 0
		return SaveDoc(s, "relay_agents", agent.ID, agent)
	}); err != nil {
		t.Fatal(err)
	}
	assertRefreshes(publicIDs, true)
	for _, enabled := range []bool{false, true} {
		if err := a.Store.Update(func(s *State) error {
			route, _ := LoadDoc[Route](s, "routes", "route-b")
			route.Enabled = enabled
			return SaveDoc(s, "routes", route.ID, route)
		}); err != nil {
			t.Fatal(err)
		}
		want := publicIDs
		if !enabled {
			want = []string{"route-a", "route-m", "route-z"}
		}
		assertRefreshes(want, true)
	}
}

func TestRelayRoutesSortingPreservesAuthorization(t *testing.T) {
	_, mux, member := commerceTestApp(t)
	for _, test := range []struct {
		user   *User
		path   string
		status int
	}{
		{nil, "/api/routes", http.StatusUnauthorized},
		{nil, "/api/admin/routes", http.StatusForbidden},
		{member, "/api/admin/routes", http.StatusForbidden},
	} {
		response := commerceTestRequest(mux, test.user, http.MethodGet, test.path, nil)
		if response.Code != test.status {
			t.Fatalf("authorization changed for %s: got %d, want %d", test.path, response.Code, test.status)
		}
		var body map[string]json.RawMessage
		if json.Unmarshal(response.Body.Bytes(), &body) != nil || body["routes"] != nil {
			t.Fatal("unauthorized response exposed route list")
		}
	}
}
