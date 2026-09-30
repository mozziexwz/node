package relayruntime

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFirewallReportingRequiresPeerNegotiation(t *testing.T) {
	id := "firewall-test-rule"
	s := &runtimeState{processes: map[string]*process{id: {rule: Rule{ID: id}, ack: Ack{State: "pending"}, firewallStatus: "error", done: make(chan struct{})}}, v2: &v2RuntimeState{disk: v2DiskState{AgentID: "agent", Records: map[string]v2Record{id: {Command: V2Command{RuleID: id, Action: "upsert"}, State: "persisted"}}}}}
	if err := s.initV2TrafficLocked(); err != nil {
		t.Fatal(err)
	}
	for _, enabled := range []bool{false, true, false} {
		s.firewallReporting = enabled
		request := s.v2Request("instance")
		raw, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "firewallStatus") != enabled {
			t.Fatal("unnegotiated optional field", string(raw))
		}
		if len(request.Acks) != 1 || request.Acks[0].State != "persisted" {
			t.Fatal("local readiness was bypassed")
		}
	}
}

func TestFirewallOptionalFieldFallbackDoesNotRetryAuthOrCurrentPeerErrors(t *testing.T) {
	for _, status := range []int{400, 401, 403} {
		for _, current := range []bool{false, true} {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if current {
					w.Header().Set("X-MSBOOST-Relay-Firewall", "1")
				}
				body, _ := io.ReadAll(r.Body)
				if calls == 1 || strings.Contains(string(body), "firewallStatus") {
					w.WriteHeader(status)
					return
				}
				io.WriteString(w, `{"protocolVersion":2,"status":"ready"}`)
			}))
			_, err := callV2(context.Background(), Config{ServerURL: server.URL}, "test-token", V2SyncRequest{Acks: []V2Ack{{State: "persisted", FirewallStatus: "error"}}})
			server.Close()
			wantFallback := status == 400 && !current
			if wantFallback && (err != nil || calls != 2) || !wantFallback && (err == nil || calls != 1) {
				t.Fatal(status, current, calls, err)
			}
		}
	}
}
