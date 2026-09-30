//go:build linux

package relayfirewall

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func newLimitedReader(r io.Reader) io.Reader { return io.LimitReader(r, 256<<10) }
func trustedStatus(path string) bool {
	for _, p := range []string{filepath.Dir(path), path} {
		st, err := os.Lstat(p)
		if err != nil || st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm()&0022 != 0 {
			return false
		}
		raw, ok := st.Sys().(*syscall.Stat_t)
		if !ok || raw.Uid != 0 {
			return false
		}
	}
	return true
}

func ensureRuntimeDirectory(dir string) error {
	// /run is volatile. Local cleanup must still work after a reboot when the
	// disabled helper has not recreated its directory. Never adopt a link or a
	// directory writable by another user.
	if !trustedStatus(filepath.Dir(dir)) {
		return errors.New("untrusted firewall runtime parent")
	}
	if err := os.Mkdir(dir, 0755); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	st, err := os.Lstat(dir)
	if err != nil || !st.IsDir() || !trustedStatus(dir) {
		return errors.New("untrusted firewall runtime directory")
	}
	return nil
}

func ProcessStart(pid int) string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ""
	}
	idx := strings.LastIndexByte(string(b), ')')
	if idx < 0 {
		return ""
	}
	fields := strings.Fields(string(b[idx+1:]))
	if len(fields) < 20 {
		return ""
	}
	return fields[19]
}

// Ports are derived from the kernel's actual socket ownership for systemd's
// MainPID, not from writable JSON supplied by the unprivileged agent.
func listeningPorts(pid int) ([]int, error) {
	base := fmt.Sprintf("/proc/%d", pid)
	fds, err := os.ReadDir(base + "/fd")
	if err != nil {
		return nil, err
	}
	inodes := map[string]bool{}
	for _, fd := range fds {
		target, e := os.Readlink(base + "/fd/" + fd.Name())
		if e == nil && strings.HasPrefix(target, "socket:[") {
			inodes[strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]")] = true
		}
	}
	ports := map[int]bool{}
	for _, family := range []string{"tcp", "tcp6"} {
		raw, e := os.ReadFile(base + "/net/" + family)
		if e != nil {
			if family == "tcp6" && errors.Is(e, os.ErrNotExist) {
				continue
			}
			return nil, e
		}
		for _, line := range strings.Split(string(raw), "\n") {
			f := strings.Fields(line)
			if len(f) < 10 || f[3] != "0A" || !inodes[f[9]] {
				continue
			}
			a := strings.Split(f[1], ":")
			if len(a) != 2 || strings.Trim(a[0], "0") != "" {
				continue
			} // Only wildcard public listeners.
			port, e := strconv.ParseInt(a[1], 16, 32)
			if e != nil || port < 1 || port > 65535 {
				return nil, errors.New("invalid kernel port")
			}
			ports[int(port)] = true
		}
	}
	if len(ports) > 8192 {
		return nil, errors.New("too many listeners")
	}
	return sortedPorts(ports), nil
}

type boundedOutput struct{ bytes.Buffer }

func (b *boundedOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 4<<20 {
		return 0, errors.New("command output too large")
	}
	return b.Buffer.Write(p)
}
func runCommand(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	var cmd *exec.Cmd
	if strings.HasSuffix(name, "-restore") {
		if len(args) != 1 {
			return "", errors.New("invalid restore input")
		}
		cmd = exec.CommandContext(ctx, "/usr/sbin/"+name, "--noflush", "--wait", "5")
		cmd.Stdin = strings.NewReader(args[0])
	} else {
		path := "/usr/sbin/" + name
		if name == "systemctl" {
			path = "/usr/bin/systemctl"
		}
		cmd = exec.CommandContext(ctx, path, args...)
	}
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C"}
	var out boundedOutput
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

func relayPID(ctx context.Context) (int, error) {
	out, err := runCommand(ctx, "systemctl", "show", "msboost-relay.service", "--property=MainPID", "--value")
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil || pid < 0 {
		return 0, errors.New("cannot identify relay process")
	}
	return pid, nil
}

func syncHost(ctx context.Context, ports []int, cleanup bool) (string, error) {
	// Do not bypass another firewall manager using a top-level INPUT accept.
	if _, err := os.Stat("/usr/bin/firewall-cmd"); err == nil && !cleanup {
		cmd := exec.CommandContext(ctx, "/usr/bin/firewall-cmd", "--state")
		if cmd.Run() == nil {
			return "firewalld", errors.New("firewalld requires explicit administrator rules; automatic UFW management is unavailable")
		}
	}
	active := false
	if _, err := os.Stat("/usr/sbin/ufw"); err == nil {
		out, e := runCommand(ctx, "ufw", "status")
		if e != nil {
			return "ufw", annotate(e, "read UFW")
		}
		active = strings.HasPrefix(out, "Status: active")
	}
	backend := "unmanaged"
	if active {
		backend = "ufw"
	}
	for _, family := range []string{"iptables", "ip6tables"} {
		if _, err := os.Stat("/usr/sbin/" + family); errors.Is(err, os.ErrNotExist) {
			if active {
				return backend, err
			}
			continue
		}
		enabled := active && !cleanup
		// UFW can intentionally disable IPv6; do not override its blanket deny.
		if family == "ip6tables" && enabled {
			config, err := os.ReadFile("/etc/default/ufw")
			if err != nil {
				return backend, err
			}
			if !strings.Contains("\n"+string(config), "\nIPV6=yes") {
				enabled = false
			}
		}
		if err := ReconcileFamily(ctx, runCommand, family, ports, enabled); err != nil {
			return backend, annotate(err, family)
		}
	}
	return backend, nil
}

func writeStatus(s Status) error {
	dir := filepath.Dir(StatusPath)
	if !trustedStatus(dir) {
		return errors.New("untrusted firewall runtime directory")
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".status-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0644); err == nil {
		_, err = f.Write(raw)
	}
	if e := f.Close(); err == nil {
		err = e
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), StatusPath)
}

// Run is root-only and reads just the fixed relay unit. The helper has no
// network API and no writable instruction files. UFW remains enabled.
func Run(ctx context.Context, cleanup bool) error {
	if os.Geteuid() != 0 {
		return errors.New("firewall maintenance requires local root")
	}
	if err := ensureRuntimeDirectory(filepath.Dir(StatusPath)); err != nil {
		return err
	}
	lock, err := os.OpenFile("/run/msboost-relay-firewall/maintenance.lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	st, err := lock.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm() != 0600 || st.Sys().(*syscall.Stat_t).Uid != 0 || st.Sys().(*syscall.Stat_t).Nlink != 1 {
		return errors.New("untrusted firewall lock")
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("firewall maintenance is already running")
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	if cleanup {
		pid, e := relayPID(ctx)
		if e != nil || pid != 0 {
			return errors.New("stop Relay before removing its firewall rules")
		}
		_, err = syncHost(ctx, nil, true)
		return err
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	lastError := ""
	for {
		pid, e := relayPID(ctx)
		s := Status{PID: pid, Ports: []int{}}
		if e == nil && pid > 0 {
			selfNS, selfErr := os.Readlink("/proc/self/ns/net")
			relayNS, relayErr := os.Readlink(fmt.Sprintf("/proc/%d/ns/net", pid))
			if selfErr != nil || relayErr != nil || selfNS != relayNS {
				e = errors.New("Relay is not in the maintenance network namespace")
			}
			s.Start = ProcessStart(pid)
			if e == nil {
				s.Ports, e = listeningPorts(pid)
			}
			if e == nil && (s.Start == "" || ProcessStart(pid) != s.Start) {
				e = errors.New("relay process changed during inspection")
			}
			if e == nil {
				current, checkErr := relayPID(ctx)
				if checkErr != nil || current != pid {
					e = errors.New("relay process changed during inspection")
				}
			}
		}
		if e == nil {
			s.Backend, e = syncHost(ctx, s.Ports, false)
		}
		s.ObservedAt = time.Now().UnixMilli()
		s.Error = errorStatus(e)
		if err := writeStatus(s); err != nil {
			return err
		}
		message := ""
		if e != nil {
			message = e.Error()
		}
		if message != lastError {
			if message != "" {
				log.Print("relay firewall: ", message)
			}
			lastError = message
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
