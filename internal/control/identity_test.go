package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func identityFixture(t *testing.T, admin bool) (*App, http.Handler) {
	t.Helper()
	c := Config{DataDir: t.TempDir(), PublicURL: "https://identity.test", MasterKey: strings.Repeat("a1", 32)}
	if admin {
		c.AdminEmail = "99999999@qq.com"
		c.AdminPassword = "test-admin-password-123"
	}
	a, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	mux := http.NewServeMux()
	a.RegisterIdentity(mux)
	return a, a.Authenticate(mux)
}
func identityRequest(h http.Handler, method, path string, input any, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(input)
	r := httptest.NewRequest(method, path, bytes.NewReader(raw))
	r.RemoteAddr = "192.0.2.40:4000"
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "https://identity.test")
	if cookie != nil {
		r.AddCookie(cookie)
	}
	if csrf != "" {
		r.Header.Set("X-CSRF-Token", csrf)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func identityResponse(t *testing.T, w *httptest.ResponseRecorder, status int) map[string]any {
	t.Helper()
	if w.Code != status {
		t.Fatalf("HTTP %d, expected %d: %s", w.Code, status, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}
func identityLoginAdmin(t *testing.T, h http.Handler) (*http.Cookie, string) {
	w := identityRequest(h, "POST", "/api/auth/login", map[string]any{"email": "99999999@qq.com", "password": "test-admin-password-123"}, nil, "")
	out := identityResponse(t, w, 200)
	return w.Result().Cookies()[0], out["csrfToken"].(string)
}
func identityRegister(t *testing.T, h http.Handler, email string) (*http.Cookie, string, map[string]any) {
	w := identityRequest(h, "POST", "/api/auth/register", map[string]any{"email": email, "password": "test-member-password-123", "agree": true}, nil, "")
	out := identityResponse(t, w, 201)
	return w.Result().Cookies()[0], out["csrfToken"].(string), out["user"].(map[string]any)
}

func TestIdentityRealCredentialsSessionAndCSRF(t *testing.T) {
	a, h := identityFixture(t, false)
	cookie, csrf, user := identityRegister(t, h, "12345678@qq.com")
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("insecure cookie: %#v", cookie)
	}
	if _, ok := user["passwordHash"]; ok {
		t.Fatal("password hash leaked")
	}
	if user["emailVerifiedAt"].(float64) != 0 {
		t.Fatal("registration incorrectly verifies email")
	}
	if err := a.Store.View(func(s *State) error {
		u := s.Users[user["id"].(string)]
		if u.PasswordHash == "" || u.PasswordHash == "test-member-password-123" {
			t.Fatal("password not hashed")
		}
		if s.Sessions[cookie.Value] != nil {
			t.Fatal("raw session token persisted")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	identityResponse(t, identityRequest(h, "POST", "/api/auth/logout", map[string]any{}, cookie, ""), 403)
	r := httptest.NewRequest("POST", "/api/auth/logout", strings.NewReader("{}"))
	r.Header.Set("Origin", "https://evil.test")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-CSRF-Token", csrf)
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	identityResponse(t, w, 403)
	identityResponse(t, identityRequest(h, "POST", "/api/auth/logout", map[string]any{}, cookie, csrf), 200)
	if got := identityResponse(t, identityRequest(h, "GET", "/api/me", nil, cookie, ""), 200); got["user"] != nil {
		t.Fatal("revoked session still authenticates")
	}
	identityResponse(t, identityRequest(h, "POST", "/api/auth/login", map[string]any{"email": "12345678@qq.com", "password": "incorrect-password"}, nil, ""), 401)
	identityResponse(t, identityRequest(h, "POST", "/api/auth/login", map[string]any{"email": "12345678@qq.com", "password": "test-member-password-123"}, nil, ""), 200)
}

func identityMail(t *testing.T, a *App) *string {
	t.Helper()
	code := new(string)
	a.mailSender = func(_ context.Context, _ SMTPConfig, secret, recipient, body string) error {
		if secret != "mail-secret" {
			return errors.New("incorrect decrypted SMTP secret")
		}
		match := regexp.MustCompile(`[0-9]{6}`).FindString(body)
		if match != "" {
			*code = match
		}
		return nil
	}
	sealed, err := a.Seal([]byte("mail-secret"))
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Store.Update(func(s *State) error {
		s.Settings["smtp"] = true
		s.Settings["smtpConfig"] = SMTPConfig{Host: "smtp.example.com", Port: 465, Sender: "12345678@qq.com", Name: "MSBOOST", Encryption: "tls", Secret: sealed, TestedAt: time.Now().UnixMilli()}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return code
}

func TestIdentityEmailPoliciesAreIndependent(t *testing.T) {
	for _, registration := range []bool{false, true} {
		for _, tool := range []bool{false, true} {
			t.Run(fmt.Sprintf("register=%v/tools=%v", registration, tool), func(t *testing.T) {
				a, h := identityFixture(t, false)
				code := identityMail(t, a)
				if err := a.Store.Update(func(s *State) error {
					s.Settings["registrationEmailVerificationRequired"] = registration
					s.Settings["freeToolsRequireVerifiedEmail"] = tool
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if registration {
					identityResponse(t, identityRequest(h, "POST", "/api/auth/register", map[string]any{"email": "12345678@qq.com", "password": "test-member-password-123", "agree": true}, nil, ""), 400)
					identityResponse(t, identityRequest(h, "POST", "/api/auth/email/send", map[string]any{"email": "12345678@qq.com", "purpose": "register"}, nil, ""), 200)
					if len(*code) != 6 {
						t.Fatal("actual mail body lacks random six digit code")
					}
				}
				w := identityRequest(h, "POST", "/api/auth/register", map[string]any{"email": "12345678@qq.com", "password": "test-member-password-123", "agree": true, "code": *code}, nil, "")
				out := identityResponse(t, w, 201)
				verified := out["user"].(map[string]any)["emailVerifiedAt"].(float64) > 0
				if verified != registration {
					t.Fatalf("registration policy verification=%v", verified)
				}
				_ = a.Store.Update(func(s *State) error {
					s.Settings["registrationEmailVerificationRequired"] = false
					s.Settings["freeToolsRequireVerifiedEmail"] = false
					return nil
				})
				me := identityResponse(t, identityRequest(h, "GET", "/api/me", nil, w.Result().Cookies()[0], ""), 200)
				if (me["user"].(map[string]any)["emailVerifiedAt"].(float64) > 0) != registration {
					t.Fatal("disabling policy changed historical verification state")
				}
			})
		}
	}
}

func TestIdentityWrongCodeLimitPersistsAndConsumedCodeCannotReplay(t *testing.T) {
	a, h := identityFixture(t, false)
	cookie, csrf, _ := identityRegister(t, h, "12345678@qq.com")
	code := identityMail(t, a)
	identityResponse(t, identityRequest(h, "POST", "/api/auth/email/send", map[string]any{"purpose": "verify"}, cookie, csrf), 200)
	if strings.Contains(identityRequest(h, "GET", "/api/me", nil, cookie, "").Body.String(), *code) {
		t.Fatal("verification code leaked")
	}
	for i := 0; i < 5; i++ {
		identityResponse(t, identityRequest(h, "POST", "/api/auth/email/verify", map[string]any{"code": "not-a-code"}, cookie, csrf), 400)
	}
	identityResponse(t, identityRequest(h, "POST", "/api/auth/email/verify", map[string]any{"code": *code}, cookie, csrf), 400)
	identityResponse(t, identityRequest(h, "POST", "/api/auth/email/send", map[string]any{"purpose": "verify"}, cookie, csrf), 400)
	if err := a.Store.Update(func(s *State) error {
		id := challengeID("verify", "12345678@qq.com")
		c, _ := LoadDoc[emailChallenge](s, "email_challenges", id)
		if c.Attempts != 5 {
			t.Fatalf("wrong attempts not durable: %d", c.Attempts)
		}
		c.CreatedAt -= 61000
		return SaveDoc(s, "email_challenges", id, c)
	}); err != nil {
		t.Fatal(err)
	}
	identityResponse(t, identityRequest(h, "POST", "/api/auth/email/send", map[string]any{"purpose": "verify"}, cookie, csrf), 200)
	identityResponse(t, identityRequest(h, "POST", "/api/auth/email/verify", map[string]any{"code": *code}, cookie, csrf), 200)
	identityResponse(t, identityRequest(h, "POST", "/api/auth/email/verify", map[string]any{"code": *code}, cookie, csrf), 400)
}

func TestIdentityConcurrentInvitationCannotOverspend(t *testing.T) {
	a, h := identityFixture(t, false)
	if err := a.Store.Update(func(s *State) error {
		s.Settings["invite"] = true
		return SaveDoc(s, "invitations", "one", Invitation{ID: "one", Code: "single-use", MaxUses: 1, Enabled: true, Uses: []InvitationUse{}})
	}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var success atomic.Int32
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			w := identityRequest(h, "POST", "/api/auth/register", map[string]any{"email": fmt.Sprintf("1234567%d@qq.com", n), "password": "test-member-password-123", "agree": true, "inviteCode": "single-use"}, nil, "")
			if w.Code == 201 {
				success.Add(1)
			} else if w.Code != 400 {
				t.Errorf("unexpected status: %d %s", w.Code, w.Body.String())
			}
		}(i)
	}
	wg.Wait()
	if success.Load() != 1 {
		t.Fatalf("successful registrations=%d", success.Load())
	}
	if err := a.Store.View(func(s *State) error {
		v, _ := LoadDoc[Invitation](s, "invitations", "one")
		if len(v.Uses) != 1 || len(s.Users) != 1 {
			t.Fatalf("invitation or users not atomic: %d/%d", len(v.Uses), len(s.Users))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestIdentitySMTPSettingsTestAndSecretRedaction(t *testing.T) {
	a, h := identityFixture(t, true)
	cookie, csrf := identityLoginAdmin(t, h)
	identityResponse(t, identityRequest(h, "PUT", "/api/admin/settings", map[string]any{"registrationEmailVerificationRequired": true}, cookie, csrf), 400)
	config := map[string]any{"host": "smtp.example.com", "port": 465, "sender": "12345678@qq.com", "name": "MSBOOST", "encryption": "tls", "secret": "mail-secret"}
	w := identityRequest(h, "PUT", "/api/admin/settings", map[string]any{"smtp": true, "smtpConfig": config}, cookie, csrf)
	identityResponse(t, w, 200)
	if strings.Contains(w.Body.String(), "mail-secret") {
		t.Fatal("SMTP secret leaked")
	}
	var delivered atomic.Int32
	a.mailSender = func(_ context.Context, c SMTPConfig, secret, recipient, body string) error {
		if secret != "mail-secret" || recipient != "99999999@qq.com" {
			return errors.New("incorrect mail credentials")
		}
		delivered.Add(1)
		return nil
	}
	identityResponse(t, identityRequest(h, "POST", "/api/admin/smtp/test", map[string]any{}, cookie, csrf), 200)
	if delivered.Load() != 1 {
		t.Fatal("SMTP marked tested without delivery")
	}
	identityResponse(t, identityRequest(h, "PUT", "/api/admin/settings", map[string]any{"registrationEmailVerificationRequired": true, "freeToolsRequireVerifiedEmail": true}, cookie, csrf), 200)
	identityResponse(t, identityRequest(h, "PUT", "/api/admin/settings", map[string]any{"smtp": false}, cookie, csrf), 400)
	config["host"] = "replacement.example.com"
	identityResponse(t, identityRequest(h, "PUT", "/api/admin/settings", map[string]any{"smtpConfig": config}, cookie, csrf), 400)
	public := identityResponse(t, identityRequest(h, "GET", "/api/settings", nil, nil, ""), 200)
	if _, ok := public["smtpConfig"]; ok {
		t.Fatal("SMTP config exposed publicly")
	}
	if err := a.Store.View(func(s *State) error {
		c, _ := getSMTP(s)
		if c.Secret == "mail-secret" || c.Secret == "" {
			t.Fatal("mail password not encrypted")
		}
		if c.Host != "smtp.example.com" {
			t.Fatal("rejected settings were not rolled back")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestIdentityAdminMutationRevokesSessionsAndAdjustsLedger(t *testing.T) {
	a, h := identityFixture(t, true)
	admin, csrf := identityLoginAdmin(t, h)
	cookie, _, user := identityRegister(t, h, "12345678@qq.com")
	id := user["id"].(string)
	identityResponse(t, identityRequest(h, "PATCH", "/api/admin/users/"+id, map[string]any{"balanceCents": 500}, admin, csrf), 400)
	identityResponse(t, identityRequest(h, "PATCH", "/api/admin/users/"+id, map[string]any{"balanceCents": 500, "reason": "test credit", "status": "suspended"}, admin, csrf), 200)
	if me := identityResponse(t, identityRequest(h, "GET", "/api/me", nil, cookie, ""), 200); me["user"] != nil {
		t.Fatal("suspended account retains active session")
	}
	if err := a.Store.View(func(s *State) error {
		u := s.Users[id]
		if u.BalanceCents != 500 {
			t.Fatal("balance failed")
		}
		entries := ListDocs[LedgerEntry](s, "ledger")
		if len(entries) != 1 || entries[0].AmountCents != 500 {
			t.Fatal("balance update missing ledger")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	adminMe := identityResponse(t, identityRequest(h, "GET", "/api/me", nil, admin, ""), 200)
	adminID := adminMe["user"].(map[string]any)["id"].(string)
	identityResponse(t, identityRequest(h, "DELETE", "/api/admin/users/"+adminID, nil, admin, csrf), 400)
}

func TestIdentityRateLimitAndTrustedProxyBoundary(t *testing.T) {
	a, h := identityFixture(t, false)
	for i := 0; i < 10; i++ {
		if !a.allow("same", 10, time.Hour) {
			t.Fatal("early rate limit")
		}
	}
	if a.allow("same", 10, time.Hour) {
		t.Fatal("rate limit not enforced")
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "203.0.113.10:1234"
	r.Header.Set("X-Forwarded-For", "1.2.3.4")
	if a.clientIP(r) != "203.0.113.10" {
		t.Fatal("untrusted peer forged source IP")
	}
	trusted, err := New(Config{DataDir: t.TempDir(), TrustedProxyCIDRs: []string{"10.88.0.2/32"}})
	if err != nil {
		t.Fatal(err)
	}
	defer trusted.Close()
	r.RemoteAddr = "10.88.0.2:3333"
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 203.0.113.80")
	if trusted.clientIP(r) != "203.0.113.80" {
		t.Fatal("failed to stop at nearest untrusted proxy hop")
	}
	for i := 0; i < 10; i++ {
		a.allow("login-email:12345678@qq.com", 10, 15*time.Minute)
	}
	identityResponse(t, identityRequest(h, "POST", "/api/auth/login", map[string]any{"email": "12345678@qq.com", "password": "wrong"}, nil, ""), 429)
}

func TestStoreMultipleConnectionsAtomicAndRollback(t *testing.T) {
	dir := t.TempDir()
	config := Config{DataDir: dir, DatabaseURL: filepath.Join(dir, "shared.db")}
	left, err := openStore(config)
	if err != nil {
		t.Fatal(err)
	}
	defer left.Close()
	right, err := openStore(config)
	if err != nil {
		t.Fatal(err)
	}
	defer right.Close()
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			s := left
			if n%2 == 1 {
				s = right
			}
			if err := s.Update(func(state *State) error {
				v, _ := LoadDoc[int](state, "counter", "one")
				return SaveDoc(state, "counter", "one", v+1)
			}); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	err = left.Update(func(s *State) error { _ = SaveDoc(s, "counter", "one", 999); return errors.New("deliberate failure") })
	if err == nil {
		t.Fatal("failed transaction committed")
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("panic not propagated")
			}
		}()
		_ = left.Update(func(s *State) error { _ = SaveDoc(s, "counter", "one", 888); panic("callback panic") })
	}()
	if err = right.View(func(s *State) error {
		v, _ := LoadDoc[int](s, "counter", "one")
		if v != 40 {
			t.Fatalf("lost update or rollback failure: %d", v)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestIdentityPersistedSessionAndEncryptionSurviveRestart(t *testing.T) {
	config := Config{DataDir: t.TempDir(), PublicURL: "https://identity.test"}
	first, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	first.RegisterIdentity(mux)
	cookie, _, user := identityRegister(t, first.Authenticate(mux), "12345678@qq.com")
	sealed, err := first.Seal([]byte("persistent-but-secret"))
	if err != nil {
		t.Fatal(err)
	}
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	plain, err := second.Open(sealed)
	if err != nil || string(plain) != "persistent-but-secret" {
		t.Fatal("master key was not preserved across restart")
	}
	mux = http.NewServeMux()
	second.RegisterIdentity(mux)
	out := identityResponse(t, identityRequest(second.Authenticate(mux), "GET", "/api/me", nil, cookie, ""), 200)
	if out["user"].(map[string]any)["id"] != user["id"] {
		t.Fatal("SQL session or identity not persisted")
	}
	if _, err = second.Open(sealed[:len(sealed)-3] + "XXX"); err == nil {
		t.Fatal("tampered credential decrypted")
	}
}

func TestIdentityFailedDeliveryNeverCreatesUsableCode(t *testing.T) {
	a, h := identityFixture(t, false)
	cookie, csrf, _ := identityRegister(t, h, "12345678@qq.com")
	identityMail(t, a)
	a.mailSender = func(context.Context, SMTPConfig, string, string, string) error {
		return errors.New("535 authentication rejected")
	}
	identityResponse(t, identityRequest(h, "POST", "/api/auth/email/send", map[string]any{"purpose": "verify"}, cookie, csrf), 502)
	if err := a.Store.View(func(s *State) error {
		if _, ok := LoadDoc[emailChallenge](s, "email_challenges", challengeID("verify", "12345678@qq.com")); ok {
			t.Fatal("failed SMTP left challenge")
		}
		events := ListDocs[map[string]any](s, "audit")
		found := false
		for _, event := range events {
			if event["action"] == "smtp.delivery.failed" && event["reason"] == "authentication_failure" {
				found = true
			}
		}
		if !found {
			t.Fatal("SMTP failure missing categorized administrator audit")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestIdentityInvitationBatchAllOrNothing(t *testing.T) {
	a, h := identityFixture(t, true)
	cookie, csrf := identityLoginAdmin(t, h)
	out := identityResponse(t, identityRequest(h, "POST", "/api/admin/invitations", map[string]any{"count": 2, "maxUses": 1}, cookie, csrf), 201)
	items := out["invitations"].([]any)
	first := items[0].(map[string]any)["id"].(string)
	second := items[1].(map[string]any)["id"].(string)
	identityResponse(t, identityRequest(h, "POST", "/api/admin/invitations/batch", map[string]any{"ids": []string{first, "does-not-exist"}, "action": "delete"}, cookie, csrf), 400)
	if err := a.Store.View(func(s *State) error {
		v, _ := LoadDoc[Invitation](s, "invitations", first)
		if v.Archived || !v.Enabled {
			t.Fatal("partial batch mutation committed")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	result := identityResponse(t, identityRequest(h, "POST", "/api/admin/invitations/batch", map[string]any{"ids": []string{first, second}, "action": "export"}, cookie, csrf), 200)
	if len(result["invitations"].([]any)) != 2 {
		t.Fatal("batch export incomplete")
	}
}

func TestIdentityPurchaseEmailPolicyIsIndependentAndRequiresMail(t *testing.T) {
	a, h := identityFixture(t, true)
	cookie, csrf := identityLoginAdmin(t, h)
	settings := identityResponse(t, identityRequest(h, "GET", "/api/settings", nil, nil, ""), 200)
	if settings["purchaseRequireVerifiedEmail"] != false {
		t.Fatal("purchase verification default changed")
	}
	identityResponse(t, identityRequest(h, "PUT", "/api/admin/settings", map[string]any{"purchaseRequireVerifiedEmail": true}, cookie, csrf), 400)
	identityMail(t, a)
	identityResponse(t, identityRequest(h, "PUT", "/api/admin/settings", map[string]any{"purchaseRequireVerifiedEmail": true}, cookie, csrf), 200)
	settings = identityResponse(t, identityRequest(h, "GET", "/api/settings", nil, nil, ""), 200)
	if settings["purchaseRequireVerifiedEmail"] != true || settings["registrationEmailVerificationRequired"] != false || settings["freeToolsRequireVerifiedEmail"] != false {
		t.Fatal("purchase policy affected unrelated verification gates")
	}
	identityResponse(t, identityRequest(h, "PUT", "/api/admin/settings", map[string]any{"smtp": false}, cookie, csrf), 400)
	identityResponse(t, identityRequest(h, "PUT", "/api/admin/settings", map[string]any{"purchaseRequireVerifiedEmail": false, "smtp": false}, cookie, csrf), 200)
}
