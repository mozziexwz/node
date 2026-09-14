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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Real forwarding is exercised, but management is an explicit mock: this is
// evidence for existing/new connections and per-rule isolation, not a claim
// that a real control-plane Compose service was stopped or backed up.
func TestRealGostKeepLastNewConnectionsAndRuleIsolation(t *testing.T) {
	if os.Getenv("MSBOOST_REAL_GOST_RECOVERY") != "1" {
		t.Skip("set MSBOOST_REAL_GOST_RECOVERY=1 in the isolated Linux GOST runner")
	}
	interfaces, err := net.Interfaces()
	if os.Geteuid() != 0 || os.Getenv("MSBOOST_TEST_LOOPBACK_NETNS") != "1" || err != nil || len(interfaces) != 1 || interfaces[0].Name != "lo" {
		t.Fatal("refusing rule-isolation test outside root loopback-only Linux netns")
	}
	binary, err := filepath.Abs(os.Getenv("GOST_TEST_BINARY"))
	if err != nil || os.Getenv("GOST_TEST_BINARY") == "" {
		t.Fatal("verified real GOST_TEST_BINARY required")
	}
	version, err := exec.Command(binary, "-V").CombinedOutput()
	if err != nil || !strings.Contains(string(version), "3.3.0") {
		t.Fatalf("real pinned GOST 3.3.0 required: %v", err)
	}
	dir, err := os.MkdirTemp("", "ri-")
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
			go func() {
				defer echoes.Done()
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	first := v2TestCommand(t, "real-gost-keep-last-rule-a", echo.Addr().String())
	second := v2TestCommand(t, "real-gost-keep-last-rule-b", echo.Addr().String())
	for second.Rule.ListenPort == first.Rule.ListenPort {
		second = v2TestCommand(t, second.RuleID, echo.Addr().String())
	}
	pause := V2Command{CommandID: "real-rule-b-explicit-pause", RuleID: second.RuleID, Generation: 2, Action: "pause", RuntimeHash: second.RuntimeHash, Reason: "isolated_rule_b_only"}
	const agentID = "isolated-real-two-rule-agent"
	const token = "isolated-two-rule-management-credential"
	const epoch = "isolated-two-rule-control-epoch"
	var mu sync.Mutex
	phase, unavailableHits := 0, 0
	ready := map[string]bool{}
	restored, stopped := false, false
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", 405)
			return
		}
		if r.URL.Path == "/api/relay-agent/register" {
			_ = json.NewEncoder(w).Encode(map[string]string{"agentId": agentID, "token": token})
			return
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
		currentPhase := phase
		for _, ack := range request.Acks {
			if ack.State == "ready" && (ack.CommandID == first.CommandID || ack.CommandID == second.CommandID) {
				ready[ack.RuleID] = true
			}
			if currentPhase == 3 && ack == (V2Ack{CommandID: pause.CommandID, RuleID: second.RuleID, Generation: 2, RuntimeHash: pause.RuntimeHash, State: "stopped"}) {
				stopped = true
			}
		}
		if currentPhase == 1 {
			unavailableHits++
		}
		if currentPhase == 2 && request.ControlEpoch == epoch && request.AppliedRevision == 1 && ready[first.RuleID] && ready[second.RuleID] {
			restored = true
		}
		mu.Unlock()
		if currentPhase == 1 {
			http.Error(w, "explicit simulated control outage", 503)
			return
		}
		response := V2SyncResponse{ProtocolVersion: 2, AgentID: agentID, RequestID: request.RequestID, ControlEpoch: epoch, PreviousRevision: request.AppliedRevision, Revision: 1, OfflinePolicy: KeepLast, Status: "ready", Commands: []V2Command{}, TrafficAcks: []V2TrafficAck{}}
		if request.AppliedRevision == 0 {
			response.Commands = []V2Command{first, second}
		}
		if currentPhase == 3 {
			response.Revision, response.Commands = 2, []V2Command{pause}
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{ServerURL: control.URL, EnrollmentToken: "isolated-enrollment-only", StateDir: dir, GostBinary: binary, OfflinePolicy: KeepLast})
	}()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("real two-rule runtime exit: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("real two-rule child cleanup did not finish")
		}
		control.Close()
		echo.Close()
		<-acceptDone
		echoes.Wait()
	}()
	wait := func(label string, predicate func() bool, pulse func()) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			complete := predicate()
			mu.Unlock()
			if complete {
				return
			}
			if pulse != nil {
				pulse()
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("real two-rule phase not observed: %s", label)
	}
	wait("both running GOST ACKs", func() bool { return ready[first.RuleID] && ready[second.RuleID] }, nil)
	originalA, originalB := v2Connect(t, first), v2Connect(t, second)
	defer originalA.Close()
	defer originalB.Close()
	sequence := 0
	pulseA := func() { sequence++; v2Echo(t, originalA, sequence) }
	pulseBoth := func() { pulseA(); v2Echo(t, originalB, sequence) }
	pulseBoth()
	mu.Lock()
	phase = 1
	mu.Unlock()
	wait("production Run received 503", func() bool { return unavailableHits > 0 }, pulseBoth)
	// This is a distinct, deliberate new connection, never a replacement for A.
	duringOutage := v2Connect(t, first)
	v2Echo(t, duringOutage, 1)
	duringOutage.Close()
	pulseBoth()
	mu.Lock()
	phase = 2
	mu.Unlock()
	wait("same-epoch management restored without changes", func() bool { return restored }, pulseBoth)
	pulseBoth()
	mu.Lock()
	phase = 3
	mu.Unlock()
	wait("only B pause is durably stopped", func() bool { return stopped }, pulseA)
	_ = originalB.SetReadDeadline(time.Now().Add(time.Second))
	var trailing [1]byte
	if n, err := originalB.Read(trailing[:]); n != 0 || err == nil {
		t.Fatal("paused B original connection did not close")
	} else if networkErr, ok := err.(net.Error); ok && networkErr.Timeout() {
		t.Fatal("paused B only timed out; no proof of closure")
	}
	if unexpected, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(second.Rule.ListenPort)), time.Second); err == nil {
		unexpected.Close()
		t.Fatal("new connection succeeded on explicitly stopped B")
	} else if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("stopped B did not explicitly refuse its old listening port: %v", err)
	}
	pulseA()
	afterPause := v2Connect(t, first)
	v2Echo(t, afterPause, 1)
	afterPause.Close()
	pulseA()
	t.Logf("REAL_GOST_RULE_ISOLATION_PASS original_a_dials=1 original_a_reconnects=0 a_new_during_503=true a_new_after_b_pause=true b_original_closed=true b_new_refused=true mock_control=true samples=%d", sequence)
}
