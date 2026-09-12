package relayruntime

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// Set GOST_TEST_BINARY to a checksum-verified GOST v3 executable. The test
// listens only on loopback and transfers bytes through two real GOST services.
func TestRealGostTwoHopForwardingObserverAndRevocation(t *testing.T) {
	for _, protocol := range []string{"tcp", "tls", "tls-wrong-name", "tls-wrong-ca"} {
		t.Run(protocol, func(t *testing.T) { testRealGostForwarding(t, protocol) })
	}
}
func testRealGostForwarding(t *testing.T, protocol string) {
	binary := os.Getenv("GOST_TEST_BINARY")
	if binary == "" {
		t.Skip("set GOST_TEST_BINARY for real GOST integration")
	}
	echo, err := net.Listen("tcp", "127.0.0.1:0")
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
			go func() { defer conn.Close(); io.Copy(conn, conn) }()
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &runtimeState{cfg: Config{StateDir: t.TempDir(), GostBinary: binary}, processes: map[string]*process{}, pending: map[string]Traffic{}, journal: filepath.Join(t.TempDir(), "journal.json"), observerToken: "test-only-token"}
	observer := httptest.NewServer(httpHandler(s))
	defer observer.Close()
	s.observerURL = observer.URL + "/observer?token=test-only-token"
	reserve := func() int {
		l, e := net.Listen("tcp", "127.0.0.1:0")
		if e != nil {
			t.Fatal(e)
		}
		port := l.Addr().(*net.TCPAddr).Port
		l.Close()
		return port
	}
	exitPort, entryPort := reserve(), reserve()
	rules := []Rule{{ID: "integration-exit", Version: 1, ListenPort: exitPort, Protocol: protocol, RateMbps: 100, Targets: []string{echo.Addr().String()}, AllowedSources: []string{"127.0.0.1"}, Strategy: "round"}, {ID: "integration-entry", Version: 1, ListenPort: entryPort, Protocol: "tcp", RateMbps: 100, Targets: []string{net.JoinHostPort("127.0.0.1", strconv.Itoa(exitPort))}, Strategy: "round", Billing: true}}
	if protocol != "tcp" {
		rules[0].Protocol = "tls"
		cert, key, e := NewTLSIdentity("relay.integration.invalid", time.Now().Add(time.Hour))
		if e != nil {
			t.Fatal(e)
		}
		rules[0].TLSCertificate, rules[0].TLSPrivateKey = cert, key
		rules[1].TargetTLS = []TLSClient{{CA: cert, ServerName: "relay.integration.invalid"}}
		if protocol == "tls-wrong-name" {
			rules[1].TargetTLS[0].ServerName = "another-node.invalid"
		}
		if protocol == "tls-wrong-ca" {
			other, _, e := NewTLSIdentity("relay.integration.invalid", time.Now().Add(time.Hour))
			if e != nil {
				t.Fatal(e)
			}
			rules[1].TargetTLS[0].CA = other
		}
	}
	for _, rule := range rules {
		s.mu.Lock()
		err = s.startLocked(ctx, rule)
		s.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		s.mu.Lock()
		for id := range s.processes {
			s.stopLocked(id)
		}
		s.mu.Unlock()
	}()
	deadline := time.Now().Add(12 * time.Second)
	for {
		ready := true
		s.mu.Lock()
		for _, p := range s.processes {
			if p.ack.State == "failed" {
				t.Errorf("GOST failed: %s", p.ack.Message)
			}
			ready = ready && p.ack.State == "ready"
		}
		s.mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("real GOST did not emit running/bind ACK")
		}
		time.Sleep(30 * time.Millisecond)
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(entryPort)), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	payload := bytes.Repeat([]byte("MSBOOST actual GOST forwarding\n"), 1000)
	writeDone := make(chan error, 1)
	go func() { _, e := conn.Write(payload); writeDone <- e }()
	received := make([]byte, len(payload))
	_, err = io.ReadFull(conn, received)
	conn.Close()
	if protocol == "tls-wrong-name" || protocol == "tls-wrong-ca" {
		if err == nil {
			t.Fatal("GOST accepted untrusted TLS peer")
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if err = <-writeDone; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(payload, received) {
		t.Fatal("two-hop payload was changed")
	}
	deadline = time.Now().Add(8 * time.Second)
	for {
		s.mu.Lock()
		sample := s.processes["integration-entry"].traffic
		s.mu.Unlock()
		if sample.InputBytes >= int64(len(payload)) && sample.OutputBytes >= int64(len(payload)) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("GOST traffic observer missing: %+v", sample)
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.mu.Lock()
	p := s.processes["integration-entry"]
	s.stopLocked("integration-entry")
	s.mu.Unlock()
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		t.Fatal("GOST failed to stop")
	}
	if c, e := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(entryPort)), 500*time.Millisecond); e == nil {
		c.Close()
		t.Fatal("revoked GOST port is still forwarding")
	}
}

type runtimeHTTPHandler struct{ s *runtimeState }

func (h runtimeHTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.s.observer(w, r) }
func httpHandler(s *runtimeState) runtimeHTTPHandler                          { return runtimeHTTPHandler{s} }
