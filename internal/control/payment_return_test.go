package control

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func paymentTestRSA(t *testing.T) paymentSecrets {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return paymentSecrets{PrivateKey: base64.StdEncoding.EncodeToString(x509.MarshalPKCS1PrivateKey(key)), PlatformPublicKey: base64.StdEncoding.EncodeToString(pub)}
}

func TestPaymentRedirectURLsAndReadonlyChannelMetadata(t *testing.T) {
	a, mux, u := commerceTestApp(t)
	for _, version := range []string{"v1", "v2"} {
		t.Run(version, func(t *testing.T) {
			secret := paymentSecrets{MerchantKey: "synthetic-md5-secret"}
			if version == "v2" {
				secret = paymentTestRSA(t)
			}
			raw, _ := json.Marshal(secret)
			sealed, err := a.Seal(raw)
			if err != nil {
				t.Fatal(err)
			}
			ch := PaymentChannel{ID: version, Name: "synthetic", Type: "alipay", Version: version, Gateway: "https://gateway.invalid/", MerchantID: "123", SealedSecrets: sealed, Enabled: true, NotifyURL: "https://untrusted.invalid", ReturnURLTemplate: "https://untrusted.invalid"}
			if err := a.Store.Update(func(s *State) error { s.Settings["payali"] = true; return SaveDoc(s, "payment_channels", ch.ID, ch) }); err != nil {
				t.Fatal(err)
			}
			link, err := a.paymentRedirect(ch, Order{ID: "synthetic-order_123", AmountCents: 1234, Plan: Plan{Name: "合成套餐"}})
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := url.Parse(link)
			if err != nil {
				t.Fatal(err)
			}
			wantPath := "/submit.php"
			if version == "v2" {
				wantPath = "/api/pay/submit"
			}
			if parsed.Path != wantPath || parsed.Host != "gateway.invalid" {
				t.Fatal("wrong checkout endpoint")
			}
			v := parsed.Query()
			if v.Get("notify_url") != "https://msboost.example/api/payments/epay/"+version+"/notify" || v.Get("return_url") != "https://msboost.example/?order=synthetic-order_123" || v.Get("money") != "12.34" {
				t.Fatal("incorrect generated parameters")
			}
			if err := paymentVerify(v, version, secret); err != nil {
				t.Fatal(err)
			}
			if version == "v2" && v.Get("timestamp") == "" {
				t.Fatal("timestamp missing")
			}
		})
	}
	admin := &User{ID: "admin", Role: "admin", Status: "active"}
	response := commerceTestRequest(mux, admin, "GET", "/api/admin/payment-channels", nil)
	if response.Code != 200 {
		t.Fatal(response.Code)
	}
	var metadata struct {
		Channels []PaymentChannel `json:"channels"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &metadata); err != nil {
		t.Fatal(err)
	}
	if len(metadata.Channels) != 2 {
		t.Fatal("missing channels")
	}
	for _, ch := range metadata.Channels {
		if ch.NotifyURL != "https://msboost.example/api/payments/epay/"+ch.ID+"/notify" || ch.ReturnURLTemplate != "https://msboost.example/?order={orderId}" || ch.SealedSecrets != "" || ch.MerchantKey != "" || ch.PrivateKey != "" {
			t.Fatal("incorrect readonly metadata or secret disclosure")
		}
	}
	public := commerceTestRequest(mux, u, "GET", "/api/payment-channels", nil)
	if strings.Contains(public.Body.String(), "notifyUrl") || strings.Contains(public.Body.String(), "returnUrlTemplate") {
		t.Fatal("private metadata leaked")
	}
	if err := json.Unmarshal(public.Body.Bytes(), &metadata); err != nil {
		t.Fatal(err)
	}
	for _, ch := range metadata.Channels {
		if ch.MerchantID != "" || ch.Gateway != "" {
			t.Fatal("merchant identity exposed")
		}
	}
}

func TestPaymentV2HTTPCallbackAndOrderReturnOwnership(t *testing.T) {
	a, mux, user := commerceTestApp(t)
	merchant, platform := paymentTestRSA(t), paymentTestRSA(t)
	secret := paymentSecrets{PrivateKey: merchant.PrivateKey, PlatformPublicKey: platform.PlatformPublicKey}
	raw, _ := json.Marshal(secret)
	sealed, err := a.Seal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Store.Update(func(s *State) error {
		if err := SaveDoc(s, "payment_channels", "rsa", PaymentChannel{ID: "rsa", Version: "v2", Type: "alipay", MerchantID: "123", SealedSecrets: sealed}); err != nil {
			return err
		}
		return SaveDoc(s, "orders", "owned", Order{ID: "owned", UserID: user.ID, ChannelID: "rsa", AmountCents: 1234, Currency: "CNY", State: "pending", Plan: Plan{Days: 1, TrafficBytes: commerceGB}, ExpiresAt: time.Now().Add(time.Minute).UnixMilli()})
	}); err != nil {
		t.Fatal(err)
	}
	// A browser return parameter cannot fulfill an order; reads require its owner.
	for _, who := range []*User{nil, {ID: "other", Role: "member", Status: "active"}, {ID: "other-admin", Role: "admin", Status: "active"}} {
		response := commerceTestRequest(mux, who, "GET", "/api/orders/owned?state=paid&trade_status=TRADE_SUCCESS", nil)
		if response.Code == 200 {
			t.Fatal("foreign order exposed")
		}
	}
	response := commerceTestRequest(mux, user, "GET", "/api/orders/owned?state=paid&trade_status=TRADE_SUCCESS", nil)
	var order Order
	if err := json.Unmarshal(response.Body.Bytes(), &order); err != nil {
		t.Fatal(err)
	}
	if order.State != "pending" {
		t.Fatal("browser query marked paid")
	}
	values := url.Values{"pid": {"123"}, "out_trade_no": {"owned"}, "money": {"12.34"}, "type": {"alipay"}, "trade_no": {"synthetic-platform-trade"}, "trade_status": {"TRADE_SUCCESS"}, "sign_type": {"RSA"}}
	send := func(signing paymentSecrets, method string) *httptest.ResponseRecorder {
		t.Helper()
		sig, err := paymentSign(values, "v2", signing)
		if err != nil {
			t.Fatal(err)
		}
		values.Set("sign", sig)
		path, body := "/api/payments/epay/rsa/notify", ""
		if method == "GET" {
			path += "?" + values.Encode()
		} else {
			body = values.Encode()
		}
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	if send(merchant, "POST").Code != http.StatusBadRequest {
		t.Fatal("merchant key accepted as platform signer")
	}
	values.Set("money", "12.35")
	if send(platform, "POST").Code != http.StatusBadRequest {
		t.Fatal("wrong amount accepted")
	}
	values.Set("money", "12.34")
	for _, method := range []string{"POST", "GET"} {
		response := send(platform, method)
		if response.Code != 200 || response.Body.String() != "success" {
			t.Fatal("verified callback failed")
		}
	}
	if err := a.Store.View(func(s *State) error {
		o, _ := LoadDoc[Order](s, "orders", "owned")
		if o.State != "paid" || entitlementVersion(s, user.ID) != 1 {
			t.Fatal("callback not fulfilled exactly once")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
