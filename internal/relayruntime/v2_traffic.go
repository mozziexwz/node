package relayruntime

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const maxV2JournalRecords = 4096

type v2TrafficMeter struct {
	input, output, at    int64
	period, month, epoch string
	version, entitlement int64
	sample               V2Traffic
}

type v2TrafficState struct {
	SchemaVersion  int                  `json:"schemaVersion"`
	Pending        map[string]V2Traffic `json:"pending"`
	Degraded       bool                 `json:"degraded"`
	GapInputBytes  int64                `json:"gapInputBytes"`
	GapOutputBytes int64                `json:"gapOutputBytes"`
	meters         map[string]*v2TrafficMeter
	batchOffset    int
}

func (s *runtimeState) initV2TrafficLocked() error {
	state := &v2TrafficState{SchemaVersion: 2, Pending: map[string]V2Traffic{}, meters: map[string]*v2TrafficMeter{}}
	path := filepath.Join(s.cfg.StateDir, "traffic-v2-journal.json")
	info, err := os.Lstat(path)
	if err == nil {
		if !info.Mode().IsRegular() || info.Size() > 8<<20 {
			return errors.New("invalid v2 traffic journal file")
		}
		if readV2PrivateJSON(path, state) != nil || state.SchemaVersion != 2 || state.Pending == nil || len(state.Pending) > maxV2JournalRecords || state.GapInputBytes < 0 || state.GapOutputBytes < 0 {
			return errors.New("invalid v2 traffic journal")
		}
		for epoch, sample := range state.Pending {
			if sample.Epoch != epoch || len(epoch) < 16 || sample.ID == "" || sample.BillingPeriodID == "" || sample.Sequence < 1 || sample.InputBytes < 0 || sample.OutputBytes < 0 || sample.CollectedFrom < 1 || sample.CollectedUntil < sample.CollectedFrom {
				return errors.New("invalid v2 traffic record")
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	s.v2Traffic = state
	return nil
}

func (s *runtimeState) persistV2TrafficLocked(state *v2TrafficState) error {
	return writeV2PrivateJSON(filepath.Join(s.cfg.StateDir, "traffic-v2-journal.json"), state)
}

func saturatingTraffic(a, b int64) int64 {
	if b > math.MaxInt64-a {
		return math.MaxInt64
	}
	return a + b
}

func (s *runtimeState) v2TrafficGapLocked(input, output int64) {
	v := s.v2Traffic
	v.Degraded = true
	v.GapInputBytes = saturatingTraffic(v.GapInputBytes, input)
	v.GapOutputBytes = saturatingTraffic(v.GapOutputBytes, output)
}

// Observer totals are per GOST process. Journal epochs are independent: metadata
// changes and UTC-month boundaries roll counters without restarting the process.
// A boundary-spanning sample stays on the old grant and is explicitly uncertain.
func (s *runtimeState) recordV2TrafficLocked(p *process, input, output, connections, now int64) {
	if s.v2Traffic == nil || !p.rule.Billing {
		return
	}
	v := s.v2Traffic
	if input < 0 || output < 0 || now < 1 {
		v.Degraded = true
		_ = s.persistV2TrafficLocked(v)
		return
	}
	if connections < 0 {
		v.Degraded = true
	}
	m := v.meters[p.epoch]
	if m == nil {
		m = &v2TrafficMeter{at: p.startedAt, period: p.v2BillingPeriod, version: p.rule.Version, entitlement: p.rule.EntitlementVersion}
		if m.at < 1 || m.at > now {
			m.at = now
		}
		m.month = time.UnixMilli(m.at).UTC().Format("2006-01")
		v.meters[p.epoch] = m
	}
	if input < m.input || output < m.output {
		v.Degraded = true // unexpected reset cannot be silently treated as zero use
		_ = s.persistV2TrafficLocked(v)
		return
	}
	di, do := input-m.input, output-m.output
	month := time.UnixMilli(now).UTC().Format("2006-01")
	boundary := m.period != p.v2BillingPeriod || m.month != month || now < m.at
	if di != 0 || do != 0 {
		if m.epoch == "" || boundary {
			m.epoch = randomID()
		}
		sample := m.sample
		_, exists := v.Pending[m.epoch]
		if sample.Epoch != m.epoch {
			sample = V2Traffic{Traffic: Traffic{ID: p.rule.ID, Version: m.version, Epoch: m.epoch, EntitlementVersion: m.entitlement}, BillingPeriodID: m.period, CollectedFrom: m.at}
		}
		if m.period == "" || (!exists && len(v.Pending) >= maxV2JournalRecords) || sample.Sequence == math.MaxInt64 || di > math.MaxInt64-sample.InputBytes || do > math.MaxInt64-sample.OutputBytes {
			s.v2TrafficGapLocked(di, do)
		} else {
			sample.Sequence++
			sample.InputBytes += di
			sample.OutputBytes += do
			sample.Connections = max(0, connections)
			sample.CollectedUntil = max(now, sample.CollectedFrom)
			sample.Uncertain = sample.Uncertain || boundary
			v.Pending[m.epoch] = sample
			m.sample = sample
		}
	}
	m.input, m.output, m.at = input, output, now
	if boundary {
		m.period, m.month, m.epoch = p.v2BillingPeriod, month, ""
		m.version, m.entitlement = p.rule.Version, p.rule.EntitlementVersion
	}
	if err := s.persistV2TrafficLocked(v); err != nil {
		v.Degraded = true
	}
}

func (s *runtimeState) v2TrafficBatchLocked() []V2Traffic {
	if s.v2Traffic == nil {
		return nil
	}
	v := s.v2Traffic
	active := map[string]bool{}
	for _, p := range s.processes {
		active[p.epoch] = true
	}
	for epoch := range v.meters {
		if !active[epoch] {
			delete(v.meters, epoch)
		}
	}
	keys := make([]string, 0, len(v.Pending))
	for key := range v.Pending {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]V2Traffic, 0, min(len(keys), MaxV2Traffic))
	if len(keys) == 0 {
		return out
	}
	// Round robin also allows valid samples past an independently rejected one.
	for i := 0; i < min(len(keys), MaxV2Traffic); i++ {
		out = append(out, v.Pending[keys[(v.batchOffset+i)%len(keys)]])
	}
	v.batchOffset = (v.batchOffset + len(out)) % len(keys)
	return out
}

func (s *runtimeState) acknowledgeV2TrafficLocked(sent []V2Traffic, acks []V2TrafficAck) error {
	if s.v2Traffic == nil {
		return nil
	}
	v := s.v2Traffic
	copyState := *v
	copyState.Pending = make(map[string]V2Traffic, len(v.Pending))
	for key, sample := range v.Pending {
		copyState.Pending[key] = sample
	}
	sentByEpoch := make(map[string]V2Traffic, len(sent))
	for _, sample := range sent {
		sentByEpoch[sample.Epoch] = sample
	}
	for _, ack := range acks {
		sample, ok := sentByEpoch[ack.Epoch]
		current, pending := copyState.Pending[ack.Epoch]
		if ok && pending && ack.RuleID == sample.ID && ack.Sequence == sample.Sequence && current.ID == sample.ID && current.Sequence == sample.Sequence {
			delete(copyState.Pending, ack.Epoch)
		}
	}
	// Never clear in-memory state before the durable ACK checkpoint succeeds.
	if err := s.persistV2TrafficLocked(&copyState); err != nil {
		v.Degraded = true
		return err
	}
	s.v2Traffic = &copyState
	return nil
}

func (s *runtimeState) v2AccountingDegradedLocked() bool {
	return s.v2Traffic != nil && s.v2Traffic.Degraded
}
