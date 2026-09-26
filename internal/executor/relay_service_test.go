package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRelayServiceCredentialNameAndExhaustionDiagnostic(t *testing.T) {
	for _, field := range []string{"DynamicUser=true", "LoadCredential=config.json:${confdir}/config.json", "LoadCredential=guard.json:${confdir}/guard.json", "LoadCredential=guard.py:${confdir}/guard.py", "ExecStart=/usr/bin/python3 \\${CREDENTIALS_DIRECTORY}/guard.py", "KillMode=control-group", "'addr':'127.0.0.1:'+b"} {
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
	syntax := exec.Command(bash, "-n")
	syntax.Stdin = strings.NewReader(relayInstallScript)
	if out, err := syntax.CombinedOutput(); err != nil {
		t.Fatalf("generated free relay installer has invalid shell syntax: %s %v", out, err)
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

func TestEmbeddedFreeRelayGuardPython(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Linux Python and socket semantics exercised in CI")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is required on deployed Debian and in Linux CI")
	}
	cmd := exec.Command(python, "free_relay_guard_test.py")
	cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("embedded free relay guard protocol tests: %s %v", out, err)
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

// The standalone free tool does not run Relay Agent. Exercise its embedded
// guard with the checksum-verified GOST and real TCP sockets, including both
// SOCKS families, normal forwarding, and child/service shutdown.
func TestRealFreeRelayGuard(t *testing.T) {
	binary := os.Getenv("GOST_TEST_BINARY")
	if binary == "" || runtime.GOOS != "linux" {
		t.Skip("set GOST_TEST_BINARY on Linux for real free relay guard")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	echo, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	var accepted atomic.Int64
	go func() {
		for {
			conn, err := echo.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			go func() { defer conn.Close(); _, _ = io.Copy(conn, conn) }()
		}
	}()
	reservePort := func() int {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		return listener.Addr().(*net.TCPAddr).Port
	}
	publicPort, backendPort := reservePort(), reservePort()
	for backendPort == publicPort {
		backendPort = reservePort()
	}
	tmp := t.TempDir()
	guardPath := filepath.Join(tmp, "guard.py")
	configPath := filepath.Join(tmp, "config.json")
	policyPath := filepath.Join(tmp, "guard.json")
	config := map[string]any{"services": []any{map[string]any{"name": "msboost-free", "addr": net.JoinHostPort("127.0.0.1", strconv.Itoa(backendPort)), "handler": map[string]string{"type": "tcp"}, "listener": map[string]string{"type": "tcp"}, "admission": "guard", "forwarder": map[string]any{"nodes": []any{map[string]string{"name": "target", "addr": echo.Addr().String()}}}}}, "admissions": []any{map[string]any{"name": "guard", "whitelist": true, "matchers": []string{"127.0.0.1"}}}}
	configJSON, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	policyJSON, err := json.Marshal(map[string]int{"publicPort": publicPort, "backendPort": backendPort})
	if err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string][]byte{guardPath: []byte(freeRelayGuardPython), configPath: configJSON, policyPath: policyJSON} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, guardPath, binary, configPath, policyPath)
	var logs bytes.Buffer
	cmd.Stderr = &logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait(); close(done) }()
	defer func() {
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			cancel()
			<-done
		}
	}()
	publicAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(publicPort))
	var probe net.Conn
	for deadline := time.Now().Add(8 * time.Second); time.Now().Before(deadline); {
		probe, err = net.DialTimeout("tcp", publicAddr, 100*time.Millisecond)
		if err == nil {
			_ = probe.Close()
			break
		}
		select {
		case e := <-done:
			t.Fatalf("free relay guard exited early: %v %s", e, logs.String())
		default:
		}
		time.Sleep(30 * time.Millisecond)
	}
	if probe == nil {
		t.Fatalf("free relay guard did not listen: %v %s", err, logs.String())
	}
	time.Sleep(100 * time.Millisecond)
	before := accepted.Load()
	for _, greeting := range [][]byte{{0x05, 0x02, 0x7f, 0x00}, {0x04, 0x01, 0x00, 0x50, 127, 0, 0, 1}} {
		client, err := net.DialTimeout("tcp", publicAddr, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		_ = client.SetDeadline(time.Now().Add(time.Second))
		if _, err := client.Write(greeting); err != nil {
			t.Fatal(err)
		}
		n, err := client.Read(make([]byte, 1))
		_ = client.Close()
		if n != 0 || err == nil || isTimeout(err) {
			t.Fatalf("SOCKS greeting was not closed: %x, n=%d, err=%v", greeting, n, err)
		}
	}
	time.Sleep(100 * time.Millisecond)
	if accepted.Load() != before {
		t.Fatalf("blocked greetings reached target: before=%d after=%d", before, accepted.Load())
	}
	client, err := net.DialTimeout("tcp", publicAddr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(3 * time.Second))
	payload := []byte("Mieru-like normal game bytes")
	if _, err := client.Write(payload); err != nil {
		t.Fatal(err)
	}
	received := make([]byte, len(payload))
	if _, err := io.ReadFull(client, received); err != nil || !bytes.Equal(received, payload) {
		t.Fatalf("normal game forwarding failed: %x %v", received, err)
	}
}

func isTimeout(err error) bool {
	var timeout net.Error
	return errors.As(err, &timeout) && timeout.Timeout()
}
