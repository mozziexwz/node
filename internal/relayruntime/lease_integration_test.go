package relayruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

// Run's production GOST config binds :port, so this test MUST run inside a
// dedicated Linux network namespace containing only loopback. It does not
// alter the production listener, lease bounds, or watchdog implementation.
// Example (using checksum-verified binaries, as root on an isolated test host):
// unshare --net -- bash -c 'ip link set lo up; exec env MSBOOST_TEST_LOOPBACK_NETNS=1 GOST_TEST_BINARY="$1" "$2" -test.run "^TestRealGostLeaseExpiryClosesExistingConnection$" -test.count=1 -test.v' bash /absolute/gost /absolute/relay.test
func TestRealGostLeaseExpiryClosesExistingConnection(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("production Run and process-death protection require Linux")
	}
	if os.Getenv("MSBOOST_TEST_LOOPBACK_NETNS") != "1" {
		t.Skip("run in a dedicated loopback-only network namespace; never on the host network")
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	if len(interfaces) != 1 || interfaces[0].Name != "lo" {
		t.Fatal("refusing test: network namespace must contain only lo")
	}
	binary := os.Getenv("GOST_TEST_BINARY")
	if binary == "" {
		t.Fatal("GOST_TEST_BINARY must identify a checksum-verified GOST v3 binary")
	}
	binary, err = filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}

	echo, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			conn, err := echo.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	reserve, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listenPort := reserve.Addr().(*net.TCPAddr).Port
	listenAddress := reserve.Addr().String()
	_ = reserve.Close()

	firstSync := make(chan time.Time, 1)
	blockedSync := make(chan struct{})
	releaseHandler := make(chan struct{})
	var blockedOnce sync.Once
	var mu sync.Mutex
	syncCount := 0
	const token = "isolated-lease-test-token-not-a-production-credential"
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", 405)
			return
		}
		switch r.URL.Path {
		case "/api/relay-agent/register":
			_ = json.NewEncoder(w).Encode(map[string]string{"agentId": "isolated-lease-test-agent", "token": token})
		case "/api/relay-agent/sync":
			if r.Header.Get("Authorization") != "Bearer "+token {
				http.Error(w, "auth", 401)
				return
			}
			mu.Lock()
			syncCount++
			count := syncCount
			mu.Unlock()
			if count == 1 {
				now := time.Now()
				firstSync <- now
				// The six-second server lease uses the real <=60s apply bound.
				// Run syncs every five seconds; its second HTTP request will be
				// blocked when the independent 250ms watchdog must kill GOST.
				_ = json.NewEncoder(w).Encode(SyncResponse{ServerTime: now.UnixMilli(), LeaseSeconds: 6, Rules: []Rule{{ID: "isolated-lease-rule", Version: 1, ListenPort: listenPort, Protocol: "tcp", Strategy: "round", RateMbps: 5, Targets: []string{echo.Addr().String()}, AllowedSources: []string{"127.0.0.1"}, LeaseUntil: now.Add(6 * time.Second).UnixMilli()}}})
				return
			}
			blockedOnce.Do(func() { close(blockedSync) })
			// Simulate a network stall, not a successful revoke response. No
			// replacement configuration can be responsible for stopping GOST.
			select {
			case <-r.Context().Done():
			case <-releaseHandler:
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer func() {
		// Release the deliberately stalled handler only during teardown, after
		// every lease assertion. Client cancellation alone need not unblock a
		// server handler that is not reading its request body.
		close(releaseHandler)
		control.Close()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	done := make(chan error, 1)
	stateDir := t.TempDir()
	go func() {
		done <- Run(ctx, Config{ServerURL: control.URL, EnrollmentToken: "local-test-enrollment", StateDir: stateDir, GostBinary: binary})
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("runtime did not stop after local test cancellation")
		}
	}()
	var granted time.Time
	select {
	case granted = <-firstSync:
	case err := <-done:
		t.Fatalf("runtime ended before lease: %v", err)
	case <-ctx.Done():
		t.Fatal("local control sync never started")
	}
	var conn net.Conn
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		conn, err = net.DialTimeout("tcp4", listenAddress, 100*time.Millisecond)
		if err == nil {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if conn == nil {
		t.Fatalf("real GOST never bound test listener: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	payload := []byte("existing-connection-before-lease-expiry")
	if _, err = conn.Write(payload); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(payload))
	if _, err = io.ReadFull(conn, got); err != nil || !bytes.Equal(payload, got) {
		t.Fatalf("real forwarding failed before expiry: %v", err)
	}
	select {
	case <-blockedSync:
	case err := <-done:
		t.Fatalf("runtime ended before blocked sync: %v", err)
	case <-ctx.Done():
		t.Fatal("second control sync was not observed")
	}
	// Prove the same connection still works while the control request is
	// blocked; observing an old EOF only after five seconds would not suffice.
	_ = conn.SetDeadline(granted.Add(6 * time.Second))
	if _, err = conn.Write(payload); err != nil {
		t.Fatalf("connection already closed before lease expiry: %v", err)
	}
	if _, err = io.ReadFull(conn, got); err != nil || !bytes.Equal(payload, got) {
		t.Fatalf("connection stopped before its authorized lease expired: %v", err)
	}
	// With all preceding payload drained, killing the GOST process must close
	// this existing connection with EOF. A timeout or merely blocking new
	// connections is not sufficient evidence of lease enforcement.
	_ = conn.SetDeadline(granted.Add(8 * time.Second))
	var one [1]byte
	n, err := conn.Read(one[:])
	if n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("existing connection did not receive EOF at lease expiry: n=%d err=%v", n, err)
	}
	if elapsed := time.Since(granted); elapsed < 5*time.Second || elapsed > 8*time.Second {
		t.Fatalf("unexpected lease enforcement timing: %s", elapsed)
	}
	if next, err := net.DialTimeout("tcp4", listenAddress, 500*time.Millisecond); err == nil {
		next.Close()
		t.Fatal("expired GOST listener still accepts new connections")
	}
	t.Log("real Run/apply/watchdog: blocked control sync, existing TCP EOF, new connection refused after six-second lease")
}
