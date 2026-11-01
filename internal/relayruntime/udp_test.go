package relayruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRuntimeHashKeepsRulesWithoutUDP(t *testing.T) {
	rule := Rule{ID: "rule-identity-unique", Version: 1, ListenPort: 21000, Protocol: "tcp", RateMbps: 5, Targets: []string{"1.1.1.1:20067", "1.0.0.1:20067"}, AllowedSources: []string{"8.8.8.8", "8.8.4.4"}, Strategy: "fifo"}
	hash, err := RuntimeHash(rule)
	if err != nil {
		t.Fatal(err)
	}
	// The canonical form from before UDP, field for field.
	old := struct {
		ListenPort                    int
		Targets, AllowedSources       []string
		Strategy, Protocol            string
		RateMbps                      int64
		TLSCertificate, TLSPrivateKey string
		TargetTLS                     []TLSClient
	}{rule.ListenPort, rule.Targets, []string{"8.8.4.4", "8.8.8.8"}, rule.Strategy, rule.Protocol, rule.RateMbps, "", "", nil}
	raw, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if hash != hex.EncodeToString(sum[:]) {
		t.Fatal("a rule without UDP changed its runtime hash and would be reissued")
	}
	rule.UDP = true
	if withUDP, err := RuntimeHash(rule); err != nil || withUDP == hash {
		t.Fatal("turning UDP on must change the runtime hash so the Agent restarts GOST")
	}
}

type gostTestConfig struct {
	Services []struct {
		Name, Addr, Limiter, Observer, Admission string
		Handler                                  struct{ Type, Chain string }
		Listener                                 struct {
			Type     string
			Metadata map[string]any
		}
		Forwarder *struct{ Nodes []map[string]any }
	}
	Limiters []struct {
		Name   string
		Limits []string
	}
}

func TestGostConfigForwardsUDPBesideTCP(t *testing.T) {
	rule := Rule{ID: "rule-identity-unique", ListenPort: 21000, Protocol: "tcp", RateMbps: 5, Targets: []string{"1.1.1.1:20067"}, AllowedSources: []string{"127.0.0.1"}, Strategy: "round", TargetTLS: []TLSClient{{CA: "pinned", ServerName: "next-hop"}}, UDP: true}
	raw, err := gostConfigAt(rule, "http://127.0.0.1/observer", gostTLSFiles{cas: []string{"ca-file"}}, "127.0.0.1:30001", "127.0.0.1:30002")
	if err != nil {
		t.Fatal(err)
	}
	var cfg gostTestConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Services) != 2 {
		t.Fatalf("want a TCP and a UDP service, got %d", len(cfg.Services))
	}
	tcp, udp := cfg.Services[0], cfg.Services[1]
	if tcp.Addr != "127.0.0.1:30001" || tcp.Handler.Chain != "next" || tcp.Limiter != "rate" {
		t.Fatalf("the TCP service changed: %+v", tcp)
	}
	if udp.Name != rule.ID+"/udp" || udp.Addr != "127.0.0.1:30002" || udp.Handler.Type != "udp" || udp.Listener.Type != "udp" {
		t.Fatalf("unexpected UDP service: %+v", udp)
	}
	if udp.Listener.Metadata["keepalive"] != true || udp.Listener.Metadata["ttl"] != "30s" {
		t.Fatal("without keepalive GOST closes the channel after one reply and Mieru stops")
	}
	if udp.Forwarder == nil || len(udp.Forwarder.Nodes) != 1 || udp.Forwarder.Nodes[0]["addr"] != "1.1.1.1:20067" || udp.Forwarder.Nodes[0]["dialer"] != nil || udp.Forwarder.Nodes[0]["connector"] != nil {
		t.Fatalf("UDP must go in plain UDP to the same target, even beside a TLS hop: %+v", udp.Forwarder)
	}
	if udp.Admission != "upstream" || udp.Observer != "observer" || udp.Limiter != "rate-udp" {
		t.Fatalf("UDP needs the same admission and observer and its own limiter: %+v", udp)
	}
	limits := map[string]string{}
	for _, limiter := range cfg.Limiters {
		limits[limiter.Name] = strings.Join(limiter.Limits, ",")
	}
	if limits["rate"] != "$ 625000B 625000B" || limits["rate-udp"] != "$ 625000B 625000B" {
		t.Fatalf("both limiters carry the rule's rate: %v", limits)
	}
	rule.UDP = false
	if raw, err = gostConfigAt(rule, "http://127.0.0.1/observer", gostTLSFiles{cas: []string{"ca-file"}}, "127.0.0.1:30001", "127.0.0.1:30002"); err != nil || strings.Contains(string(raw), "udp") {
		t.Fatal("a rule without UDP must not get a UDP service")
	}
}

func TestObserverBillsTCPAndUDPTogether(t *testing.T) {
	s, p, _ := trafficV2Fixture(t)
	p.rule.UDP, p.startedAt, s.observerToken = true, time.Now().UnixMilli(), "token"
	post := func(body string) {
		t.Helper()
		w := httptest.NewRecorder()
		s.observer(w, httptest.NewRequest("POST", "http://127.0.0.1/observer?token=token&epoch="+p.epoch, strings.NewReader(body)))
		if w.Code != 200 {
			t.Fatalf("observer %d %s", w.Code, w.Body.String())
		}
	}
	stats := func(service string, input, output, connections int) string {
		return fmt.Sprintf(`{"events":[{"kind":"service","service":%q,"type":"stats","stats":{"inputBytes":%d,"outputBytes":%d,"currentConns":%d}}]}`, service, input, output, connections)
	}
	post(stats(p.rule.ID, 100, 1000, 2))
	post(stats(p.rule.ID+"/udp", 50, 500, 0))
	post(stats(p.rule.ID+"/udp", 50, 500, 0)) // a repeated cumulative report
	if p.traffic.InputBytes != 150 || p.traffic.OutputBytes != 1500 || p.traffic.Connections != 2 {
		t.Fatalf("process totals must add both services: %+v", p.traffic)
	}
	if batch := s.v2TrafficBatchLocked(); len(batch) != 1 || batch[0].InputBytes != 150 || batch[0].OutputBytes != 1500 {
		t.Fatalf("UDP traffic must be billed with TCP: %+v", batch)
	}
	p.ack.State = "pending"
	post(`{"events":[{"kind":"service","service":"` + p.rule.ID + `/udp","type":"status","status":{"state":"failed"}}]}`)
	if p.ack.State != "pending" {
		t.Fatal("the UDP service must not decide the rule's state")
	}
	p.rule.UDP = false
	post(stats(p.rule.ID+"/udp", 900, 900, 0))
	if p.traffic.InputBytes != 150 {
		t.Fatal("a UDP service is ignored for a rule without UDP")
	}
}

func udpEcho(t *testing.T) *net.UDPConn {
	t.Helper()
	backend, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	go func() {
		buffer := make([]byte, 2048)
		for {
			n, from, err := backend.ReadFromUDP(buffer)
			if err != nil {
				return
			}
			_, _ = backend.WriteToUDP(append([]byte("echo:"), buffer[:n]...), from)
		}
	}()
	return backend
}

func freeUDPPort(t *testing.T) int {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{})
	if err != nil {
		t.Fatal(err)
	}
	port := conn.LocalAddr().(*net.UDPAddr).Port
	_ = conn.Close()
	return port
}

func serveUDPGuard(t *testing.T, rule Rule, backend string) *udpGuard {
	t.Helper()
	guard, err := newUDPGuard(rule, backend)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	guard.serve(ctx)
	t.Cleanup(func() {
		cancel()
		guard.wait()
	})
	return guard
}

// exchange retries while the guard binds asynchronously; "" means no reply.
// Until then Linux answers with ICMP and the next read fails at once, so each
// failed try waits before the next.
func exchange(t *testing.T, port int, payload string, tries int) string {
	t.Helper()
	client, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	buffer := make([]byte, 2048)
	for i := 0; i < tries; i++ {
		if _, err := client.Write([]byte(payload)); err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		_ = client.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		if n, err := client.Read(buffer); err == nil {
			return string(buffer[:n])
		}
		time.Sleep(50 * time.Millisecond)
	}
	return ""
}

func guardSessions(g *udpGuard) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.sessions)
}

func TestUDPGuardRelaysToLoopbackAndExpiresIdleClients(t *testing.T) {
	backend := udpEcho(t)
	port := freeUDPPort(t)
	guard := serveUDPGuard(t, Rule{ListenPort: port}, backend.LocalAddr().String())
	if reply := exchange(t, port, "hello", 20); reply != "echo:hello" {
		t.Fatalf("no relay through the guard: %q", reply)
	}
	if n := guardSessions(guard); n != 1 {
		t.Fatalf("one client must be one session, got %d", n)
	}
	guard.sweep(time.Now().Add(udpGuardIdle + time.Second))
	if n := guardSessions(guard); n != 0 {
		t.Fatalf("idle session kept: %d", n)
	}
	if reply := exchange(t, port, "again", 20); reply != "echo:again" {
		t.Fatalf("a returning client must get a new session: %q", reply)
	}
}

func TestUDPGuardDropsUnlistedSources(t *testing.T) {
	backend := udpEcho(t)
	port := freeUDPPort(t)
	guard := serveUDPGuard(t, Rule{ListenPort: port, AllowedSources: []string{"192.0.2.1"}}, backend.LocalAddr().String())
	if reply := exchange(t, port, "hello", 5); reply != "" {
		t.Fatalf("an unlisted source reached the target: %q", reply)
	}
	if n := guardSessions(guard); n != 0 {
		t.Fatalf("an unlisted source opened a session: %d", n)
	}
}

func TestUDPGuardWaitsForABusyPort(t *testing.T) {
	retry := udpGuardRetry
	udpGuardRetry = 50 * time.Millisecond
	defer func() { udpGuardRetry = retry }()
	backend := udpEcho(t)
	busy, err := net.ListenUDP("udp", &net.UDPAddr{})
	if err != nil {
		t.Fatal(err)
	}
	port := busy.LocalAddr().(*net.UDPAddr).Port
	guard := serveUDPGuard(t, Rule{ListenPort: port}, backend.LocalAddr().String())
	time.Sleep(200 * time.Millisecond)
	guard.mu.Lock()
	bound := guard.conn != nil
	guard.mu.Unlock()
	if bound {
		t.Fatal("bound a port held by another socket")
	}
	_ = busy.Close()
	if reply := exchange(t, port, "late", 20); reply != "echo:late" {
		t.Fatalf("the guard must bind once the port is free: %q", reply)
	}
}

func TestUDPGuardBackendIsLoopbackOnly(t *testing.T) {
	if _, err := newUDPGuard(Rule{ListenPort: 21000}, "192.0.2.1:30000"); err == nil {
		t.Fatal("the guard must only feed a loopback GOST port")
	}
	conn, err := reserveUDPBackend(21000)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if addr := conn.LocalAddr().(*net.UDPAddr); !addr.IP.IsLoopback() || addr.Port == 21000 {
		t.Fatalf("unexpected private UDP port %v", addr)
	}
}
