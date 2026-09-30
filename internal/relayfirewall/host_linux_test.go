//go:build linux

package relayfirewall

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestUFWLockCoordinatesWithPythonLockf(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("root-owned UFW operation lock")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python lockf interoperability needs python3")
	}
	path := filepath.Join(t.TempDir(), "ufw.lock")
	lock, err := lockUFW(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	probe := `import fcntl,sys
f=open(sys.argv[1],'w')
try: fcntl.lockf(f,fcntl.LOCK_EX|fcntl.LOCK_NB)
except BlockingIOError: sys.exit(0)
sys.exit(9)`
	if out, err := exec.Command(python, "-c", probe, path).CombinedOutput(); err != nil {
		t.Fatal("UFW lockf did not observe helper lock", err, string(out))
	}
	lock.Close()
	// Hold the lock from a different process; waiting must respect cancellation.
	child := exec.Command(python, "-u", "-c", `import fcntl,sys
f=open(sys.argv[1],'w');fcntl.lockf(f,fcntl.LOCK_EX);print('locked',flush=True);sys.stdin.read()`, path)
	in, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { in.Close(); child.Wait() }()
	b := make([]byte, 7)
	if _, err = io.ReadFull(out, b); err != nil || string(b) != "locked\n" {
		t.Fatal(err, string(b))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if unexpected, err := lockUFW(ctx, path); err == nil {
		unexpected.Close()
		t.Fatal("ignored UFW's operation lock")
	}
}

func TestUFWLockRejectsUnsafeFiles(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("root-only UFW operation lock")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "lock")
	if err := os.WriteFile(path, nil, 0666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0666); err != nil {
		t.Fatal(err)
	}
	check := func(p string) {
		t.Helper()
		if f, err := lockUFW(context.Background(), p); err == nil {
			f.Close()
			t.Fatal("accepted unsafe lock", p)
		}
	}
	check(path)
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	check(filepath.Join(dir, "link"))
	if err := os.Link(path, filepath.Join(dir, "hard")); err != nil {
		t.Fatal(err)
	}
	check(path)
}

func TestKernelListenerOwnership(t *testing.T) {
	public, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer public.Close()
	private, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer private.Close()
	ports, err := listeningPorts(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range ports {
		if p == private.Addr().(*net.TCPAddr).Port {
			t.Fatal("included private listener")
		}
		found = found || p == public.Addr().(*net.TCPAddr).Port
	}
	if !found || ProcessStart(os.Getpid()) == "" {
		t.Fatal(ports)
	}
}

// Invoked only by the disposable systemd fixture, under DynamicUser.
func TestFirewallServiceChild(t *testing.T) {
	if os.Getenv("MSBOOST_FIREWALL_SERVICE_CHILD") != "1" {
		t.Skip("systemd fixture only")
	}
	l, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	port := l.Addr().(*net.TCPAddr).Port
	if err = os.WriteFile("/run/msboost-firewall-probe/port", []byte(strconv.Itoa(port)), 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(40 * time.Second)
	for time.Now().Before(deadline) {
		if Check(StatusPath, os.Getpid(), ProcessStart(os.Getpid()), port, time.Now()) == "ready" {
			if err = os.WriteFile("/run/msboost-firewall-probe/ready", []byte("ready"), 0600); err != nil {
				t.Fatal(err)
			}
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, err = os.Stat("/run/msboost-firewall-probe/ready"); err != nil {
		t.Fatal("DynamicUser could not confirm root firewall report")
	}
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go func() { defer c.Close(); _ = c.SetDeadline(time.Now().Add(3 * time.Second)); io.Copy(c, c) }()
	}
}

func TestTrustedFirewallStatus(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("root-owned status evidence")
	}
	path := filepath.Join(t.TempDir(), "status.json")
	now := time.Now()
	pid := os.Getpid()
	start := ProcessStart(pid)
	s := Status{PID: pid, Start: start, ObservedAt: now.UnixMilli(), Ports: []int{40897}}
	write := func() {
		raw, _ := json.Marshal(s)
		if err := os.WriteFile(path, raw, 0644); err != nil {
			t.Fatal(err)
		}
	}
	write()
	if Check(path, pid, start, 40897, now) != "ready" {
		t.Fatal("valid status not accepted")
	}
	if Check(path, pid+1, start, 40897, now) != "pending" || Check(path, pid, start, 40898, now) != "pending" {
		t.Fatal("wrong PID or port accepted")
	}
	s.Error = "firewall_maintenance_failed"
	write()
	if Check(path, pid, start, 40897, now) != "error" {
		t.Fatal("error treated as success")
	}
	s.Error = ""
	s.ObservedAt = now.Add(-time.Minute).UnixMilli()
	write()
	if Check(path, pid, start, 40897, now) != "pending" {
		t.Fatal("stale status accepted")
	}
	if err := os.Chmod(path, 0666); err != nil {
		t.Fatal(err)
	}
	if Check(path, pid, start, 40897, now) != "pending" {
		t.Fatal("writable status accepted")
	}
}

func TestCleanupRecreatesVolatileDirectoryWithoutAdoptingLinks(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("root-owned runtime directory")
	}
	parent := t.TempDir()
	dir := filepath.Join(parent, "runtime")
	if err := ensureRuntimeDirectory(dir); err != nil {
		t.Fatal(err)
	}
	if err := ensureRuntimeDirectory(dir); err != nil {
		t.Fatal("existing trusted directory", err)
	}
	if err := os.Chmod(dir, 0777); err != nil {
		t.Fatal(err)
	}
	if ensureRuntimeDirectory(dir) == nil {
		t.Fatal("adopted writable runtime directory")
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if ensureRuntimeDirectory(link) == nil {
		t.Fatal("adopted symlink")
	}
}

// Run ONLY in an explicitly disposable network namespace. Exercises the real
// iptables backend, not the host firewall or a mock executable.
func TestIsolatedUFWChain(t *testing.T) {
	if os.Getenv("MSBOOST_FIREWALL_NETNS_TEST") != "1" {
		t.Skip("requires isolated Linux network namespace")
	}
	if os.Geteuid() != 0 {
		t.Fatal("requires root in isolated namespace")
	}
	ctx := context.Background()
	for _, tool := range []string{"iptables", "ip6tables"} {
		parent := "ufw-user-input"
		if tool == "ip6tables" {
			parent = "ufw6-user-input"
		}
		setup := [][]string{{"-N", parent}, {"-P", "INPUT", "DROP"}, {"-A", "INPUT", "-j", parent}, {"-A", parent, "-p", "tcp", "--dport", "22", "-j", "ACCEPT"}}
		if os.Getenv("MSBOOST_FIREWALL_REAL_UFW") == "1" {
			setup = nil
		}
		for _, args := range setup {
			if out, err := runCommand(ctx, tool, args...); err != nil {
				t.Fatal(tool, out, err)
			}
		}
		for _, ports := range [][]int{{40897}, {40897, 48702}, {48702}, nil} {
			if err := ReconcileFamily(ctx, runCommand, tool, ports, true); err != nil {
				t.Fatal(err)
			}
			raw, err := runCommand(ctx, tool, "-S", parent)
			if err != nil || !strings.Contains(raw, "--dport 22 -j ACCEPT") {
				t.Fatal("SSH rule changed", raw, err)
			}
			for _, port := range ports {
				if out, e := runCommand(ctx, tool, append([]string{"-C", Chain}, portRule(port)...)...); e != nil {
					t.Fatal(strconv.Itoa(port), out, e)
				}
			}
		}
		if out, err := runCommand(ctx, tool, append([]string{"-C", parent}, jumpRule()...)...); err == nil {
			t.Fatal("stale jump", out)
		}
	}
}

func TestRealUFWTCPReachability(t *testing.T) {
	if os.Getenv("MSBOOST_FIREWALL_REAL_UFW") != "1" {
		t.Skip("requires private mount/network namespaces and UFW")
	}
	ctx := context.Background()
	peer := exec.Command("unshare", "--net", "--", "sleep", "80")
	if err := peer.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = peer.Process.Kill(); _ = peer.Wait() }()
	pid := strconv.Itoa(peer.Process.Pid)
	command := func(name string, args ...string) {
		t.Helper()
		if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
			t.Fatal(name, string(out), err)
		}
	}
	// Wait for unshare to finish creating the peer namespace before moving veth.
	for i := 0; i < 100; i++ {
		a, _ := os.Readlink("/proc/self/ns/net")
		b, _ := os.Readlink("/proc/" + pid + "/ns/net")
		if a != b && b != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	command("ip", "link", "add", "msb-server", "type", "veth", "peer", "name", "msb-client")
	command("ip", "link", "set", "msb-client", "netns", pid)
	command("ip", "addr", "add", "10.237.53.1/24", "dev", "msb-server")
	command("ip", "link", "set", "msb-server", "up")
	command("nsenter", "-t", pid, "-n", "--", "ip", "addr", "add", "10.237.53.2/24", "dev", "msb-client")
	command("nsenter", "-t", pid, "-n", "--", "ip", "link", "set", "msb-client", "up")
	l, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	port := l.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			c, e := l.Accept()
			if e != nil {
				return
			}
			go func() { defer c.Close(); _ = c.SetDeadline(time.Now().Add(2 * time.Second)); io.Copy(c, c) }()
		}
	}()
	probe := func(want bool) {
		t.Helper()
		code := "import socket; s=socket.create_connection(('10.237.53.1'," + strconv.Itoa(port) + "),0.6);s.settimeout(1);s.sendall(b'hello');assert s.recv(5)==b'hello';s.close()"
		out, e := exec.Command("nsenter", "-t", pid, "-n", "--", "python3", "-c", code).CombinedOutput()
		if (e == nil) != want {
			t.Fatal("TCP mismatch", want, string(out), e)
		}
	}
	probe(false)
	if _, err = syncHost(ctx, []int{port}, false); err != nil {
		t.Fatal(err)
	}
	probe(true)
	command("ufw", "reload")
	if _, err = syncHost(ctx, []int{port}, false); err != nil {
		t.Fatal(err)
	}
	probe(true)
	if _, err = syncHost(ctx, nil, true); err != nil {
		t.Fatal(err)
	}
	probe(false)
	// An explicit administrator deny keeps priority over our automatic permit.
	command("ufw", "deny", strconv.Itoa(port)+"/tcp")
	if _, err = syncHost(ctx, []int{port}, false); err != nil {
		t.Fatal(err)
	}
	probe(false)
	if _, err = syncHost(ctx, nil, true); err != nil {
		t.Fatal(err)
	}
}
