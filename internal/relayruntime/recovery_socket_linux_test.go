//go:build linux

package relayruntime

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func socketRecoveryTestFixture(t *testing.T) (*runtimeState, context.Context) {
	t.Helper()
	s, ctx, _ := v2TestFixture(t)
	// Linux sockaddr_un is limited to 108 bytes. Test names and an isolated
	// runner's descriptive TMPDIR must not become a fake runtime failure.
	parent, err := os.MkdirTemp("", "rs-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(parent) })
	dir := filepath.Join(parent, "s")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	s.cfg.StateDir, s.v2.path = dir, filepath.Join(dir, v2StateFile)
	return s, ctx
}

func TestV2RecoveryLinuxSocketRootPeerAndPrivateFiles(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("protected recovery client requires isolated Linux root")
	}
	s, ctx := socketRecoveryTestFixture(t)
	closeSocket, err := serveV2Recovery(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	defer closeSocket()
	var snapshot V2RecoverySnapshot
	if err := callRecoverySocket(ctx, s.cfg.StateDir, "snapshot", struct{}{}, &snapshot); err != nil {
		t.Fatal(err)
	}
	if !s.v2.disk.RecoveryRequired || snapshot.AgentID != s.v2.disk.AgentID {
		t.Fatal("authenticated root snapshot did not freeze original Agent")
	}
	plan := recoveryTestPlan(s, snapshot)
	var result V2RecoveryResult
	if err := callRecoverySocket(ctx, s.cfg.StateDir, "adopt", plan, &result); err != nil {
		t.Fatal(err)
	}
	if result.PlanID != plan.PlanID || s.v2.token != plan.Token {
		t.Fatal("root Unix socket did not hot-reload credentials")
	}
	path := filepath.Join(s.cfg.StateDir, v2RecoverySocketName)
	if err := os.Chmod(path, 0666); err != nil {
		t.Fatal(err)
	}
	if err := callRecoverySocket(ctx, s.cfg.StateDir, "snapshot", struct{}{}, &snapshot); err == nil {
		t.Fatal("root client trusted an insecure socket")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	inputPath := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(inputPath, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := RunV2RecoveryClient(ctx, s.cfg.StateDir, "adopt", inputPath); err == nil {
		t.Fatal("root client accepted a non-private plan")
	}
}

func TestV2RecoveryLinuxPeerChild(t *testing.T) {
	path := os.Getenv("MSBOOST_RECOVERY_UNPRIVILEGED_SOCKET")
	if path == "" {
		return
	}
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		os.Exit(3)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	_, _ = fmt.Fprint(conn, "POST /snapshot HTTP/1.1\r\nHost: local.invalid\r\nContent-Length: 2\r\n\r\n{}")
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err == nil {
		response.Body.Close()
		os.Exit(2)
	}
	os.Exit(0)
}

func TestV2RecoveryLinuxSOPEERCREDRejectsNonRootEvenWithOpenSocketMode(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("peer privilege test requires isolated Linux root")
	}
	s, ctx := socketRecoveryTestFixture(t)
	closeSocket, err := serveV2Recovery(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	defer closeSocket()
	parent := filepath.Dir(s.cfg.StateDir)
	if err := os.Chmod(parent, 0711); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(parent, 0700)
	if err := os.Chmod(s.cfg.StateDir, 0711); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(s.cfg.StateDir, 0700)
	socket := filepath.Join(s.cfg.StateDir, v2RecoverySocketName)
	if err := os.Chmod(socket, 0666); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(socket, 0600)
	// The test binary produced by `go test` can itself live beneath a 0700
	// directory. Copy only this test executable into our traversable temp dir,
	// so a permission failure cannot masquerade as a SO_PEERCRED rejection.
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	childBinary := filepath.Join(parent, "unprivileged-recovery-test")
	if err := os.WriteFile(childBinary, data, 0755); err != nil {
		t.Fatal(err)
	}
	// The isolated harness deliberately inherits umask 077. Make only this
	// disposable test executable traversable by the non-root peer; otherwise
	// EACCES during exec would prevent reaching the SO_PEERCRED assertion.
	if err := os.Chmod(childBinary, 0755); err != nil {
		t.Fatal(err)
	}
	childCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	child := exec.CommandContext(childCtx, childBinary, "-test.run=^TestV2RecoveryLinuxPeerChild$")
	child.Env = append(os.Environ(), "MSBOOST_RECOVERY_UNPRIVILEGED_SOCKET="+socket)
	child.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 65534, Gid: 65534, NoSetGroups: true}}
	output, err := child.CombinedOutput()
	if errors.Is(err, syscall.EPERM) {
		t.Skip("isolated container lacks CAP_SETUID/CAP_SETGID for peer test")
	}
	if err != nil {
		t.Fatalf("non-root peer was accepted or did not reach the socket: %v %s", err, output)
	}
	if s.v2.disk.RecoveryRequired {
		t.Fatal("non-root peer invoked recovery snapshot")
	}
}

func TestV2RecoveryLinuxSocketDoesNotReplaceLiveOrUntrustedEndpoints(t *testing.T) {
	s, ctx := socketRecoveryTestFixture(t)
	path := filepath.Join(s.cfg.StateDir, v2RecoverySocketName)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if closeSocket, err := serveV2Recovery(ctx, s); err == nil {
		closeSocket()
		t.Fatal("replaced a live local recovery listener")
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	closeSocket, err := serveV2Recovery(ctx, s)
	if err != nil {
		t.Fatalf("verified stale owned socket was not recovered: %v", err)
	}
	closeSocket()
	if err := os.WriteFile(path, []byte("not a socket"), 0600); err != nil {
		t.Fatal(err)
	}
	if closeSocket, err := serveV2Recovery(ctx, s); err == nil {
		closeSocket()
		t.Fatal("replaced an unrelated regular file")
	}
	if raw, err := os.ReadFile(path); err != nil || string(raw) != "not a socket" {
		t.Fatal("untrusted endpoint file was modified")
	}
	link := filepath.Join(t.TempDir(), "arbitrary-state-link")
	if err := os.Symlink(s.cfg.StateDir, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolveRecoveryClientDirectory(link); err == nil {
		t.Fatal("client followed an arbitrary state-directory symlink")
	}
}

func TestV2RecoveryLinuxLocalJSONRejectsUnknownAndDuplicateFields(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("protected recovery client requires isolated Linux root")
	}
	s, ctx := socketRecoveryTestFixture(t)
	closeSocket, err := serveV2Recovery(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	defer closeSocket()
	for _, body := range []string{`{"unexpected":true}`, `{"a":1,"a":2}`, `null`, `{} {}`} {
		conn, err := net.Dial("unix", filepath.Join(s.cfg.StateDir, v2RecoverySocketName))
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.SetDeadline(time.Now().Add(time.Second))
		_, err = fmt.Fprintf(conn, "POST /snapshot HTTP/1.1\r\nHost: local.invalid\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
		if err != nil {
			conn.Close()
			t.Fatal(err)
		}
		response, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			conn.Close()
			t.Fatal(err)
		}
		response.Body.Close()
		conn.Close()
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("ambiguous local request accepted: %s", strings.TrimSpace(body))
		}
	}
	if s.v2.disk.RecoveryRequired {
		t.Fatal("rejected JSON mutated recovery authority")
	}
}
