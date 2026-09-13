package deploy_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// Snapshot from both official lists, verified 2026-09-13. Changes must be
// reviewed together with Caddyfile; no live trust-list download at startup.
const cloudflareRanges = "103.21.244.0/22 103.22.200.0/22 103.31.4.0/22 104.16.0.0/13 104.24.0.0/14 108.162.192.0/18 131.0.72.0/22 141.101.64.0/18 162.158.0.0/15 172.64.0.0/13 173.245.48.0/20 188.114.96.0/20 190.93.240.0/20 197.234.240.0/22 198.41.128.0/17 2400:cb00::/32 2606:4700::/32 2803:f800::/32 2405:b500::/32 2405:8100::/32 2a06:98c0::/29 2c0f:f248::/32"

func productionCaddyfile(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("Caddyfile")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCaddyCloudflareTrustBoundary(t *testing.T) {
	config := productionCaddyfile(t)
	var trust []string
	for _, line := range strings.Split(config, "\n") {
		parts := strings.Fields(line)
		if len(parts) > 1 && parts[0] == "trusted_proxies" {
			if parts[1] != "static" || trust != nil {
				t.Fatal("only one static allowlist is permitted")
			}
			trust = parts[2:]
		}
	}
	want := strings.Fields(cloudflareRanges)
	slices.Sort(trust)
	slices.Sort(want)
	if !reflect.DeepEqual(trust, want) {
		t.Fatalf("Cloudflare allowlist changed: got %v", trust)
	}
	for _, raw := range trust {
		prefix, err := netip.ParsePrefix(raw)
		if err != nil || prefix.Bits() == 0 || prefix.Addr().IsPrivate() || prefix.Addr().IsLoopback() {
			t.Fatalf("unsafe trusted proxy range %q", raw)
		}
	}
	for _, directive := range []string{"trusted_proxies_strict", "client_ip_headers CF-Connecting-IP X-Forwarded-For", "header_up X-Forwarded-For {client_ip}", "header_up -CF-Connecting-IP", "header_up -Forwarded", "header_up -X-Real-IP", "header_up -True-Client-IP"} {
		if !strings.Contains(config, directive) {
			t.Fatalf("missing %s", directive)
		}
	}
	compose, err := os.ReadFile("compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(compose, []byte("TRUSTED_PROXY_CIDRS: 172.30.86.2/32")) || !bytes.Contains(compose, []byte("ipv4_address: 172.30.86.2")) {
		t.Fatal("app must trust precisely the Caddy peer, not Cloudflare or all Docker addresses")
	}
}

// Runs the actual Caddy executable, not a reimplementation of its header
// semantics. Only loopback listeners and disposable files are used. The second
// fixture replaces the reviewed CF list with loopback to simulate a trusted
// edge without assigning any public Cloudflare address or contacting Cloudflare.
func TestRealCaddyClientIP(t *testing.T) {
	binary := os.Getenv("CADDY_TEST_BINARY")
	if binary == "" {
		t.Skip("set CADDY_TEST_BINARY to a verified Caddy >= 2.8 executable")
	}
	binary, err := filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	version, err := exec.Command(binary, "version").CombinedOutput()
	if err != nil {
		t.Fatalf("Caddy version: %v: %s", err, version)
	}
	t.Logf("Caddy: %s", strings.TrimSpace(string(version)))
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(r.Header)
	}))
	defer backend.Close()
	for _, trusted := range []bool{false, true} {
		name := "production_untrusted_direct"
		if trusted {
			name = "isolated_trusted_edge_simulation"
		}
		t.Run(name, func(t *testing.T) {
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			address := listener.Addr().String()
			_ = listener.Close()
			config := productionCaddyfile(t)
			config = strings.Replace(config, "{$MSBOOST_SITE_ADDRESS}", "http://"+address, 1)
			config = strings.Replace(config, "server:8080", backend.URL, 1)
			config = strings.Replace(config, "{", "{\n    admin off\n    persist_config off", 1)
			if trusted {
				config = strings.Replace(config, cloudflareRanges, "127.0.0.1/32", 1)
			}
			configPath := filepath.Join(t.TempDir(), "Caddyfile")
			if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
				t.Fatal(err)
			}
			if output, err := exec.Command(binary, "validate", "--adapter", "caddyfile", "--config", configPath).CombinedOutput(); err != nil {
				t.Fatalf("invalid Caddy syntax: %v: %s", err, output)
			}
			var output bytes.Buffer
			cmd := exec.Command(binary, "run", "--adapter", "caddyfile", "--config", configPath)
			cmd.Stdout = &output
			cmd.Stderr = &output
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
			transport := &http.Transport{Proxy: nil}
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
			base := "http://" + address
			ready := false
			for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
				res, err := client.Get(base)
				if err == nil {
					_, _ = io.Copy(io.Discard, res.Body)
					res.Body.Close()
					if res.StatusCode == 200 {
						ready = true
						break
					}
				}
				time.Sleep(50 * time.Millisecond)
			}
			if !ready {
				t.Fatal("Caddy failed to serve loopback test listener")
			}
			for _, tc := range []struct{ name, cf, xff, want string }{
				{"no_forwarding_headers", "", "", "127.0.0.1"},
				{"cf_overrides_forged_xff", "198.51.100.72", "192.0.2.66, 203.0.113.90", "198.51.100.72"},
				{"cf_ipv6", "2001:db8::42", "192.0.2.66", "2001:db8::42"},
				{"xff_rightmost_not_spoofed_leftmost", "", "192.0.2.66, 203.0.113.90", "203.0.113.90"},
				{"invalid_headers_fall_back_to_peer", "not-an-ip", "not-an-ip", "127.0.0.1"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					req, _ := http.NewRequest(http.MethodGet, base, nil)
					if tc.cf != "" {
						req.Header.Set("CF-Connecting-IP", tc.cf)
					}
					if tc.xff != "" {
						req.Header.Set("X-Forwarded-For", tc.xff)
					}
					for _, h := range []string{"X-Real-IP", "True-Client-IP", "CF-Connecting-IPv6", "Forwarded"} {
						req.Header.Set(h, "192.0.2.66")
					}
					res, err := client.Do(req)
					if err != nil {
						t.Fatal(err)
					}
					defer res.Body.Close()
					var headers http.Header
					if err := json.NewDecoder(res.Body).Decode(&headers); err != nil {
						t.Fatal(err)
					}
					want := tc.want
					if !trusted {
						want = "127.0.0.1"
					}
					if got := headers.Values("X-Forwarded-For"); len(got) != 1 || got[0] != want {
						t.Fatalf("XFF=%v, want exactly [%s]", got, want)
					}
					for _, h := range []string{"CF-Connecting-IP", "CF-Connecting-IPv6", "X-Real-IP", "True-Client-IP", "Forwarded"} {
						if headers.Get(h) != "" {
							t.Errorf("unvalidated identity header %s reached backend", h)
						}
					}
				})
			}
		})
	}
}
