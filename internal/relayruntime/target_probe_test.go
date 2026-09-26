package relayruntime

import (
	"context"
	"net"
	"testing"
	"time"
)

func targetProbeFixture(target string) *runtimeState {
	ruleID := "target-probe-rule-001"
	return &runtimeState{
		processes: map[string]*process{ruleID: {
			rule: Rule{ID: ruleID, Targets: []string{target}},
			ack:  Ack{State: "ready"},
			done: make(chan struct{}),
		}},
		v2: &v2RuntimeState{wake: make(chan struct{}, 1)},
	}
}

func TestTargetProbeUsesRunningRuleEndpointAndReportsNodeConnectTime(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan struct{}, 1)
	go func() {
		connection, err := listener.Accept()
		if err == nil {
			accepted <- struct{}{}
			_ = connection.Close()
		}
	}()
	s := targetProbeFixture(listener.Addr().String())
	request := TargetProbeRequest{ID: "probe-001", RuleID: "target-probe-rule-001", Target: listener.Addr().String(), ExpiresAt: time.Now().Add(10 * time.Second).UnixMilli()}
	s.acceptTargetProbeResponse(context.Background(), nil, V2SyncResponse{TargetProbes: []TargetProbeRequest{request}})
	select {
	case <-s.v2.wake:
	case <-time.After(2 * time.Second):
		t.Fatal("node did not finish bounded target probe")
	}
	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Fatal("configured target did not observe node connection")
	}
	s.mu.Lock()
	results := s.targetProbeBatchLocked()
	s.mu.Unlock()
	if len(results) != 1 || results[0].Status != "success" || results[0].RuleID != request.RuleID || results[0].LatencyMS < 0 {
		t.Fatalf("wrong node probe result: %+v", results)
	}
	s.acceptTargetProbeResponse(context.Background(), results, V2SyncResponse{TargetProbeAcks: []string{request.ID}})
	s.mu.Lock()
	remaining := s.targetProbeBatchLocked()
	s.mu.Unlock()
	if len(remaining) != 0 {
		t.Fatalf("acknowledged result was resent: %+v", remaining)
	}
}

func TestTargetProbeRejectsUnconfiguredEndpointAndUnsentAck(t *testing.T) {
	s := targetProbeFixture("127.0.0.1:12345")
	request := TargetProbeRequest{ID: "probe-002", RuleID: "target-probe-rule-001", Target: "127.0.0.1:54321", ExpiresAt: time.Now().Add(10 * time.Second).UnixMilli()}
	s.acceptTargetProbeResponse(context.Background(), nil, V2SyncResponse{TargetProbes: []TargetProbeRequest{request}})
	s.mu.Lock()
	results := s.targetProbeBatchLocked()
	s.mu.Unlock()
	if len(results) != 1 || results[0].Status != "unavailable" || results[0].LatencyMS != 0 {
		t.Fatalf("unconfigured target was probed or hidden: %+v", results)
	}
	s.acceptTargetProbeResponse(context.Background(), nil, V2SyncResponse{TargetProbeAcks: []string{request.ID}})
	s.mu.Lock()
	remaining := s.targetProbeBatchLocked()
	s.mu.Unlock()
	if len(remaining) != 1 {
		t.Fatal("unsent acknowledgement erased target probe result")
	}
}
