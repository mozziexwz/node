package relayruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"strconv"
	"testing"
	"time"
)

func TestScreenSOCKSFragmentedHandshakesAndGameTraffic(t *testing.T) {
	tests := []struct {
		name    string
		parts   [][]byte
		blocked bool
	}{
		{"socks5 no authentication", [][]byte{{0x05}, {0x01}, {0x00}}, true},
		{"socks5 password", [][]byte{{0x05, 0x02}, {0x00}, {0x02}}, true},
		{"socks5 split methods", [][]byte{{0x05, 0x04, 0x31}, {0x37, 0x00, 0x02}}, true},
		{"socks4 connect", [][]byte{{0x04}, {0x01, 0x00, 0x50}, {127, 0, 0, 1}, {0x00}}, true},
		{"socks4 bind", [][]byte{{0x04, 0x02, 0x01, 0xbb, 127, 0, 0, 1, 'u'}, {0x00}}, true},
		{"encrypted-like bytes", [][]byte{{0x05, 0x02, 0x31, 0x42}, {0x93, 0x18}}, false},
		{"large random Mieru-like prefix", [][]byte{{0x05, 0xc8}, bytes.Repeat([]byte{0x00, 0x02, 0x31}, 70)}, false},
		{"ordinary game bytes", [][]byte{{0x13}, {0x37, 0x29}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, server := net.Pipe()
			defer client.Close()
			defer server.Close()
			all := bytes.Join(tt.parts, nil)
			go func() {
				for _, part := range tt.parts {
					if _, err := client.Write(part); err != nil {
						return
					}
				}
				_ = client.Close()
			}()
			prefix, blocked, err := screenSOCKS(server)
			if err != nil || blocked != tt.blocked {
				t.Fatalf("screen result: blocked=%t, err=%v", blocked, err)
			}
			if !blocked {
				rest, err := io.ReadAll(server)
				if err != nil || !bytes.Equal(append(prefix, rest...), all) {
					t.Fatalf("normal payload changed: prefix=%x rest=%x err=%v", prefix, rest, err)
				}
			}
		})
	}
}

func TestSocksGuardBlocksBeforeLoopbackTarget(t *testing.T) {
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	accepted := make(chan struct{}, 4)
	go func() {
		for {
			conn, err := backend.Accept()
			if err != nil {
				return
			}
			accepted <- struct{}{}
			go func() { defer conn.Close(); _, _ = io.Copy(conn, conn) }()
		}
	}()
	portListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := portListener.Addr().(*net.TCPAddr).Port
	_ = portListener.Close()
	g, err := newSocksGuard(Rule{ListenPort: port, AllowedSources: []string{"127.0.0.1"}}, backend.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g.serve(ctx)
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	blocked, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = blocked.SetDeadline(time.Now().Add(time.Second))
	if _, err := blocked.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		t.Fatal(err)
	}
	if n, err := blocked.Read(make([]byte, 1)); n != 0 || err == nil {
		t.Fatalf("SOCKS greeting was not closed: n=%d err=%v", n, err)
	}
	_ = blocked.Close()
	select {
	case <-accepted:
		t.Fatal("blocked SOCKS handshake reached backend")
	case <-time.After(50 * time.Millisecond):
	}
	game, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer game.Close()
	_ = game.SetDeadline(time.Now().Add(time.Second))
	payload := []byte("game traffic")
	if _, err := game.Write(payload); err != nil {
		t.Fatal(err)
	}
	received := make([]byte, len(payload))
	if _, err := io.ReadFull(game, received); err != nil || !bytes.Equal(received, payload) {
		t.Fatalf("normal forwarding failed: %x %v", received, err)
	}
	cancel()
	g.wait()
	if conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Fatal("public guard remained after stop")
	}
}

func TestSocksGuardRejectsInvalidSourceACL(t *testing.T) {
	if _, err := parseGuardSources([]string{"not-an-ip"}); err == nil {
		t.Fatal("invalid ACL accepted")
	}
}

func TestPreparedRelayGOSTBindsOnlyToLoopback(t *testing.T) {
	s := &runtimeState{cfg: Config{StateDir: t.TempDir()}, observerURL: "http://127.0.0.1:9876/observer?token=test"}
	rule := Rule{ID: "private-backend", ListenPort: 24888, Protocol: "tcp", RateMbps: 10, Targets: []string{"127.0.0.1:12345"}, AllowedSources: []string{"192.0.2.17"}, Strategy: "round"}
	prepared, err := s.prepareProcess(rule)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.cleanup()
	raw, err := os.ReadFile(prepared.path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Services []struct {
			Addr string `json:"addr"`
		} `json:"services"`
		Admissions []struct {
			Matchers []string `json:"matchers"`
		} `json:"admissions"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Services) != 1 || cfg.Services[0].Addr != prepared.backendAddr || len(cfg.Admissions) != 1 || len(cfg.Admissions[0].Matchers) != 1 || cfg.Admissions[0].Matchers[0] != "127.0.0.1" {
		t.Fatalf("GOST was not loopback-only: %s", raw)
	}
	if _, err := os.Stat(prepared.path); err != nil {
		t.Fatal(err)
	}
	if _, err := s.prepareProcess(Rule{ID: "bad-acl", ListenPort: 24889, Protocol: "tcp", RateMbps: 10, Targets: []string{"127.0.0.1:12345"}, AllowedSources: []string{"not-an-ip"}, Strategy: "round"}); err == nil {
		t.Fatal("invalid ACL was not rejected during preparation")
	}
}
