package relayruntime

import (
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// This uses the loopback TCP test executable, not GOST. It verifies the
// production stop/reap/config-cleanup boundary separately from the real-GOST
// watchdog integration, so an early EOF is never treated as a stopped ACK.
func TestLeaseStopReapsChildBeforeStoppedAndConfigRemoval(t *testing.T) {
	s, ctx, target := v2TestFixture(t)
	s.cfg.OfflinePolicy = "lease"
	s.journal = filepath.Join(s.cfg.StateDir, "traffic-journal.json")
	command := v2TestCommand(t, "lease-stop-reap-test-rule", target)
	s.mu.Lock()
	err := s.startLocked(ctx, *command.Rule)
	p := s.processes[command.RuleID]
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(3 * time.Second); ; {
		s.mu.Lock()
		ready := p.ack.State == "ready"
		s.mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("loopback child never became ready")
		}
		time.Sleep(5 * time.Millisecond)
	}
	conn := v2Connect(t, command)
	v2Echo(t, conn, 1)
	configPath := p.cmd.Args[2]
	info, err := os.Lstat(configPath)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("started child lacks an existing configuration: %v", err)
	}
	s.mu.Lock()
	s.stopLocked(command.RuleID)
	// Holding the runtime mutex prevents the Wait goroutine publishing its
	// result yet, even if the operating system has already closed sockets.
	if p.ack.State != "stopping" {
		s.mu.Unlock()
		t.Fatal("stop published stopped before the Wait result was committed")
	}
	s.mu.Unlock()
	select {
	case <-p.done:
	case <-time.After(3 * time.Second):
		t.Fatal("child stop never reached cmd.Wait completion")
	}
	s.mu.Lock()
	stopped := p.ack.State == "stopped" && p.cmd.ProcessState != nil
	s.mu.Unlock()
	if !stopped {
		t.Fatal("reaped child did not publish stopped")
	}
	if _, err := os.Lstat(configPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("config cleanup did not follow child reap: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	reset, refused := syscall.ECONNRESET, syscall.ECONNREFUSED
	if runtime.GOOS == "windows" {
		// Winsock uses WSAECONNRESET/WSAECONNREFUSED, not POSIX errno values.
		reset, refused = syscall.Errno(10054), syscall.Errno(10061)
	}
	var one [1]byte
	if n, err := conn.Read(one[:]); n != 0 || !errors.Is(err, io.EOF) && !errors.Is(err, reset) {
		t.Fatalf("stopped child's original connection remained open: n=%d err=%v", n, err)
	}
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(command.Rule.ListenPort))
	if next, err := net.DialTimeout("tcp4", address, time.Second); err == nil {
		next.Close()
		t.Fatal("reaped child's listener accepted a new connection")
	} else if !errors.Is(err, refused) {
		t.Fatalf("reaped listener must explicitly refuse, not time out: %v", err)
	}
}
