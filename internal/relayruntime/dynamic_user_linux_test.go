//go:build linux

package relayruntime

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// This is a startup/state/Unix-peer integration helper, not a forwarding or
// zero-downtime installer migration test. The shell creates a fresh real systemd
// DynamicUser unit with PrivateNetwork=true and a random StateDirectory. Both
// entry points are opt-in; default Linux CI never creates a service.
var dynamicUserNamePattern = regexp.MustCompile(`^msboost-dyn-[a-f0-9]{12}$`)

type dynamicUserReady struct {
	Phase string `json:"phase"`
	PID   int    `json:"pid"`
	UID   uint32 `json:"uid"`
	Dev   uint64 `json:"dev"`
	Inode uint64 `json:"inode"`
}

func dynamicUserTestPaths(t *testing.T) (string, string, string) {
	t.Helper()
	name := os.Getenv("MSBOOST_DYNAMIC_NAME")
	if !dynamicUserNamePattern.MatchString(name) {
		t.Fatal("exact random DynamicUser fixture name required")
	}
	return name, filepath.Join("/var/lib", name), filepath.Join("/var/lib/private", name)
}

func dynamicUserProcOnlyLoopback(data []byte) bool {
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 || !strings.HasPrefix(strings.TrimSpace(lines[0]), "Inter-|") || !strings.Contains(lines[0], "Receive") || !strings.Contains(lines[0], "Transmit") || !strings.HasPrefix(strings.TrimSpace(lines[1]), "face |bytes") {
		return false
	}
	name, counters, found := strings.Cut(lines[2], ":")
	if !found || strings.TrimSpace(name) != "lo" {
		return false
	}
	values := strings.Fields(counters)
	if len(values) != 16 {
		return false
	}
	for _, value := range values {
		for _, digit := range value {
			if digit < '0' || digit > '9' {
				return false
			}
		}
		if _, err := strconv.ParseUint(value, 10, 64); err != nil {
			return false
		}
	}
	return true
}

func TestDynamicUserServiceChild(t *testing.T) {
	if os.Getenv("MSBOOST_DYNAMIC_USER_VALIDATION") != "service" {
		t.Skip("opt-in real random DynamicUser service child")
	}
	_, public, private := dynamicUserTestPaths(t)
	uid := os.Geteuid()
	if uid < 61184 || uid > 65519 {
		t.Fatalf("actual dynamic UID outside expected range: uid=%d", uid)
	}
	if os.Getenv("STATE_DIRECTORY") != public {
		t.Fatal("systemd StateDirectory environment does not match the exact random public path")
	}
	// net.Interfaces uses AF_NETLINK on Linux and is intentionally prohibited
	// by the real production unit's address-family filter. Keep that filter;
	// use the current process's network-namespace proc view for this probe.
	fd, netlinkErr := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if netlinkErr == nil {
		_ = unix.Close(fd)
		t.Fatal("production unit AF_NETLINK restriction was not enforced")
	}
	if !errors.Is(netlinkErr, unix.EAFNOSUPPORT) && !errors.Is(netlinkErr, unix.EPERM) {
		t.Fatal("AF_NETLINK probe failed for an unexpected reason")
	}
	interfaces, err := os.ReadFile("/proc/self/net/dev")
	if err != nil || !dynamicUserProcOnlyLoopback(interfaces) {
		t.Fatal("service process proc network view is not strictly loopback-only")
	}
	netlinkClass := "EAFNOSUPPORT"
	if errors.Is(netlinkErr, unix.EPERM) {
		netlinkClass = "EPERM"
	}
	t.Logf("DYNAMIC_USER_GUARDS_PASS inside_uid=%d state_directory_matches=true af_netlink_errno=%s proc_only_loopback=true", uid, netlinkClass)
	if deadline, bounded := t.Deadline(); !bounded || time.Until(deadline) < time.Minute {
		t.Fatal("explicit bounded service test timeout required")
	}
	var link, directory unix.Stat_t
	target, err := os.Readlink(public)
	if err != nil || unix.Lstat(public, &link) != nil || link.Uid != 0 || link.Mode&unix.S_IFMT != unix.S_IFLNK {
		t.Fatal("real systemd public StateDirectory link was not observed")
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(public), target)
	}
	if filepath.Clean(target) != private || unix.Lstat(private, &directory) != nil || directory.Mode&unix.S_IFMT != unix.S_IFDIR || directory.Mode&07777 != 0700 || directory.Uid != uint32(uid) {
		t.Fatal("private systemd directory ownership or exact link target mismatch")
	}
	gost := os.Getenv("GOST_TEST_BINARY")
	file, err := os.Open(gost)
	if err != nil || !filepath.IsAbs(gost) {
		t.Fatal("fixed pinned GOST asset required")
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	_ = file.Close()
	if copyErr != nil || hex.EncodeToString(hash.Sum(nil)) != "1d8f971e9447cf4114fb1376b85c8c14e840db50ae8dbe895398a34e97c13e08" {
		t.Fatal("GOST checksum mismatch")
	}
	cfg := Config{ServerURL: "http://127.0.0.1:1", StateDir: public, GostBinary: gost, OfflinePolicy: KeepLast}
	// This is the actual startup entry point, before any registration/network
	// access. Do not canonicalize or weaken the production symlink rejection.
	if err := Run(context.Background(), cfg); err == nil || err.Error() != "relay state path must not contain links" {
		t.Fatal("public DynamicUser symlink did not fail closed at actual Run entry")
	}
	cfg.StateDir = private
	phase := "first"
	statePath := filepath.Join(private, v2StateFile)
	if _, err := os.Lstat(statePath); errors.Is(err, os.ErrNotExist) {
		state := v2DiskState{Schema: ProtocolV2, OfflinePolicy: KeepLast, ServerURL: cfg.ServerURL, AgentID: randomID(), ControlEpoch: randomID(), Revision: 7, ManagementToken: randomID() + randomID(), Records: map[string]v2Record{}}
		if err := writeV2PrivateJSON(statePath, state); err != nil {
			t.Fatal("cannot seed synthetic private v2 state")
		}
	} else if err == nil {
		phase = "restart"
		var saved v2DiskState
		if readV2PrivateJSON(statePath, &saved) != nil || !saved.RecoveryRequired || saved.Recovery == nil || saved.ManagementToken == "" || saved.Revision != 7 || len(saved.Records) != 0 || validateV2RecoveryCheckpoint(saved) != nil {
			t.Fatal("restart did not retain the root-authorized frozen private state")
		}
	} else {
		t.Fatal("cannot inspect fixture state")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Error("actual private-path runtime exited unsuccessfully")
			}
		case <-time.After(5 * time.Second):
			t.Error("actual runtime did not shut down")
		}
	}()
	socket := filepath.Join(private, v2RecoverySocketName)
	ready := false
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		if _, err := recoverySocketInfo(socket, uint32(uid)); err == nil {
			ready = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !ready {
		t.Fatal("actual private-path runtime recovery socket did not become ready")
	}
	conn, err := net.DialTimeout("unix", socket, time.Second)
	if err != nil {
		t.Fatal("same dynamic UID could not reach its owned socket for negative peer test")
	}
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	_, _ = fmt.Fprint(conn, "POST /snapshot HTTP/1.1\r\nHost: local.invalid\r\nContent-Length: 2\r\n\r\n{}")
	response, readErr := http.ReadResponse(bufio.NewReader(conn), nil)
	_ = conn.Close()
	if response != nil {
		_ = response.Body.Close()
	}
	if readErr == nil || !(errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) || errors.Is(readErr, syscall.ECONNRESET)) {
		t.Fatal("SO_PEERCRED did not explicitly reject the non-root socket owner")
	}
	marker := dynamicUserReady{Phase: phase, PID: os.Getpid(), UID: uint32(uid), Dev: uint64(directory.Dev), Inode: directory.Ino}
	if err := writeV2PrivateJSON(filepath.Join(private, "dynamic-"+phase+".json"), marker); err != nil {
		t.Fatal("cannot persist non-secret readiness evidence")
	}
	t.Logf("DYNAMIC_USER_SERVICE_READY phase=%s inside_uid=%d state_dev=%d state_inode=%d state_uid=%d state_gid=%d state_mode=%04o af_netlink_explicitly_denied=true proc_only_loopback=true public_link_rejected=true private_run_started=true nonroot_socket_owner_denied=true synthetic_records=0", phase, uid, directory.Dev, directory.Ino, directory.Uid, directory.Gid, directory.Mode&07777)
	<-ctx.Done()
}

func TestDynamicUserRootEvidence(t *testing.T) {
	if os.Getenv("MSBOOST_DYNAMIC_USER_VALIDATION") != "host" {
		t.Skip("opt-in host-side real DynamicUser socket evidence")
	}
	name, _, private := dynamicUserTestPaths(t)
	phase := os.Getenv("MSBOOST_DYNAMIC_PHASE")
	reports := os.Getenv("MSBOOST_DYNAMIC_REPORTS")
	pid, err := strconv.Atoi(os.Getenv("MSBOOST_DYNAMIC_PID"))
	if os.Geteuid() != 0 || err != nil || pid <= 1 || phase != "first" && phase != "restart" || !regexp.MustCompile(`^/root/msboost-dynamic-user\.[A-Za-z0-9]{8}$`).MatchString(reports) || checkV2PrivatePath(reports, true) != nil {
		t.Fatal("exact root-private report directory and live unit identity required")
	}
	var directory, socketMetadata unix.Stat_t
	socketPath := filepath.Join(private, v2RecoverySocketName)
	if unix.Lstat(private, &directory) != nil || directory.Mode&unix.S_IFMT != unix.S_IFDIR || directory.Mode&07777 != 0700 || unix.Lstat(socketPath, &socketMetadata) != nil {
		t.Fatal("host state/socket metadata unavailable")
	}
	var marker dynamicUserReady
	markerRaw, err := os.ReadFile(filepath.Join(private, "dynamic-"+phase+".json"))
	if err != nil || strictV2JSON(markerRaw, &marker) != nil || marker.Phase != phase || marker.PID != pid || marker.UID != directory.Uid || marker.Dev != uint64(directory.Dev) || marker.Inode != directory.Ino {
		t.Fatal("host and service namespace state ownership/inode evidence differs")
	}
	before, err := recoverySocketInfo(socketPath, directory.Uid)
	if err != nil {
		t.Fatal("host recovery socket strict ownership check failed")
	}
	conn, err := net.DialTimeout("unix", socketPath, time.Second)
	if err != nil {
		t.Fatal("host root cannot connect to the actual service socket")
	}
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		conn.Close()
		t.Fatal("unexpected local socket type")
	}
	raw, err := unixConn.SyscallConn()
	var peer *unix.Ucred
	var peerErr error
	if err == nil {
		err = raw.Control(func(fd uintptr) { peer, peerErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) })
	}
	_ = conn.Close()
	after, statErr := recoverySocketInfo(socketPath, directory.Uid)
	if err != nil || peerErr != nil || peer == nil || peer.Uid != directory.Uid || int(peer.Pid) != pid || statErr != nil || !os.SameFile(before, after) {
		t.Fatal("actual host peer PID/UID or socket inode differs from the owned unit")
	}
	cgroup, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil || !strings.Contains(string(cgroup), "/"+name+".service") {
		t.Fatal("socket peer is not in the exact random service cgroup")
	}
	serviceNetns, serviceNetnsErr := os.Stat(fmt.Sprintf("/proc/%d/ns/net", pid))
	hostNetns, hostNetnsErr := os.Stat("/proc/self/ns/net")
	if serviceNetnsErr != nil || hostNetnsErr != nil || os.SameFile(serviceNetns, hostNetns) {
		t.Fatal("service socket peer is not isolated in a different actual network namespace")
	}
	var snapshot V2RecoverySnapshot
	if readV2PrivateJSON(filepath.Join(reports, phase+"-snapshot.json"), &snapshot) != nil {
		t.Fatal("actual root agent CLI did not save a private valid snapshot")
	}
	stateRaw, err := os.ReadFile(filepath.Join(private, v2StateFile))
	var state v2DiskState
	if err != nil || strictV2JSON(stateRaw, &state) != nil || state.Schema != ProtocolV2 || state.OfflinePolicy != KeepLast || state.ServerURL != "http://127.0.0.1:1" || state.ManagementToken == "" || !state.RecoveryRequired || state.Recovery == nil || len(state.Records) != 0 || state.Revision != 7 || validateV2RecoveryCheckpoint(state) != nil {
		t.Fatal("root snapshot did not persist the exact synthetic recovery freeze")
	}
	if snapshot.ProtocolVersion != ProtocolV2 || snapshot.AgentID != state.AgentID || snapshot.ControlEpoch != state.ControlEpoch || snapshot.RecoveryID != state.Recovery.RecoveryID || snapshot.Revision != state.Revision || len(snapshot.Records) != 0 || !snapshot.HistoryIncomplete {
		t.Fatal("actual root CLI snapshot does not match retained runtime state")
	}
	baseline := filepath.Join(reports, "baseline-private-state.json")
	if phase == "first" {
		if _, err := os.Lstat(baseline); !errors.Is(err, os.ErrNotExist) || writeV2PrivateJSON(baseline, state) != nil {
			t.Fatal("cannot exclusively initialize private persistence baseline")
		}
	} else {
		var original v2DiskState
		if readV2PrivateJSON(baseline, &original) != nil || original.AgentID != state.AgentID || original.ControlEpoch != state.ControlEpoch || original.ManagementToken != state.ManagementToken || original.Revision != state.Revision || original.Recovery == nil || original.Recovery.RecoveryID != state.Recovery.RecoveryID || !original.RecoveryRequired {
			t.Fatal("restart changed identity, private credential, revision or recovery freeze")
		}
	}
	t.Logf("DYNAMIC_USER_HOST_PASS phase=%s host_state_dev=%d host_state_inode=%d host_state_uid=%d host_state_gid=%d host_state_mode=%04o host_socket_uid=%d actual_peer_uid=%d actual_peer_pid=%d distinct_service_netns=true strict_root_cli_snapshot=true frozen_state_persisted=true", phase, directory.Dev, directory.Ino, directory.Uid, directory.Gid, directory.Mode&07777, socketMetadata.Uid, peer.Uid, peer.Pid)
}

func TestDynamicUserFixtureNameScope(t *testing.T) {
	for _, value := range []string{"msboost-relay", "../msboost-dyn-123456789abc", "msboost-dyn-123456789abc/other", "msboost-dyn-ABCDEF123456", "msboost-dyn-123456789ab"} {
		if dynamicUserNamePattern.MatchString(value) {
			t.Fatal("fixture accepted a non-isolated state directory name")
		}
	}
	if !dynamicUserNamePattern.MatchString("msboost-dyn-123456789abc") {
		t.Fatal("fixture rejected its exact random name format")
	}
}

func TestDynamicUserProcLoopbackScope(t *testing.T) {
	header := "Inter-|   Receive                                                |  Transmit\n face |bytes packets errs drop fifo frame compressed multicast|bytes packets errs drop fifo colls carrier compressed\n"
	row := "    lo:  0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0\n"
	for name, fixture := range map[string]string{
		"header_only":        header,
		"second_interface":   header + row + strings.Replace(row, "lo:", "eth0:", 1),
		"duplicate_loopback": header + row + row,
		"non_loopback":       header + strings.Replace(row, "lo:", "eth0:", 1),
		"negative_counter":   header + strings.Replace(row, "0", "-1", 1),
		"invalid_counter":    header + strings.Replace(row, "0", "x", 1),
		"overflow_counter":   header + strings.Replace(row, "0", "18446744073709551616", 1),
		"missing_counter":    header + strings.Replace(row, "0 ", "", 1),
		"invalid_header":     "bad\nheader\n" + row,
	} {
		t.Run(name, func(t *testing.T) {
			if dynamicUserProcOnlyLoopback([]byte(fixture)) {
				t.Fatal("accepted ambiguous or non-loopback proc interface inventory")
			}
		})
	}
	if !dynamicUserProcOnlyLoopback([]byte(header + row)) {
		t.Fatal("rejected valid isolated Linux loopback interface inventory")
	}
}
