package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestRelayServiceCredentialNameAndExhaustionDiagnostic(t *testing.T) {
	for _, field := range []string{"DynamicUser=true", "LoadCredential=config.json:${confdir}/config.json", "-C %d/config.json"} {
		if !strings.Contains(relayInstallScript, field) {
			t.Fatalf("missing protected JSON credential contract: %s", field)
		}
	}
	// Execute only the fixed diagnostic functions and post-retry terminal branch,
	// never any install, service, firewall, or cleanup action.
	pos := strings.LastIndex(relayInstallScript, "\ndone\n")
	if pos < 0 {
		t.Fatal("relay retry loop terminator missing")
	}
	tail := relayInstallScript[pos+len("\ndone\n"):]
	if strings.TrimSpace(tail) != "relay_fail service_failed" {
		t.Fatal("retry exhaustion must produce a stable diagnostic, not bare exit")
	}
	bash, err := exec.LookPath("bash")
	if err != nil && runtime.GOOS == "windows" {
		bash = "C:/Program Files/Git/bin/bash.exe"
		_, err = os.Stat(bash)
	}
	if err != nil {
		t.Fatal(err)
	}
	functions, _, _ := strings.Cut(relayStageScript, "\nmanaged=")
	cmd := exec.Command(bash, "-s")
	cmd.Stdin = strings.NewReader("set -Eeuo pipefail\n" + diagnosticPrelude + functions + "\nmsboost_phase=service\n" + tail)
	out, err := cmd.CombinedOutput()
	if err == nil || marker(out, "MSBOOST_ERROR_CODE") != "service_failed" || marker(out, "MSBOOST_ERROR_PHASE") != "service" {
		t.Fatalf("retry exhaustion lost diagnostic: %s %v", out, err)
	}
	d := remoteDiagnostic(err, out, "relay")
	if d.Code != "service_failed" || d.Phase != "service" {
		t.Fatalf("wrong public diagnostic: %+v", d)
	}
}

// Systemd LoadCredential materializes a file under its credential ID. GOST
// v3 chooses the parser from that filename's suffix. Exercise the actual
// checksum-verified GOST, using only disposable credentials and loopback TCP.
func TestRealGostCredentialConfigFileName(t *testing.T) {
	binary := os.Getenv("GOST_TEST_BINARY")
	if binary == "" {
		t.Skip("set GOST_TEST_BINARY to a checksum-verified GOST v3 executable")
	}
	binary, err := filepath.Abs(binary)
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
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	reserve, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reserve.Addr().String()
	reserve.Close()
	config := map[string]any{"services": []any{map[string]any{"name": "msboost-free", "addr": address, "handler": map[string]string{"type": "tcp"}, "listener": map[string]string{"type": "tcp"}, "forwarder": map[string]any{"nodes": []any{map[string]string{"name": "target", "addr": echo.Addr().String()}}}}}}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"config", "config.json"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), name)
			if err := os.WriteFile(path, data, 0400); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, binary, "-C", path)
			if name == "config" {
				out, err := cmd.CombinedOutput()
				var log struct {
					Msg string `json:"msg"`
				}
				if err == nil || ctx.Err() != nil || json.Unmarshal(bytes.TrimSpace(out), &log) != nil || log.Msg != `Unsupported Config Type ""` {
					t.Fatalf("old extensionless regression not reproduced: %s %v", out, err)
				}
				return
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			defer func() { cancel(); <-done }()
			var conn net.Conn
			for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
				conn, err = net.DialTimeout("tcp", address, 100*time.Millisecond)
				if err == nil {
					break
				}
				time.Sleep(30 * time.Millisecond)
			}
			if conn == nil {
				t.Fatalf("JSON-named credential did not start listener: %v", err)
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
			payload := []byte("msboost-credential-filename-regression")
			if _, err := conn.Write(payload); err != nil {
				t.Fatal(err)
			}
			got := make([]byte, len(payload))
			if _, err := io.ReadFull(conn, got); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, payload) {
				t.Fatal("JSON credential listener failed real TCP forwarding")
			}
		})
	}
}
