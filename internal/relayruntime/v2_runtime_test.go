package relayruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// This child exercises real TCP/process lifetimes on loopback, not GOST's
// protocol implementation. Production GOST/TLS endurance remains a separate
// integration gate. No test listener binds a public interface.
func TestMain(m *testing.M) {
	if len(os.Args) == 3 && os.Args[1] == "-C" {
		os.Exit(runV2TCPHelper(os.Args[2]))
	}
	os.Exit(m.Run())
}

func runV2TCPHelper(path string) int {
	var config struct {
		Services []struct {
			Name      string `json:"name"`
			Addr      string `json:"addr"`
			Forwarder struct {
				Nodes []struct {
					Addr string `json:"addr"`
				} `json:"nodes"`
			} `json:"forwarder"`
		} `json:"services"`
		Observers []struct {
			Plugin struct {
				Addr string `json:"addr"`
			} `json:"plugin"`
		} `json:"observers"`
	}
	raw, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(raw, &config) != nil || len(config.Services) != 1 || len(config.Services[0].Forwarder.Nodes) != 1 || len(config.Observers) != 1 {
		return 2
	}
	service := config.Services[0]
	_, port, err := net.SplitHostPort(service.Addr)
	if err != nil {
		return 2
	}
	listen, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		return 2
	}
	defer listen.Close()
	body, _ := json.Marshal(map[string]any{"events": []any{map[string]any{"kind": "service", "service": service.Name, "type": "status", "status": map[string]string{"state": "running"}}}})
	client := &http.Client{Timeout: time.Second}
	res, err := client.Post(config.Observers[0].Plugin.Addr, "application/json", bytes.NewReader(body))
	if err != nil {
		return 2
	}
	res.Body.Close()
	for {
		in, err := listen.Accept()
		if err != nil {
			return 0
		}
		go func() {
			defer in.Close()
			out, err := net.DialTimeout("tcp", service.Forwarder.Nodes[0].Addr, time.Second)
			if err != nil {
				return
			}
			defer out.Close()
			done := make(chan struct{})
			go func() { _, _ = io.Copy(out, in); close(done) }()
			_, _ = io.Copy(in, out)
			_ = in.Close()
			_ = out.Close()
			<-done
		}()
	}
}

func v2TestFixture(t *testing.T) (*runtimeState, context.Context, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{ServerURL: "https://control.example.test", StateDir: dir, GostBinary: binary, OfflinePolicy: KeepLast}
	disk, err := loadV2State(cfg, "agent-test-v2")
	if err != nil {
		t.Fatal(err)
	}
	s := &runtimeState{cfg: cfg, processes: map[string]*process{}, pending: map[string]Traffic{}, observerToken: "test-observer", v2: &v2RuntimeState{disk: disk, path: filepath.Join(dir, v2StateFile), prepared: map[string]v2Prepared{}, retryAt: map[string]time.Time{}}}
	if err = s.initV2TrafficLocked(); err != nil {
		t.Fatal(err)
	}
	observer := httptest.NewServer(httpHandler(s))
	s.observerURL = observer.URL + "/observer?token=test-observer"
	echo, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { defer conn.Close(); _, _ = io.Copy(conn, conn) }()
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		s.mu.Lock()
		processes := []*process{}
		for id, p := range s.processes {
			s.stopLocked(id)
			processes = append(processes, p)
		}
		for _, p := range s.v2.prepared {
			p.process.cleanup()
		}
		s.mu.Unlock()
		for _, p := range processes {
			select {
			case <-p.done:
			case <-time.After(3 * time.Second):
				t.Error("child did not exit")
			}
		}
		observer.Close()
		echo.Close()
	})
	return s, ctx, echo.Addr().String()
}

func v2TestCommand(t *testing.T, id, target string) V2Command {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	rule := Rule{ID: id, Version: 1, ListenPort: port, Targets: []string{target}, AllowedSources: []string{"127.0.0.1"}, Protocol: "tcp", Strategy: "round", RateMbps: 5}
	hash, err := RuntimeHash(rule)
	if err != nil {
		t.Fatal(err)
	}
	return V2Command{CommandID: id + "-command-1", RuleID: id, Generation: 1, Action: "upsert", RuntimeHash: hash, Rule: &rule}
}

func v2TestResponse(s *runtimeState, commands []V2Command) (V2SyncRequest, V2SyncResponse) {
	request := s.v2Request("test-instance")
	return request, V2SyncResponse{ProtocolVersion: ProtocolV2, AgentID: request.AgentID, RequestID: request.RequestID, ControlEpoch: "control-epoch-v2", PreviousRevision: request.AppliedRevision, Revision: request.AppliedRevision + 1, Status: "ready", OfflinePolicy: KeepLast, Commands: commands, TrafficAcks: []V2TrafficAck{}}
}

func TestV2RequestAdvertisesGuardWithoutChangingRequiredBaseline(t *testing.T) {
	s, _, _ := v2TestFixture(t)
	request := s.v2Request("test-instance")
	if len(request.Capabilities) != len(V2Capabilities)+1 || request.Capabilities[len(request.Capabilities)-1] != SocksGuardCapability {
		t.Fatalf("new agent did not advertise optional guard: %v", request.Capabilities)
	}
	for i, required := range V2Capabilities {
		if request.Capabilities[i] != required {
			t.Fatalf("required v2 capability %s was displaced", required)
		}
	}
}

func v2WaitState(t *testing.T, s *runtimeState, ctx context.Context, id, state string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		err := s.reconcileV2Locked(ctx)
		got := s.v2.disk.Records[id].State
		if state == "ready" && (s.processes[id] == nil || s.processes[id].ack.State != "ready") {
			got = "persisted"
		}
		s.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		if got == state {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("rule did not become %s", state)
}

func v2Connect(t *testing.T, command V2Command) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(command.Rule.ListenPort)), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func v2Echo(t *testing.T, conn net.Conn, sequence int) {
	t.Helper()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	payload := []byte(fmt.Sprintf("same-connection-sequence-%08d", sequence))
	if _, err := conn.Write(payload); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("original TCP connection interrupted: %v", err)
	}
}

func TestV2KeepsSameTCPConnectionAcrossNoopMetadataAndBadCandidates(t *testing.T) {
	s, ctx, target := v2TestFixture(t)
	first := v2TestCommand(t, "test-v2-rule-first", target)
	second := v2TestCommand(t, "test-v2-rule-second", target)
	request, response := v2TestResponse(s, []V2Command{first, second})
	if err := s.applyV2Response(ctx, request, response); err != nil {
		t.Fatal(err)
	}
	v2WaitState(t, s, ctx, first.RuleID, "ready")
	v2WaitState(t, s, ctx, second.RuleID, "ready")
	conn, other := v2Connect(t, first), v2Connect(t, second)
	v2Echo(t, conn, 1)
	v2Echo(t, other, 1)
	s.mu.Lock()
	pid := s.processes[first.RuleID].cmd.Process.Pid
	s.processes[first.RuleID].expires = time.Now().Add(-time.Hour)
	s.mu.Unlock()
	request, response = v2TestResponse(s, []V2Command{})
	if err := s.applyV2Response(ctx, request, response); err != nil {
		t.Fatal(err)
	}
	v2Echo(t, conn, 2)
	updated := first
	copyRule := *first.Rule
	copyRule.Version = 7
	copyRule.EntitlementVersion = 8
	updated.Rule = &copyRule
	updated.CommandID = first.RuleID + "-command-2"
	updated.Generation = 2
	request, response = v2TestResponse(s, []V2Command{updated})
	if err := s.applyV2Response(ctx, request, response); err != nil {
		t.Fatal(err)
	}
	v2Echo(t, conn, 3)
	s.mu.Lock()
	if s.processes[first.RuleID].cmd.Process.Pid != pid || s.processes[first.RuleID].rule.Version != 7 {
		t.Fatal("metadata restarted process or was not updated")
	}
	s.mu.Unlock()
	for _, kind := range []string{"bad-target", "same-generation-conflict", "duplicate-rule", "missing-commands", "wrong-request", "oversized-batch", "state-write-failure"} {
		t.Run(kind, func(t *testing.T) {
			bad := updated
			bad.CommandID = first.RuleID + "-command-3"
			bad.Generation = 3
			newRule := *updated.Rule
			bad.Rule = &newRule
			commands := []V2Command{{CommandID: second.RuleID + "-stop", RuleID: second.RuleID, Generation: 2, Action: "revoke"}, bad}
			request, response := v2TestResponse(s, commands)
			oldPath := s.v2.path
			switch kind {
			case "bad-target":
				newRule.ListenPort = 0
			case "same-generation-conflict":
				bad.Generation = 2
				response.Commands[1] = bad
			case "duplicate-rule":
				response.Commands = append(response.Commands, bad)
			case "missing-commands":
				response.Commands = nil
			case "wrong-request":
				response.RequestID = "wrong-request"
			case "oversized-batch":
				response.Commands = make([]V2Command, 33)
			case "state-write-failure":
				s.v2.path = s.cfg.StateDir
			}
			err := s.applyV2Response(ctx, request, response)
			s.v2.path = oldPath
			if err == nil {
				t.Fatal("invalid response applied")
			}
			v2Echo(t, conn, 4)
			v2Echo(t, other, 4)
		})
	}
	// Candidate filesystem failure is also staged before stopping the old rule.
	changed := updated
	changed.Generation = 3
	changed.CommandID = first.RuleID + "-command-3"
	newRule := *updated.Rule
	newRule.RateMbps = 6
	changed.Rule = &newRule
	changed.RuntimeHash, _ = RuntimeHash(newRule)
	request, response = v2TestResponse(s, []V2Command{changed})
	oldDir := s.cfg.StateDir
	s.cfg.StateDir = filepath.Join(oldDir, "not-present")
	if err := s.applyV2Response(ctx, request, response); err == nil {
		t.Fatal("candidate file failure not reported")
	}
	s.cfg.StateDir = oldDir
	v2Echo(t, conn, 5)
	v2Echo(t, other, 5)
	// The existing rule can accept a new TCP connection while management is lost.
	v2Echo(t, v2Connect(t, first), 1)
}

func TestV2ExplicitStopWaitsForExitAndTombstoneCannotRevive(t *testing.T) {
	s, ctx, target := v2TestFixture(t)
	first := v2TestCommand(t, "test-v2-stop-first", target)
	second := v2TestCommand(t, "test-v2-stop-second", target)
	request, response := v2TestResponse(s, []V2Command{first, second})
	if err := s.applyV2Response(ctx, request, response); err != nil {
		t.Fatal(err)
	}
	v2WaitState(t, s, ctx, first.RuleID, "ready")
	v2WaitState(t, s, ctx, second.RuleID, "ready")
	conn, other := v2Connect(t, first), v2Connect(t, second)
	v2Echo(t, conn, 1)
	v2Echo(t, other, 1)
	stop := V2Command{CommandID: first.RuleID + "-revoke", RuleID: first.RuleID, Generation: 2, Action: "revoke"}
	request, response = v2TestResponse(s, []V2Command{stop})
	if err := s.applyV2Response(ctx, request, response); err != nil {
		t.Fatal(err)
	}
	if s.v2.disk.Records[first.RuleID].State == "stopped" {
		t.Fatal("stop ACK emitted before process exit/result commit")
	}
	v2WaitState(t, s, ctx, first.RuleID, "stopped")
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	var b [1]byte
	n, readErr := conn.Read(b[:])
	networkErr, reset := readErr.(net.Error)
	if n != 0 || (readErr != io.EOF && (!reset || networkErr.Timeout())) {
		t.Fatalf("revoked TCP connection did not close: %v", readErr)
	}
	v2Echo(t, other, 2)
	disk, err := loadV2State(s.cfg, s.v2.disk.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	if disk.Records[first.RuleID].Command.Action != "revoke" || disk.Records[first.RuleID].State != "stopped" {
		t.Fatal("terminal stop was not durable")
	}
	revive := first
	revive.Generation = 3
	revive.CommandID = first.RuleID + "-revive"
	request, response = v2TestResponse(s, []V2Command{revive})
	if err := s.applyV2Response(ctx, request, response); err == nil {
		t.Fatal("revoked identity revived")
	}
	v2Echo(t, other, 3)
	request, response = v2TestResponse(s, []V2Command{stop})
	if err := s.applyV2Response(ctx, request, response); err != nil {
		t.Fatal("duplicate terminal command was not idempotent")
	}
}

func TestV2ControlEpochRollbackFreezesWithoutClosingTCP(t *testing.T) {
	for _, kind := range []string{"epoch", "revision", "recovery"} {
		t.Run(kind, func(t *testing.T) {
			s, ctx, target := v2TestFixture(t)
			command := v2TestCommand(t, "test-v2-freeze-rule", target)
			request, response := v2TestResponse(s, []V2Command{command})
			if err := s.applyV2Response(ctx, request, response); err != nil {
				t.Fatal(err)
			}
			v2WaitState(t, s, ctx, command.RuleID, "ready")
			conn := v2Connect(t, command)
			v2Echo(t, conn, 1)
			request, response = v2TestResponse(s, []V2Command{})
			switch kind {
			case "epoch":
				response.ControlEpoch = "replaced-control-epoch"
			case "revision":
				response.Revision = 0
			case "recovery":
				response.Status = "recovery_required"
			}
			if err := s.applyV2Response(ctx, request, response); err == nil || !s.v2.disk.RecoveryRequired {
				t.Fatal("unsafe control state did not freeze")
			}
			v2Echo(t, conn, 2)
			disk, err := loadV2State(s.cfg, s.v2.disk.AgentID)
			if err != nil || !disk.RecoveryRequired {
				t.Fatal("recovery freeze was not durable")
			}
			request, response = v2TestResponse(s, []V2Command{})
			if err := s.applyV2Response(ctx, request, response); err == nil {
				t.Fatal("recovery requires an explicit trusted takeover")
			}
		})
	}
}

func TestV2StrictResponseTransportAndCacheIdentity(t *testing.T) {
	for _, body := range []string{"{}", "null", "", `{"protocolVersion":2}`, `{"protocolVersion":2} garbage`, `{"unknown":true}`, `{"protocolVersion":`, `{"protocolVersion":1,"protocolVersion":2}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }))
		s, ctx, _ := v2TestFixture(t)
		request := s.v2Request("test-instance")
		response, err := callV2(ctx, Config{ServerURL: server.URL}, "test-token", request)
		if err == nil {
			err = s.applyV2Response(ctx, request, response)
		}
		server.Close()
		if err == nil {
			t.Fatalf("bad v2 response accepted: %q", body)
		}
	}
	for _, status := range []int{401, 403, 502, 503, 522} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
		_, err := callV2(context.Background(), Config{ServerURL: server.URL}, "test-token", V2SyncRequest{})
		server.Close()
		if err == nil {
			t.Fatal("HTTP failure accepted")
		}
	}
	s, ctx, target := v2TestFixture(t)
	command := v2TestCommand(t, "test-v2-cache-rule", target)
	request, response := v2TestResponse(s, []V2Command{command})
	if err := s.applyV2Response(ctx, request, response); err != nil {
		t.Fatal(err)
	}
	v2WaitState(t, s, ctx, command.RuleID, "ready")
	wrong := s.cfg
	wrong.ServerURL = "https://other.example.test"
	if _, err := loadV2State(wrong, s.v2.disk.AgentID); err == nil {
		t.Fatal("cache accepted different control origin")
	}
	if _, err := loadV2State(s.cfg, "different-agent"); err == nil {
		t.Fatal("cache accepted different agent")
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(s.v2.path, 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := loadV2State(s.cfg, s.v2.disk.AgentID); err == nil {
			t.Fatal("public cache accepted")
		}
		_ = os.Chmod(s.v2.path, 0600)
	}
	bad := filepath.Join(s.cfg.StateDir, "bad-state.json")
	if err := os.WriteFile(bad, []byte(`{"schema":2} {}`), 0600); err != nil {
		t.Fatal(err)
	}
	var disk v2DiskState
	if err := readV2PrivateJSON(bad, &disk); err == nil {
		t.Fatal("extra cached JSON accepted")
	}
}

func TestV2InvalidPolicyDoesNotSilentlyDowngrade(t *testing.T) {
	if err := Run(context.Background(), Config{OfflinePolicy: "forever"}); err == nil || !strings.Contains(err.Error(), "offline policy") {
		t.Fatal("unknown policy accepted")
	}
}

func TestV2UnexpectedExitRetriesSameCommandWithoutRestartingOthers(t *testing.T) {
	s, ctx, target := v2TestFixture(t)
	first := v2TestCommand(t, "test-v2-retry-first", target)
	second := v2TestCommand(t, "test-v2-retry-second", target)
	request, response := v2TestResponse(s, []V2Command{first, second})
	if err := s.applyV2Response(ctx, request, response); err != nil {
		t.Fatal(err)
	}
	v2WaitState(t, s, ctx, first.RuleID, "ready")
	v2WaitState(t, s, ctx, second.RuleID, "ready")
	other := v2Connect(t, second)
	v2Echo(t, other, 1)
	s.mu.Lock()
	p := s.processes[first.RuleID]
	oldPID := p.cmd.Process.Pid
	s.mu.Unlock()
	if err := p.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.done:
	case <-time.After(3 * time.Second):
		t.Fatal("killed helper did not exit")
	}
	v2WaitState(t, s, ctx, first.RuleID, "ready")
	s.mu.Lock()
	newPID := s.processes[first.RuleID].cmd.Process.Pid
	s.mu.Unlock()
	if oldPID == newPID {
		t.Fatal("failed process not replaced")
	}
	v2Echo(t, v2Connect(t, first), 1)
	v2Echo(t, other, 2)
}

func TestV2CacheColdStartRebuildsRunningRulesAndRetainsTombstones(t *testing.T) {
	s, ctx, target := v2TestFixture(t)
	command := v2TestCommand(t, "test-v2-cold-running", target)
	stop := V2Command{CommandID: "test-v2-cold-revoke", RuleID: "test-v2-cold-revoked", Generation: 1, Action: "revoke"}
	request, response := v2TestResponse(s, []V2Command{command, stop})
	if err := s.applyV2Response(ctx, request, response); err != nil {
		t.Fatal(err)
	}
	v2WaitState(t, s, ctx, command.RuleID, "ready")
	v2WaitState(t, s, ctx, stop.RuleID, "stopped")
	s.mu.Lock()
	p := s.processes[command.RuleID]
	s.stopLocked(command.RuleID)
	s.mu.Unlock()
	select {
	case <-p.done:
	case <-time.After(3 * time.Second):
		t.Fatal("maintenance stop did not finish")
	}
	disk, err := loadV2State(s.cfg, s.v2.disk.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a new Agent lifecycle using the same private trusted cache and
	// token identity. No HTTP control response is involved in reconstruction.
	s.mu.Lock()
	s.processes = map[string]*process{}
	s.v2.disk = disk
	s.v2.cacheOnly = true
	err = s.reconcileV2Locked(ctx)
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	v2WaitState(t, s, ctx, command.RuleID, "ready")
	v2Echo(t, v2Connect(t, command), 1)
	s.mu.Lock()
	_, revived := s.processes[stop.RuleID]
	s.mu.Unlock()
	if revived {
		t.Fatal("cached tombstone revived")
	}
	conflicting := disk.Records[command.RuleID]
	conflicting.State = "persisted"
	other := *conflicting.LastApplied
	other.CommandID += "-conflict"
	conflicting.LastApplied = &other
	disk.Records[command.RuleID] = conflicting
	if err := writeV2PrivateJSON(s.v2.path, disk); err != nil {
		t.Fatal(err)
	}
	if _, err := loadV2State(s.cfg, s.v2.disk.AgentID); err == nil {
		t.Fatal("conflicting last-applied generation accepted from cache")
	}
	if err := os.WriteFile(s.v2.path, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadV2State(s.cfg, s.v2.disk.AgentID); err == nil {
		t.Fatal("incomplete cache silently reset identity")
	}
}

func TestV2FrozenPendingCandidateCannotReplaceLastGoodTCP(t *testing.T) {
	s, ctx, target := v2TestFixture(t)
	command := v2TestCommand(t, "test-v2-frozen-pending", target)
	request, response := v2TestResponse(s, []V2Command{command})
	if err := s.applyV2Response(ctx, request, response); err != nil {
		t.Fatal(err)
	}
	v2WaitState(t, s, ctx, command.RuleID, "ready")
	conn := v2Connect(t, command)
	v2Echo(t, conn, 1)
	changed := command
	rule := *command.Rule
	rule.RateMbps = 6
	changed.Rule = &rule
	changed.RuntimeHash, _ = RuntimeHash(rule)
	changed.Generation = 2
	changed.CommandID = command.RuleID + "-pending-new"
	// Model an interruption after validated intent commit but before the
	// candidate's secret files/process are prepared. LastApplied is still v1.
	if err := validateV2Command(changed); err != nil {
		t.Fatal(err)
	}
	record := s.v2.disk.Records[command.RuleID]
	record.Command = changed
	record.State = "persisted"
	s.v2.disk.Records[command.RuleID] = record
	s.v2.disk.Revision++
	if err := writeV2PrivateJSON(s.v2.path, s.v2.disk); err != nil {
		t.Fatal(err)
	}
	request, response = v2TestResponse(s, []V2Command{})
	response.ControlEpoch = "other-control-epoch"
	if err := s.applyV2Response(ctx, request, response); err == nil {
		t.Fatal("epoch change not frozen")
	}
	s.mu.Lock()
	delete(s.v2.retryAt, command.RuleID)
	err := s.reconcileV2Locked(ctx)
	p := s.processes[command.RuleID]
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if p.rule.RateMbps != 5 || p.stopping {
		t.Fatal("frozen reconciliation applied a pending candidate")
	}
	v2Echo(t, conn, 2)
	// Even after a real process failure, freeze reconstructs only LastApplied.
	if err := p.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.done:
	case <-time.After(3 * time.Second):
		t.Fatal("helper did not exit")
	}
	s.mu.Lock()
	err = s.reconcileV2Locked(ctx)
	restored := s.processes[command.RuleID]
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if restored.rule.RateMbps != 5 {
		t.Fatal("frozen restart used pending instead of last applied configuration")
	}
}

func TestV2RunLoopKeepsTCPAfterControl401(t *testing.T) {
	fixture, _, target := v2TestFixture(t)
	command := v2TestCommand(t, "test-v2-loop-running", target)
	firstSync := make(chan struct{})
	failedSync := make(chan struct{})
	seen := 0
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request V2SyncRequest
		if r.URL.Path != "/api/relay-agent/v2/sync" || json.NewDecoder(r.Body).Decode(&request) != nil {
			http.Error(w, "bad request", 400)
			return
		}
		seen++
		if seen == 1 {
			json.NewEncoder(w).Encode(V2SyncResponse{ProtocolVersion: ProtocolV2, AgentID: request.AgentID, RequestID: request.RequestID, ControlEpoch: "control-epoch-v2", PreviousRevision: 0, Revision: 1, Status: "ready", OfflinePolicy: KeepLast, Commands: []V2Command{command}, TrafficAcks: []V2TrafficAck{}})
			close(firstSync)
			return
		}
		if seen == 2 {
			close(failedSync)
		}
		w.WriteHeader(401)
	}))
	defer control.Close()
	cfg := fixture.cfg
	cfg.ServerURL = control.URL
	cfg.StateDir = t.TempDir()
	_ = os.Chmod(cfg.StateDir, 0700)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runV2(ctx, cfg, "agent-loop-test", "loop-test-token") }()
	loopExited := false
	defer func() {
		cancel()
		if loopExited {
			return
		}
		select {
		case <-done:
		case <-time.After(4 * time.Second):
			t.Error("v2 loop did not cancel")
		}
	}()
	select {
	case <-firstSync:
	case <-time.After(3 * time.Second):
		t.Fatal("first v2 sync missing")
	}
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(command.Rule.ListenPort))
	var conn net.Conn
	var err error
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		conn, err = net.DialTimeout("tcp4", address, 50*time.Millisecond)
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if conn == nil {
		t.Fatal("loop helper did not bind")
	}
	defer conn.Close()
	v2Echo(t, conn, 1)
	select {
	case <-failedSync:
	case <-time.After(8 * time.Second):
		t.Fatal("control 401 was not exercised")
	}
	for sequence := 2; sequence < 8; sequence++ {
		v2Echo(t, conn, sequence)
		time.Sleep(25 * time.Millisecond)
	}
	v2Echo(t, v2Connect(t, command), 1)
	cancel()
	select {
	case <-done:
		loopExited = true
	case <-time.After(4 * time.Second):
		t.Fatal("v2 loop did not reap its children on cancellation")
	}
	_ = conn.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
	_, err = conn.Read(make([]byte, 1))
	if timeout, ok := err.(net.Error); err == nil || ok && timeout.Timeout() {
		t.Fatal("runV2 returned while its forwarding child was still live")
	}
}
