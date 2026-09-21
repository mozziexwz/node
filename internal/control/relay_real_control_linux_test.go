//go:build linux

package control

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mozziexwz/node/internal/relayruntime"
)

type realControlObservedResponse struct {
	http.ResponseWriter
	status int
}

func (w *realControlObservedResponse) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

// This uses actual control handlers, transactions, enrollment, command/traffic
// ACKs and the production Run loop. Only the echo target is synthetic. It does
// NOT create a whole-site archive or claim a full disaster-backup cycle.
//
// MSBOOST_REAL_CONTROL_OUTAGE_DURATION is an exact enum: unset/5m (the CI
// default), 30m or 24h. A 30m run means the SAME actual control HTTP listener is
// continuously closed for >=30m; it is different evidence from a 30m sequence
// of simulated/mixed DNS, timeout, 502/503 and network faults. Both keep the
// original data TCP connection, but their fault and duration claims differ.
// Operators must explicitly provide enough -test.timeout (for example 8m,
// 35m or 25h respectively). The fixture never changes/disables Go's deadline,
// and the default CI invocation remains a five-minute listener outage.
func TestRealControlBackupPauseAndOfflineConnection(t *testing.T) {
	if os.Getenv("MSBOOST_REAL_CONTROL_OUTAGE") != "1" {
		t.Skip("opt-in real control/GOST listener outage test; defaults to five minutes")
	}
	outageDuration, err := realControlOutageDuration(os.Getenv("MSBOOST_REAL_CONTROL_OUTAGE_DURATION"))
	if err != nil {
		t.Fatal(err)
	}
	testDeadline, bounded := t.Deadline()
	if err := realControlOutageTimeout(outageDuration, time.Until(testDeadline), bounded); err != nil {
		t.Fatal(err)
	}
	interfaces, err := net.Interfaces()
	if os.Geteuid() != 0 || os.Getenv("MSBOOST_TEST_LOOPBACK_NETNS") != "1" || err != nil || len(interfaces) != 1 || interfaces[0].Name != "lo" {
		t.Fatal("requires a new root loopback-only network namespace")
	}
	gost := os.Getenv("GOST_TEST_BINARY")
	if !filepath.IsAbs(gost) {
		t.Fatal("verified absolute GOST binary required")
	}
	version, err := exec.Command(gost, "-V").CombinedOutput()
	if err != nil || !strings.Contains(string(version), "3.3.0") {
		t.Fatal("verified GOST 3.3.0 required")
	}
	dir, err := os.MkdirTemp("", "rc-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	echo, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var echoes sync.WaitGroup
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			conn, err := echo.Accept()
			if err != nil {
				return
			}
			echoes.Add(1)
			go func() { defer echoes.Done(); defer conn.Close(); _, _ = io.Copy(conn, conn) }()
		}
	}()
	defer func() { echo.Close(); <-acceptDone; echoes.Wait() }()
	portProbe, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := portProbe.Addr().(*net.TCPAddr).Port
	portProbe.Close()
	f := newRelayV2Fixture(t)
	enrollment := commerceID() + commerceID()
	if err := f.app.Store.Update(func(s *State) error {
		DeleteDoc(s, "relay_agents", f.agents[1].ID)
		agent := f.agents[0]
		agent.TokenHash, agent.EnrollmentHash = "", commerceHash(enrollment)
		agent.EnrollmentExpires = time.Now().Add(10 * time.Minute).UnixMilli()
		agent.PortRanges = []PortRange{{Start: port, End: port}}
		if err := SaveDoc(s, "relay_agents", agent.ID, agent); err != nil {
			return err
		}
		if err := SaveDoc(s, "routes", "route", Route{ID: "route", Type: "port_forward", EntryAgentID: agent.ID, Enabled: true, RateMbps: 5}); err != nil {
			return err
		}
		rule, _ := LoadDoc[UserRule](s, "user_rules", f.user.ID+":route")
		rule.Segments = rule.Segments[:1]
		rule.Segments[0].Runtime.ListenPort = port
		rule.Segments[0].Runtime.Targets = []string{echo.Addr().String()}
		return SaveDoc(s, "user_rules", f.user.ID+":route", rule)
	}); err != nil {
		t.Fatal(err)
	}
	var gatedRequests atomic.Int64
	var countingGate atomic.Bool
	realHandler := f.app.Authenticate(f.mux)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed := &realControlObservedResponse{ResponseWriter: w, status: http.StatusOK}
		realHandler.ServeHTTP(observed, r)
		if countingGate.Load() && r.URL.Path == "/api/relay-agent/v2/sync" && observed.status == http.StatusServiceUnavailable {
			gatedRequests.Add(1)
		}
	})
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	defer func() {
		server.Close()
		if err := <-serveDone; !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("real control server exit: %v", err)
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	runtimeDone := make(chan error, 1)
	go func() {
		runtimeDone <- relayruntime.Run(ctx, relayruntime.Config{ServerURL: "http://" + address, EnrollmentToken: enrollment, StateDir: dir, GostBinary: gost, OfflinePolicy: relayruntime.KeepLast})
	}()
	defer func() {
		cancel()
		select {
		case err := <-runtimeDone:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("runtime shutdown: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("runtime did not reap its GOST child")
		}
	}()
	var original net.Conn
	sequence := 0
	pulse := func(conn net.Conn) {
		t.Helper()
		sequence++
		payload := fmt.Sprintf("real-control-sequence-%06d\n", sequence)
		if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(conn, payload); err != nil {
			t.Fatal("original TCP write failed; no reconnect attempted")
		}
		response := make([]byte, len(payload))
		if _, err := io.ReadFull(conn, response); err != nil || string(response) != payload {
			t.Fatal("original TCP sequence/echo failed; no reconnect attempted")
		}
	}
	wait := func(label string, ready func() bool) {
		t.Helper()
		deadline := time.Now().Add(50 * time.Second)
		for time.Now().Before(deadline) {
			if ready() {
				return
			}
			if original != nil {
				pulse(original)
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Fatalf("real control phase timed out: %s", label)
	}
	ready := func() bool {
		var report BackupPauseReport
		if err := f.app.Store.View(func(s *State) error { report = backupPausePreflight(s, time.Now().UnixMilli()); return nil }); err != nil {
			t.Fatal(err)
		}
		return report.CanPauseControl
	}
	wait("actual enrollment, running ACK and backup admission", ready)
	var before RelayV2Command
	if err := f.app.Store.View(func(s *State) error {
		before, _ = LoadDoc[RelayV2Command](s, "relay_v2_commands", f.agents[0].ID+":"+f.ruleID)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if before.AckState != "ready" || before.Action != "upsert" {
		t.Fatal("actual runtime was not confirmed")
	}
	original, err = net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close()
	pulse(original)
	gateToken := strings.Repeat("ab", 32)
	if _, err := backupPauseAcquire(f.app.Store, gateToken, time.Now().UnixMilli()); err != nil {
		t.Fatal("actual strict backup gate refused confirmed running node")
	}
	countingGate.Store(true)
	wait("actual runtime reached frozen control API", func() bool { return gatedRequests.Load() > 0 })
	if err := f.app.Store.Update(func(*State) error { return nil }); !errors.Is(err, ErrBackupPauseActive) {
		t.Fatal("real backup gate failed to freeze writes")
	}
	if _, err := backupPauseRelease(f.app.Store, gateToken); err != nil {
		t.Fatal(err)
	}
	releasedAt := time.Now().UnixMilli()
	var releasedSequence int64
	if err := f.app.Store.View(func(s *State) error {
		catalog, _ := LoadDoc[RelayV2Catalog](s, "relay_v2_catalogs", f.agents[0].ID)
		releasedSequence = catalog.Sequence
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	countingGate.Store(false)
	wait("normal control resumed after matching gate release", func() bool {
		fresh := false
		if err := f.app.Store.View(func(s *State) error {
			catalog, _ := LoadDoc[RelayV2Catalog](s, "relay_v2_catalogs", f.agents[0].ID)
			agent, _ := LoadDoc[RelayAgent](s, "relay_agents", f.agents[0].ID)
			command, _ := LoadDoc[RelayV2Command](s, "relay_v2_commands", f.agents[0].ID+":"+f.ruleID)
			fresh = catalog.Sequence > releasedSequence && agent.LastSeen >= releasedAt && command.AppliedAt >= releasedAt && backupPausePreflight(s, time.Now().UnixMilli()).CanPauseControl
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return fresh
	})
	// Stop the real HTTP control listener, not a mock response. Runtime's data
	// plane remains in the same namespace and keeps the original TCP connection.
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	serverExit := <-serveDone
	// Keep the deferred lifecycle balanced if a subsequent assertion fails.
	serveDone = make(chan error, 1)
	serveDone <- http.ErrServerClosed
	if !errors.Is(serverExit, http.ErrServerClosed) {
		t.Fatal(serverExit)
	}
	t.Logf("REAL_CONTROL_OUTAGE_STARTED duration=%s actual_listener_closed=true fault=continuous_real_http_listener_closure", outageDuration)
	started := time.Now()
	for time.Since(started) < outageDuration {
		pulse(original)
		time.Sleep(time.Second)
	}
	during, err := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
	if err != nil {
		t.Fatal("same authorized rule refused a new connection during real control outage")
	}
	pulse(during)
	during.Close()
	pulse(original)
	listener, err = net.Listen("tcp4", address)
	if err != nil {
		t.Fatal(err)
	}
	listenerRestoredAt := time.Now()
	listenerOutage := listenerRestoredAt.Sub(started)
	server = &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	serveDone = make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	wait("same-origin real control confirms original runtime again", ready)
	pulse(original)
	if err := f.app.Store.View(func(s *State) error {
		after, _ := LoadDoc[RelayV2Command](s, "relay_v2_commands", f.agents[0].ID+":"+f.ruleID)
		if !backupPauseSameIntent(before, after) || after.AckState != "ready" || after.AppliedAt <= started.UnixMilli() {
			t.Fatal("real reconnect changed command or failed to produce a fresh ACK")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	t.Logf("REAL_CONTROL_KEEP_LAST_PASS actual_gate=true actual_http_listener_outage=%s recovery_to_fresh_ack=%s original_dials=1 original_reconnects=0 new_connection_during_outage=true command_unchanged=true full_archive_cycle=false samples=%d requested_listener_outage=%s fault=continuous_real_http_listener_closure", listenerOutage.Round(time.Millisecond), time.Since(listenerRestoredAt).Round(time.Millisecond), sequence, outageDuration)
}

func realControlOutageDuration(value string) (time.Duration, error) {
	switch value {
	case "", "5m":
		return 5 * time.Minute, nil
	case "30m":
		return 30 * time.Minute, nil
	case "24h":
		return 24 * time.Hour, nil
	default:
		return 0, errors.New("MSBOOST_REAL_CONTROL_OUTAGE_DURATION must be exactly 5m, 30m or 24h (unset defaults to 5m)")
	}
}

func realControlOutageTimeout(duration, remaining time.Duration, bounded bool) error {
	// This is only an early refusal of clearly insufficient/unbounded runs;
	// setup, recovery and cleanup still obey the test runner's actual deadline.
	// No environment variable or helper can extend it. The existing 8m CI
	// timeout has room for the default 5m outage and this 2m minimum reserve.
	if !bounded || remaining < duration+2*time.Minute {
		return errors.New("set an explicit positive -test.timeout with at least the selected outage plus 2m remaining; recommended 8m/35m/25h for 5m/30m/24h; the fixture does not change Go's timeout")
	}
	return nil
}

func TestRealControlOutageDuration(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{{"", 5 * time.Minute}, {"5m", 5 * time.Minute}, {"30m", 30 * time.Minute}, {"24h", 24 * time.Hour}} {
		t.Run("valid_"+tc.value, func(t *testing.T) {
			actual, err := realControlOutageDuration(tc.value)
			if err != nil || actual != tc.want {
				t.Fatalf("duration enum mismatch: got %s, want %s, err=%v", actual, tc.want, err)
			}
		})
	}
	for _, value := range []string{"0", "-5m", "1m", "300s", "5m0s", "30M", " 30m", "30m ", "30m\n", "1440m", "25h", "1h", "24h0m", "24h;echo", "1e9h"} {
		t.Run("invalid_"+value, func(t *testing.T) {
			if _, err := realControlOutageDuration(value); err == nil {
				t.Fatal("non-enumerated outage duration accepted")
			}
		})
	}
}

func TestRealControlOutageTimeout(t *testing.T) {
	for _, tc := range []struct {
		name      string
		duration  time.Duration
		remaining time.Duration
		bounded   bool
		accepted  bool
	}{
		{"existing_ci_5m", 5 * time.Minute, 8 * time.Minute, true, true},
		{"explicit_30m", 30 * time.Minute, 35 * time.Minute, true, true},
		{"explicit_24h", 24 * time.Hour, 25 * time.Hour, true, true},
		{"exact_reserve", 5 * time.Minute, 7 * time.Minute, true, true},
		{"one_tick_short", 5 * time.Minute, 7*time.Minute - time.Nanosecond, true, false},
		{"default_go_timeout_not_enough_for_30m", 30 * time.Minute, 10 * time.Minute, true, false},
		{"default_go_timeout_not_enough_for_24h", 24 * time.Hour, 10 * time.Minute, true, false},
		{"disabled_timeout", 24 * time.Hour, 0, false, false},
		{"missing_deadline_even_with_duration", 5 * time.Minute, time.Hour, false, false},
		{"elapsed_deadline", 5 * time.Minute, -time.Second, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := realControlOutageTimeout(tc.duration, tc.remaining, tc.bounded); (err == nil) != tc.accepted {
				t.Fatalf("deadline guard accepted=%v, want %v", err == nil, tc.accepted)
			}
		})
	}
}
