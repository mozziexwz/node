package relayruntime

import (
	"context"
	"net"
	"sort"
	"strconv"
	"time"
)

type targetProbeRecord struct {
	result    TargetProbeResult
	expiresAt int64
}

func (s *runtimeState) targetProbeBatchLocked() []TargetProbeResult {
	if s.v2.targetProbeResults == nil {
		return nil
	}
	now := time.Now().UnixMilli()
	ids := make([]string, 0, len(s.v2.targetProbeResults))
	for id, record := range s.v2.targetProbeResults {
		if record.expiresAt <= now {
			delete(s.v2.targetProbeResults, id)
		} else {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	results := make([]TargetProbeResult, 0, min(len(ids), MaxTargetProbes))
	for _, id := range ids {
		if len(results) == MaxTargetProbes {
			break
		}
		results = append(results, s.v2.targetProbeResults[id].result)
	}
	return results
}

// targetProbePermittedLocked binds the challenge to a live local GOST rule.
// The controller cannot ask a relay to scan an arbitrary destination.
func (s *runtimeState) targetProbePermittedLocked(request TargetProbeRequest) bool {
	p := s.processes[request.RuleID]
	if p == nil || p.stopping || processDone(p) || p.ack.State != "ready" || p.rule.ID != request.RuleID {
		return false
	}
	host, rawPort, err := net.SplitHostPort(request.Target)
	if err != nil || net.ParseIP(host) == nil {
		return false
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil || port < 1 || port > 65535 {
		return false
	}
	for _, target := range p.rule.Targets {
		if target == request.Target {
			return true
		}
	}
	return false
}

func (s *runtimeState) acceptTargetProbeResponse(ctx context.Context, sent []TargetProbeResult, response V2SyncResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.v2.targetProbeResults == nil {
		s.v2.targetProbeResults = make(map[string]targetProbeRecord)
	}
	if s.v2.targetProbeRunning == nil {
		s.v2.targetProbeRunning = make(map[string]bool)
	}
	sentIDs := make(map[string]bool, len(sent))
	for _, result := range sent {
		sentIDs[result.ID] = true
	}
	for _, id := range response.TargetProbeAcks {
		if sentIDs[id] {
			delete(s.v2.targetProbeResults, id)
		}
	}
	now := time.Now().UnixMilli()
	for _, request := range response.TargetProbes {
		if !validV2ID(request.ID) || !validV2ID(request.RuleID) || request.ExpiresAt <= now || request.ExpiresAt > now+int64(30*time.Second/time.Millisecond) {
			continue
		}
		if _, done := s.v2.targetProbeResults[request.ID]; done || s.v2.targetProbeRunning[request.ID] || len(s.v2.targetProbeRunning) >= MaxTargetProbes {
			continue
		}
		if !s.targetProbePermittedLocked(request) {
			s.v2.targetProbeResults[request.ID] = targetProbeRecord{result: TargetProbeResult{ID: request.ID, RuleID: request.RuleID, Status: "unavailable"}, expiresAt: request.ExpiresAt}
			select {
			case s.v2.wake <- struct{}{}:
			default:
			}
			continue
		}
		s.v2.targetProbeRunning[request.ID] = true
		go s.runTargetProbe(ctx, request)
	}
}

func (s *runtimeState) runTargetProbe(parent context.Context, request TargetProbeRequest) {
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	start := time.Now()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", request.Target)
	latency := time.Since(start).Milliseconds()
	status := "success"
	if err != nil {
		status = "failed"
		latency = 0
	} else {
		_ = conn.Close()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.v2.targetProbeRunning, request.ID)
	if time.Now().UnixMilli() >= request.ExpiresAt || s.v2.disk.RecoveryRequired {
		return
	}
	s.v2.targetProbeResults[request.ID] = targetProbeRecord{result: TargetProbeResult{ID: request.ID, RuleID: request.RuleID, Status: status, LatencyMS: latency}, expiresAt: request.ExpiresAt}
	select {
	case s.v2.wake <- struct{}{}:
	default:
	}
}
