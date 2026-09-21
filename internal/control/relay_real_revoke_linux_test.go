//go:build linux

package control

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/mozziexwz/node/internal/relayruntime"
)

// Opt-in T12/T14 lifecycle proof with a real HTTP control backend, member
// session/CSRF, production Run/GOST and the actual periodic cleanup worker.
// One real stopped-ACK request is lost before reaching the backend; further
// such requests are held with 503 until AFTER the 45s legacy deadline. Then a
// real stopped ACK is committed but its HTTP response is lost. No ACK is
// fabricated, and no production deadline is shortened or advanced.
// Run only in a new root loopback-only namespace, with -test.timeout=4m.
func TestRealControlMemberRevokeWithLostStopACK(t *testing.T) {
	if os.Getenv("MSBOOST_REAL_CONTROL_REVOKE") != "1" {
		t.Skip("opt-in real member revoke/lost stopped ACK/GOST isolation test")
	}
	interfaces, err := net.Interfaces()
	if os.Geteuid() != 0 || os.Getenv("MSBOOST_TEST_LOOPBACK_NETNS") != "1" || err != nil || len(interfaces) != 1 || interfaces[0].Name != "lo" {
		t.Fatal("requires a new root loopback-only network namespace")
	}
	if deadline, bounded := t.Deadline(); !bounded || time.Until(deadline) < 3*time.Minute {
		t.Fatal("provide an explicit bounded -test.timeout=4m or longer")
	}
	gost := os.Getenv("GOST_TEST_BINARY")
	if !filepath.IsAbs(gost) {
		t.Fatal("verified absolute GOST binary required")
	}
	file, err := os.Open(gost)
	if err != nil {
		t.Fatal("cannot open pinned GOST")
	}
	hash := sha256.New()
	_, err = io.Copy(hash, file)
	file.Close()
	if err != nil || hex.EncodeToString(hash.Sum(nil)) != "1d8f971e9447cf4114fb1376b85c8c14e840db50ae8dbe895398a34e97c13e08" {
		t.Fatal("requires the pinned Linux amd64 GOST 3.3.0 validation binary")
	}
	dir, err := os.MkdirTemp("", "rr-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	echo, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	probeA, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	probeB, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		probeA.Close()
		t.Fatal(err)
	}
	portA, portB := probeA.Addr().(*net.TCPAddr).Port, probeB.Addr().(*net.TCPAddr).Port
	probeA.Close()
	probeB.Close()
	f := newRelayV2Fixture(t)
	secondRuleID, secondUserID := commerceID(), "unaffected-buyer"
	enrollment, session, csrf := commerceID()+commerceID(), commerceID()+commerceID(), commerceID()+commerceID()
	if err := f.app.Store.Update(func(s *State) error {
		DeleteDoc(s, "relay_agents", f.agents[1].ID)
		a := f.agents[0]
		a.TokenHash, a.EnrollmentHash = "", commerceHash(enrollment)
		a.EnrollmentExpires = time.Now().Add(10 * time.Minute).UnixMilli()
		a.PortRanges = []PortRange{{Start: portA, End: portA}, {Start: portB, End: portB}}
		if err := SaveDoc(s, "relay_agents", a.ID, a); err != nil {
			return err
		}
		for _, id := range []string{"route", "unaffected"} {
			if err := SaveDoc(s, "routes", id, Route{ID: id, Type: "port_forward", EntryAgentID: a.ID, Enabled: true, RateMbps: 5}); err != nil {
				return err
			}
		}
		rule, _ := LoadDoc[UserRule](s, "user_rules", f.user.ID+":route")
		rule.Segments = rule.Segments[:1]
		rule.EntryPort, rule.TargetHash = portA, "old-target-lock"
		rule.Segments[0].Runtime.ListenPort = portA
		rule.Segments[0].Runtime.Targets = []string{echo.Addr().String()}
		if err := SaveDoc(s, "user_rules", f.user.ID+":route", rule); err != nil {
			return err
		}
		other := *s.Users[f.user.ID]
		other.ID = secondUserID
		other.Email = "unaffected@example.com"
		s.Users[other.ID] = &other
		rule.ID, rule.UserID, rule.RouteID, rule.EntryPort, rule.TargetHash = secondRuleID, other.ID, "unaffected", portB, "unaffected-target"
		rule.Segments = append([]RelaySegment(nil), rule.Segments...)
		rule.Segments[0].Runtime.ID, rule.Segments[0].Runtime.ListenPort = secondRuleID, portB
		if err := SaveDoc(s, "user_rules", other.ID+":unaffected", rule); err != nil {
			return err
		}
		if err := SaveDoc(s, "user_targets", f.user.ID, UserTarget{Hash: "old-target-lock", Host: "127.0.0.1", Port: echo.Addr().(*net.TCPAddr).Port}); err != nil {
			return err
		}
		s.Sessions[tokenHash(session)] = &Session{UserID: f.user.ID, CSRFToken: csrf, ExpiresAt: time.Now().Add(10 * time.Minute).UnixMilli()}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var holdStopped atomic.Bool
	holdStopped.Store(true)
	var lostBefore atomic.Int64
	var lossAfterClaimed atomic.Bool
	var committedLossAt atomic.Int64
	var successfulRetryAt atomic.Int64
	var syncTimingMu sync.Mutex
	var previousSyncAt time.Time
	startedAt := time.Now()
	firstStopped := make(chan relayruntime.V2Ack, 1)
	responseLost := make(chan struct{}, 1)
	errorsSeen := make(chan string, 4)
	realHandler := f.app.Authenticate(f.mux)
	closeWithoutResponse := func(w http.ResponseWriter) {
		h, ok := w.(http.Hijacker)
		if !ok {
			select {
			case errorsSeen <- "real HTTP writer cannot drop a response":
			default:
			}
			return
		}
		c, _, err := h.Hijack()
		if err != nil {
			select {
			case errorsSeen <- "HTTP response drop failed":
			default:
			}
			return
		}
		_ = c.Close()
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var stopped *relayruntime.V2Ack
		isSync := r.URL.Path == "/api/relay-agent/v2/sync"
		backendStatus, deliveredStatus, responseClass := 0, 0, "not_called"
		sequence, revision, ackCount, trafficCount := int64(0), int64(0), 0, 0
		if isSync {
			now := time.Now()
			syncTimingMu.Lock()
			interval := time.Duration(0)
			if !previousSyncAt.IsZero() {
				interval = now.Sub(previousSyncAt)
			}
			previousSyncAt = now
			syncTimingMu.Unlock()
			defer func() {
				// Fixed classes and numeric protocol metadata only: never log the
				// request/response body, identities, targets, credentials or errors.
				t.Logf("REAL_REVOKE_SYNC elapsed=%s interval=%s sequence=%d revision=%d ack_count=%d traffic_count=%d stopped_ack=%t backend_status=%d delivered_status=%d class=%s", time.Since(startedAt).Round(time.Millisecond), interval.Round(time.Millisecond), sequence, revision, ackCount, trafficCount, stopped != nil, backendStatus, deliveredStatus, responseClass)
			}()
		}
		if isSync {
			raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			_ = r.Body.Close()
			var in relayruntime.V2SyncRequest
			if err != nil || json.Unmarshal(raw, &in) != nil {
				select {
				case errorsSeen <- "cannot inspect actual bounded runtime request":
				default:
				}
				deliveredStatus, responseClass = 500, "fixture_request_decode_failed"
				w.WriteHeader(deliveredStatus)
				return
			}
			sequence, revision, ackCount, trafficCount = in.Sequence, in.AppliedRevision, len(in.Acks), len(in.Traffic)
			r.Body = io.NopCloser(bytes.NewReader(raw))
			for i := range in.Acks {
				if in.Acks[i].RuleID == f.ruleID && in.Acks[i].State == "stopped" {
					ack := in.Acks[i]
					stopped = &ack
					break
				}
			}
		}
		if stopped != nil && holdStopped.Load() {
			select {
			case firstStopped <- *stopped:
			default:
			}
			if lostBefore.Add(1) == 1 {
				responseClass = "dropped_before_backend"
				closeWithoutResponse(w)
			} else {
				deliveredStatus, responseClass = http.StatusServiceUnavailable, "held_before_backend"
				w.WriteHeader(http.StatusServiceUnavailable)
			}
			return // The real backend has not received this actual stopped proof.
		}
		if stopped != nil && lossAfterClaimed.CompareAndSwap(false, true) {
			recorded := httptest.NewRecorder()
			realHandler.ServeHTTP(recorded, r) // Real transaction and real response.
			backendStatus, responseClass = recorded.Code, "dropped_after_backend"
			if recorded.Code != http.StatusOK {
				select {
				case errorsSeen <- "real stopped ACK did not commit successfully":
				default:
				}
			}
			committedLossAt.Store(time.Now().UnixNano())
			closeWithoutResponse(w)
			responseLost <- struct{}{}
			return
		}
		if isSync {
			recorded := httptest.NewRecorder()
			realHandler.ServeHTTP(recorded, r)
			backendStatus, deliveredStatus = recorded.Code, recorded.Code
			responseClass = "backend_non_success"
			if recorded.Code == http.StatusOK {
				var response struct {
					Status string `json:"status"`
				}
				responseClass = "backend_success_other"
				if json.Unmarshal(recorded.Body.Bytes(), &response) == nil {
					switch response.Status {
					case "ready":
						responseClass = "backend_ready"
						if committedLossAt.Load() > 0 {
							successfulRetryAt.CompareAndSwap(0, time.Now().UnixNano())
						}
					case "recovery_required":
						responseClass = "backend_recovery_required"
					}
				}
			}
			for name, values := range recorded.Header() {
				for _, value := range values {
					w.Header().Add(name, value)
				}
			}
			w.WriteHeader(recorded.Code)
			_, _ = w.Write(recorded.Body.Bytes())
			return
		}
		realHandler.ServeHTTP(w, r)
	})
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := "http://" + listener.Addr().String()
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	go server.Serve(listener)
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	f.app.StartRelay(ctx)
	runtimeDone := make(chan error, 1)
	go func() {
		runtimeDone <- relayruntime.Run(ctx, relayruntime.Config{ServerURL: address, EnrollmentToken: enrollment, StateDir: dir, GostBinary: gost, OfflinePolicy: relayruntime.KeepLast})
	}()
	defer func() {
		cancel()
		select {
		case err := <-runtimeDone:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Error("runtime shutdown failed")
			}
		case <-time.After(5 * time.Second):
			t.Error("runtime did not reap children")
		}
	}()
	var unaffected net.Conn
	samples := 0
	pulse := func() {
		t.Helper()
		select {
		case err := <-errorsSeen:
			t.Fatal(err)
		default:
		}
		if unaffected == nil {
			return
		}
		samples++
		payload := fmt.Sprintf("unaffected-original-%08d\n", samples)
		_ = unaffected.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := io.WriteString(unaffected, payload); err != nil {
			t.Fatal("unaffected original TCP write failed; no reconnect")
		}
		response := make([]byte, len(payload))
		if _, err := io.ReadFull(unaffected, response); err != nil || string(response) != payload {
			t.Fatal("unaffected original TCP echo failed; no reconnect")
		}
	}
	wait := func(label string, limit time.Duration, condition func() bool) {
		t.Helper()
		deadline := time.Now().Add(limit)
		for time.Now().Before(deadline) {
			pulse()
			if condition() {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("real revoke phase timed out: %s", label)
	}
	var unaffectedBefore RelayV2Command
	wait("two actual GOST rules confirmed", 60*time.Second, func() bool {
		ready := false
		if err := f.app.Store.View(func(s *State) error {
			a, _ := LoadDoc[RelayV2Command](s, "relay_v2_commands", f.agents[0].ID+":"+f.ruleID)
			b, _ := LoadDoc[RelayV2Command](s, "relay_v2_commands", f.agents[0].ID+":"+secondRuleID)
			ready = a.AckState == "ready" && b.AckState == "ready" && backupPausePreflight(s, time.Now().UnixMilli()).CanPauseControl
			unaffectedBefore = b
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return ready
	})
	unaffected, err = net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", portB), time.Second)
	if err != nil {
		t.Fatal("unaffected initial dial failed")
	}
	defer unaffected.Close()
	pulse()
	deleted, err := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", portA), time.Second)
	if err != nil {
		t.Fatal("deleted rule initial dial failed")
	}
	defer deleted.Close()
	_ = deleted.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err = deleted.Write([]byte("before-delete")); err != nil {
		t.Fatal(err)
	}
	echoed := make([]byte, len("before-delete"))
	if _, err = io.ReadFull(deleted, echoed); err != nil || string(echoed) != "before-delete" {
		t.Fatal("deleted rule did not initially forward")
	}
	client := &http.Client{Timeout: 5 * time.Second}
	memberCall := func(method, path string, body []byte) {
		t.Helper()
		r, err := http.NewRequest(method, address+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.AddCookie(&http.Cookie{Name: "msboost_session", Value: session})
		r.Header.Set("X-CSRF-Token", csrf)
		r.Header.Set("Origin", f.app.Config.PublicURL)
		r.Header.Set("Content-Type", "application/json")
		response, err := client.Do(r)
		if err != nil {
			t.Fatal("real member HTTP operation failed")
		}
		defer response.Body.Close()
		var report struct {
			State      string `json:"state"`
			StopStatus string `json:"stopStatus"`
		}
		if response.StatusCode != http.StatusAccepted || json.NewDecoder(response.Body).Decode(&report) != nil || report.State != "revoking" || report.StopStatus != "pending" {
			t.Fatal("member operation did not explicitly report pending revoke")
		}
	}
	memberCall(http.MethodDelete, "/api/user/routes/route/rules", nil)
	memberCall(http.MethodPost, "/api/user/target/reset", []byte(`{"confirm":true}`))
	var observed relayruntime.V2Ack
	wait("production Run emits an actual stopped ACK", 30*time.Second, func() bool {
		select {
		case observed = <-firstStopped:
			return true
		default:
			return false
		}
	})
	assertRefused := func() {
		t.Helper()
		c, err := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", portA), time.Second)
		if c != nil {
			c.Close()
		}
		if !errors.Is(err, syscall.ECONNREFUSED) {
			t.Fatal("revoked rule must explicitly refuse new TCP, not timeout or route elsewhere")
		}
	}
	assertRefused()
	_ = deleted.SetReadDeadline(time.Now().Add(3 * time.Second))
	var one [1]byte
	if _, err := deleted.Read(one[:]); err == nil || !(errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET)) {
		t.Fatal("revocation did not close the original deleted-rule TCP")
	}
	var releaseDeadline int64
	assertReserved := func() {
		t.Helper()
		if err := f.app.Store.View(func(s *State) error {
			rule, ok := LoadDoc[UserRule](s, "user_rules", f.user.ID+":route")
			target, targetOK := LoadDoc[UserTarget](s, "user_targets", f.user.ID)
			command, _ := LoadDoc[RelayV2Command](s, "relay_v2_commands", f.agents[0].ID+":"+f.ruleID)
			if !ok || rule.State != "revoking" || !targetOK || target.Hash != "old-target-lock" || target.Host != "127.0.0.1" || target.Port != echo.Addr().(*net.TCPAddr).Port || command.Action != "revoke" || command.AckState == "stopped" || len(rule.Segments) != 1 || rule.Segments[0].StopConfirmed || rule.TargetHash != "old-target-lock" || len(rule.Segments[0].Runtime.Targets) != 1 || rule.Segments[0].Runtime.Targets[0] != echo.Addr().String() {
				return errors.New("unreceived exact stop proof released or altered protected resources")
			}
			if command.CommandID != observed.CommandID || command.Generation != observed.Generation || command.RuntimeHash != observed.RuntimeHash {
				return errors.New("real stopped ACK did not bind the current revoke")
			}
			if rule.DeleteAfter > releaseDeadline {
				releaseDeadline = rule.DeleteAfter
			}
			if target.ResetUntil > releaseDeadline {
				releaseDeadline = target.ResetUntil
			}
			for _, port := range []int{portA, portB} {
				a := f.agents[0]
				a.PortRanges = []PortRange{{Start: port, End: port}}
				if _, err := relayReservePort(s, a); err == nil {
					return errors.New("occupied or unconfirmed port was reusable")
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	assertReserved()
	// Cross the actual deadline plus a complete real 5s cleanup-worker tick.
	for time.Now().UnixMilli() < releaseDeadline+6000 {
		pulse()
		assertReserved()
		time.Sleep(100 * time.Millisecond)
	}
	assertReserved()
	assertRefused()
	if lostBefore.Load() < 1 {
		t.Fatal("no real stopped ACK was lost before commit")
	}
	beforeCommit := time.Now().UnixMilli()
	holdStopped.Store(false)
	// The long intentional 503 period reaches the production management
	// backoff cap: 30s plus <500ms jitter, then an HTTP timeout up to 10s.
	// Observe a complete permitted attempt; do not shorten production timing.
	const managementRetryWindow = 45 * time.Second
	wait("real stopped ACK commits but one response is lost", managementRetryWindow, func() bool {
		select {
		case <-responseLost:
			return true
		default:
			return false
		}
	})
	wait("exact archive and resource release after real stopped proof", 30*time.Second, func() bool {
		released := false
		if err := f.app.Store.View(func(s *State) error {
			_, active := LoadDoc[UserRule](s, "user_rules", f.user.ID+":route")
			_, locked := LoadDoc[UserTarget](s, "user_targets", f.user.ID)
			archive, archived := LoadDoc[UserRule](s, "relay_rule_archive", f.ruleID)
			command, _ := LoadDoc[RelayV2Command](s, "relay_v2_commands", f.agents[0].ID+":"+f.ruleID)
			other, _ := LoadDoc[RelayV2Command](s, "relay_v2_commands", f.agents[0].ID+":"+secondRuleID)
			released = !active && !locked && archived && archive.State == "revoked" && len(archive.Segments) == 1 && relayRuleRevoked(archive, time.Now().UnixMilli()) && archive.Segments[0].LastCommandID == observed.CommandID && archive.Segments[0].ConfigGeneration == observed.Generation && command.Action == "revoke" && command.AckState == "stopped" && command.AppliedAt >= beforeCommit && command.CommandID == observed.CommandID && command.Generation == observed.Generation && command.RuntimeHash == observed.RuntimeHash && backupPauseSameIntent(unaffectedBefore, other) && other.AckState == "ready"
			if released {
				a := f.agents[0]
				a.PortRanges = []PortRange{{Start: portA, End: portA}}
				p, err := relayReservePort(s, a)
				if err != nil || p != portA {
					return errors.New("exact stopped port not released")
				}
				a.PortRanges = []PortRange{{Start: portB, End: portB}}
				if _, err := relayReservePort(s, a); err == nil {
					return errors.New("unaffected port released")
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return released
	})
	// Give Run a real successful retry after the deliberately lost response.
	var sequenceAfterLoss int64
	_ = f.app.Store.View(func(s *State) error {
		c, _ := LoadDoc[RelayV2Catalog](s, "relay_v2_catalogs", f.agents[0].ID)
		sequenceAfterLoss = c.Sequence
		return nil
	})
	var finalSequence int64
	wait("runtime retries after lost committed response", managementRetryWindow, func() bool {
		fresh := false
		_ = f.app.Store.View(func(s *State) error {
			c, _ := LoadDoc[RelayV2Catalog](s, "relay_v2_catalogs", f.agents[0].ID)
			finalSequence = c.Sequence
			fresh = c.Sequence > sequenceAfterLoss && successfulRetryAt.Load() > committedLossAt.Load()
			return nil
		})
		return fresh
	})
	assertRefused()
	pulse()
	t.Logf("REAL_REVOKE_RETRY committed_sequence=%d fresh_sequence=%d actual_retry_interval=%s observation_window=%s", sequenceAfterLoss, finalSequence, time.Duration(successfulRetryAt.Load()-committedLossAt.Load()).Round(time.Millisecond), managementRetryWindow)
	t.Logf("REAL_CONTROL_MEMBER_REVOKE_PASS real_member_delete=true real_target_reset=true stop_ack_generated_by_run=true lost_before_commit=%d committed_response_lost=1 resources_retained_past_legacy_deadline=true exact_stopped_archive=true exact_port_and_target_released=true deleted_new_tcp=ECONNREFUSED deleted_original_tcp_closed=true unaffected_original_dials=1 unaffected_original_reconnects=0 unaffected_samples=%d", lostBefore.Load(), samples)
}
