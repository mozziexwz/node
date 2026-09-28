package control

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestOnlinePaymentWhitelistLifecycle(t *testing.T) {
	a, mux, u := commerceTestApp(t)
	a.RegisterIdentity(mux)
	admin := &User{ID: "admin", Role: "admin", Status: "active"}
	secret := paymentSecrets{MerchantKey: "synthetic-whitelist-secret"}
	raw, _ := json.Marshal(secret)
	sealed, err := a.Seal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Store.Update(func(s *State) error {
		for _, kind := range []string{"alipay", "wxpay"} {
			if err := SaveDoc(s, "payment_channels", kind, PaymentChannel{ID: kind, Name: kind, Type: kind, Version: "v1", Gateway: "https://gateway.invalid", MerchantID: "123", SealedSecrets: sealed, Enabled: true}); err != nil {
				return err
			}
		}
		return SaveDoc(s, "plans", "plan", Plan{ID: "plan", Name: "test", Enabled: true, PriceCents: 100, Days: 1, TrafficBytes: commerceGB, RateMbps: 1, Level: 1})
	}); err != nil {
		t.Fatal(err)
	}
	channels := func(want int) {
		t.Helper()
		w := commerceTestRequest(mux, u, "GET", "/api/payment-channels", nil)
		var body struct {
			Channels []PaymentChannel `json:"channels"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil || len(body.Channels) != want {
			t.Fatalf("channels: %d %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Header().Get("Cache-Control"), "no-store") {
			t.Fatal("personalized channels cached")
		}
	}
	input := func(channel, key string) map[string]any {
		return map[string]any{"planId": "plan", "channelId": channel, "requestId": key, "confirmReplace": true}
	}
	channels(0)
	if w := commerceTestRequest(mux, nil, "GET", "/api/payment-channels", nil); w.Code != 401 {
		t.Fatal("anonymous channels exposed")
	}
	for _, kind := range []string{"alipay", "wxpay"} {
		w := commerceTestRequest(mux, u, "POST", "/api/orders", input(kind, "blocked-"+kind))
		if w.Code != 409 || !strings.Contains(w.Body.String(), "未开通在线支付") {
			t.Fatal(w.Body.String())
		}
	}
	if w := commerceTestRequest(mux, u, "PATCH", "/api/admin/users/"+u.ID, map[string]any{"onlinePaymentAllowed": true}); w.Code != 403 {
		t.Fatal("member granted own permission")
	}
	grant := func(allowed bool) {
		t.Helper()
		w := commerceTestRequest(mux, admin, "PATCH", "/api/admin/users/"+u.ID, map[string]any{"onlinePaymentAllowed": allowed})
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
	}
	grant(true)
	channels(2)
	// Partial identity edits and initialization must preserve an explicit grant.
	if w := commerceTestRequest(mux, admin, "PATCH", "/api/admin/users/"+u.ID, map[string]any{"status": "active"}); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if err := a.initialize(); err != nil {
		t.Fatal(err)
	}
	channels(2)
	if err := a.Store.Update(func(s *State) error { s.Settings["payali"] = false; return nil }); err != nil {
		t.Fatal(err)
	}
	channels(1)
	if w := commerceTestRequest(mux, u, "POST", "/api/orders", input("alipay", "global-off-order")); w.Code != 409 {
		t.Fatal("global switch bypassed")
	}
	if err := a.Store.Update(func(s *State) error { s.Settings["payali"] = true; return nil }); err != nil {
		t.Fatal(err)
	}
	w := commerceTestRequest(mux, u, "POST", "/api/orders", input("alipay", "authorized-order"))
	var order Order
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &order) != nil || order.PaymentURL == "" {
		t.Fatalf("authorized order: %d %s", w.Code, w.Body.String())
	}
	grant(false)
	channels(0)
	for _, path := range []string{"/api/orders", "/api/orders/" + order.ID} {
		w = commerceTestRequest(mux, u, "GET", path, nil)
		if w.Code != 200 || strings.Contains(w.Body.String(), "gateway.invalid") {
			t.Fatal("old order payment link exposed", w.Body.String())
		}
	}
	w = commerceTestRequest(mux, u, "POST", "/api/orders", input("alipay", "authorized-order"))
	if w.Code != 201 || strings.Contains(w.Body.String(), "gateway.invalid") {
		t.Fatal("idempotent replay exposed revoked link", w.Body.String())
	}
	w = commerceTestRequest(mux, u, "POST", "/api/orders", input("balance", "pending-balance"))
	if w.Code != 409 || !strings.Contains(w.Body.String(), "到期") {
		t.Fatal("pending deadline missing")
	}
	// Payment for a legitimately issued order must settle after grant revocation.
	values := url.Values{"pid": {"123"}, "type": {"alipay"}, "out_trade_no": {order.ID}, "trade_no": {"whitelist-trade"}, "trade_status": {"TRADE_SUCCESS"}, "money": {"1.00"}, "sign_type": {"MD5"}}
	signature, err := paymentSign(values, "v1", secret)
	if err != nil {
		t.Fatal(err)
	}
	values.Set("sign", signature)
	for i := 0; i < 2; i++ {
		w = commerceTestRequest(mux, nil, "GET", "/api/payments/epay/alipay/notify?"+values.Encode(), nil)
		if w.Code != 200 || w.Body.String() != "success" {
			t.Fatal("legitimate payment rejected", w.Body.String())
		}
	}
	if err := a.Store.View(func(s *State) error {
		o, _ := LoadDoc[Order](s, "orders", order.ID)
		if o.State != "paid" {
			t.Fatal("revoked user's prior order not settled", o.State, o.ReviewReason)
		}
		count := 0
		for _, event := range ListDocs[map[string]any](s, "audit") {
			if event["action"] == "user.online_payment.update" {
				count++
			}
		}
		if count != 2 {
			t.Fatal("grant/revoke audit missing", count)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestWhitelistDoesNotRestrictMapleOrFreeEntitlements(t *testing.T) {
	for _, price := range []int64{0, 100} {
		t.Run(centsString(price), func(t *testing.T) {
			a, mux, u := commerceTestApp(t)
			if err := a.Store.Update(func(s *State) error {
				s.Users[u.ID].BalanceCents = price
				return SaveDoc(s, "plans", "plan", Plan{ID: "plan", Enabled: true, PriceCents: price, Days: 1, TrafficBytes: commerceGB, RateMbps: 1, Level: 1})
			}); err != nil {
				t.Fatal(err)
			}
			w := commerceTestRequest(mux, u, "POST", "/api/orders", map[string]any{"planId": "plan", "channelId": "balance", "requestId": "maple-without-grant", "confirmReplace": true})
			var order Order
			if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &order) != nil || order.State != "paid" || order.PaymentURL != "" {
				t.Fatal(w.Code, w.Body.String())
			}
		})
	}
}

func TestWhitelistMissingFieldClosedAndResponseCopyOnly(t *testing.T) {
	var legacy User
	if err := json.Unmarshal([]byte(`{"id":"old","status":"active"}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.OnlinePaymentAllowed {
		t.Fatal("legacy user implicitly authorized")
	}
	a, _, u := commerceTestApp(t)
	o := Order{ID: "existing", UserID: u.ID, PaymentURL: "https://gateway.invalid/old", State: "pending", ExpiresAt: time.Now().Add(time.Minute).UnixMilli()}
	if err := a.Store.View(func(s *State) error {
		view := memberOrderView(s, u.ID, o)
		if view.PaymentURL != "" || o.PaymentURL == "" {
			t.Fatal("response sanitization mutated issued order")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
