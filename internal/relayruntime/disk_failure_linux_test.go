//go:build linux

package relayruntime

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

// This is a bounded real-kernel disk fault, not a host-disk exhaustion test.
// Run only in a NEW `unshare --mount --net --propagation private` namespace
// with lo enabled. The test mounts its own 1MiB tmpfs, never an existing path.
// It exercises actual GOST/observer/state persistence but NOT a real control
// backend or PostgreSQL accounting reconciliation.
func TestRealGostPrivateDiskFullKeepsOriginalConnection(t *testing.T) {
	if os.Getenv("MSBOOST_REAL_GOST_DISK_FAILURE") != "1" {
		t.Skip("opt-in private 1MiB tmpfs and real GOST disk failure")
	}
	interfaces, err := net.Interfaces()
	selfMount, selfErr := os.Readlink("/proc/self/ns/mnt")
	initMount, initErr := os.Readlink("/proc/1/ns/mnt")
	mounts, mountErr := os.ReadFile("/proc/self/mountinfo")
	if os.Geteuid() != 0 || os.Getenv("MSBOOST_TEST_LOOPBACK_NETNS") != "1" || os.Getenv("MSBOOST_TEST_PRIVATE_MOUNT_NS") != "1" || err != nil || len(interfaces) != 1 || interfaces[0].Name != "lo" || selfErr != nil || initErr != nil || selfMount == initMount || mountErr != nil {
		t.Fatal("requires a new root, loopback-only, private mount namespace")
	}
	for _, field := range strings.Fields(string(mounts)) {
		if strings.HasPrefix(field, "shared:") {
			t.Fatal("shared mount propagation is forbidden")
		}
	}
	gost := os.Getenv("GOST_TEST_BINARY")
	info, err := os.Lstat(gost)
	if !filepath.IsAbs(gost) || err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 {
		t.Fatal("trusted absolute, non-writable GOST executable required")
	}
	binary, err := os.ReadFile(gost)
	if err != nil || fmt.Sprintf("%x", sha256.Sum256(binary)) != "1d8f971e9447cf4114fb1376b85c8c14e840db50ae8dbe895398a34e97c13e08" {
		t.Fatal("GOST 3.3.0 linux/amd64 executable checksum mismatch")
	}
	dir, err := os.MkdirTemp("", "msboost-disk-fault-")
	if err != nil {
		t.Fatal(err)
	}
	mounted := false
	// Registered before the runtime fixture: its process/observer cleanup runs
	// first. Never recursively remove a still-mounted filesystem on failure.
	t.Cleanup(func() {
		if mounted {
			if err := syscall.Unmount(dir, 0); err != nil {
				t.Errorf("private tmpfs unmount failed; retained exact path %s", dir)
				return
			}
		}
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("private fixture directory cleanup failed: %v", err)
		}
	})
	if err := syscall.Mount("msboost-disk-fault", dir, "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, "size=1048576,mode=0700"); err != nil {
		t.Fatal(err)
	}
	mounted = true
	var fs syscall.Statfs_t
	if err := syscall.Statfs(dir, &fs); err != nil || fs.Type != 0x01021994 || fs.Blocks*uint64(fs.Bsize) > 1048576 {
		t.Fatal("fixture filesystem is not the bounded private tmpfs")
	}
	s, ctx, target := v2TestFixture(t)
	s.cfg.StateDir, s.cfg.GostBinary = dir, gost
	s.v2.path = filepath.Join(dir, v2StateFile)
	if err := s.initV2TrafficLocked(); err != nil {
		t.Fatal(err)
	}
	command := v2TestCommand(t, "real-gost-disk-rule", target)
	command.Rule.Billing, command.Rule.EntitlementVersion = true, 1
	command.BillingPeriodID = "isolated-disk-billing-grant"
	request, response := v2TestResponse(s, []V2Command{command})
	if err := s.applyV2Response(ctx, request, response); err != nil {
		t.Fatal(err)
	}
	v2WaitState(t, s, ctx, command.RuleID, "ready")
	original := v2Connect(t, command)
	sequence := 0
	s.mu.Lock()
	before := s.processes[command.RuleID]
	s.mu.Unlock()
	pulse := func() {
		t.Helper()
		sequence++
		v2Echo(t, original, sequence)
		s.mu.Lock()
		unchanged := s.processes[command.RuleID] == before && !before.stopping && !processDone(before) && sameV2Command(s.v2.disk.Records[command.RuleID].Command, command)
		s.mu.Unlock()
		if !unchanged {
			t.Fatal("original GOST or authorized command changed during disk fault")
		}
	}
	wait := func(label string, ready func() bool) {
		t.Helper()
		for deadline := time.Now().Add(12 * time.Second); time.Now().Before(deadline); {
			pulse()
			s.mu.Lock()
			ok := ready()
			s.mu.Unlock()
			if ok {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("disk fault phase timed out: %s", label)
	}
	wait("actual GOST observer collected bidirectional usage", func() bool {
		if len(s.v2Traffic.Pending) == 0 {
			return false
		}
		for _, sample := range s.v2Traffic.Pending {
			if sample.InputBytes <= 0 || sample.OutputBytes <= 0 {
				return false
			}
		}
		return true
	})
	s.mu.Lock()
	var initialJournal v2TrafficState
	initialErr := readV2PrivateJSON(filepath.Join(dir, "traffic-v2-journal.json"), &initialJournal)
	initialOK := initialErr == nil && !initialJournal.Degraded && !s.v2AccountingDegradedLocked() && reflect.DeepEqual(initialJournal.Pending, s.v2Traffic.Pending)
	for _, sample := range initialJournal.Pending {
		initialOK = initialOK && sample.ID == command.RuleID && sample.BillingPeriodID == command.BillingPeriodID && sample.Epoch != "" && sample.InputBytes > 0 && sample.OutputBytes > 0
	}
	s.mu.Unlock()
	if !initialOK || len(initialJournal.Pending) == 0 {
		t.Fatal("initial actual observer usage was not healthy and durably persisted before fault injection")
	}
	stateBefore, err := os.ReadFile(s.v2.path)
	if err != nil {
		t.Fatal(err)
	}
	fillPath := filepath.Join(dir, "owned-capacity-fixture")
	fill, err := os.OpenFile(fillPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	filled := 0
	chunk := make([]byte, 16384)
	for filled <= 1048576 {
		n, writeErr := fill.Write(chunk)
		filled += n
		if writeErr != nil {
			err = writeErr
			break
		}
	}
	if closeErr := fill.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if !errors.Is(err, syscall.ENOSPC) || filled > 1048576 {
		t.Fatal("bounded tmpfs did not produce a real kernel ENOSPC")
	}
	if err := writeV2PrivateJSON(filepath.Join(dir, "write-probe.json"), map[string]bool{"probe": true}); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("production atomic writer did not report kernel ENOSPC: %v", err)
	}
	candidate := command
	rule := *command.Rule
	rule.RateMbps++
	candidate.Rule, candidate.Generation, candidate.CommandID = &rule, 2, "real-gost-disk-candidate"
	candidate.RuntimeHash, err = RuntimeHash(rule)
	if err != nil {
		t.Fatal(err)
	}
	request, response = v2TestResponse(s, []V2Command{candidate})
	if err := s.applyV2Response(ctx, request, response); err == nil {
		t.Fatal("new candidate accepted on full private state filesystem")
	}
	pulse()
	wait("actual observer sets visible degradation without stopping forwarding", func() bool { return s.v2AccountingDegradedLocked() && len(s.v2Traffic.Pending) > 0 })
	s.mu.Lock()
	batch := s.v2TrafficBatchLocked()
	acks := make([]V2TrafficAck, 0, len(batch))
	for _, sample := range batch {
		acks = append(acks, V2TrafficAck{RuleID: sample.ID, Epoch: sample.Epoch, Sequence: sample.Sequence})
	}
	ackErr := s.acknowledgeV2TrafficLocked(batch, acks)
	retained := len(s.v2Traffic.Pending) == len(batch)
	s.mu.Unlock()
	if ackErr == nil || !retained {
		t.Fatal("failed durable traffic ACK removed uncommitted samples")
	}
	stateAfter, err := os.ReadFile(s.v2.path)
	if err != nil || string(stateAfter) != string(stateBefore) {
		t.Fatal("full-disk candidate mutated the last durable authorized intent")
	}
	if err := os.Remove(fillPath); err != nil {
		t.Fatal(err)
	}
	// Restoring capacity must not silently clear accounting degradation or
	// authorize the rejected candidate. Persist the actual observer's retained
	// journal and verify it through the production private-file reader.
	s.mu.Lock()
	err = s.persistV2TrafficLocked(s.v2Traffic)
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	var disk v2TrafficState
	if err := readV2PrivateJSON(filepath.Join(dir, "traffic-v2-journal.json"), &disk); err != nil || !disk.Degraded || len(disk.Pending) == 0 {
		t.Fatal("restored capacity lost the retained accounting warning/samples")
	}
	request, response = v2TestResponse(s, []V2Command{candidate})
	if err := s.applyV2Response(ctx, request, response); err == nil {
		t.Fatal("restored disk silently unfroze new configuration")
	}
	pulse()
	newConnection := v2Connect(t, command)
	v2Echo(t, newConnection, 1)
	pulse()
	// Remount ONLY the same test-owned tmpfs, not a host filesystem, to prove
	// that read-only failure is a kernel EROFS rather than chmod under root.
	if err := syscall.Mount("", dir, "tmpfs", syscall.MS_REMOUNT|syscall.MS_RDONLY|syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, ""); err != nil {
		t.Fatal(err)
	}
	if err := writeV2PrivateJSON(filepath.Join(dir, "readonly-probe.json"), map[string]bool{"probe": true}); !errors.Is(err, syscall.EROFS) {
		t.Fatalf("production atomic writer did not report kernel EROFS: %v", err)
	}
	for i := 0; i < 15; i++ {
		pulse()
		time.Sleep(100 * time.Millisecond)
	}
	if err := syscall.Mount("", dir, "tmpfs", syscall.MS_REMOUNT|syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, ""); err != nil {
		t.Fatal(err)
	}
	pulse()
	t.Logf("REAL_GOST_DISK_FULL_PASS private_tmpfs_max_bytes=1048576 kernel_enospc=true kernel_erofs=true original_dials=1 original_reconnects=0 same_process=true candidate_rejected=true actual_observer_degraded=true failed_ack_retained=true warning_persisted_after_capacity_restore=true new_connection=true samples=%d", sequence)
}
