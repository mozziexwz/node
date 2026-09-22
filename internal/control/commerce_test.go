package control

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mozziexwz/node/internal/relayruntime"
)

func commerceTestApp(t *testing.T) (*App, *http.ServeMux, *User) {
	t.Helper()
	a, err := New(Config{DataDir: t.TempDir(), PublicURL: "https://msboost.example", AdminEmail: "123456789@qq.com", AdminPassword: "Safe-Test-Password-123!"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	u := &User{ID: "buyer", Email: "buyer@example.com", Role: "member", Status: "active", RateMbps: 5}
	if err := a.Store.Update(func(s *State) error { s.Users[u.ID] = u; return nil }); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	a.RegisterCommerce(mux)
	a.RegisterRelay(mux)
	return a, mux, u
}
func commerceTestRequest(mux http.Handler, u *User, method, path string, input any) *httptest.ResponseRecorder {
	var body bytes.Buffer
	if input != nil {
		json.NewEncoder(&body).Encode(input)
	}
	r := httptest.NewRequest(method, path, &body)
	r.Header.Set("Content-Type", "application/json")
	if u != nil {
		r = r.WithContext(context.WithValue(r.Context(), authContextKey{}, &requestIdentity{user: u}))
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}
func TestPurchaseThresholdExact(t *testing.T) {
	now := int64(1000000)
	cases := []struct {
		timeLeft, bytes int64
		allowed         bool
	}{{30 * commerceDay, 10 * commerceGB, false}, {30*commerceDay - 1, 10 * commerceGB, true}, {30 * commerceDay, 10*commerceGB - 1, true}, {31 * commerceDay, 11 * commerceGB, false}, {0, 0, true}}
	for _, tc := range cases {
		u := &User{ExpiresAt: now + tc.timeLeft, TrafficTotal: tc.bytes}
		if got := purchaseAllowed(u, now); got != tc.allowed {
			t.Errorf("time %d bytes %d: %v", tc.timeLeft, tc.bytes, got)
		}
	}
}
func TestCardConcurrentRedeemExactlyOnce(t *testing.T) {
	a, mux, u := commerceTestApp(t)
	code := "MSB-TEST-PRIVATE-CARD"
	sealed, err := a.Seal([]byte(code))
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Store.Update(func(s *State) error {
		return SaveDoc(s, "cards", "card", BalanceCard{ID: "card", CodeHash: commerceHash(code), SealedCode: sealed, AmountCents: 1200, Status: "active"})
	}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			w := commerceTestRequest(mux, u, "POST", "/api/wallet/redeem", map[string]any{"code": code, "requestId": fmt.Sprintf("redeem-%08d", n)})
			if w.Code != 200 && (w.Code != 409 || !strings.Contains(w.Body.String(), "已被使用")) {
				t.Errorf("redeem: %d %s", w.Code, w.Body.String())
			}
		}(i)
	}
	wg.Wait()
	err = a.Store.View(func(s *State) error {
		if s.Users[u.ID].BalanceCents != 1200 {
			t.Errorf("balance = %d", s.Users[u.ID].BalanceCents)
		}
		if len(ListDocs[LedgerEntry](s, "ledger")) != 1 {
			t.Error("duplicate ledger entry")
		}
		c, _ := LoadDoc[BalanceCard](s, "cards", "card")
		if c.UsedBy != u.ID || c.Status != "used" || strings.Contains(c.SealedCode, code) {
			t.Error("invalid redemption state")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
func TestBalancePurchaseIdempotentAndReplace(t *testing.T) {
	a, mux, u := commerceTestApp(t)
	now := time.Now().UnixMilli()
	err := a.Store.Update(func(s *State) error {
		s.Users[u.ID].BalanceCents = 1000
		s.Users[u.ID].ExpiresAt = now + 5*commerceDay
		s.Users[u.ID].TrafficTotal = 10 * commerceGB
		s.Users[u.ID].TrafficUsed = commerceGB
		return SaveDoc(s, "plans", "plan", Plan{ID: "plan", Name: "test", PriceCents: 300, Days: 3, TrafficBytes: 2 * commerceGB, RateMbps: 10, Enabled: true, Level: 2})
	})
	if err != nil {
		t.Fatal(err)
	}
	in := map[string]any{"planId": "plan", "channelId": "balance", "requestId": "same-order-123", "confirmReplace": true}
	for i := 0; i < 3; i++ {
		w := commerceTestRequest(mux, u, "POST", "/api/orders", in)
		if w.Code != 201 {
			t.Fatalf("purchase %d %s", w.Code, w.Body.String())
		}
	}
	err = a.Store.View(func(s *State) error {
		user := s.Users[u.ID]
		if user.BalanceCents != 700 || user.TrafficTotal != 2*commerceGB || user.TrafficUsed != 0 || user.ExpiresAt > now+3*commerceDay+5000 || user.Level != 2 {
			t.Errorf("replacement failed: %+v", user)
		}
		if len(ListDocs[Order](s, "orders")) != 1 || len(ListDocs[LedgerEntry](s, "ledger")) != 1 {
			t.Error("idempotency failed")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
func TestEpayMD5AndRSA(t *testing.T) {
	values := url.Values{"pid": {"123"}, "out_trade_no": {"order"}, "money": {"12.34"}, "trade_status": {"TRADE_SUCCESS"}}
	v1 := paymentSecrets{MerchantKey: "Secret-MD5-Key"}
	sig, err := paymentSign(values, "v1", v1)
	if err != nil {
		t.Fatal(err)
	}
	values.Set("sign", sig)
	values.Set("sign_type", "MD5")
	if err = paymentVerify(values, "v1", v1); err != nil {
		t.Fatal(err)
	}
	values.Set("money", "12.35")
	if paymentVerify(values, "v1", v1) == nil {
		t.Error("tampered MD5 accepted")
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	v2 := paymentSecrets{PrivateKey: base64.StdEncoding.EncodeToString(x509.MarshalPKCS1PrivateKey(key)), PlatformPublicKey: base64.StdEncoding.EncodeToString(pub)}
	values.Set("sign_type", "RSA")
	sig, err = paymentSign(values, "v2", v2)
	if err != nil {
		t.Fatal(err)
	}
	values.Set("sign", sig)
	if err = paymentVerify(values, "v2", v2); err != nil {
		t.Fatal(err)
	}
	values.Set("pid", "attacker")
	if paymentVerify(values, "v2", v2) == nil {
		t.Error("tampered RSA accepted")
	}
	values["pid"] = []string{"123", "456"}
	if _, err = paymentCanonical(values); err == nil {
		t.Error("duplicate signed fields accepted")
	}
}
func TestMoneyParsingNoFloatRounding(t *testing.T) {
	for in, want := range map[string]int64{"0": 0, "1": 100, "1.2": 120, "0.01": 1, "999999.99": 99999999} {
		n, err := paymentCents(in)
		if err != nil || n != want {
			t.Errorf("%s => %d %v", in, n, err)
		}
	}
	for _, in := range []string{"1.001", "-1", "1e2", "NaN", "+1", ".01", "1.", " 1.00", "9999999999"} {
		if _, err := paymentCents(in); err == nil {
			t.Errorf("accepted %s", in)
		}
	}
}
func TestPaymentCallbackBoundAmountMerchantAndLatePayment(t *testing.T) {
	a, mux, u := commerceTestApp(t)
	secret := paymentSecrets{MerchantKey: "Callback-Secret"}
	raw, _ := json.Marshal(secret)
	sealed, err := a.Seal(raw)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	err = a.Store.Update(func(s *State) error {
		if err := SaveDoc(s, "payment_channels", "channel", PaymentChannel{ID: "channel", Version: "v1", Type: "alipay", MerchantID: "123", Enabled: true, SealedSecrets: sealed}); err != nil {
			return err
		}
		return SaveDoc(s, "orders", "order", Order{ID: "order", UserID: u.ID, ChannelID: "channel", Currency: "CNY", State: "pending", AmountCents: 1234, Plan: Plan{Days: 3, TrafficBytes: 5 * commerceGB}, ExpiresAt: now + 60000})
	})
	if err != nil {
		t.Fatal(err)
	}
	values := url.Values{"pid": {"123"}, "out_trade_no": {"order"}, "money": {"12.35"}, "type": {"alipay"}, "trade_no": {"trade"}, "trade_status": {"TRADE_SUCCESS"}, "sign_type": {"MD5"}}
	send := func() int {
		sig, _ := paymentSign(values, "v1", secret)
		values.Set("sign", sig)
		return commerceTestRequest(mux, nil, "GET", "/api/payments/epay/channel/notify?"+values.Encode(), nil).Code
	}
	if send() != 400 {
		t.Fatal("wrong amount accepted")
	}
	values.Set("money", "12.34")
	values.Set("pid", "wrong")
	if send() != 400 {
		t.Fatal("wrong merchant accepted")
	}
	values.Set("pid", "123")
	if send() != 200 || send() != 200 {
		t.Fatal("valid callback or duplicate failed")
	}
	err = a.Store.Update(func(s *State) error {
		if entitlementVersion(s, u.ID) != 1 {
			t.Error("duplicate fulfillment")
		}
		o, _ := LoadDoc[Order](s, "orders", "order")
		o.ID = "late"
		o.State = "expired"
		o.TradeNo = ""
		o.ExpiresAt = now - 1
		o.EntitlementVersion = 1
		return SaveDoc(s, "orders", o.ID, o)
	})
	if err != nil {
		t.Fatal(err)
	}
	values.Set("out_trade_no", "late")
	values.Set("trade_no", "late-trade")
	if send() != 200 {
		t.Fatal("late payment not acknowledged")
	}
	err = a.Store.View(func(s *State) error {
		o, _ := LoadDoc[Order](s, "orders", "late")
		if o.State != "paid_review" || entitlementVersion(s, u.ID) != 1 {
			t.Error("late payment replaced entitlement")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
func TestRelayPortValidation(t *testing.T) {
	cases := []struct {
		ranges []PortRange
		valid  bool
	}{{[]PortRange{{1, 10}, {11, 20}}, true}, {[]PortRange{{1, 10}, {10, 20}}, false}, {[]PortRange{{2, 1}}, false}, {[]PortRange{{0, 10}}, false}, {[]PortRange{{1, 65536}}, false}, {nil, false}}
	for _, tc := range cases {
		if (validatePortRanges(tc.ranges) == nil) != tc.valid {
			t.Errorf("range %+v", tc.ranges)
		}
	}
}
func TestRelayTrafficSingleBillingPointAndEpochDedup(t *testing.T) {
	s := newState()
	s.Users["user"] = &User{ID: "user", Status: "active", TrafficTotal: 100000}
	rule := UserRule{ID: "rule", UserID: "user", RouteID: "route", Version: 1, Segments: []RelaySegment{{AgentID: "entry", Runtime: relayruntime.Rule{Version: 1, Billing: true}}, {AgentID: "exit", Runtime: relayruntime.Rule{Version: 1}}}}
	if err := SaveDoc(s, "user_rules", "user:route", rule); err != nil {
		t.Fatal(err)
	}
	report := relayruntime.Traffic{ID: "rule", Version: 1, Epoch: "unique-process-epoch", Sequence: 1, InputBytes: 100, OutputBytes: 200}
	for _, id := range []string{"entry", "entry", "exit"} {
		if err := applyRelayTraffic(s, RelayAgent{ID: id}, report, time.Now().UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	if s.Users["user"].TrafficUsed != 300 {
		t.Fatal("duplicate/hop double accounting")
	}
	report.Sequence = 2
	report.InputBytes = 150
	if err := applyRelayTraffic(s, RelayAgent{ID: "entry"}, report, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if s.Users["user"].TrafficUsed != 350 {
		t.Fatal("delta accounting failed")
	}
	report.Sequence = 1
	report.Epoch = "second-unique-process"
	report.InputBytes = 20
	report.OutputBytes = 30
	if err := applyRelayTraffic(s, RelayAgent{ID: "entry"}, report, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if s.Users["user"].TrafficUsed != 400 {
		t.Fatal("restart lost accounting")
	}
}

const relayTestConfig = `{"profiles":[{"user":{"name":"same-user","password":"secret-password"},"servers":[{"ipAddress":"1.1.1.1","portBindings":[{"port":12345,"protocol":"TCP"}]}]}],"socks5Port":10086}`

func TestRelayTargetAuthIdentityAndExpiryDownload(t *testing.T) {
	a, mux, u := commerceTestApp(t)
	now := time.Now().UnixMilli()
	err := a.Store.Update(func(s *State) error {
		s.Users[u.ID].ExpiresAt = now + commerceDay
		s.Users[u.ID].TrafficTotal = commerceGB
		agent := RelayAgent{ID: "agent", Address: "8.8.8.8", Enabled: true, Capability: "relay", PortRanges: []PortRange{{20000, 20010}}, LastSeen: now}
		if err := SaveDoc(s, "relay_agents", agent.ID, agent); err != nil {
			return err
		}
		for _, id := range []string{"one", "two"} {
			route := Route{ID: id, Name: id, EntryAgentID: "agent", EntryAddress: "8.8.8.8", Exit: RouteStage{AgentIDs: []string{"agent"}, Protocol: "tcp", Strategy: "round"}, Enabled: true, RateMbps: 5}
			if err := SaveDoc(s, "routes", id, route); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	w := commerceTestRequest(mux, u, "POST", "/api/user/routes/one/rules", map[string]any{"config": json.RawMessage(relayTestConfig), "requestId": "rule-create-123"})
	if w.Code != 202 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	changed := strings.ReplaceAll(relayTestConfig, "secret-password", "different-password")
	w = commerceTestRequest(mux, u, "POST", "/api/user/routes/two/rules", map[string]any{"config": json.RawMessage(changed), "requestId": "rule-create-456"})
	if w.Code != 409 {
		t.Fatal("different auth accepted as same target")
	}
	w = commerceTestRequest(mux, u, "GET", "/api/user/routes/one/config", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "secret-password") {
		t.Fatal("valid config cannot be downloaded")
	}
	other := &User{ID: "other", Status: "active"}
	if w = commerceTestRequest(mux, other, "GET", "/api/user/routes/one/config", nil); w.Code == 200 {
		t.Fatal("cross-user configuration disclosure")
	}
	err = a.Store.Update(func(s *State) error { s.Users[u.ID].ExpiresAt = now - 1; return relayCleanup(s, now) })
	if err != nil {
		t.Fatal(err)
	}
	if w = commerceTestRequest(mux, u, "GET", "/api/user/routes/one/config", nil); w.Code == 200 {
		t.Fatal("expired download still works")
	}
	err = a.Store.View(func(s *State) error {
		rule, _ := LoadDoc[UserRule](s, "user_rules", u.ID+":one")
		if rule.SealedConfig != "" || rule.State != "revoking" {
			t.Error("expiry did not delete secret and revoke runtime")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
