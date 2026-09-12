package control

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

type User struct {
	ID              string `json:"id"`
	Email           string `json:"email"`
	Role            string `json:"role"`
	Status          string `json:"status"`
	PasswordHash    string `json:"passwordHash,omitempty"`
	EmailVerifiedAt int64  `json:"emailVerifiedAt"`
	CreatedAt       int64  `json:"createdAt"`
	ExpiresAt       int64  `json:"expiresAt"`
	BalanceCents    int64  `json:"balanceCents"`
	TrafficTotal    int64  `json:"trafficTotal"`
	TrafficUsed     int64  `json:"trafficUsed"`
	RateMbps        int64  `json:"rateMbps"`
}

type Session struct {
	UserID    string `json:"userId"`
	CSRFToken string `json:"csrfToken"`
	CreatedAt int64  `json:"createdAt"`
	ExpiresAt int64  `json:"expiresAt"`
}

type SMTPConfig struct {
	Host                 string `json:"host"`
	Port                 int    `json:"port"`
	Sender               string `json:"sender"`
	Encryption           string `json:"encryption"`
	Name                 string `json:"name"`
	Secret               string `json:"secret,omitempty"`
	CredentialConfigured bool   `json:"credentialConfigured"`
	TestedAt             int64  `json:"testedAt"`
}

type Invitation struct {
	ID        string          `json:"id"`
	Code      string          `json:"code"`
	MaxUses   int             `json:"maxUses"`
	Uses      []InvitationUse `json:"uses"`
	Enabled   bool            `json:"enabled"`
	Archived  bool            `json:"archived"`
	CreatedAt int64           `json:"createdAt"`
	Note      string          `json:"note"`
	Batch     string          `json:"batch"`
}
type InvitationUse struct {
	User string `json:"user"`
	At   int64  `json:"at"`
}
type emailChallenge struct {
	ID        string `json:"id"`
	Email     string `json:"email"`
	Owner     string `json:"owner"`
	Purpose   string `json:"purpose"`
	Hash      string `json:"hash"`
	CreatedAt int64  `json:"createdAt"`
	ExpiresAt int64  `json:"expiresAt"`
	Attempts  int    `json:"attempts"`
	Ready     bool   `json:"ready"`
}

func safeUser(u *User) *User {
	if u == nil {
		return nil
	}
	copy := *u
	copy.PasswordHash = ""
	return &copy
}

func (a *App) initialize() error {
	if (a.Config.AdminEmail == "") != (a.Config.AdminPassword == "") {
		return errors.New("ADMIN_EMAIL and ADMIN_PASSWORD must be supplied together")
	}
	var hash []byte
	var err error
	if a.Config.AdminEmail != "" {
		if _, err = mail.ParseAddress(a.Config.AdminEmail); err != nil {
			return errors.New("invalid ADMIN_EMAIL")
		}
		if err = validPassword(a.Config.AdminPassword); err != nil {
			return err
		}
		hash, err = bcrypt.GenerateFromPassword([]byte(a.Config.AdminPassword), bcrypt.DefaultCost)
		if err != nil {
			return err
		}
	}
	return a.Store.Update(func(s *State) error {
		defaults := map[string]any{"register": true, "invite": false, "turnstile": false, "smtp": false, "deploy": true, "relay": true, "dd": true, "paidCreate": true, "planSale": true, "cards": true, "paywx": true, "payali": true, "monitor": true, "localSave": true, "publicArticles": true, "attachments": true, "maintenance": false, "registrationEmailVerificationRequired": false, "freeToolsRequireVerifiedEmail": false, "purchaseRequireVerifiedEmail": false, "limits": map[string]any{"deploy": map[string]any{"minutes": 15, "count": 5}, "relay": map[string]any{"minutes": 15, "count": 5}, "dd": map[string]any{"minutes": 15, "count": 5}, "fingerprint": map[string]any{"minutes": 30, "count": 10}}}
		for k, v := range defaults {
			if _, ok := s.Settings[k]; !ok {
				s.Settings[k] = v
			}
		}
		if len(hash) > 0 {
			for _, u := range s.Users {
				if u.Role == "admin" {
					return nil
				}
			}
			email := strings.ToLower(strings.TrimSpace(a.Config.AdminEmail))
			for _, u := range s.Users {
				if u.Email == email {
					return errors.New("bootstrap email already belongs to a non-admin user")
				}
			}
			u := &User{ID: ID(), Email: email, Role: "admin", Status: "active", PasswordHash: string(hash), CreatedAt: time.Now().UnixMilli(), RateMbps: 1}
			s.Users[u.ID] = u
		}
		return nil
	})
}

func validPassword(password string) error {
	if len(password) < 12 || len(password) > 72 {
		return errors.New("密码需为 12–72 字节")
	}
	return nil
}

func sendSMTP(ctx context.Context, config SMTPConfig, secret, recipient, body string) error {
	address := net.JoinHostPort(config.Host, strconv.Itoa(config.Port))
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	var conn net.Conn
	var err error
	tlsConfig := &tls.Config{ServerName: config.Host, MinVersion: tls.VersionTLS12}
	if config.Encryption == "tls" {
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: tlsConfig}).DialContext(ctx, "tcp", address)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", address)
	}
	if err != nil {
		return err
	}
	defer conn.Close()
	deadline := time.Now().Add(25 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)
	client, err := smtp.NewClient(conn, config.Host)
	if err != nil {
		return err
	}
	defer client.Close()
	if config.Encryption == "starttls" {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return errors.New("SMTP server does not support required STARTTLS")
		}
		if err = client.StartTLS(tlsConfig); err != nil {
			return err
		}
	}
	if err = client.Auth(smtp.PlainAuth("", config.Sender, secret, config.Host)); err != nil {
		return err
	}
	if err = client.Mail(config.Sender); err != nil {
		return err
	}
	if err = client.Rcpt(recipient); err != nil {
		return err
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	from := (&mail.Address{Name: config.Name, Address: config.Sender}).String()
	message := "From: " + from + "\r\nTo: " + recipient + "\r\nSubject: MSBOOST email verification\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n" + body + "\r\n"
	if _, err = w.Write([]byte(message)); err != nil {
		return err
	}
	if err = w.Close(); err != nil {
		return err
	}
	return client.Quit()
}

// RegisterIdentity is extended below; all routes are called behind Authenticate.
func (a *App) RegisterIdentity(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/me", a.me)
	mux.HandleFunc("POST /api/auth/register", a.register)
	mux.HandleFunc("POST /api/auth/login", a.login)
	mux.HandleFunc("POST /api/auth/logout", a.logout)
	mux.HandleFunc("POST /api/auth/email/send", a.emailSend)
	mux.HandleFunc("POST /api/auth/email/verify", a.emailVerify)
	mux.HandleFunc("GET /api/settings", a.publicSettings)
	mux.HandleFunc("GET /api/admin/settings", a.adminSettings)
	mux.HandleFunc("PUT /api/admin/settings", a.adminSettings)
	mux.HandleFunc("PATCH /api/admin/settings", a.adminSettings)
	mux.HandleFunc("POST /api/admin/smtp/test", a.smtpTest)
	mux.HandleFunc("GET /api/admin/users", a.adminUsers)
	mux.HandleFunc("POST /api/admin/users", a.adminUsers)
	mux.HandleFunc("PATCH /api/admin/users/{id}", a.adminUser)
	mux.HandleFunc("PUT /api/admin/users/{id}", a.adminUser)
	mux.HandleFunc("DELETE /api/admin/users/{id}", a.adminUser)
	mux.HandleFunc("POST /api/admin/users/{id}/password", a.adminPassword)
	mux.HandleFunc("GET /api/admin/invitations", a.invitations)
	mux.HandleFunc("POST /api/admin/invitations", a.invitations)
	mux.HandleFunc("POST /api/admin/invitations/batch", a.invitationBatch)
	mux.HandleFunc("PATCH /api/admin/invitations/{id}", a.invitation)
	mux.HandleFunc("PUT /api/admin/invitations/{id}", a.invitation)
	mux.HandleFunc("DELETE /api/admin/invitations/{id}", a.invitation)
}

func (a *App) me(w http.ResponseWriter, r *http.Request) {
	i, err := a.identity(r)
	if err != nil {
		WriteJSON(w, 200, map[string]any{"user": nil, "csrfToken": ""})
		return
	}
	WriteJSON(w, 200, map[string]any{"user": safeUser(i.user), "csrfToken": i.session.CSRFToken})
}

func (a *App) login(w http.ResponseWriter, r *http.Request) {
	if !a.allow("login-ip:"+a.clientIP(r), 20, 15*time.Minute) {
		Fail(w, 429, "登录尝试过于频繁，请稍后重试")
		return
	}
	var in struct {
		Email          string `json:"email"`
		Password       string `json:"password"`
		Agree          bool   `json:"agree"`
		TurnstileToken string `json:"turnstileToken"`
	}
	if err := Decode(r, &in); err != nil {
		Fail(w, 400, err.Error())
		return
	}
	email := strings.ToLower(strings.TrimSpace(in.Email))
	if !a.allow("login-email:"+email, 10, 15*time.Minute) {
		Fail(w, 429, "登录尝试过于频繁，请稍后重试")
		return
	}
	if err := a.verifyTurnstile(r, in.TurnstileToken); err != nil {
		Fail(w, 400, err.Error())
		return
	}
	var found *User
	if err := a.Store.View(func(s *State) error {
		for _, u := range s.Users {
			if u.Email == email {
				copy := *u
				found = &copy
				break
			}
		}
		return nil
	}); err != nil {
		Fail(w, 500, "数据库读取失败")
		return
	}
	hash := "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"
	if found != nil {
		hash = found.PasswordHash
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(in.Password)) != nil || found == nil || found.Status != "active" {
		Fail(w, 401, "邮箱或密码错误，或账号已停用")
		return
	}
	token, csrf := ID(), ID()
	var out *User
	err := a.Store.Update(func(s *State) error {
		u := s.Users[found.ID]
		if u == nil || u.Status != "active" || u.PasswordHash != found.PasswordHash {
			return errors.New("账号状态已变化，请重新登录")
		}
		out = safeUser(u)
		newSession(s, u.ID, token, csrf)
		return nil
	})
	if err != nil {
		Fail(w, 401, err.Error())
		return
	}
	a.setCookie(w, token, 7*24*3600)
	WriteJSON(w, 200, map[string]any{"user": out, "csrfToken": csrf})
}

func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	if i, err := a.identity(r); err == nil {
		if err = a.Store.Update(func(s *State) error { delete(s.Sessions, i.hash); return nil }); err != nil {
			Fail(w, 500, "退出失败")
			return
		}
	}
	a.setCookie(w, "", -1)
	WriteJSON(w, 200, map[string]any{"ok": true})
}

func newSession(s *State, userID, token, csrf string) {
	now := time.Now().UnixMilli()
	for k, v := range s.Sessions {
		if v.ExpiresAt <= now {
			delete(s.Sessions, k)
		}
	}
	s.Sessions[tokenHash(token)] = &Session{UserID: userID, CSRFToken: csrf, CreatedAt: now, ExpiresAt: now + int64(7*24*time.Hour/time.Millisecond)}
}
func (a *App) setCookie(w http.ResponseWriter, token string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: "msboost_session", Value: token, Path: "/", HttpOnly: true, Secure: a.Config.SecureCookies, SameSite: http.SameSiteLaxMode, MaxAge: maxAge})
}

func qqEmail(email string) bool {
	if !strings.HasSuffix(email, "@qq.com") {
		return false
	}
	id := strings.TrimSuffix(email, "@qq.com")
	if len(id) < 5 || len(id) > 12 {
		return false
	}
	for _, c := range id {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func (a *App) register(w http.ResponseWriter, r *http.Request) {
	if !a.allow("register:"+a.clientIP(r), 10, time.Hour) {
		Fail(w, 429, "注册过于频繁，请稍后重试")
		return
	}
	var in struct {
		Email          string `json:"email"`
		Password       string `json:"password"`
		InviteCode     string `json:"inviteCode"`
		Code           string `json:"code"`
		Agree          bool   `json:"agree"`
		TurnstileToken string `json:"turnstileToken"`
	}
	if err := Decode(r, &in); err != nil {
		Fail(w, 400, err.Error())
		return
	}
	in.Email = strings.ToLower(strings.TrimSpace(in.Email))
	if !qqEmail(in.Email) {
		Fail(w, 400, "请填写纯数字 QQ 邮箱")
		return
	}
	if !in.Agree {
		Fail(w, 400, "请先同意用户协议与隐私政策")
		return
	}
	if err := validPassword(in.Password); err != nil {
		Fail(w, 400, err.Error())
		return
	}
	if err := a.verifyTurnstile(r, in.TurnstileToken); err != nil {
		Fail(w, 400, err.Error())
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
	if err != nil {
		Fail(w, 500, "密码处理失败")
		return
	}
	token, csrf := ID(), ID()
	var out *User
	var denied error
	err = a.Store.Update(func(s *State) error {
		if !boolSetting(s, "register") {
			return errors.New("注册已关闭")
		}
		for _, u := range s.Users {
			if u.Email == in.Email {
				return errors.New("此邮箱已注册，请直接登录")
			}
		}
		var invite *Invitation
		if boolSetting(s, "invite") {
			for _, v := range ListDocs[Invitation](s, "invitations") {
				if v.Code == in.InviteCode && !v.Archived && v.Enabled && len(v.Uses) < v.MaxUses {
					copy := v
					invite = &copy
					break
				}
			}
			if invite == nil {
				return errors.New("邀请码不可用或次数已用完")
			}
		}
		verified := int64(0)
		if boolSetting(s, "registrationEmailVerificationRequired") {
			if e := consumeChallenge(s, "register", in.Email, "", in.Code); e != nil {
				denied = e
				return nil
			}
			verified = time.Now().UnixMilli()
		}
		u := &User{ID: ID(), Email: in.Email, Role: "member", Status: "active", PasswordHash: string(hash), EmailVerifiedAt: verified, CreatedAt: time.Now().UnixMilli(), RateMbps: 1}
		s.Users[u.ID] = u
		if invite != nil {
			invite.Uses = append(invite.Uses, InvitationUse{User: u.Email, At: u.CreatedAt})
			if e := SaveDoc(s, "invitations", invite.ID, invite); e != nil {
				return e
			}
		}
		newSession(s, u.ID, token, csrf)
		out = safeUser(u)
		return nil
	})
	if err == nil {
		err = denied
	}
	if err != nil {
		Fail(w, 400, err.Error())
		return
	}
	a.setCookie(w, token, 7*24*3600)
	WriteJSON(w, 201, map[string]any{"user": out, "csrfToken": csrf})
}

func canonicalPurpose(p string) string {
	if p == "registration" {
		return "register"
	}
	if p == "account" {
		return "verify"
	}
	return p
}
func challengeID(purpose, email string) string { return tokenHash(purpose + ":" + email) }
func challengeHash(id, code string) string {
	h := sha256.Sum256([]byte(id + ":" + code))
	return hex.EncodeToString(h[:])
}
func consumeChallenge(s *State, purpose, email, owner, code string) error {
	id := challengeID(purpose, email)
	c, ok := LoadDoc[emailChallenge](s, "email_challenges", id)
	if !ok || !c.Ready || c.ExpiresAt <= time.Now().UnixMilli() || c.Owner != owner {
		return errors.New("验证码不存在或已过期，请重新获取")
	}
	if c.Attempts >= 5 {
		return errors.New("验证错误次数过多，请重新获取验证码")
	}
	if subtle.ConstantTimeCompare([]byte(c.Hash), []byte(challengeHash(c.ID, code))) != 1 {
		c.Attempts++
		if err := SaveDoc(s, "email_challenges", id, c); err != nil {
			return err
		}
		return errors.New("验证码不正确")
	}
	DeleteDoc(s, "email_challenges", id)
	return nil
}

func getSMTP(s *State) (SMTPConfig, error) {
	var c SMTPConfig
	raw, err := json.Marshal(s.Settings["smtpConfig"])
	if err != nil {
		return c, err
	}
	err = json.Unmarshal(raw, &c)
	return c, err
}
func smtpReady(s *State) bool {
	c, err := getSMTP(s)
	return err == nil && boolSetting(s, "smtp") && c.Secret != "" && c.TestedAt > 0
}

func (a *App) emailSend(w http.ResponseWriter, r *http.Request) {
	if !a.allow("email-ip:"+a.clientIP(r), 20, time.Hour) {
		Fail(w, 429, "验证码发送过于频繁")
		return
	}
	var in struct {
		Email          string `json:"email"`
		Purpose        string `json:"purpose"`
		TurnstileToken string `json:"turnstileToken"`
	}
	if err := Decode(r, &in); err != nil {
		Fail(w, 400, err.Error())
		return
	}
	in.Purpose = canonicalPurpose(in.Purpose)
	in.Email = strings.ToLower(strings.TrimSpace(in.Email))
	owner := ""
	if in.Purpose == "verify" {
		u, err := a.User(r)
		if err != nil {
			Fail(w, 401, err.Error())
			return
		}
		in.Email = u.Email
		owner = u.ID
	} else if in.Purpose != "register" {
		Fail(w, 400, "验证码用途无效")
		return
	}
	if !qqEmail(in.Email) {
		Fail(w, 400, "请填写纯数字 QQ 邮箱")
		return
	}
	if in.Purpose == "register" {
		if err := a.verifyTurnstile(r, in.TurnstileToken); err != nil {
			Fail(w, 400, err.Error())
			return
		}
	}
	var config SMTPConfig
	n, err := rand.Int(rand.Reader, big.NewInt(1000000))
	if err != nil {
		Fail(w, 500, "验证码生成失败")
		return
	}
	code := fmt.Sprintf("%06d", n.Int64())
	now := time.Now().UnixMilli()
	id := challengeID(in.Purpose, in.Email)
	c := emailChallenge{ID: ID(), Email: in.Email, Owner: owner, Purpose: in.Purpose, CreatedAt: now, ExpiresAt: now + 600000}
	c.Hash = challengeHash(c.ID, code)
	err = a.Store.Update(func(s *State) error {
		if !smtpReady(s) {
			return errors.New("邮件服务未就绪，请联系管理员")
		}
		if in.Purpose == "register" {
			if !boolSetting(s, "register") || !boolSetting(s, "registrationEmailVerificationRequired") {
				return errors.New("当前注册无需验证码，或注册已关闭")
			}
			for _, u := range s.Users {
				if u.Email == in.Email {
					return errors.New("邮箱已注册，请登录后验证")
				}
			}
		}
		if old, ok := LoadDoc[emailChallenge](s, "email_challenges", id); ok && now-old.CreatedAt < 60000 {
			return errors.New("请等待 60 秒后重新发送")
		}
		var e error
		config, e = getSMTP(s)
		if e != nil {
			return e
		}
		return SaveDoc(s, "email_challenges", id, c)
	})
	if err != nil {
		Fail(w, 400, err.Error())
		return
	}
	secret, err := a.Open(config.Secret)
	if err == nil {
		err = a.mailSender(r.Context(), config, string(secret), in.Email, "您的 MSBOOST 验证码为："+code+"\n10 分钟内有效。请勿向任何人透露此验证码。")
	}
	if err != nil {
		category := mailFailureCategory(err)
		_ = a.Store.Update(func(s *State) error {
			if current, ok := LoadDoc[emailChallenge](s, "email_challenges", id); ok && current.ID == c.ID {
				DeleteDoc(s, "email_challenges", id)
			}
			return identityAudit(s, owner, "smtp.delivery.failed", in.Email, category)
		})
		Fail(w, 502, "邮件发送失败，请联系管理员检查 SMTP 配置")
		return
	}
	err = a.Store.Update(func(s *State) error {
		current, ok := LoadDoc[emailChallenge](s, "email_challenges", id)
		if !ok || current.ID != c.ID {
			return errors.New("验证码已失效，请重新发送")
		}
		current.Ready = true
		return SaveDoc(s, "email_challenges", id, current)
	})
	if err != nil {
		Fail(w, 500, "验证码状态保存失败")
		return
	}
	WriteJSON(w, 200, map[string]any{"ok": true, "expiresIn": 600, "retryAfter": 60})
}

func (a *App) emailVerify(w http.ResponseWriter, r *http.Request) {
	u, err := a.User(r)
	if err != nil {
		Fail(w, 401, err.Error())
		return
	}
	var in struct {
		Code string `json:"code"`
	}
	if err = Decode(r, &in); err != nil {
		Fail(w, 400, err.Error())
		return
	}
	var denied error
	var out *User
	err = a.Store.Update(func(s *State) error {
		current := s.Users[u.ID]
		if current == nil || current.Status != "active" || current.Email != u.Email {
			return errors.New("账号状态已变化")
		}
		if e := consumeChallenge(s, "verify", current.Email, current.ID, in.Code); e != nil {
			denied = e
			return nil
		}
		current.EmailVerifiedAt = time.Now().UnixMilli()
		out = safeUser(current)
		return nil
	})
	if err == nil {
		err = denied
	}
	if err != nil {
		Fail(w, 400, err.Error())
		return
	}
	WriteJSON(w, 200, map[string]any{"ok": true, "user": out})
}

func (a *App) verifyTurnstile(r *http.Request, token string) error {
	var enabled bool
	var sealed, site string
	if err := a.Store.View(func(s *State) error {
		enabled = boolSetting(s, "turnstile")
		sealed, _ = s.Settings["turnstileSecret"].(string)
		site, _ = s.Settings["turnstileSiteKey"].(string)
		return nil
	}); err != nil {
		return err
	}
	if !enabled {
		return nil
	}
	if token == "" || sealed == "" || site == "" {
		return errors.New("请完成人机验证")
	}
	secret, err := a.Open(sealed)
	if err != nil {
		return errors.New("人机验证配置无效")
	}
	form := url.Values{"secret": {string(secret)}, "response": {token}, "remoteip": {a.clientIP(r)}}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, "https://challenges.cloudflare.com/turnstile/v0/siteverify", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return errors.New("人机验证服务暂不可用")
	}
	defer res.Body.Close()
	var result struct {
		Success  bool   `json:"success"`
		Hostname string `json:"hostname"`
	}
	if err = json.NewDecoder(res.Body).Decode(&result); err != nil || !result.Success {
		return errors.New("人机验证失败")
	}
	if a.Config.PublicURL != "" {
		u, _ := url.Parse(a.Config.PublicURL)
		if result.Hostname != u.Hostname() {
			return errors.New("人机验证站点不匹配")
		}
	}
	return nil
}

// Administration handlers are defined in the next increment.
func (a *App) publicSettings(w http.ResponseWriter, r *http.Request) {
	var settings map[string]any
	err := a.Store.View(func(s *State) error { settings = filteredSettings(s, false); return nil })
	if err != nil {
		Fail(w, 500, "数据库读取失败")
		return
	}
	WriteJSON(w, 200, settings)
}
func filteredSettings(s *State, admin bool) map[string]any {
	out := map[string]any{}
	for _, key := range []string{"register", "invite", "turnstile", "smtp", "deploy", "relay", "dd", "paidCreate", "planSale", "cards", "paywx", "payali", "monitor", "localSave", "publicArticles", "attachments", "maintenance", "registrationEmailVerificationRequired", "freeToolsRequireVerifiedEmail", "purchaseRequireVerifiedEmail", "turnstileSiteKey", "limits"} {
		out[key] = s.Settings[key]
	}
	out["registrationEmailVerification"] = out["registrationEmailVerificationRequired"]
	out["freeToolsVerifiedOnly"] = out["freeToolsRequireVerifiedEmail"]
	out["smtpReady"] = smtpReady(s)
	if admin {
		c, _ := getSMTP(s)
		c.CredentialConfigured = c.Secret != ""
		c.Secret = ""
		out["smtpConfig"] = c
		out["turnstileSecretConfigured"] = s.Settings["turnstileSecret"] != "" && s.Settings["turnstileSecret"] != nil
	}
	return out
}
func (a *App) adminSettings(w http.ResponseWriter, r *http.Request) {
	actor, err := a.Admin(r)
	if err != nil {
		Fail(w, 403, err.Error())
		return
	}
	if r.Method != "GET" {
		var patch map[string]json.RawMessage
		if err = Decode(r, &patch); err != nil {
			Fail(w, 400, err.Error())
			return
		}
		err = a.Store.Update(func(s *State) error {
			for key, raw := range patch {
				if key == "registrationEmailVerification" {
					key = "registrationEmailVerificationRequired"
				}
				if key == "freeToolsVerifiedOnly" {
					key = "freeToolsRequireVerifiedEmail"
				}
				switch key {
				case "register", "invite", "turnstile", "smtp", "deploy", "relay", "dd", "paidCreate", "planSale", "cards", "paywx", "payali", "monitor", "localSave", "publicArticles", "attachments", "maintenance", "registrationEmailVerificationRequired", "freeToolsRequireVerifiedEmail", "purchaseRequireVerifiedEmail":
					var value bool
					if json.Unmarshal(raw, &value) != nil {
						return fmt.Errorf("%s 必须为布尔值", key)
					}
					s.Settings[key] = value
				case "turnstileSiteKey":
					var value string
					if json.Unmarshal(raw, &value) != nil || len(value) > 200 {
						return errors.New("Site Key 无效")
					}
					s.Settings[key] = strings.TrimSpace(value)
				case "turnstileSecret":
					var value string
					if json.Unmarshal(raw, &value) != nil || len(value) > 1000 {
						return errors.New("Secret Key 无效")
					}
					if value != "" {
						sealed, e := a.Seal([]byte(value))
						if e != nil {
							return e
						}
						s.Settings[key] = sealed
					}
				case "smtpConfig":
					var config SMTPConfig
					if json.Unmarshal(raw, &config) != nil {
						return errors.New("SMTP 配置格式无效")
					}
					old, _ := getSMTP(s)
					config, e := a.prepareSMTP(config, old)
					if e != nil {
						return e
					}
					if config.Host != old.Host || config.Port != old.Port || config.Sender != old.Sender || config.Encryption != old.Encryption || config.Name != old.Name || config.Secret != old.Secret {
						config.TestedAt = 0
					} else {
						config.TestedAt = old.TestedAt
					}
					s.Settings[key] = config
				case "limits":
					var limits map[string]struct {
						Minutes float64 `json:"minutes"`
						Count   float64 `json:"count"`
					}
					if json.Unmarshal(raw, &limits) != nil || len(limits) != 4 {
						return errors.New("请提供四项完整限频规则")
					}
					for _, kind := range []string{"deploy", "relay", "dd", "fingerprint"} {
						v, ok := limits[kind]
						if !ok || v.Minutes < 1 || v.Minutes > 1440 || v.Count < 1 || v.Count > 100 || math.Trunc(v.Minutes) != v.Minutes || math.Trunc(v.Count) != v.Count {
							return errors.New("时间窗口需为 1–1440 分钟，次数为 1–100")
						}
					}
					s.Settings[key] = limits
				default:
					return fmt.Errorf("不支持的设置字段 %s", key)
				}
			}
			if (boolSetting(s, "registrationEmailVerificationRequired") || boolSetting(s, "freeToolsRequireVerifiedEmail") || boolSetting(s, "purchaseRequireVerifiedEmail")) && !smtpReady(s) {
				return errors.New("请先关闭邮箱验证策略，配置并实际发送 SMTP 测试邮件后再启用")
			}
			if boolSetting(s, "turnstile") {
				secret, _ := s.Settings["turnstileSecret"].(string)
				site, _ := s.Settings["turnstileSiteKey"].(string)
				if secret == "" || site == "" {
					return errors.New("开启人机验证前请先配置 Site Key 与 Secret Key")
				}
			}
			return identityAudit(s, actor.ID, "settings.update", "", "")
		})
		if err != nil {
			Fail(w, 400, err.Error())
			return
		}
	}
	var out map[string]any
	err = a.Store.View(func(s *State) error { out = filteredSettings(s, true); return nil })
	if err != nil {
		Fail(w, 500, "数据库读取失败")
		return
	}
	WriteJSON(w, 200, out)
}

func (a *App) prepareSMTP(in, old SMTPConfig) (SMTPConfig, error) {
	in.Host = strings.TrimSpace(in.Host)
	in.Sender = strings.ToLower(strings.TrimSpace(in.Sender))
	if in.Host == "" || strings.ContainsAny(in.Host, "\r\n /\\@:") || in.Port < 1 || in.Port > 65535 {
		return in, errors.New("SMTP 主机或端口无效")
	}
	address, err := mail.ParseAddress(in.Sender)
	if err != nil || address.Address != in.Sender {
		return in, errors.New("发件邮箱无效")
	}
	if in.Encryption != "tls" && in.Encryption != "starttls" {
		return in, errors.New("邮件发送必须使用 TLS 或 STARTTLS")
	}
	if len(in.Name) > 120 || strings.ContainsAny(in.Name, "\r\n") {
		return in, errors.New("邮件显示名称无效")
	}
	if in.Name == "" {
		in.Name = "MSBOOST"
	}
	if in.Secret == "" {
		in.Secret = old.Secret
	} else {
		if len(in.Secret) > 4096 {
			return in, errors.New("SMTP 授权码过长")
		}
		in.Secret, err = a.Seal([]byte(in.Secret))
		if err != nil {
			return in, err
		}
	}
	if in.Secret == "" {
		return in, errors.New("请填写 SMTP 授权码")
	}
	in.CredentialConfigured = true
	in.TestedAt = 0
	return in, nil
}

func (a *App) smtpTest(w http.ResponseWriter, r *http.Request) {
	actor, err := a.Admin(r)
	if err != nil {
		Fail(w, 403, err.Error())
		return
	}
	if !a.allow("smtp-test:"+actor.ID, 5, time.Minute) {
		Fail(w, 429, "测试邮件发送过于频繁")
		return
	}
	var in struct {
		Recipient  string      `json:"recipient"`
		SMTPConfig *SMTPConfig `json:"smtpConfig"`
	}
	if err = Decode(r, &in); err != nil {
		Fail(w, 400, err.Error())
		return
	}
	if in.Recipient == "" {
		in.Recipient = actor.Email
	}
	address, err := mail.ParseAddress(in.Recipient)
	if err != nil || address.Address != in.Recipient {
		Fail(w, 400, "测试收件邮箱无效")
		return
	}
	var config SMTPConfig
	err = a.Store.View(func(s *State) error {
		if !boolSetting(s, "smtp") {
			return errors.New("请先开启 SMTP 邮件发送")
		}
		var e error
		config, e = getSMTP(s)
		return e
	})
	if err != nil {
		Fail(w, 400, err.Error())
		return
	}
	original := config
	if in.SMTPConfig != nil {
		config, err = a.prepareSMTP(*in.SMTPConfig, config)
		if err != nil {
			Fail(w, 400, err.Error())
			return
		}
	}
	secret, err := a.Open(config.Secret)
	if err == nil {
		err = a.mailSender(r.Context(), config, string(secret), in.Recipient, "这是一封 MSBOOST 邮件服务测试邮件。您的 SMTP 配置已完成实际发送验证。")
	}
	if err != nil {
		_ = a.Store.Update(func(s *State) error {
			return identityAudit(s, actor.ID, "smtp.test.failed", in.Recipient, mailFailureCategory(err))
		})
		Fail(w, 502, "SMTP 测试发送失败，请检查主机、端口、授权码和 TLS 配置")
		return
	}
	err = a.Store.Update(func(s *State) error {
		current, _ := getSMTP(s)
		if current != original {
			return errors.New("SMTP 配置已被其他管理员修改，请重新测试")
		}
		if !boolSetting(s, "smtp") {
			return errors.New("SMTP 服务已被关闭")
		}
		config.TestedAt = time.Now().UnixMilli()
		s.Settings["smtpConfig"] = config
		return identityAudit(s, actor.ID, "smtp.test", in.Recipient, "")
	})
	if err != nil {
		Fail(w, 409, err.Error())
		return
	}
	WriteJSON(w, 200, map[string]any{"ok": true, "testedAt": config.TestedAt})
}

type adminUserInput struct {
	Email        *string `json:"email"`
	Password     string  `json:"password"`
	Role         *string `json:"role"`
	Status       *string `json:"status"`
	BalanceCents *int64  `json:"balanceCents"`
	ExpiresAt    *int64  `json:"expiresAt"`
	TrafficTotal *int64  `json:"trafficTotal"`
	TrafficUsed  *int64  `json:"trafficUsed"`
	RateMbps     *int64  `json:"rateMbps"`
	Reason       string  `json:"reason"`
}

func (a *App) adminUsers(w http.ResponseWriter, r *http.Request) {
	actor, err := a.Admin(r)
	if err != nil {
		Fail(w, 403, err.Error())
		return
	}
	if r.Method == "POST" {
		a.saveAdminUser(w, r, actor, "")
		return
	}
	users := []*User{}
	err = a.Store.View(func(s *State) error {
		for _, u := range s.Users {
			users = append(users, safeUser(u))
		}
		return nil
	})
	if err != nil {
		Fail(w, 500, "数据库读取失败")
		return
	}
	sort.Slice(users, func(i, j int) bool { return users[i].CreatedAt > users[j].CreatedAt })
	WriteJSON(w, 200, map[string]any{"users": users})
}
func (a *App) adminUser(w http.ResponseWriter, r *http.Request) {
	actor, err := a.Admin(r)
	if err != nil {
		Fail(w, 403, err.Error())
		return
	}
	id := r.PathValue("id")
	if r.Method != "DELETE" {
		a.saveAdminUser(w, r, actor, id)
		return
	}
	err = a.Store.Update(func(s *State) error {
		u := s.Users[id]
		if u == nil {
			return errors.New("用户不存在")
		}
		if actor.ID == id {
			return errors.New("不能删除当前登录管理员")
		}
		if u.Role == "admin" && activeAdminCount(s) <= 1 {
			return errors.New("不能删除最后一个管理员")
		}
		delete(s.Users, id)
		revokeSessions(s, id)
		for _, collection := range []string{"email_challenges", "sessions"} {
			for key := range s.Docs[collection] {
				if c, ok := LoadDoc[emailChallenge](s, collection, key); ok && c.Owner == id {
					DeleteDoc(s, collection, key)
				}
			}
		}
		return identityAudit(s, actor.ID, "user.delete", id, "")
	})
	if err != nil {
		Fail(w, 400, err.Error())
		return
	}
	WriteJSON(w, 200, map[string]any{"ok": true})
}
func (a *App) saveAdminUser(w http.ResponseWriter, r *http.Request, actor *User, id string) {
	var in adminUserInput
	if err := Decode(r, &in); err != nil {
		Fail(w, 400, err.Error())
		return
	}
	if id == "" && in.Password == "" {
		Fail(w, 400, "新增用户必须设置密码")
		return
	}
	var hash []byte
	var err error
	if in.Password != "" {
		if err = validPassword(in.Password); err != nil {
			Fail(w, 400, err.Error())
			return
		}
		hash, err = bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
		if err != nil {
			Fail(w, 500, "密码处理失败")
			return
		}
	}
	var out *User
	created := id == ""
	err = a.Store.Update(func(s *State) error {
		u := s.Users[id]
		if created {
			u = &User{ID: ID(), Role: "member", Status: "active", CreatedAt: time.Now().UnixMilli(), RateMbps: 1}
		} else if u == nil {
			return errors.New("用户不存在")
		}
		before := *u
		if in.Email != nil {
			email := strings.ToLower(strings.TrimSpace(*in.Email))
			if !qqEmail(email) {
				return errors.New("请填写纯数字 QQ 邮箱")
			}
			for _, other := range s.Users {
				if other.ID != u.ID && other.Email == email {
					return errors.New("此邮箱已注册")
				}
			}
			if email != u.Email {
				u.Email = email
				u.EmailVerifiedAt = 0
				revokeSessions(s, u.ID)
			}
		}
		if u.Email == "" {
			return errors.New("请填写邮箱")
		}
		if in.Role != nil {
			role := *in.Role
			if role == "user" {
				role = "member"
			}
			if role != "member" && role != "admin" {
				return errors.New("用户角色无效")
			}
			u.Role = role
		}
		if in.Status != nil {
			if *in.Status != "active" && *in.Status != "suspended" {
				return errors.New("用户状态无效")
			}
			u.Status = *in.Status
		}
		if !created && before.Role == "admin" && (u.Role != "admin" || u.Status != "active") {
			if actor.ID == u.ID {
				return errors.New("不能停用或降级当前登录管理员")
			}
			if activeAdminCount(s) == 0 {
				return errors.New("不能停用最后一个管理员")
			}
		}
		if in.RateMbps != nil {
			if *in.RateMbps < 1 || *in.RateMbps > 1000000 {
				return errors.New("速率需为 1–1000000 Mbps")
			}
			u.RateMbps = *in.RateMbps
		}
		for _, v := range []*int64{in.BalanceCents, in.ExpiresAt, in.TrafficTotal, in.TrafficUsed} {
			if v != nil && *v < 0 {
				return errors.New("余额、权益时间和流量不能为负")
			}
		}
		if in.BalanceCents != nil && *in.BalanceCents != u.BalanceCents {
			if strings.TrimSpace(in.Reason) == "" {
				return errors.New("调整余额必须填写原因")
			}
			if err := appendLedger(s, u, *in.BalanceCents-u.BalanceCents, "admin", actor.ID, in.Reason, time.Now().UnixMilli()); err != nil {
				return err
			}
		}
		if in.ExpiresAt != nil {
			u.ExpiresAt = *in.ExpiresAt
		}
		if in.TrafficTotal != nil {
			u.TrafficTotal = *in.TrafficTotal
		}
		if in.TrafficUsed != nil {
			u.TrafficUsed = *in.TrafficUsed
		}
		if u.TrafficUsed > u.TrafficTotal {
			return errors.New("已用流量不能超过总流量")
		}
		if len(hash) > 0 {
			u.PasswordHash = string(hash)
			revokeSessions(s, u.ID)
		}
		if u.Role != before.Role || u.Status != before.Status {
			revokeSessions(s, u.ID)
		}
		if u.ExpiresAt != before.ExpiresAt || u.TrafficTotal != before.TrafficTotal || u.TrafficUsed != before.TrafficUsed || u.RateMbps != before.RateMbps {
			if err := SaveDoc(s, "entitlement_versions", u.ID, entitlementVersion(s, u.ID)+1); err != nil {
				return err
			}
		}
		s.Users[u.ID] = u
		out = safeUser(u)
		return identityAudit(s, actor.ID, "user.save", u.ID, in.Reason)
	})
	if err != nil {
		Fail(w, 400, err.Error())
		return
	}
	status := 200
	if created {
		status = 201
	}
	WriteJSON(w, status, map[string]any{"user": out})
}

func activeAdminCount(s *State) int {
	count := 0
	for _, u := range s.Users {
		if u.Role == "admin" && u.Status == "active" {
			count++
		}
	}
	return count
}
func revokeSessions(s *State, id string) {
	for token, session := range s.Sessions {
		if session.UserID == id {
			delete(s.Sessions, token)
		}
	}
}
func identityAudit(s *State, actor, action, target, reason string) error {
	id := ID()
	return SaveDoc(s, "audit", id, map[string]any{"id": id, "actor": actor, "action": action, "target": target, "reason": reason, "createdAt": time.Now().UnixMilli()})
}

func (a *App) adminPassword(w http.ResponseWriter, r *http.Request) {
	actor, err := a.Admin(r)
	if err != nil {
		Fail(w, 403, err.Error())
		return
	}
	var in struct {
		Password string `json:"password"`
		Reason   string `json:"reason"`
	}
	if err = Decode(r, &in); err != nil {
		Fail(w, 400, err.Error())
		return
	}
	if err = validPassword(in.Password); err != nil {
		Fail(w, 400, err.Error())
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
	if err != nil {
		Fail(w, 500, "密码处理失败")
		return
	}
	err = a.Store.Update(func(s *State) error {
		u := s.Users[r.PathValue("id")]
		if u == nil {
			return errors.New("用户不存在")
		}
		u.PasswordHash = string(hash)
		revokeSessions(s, u.ID)
		return identityAudit(s, actor.ID, "user.password.reset", u.ID, in.Reason)
	})
	if err != nil {
		Fail(w, 400, err.Error())
		return
	}
	WriteJSON(w, 200, map[string]any{"ok": true})
}

func (a *App) invitations(w http.ResponseWriter, r *http.Request) {
	actor, err := a.Admin(r)
	if err != nil {
		Fail(w, 403, err.Error())
		return
	}
	if r.Method == "GET" {
		var out []Invitation
		err = a.Store.View(func(s *State) error { out = ListDocs[Invitation](s, "invitations"); return nil })
		if err != nil {
			Fail(w, 500, "数据库读取失败")
			return
		}
		sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
		WriteJSON(w, 200, map[string]any{"invitations": out})
		return
	}
	var in struct {
		Quantity int    `json:"quantity"`
		Count    int    `json:"count"`
		MaxUses  int    `json:"maxUses"`
		Note     string `json:"note"`
		Batch    string `json:"batch"`
		Enabled  *bool  `json:"enabled"`
	}
	if err = Decode(r, &in); err != nil {
		Fail(w, 400, err.Error())
		return
	}
	if in.Quantity == 0 {
		in.Quantity = in.Count
	}
	if in.Quantity < 1 || in.Quantity > 100 || in.MaxUses < 1 || in.MaxUses > 10000 || len(in.Note) > 300 || len(in.Batch) > 300 {
		Fail(w, 400, "生成数量需为 1–100，每码次数为 1–10000")
		return
	}
	out := []Invitation{}
	err = a.Store.Update(func(s *State) error {
		for i := 0; i < in.Quantity; i++ {
			v := Invitation{ID: ID(), Code: "MSB-INVITE-" + strings.ToUpper(hex.EncodeToString([]byte(ID())[:16])), MaxUses: in.MaxUses, Uses: []InvitationUse{}, Enabled: in.Enabled == nil || *in.Enabled, CreatedAt: time.Now().UnixMilli(), Note: in.Note, Batch: in.Batch}
			if err := SaveDoc(s, "invitations", v.ID, v); err != nil {
				return err
			}
			out = append(out, v)
		}
		return identityAudit(s, actor.ID, "invitation.create", strconv.Itoa(in.Quantity), in.Note)
	})
	if err != nil {
		Fail(w, 400, err.Error())
		return
	}
	WriteJSON(w, 201, map[string]any{"invitations": out})
}
func (a *App) invitation(w http.ResponseWriter, r *http.Request) {
	actor, err := a.Admin(r)
	if err != nil {
		Fail(w, 403, err.Error())
		return
	}
	var in struct {
		MaxUses  *int    `json:"maxUses"`
		Enabled  *bool   `json:"enabled"`
		Archived *bool   `json:"archived"`
		Note     *string `json:"note"`
	}
	if r.Method != "DELETE" {
		if err = Decode(r, &in); err != nil {
			Fail(w, 400, err.Error())
			return
		}
	}
	var out Invitation
	err = a.Store.Update(func(s *State) error {
		v, ok := LoadDoc[Invitation](s, "invitations", r.PathValue("id"))
		if !ok {
			return errors.New("邀请码不存在")
		}
		if r.Method == "DELETE" {
			v.Archived = true
			v.Enabled = false
		} else {
			if in.MaxUses != nil {
				if *in.MaxUses < 1 || *in.MaxUses < len(v.Uses) || *in.MaxUses > 10000 {
					return errors.New("次数不能低于已使用次数，最多 10000")
				}
				v.MaxUses = *in.MaxUses
			}
			if in.Enabled != nil {
				v.Enabled = *in.Enabled
			}
			if in.Archived != nil {
				v.Archived = *in.Archived
			}
			if in.Note != nil {
				if len(*in.Note) > 300 {
					return errors.New("备注过长")
				}
				v.Note = *in.Note
			}
		}
		out = v
		if err := SaveDoc(s, "invitations", v.ID, v); err != nil {
			return err
		}
		return identityAudit(s, actor.ID, "invitation.update", v.ID, "")
	})
	if err != nil {
		Fail(w, 400, err.Error())
		return
	}
	WriteJSON(w, 200, map[string]any{"invitation": out})
}

func (a *App) invitationBatch(w http.ResponseWriter, r *http.Request) {
	actor, err := a.Admin(r)
	if err != nil {
		Fail(w, 403, err.Error())
		return
	}
	var in struct {
		IDs    []string `json:"ids"`
		Action string   `json:"action"`
	}
	if err = Decode(r, &in); err != nil {
		Fail(w, 400, err.Error())
		return
	}
	if len(in.IDs) == 0 || len(in.IDs) > 1000 {
		Fail(w, 400, "请选择 1–1000 个邀请码")
		return
	}
	if in.Action != "enable" && in.Action != "disable" && in.Action != "delete" && in.Action != "export" {
		Fail(w, 400, "批量操作无效")
		return
	}
	out := []Invitation{}
	err = a.Store.Update(func(s *State) error {
		seen := map[string]bool{}
		for _, id := range in.IDs {
			if seen[id] {
				continue
			}
			seen[id] = true
			v, ok := LoadDoc[Invitation](s, "invitations", id)
			if !ok {
				return errors.New("选择的邀请码已不存在")
			}
			switch in.Action {
			case "enable":
				if v.Archived {
					return errors.New("归档的邀请码不能启用")
				}
				v.Enabled = true
			case "disable":
				v.Enabled = false
			case "delete":
				v.Enabled = false
				v.Archived = true
			}
			if in.Action != "export" {
				if e := SaveDoc(s, "invitations", id, v); e != nil {
					return e
				}
			}
			out = append(out, v)
		}
		return identityAudit(s, actor.ID, "invitation.batch."+in.Action, strconv.Itoa(len(out)), "")
	})
	if err != nil {
		Fail(w, 400, err.Error())
		return
	}
	WriteJSON(w, 200, map[string]any{"invitations": out, "count": len(out)})
}

// Persist a bounded operational category, never raw SMTP responses or secrets.
func mailFailureCategory(err error) string {
	var network net.Error
	if errors.As(err, &network) {
		if network.Timeout() {
			return "network_timeout"
		}
		return "network_failure"
	}
	var certificate *tls.CertificateVerificationError
	if errors.As(err, &certificate) {
		return "tls_certificate_failure"
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "535") || strings.Contains(message, "authentication") || strings.Contains(message, "auth") {
		return "authentication_failure"
	}
	if strings.Contains(message, "tls") || strings.Contains(message, "certificate") {
		return "tls_failure"
	}
	return "delivery_failure"
}
