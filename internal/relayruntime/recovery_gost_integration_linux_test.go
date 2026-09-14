//go:build linux

package relayruntime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Opt-in real GOST verification of the actual production Run loop and root
// Unix-socket channel. This test never uses the TCP helper executable and must
// run in a dedicated loopback-only network namespace, not a production host's
// network. A skipped/compiled test is not reported as runtime acceptance.
func TestRealGostProtectedRecoveryKeepsOriginalConnection(t *testing.T) {
	if os.Getenv("MSBOOST_REAL_GOST_RECOVERY") != "1" {
		t.Skip("set MSBOOST_REAL_GOST_RECOVERY=1 in the isolated Linux GOST runner")
	}
	if os.Geteuid() != 0 || os.Getenv("MSBOOST_TEST_LOOPBACK_NETNS") != "1" {
		t.Fatal("real recovery requires isolated Linux root and loopback-only netns")
	}
	interfaces, err := net.Interfaces()
	if err != nil || len(interfaces) != 1 || interfaces[0].Name != "lo" {
		t.Fatal("refusing real recovery test outside a loopback-only network namespace")
	}
	binary, err := filepath.Abs(os.Getenv("GOST_TEST_BINARY"))
	if err != nil || os.Getenv("GOST_TEST_BINARY") == "" {
		t.Fatal("verified real GOST_TEST_BINARY required")
	}
	version, err := exec.Command(binary, "-V").CombinedOutput()
	if err != nil || !strings.Contains(string(version), "3.3.0") {
		t.Fatalf("real pinned GOST 3.3.0 required: %v", err)
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
	echoDone := make(chan struct{})
	go func() {
		defer close(echoDone)
		conn, err := echo.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(conn, conn)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	command := v2TestCommand(t, "real-gost-recovery-rule", echo.Addr().String())
	const agentID = "real-gost-recovery-agent"
	const oldToken = "isolated-original-management-credential"
	const newToken = "isolated-root-replacement-management-credential"
	const oldEpoch = "isolated-original-control-epoch"
	const newEpoch = "isolated-root-replacement-control-epoch"
	var mu sync.Mutex
	phase, oldReady, newReady, stoppedAck := 0, false, false, false
	pause := V2Command{CommandID: "real-root-explicit-pause", RuleID: command.RuleID, Generation: command.Generation + 1, Action: "pause", RuntimeHash: command.RuntimeHash, Reason: "isolated_explicit_pause"}
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", 405)
			return
		}
		if r.URL.Path == "/api/relay-agent/register" {
			_ = json.NewEncoder(w).Encode(map[string]string{"agentId": agentID, "token": oldToken})
			return
		}
		mu.Lock()
		currentPhase := phase
		mu.Unlock()
		token := oldToken
		if currentPhase >= 2 {
			token = newToken
		}
		if r.URL.Path != "/api/relay-agent/v2/sync" || r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "fixture auth", 401)
			return
		}
		var request V2SyncRequest
		if json.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(&request) != nil {
			http.Error(w, "fixture JSON", 400)
			return
		}
		mu.Lock()
		for _, ack := range request.Acks {
			if ack.CommandID == command.CommandID && ack.State == "ready" {
				if currentPhase == 0 {
					oldReady = true
				}
				if currentPhase == 3 && request.ControlEpoch == newEpoch {
					newReady = true
				}
			}
			if currentPhase == 4 && ack.CommandID == pause.CommandID && ack.State == "stopped" {
				stoppedAck = true
			}
		}
		mu.Unlock()
		response := V2SyncResponse{ProtocolVersion: 2, AgentID: agentID, RequestID: request.RequestID, ControlEpoch: oldEpoch, PreviousRevision: request.AppliedRevision, Revision: 1, Status: "ready", OfflinePolicy: KeepLast, Commands: []V2Command{}, TrafficAcks: []V2TrafficAck{}}
		if currentPhase == 0 && request.AppliedRevision == 0 {
			response.Commands = []V2Command{command}
		}
		if currentPhase >= 1 {
			response.ControlEpoch = newEpoch
		}
		if currentPhase == 1 || currentPhase == 2 {
			response.Status = "recovery_required"
		}
		if currentPhase == 4 {
			response.Revision, response.Commands = 2, []V2Command{pause}
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{ServerURL: control.URL, EnrollmentToken: "isolated-enrollment-only", StateDir: dir, GostBinary: binary, OfflinePolicy: KeepLast})
	}()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("real runtime exit: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("real runtime did not finish child cleanup")
		}
		control.Close()
		echo.Close()
		select {
		case <-echoDone:
		case <-time.After(3 * time.Second):
			t.Error("real echo target retained an unexpected connection")
		}
	}()
	wait := func(label string, predicate func() bool, pulse func()) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if predicate() {
				return
			}
			if pulse != nil {
				pulse()
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("real recovery phase not observed: %s", label)
	}
	wait("initial real GOST ready ACK", func() bool { mu.Lock(); defer mu.Unlock(); return oldReady }, nil)
	conn := v2Connect(t, command) // The sole client dial for the entire test.
	defer conn.Close()
	sample := 0
	pulse := func() { sample++; v2Echo(t, conn, sample) }
	pulse()
	mu.Lock()
	phase = 1
	mu.Unlock()
	wait("unsolicited epoch frozen", func() bool {
		var state v2DiskState
		return readV2PrivateJSON(filepath.Join(dir, v2StateFile), &state) == nil && state.RecoveryRequired
	}, pulse)
	var snapshot V2RecoverySnapshot
	if err := callRecoverySocket(ctx, dir, "snapshot", struct{}{}, &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Records) != 1 || !snapshot.Records[0].Running || snapshot.Records[0].ProcessEpoch == "" || snapshot.Records[0].LastApplied == nil {
		t.Fatal("root snapshot did not identify the original live GOST")
	}
	beforeEpoch := snapshot.Records[0].ProcessEpoch
	last := RecoveryCommandRef(*snapshot.Records[0].LastApplied)
	plan := V2RecoveryPlan{ProtocolVersion: 2, RecoveryID: snapshot.RecoveryID, PlanID: "isolated-real-root-plan", PlanSequence: snapshot.PlanSequence + 1, AgentID: agentID, ExpectedServerURL: snapshot.ServerURL, ExpectedControlEpoch: snapshot.ControlEpoch, ServerURL: control.URL, ControlEpoch: newEpoch, Token: newToken, Revision: 1, Decisions: []V2RecoveryDecision{{RuleID: command.RuleID, Action: "adopt", ExpectedCommand: RecoveryCommandRef(snapshot.Records[0].Command), ExpectedLastApplied: &last, ExpectedRuntimeHash: snapshot.Records[0].RuntimeHash, Command: &command}}}
	mu.Lock()
	phase = 2
	mu.Unlock()
	var result V2RecoveryResult
	if err := callRecoverySocket(ctx, dir, "adopt", plan, &result); err != nil {
		t.Fatal(err)
	}
	pulse()
	var after V2RecoverySnapshot
	if err := callRecoverySocket(ctx, dir, "snapshot", struct{}{}, &after); err != nil {
		t.Fatal(err)
	}
	if after.Records[0].ProcessEpoch != beforeEpoch || after.PlanSequence != plan.PlanSequence || after.ControlEpoch != newEpoch {
		t.Fatal("trusted takeover restarted real GOST or lost its checkpoint")
	}
	if err := callRecoverySocket(ctx, dir, "adopt", plan, &result); err != nil {
		t.Fatalf("real root retry was not idempotent: %v", err)
	}
	mu.Lock()
	phase = 3
	mu.Unlock()
	wait("new trusted epoch confirmed without commands", func() bool {
		mu.Lock()
		ready := newReady
		mu.Unlock()
		var state v2DiskState
		return ready && readV2PrivateJSON(filepath.Join(dir, v2StateFile), &state) == nil && !state.RecoveryRequired
	}, pulse)
	pulse()
	mu.Lock()
	phase = 4
	mu.Unlock()
	wait("explicit pause completed and ACKed", func() bool { mu.Lock(); defer mu.Unlock(); return stoppedAck }, nil)
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	var trailing [1]byte
	if _, err := conn.Read(trailing[:]); err == nil {
		t.Fatal("explicit real GOST pause did not close the original connection")
	} else if networkErr, ok := err.(net.Error); ok && networkErr.Timeout() {
		t.Fatal("connection merely timed out instead of closing after explicit pause")
	}
	t.Logf("REAL_GOST_RECOVERY_PASS client_dials=1 reconnects=0 root_socket=true original_process_epoch_unchanged=true trusted_epoch_acked=true explicit_pause_stopped=true samples=%d", sample)
}
