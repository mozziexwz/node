package relayruntime

import (
	"context"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// Set GOST_TEST_BINARY to a checksum-verified GOST v3 executable. Two real
// GOST processes relay UDP on loopback beside a TLS TCP hop: entry guard ->
// GOST -> exit guard (source ACL) -> GOST -> a UDP echo.
func TestRealGostTwoHopUDP(t *testing.T) {
	binary := os.Getenv("GOST_TEST_BINARY")
	if binary == "" {
		t.Skip("set GOST_TEST_BINARY for real GOST integration")
	}
	echo := udpEcho(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &runtimeState{cfg: Config{StateDir: t.TempDir(), GostBinary: binary}, processes: map[string]*process{}, pending: map[string]Traffic{}, journal: filepath.Join(t.TempDir(), "journal.json"), observerToken: "test-only-token"}
	observer := httptest.NewServer(httpHandler(s))
	defer observer.Close()
	s.observerURL = observer.URL + "/observer?token=test-only-token"
	reserve := func() int {
		tcp, e := net.Listen("tcp", "127.0.0.1:0")
		if e != nil {
			t.Fatal(e)
		}
		defer tcp.Close()
		port := tcp.Addr().(*net.TCPAddr).Port
		udp, e := net.ListenUDP("udp", &net.UDPAddr{Port: port})
		if e != nil {
			t.Skip("port not free for UDP too")
		}
		udp.Close()
		return port
	}
	exitPort, entryPort := reserve(), reserve()
	cert, key, err := NewTLSIdentity("relay.integration.invalid", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	rules := []Rule{
		{ID: "integration-udp-exit", Version: 1, ListenPort: exitPort, Protocol: "tls", TLSCertificate: cert, TLSPrivateKey: key, RateMbps: 100, Targets: []string{echo.LocalAddr().String()}, AllowedSources: []string{"127.0.0.1"}, Strategy: "round", UDP: true},
		{ID: "integration-udp-entry", Version: 1, ListenPort: entryPort, Protocol: "tcp", RateMbps: 100, Targets: []string{net.JoinHostPort("127.0.0.1", strconv.Itoa(exitPort))}, TargetTLS: []TLSClient{{CA: cert, ServerName: "relay.integration.invalid"}}, Strategy: "round", Billing: true, UDP: true},
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
	for i := 0; i < 50; i++ {
		payload := "mieru-like datagram " + strconv.Itoa(i)
		if reply := exchange(t, entryPort, payload, 25); reply != "echo:"+payload {
			t.Fatalf("UDP was not relayed over two real GOST hops: %q", reply)
		}
	}
	deadline := time.Now().Add(8 * time.Second)
	for {
		s.mu.Lock()
		p := s.processes["integration-udp-entry"]
		traffic, udpBytes, ack := p.traffic, p.serviceBytes[1], p.ack.State
		s.mu.Unlock()
		if traffic.InputBytes > 0 && traffic.OutputBytes > 0 && udpBytes[0] == traffic.InputBytes && ack == "ready" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("UDP traffic missing from the entry's totals: %+v udp=%v ack=%s", traffic, udpBytes, ack)
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.mu.Lock()
	p := s.processes["integration-udp-entry"]
	s.stopLocked("integration-udp-entry")
	s.mu.Unlock()
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		t.Fatal("GOST failed to stop")
	}
	if reply := exchange(t, entryPort, "after revoke", 3); reply != "" {
		t.Fatal("revoked rule still relays UDP")
	}
}
