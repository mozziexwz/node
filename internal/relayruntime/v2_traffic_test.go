package relayruntime

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func trafficV2Fixture(t *testing.T) (*runtimeState, *process, int64) {
	t.Helper()
	now := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC).UnixMilli()
	p := &process{epoch: "process-epoch-unique", startedAt: now, rule: Rule{ID: "rule-identity-unique", Version: 1, EntitlementVersion: 1, Billing: true}, v2BillingPeriod: "grant-period-one"}
	dir := t.TempDir()
	// testing.TempDir's numbered leaf uses 0777 subject to the runner's umask;
	// its private parent does not satisfy the runtime's private-leaf contract.
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := checkV2PrivatePath(dir, true); err != nil {
		t.Fatalf("traffic fixture must use the production private state permissions: %v", err)
	}
	s := &runtimeState{cfg: Config{StateDir: dir, OfflinePolicy: KeepLast}, processes: map[string]*process{p.rule.ID: p}}
	if err := s.initV2TrafficLocked(); err != nil {
		t.Fatal(err)
	}
	return s, p, now
}

func assertTrafficV2Checkpoint(t *testing.T, s *runtimeState, degraded bool) {
	t.Helper()
	var disk v2TrafficState
	if err := readV2PrivateJSON(filepath.Join(s.cfg.StateDir, "traffic-v2-journal.json"), &disk); err != nil {
		t.Fatalf("traffic fixture did not durably persist its private checkpoint: %v", err)
	}
	if s.v2Traffic.Degraded != degraded || disk.Degraded != degraded || disk.SchemaVersion != 2 ||
		disk.GapInputBytes != s.v2Traffic.GapInputBytes || disk.GapOutputBytes != s.v2Traffic.GapOutputBytes ||
		!reflect.DeepEqual(disk.Pending, s.v2Traffic.Pending) {
		t.Fatal("persisted accounting checkpoint does not match the expected in-memory samples/warning")
	}
}

func TestV2TrafficExplicitAckAndContinuedCumulativeCounters(t *testing.T) {
	s, p, now := trafficV2Fixture(t)
	s.recordV2TrafficLocked(p, 100, 200, 1, now+1000)
	assertTrafficV2Checkpoint(t, s, false)
	batch := s.v2TrafficBatchLocked()
	if len(batch) != 1 || batch[0].Sequence != 1 {
		t.Fatal("missing initial sample")
	}
	for _, acks := range [][]V2TrafficAck{nil, {{RuleID: "wrong", Epoch: batch[0].Epoch, Sequence: 1}}, {{RuleID: p.rule.ID, Epoch: batch[0].Epoch, Sequence: 2}}} {
		if err := s.acknowledgeV2TrafficLocked(batch, acks); err != nil {
			t.Fatal(err)
		}
		if len(s.v2Traffic.Pending) != 1 {
			t.Fatal("arbitrary HTTP success or mismatched ACK removed sample")
		}
	}
	ack := []V2TrafficAck{{RuleID: p.rule.ID, Epoch: batch[0].Epoch, Sequence: 1}}
	if err := s.acknowledgeV2TrafficLocked(batch, ack); err != nil {
		t.Fatal(err)
	}
	if len(s.v2Traffic.Pending) != 0 {
		t.Fatal("explicit acknowledged sample retained")
	}
	assertTrafficV2Checkpoint(t, s, false)
	s.recordV2TrafficLocked(p, 150, 250, 1, now+2000)
	assertTrafficV2Checkpoint(t, s, false)
	next := s.v2TrafficBatchLocked()
	if len(next) != 1 || next[0].Epoch != batch[0].Epoch || next[0].Sequence != 2 || next[0].InputBytes != 150 || next[0].OutputBytes != 250 {
		t.Fatal("post-ACK counters reset and would be lost to deduplication")
	}
	if err := s.acknowledgeV2TrafficLocked(batch, ack); err != nil {
		t.Fatal(err)
	}
	if len(s.v2Traffic.Pending) != 1 {
		t.Fatal("old ACK cleared newer observer data")
	}
	loaded := &runtimeState{cfg: s.cfg}
	if err := loaded.initV2TrafficLocked(); err != nil {
		t.Fatal(err)
	}
	if len(loaded.v2Traffic.Pending) != 1 {
		t.Fatal("unacknowledged data not durable")
	}
}

func TestV2TrafficBoundariesNeverMixNewEntitlement(t *testing.T) {
	s, p, now := trafficV2Fixture(t)
	s.recordV2TrafficLocked(p, 100, 200, 1, now+1000)
	assertTrafficV2Checkpoint(t, s, false)
	p.v2BillingPeriod = "grant-period-two"
	p.rule.EntitlementVersion = 2
	p.rule.Version = 2
	s.recordV2TrafficLocked(p, 110, 220, 1, now+2000)
	s.recordV2TrafficLocked(p, 120, 230, 1, now+3000)
	assertTrafficV2Checkpoint(t, s, false)
	batch := s.v2TrafficBatchLocked()
	if len(batch) != 3 {
		t.Fatalf("want old, boundary-review and new samples, got %d", len(batch))
	}
	old, review, newPeriod := false, false, false
	for _, sample := range batch {
		switch {
		case sample.BillingPeriodID == "grant-period-one" && !sample.Uncertain:
			old = sample.InputBytes == 100 && sample.OutputBytes == 200
		case sample.BillingPeriodID == "grant-period-one" && sample.Uncertain:
			review = sample.InputBytes == 10 && sample.OutputBytes == 20
		case sample.BillingPeriodID == "grant-period-two":
			newPeriod = sample.EntitlementVersion == 2 && sample.InputBytes == 10 && sample.OutputBytes == 10 && !sample.Uncertain
		}
	}
	if !old || !review || !newPeriod {
		t.Fatal("billing boundary lost or mixed usage")
	}
	month := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	s.recordV2TrafficLocked(p, 130, 240, 1, month)
	assertTrafficV2Checkpoint(t, s, false)
	for _, sample := range s.v2Traffic.Pending {
		if sample.CollectedUntil == month && !sample.Uncertain {
			t.Fatal("cross-month interval pretends precise month allocation")
		}
	}
}

func TestV2TrafficDiskFailureAndMemoryBoundDoNotExit(t *testing.T) {
	s, p, now := trafficV2Fixture(t)
	s.recordV2TrafficLocked(p, 10, 20, 1, now+1000)
	assertTrafficV2Checkpoint(t, s, false)
	batch := s.v2TrafficBatchLocked()
	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, []byte("test"), 0600); err != nil {
		t.Fatal(err)
	}
	s.cfg.StateDir = file
	if err := s.acknowledgeV2TrafficLocked(batch, []V2TrafficAck{{RuleID: p.rule.ID, Epoch: batch[0].Epoch, Sequence: 1}}); err == nil {
		t.Fatal("expected persistence failure")
	}
	if len(s.v2Traffic.Pending) != 1 || !s.v2AccountingDegradedLocked() {
		t.Fatal("failed durable ACK checkpoint lost data or warning")
	}
	s.recordV2TrafficLocked(p, 20, 40, 1, now+2000)
	if s.v2Traffic.Pending[batch[0].Epoch].InputBytes != 20 {
		t.Fatal("disk failure discarded in-memory counters")
	}
	for len(s.v2Traffic.Pending) < maxV2JournalRecords {
		key := fmt.Sprintf("buffer-epoch-%08d", len(s.v2Traffic.Pending))
		s.v2Traffic.Pending[key] = V2Traffic{Traffic: Traffic{Epoch: key}}
	}
	p.v2BillingPeriod = "another-grant"
	s.recordV2TrafficLocked(p, 30, 50, 1, now+3000)
	if len(s.v2Traffic.Pending) != maxV2JournalRecords || s.v2Traffic.GapInputBytes != 10 || s.v2Traffic.GapOutputBytes != 10 {
		t.Fatal("unbounded buffering or silent accounting gap")
	}
	if len(s.v2TrafficBatchLocked()) != MaxV2Traffic {
		t.Fatal("unbounded request batch")
	}
}

func TestV2TrafficInvalidCounterWarningSurvivesRestart(t *testing.T) {
	for _, negative := range []bool{true, false} {
		s, p, now := trafficV2Fixture(t)
		s.recordV2TrafficLocked(p, 10, 20, 1, now+1000)
		assertTrafficV2Checkpoint(t, s, false)
		input := int64(5)
		if negative {
			input = -1
		}
		s.recordV2TrafficLocked(p, input, 20, 1, now+2000)
		assertTrafficV2Checkpoint(t, s, true)
		loaded := &runtimeState{cfg: s.cfg}
		if err := loaded.initV2TrafficLocked(); err != nil {
			t.Fatal(err)
		}
		if !loaded.v2AccountingDegradedLocked() {
			t.Fatal("invalid counter warning disappeared on restart")
		}
		if len(loaded.v2Traffic.Pending) != 1 {
			t.Fatal("invalid counter discarded valid pending sample")
		}
	}
}
