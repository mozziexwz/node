//go:build linux

package control

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mozziexwz/node/internal/executor"
	"github.com/mozziexwz/node/internal/relayruntime"
)

// Opt-in proof with a real HTTP control backend, the production Agent Run and
// real GOST: a rule waiting for UDP relays TCP only, a passing check reissues
// it, and the Agent then relays UDP on the same port and bills it. Run only in
// a new root loopback-only namespace, with -test.timeout=3m.
func TestRealControlRelayUDPOpensAfterCheck(t *testing.T) {
	if os.Getenv("MSBOOST_REAL_CONTROL_UDP") != "1" {
		t.Skip("opt-in real control/Agent/GOST UDP test")
	}
	interfaces, err := net.Interfaces()
	if os.Geteuid() != 0 || os.Getenv("MSBOOST_TEST_LOOPBACK_NETNS") != "1" || err != nil || len(interfaces) != 1 || interfaces[0].Name != "lo" {
		t.Fatal("requires a new root loopback-only network namespace")
	}
	gost := os.Getenv("GOST_TEST_BINARY")
	if !filepath.IsAbs(gost) {
		t.Fatal("verified absolute GOST binary required")
	}
	dir, err := os.MkdirTemp("", "ru-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	// The target answers TCP and UDP on one port, like a node.
	tcpEcho, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer tcpEcho.Close()
	go func() {
		for {
			c, err := tcpEcho.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	target := tcpEcho.Addr().(*net.TCPAddr).Port
	udpEcho, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: target})
	if err != nil {
		t.Fatal(err)
	}
	defer udpEcho.Close()
	go func() {
		buffer := make([]byte, 2048)
		for {
			n, from, err := udpEcho.ReadFromUDP(buffer)
			if err != nil {
				return
			}
			_, _ = udpEcho.WriteToUDP(buffer[:n], from)
		}
	}()
	entry := freeTCPAndUDPPort(t)
	f := newRelayV2Fixture(t)
	enrollment := commerceID() + commerceID()
	key := f.user.ID + ":route"
	if err := f.app.Store.Update(func(s *State) error {
		DeleteDoc(s, "relay_agents", f.agents[1].ID)
		a := f.agents[0]
		a.TokenHash, a.EnrollmentHash = "", commerceHash(enrollment)
		a.EnrollmentExpires = time.Now().Add(10 * time.Minute).UnixMilli()
		a.PortRanges = []PortRange{{Start: entry, End: entry}}
		if err := SaveDoc(s, "relay_agents", a.ID, a); err != nil {
			return err
		}
		if err := SaveDoc(s, "routes", "route", Route{ID: "route", Type: "port_forward", EntryAgentID: a.ID, Enabled: true, RateMbps: 5}); err != nil {
			return err
		}
		s.Settings["relayVerify"] = RelayVerifySettings{Free: "selftest", Code: "log", Donation: "enforce", RecheckHours: 24, GraceHours: 48, BreakerPercent: 20}
		rule, _ := LoadDoc[UserRule](s, "user_rules", key)
		rule.Segments = rule.Segments[:1]
		rule.EntryAddress, rule.EntryPort = a.Address, entry
		rule.Segments[0].Runtime.ListenPort = entry
		rule.Segments[0].Runtime.Targets = []string{tcpEcho.Addr().String()}
		config, err := executor.RewriteClientConfig([]byte(relayTestConfigUDP), rule.EntryAddress, entry)
		if err != nil {
			return err
		}
		if rule.SealedConfig, err = f.app.Seal(config); err != nil {
			return err
		}
		rule.UDP, rule.Verify = "pending", &RuleVerify{State: "pending"}
		return SaveDoc(s, "user_rules", key, rule)
	}); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: f.app.Authenticate(f.mux), ReadHeaderTimeout: 5 * time.Second}
	go server.Serve(listener)
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	f.app.StartRelay(ctx)
	runtimeDone := make(chan error, 1)
	go func() {
		runtimeDone <- relayruntime.Run(ctx, relayruntime.Config{ServerURL: "http://" + listener.Addr().String(), EnrollmentToken: enrollment, StateDir: dir, GostBinary: gost, OfflinePolicy: relayruntime.KeepLast})
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
	waitSegment := func(udp bool) {
		t.Helper()
		for deadline := time.Now().Add(60 * time.Second); ; time.Sleep(100 * time.Millisecond) {
			ready := false
			_ = f.app.Store.View(func(s *State) error {
				rule, _ := LoadDoc[UserRule](s, "user_rules", key)
				ready = len(rule.Segments) == 1 && rule.Segments[0].Runtime.UDP == udp && relaySegmentReady(s, rule.Segments[0], time.Now().UnixMilli())
				return nil
			})
			if ready {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("segment not ready with udp=%v", udp)
			}
		}
	}
	tcpRoundTrip := func() {
		t.Helper()
		conn, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", itoa(entry)), 3*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		payload := []byte("Mieru-like TCP bytes")
		if _, err := conn.Write(payload); err != nil {
			t.Fatal(err)
		}
		got := make([]byte, len(payload))
		if _, err := io.ReadFull(conn, got); err != nil || !bytes.Equal(got, payload) {
			t.Fatalf("TCP through the Agent: %q %v", got, err)
		}
	}
	waitSegment(false)
	tcpRoundTrip()
	if reply := udpRoundTrip(t, entry, "before the check", 5); reply != "" {
		t.Fatal("UDP was relayed before the target passed a check")
	}
	stub := &stubRouteVerifier{}
	f.app.SetRouteVerifier(stub.verify)
	f.runVerify(t, key)
	if rule := f.rule(t, key); rule.UDP != "on" {
		t.Fatalf("a passing check did not open UDP: %q %+v", rule.UDP, rule.Verify)
	}
	waitSegment(true)
	sent := 0
	for i := 0; i < 20; i++ {
		payload := "Mieru-like UDP datagram " + itoa(i)
		if reply := udpRoundTrip(t, entry, payload, 25); reply != payload {
			t.Fatalf("UDP through the Agent after the check: %q", reply)
		}
		sent += len(payload)
	}
	tcpRoundTrip()
	for deadline := time.Now().Add(60 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		rule := f.rule(t, key)
		if rule.InputBytes >= int64(sent) && rule.OutputBytes >= int64(sent) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("UDP bytes not billed: in=%d out=%d sent=%d", rule.InputBytes, rule.OutputBytes, sent)
		}
	}
}

func itoa(n int) string { return string(appendInt(nil, n)) }

func appendInt(b []byte, n int) []byte {
	if n >= 10 {
		b = appendInt(b, n/10)
	}
	return append(b, byte('0'+n%10))
}

func freeTCPAndUDPPort(t *testing.T) int {
	t.Helper()
	for i := 0; i < 32; i++ {
		tcp, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := tcp.Addr().(*net.TCPAddr).Port
		udp, err := net.ListenUDP("udp", &net.UDPAddr{Port: port})
		tcp.Close()
		if err == nil {
			udp.Close()
			return port
		}
	}
	t.Fatal("no port free for both TCP and UDP")
	return 0
}

// udpRoundTrip retries while the Agent binds asynchronously; "" means no reply.
func udpRoundTrip(t *testing.T, port int, payload string, tries int) string {
	t.Helper()
	conn, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	buffer := make([]byte, 2048)
	for i := 0; i < tries; i++ {
		_, _ = conn.Write([]byte(payload))
		_ = conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		if n, err := conn.Read(buffer); err == nil {
			return string(buffer[:n])
		}
		time.Sleep(50 * time.Millisecond)
	}
	return ""
}
