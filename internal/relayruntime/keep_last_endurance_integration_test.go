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
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// This is deliberately opt-in: compile-only or a skipped test is NOT evidence
// of Linux/GOST endurance. Each topology has independent production Run loops
// for every hop; the client dials exactly once after setup, never reconnects.
// Run only via deploy/relay_endurance_test.sh in a new loopback-only netns.
func TestRealGostKeepLastEndurance(t *testing.T) {
	rawDuration := os.Getenv("MSBOOST_KEEP_LAST_DURATION")
	if rawDuration == "" {
		t.Skip("set MSBOOST_KEEP_LAST_DURATION=5m or 30m for real endurance")
	}
	duration, err := time.ParseDuration(rawDuration)
	if err != nil || duration < 5*time.Minute || duration > 24*time.Hour {
		t.Fatal("real endurance duration must be between 5m and 24h")
	}
	if runtime.GOOS != "linux" || os.Getenv("MSBOOST_TEST_LOOPBACK_NETNS") != "1" {
		t.Fatal("real endurance requires a dedicated Linux loopback network namespace")
	}
	interfaces, err := net.Interfaces()
	if err != nil || len(interfaces) != 1 || interfaces[0].Name != "lo" {
		t.Fatal("refusing test outside a loopback-only network namespace")
	}
	binary, err := filepath.Abs(os.Getenv("GOST_TEST_BINARY"))
	if err != nil || os.Getenv("GOST_TEST_BINARY") == "" {
		t.Fatal("GOST_TEST_BINARY must identify the verified real GOST executable")
	}
	for _, topology := range []struct {
		name string
		hops int
		tls  bool
	}{{"single_tcp", 1, false}, {"two_hop_tcp", 2, false}, {"three_hop_tls", 3, true}} {
		t.Run(topology.name, func(t *testing.T) {
			t.Parallel()
			keepLastRealTopology(t, binary, duration, topology.hops, topology.tls)
		})
	}
}

var endurancePhases = []string{"http_503", "http_401", "malformed_json", "truncated_body", "request_timeout", "epoch_change"}

type enduranceControl struct {
	phase   *atomic.Int32
	command V2Command
	id      string
	mu      sync.Mutex
	hits    map[string]int
	ready   bool
	release chan struct{}
}

func (c *enduranceControl) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	const syntheticToken = "isolated-endurance-fixture-token-not-a-production-credential"
	if r.URL.Path == "/api/relay-agent/register" {
		_ = json.NewEncoder(w).Encode(map[string]string{"agentId": c.id, "token": syntheticToken})
		return
	}
	if r.URL.Path != "/api/relay-agent/v2/sync" || r.Header.Get("Authorization") != "Bearer "+syntheticToken {
		http.Error(w, "fixture authorization", http.StatusUnauthorized)
		return
	}
	var request V2SyncRequest
	if json.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(&request) != nil {
		http.Error(w, "fixture request", http.StatusBadRequest)
		return
	}
	phase := int(c.phase.Load())
	c.mu.Lock()
	for _, ack := range request.Acks {
		if ack.CommandID == c.command.CommandID && ack.State == "ready" {
			c.ready = true
		}
	}
	if phase >= 0 {
		c.hits[endurancePhases[phase]]++
	}
	c.mu.Unlock()
	if phase >= 0 {
		switch endurancePhases[phase] {
		case "http_503":
			http.Error(w, "fixture unavailable", http.StatusServiceUnavailable)
			return
		case "http_401":
			http.Error(w, "fixture unavailable", http.StatusUnauthorized)
			return
		case "malformed_json":
			_, _ = io.WriteString(w, `{"protocolVersion":`)
			return
		case "truncated_body":
			w.Header().Set("Content-Length", "1000")
			_, _ = io.WriteString(w, `{"status":"ready"}`)
			return
		case "request_timeout":
			// Fully consume the request first, and provide explicit teardown as
			// well as cancellation: stalled handlers must not deadlock Close.
			select {
			case <-r.Context().Done():
			case <-c.release:
			}
			return
		}
	}
	response := V2SyncResponse{ProtocolVersion: ProtocolV2, AgentID: c.id, RequestID: request.RequestID, ControlEpoch: "isolated-original-epoch", PreviousRevision: request.AppliedRevision, Revision: 1, Status: "ready", OfflinePolicy: KeepLast, Commands: []V2Command{}, TrafficAcks: []V2TrafficAck{}}
	if request.AppliedRevision == 0 {
		response.Commands = []V2Command{c.command}
	}
	if phase >= 0 && endurancePhases[phase] == "epoch_change" {
		response.ControlEpoch = "isolated-restored-control-epoch"
	}
	for _, sample := range request.Traffic {
		response.TrafficAcks = append(response.TrafficAcks, V2TrafficAck{RuleID: sample.ID, Epoch: sample.Epoch, Sequence: sample.Sequence})
	}
	_ = json.NewEncoder(w).Encode(response)
}

func keepLastRealTopology(t *testing.T, binary string, duration time.Duration, hops int, useTLS bool) {
	t.Helper()
	echo, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	var echoConnections sync.WaitGroup
	go func() {
		for {
			conn, e := echo.Accept()
			if e != nil {
				return
			}
			echoConnections.Add(1)
			go func() {
				defer echoConnections.Done()
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var phase atomic.Int32
	phase.Store(-1)
	controls := make([]*enduranceControl, 0, hops)
	stateDirs := make([]string, 0, hops)
	done := make(chan error, hops)
	var servers []*httptest.Server
	defer func() {
		cancel()
		for _, c := range controls {
			close(c.release)
		}
		for range controls {
			select {
			case err := <-done:
				if err != nil && err != context.Canceled {
					t.Errorf("production runtime ended unexpectedly: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Error("production runtime cleanup did not finish")
			}
		}
		for _, server := range servers {
			server.Close()
		}
		echo.Close()
		echoConnections.Wait()
	}()
	// Construct from exit back to entry, with a distinct Run/state/control for
	// every hop. TLS links use the actual certificate and strict hostname/CA.
	target := echo.Addr().String()
	var upstream *TLSClient
	for hop := 0; hop < hops; hop++ {
		reserve, e := net.Listen("tcp4", "127.0.0.1:0")
		if e != nil {
			t.Fatal(e)
		}
		port := reserve.Addr().(*net.TCPAddr).Port
		reserve.Close()
		id := fmt.Sprintf("isolated-%s-hop-%d", t.Name()[len("TestRealGostKeepLastEndurance/"):], hop)
		rule := Rule{ID: id, Version: 1, ListenPort: port, Protocol: "tcp", Strategy: "round", RateMbps: 5, Targets: []string{target}, AllowedSources: []string{"127.0.0.1"}}
		if upstream != nil {
			rule.TargetTLS = []TLSClient{*upstream}
		}
		if useTLS && hop < hops-1 {
			name := fmt.Sprintf("hop-%d.endurance.invalid", hop)
			cert, key, e := NewTLSIdentity(name, time.Now().Add(duration+time.Hour))
			if e != nil {
				t.Fatal(e)
			}
			rule.Protocol, rule.TLSCertificate, rule.TLSPrivateKey = "tls", cert, key
			upstream = &TLSClient{CA: cert, ServerName: name}
		}
		hash, e := RuntimeHash(rule)
		if e != nil {
			t.Fatal(e)
		}
		c := &enduranceControl{phase: &phase, id: id, command: V2Command{CommandID: id + "-command", RuleID: id, Generation: 1, Action: "upsert", RuntimeHash: hash, Rule: &rule}, hits: map[string]int{}, release: make(chan struct{})}
		server := httptest.NewServer(http.HandlerFunc(c.serve))
		controls, servers = append(controls, c), append(servers, server)
		// Keep the absolute Unix socket path under Linux's sockaddr_un bound,
		// including when the runner's private TMPDIR has a descriptive prefix.
		dir, e := os.MkdirTemp("", "kl-")
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		stateDirs = append(stateDirs, dir)
		cfg := Config{ServerURL: server.URL, EnrollmentToken: "isolated-fixture-enrollment", StateDir: dir, GostBinary: binary, OfflinePolicy: KeepLast}
		go func() { done <- Run(ctx, cfg) }()
		target = net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	}
	// Wait for real GOST's running/bind ACK at every independently managed hop.
	for deadline := time.Now().Add(20 * time.Second); ; {
		ready := true
		for _, c := range controls {
			c.mu.Lock()
			ready = ready && c.ready
			c.mu.Unlock()
		}
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("real GOST did not become ready at every hop")
		}
		time.Sleep(50 * time.Millisecond)
	}
	conn, err := net.DialTimeout("tcp4", target, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	started := time.Now()
	lastPhase := -1
	var samples int
	for time.Since(started) < duration {
		nextPhase := min(int(time.Since(started)*time.Duration(len(endurancePhases))/duration), len(endurancePhases)-1)
		if nextPhase != lastPhase {
			phase.Store(int32(nextPhase))
			lastPhase = nextPhase
			t.Logf("REAL_GOST_KEEP_LAST phase=%s hops=%d tls=%t elapsed=%s", endurancePhases[nextPhase], hops, useTLS, time.Since(started).Round(time.Millisecond))
		}
		payload := []byte(fmt.Sprintf("same-tcp-connection:%s:%08d", t.Name(), samples))
		if err = conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err = conn.Write(payload); err != nil {
			t.Fatalf("existing connection write failed; no reconnect permitted: %v", err)
		}
		got := make([]byte, len(payload))
		if _, err = io.ReadFull(conn, got); err != nil || !bytes.Equal(payload, got) {
			t.Fatalf("existing connection lost/corrupted; no reconnect permitted: %v", err)
		}
		samples++
		time.Sleep(time.Second)
	}
	for i, c := range controls {
		c.mu.Lock()
		for _, name := range endurancePhases {
			if c.hits[name] < 1 {
				t.Errorf("hop %d never actually observed fault %s", i, name)
			}
		}
		c.mu.Unlock()
		var disk v2DiskState
		if err := readV2PrivateJSON(filepath.Join(stateDirs[i], v2StateFile), &disk); err != nil || !disk.RecoveryRequired {
			t.Errorf("hop %d did not freeze management after epoch change: %v", i, err)
		}
	}
	if !t.Failed() {
		t.Logf("REAL_GOST_KEEP_LAST_PASS duration=%s hops=%d tls=%t samples=%d client_dials=1 reconnects=0 all_faults_observed=true", time.Since(started).Round(time.Millisecond), hops, useTLS, samples)
	}
}
