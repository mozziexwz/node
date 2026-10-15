package control

import (
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Agent endpoints accept bearer and enrollment tokens from the internet. Every
// store transaction is serialized, so a flood of invalid tokens must not reach
// it. This index of token hashes rejects unknown credentials from memory; a
// known hash is only a pre-check, and each handler still verifies the token
// inside its own transaction, so a stale entry never authorizes anything.
const (
	// Known entries are trusted for pre-checking for at most this long.
	credentialIndexMaxAge = time.Minute
	// A miss rebuilds the index after this interval even without a local write,
	// so tokens written by another process are accepted within seconds.
	credentialMissRefresh = 5 * time.Second
	// Failed credentials per source before further unknown tokens get 429.
	// Valid tokens are never limited by this counter.
	agentAuthFailureLimit  = 30
	agentAuthFailureWindow = 10 * time.Minute
)

var (
	errAgentAuthLimited     = errors.New("鉴权失败次数过多，请稍后重试")
	errAgentAuthUnavailable = errors.New("控制面暂不可用，请稍后重试")
)

type credentialIndex struct {
	relay      map[string]string // relay token hash -> agent ID
	enrollment map[string]int64  // enrollment or retry hash -> latest expiry
	executor   map[string]string // executor token hash -> status
}

type credentialCache struct {
	mu       sync.Mutex
	index    *credentialIndex
	builtAt  time.Time
	revision int64
}

// bearerToken accepts the same header shape as the authoritative checks.
func bearerToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	token := strings.TrimPrefix(header, "Bearer ")
	return token, token != header && token != "" && len(token) <= 200
}

func (a *App) rebuildCredentialsLocked() error {
	c := &a.credentials
	revision := a.Store.Writes()
	index := &credentialIndex{relay: map[string]string{}, enrollment: map[string]int64{}, executor: map[string]string{}}
	err := a.Store.View(func(s *State) error {
		for _, agent := range ListDocs[RelayAgent](s, "relay_agents") {
			if _, retired := s.Docs[relayRetirementCollection][agent.ID]; retired {
				continue
			}
			if agent.TokenHash != "" && agent.Capability == "relay" && !agent.Archived {
				index.relay[agent.TokenHash] = agent.ID
			}
			if agent.EnrollmentHash != "" {
				index.enrollment[agent.EnrollmentHash] = max(index.enrollment[agent.EnrollmentHash], agent.EnrollmentExpires)
			}
			if agent.EnrollmentRetryHash != "" {
				index.enrollment[agent.EnrollmentRetryHash] = max(index.enrollment[agent.EnrollmentRetryHash], agent.EnrollmentRetryUntil)
			}
		}
		for _, record := range ListDocs[ExecutorRecord](s, "executors") {
			if record.TokenHash != "" {
				index.executor[record.TokenHash] = record.Status
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	c.index, c.builtAt, c.revision = index, time.Now(), revision
	return nil
}

// agentCredential answers from memory whenever it can. It rebuilds at most
// once per local write or refresh interval, never once per unknown token, and
// a source that keeps failing may only force a rebuild of an expired index.
func (a *App) agentCredential(r *http.Request, match func(*credentialIndex) bool) error {
	c := &a.credentials
	c.mu.Lock()
	defer c.mu.Unlock()
	age := time.Since(c.builtAt)
	current := c.index != nil && c.revision == a.Store.Writes()
	if c.index != nil && (current || age < credentialIndexMaxAge) && match(c.index) {
		return nil
	}
	failures := "agent-auth-fail:" + a.rateIP(r)
	expired := c.index == nil || age >= credentialIndexMaxAge
	if a.limited(failures, agentAuthFailureLimit, agentAuthFailureWindow) && !expired {
		return errAgentAuthLimited
	}
	if expired || !current || age >= credentialMissRefresh {
		if err := a.rebuildCredentialsLocked(); err != nil {
			return errAgentAuthUnavailable
		}
		if match(c.index) {
			return nil
		}
	}
	a.record(failures)
	return errors.New("unauthorized")
}

// agentAuthFailed counts a credential that passed the memory pre-check but
// failed the authoritative check, for example a token revoked moments ago.
func (a *App) agentAuthFailed(r *http.Request) {
	a.record("agent-auth-fail:" + a.rateIP(r))
}

// agentAuthStatus maps a pre-check error to the response each agent expects:
// 429 and 503 are retried, 401 means the credential itself was rejected.
func agentAuthStatus(err error) int {
	switch {
	case errors.Is(err, errAgentAuthLimited):
		return http.StatusTooManyRequests
	case errors.Is(err, errAgentAuthUnavailable):
		return http.StatusServiceUnavailable
	default:
		return http.StatusUnauthorized
	}
}

func agentAuthFail(w http.ResponseWriter, err error, unauthorized string) {
	status := agentAuthStatus(err)
	switch status {
	case http.StatusTooManyRequests:
		w.Header().Set("Retry-After", "60")
		Fail(w, status, errAgentAuthLimited.Error())
	case http.StatusServiceUnavailable:
		w.Header().Set("Retry-After", "5")
		Fail(w, status, errAgentAuthUnavailable.Error())
	default:
		Fail(w, status, unauthorized)
	}
}
