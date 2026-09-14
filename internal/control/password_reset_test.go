package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const resetTestEmail = "12345678@qq.com"
const resetTestPassword = "new-member-password-987"

type resetMailbox struct {
	mu   sync.Mutex
	mail []EmailMessage
	to   []string
}

func resetMailFixture(t *testing.T, a *App) *resetMailbox {
	t.Helper()
	box := &resetMailbox{}
	sealed, err := a.Seal([]byte("recovery-test-smtp-secret"))
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
	a.mailSender = func(_ context.Context, _ SMTPConfig, secret, recipient string, message EmailMessage) error {
		if secret != "recovery-test-smtp-secret" {
			return errors.New("test secret mismatch")
		}
		box.mu.Lock()
		defer box.mu.Unlock()
		box.mail = append(box.mail, message)
		box.to = append(box.to, recipient)
		return nil
	}
	return box
}

func resetRequest(h http.Handler, email string) *httptest.ResponseRecorder {
	return identityRequest(h, "POST", "/api/auth/password/reset/request", map[string]any{"email": email}, nil, "")
}

func resetConfirm(h http.Handler, email, code, password string) *httptest.ResponseRecorder {
	return identityRequest(h, "POST", "/api/auth/password/reset/confirm", map[string]any{"email": email, "code": code, "newPassword": password}, nil, "")
}

func resetCode(t *testing.T, a *App, box *resetMailbox) string {
	t.Helper()
	a.passwordRecovery.wg.Wait()
	box.mu.Lock()
	defer box.mu.Unlock()
	for _, message := range box.mail {
		if message.Subject == "[MSBOOST] 找回密码验证码" {
			return regexp.MustCompile(`[0-9]{6}`).FindString(message.TextBody)
		}
	}
	t.Fatal("recovery mail missing")
	return ""
}

func putResetChallenge(t *testing.T, a *App, userID, purpose, code string) emailChallenge {
	t.Helper()
	var c emailChallenge
	if err := a.Store.Update(func(s *State) error {
		u := s.Users[userID]
		c = emailChallenge{ID: ID(), Owner: u.ID, Email: u.Email, Purpose: purpose, Ready: true, CreatedAt: time.Now().UnixMilli(), ExpiresAt: time.Now().Add(passwordResetLifetime).UnixMilli(), CredentialsHash: tokenHash(u.PasswordHash)}
		c.Hash = challengeHash(c.ID, code)
		return SaveDoc(s, "email_challenges", challengeID(purpose, u.Email), c)
	}); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestPasswordResetVerifiedAndUnverifiedMembers(t *testing.T) {
	for _, verified := range []bool{false, true} {
		t.Run(map[bool]string{false: "unverified", true: "verified"}[verified], func(t *testing.T) {
			a, h := identityFixture(t, false)
			cookie, _, user := identityRegister(t, h, resetTestEmail)
			otherCookie, _, _ := identityRegister(t, h, "12345679@qq.com")
			uid := user["id"].(string)
			var verifiedAt int64
			if verified {
				verifiedAt = time.Now().UnixMilli()
			}
			_ = a.Store.Update(func(s *State) error {
				u := s.Users[uid]
				u.EmailVerifiedAt = verifiedAt
				u.BalanceCents = 123
				u.TrafficUsed = 456
				return nil
			})
			box := resetMailFixture(t, a)
			out := identityResponse(t, resetRequest(h, " 12345678@QQ.COM "), 200)
			if out["message"] != passwordResetMessage || out["retryAfter"] != float64(60) {
				t.Fatal("nonuniform request response")
			}
			code := resetCode(t, a, box)
			if len(code) != 6 {
				t.Fatal("missing six digit code")
			}
			if got := identityResponse(t, identityRequest(h, "GET", "/api/me", nil, cookie, ""), 200); got["user"] == nil {
				t.Fatal("request invalidated session before proof")
			}
			w := resetConfirm(h, resetTestEmail, code, resetTestPassword)
			identityResponse(t, w, 200)
			if len(w.Result().Cookies()) != 0 {
				t.Fatal("reset automatically logged in")
			}
			a.passwordRecovery.wg.Wait()
			identityResponse(t, resetConfirm(h, resetTestEmail, code, "another-password-987"), 400)
			if got := identityResponse(t, identityRequest(h, "GET", "/api/me", nil, cookie, ""), 200); got["user"] != nil {
				t.Fatal("old session survived")
			}
			if got := identityResponse(t, identityRequest(h, "GET", "/api/me", nil, otherCookie, ""), 200); got["user"] == nil {
				t.Fatal("unrelated session revoked")
			}
			identityResponse(t, identityRequest(h, "POST", "/api/auth/login", map[string]any{"email": resetTestEmail, "password": "test-member-password-123"}, nil, ""), 401)
			identityResponse(t, identityRequest(h, "POST", "/api/auth/login", map[string]any{"email": resetTestEmail, "password": resetTestPassword}, nil, ""), 200)
			_ = a.Store.View(func(s *State) error {
				u := s.Users[uid]
				if u.EmailVerifiedAt != verifiedAt || u.Role != "member" || u.BalanceCents != 123 || u.TrafficUsed != 456 {
					t.Fatal("reset mutated independent membership/verification/billing state")
				}
				if _, ok := LoadDoc[emailChallenge](s, "email_challenges", challengeID(passwordResetPurpose, resetTestEmail)); ok {
					t.Fatal("consumed challenge survived")
				}
				audit, _ := json.Marshal(s.Docs["audit"])
				if strings.Contains(string(audit), code) || strings.Contains(string(audit), resetTestPassword) {
					t.Fatal("secret in audit")
				}
				return nil
			})
			box.mu.Lock()
			defer box.mu.Unlock()
			if len(box.mail) != 2 || box.mail[1].Subject != "[MSBOOST] 密码重置成功通知" {
				t.Fatal("missing successful reset notice")
			}
			if strings.Contains(box.mail[1].TextBody, resetTestPassword) {
				t.Fatal("password in notice")
			}
		})
	}
}

func TestPasswordResetIneligibleAccountsAreIndistinguishable(t *testing.T) {
	a, h := identityFixture(t, true)
	_, _, member := identityRegister(t, h, resetTestEmail)
	_ = a.Store.Update(func(s *State) error { s.Users[member["id"].(string)].Status = "disabled"; return nil })
	box := resetMailFixture(t, a)
	var previous string
	for _, email := range []string{resetTestEmail, "99999999@qq.com", "77777777@qq.com"} {
		w := resetRequest(h, email)
		identityResponse(t, w, 200)
		if previous != "" && previous != w.Body.String() {
			t.Fatal("account enumeration in response")
		}
		previous = w.Body.String()
		identityResponse(t, resetConfirm(h, email, "028461", resetTestPassword), 400)
	}
	a.passwordRecovery.wg.Wait()
	box.mu.Lock()
	defer box.mu.Unlock()
	if len(box.mail) != 0 {
		t.Fatal("ineligible/admin account received recovery mail")
	}
	var adminID string
	_ = a.Store.View(func(s *State) error {
		for _, u := range s.Users {
			if u.Role == "admin" {
				adminID = u.ID
			}
		}
		return nil
	})
	putResetChallenge(t, a, adminID, passwordResetPurpose, "028461")
	identityResponse(t, resetConfirm(h, "99999999@qq.com", "028461", resetTestPassword), 400)
	identityLoginAdmin(t, h)
}

func TestPasswordResetPurposeExpiryReadyAndAttemptLimit(t *testing.T) {
	for _, scenario := range []string{"register", "verify", "expired", "not-ready", "wrong-owner", "wrong-password-version", "locked"} {
		t.Run(scenario, func(t *testing.T) {
			a, h := identityFixture(t, false)
			_, _, u := identityRegister(t, h, resetTestEmail)
			purpose := passwordResetPurpose
			if scenario == "register" || scenario == "verify" {
				purpose = scenario
			}
			c := putResetChallenge(t, a, u["id"].(string), purpose, "028461")
			switch scenario {
			case "expired":
				c.ExpiresAt = time.Now().Add(-time.Second).UnixMilli()
			case "not-ready":
				c.Ready = false
			case "wrong-owner":
				c.Owner = "missing"
			case "wrong-password-version":
				c.CredentialsHash = "outdated"
			case "locked":
				c.Attempts = 5
			}
			_ = a.Store.Update(func(s *State) error { return SaveDoc(s, "email_challenges", challengeID(purpose, resetTestEmail), c) })
			identityResponse(t, resetConfirm(h, resetTestEmail, "028461", resetTestPassword), 400)
		})
	}
	a, h := identityFixture(t, false)
	_, _, u := identityRegister(t, h, resetTestEmail)
	putResetChallenge(t, a, u["id"].(string), passwordResetPurpose, "028461")
	for i := 0; i < 5; i++ {
		identityResponse(t, resetConfirm(h, resetTestEmail, "000000", resetTestPassword), 400)
	}
	_ = a.Store.View(func(s *State) error {
		c, _ := LoadDoc[emailChallenge](s, "email_challenges", challengeID(passwordResetPurpose, resetTestEmail))
		if c.Attempts != 5 {
			t.Fatalf("wrong attempts rolled back: %d", c.Attempts)
		}
		return nil
	})
	identityResponse(t, resetConfirm(h, resetTestEmail, "028461", resetTestPassword), 400)
}

func TestPasswordResetRoleChangeAndConcurrentConsumption(t *testing.T) {
	a, h := identityFixture(t, false)
	_, _, u := identityRegister(t, h, resetTestEmail)
	uid := u["id"].(string)
	putResetChallenge(t, a, uid, passwordResetPurpose, "028461")
	_ = a.Store.Update(func(s *State) error { s.Users[uid].Role = "admin"; return nil })
	identityResponse(t, resetConfirm(h, resetTestEmail, "028461", resetTestPassword), 400)
	_ = a.Store.Update(func(s *State) error { s.Users[uid].Role = "member"; return nil })
	var wg sync.WaitGroup
	results := make(chan int, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); results <- resetConfirm(h, resetTestEmail, "028461", resetTestPassword).Code }()
	}
	wg.Wait()
	close(results)
	count := 0
	for status := range results {
		if status == 200 {
			count++
		} else if status != 400 {
			t.Fatalf("unexpected response %d", status)
		}
	}
	if count != 1 {
		t.Fatalf("single use code consumed %d times", count)
	}
}

func TestPasswordResetSendFailureDoesNotEraseNewerChallenge(t *testing.T) {
	a, h := identityFixture(t, false)
	_, _, u := identityRegister(t, h, resetTestEmail)
	resetMailFixture(t, a)
	var replacement emailChallenge
	a.mailSender = func(_ context.Context, _ SMTPConfig, _, _ string, _ EmailMessage) error {
		replacement = putResetChallenge(t, a, u["id"].(string), passwordResetPurpose, "019283")
		return errors.New("sensitive SMTP diagnostic must not be exposed")
	}
	identityResponse(t, resetRequest(h, resetTestEmail), 200)
	a.passwordRecovery.wg.Wait()
	_ = a.Store.View(func(s *State) error {
		got, _ := LoadDoc[emailChallenge](s, "email_challenges", challengeID(passwordResetPurpose, resetTestEmail))
		if got.ID != replacement.ID {
			t.Fatal("newer challenge erased")
		}
		return nil
	})
}

func TestPasswordResetSMTPFailureAndNoticeFailure(t *testing.T) {
	a, h := identityFixture(t, false)
	_, _, u := identityRegister(t, h, resetTestEmail)
	resetMailFixture(t, a)
	a.mailSender = func(_ context.Context, _ SMTPConfig, _, _ string, _ EmailMessage) error {
		return errors.New("secret SMTP detail")
	}
	identityResponse(t, resetRequest(h, resetTestEmail), 200)
	a.passwordRecovery.wg.Wait()
	_ = a.Store.View(func(s *State) error {
		if _, ok := LoadDoc[emailChallenge](s, "email_challenges", challengeID(passwordResetPurpose, resetTestEmail)); ok {
			t.Fatal("failed delivery challenge survived")
		}
		return nil
	})
	putResetChallenge(t, a, u["id"].(string), passwordResetPurpose, "028461")
	identityResponse(t, resetConfirm(h, resetTestEmail, "028461", resetTestPassword), 200)
	a.passwordRecovery.wg.Wait()
	_ = a.Store.View(func(s *State) error {
		if bcrypt.CompareHashAndPassword([]byte(s.Users[u["id"].(string)].PasswordHash), []byte(resetTestPassword)) != nil {
			t.Fatal("notice failure rolled back password")
		}
		raw, _ := json.Marshal(s.Docs["audit"])
		if strings.Contains(string(raw), "secret SMTP detail") || strings.Contains(string(raw), resetTestEmail) {
			t.Fatal("raw secret/email in recovery audit")
		}
		if !strings.Contains(string(raw), "password.recovery.notice.failed") {
			t.Fatal("notice failure not observable")
		}
		return nil
	})
}

func TestPasswordResetAcceptedMailSurvivesQuitCancellation(t *testing.T) {
	a, h := identityFixture(t, false)
	identityRegister(t, h, resetTestEmail)
	resetMailFixture(t, a)
	a.mailSender = func(_ context.Context, _ SMTPConfig, _, _ string, _ EmailMessage) error {
		// The transport has received DATA acceptance, then its QUIT timed out.
		a.passwordRecovery.cancel()
		return nil
	}
	identityResponse(t, resetRequest(h, resetTestEmail), 200)
	a.passwordRecovery.wg.Wait()
	_ = a.Store.View(func(s *State) error {
		c, ok := LoadDoc[emailChallenge](s, "email_challenges", challengeID(passwordResetPurpose, resetTestEmail))
		if !ok || !c.Ready {
			t.Fatal("accepted mail invalidated by QUIT/shutdown cancellation")
		}
		return nil
	})
}

func TestPasswordResetRechecksAccountAfterDelivery(t *testing.T) {
	for _, change := range []string{"administrator", "password", "disabled"} {
		t.Run(change, func(t *testing.T) {
			a, h := identityFixture(t, false)
			_, _, user := identityRegister(t, h, resetTestEmail)
			resetMailFixture(t, a)
			a.mailSender = func(_ context.Context, _ SMTPConfig, _, _ string, _ EmailMessage) error {
				return a.Store.Update(func(s *State) error {
					u := s.Users[user["id"].(string)]
					switch change {
					case "administrator":
						u.Role = "admin"
					case "password":
						u.PasswordHash = "newer-credential-version"
					case "disabled":
						u.Status = "disabled"
					}
					return nil
				})
			}
			identityResponse(t, resetRequest(h, resetTestEmail), 200)
			a.passwordRecovery.wg.Wait()
			_ = a.Store.View(func(s *State) error {
				if _, ok := LoadDoc[emailChallenge](s, "email_challenges", challengeID(passwordResetPurpose, resetTestEmail)); ok {
					t.Fatal("outdated/ineligible challenge became usable")
				}
				return nil
			})
		})
	}
}

func TestPasswordRecoveryShutdownCancelsJobs(t *testing.T) {
	a, h := identityFixture(t, false)
	identityRegister(t, h, resetTestEmail)
	resetMailFixture(t, a)
	entered := make(chan struct{})
	a.mailSender = func(ctx context.Context, _ SMTPConfig, _, _ string, _ EmailMessage) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}
	identityResponse(t, resetRequest(h, resetTestEmail), 200)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("mail job missing")
	}
	closed := make(chan error, 1)
	go func() { closed <- a.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("close did not cancel/wait for mail")
	}
	if a.queuePasswordMail(func(context.Context) { t.Error("job accepted after close") }) {
		t.Fatal("recovery queue reopened after shutdown")
	}
}

func TestPasswordResetRequestDoesNotWaitForSMTPAndHasBoundedJobs(t *testing.T) {
	a, h := identityFixture(t, false)
	identityRegister(t, h, resetTestEmail)
	resetMailFixture(t, a)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	a.mailSender = func(ctx context.Context, _ SMTPConfig, _, _ string, _ EmailMessage) error {
		close(entered)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- resetRequest(h, resetTestEmail) }()
	select {
	case w := <-done:
		identityResponse(t, w, 200)
	case <-time.After(2 * time.Second):
		t.Fatal("anonymous response waited for SMTP")
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("mail job did not start")
	}
	w := resetRequest(h, resetTestEmail)
	identityResponse(t, w, 200)
	if !strings.Contains(w.Body.String(), passwordResetMessage) {
		t.Fatal("cooldown reveals eligibility")
	}
	for range 7 {
		if !a.queuePasswordMail(func(ctx context.Context) {
			select {
			case <-release:
			case <-ctx.Done():
			}
		}) {
			t.Fatal("premature capacity limit")
		}
	}
	if a.queuePasswordMail(func(context.Context) {}) {
		t.Fatal("unbounded recovery workers")
	}
	once.Do(func() { close(release) })
	a.passwordRecovery.wg.Wait()
}

func TestPasswordResetValidationOriginCaptchaAndLimits(t *testing.T) {
	a, h := identityFixture(t, false)
	identityRegister(t, h, resetTestEmail)
	resetMailFixture(t, a)
	identityResponse(t, resetRequest(h, "someone@example.com"), 400)
	identityResponse(t, resetConfirm(h, resetTestEmail, "028461", strings.Repeat("密", 25)), 400)
	r := httptest.NewRequest("POST", "/api/auth/password/reset/request", strings.NewReader(`{"email":"12345678@qq.com"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "https://evil.example")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	identityResponse(t, w, 403)
	_ = a.Store.Update(func(s *State) error { s.Settings["turnstile"] = true; return nil })
	identityResponse(t, resetRequest(h, resetTestEmail), 400)
	identityResponse(t, resetConfirm(h, resetTestEmail, "028461", resetTestPassword), 400)
	_ = a.Store.Update(func(s *State) error { s.Settings["turnstile"] = false; return nil })
	for range 20 {
		resetRequest(h, "77777777@qq.com")
	}
	identityResponse(t, resetRequest(h, resetTestEmail), 429)
	a.passwordRecovery.wg.Wait()
}
