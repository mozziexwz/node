package control

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Config struct {
	Version                                                               string
	DataDir, DatabaseURL, PublicURL, MasterKey, AdminEmail, AdminPassword string
	NodeScript, ReinstallScript, ReinstallSHA256                          string
	SecureCookies                                                         bool
	TrustedProxyCIDRs                                                     []string
}

type App struct {
	Store          *Store
	Config         Config
	aead           cipher.AEAD
	rateMu         sync.Mutex
	rates          map[string][]int64
	mailSender     func(context.Context, SMTPConfig, string, string, string) error
	trustedProxies []*net.IPNet
}

func New(c Config) (*App, error) {
	if c.Version == "" {
		c.Version = "dev"
	}
	var trusted []*net.IPNet
	for _, cidr := range c.TrustedProxyCIDRs {
		if strings.TrimSpace(cidr) == "" {
			continue
		}
		_, network, err := net.ParseCIDR(strings.TrimSpace(cidr))
		if err != nil {
			return nil, fmt.Errorf("invalid trusted proxy CIDR %q: %w", cidr, err)
		}
		trusted = append(trusted, network)
	}
	if c.DataDir == "" {
		c.DataDir = "data"
	}
	if c.PublicURL != "" {
		u, err := url.Parse(c.PublicURL)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			return nil, errors.New("PublicURL must be an http(s) origin")
		}
		c.PublicURL = strings.TrimRight(c.PublicURL, "/")
		if u.Scheme == "https" {
			c.SecureCookies = true
		}
	}
	key, err := masterKey(c)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	store, err := openStore(c)
	if err != nil {
		return nil, err
	}
	a := &App{Store: store, Config: c, aead: aead, rates: map[string][]int64{}, trustedProxies: trusted}
	a.mailSender = sendSMTP
	if err = a.initialize(); err != nil {
		store.Close()
		return nil, err
	}
	return a, nil
}

func masterKey(c Config) ([]byte, error) {
	if c.MasterKey != "" {
		for _, decode := range []func(string) ([]byte, error){hex.DecodeString, base64.StdEncoding.DecodeString, base64.RawURLEncoding.DecodeString} {
			if key, err := decode(c.MasterKey); err == nil && len(key) == 32 {
				return key, nil
			}
		}
		return nil, errors.New("MASTER_KEY must encode exactly 32 random bytes as hex or base64")
	}
	if err := os.MkdirAll(c.DataDir, 0700); err != nil {
		return nil, err
	}
	path := filepath.Join(c.DataDir, "master.key")
	if key, err := os.ReadFile(path); err == nil {
		if len(key) != 32 {
			return nil, errors.New("invalid data/master.key; preserve the existing key to decrypt stored credentials")
		}
		return key, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if os.IsExist(err) {
		return masterKey(c)
	}
	if err != nil {
		return nil, err
	}
	_, writeErr := f.Write(key)
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr != nil {
		return nil, writeErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return key, nil
}

func (a *App) Close() error { return a.Store.Close() }

func ID() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("cryptographic random source unavailable: %v", err))
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func (a *App) Seal(plain []byte) (string, error) {
	nonce := make([]byte, a.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(a.aead.Seal(nonce, nonce, plain, []byte("msboost:v1"))), nil
}

func (a *App) Open(sealed string) ([]byte, error) {
	b, err := base64.RawURLEncoding.DecodeString(sealed)
	if err != nil || len(b) < a.aead.NonceSize() {
		return nil, errors.New("invalid encrypted secret")
	}
	return a.aead.Open(nil, b[:a.aead.NonceSize()], b[a.aead.NonceSize():], []byte("msboost:v1"))
}

func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func Fail(w http.ResponseWriter, status int, msg string) {
	WriteJSON(w, status, map[string]any{"error": msg})
}

func Decode(r *http.Request, v any) error {
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || contentType != "application/json" {
		return errors.New("请求必须使用 application/json")
	}
	decoder := json.NewDecoder(io.LimitReader(r.Body, 2<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(v); err != nil {
		return errors.New("请求 JSON 格式或字段无效")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("请求只能包含一个 JSON 对象")
	}
	return nil
}

type authContextKey struct{}
type requestIdentity struct {
	user    *User
	session *Session
	hash    string
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (a *App) identity(r *http.Request) (*requestIdentity, error) {
	if found, ok := r.Context().Value(authContextKey{}).(*requestIdentity); ok && found != nil {
		return found, nil
	}
	cookie, err := r.Cookie("msboost_session")
	if err != nil || len(cookie.Value) < 32 || len(cookie.Value) > 200 {
		return nil, errors.New("请先登录")
	}
	i := &requestIdentity{hash: tokenHash(cookie.Value)}
	err = a.Store.View(func(s *State) error {
		session := s.Sessions[i.hash]
		if session == nil || session.ExpiresAt <= time.Now().UnixMilli() {
			return errors.New("会话已失效，请重新登录")
		}
		user := s.Users[session.UserID]
		if user == nil || user.Status != "active" {
			return errors.New("账号已停用或不存在")
		}
		u, ss := *user, *session
		i.user, i.session = &u, &ss
		return nil
	})
	return i, err
}

func (a *App) User(r *http.Request) (*User, error) {
	i, err := a.identity(r)
	if err != nil {
		return nil, err
	}
	return i.user, nil
}
func (a *App) Admin(r *http.Request) (*User, error) {
	u, err := a.User(r)
	if err != nil {
		return nil, err
	}
	if u.Role != "admin" {
		return nil, errors.New("仅管理员可执行此操作")
	}
	return u, nil
}

// Authenticate is applied once around the complete API router. It validates
// browser origins and requires a session-bound token for authenticated writes.
func (a *App) Authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		unsafe := r.Method != "GET" && r.Method != "HEAD" && r.Method != "OPTIONS"
		if unsafe {
			origin := r.Header.Get("Origin")
			expected := a.Config.PublicURL
			if expected == "" {
				scheme := "http"
				if r.TLS != nil {
					scheme = "https"
				}
				expected = scheme + "://" + r.Host
			}
			if (origin != "" && origin != expected) || r.Header.Get("Sec-Fetch-Site") == "cross-site" {
				Fail(w, http.StatusForbidden, "请求来源校验失败")
				return
			}
		}
		identity, err := a.identity(r)
		if err == nil {
			if unsafe && subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(identity.session.CSRFToken)) != 1 {
				Fail(w, http.StatusForbidden, "CSRF 校验失败，请刷新页面")
				return
			}
			r = r.WithContext(context.WithValue(r.Context(), authContextKey{}, identity))
		}
		next.ServeHTTP(w, r)
	})
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

// Only proxy hops in an explicit allowlist may supply forwarding information.
// Walk from the server toward the client; stop at the first untrusted hop.
func (a *App) clientIP(r *http.Request) string {
	peer := remoteIP(r)
	trusted := func(raw string) bool {
		ip := net.ParseIP(strings.TrimSpace(raw))
		if ip == nil {
			return false
		}
		for _, network := range a.trustedProxies {
			if network.Contains(ip) {
				return true
			}
		}
		return false
	}
	if !trusted(peer) {
		return peer
	}
	forwarded := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(forwarded) - 1; i >= 0; i-- {
		hop := strings.TrimSpace(forwarded[i])
		ip := net.ParseIP(hop)
		if ip == nil {
			return peer
		}
		peer = ip.String()
		if !trusted(peer) {
			return peer
		}
	}
	return peer
}

// Rate limits do not trust X-Forwarded-For; configure the reverse proxy to
// preserve a safe source address instead of accepting a client-supplied IP.
func (a *App) allow(key string, maximum int, window time.Duration) bool {
	a.rateMu.Lock()
	defer a.rateMu.Unlock()
	now, cutoff := time.Now().UnixMilli(), time.Now().Add(-window).UnixMilli()
	if len(a.rates) > 10000 {
		for k, events := range a.rates {
			if len(events) == 0 || events[len(events)-1] <= now-int64(time.Hour/time.Millisecond) {
				delete(a.rates, k)
			}
		}
		if len(a.rates) > 20000 {
			return false
		}
	}
	valid := a.rates[key][:0]
	for _, at := range a.rates[key] {
		if at > cutoff {
			valid = append(valid, at)
		}
	}
	if len(valid) >= maximum {
		a.rates[key] = valid
		return false
	}
	a.rates[key] = append(valid, now)
	return true
}

func boolSetting(s *State, key string) bool { v, _ := s.Settings[key].(bool); return v }
