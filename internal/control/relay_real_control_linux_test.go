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
func TestRealControlBackupPauseAndOfflineConnection(t *testing.T) {
	if os.Getenv("MSBOOST_REAL_CONTROL_OUTAGE") != "1" {
		t.Skip("opt-in five-minute real control/GOST outage test")
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
	t.Log("REAL_CONTROL_OUTAGE_STARTED duration=5m actual_listener_closed=true")
	started := time.Now()
	for time.Since(started) < 5*time.Minute {
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
	t.Logf("REAL_CONTROL_KEEP_LAST_PASS actual_gate=true actual_http_listener_outage=%s original_dials=1 original_reconnects=0 new_connection_during_outage=true command_unchanged=true full_archive_cycle=false samples=%d", time.Since(started).Round(time.Millisecond), sequence)
}
