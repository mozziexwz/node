package control

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestRelayV2AccountingReadRequiresAdminAndKeepsHistoricalData(t *testing.T) {
	a, mux, member := commerceTestApp(t)
	admin := &User{ID: "admin", Role: "admin", Status: "active"}
	if err := a.Store.Update(func(s *State) error {
		for _, p := range []RelayBillingPeriodV2{{UserID: "z", EntitlementVersion: 2, ConfirmedBytes: 100, ReviewBytes: 50}, {UserID: "a", EntitlementVersion: 1, ConfirmedBytes: 200, ReviewBytes: 20}} {
			if err := SaveDoc(s, "relay_v2_billing_periods", p.UserID, p); err != nil {
				return err
			}
		}
		return SaveDoc(s, "relay_agents", "agent", RelayAgent{ID: "agent", AccountingDegraded: true, TokenHash: "private-must-not-leak", Address: "private-target-must-not-leak"})
	}); err != nil {
		t.Fatal(err)
	}
	for _, user := range []*User{nil, member} {
		if out := commerceTestRequest(mux, user, http.MethodGet, "/api/admin/relay-accounting", nil); out.Code != http.StatusForbidden {
			t.Fatal("non-admin accounting access")
		}
	}
	var before []byte
	if err := a.Store.View(func(s *State) error { before, _ = json.Marshal(s); return nil }); err != nil {
		t.Fatal(err)
	}
	response := commerceTestRequest(mux, admin, http.MethodGet, "/api/admin/relay-accounting", nil)
	var body struct {
		Periods        []RelayBillingPeriodV2 `json:"periods"`
		ReviewBytes    int64                  `json:"reviewBytes"`
		DegradedAgents int                    `json:"degradedAgents"`
	}
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &body) != nil || body.ReviewBytes != 70 || body.DegradedAgents != 1 || len(body.Periods) != 2 || body.Periods[0].UserID != "a" {
		t.Fatal("incorrect history/review summary")
	}
	if err := a.Store.View(func(s *State) error {
		after, _ := json.Marshal(s)
		if string(after) != string(before) {
			t.Fatal("read-only review mutated finances")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var generic map[string]any
	_ = json.Unmarshal(response.Body.Bytes(), &generic)
	for _, key := range []string{"token", "tokenHash", "address", "targets", "agents"} {
		if _, ok := generic[key]; ok {
			t.Fatal("accounting review exposed node details")
		}
	}
}
