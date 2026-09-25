package control

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
)

func TestSupplement32ResetPlanQuotaForAllUsersPreservesHistory(t *testing.T) {
	a, mux, first := commerceTestApp(t)
	second := &User{ID: "second-buyer", Email: "second@example.com", Role: "member", Status: "active", RateMbps: 5}
	admin := &User{ID: "quota-admin", Role: "admin", Status: "active"}
	plan := Plan{ID: "quota-plan", Name: "限兑权益", Days: 1, TrafficBytes: commerceGB, RateMbps: 5, Enabled: true, MaxPurchasesPerUser: 1}
	if err := a.Store.Update(func(s *State) error {
		s.Users[second.ID] = second
		return SaveDoc(s, "plans", plan.ID, plan)
	}); err != nil {
		t.Fatal(err)
	}
	purchase := func(user *User, id string) int {
		return commerceTestRequest(mux, user, http.MethodPost, "/api/orders", map[string]any{"planId": plan.ID, "channelId": "balance", "requestId": id, "confirmReplace": true}).Code
	}
	for _, user := range []*User{first, second} {
		if status := purchase(user, "first-"+user.ID); status != http.StatusCreated {
			t.Fatalf("initial purchase for %s: %d", user.ID, status)
		}
		if status := purchase(user, "blocked-"+user.ID); status != http.StatusConflict {
			t.Fatalf("quota bypass for %s: %d", user.ID, status)
		}
	}
	if response := commerceTestRequest(mux, first, http.MethodPost, "/api/admin/plans/"+plan.ID+"/reset-redemption-count", nil); response.Code != http.StatusForbidden {
		t.Fatalf("member reset quota: %d %s", response.Code, response.Body.String())
	}
	response := commerceTestRequest(mux, admin, http.MethodPost, "/api/admin/plans/"+plan.ID+"/reset-redemption-count", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("reset quota: %d %s", response.Code, response.Body.String())
	}
	var updated Plan
	if err := json.Unmarshal(response.Body.Bytes(), &updated); err != nil || updated.RedemptionCountEpoch != 1 {
		t.Fatalf("reset epoch: %+v %v", updated, err)
	}
	response = commerceTestRequest(mux, first, http.MethodGet, "/api/plans", nil)
	var listing struct {
		Plans           []commercePlanView `json:"plans"`
		PurchasedCounts map[string]int64   `json:"purchasedCounts"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &listing); err != nil || listing.PurchasedCounts[plan.ID] != 0 || len(listing.Plans) != 1 || !listing.Plans[0].Eligible {
		t.Fatalf("quota remained consumed after reset: %s %v", response.Body.String(), err)
	}
	for _, user := range []*User{first, second} {
		if status := purchase(user, "second-"+user.ID); status != http.StatusCreated {
			t.Fatalf("renewal after global reset for %s: %d", user.ID, status)
		}
		if status := purchase(user, "third-"+user.ID); status != http.StatusConflict {
			t.Fatalf("quota bypass after reset for %s: %d", user.ID, status)
		}
		if status := purchase(user, "first-"+user.ID); status != http.StatusCreated {
			t.Fatalf("old idempotent retry changed: %d", status)
		}
	}
	if err := a.Store.View(func(s *State) error {
		paid, original, current := 0, 0, 0
		for _, order := range ListDocs[Order](s, "orders") {
			if order.State != "paid" || order.Plan.ID != plan.ID {
				continue
			}
			paid++
			switch order.RedemptionCountEpoch {
			case 0:
				original++
			case 1:
				current++
			default:
				t.Errorf("unexpected epoch %d", order.RedemptionCountEpoch)
			}
		}
		if paid != 4 || original != 2 || current != 2 {
			t.Errorf("reset altered paid history: paid=%d original=%d current=%d", paid, original, current)
		}
		if len(ListDocs[map[string]any](s, "commerce_audit")) != 1 {
			t.Error("reset audit missing")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSupplement32CardTotalUsesAndOneUsePerMemberConcurrent(t *testing.T) {
	a, mux, first := commerceTestApp(t)
	admin := &User{ID: "card-admin", Role: "admin", Status: "active"}
	users := []*User{first}
	for i := 1; i < 8; i++ {
		users = append(users, &User{ID: fmt.Sprintf("card-user-%d", i), Email: fmt.Sprintf("card%d@example.com", i), Role: "member", Status: "active"})
	}
	if err := a.Store.Update(func(s *State) error {
		for _, user := range users[1:] {
			s.Users[user.ID] = user
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	generate := func(maxUses int64) BalanceCard {
		t.Helper()
		response := commerceTestRequest(mux, admin, http.MethodPost, "/api/admin/cards", map[string]any{"count": 1, "amountCents": 300, "maxUses": maxUses})
		if response.Code != http.StatusCreated {
			t.Fatalf("generate card: %d %s", response.Code, response.Body.String())
		}
		var body struct {
			Cards []BalanceCard `json:"cards"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || len(body.Cards) != 1 {
			t.Fatalf("decode generated card: %s %v", response.Body.String(), err)
		}
		return body.Cards[0]
	}
	redeem := func(user *User, card BalanceCard, id string) int {
		return commerceTestRequest(mux, user, http.MethodPost, "/api/wallet/redeem", map[string]any{"code": card.Code, "requestId": id}).Code
	}
	limited := generate(3)
	statuses := make(chan int, len(users))
	var wg sync.WaitGroup
	for i, user := range users {
		wg.Add(1)
		go func(i int, user *User) {
			defer wg.Done()
			statuses <- redeem(user, limited, fmt.Sprintf("limited-%08d", i))
		}(i, user)
	}
	wg.Wait()
	close(statuses)
	successes := 0
	for status := range statuses {
		if status == http.StatusOK {
			successes++
		} else if status != http.StatusConflict {
			t.Fatalf("unexpected concurrent redemption: %d", status)
		}
	}
	if successes != 3 {
		t.Fatalf("card cap not atomic: got %d, want 3", successes)
	}
	unlimited := generate(0)
	firstStatuses := make(chan int, 12)
	for i := 0; i < cap(firstStatuses); i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			firstStatuses <- redeem(first, unlimited, fmt.Sprintf("unlimited-race-%08d", i))
		}(i)
	}
	wg.Wait()
	close(firstStatuses)
	firstSuccesses := 0
	for status := range firstStatuses {
		if status == http.StatusOK {
			firstSuccesses++
		} else if status != http.StatusConflict {
			t.Fatalf("unexpected same-member race status: %d", status)
		}
	}
	if firstSuccesses != 1 {
		t.Fatalf("same member credited shared code %d times", firstSuccesses)
	}
	for i, user := range users[1:] {
		if status := redeem(user, unlimited, fmt.Sprintf("unlimited-%08d", i)); status != http.StatusOK {
			t.Fatalf("unlimited card rejected distinct user %s: %d", user.ID, status)
		}
		if status := redeem(user, unlimited, fmt.Sprintf("unlimited-again-%08d", i)); status != http.StatusConflict {
			t.Fatalf("same member used unlimited card twice: %d", status)
		}
	}
	response := commerceTestRequest(mux, admin, http.MethodGet, "/api/admin/cards", nil)
	var listing struct {
		Cards []BalanceCard `json:"cards"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	cardByID := map[string]BalanceCard{}
	for _, card := range listing.Cards {
		cardByID[card.ID] = card
	}
	if card := cardByID[limited.ID]; card.UsedCount != 3 || card.MaxUses != 3 || card.Status != "used" || len(card.Uses) != 3 {
		t.Fatalf("limited card usage view wrong: %+v", card)
	}
	if card := cardByID[unlimited.ID]; card.UsedCount != int64(len(users)) || card.MaxUses != 0 || card.Status != "active" || len(card.Uses) != len(users) {
		t.Fatalf("unlimited card usage view wrong: %+v", card)
	}
	for _, use := range cardByID[unlimited.ID].Uses {
		if use.UserID == "" || use.UserEmail == "" || use.UsedAt == 0 {
			t.Fatalf("card usage metadata missing: %+v", use)
		}
	}
	if response := commerceTestRequest(mux, first, http.MethodGet, "/api/wallet", nil); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), unlimited.Code) {
		t.Fatalf("member wallet lost own shared-code history: %d %s", response.Code, response.Body.String())
	}
	if err := a.Store.View(func(s *State) error {
		if len(ListDocs[LedgerEntry](s, "ledger")) != 3+len(users) {
			t.Errorf("card credits duplicated or lost: %d", len(ListDocs[LedgerEntry](s, "ledger")))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSupplement32CardLimitValidationAndLegacyDefault(t *testing.T) {
	_, mux, _ := commerceTestApp(t)
	admin := &User{ID: "card-limit-admin", Role: "admin", Status: "active"}
	for _, maxUses := range []int64{-1, 1000001} {
		response := commerceTestRequest(mux, admin, http.MethodPost, "/api/admin/cards", map[string]any{"count": 1, "amountCents": 100, "maxUses": maxUses})
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid maxUses %d accepted: %d %s", maxUses, response.Code, response.Body.String())
		}
	}
	response := commerceTestRequest(mux, admin, http.MethodPost, "/api/admin/cards", map[string]any{"count": 1, "amountCents": 100})
	if response.Code != http.StatusCreated {
		t.Fatalf("legacy caller generation failed: %d %s", response.Code, response.Body.String())
	}
	var body struct {
		Cards []BalanceCard `json:"cards"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || len(body.Cards) != 1 || body.Cards[0].MaxUses != 1 {
		t.Fatalf("omitted maxUses should retain one-use default: %s %v", response.Body.String(), err)
	}
}
