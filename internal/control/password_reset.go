package control

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const passwordResetPurpose = "password_reset"
const passwordResetMessage = "如果该邮箱对应可找回的会员账号，我们将发送验证码，请留意收件箱。"
const passwordResetDenied = "验证码无效或已过期，请重新获取"
const passwordResetLifetime = 10 * time.Minute
const passwordResetCooldown = time.Minute

// SMTP latency must not reveal whether an anonymous recovery email exists.
// Jobs are bounded, ephemeral and never persist a plaintext code. Shutdown
// cancels/waits for jobs before closing the database. A lost job is retriable
// after the cooldown; this is intentionally not a durable delivery queue.
type passwordRecovery struct {
	mu      sync.Mutex
	closing bool
	wg      sync.WaitGroup
	slots   chan struct{}
	ctx     context.Context
	cancel  context.CancelFunc
}

func newPasswordRecovery() *passwordRecovery {
	ctx, cancel := context.WithCancel(context.Background())
	return &passwordRecovery{slots: make(chan struct{}, 8), ctx: ctx, cancel: cancel}
}

func (p *passwordRecovery) close() {
	p.mu.Lock()
	p.closing = true
	p.cancel()
	p.mu.Unlock()
	p.wg.Wait()
}

func (a *App) queuePasswordMail(fn func(context.Context)) bool {
	p := a.passwordRecovery
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closing {
		return false
	}
	select {
	case p.slots <- struct{}{}:
	default:
		return false
	}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer func() { <-p.slots }()
		ctx, cancel := context.WithTimeout(p.ctx, 30*time.Second)
		defer cancel()
		fn(ctx)
	}()
	return true
}

func recoverableMember(u *User) bool {
	// EmailVerifiedAt is deliberately not a prerequisite. Admin and unknown
	// roles are rejected at issuance AND consumption, including role changes.
	return u != nil && u.Role == "member" && u.Status == "active"
}

func (a *App) passwordResetRequest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !a.allow("password-reset-ip:"+a.clientIP(r), 20, time.Hour) || !a.allow("password-reset-global", 100, time.Minute) {
		Fail(w, 429, "申请过于频繁，请稍后重试")
		return
	}
	var in struct {
		Email          string `json:"email"`
		TurnstileToken string `json:"turnstileToken"`
	}
	if err := Decode(r, &in); err != nil {
		Fail(w, 400, "请求格式无效")
		return
	}
	in.Email = strings.ToLower(strings.TrimSpace(in.Email))
	if !qqEmail(in.Email) {
		Fail(w, 400, "请填写纯数字 QQ 邮箱")
		return
	}
	if err := a.verifyTurnstile(r, in.TurnstileToken); err != nil {
		Fail(w, 400, err.Error())
		return
	}
	var ready bool
	if err := a.Store.View(func(s *State) error { ready = smtpReady(s); return nil }); err != nil || !ready {
		Fail(w, 503, "邮件服务暂不可用，请联系站点管理员")
		return
	}
	// Apply identical account limits to existing, missing and administrator
	// emails. Neither an ineligible account nor a cooldown reveals itself.
	key := tokenHash(in.Email)
	if a.allow("password-reset-email-minute:"+key, 1, passwordResetCooldown) && a.allow("password-reset-email-hour:"+key, 5, time.Hour) {
		if !a.queuePasswordMail(func(ctx context.Context) { a.issuePasswordReset(ctx, in.Email) }) {
			Fail(w, 503, "邮件服务繁忙，请稍后重试")
			return
		}
	}
	WriteJSON(w, 200, map[string]any{"ok": true, "message": passwordResetMessage, "retryAfter": 60, "expiresIn": 600})
}

func (a *App) issuePasswordReset(ctx context.Context, email string) {
	if ctx.Err() != nil {
		return
	}
	n, err := rand.Int(rand.Reader, big.NewInt(1000000))
	if err != nil {
		return
	}
	code, now := fmt.Sprintf("%06d", n.Int64()), time.Now().UnixMilli()
	id := challengeID(passwordResetPurpose, email)
	challenge := emailChallenge{ID: ID(), Purpose: passwordResetPurpose, Email: email, CreatedAt: now, ExpiresAt: now + passwordResetLifetime.Milliseconds()}
	challenge.Hash = challengeHash(challenge.ID, code)
	var config SMTPConfig
	eligible := false
	err = a.Store.Update(func(s *State) error {
		if !smtpReady(s) || ctx.Err() != nil {
			return nil
		}
		for _, u := range s.Users {
			if u.Email == email && recoverableMember(u) {
				challenge.Owner = u.ID
				challenge.CredentialsHash = tokenHash(u.PasswordHash)
				break
			}
		}
		if challenge.Owner == "" {
			return nil
		}
		if previous, ok := LoadDoc[emailChallenge](s, "email_challenges", id); ok && now-previous.CreatedAt < passwordResetCooldown.Milliseconds() {
			return nil
		}
		var err error
		config, err = getSMTP(s)
		if err != nil {
			return err
		}
		eligible = true
		return SaveDoc(s, "email_challenges", id, challenge)
	})
	if err != nil || !eligible {
		return
	}
	err = a.sendAccountEmail(ctx, config, email, passwordResetPurpose, EmailData{Code: code, TTLMinutes: int(passwordResetLifetime / time.Minute), ExpiresAtLabel: emailTimeLabel(challenge.ExpiresAt), Year: time.UnixMilli(challenge.CreatedAt).In(emailDisplayZone).Year()})
	_ = a.Store.Update(func(s *State) error {
		current, ok := LoadDoc[emailChallenge](s, "email_challenges", id)
		if !ok || current.ID != challenge.ID {
			return nil // Never erase a newer concurrent request.
		}
		if err != nil {
			DeleteDoc(s, "email_challenges", id)
			return identityAudit(s, challenge.Owner, "password.recovery.delivery.failed", maskedEmail(email), mailFailureCategory(err))
		}
		u := s.Users[challenge.Owner]
		// Successful SMTP DATA acceptance remains valid even if cancellation
		// happened during QUIT; the code still expires at its original deadline.
		if !recoverableMember(u) || u.Email != email || tokenHash(u.PasswordHash) != challenge.CredentialsHash || current.ExpiresAt <= time.Now().UnixMilli() {
			DeleteDoc(s, "email_challenges", id)
			return nil
		}
		current.Ready = true
		return SaveDoc(s, "email_challenges", id, current)
	})
}

func (a *App) passwordResetConfirm(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !a.allow("password-reset-confirm-ip:"+a.clientIP(r), 30, 15*time.Minute) || !a.allow("password-reset-confirm-global", 100, time.Minute) {
		Fail(w, 429, "尝试过于频繁，请稍后重试")
		return
	}
	var in struct {
		Email          string `json:"email"`
		Code           string `json:"code"`
		NewPassword    string `json:"newPassword"`
		TurnstileToken string `json:"turnstileToken"`
	}
	if err := Decode(r, &in); err != nil {
		Fail(w, 400, "请求格式无效")
		return
	}
	in.Email = strings.ToLower(strings.TrimSpace(in.Email))
	if !qqEmail(in.Email) {
		Fail(w, 400, "请填写纯数字 QQ 邮箱")
		return
	}
	if err := validPassword(in.NewPassword, in.Email); err != nil {
		Fail(w, 400, err.Error())
		return
	}
	if !a.allow("password-reset-confirm-email:"+tokenHash(in.Email), 15, 15*time.Minute) {
		Fail(w, 429, "尝试过于频繁，请稍后重试")
		return
	}
	if err := a.verifyTurnstile(r, in.TurnstileToken); err != nil {
		Fail(w, 400, err.Error())
		return
	}
	// Perform the same bounded bcrypt work for all eligible input shapes, not
	// just real users. The transaction below rechecks all mutable conditions.
	hash, err := bcrypt.GenerateFromPassword([]byte(in.NewPassword), bcrypt.DefaultCost)
	in.NewPassword = ""
	if err != nil {
		Fail(w, 500, "密码处理失败")
		return
	}
	accepted := false
	var owner string
	var config SMTPConfig
	var notify bool
	var changedAt int64
	err = a.Store.Update(func(s *State) error {
		c, ok := LoadDoc[emailChallenge](s, "email_challenges", challengeID(passwordResetPurpose, in.Email))
		if !ok || c.Purpose != passwordResetPurpose || c.Email != in.Email {
			return nil
		}
		u := s.Users[c.Owner]
		if !recoverableMember(u) || u.Email != in.Email || c.CredentialsHash == "" || c.CredentialsHash != tokenHash(u.PasswordHash) {
			return nil
		}
		if err := consumeChallenge(s, passwordResetPurpose, in.Email, u.ID, in.Code); err != nil {
			// Commit the failure counter rather than rolling back its increment.
			return nil
		}
		u.PasswordHash = string(hash)
		revokeSessions(s, u.ID)
		for id := range s.Docs["email_challenges"] {
			if challenge, ok := LoadDoc[emailChallenge](s, "email_challenges", id); ok && challenge.Purpose == passwordResetPurpose && (challenge.Owner == u.ID || challenge.Email == u.Email) {
				DeleteDoc(s, "email_challenges", id)
			}
		}
		// Proving mailbox control for recovery does not silently change separate
		// email-verification/free-tool policies or any subscription/balance.
		owner, changedAt = u.ID, time.Now().UnixMilli()
		config, _ = getSMTP(s)
		notify = smtpReady(s)
		accepted = true
		return identityAudit(s, u.ID, "password.recovery.completed", u.ID, "member_email_code")
	})
	if err != nil {
		Fail(w, 500, "密码更新失败，请稍后重试")
		return
	}
	if !accepted {
		Fail(w, 400, passwordResetDenied)
		return
	}
	if !notify || !a.queuePasswordMail(func(ctx context.Context) {
		if err := a.sendAccountEmail(ctx, config, in.Email, "password_changed", EmailData{EventAtLabel: emailTimeLabel(changedAt), Year: time.UnixMilli(changedAt).In(emailDisplayZone).Year()}); err != nil {
			_ = a.Store.Update(func(s *State) error {
				return identityAudit(s, owner, "password.recovery.notice.failed", maskedEmail(in.Email), mailFailureCategory(err))
			})
		}
	}) {
		_ = a.Store.Update(func(s *State) error {
			return identityAudit(s, owner, "password.recovery.notice.failed", maskedEmail(in.Email), "delivery_unavailable")
		})
	}
	// Never automatically sign in. Old sessions, including this browser's,
	// have been revoked atomically with the password change.
	WriteJSON(w, 200, map[string]any{"ok": true, "message": "密码已重置，请使用新密码重新登录。"})
}
