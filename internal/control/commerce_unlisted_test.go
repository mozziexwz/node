package control

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestUnlistedEntitlementOnlyCurrentValidHolderCanRenew(t *testing.T) {
	a, mux, holder := commerceTestApp(t)
	now := time.Now().UnixMilli()
	plan := Plan{ID: "unlisted-plan", Name: "已下架权益", Days: 1, TrafficBytes: commerceGB, RateMbps: 5, Enabled: false, Trial: true, MaxPurchasesPerUser: 1}
	outsider := &User{ID: "unlisted-outsider", Email: "outsider@example.com", Role: "member", Status: "active"}
	expired := &User{ID: "unlisted-expired", Email: "expired@example.com", Role: "member", Status: "active", PlanID: plan.ID, ExpiresAt: now - 1}
	if err := a.Store.Update(func(s *State) error {
		s.Users[holder.ID].PlanID = plan.ID
		s.Users[holder.ID].ExpiresAt = now + 2*commerceDay
		s.Users[holder.ID].TrafficTotal = 20 * commerceGB
		s.Users[holder.ID].TrafficUsed = 15 * commerceGB
		s.Users[outsider.ID] = outsider
		s.Users[expired.ID] = expired
		return SaveDoc(s, "plans", plan.ID, plan)
	}); err != nil {
		t.Fatal(err)
	}
	list := func(user *User) []commercePlanView {
		t.Helper()
		response := commerceTestRequest(mux, user, http.MethodGet, "/api/plans", nil)
		if response.Code != http.StatusOK {
			t.Fatalf("list for %v: %d %s", user, response.Code, response.Body.String())
		}
		var body struct {
			Plans []commercePlanView `json:"plans"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body.Plans
	}
	if plans := list(holder); len(plans) != 1 || plans[0].ID != plan.ID || !plans[0].Eligible || plans[0].Enabled {
		t.Fatalf("active holder lost unlisted renewal: %+v", plans)
	}
	for _, user := range []*User{outsider, expired, nil} {
		if plans := list(user); len(plans) != 0 {
			t.Fatalf("unlisted entitlement disclosed to non-holder %v: %+v", user, plans)
		}
	}
	for _, user := range []*User{outsider, expired} {
		response := commerceTestRequest(mux, user, http.MethodPost, "/api/orders", map[string]any{"planId": plan.ID, "channelId": "balance", "requestId": "blocked-" + user.ID, "confirmReplace": true})
		if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "已下架") {
			t.Fatalf("non-holder ordered unlisted entitlement: %d %s", response.Code, response.Body.String())
		}
	}
	// A payment opened before unlisting must not bypass the new-customer gate
	// when the callback finally commits it.
	if err := a.Store.Update(func(s *State) error {
		snapshot := plan
		snapshot.Enabled = true
		pending := Order{ID: "pending-before-unlisting", UserID: outsider.ID, Plan: snapshot, State: "pending"}
		return fulfillOrder(s, &pending, s.Users[outsider.ID], now)
	}); err == nil || !strings.Contains(err.Error(), "已下架") {
		t.Fatalf("old pending payment bypassed unlisting: %v", err)
	}
	response := commerceTestRequest(mux, holder, http.MethodPost, "/api/orders", map[string]any{"planId": plan.ID, "channelId": "balance", "requestId": "holder-renewal", "confirmReplace": true})
	if response.Code != http.StatusCreated {
		t.Fatalf("active holder cannot renew unlisted entitlement: %d %s", response.Code, response.Body.String())
	}
	response = commerceTestRequest(mux, holder, http.MethodPost, "/api/orders", map[string]any{"planId": plan.ID, "channelId": "balance", "requestId": "holder-second-renewal", "confirmReplace": true})
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "次数上限") {
		t.Fatalf("unlisted trial-shaped plan bypassed configured limit: %d %s", response.Code, response.Body.String())
	}
}
