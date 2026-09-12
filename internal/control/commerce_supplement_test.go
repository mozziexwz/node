package control

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPlanPerUserLimitConcurrentAndEmailGate(t *testing.T) {
	a, mux, user := commerceTestApp(t)
	plan := Plan{ID: "limited", Name: "体验", Days: 1, TrafficBytes: commerceGB, RateMbps: 1, Enabled: true, MaxPurchasesPerUser: 1}
	if err := a.Store.Update(func(s *State) error {
		s.Settings["purchaseRequireVerifiedEmail"] = true
		return SaveDoc(s, "plans", plan.ID, plan)
	}); err != nil {
		t.Fatal(err)
	}
	purchase := func(id string) int {
		return commerceTestRequest(mux, user, "POST", "/api/orders", map[string]any{"planId": plan.ID, "channelId": "balance", "requestId": id, "confirmReplace": true}).Code
	}
	if purchase("unverified-order") != 409 {
		t.Fatal("unverified purchase accepted")
	}
	if err := a.Store.Update(func(s *State) error { s.Users[user.ID].EmailVerifiedAt = time.Now().UnixMilli(); return nil }); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	statuses := make(chan int, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); statuses <- purchase(fmt.Sprintf("purchase-attempt-%02d", i)) }(i)
	}
	wg.Wait()
	close(statuses)
	success := 0
	for status := range statuses {
		if status == 201 {
			success++
		} else if status != 409 {
			t.Fatalf("unexpected status %d", status)
		}
	}
	if success != 1 {
		t.Fatalf("%d concurrent purchases fulfilled", success)
	}
	var paid Order
	if err := a.Store.View(func(s *State) error {
		for _, o := range ListDocs[Order](s, "orders") {
			if o.State == "paid" {
				paid = o
			}
		}
		if len(ListDocs[Order](s, "orders")) != 1 {
			t.Error("rejected purchases persisted")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if purchase(paid.RequestID) != 201 {
		t.Fatal("same request retry consumed limit")
	}
	if purchase("new-purchase-after-limit") != 409 {
		t.Fatal("new purchase bypassed limit")
	}
	if err := a.Store.Update(func(s *State) error {
		p, _ := LoadDoc[Plan](s, "plans", plan.ID)
		p.MaxPurchasesPerUser = 0
		return SaveDoc(s, "plans", plan.ID, p)
	}); err != nil {
		t.Fatal(err)
	}
	if purchase("unlimited-plan-new-request") != 201 {
		t.Fatal("zero must allow unlimited purchases")
	}
}

func TestPaymentLimitAcrossChannelsAndConcurrentNotifications(t *testing.T) {
	a, mux, user := commerceTestApp(t)
	now := time.Now().UnixMilli()
	plan := Plan{ID: "experience", Name: "体验", PriceCents: 100, Days: 1, TrafficBytes: commerceGB, RateMbps: 1, Enabled: true, MaxPurchasesPerUser: 1}
	secret := paymentSecrets{MerchantKey: "test-callback-key"}
	raw, _ := json.Marshal(secret)
	sealed, err := a.Seal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Store.Update(func(s *State) error {
		if err := SaveDoc(s, "plans", plan.ID, plan); err != nil {
			return err
		}
		// Two already-created orders model a quota changed while checkout was open.
		for _, channel := range []string{"alipay", "wxpay"} {
			if err := SaveDoc(s, "payment_channels", channel, PaymentChannel{ID: channel, Version: "v1", Type: channel, MerchantID: "merchant", Enabled: true, SealedSecrets: sealed}); err != nil {
				return err
			}
			if err := SaveDoc(s, "orders", channel, Order{ID: channel, UserID: user.ID, ChannelID: channel, Currency: "CNY", AmountCents: 100, State: "pending", Plan: plan, ExpiresAt: now + 60000}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, channel := range []string{"alipay", "wxpay"} {
		wg.Add(1)
		go func(channel string) {
			defer wg.Done()
			values := url.Values{"pid": {"merchant"}, "out_trade_no": {channel}, "money": {"1.00"}, "type": {channel}, "trade_no": {"trade-" + channel}, "trade_status": {"TRADE_SUCCESS"}, "sign_type": {"MD5"}}
			signature, e := paymentSign(values, "v1", secret)
			if e != nil {
				t.Error(e)
				return
			}
			values.Set("sign", signature)
			for retry := 0; retry < 2; retry++ {
				response := commerceTestRequest(mux, nil, "GET", "/api/payments/epay/"+channel+"/notify?"+values.Encode(), nil)
				if response.Code != 200 {
					t.Errorf("callback %d %s", response.Code, response.Body.String())
				}
			}
		}(channel)
	}
	wg.Wait()
	if err = a.Store.View(func(s *State) error {
		paid, review := 0, 0
		for _, order := range ListDocs[Order](s, "orders") {
			if order.State == "paid" {
				paid++
			}
			if order.State == "paid_review" {
				review++
				if !strings.Contains(order.ReviewReason, "次数上限") {
					t.Error("limit review reason missing")
				}
			}
		}
		if paid != 1 || review != 1 || entitlementVersion(s, user.ID) != 1 {
			t.Errorf("paid=%d review=%d version=%d", paid, review, entitlementVersion(s, user.ID))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, channel := range []string{"balance", "alipay", "wxpay"} {
		response := commerceTestRequest(mux, user, "POST", "/api/orders", map[string]any{"planId": plan.ID, "channelId": channel, "requestId": "after-limit-" + channel, "confirmReplace": true})
		if response.Code != 409 || !strings.Contains(response.Body.String(), "次数上限") {
			t.Fatalf("limit bypass %s: %s", channel, response.Body.String())
		}
	}
	admin := &User{ID: "admin", Role: "admin", Status: "active"}
	response := commerceTestRequest(mux, admin, "GET", "/api/admin/orders", nil)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"userEmail":"`+user.Email+`"`) {
		t.Fatal("admin order email missing")
	}
}

func TestCardNewRequestRejectedRetryAndArchiveHistoryPreserved(t *testing.T) {
	a, mux, user := commerceTestApp(t)
	code := "MSB-SUPPLEMENT-CARD"
	sealed, err := a.Seal([]byte(code))
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Store.Update(func(s *State) error {
		return SaveDoc(s, "cards", "card", BalanceCard{ID: "card", CodeHash: commerceHash(code), SealedCode: sealed, AmountCents: 100, Status: "active"})
	}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"original-request", "original-request"} {
		response := commerceTestRequest(mux, user, "POST", "/api/wallet/redeem", map[string]any{"code": code, "requestId": id})
		if response.Code != 200 {
			t.Fatal(response.Body.String())
		}
	}
	response := commerceTestRequest(mux, user, "POST", "/api/wallet/redeem", map[string]any{"code": code, "requestId": "new-redemption-request"})
	if response.Code != 409 || !strings.Contains(response.Body.String(), "已被使用") {
		t.Fatal("new request did not reject used card")
	}
	admin := &User{ID: "admin", Role: "admin", Status: "active"}
	response = commerceTestRequest(mux, admin, "POST", "/api/admin/cards/batch", map[string]any{"ids": []string{"card"}, "action": "delete"})
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	response = commerceTestRequest(mux, admin, "GET", "/api/admin/cards", nil)
	if strings.Contains(response.Body.String(), code) {
		t.Fatal("archived card visible by default")
	}
	response = commerceTestRequest(mux, admin, "GET", "/api/admin/cards?status=archived", nil)
	if !strings.Contains(response.Body.String(), code) || !strings.Contains(response.Body.String(), user.Email) || !strings.Contains(response.Body.String(), `"usedAt":`) {
		t.Fatal("archive usage metadata missing")
	}
	response = commerceTestRequest(mux, user, "GET", "/api/wallet", nil)
	if !strings.Contains(response.Body.String(), `"cardCode":"`+code+`"`) {
		t.Fatal("own card history missing")
	}
	if err = a.Store.View(func(s *State) error {
		raw, _ := json.Marshal(s)
		if strings.Contains(string(raw), code) {
			t.Error("card plaintext persisted")
		}
		if len(ListDocs[LedgerEntry](s, "ledger")) != 1 || s.Users[user.ID].BalanceCents != 100 {
			t.Error("duplicate card credit")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
