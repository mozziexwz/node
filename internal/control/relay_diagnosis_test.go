package control

import (
	"testing"

	"github.com/mozziexwz/node/internal/relayruntime"
)

func TestRelayDiagnosticBrokerAcceptsOnlyMatchingAgentAndRule(t *testing.T) {
	var broker relayDiagnosticBroker
	job, err := broker.begin("member-001", "rule-001", "exit-001", "8.8.8.8:443")
	if err != nil {
		t.Fatal(err)
	}
	defer broker.end(job.id)
	if _, err := broker.begin("member-001", "rule-002", "exit-001", "8.8.4.4:443"); err == nil {
		t.Fatal("same member started overlapping target probes")
	}
	wrongAgent := relayruntime.V2SyncResponse{Status: "ready"}
	broker.exchange("exit-002", []relayruntime.TargetProbeResult{{ID: job.id, RuleID: job.ruleID, Status: "success", LatencyMS: 1}}, &wrongAgent)
	if len(wrongAgent.TargetProbeAcks) != 0 || len(wrongAgent.TargetProbes) != 0 {
		t.Fatal("different agent received or completed target probe")
	}
	wrongRule := relayruntime.V2SyncResponse{Status: "ready"}
	broker.exchange("exit-001", []relayruntime.TargetProbeResult{{ID: job.id, RuleID: "rule-002", Status: "success", LatencyMS: 1}}, &wrongRule)
	if len(wrongRule.TargetProbeAcks) != 0 || len(wrongRule.TargetProbes) != 1 || wrongRule.TargetProbes[0].Target != "8.8.8.8:443" {
		t.Fatal("different rule completed or changed pinned target")
	}
	valid := relayruntime.V2SyncResponse{Status: "ready"}
	broker.exchange("exit-001", []relayruntime.TargetProbeResult{{ID: job.id, RuleID: job.ruleID, Status: "success", LatencyMS: 2}}, &valid)
	if len(valid.TargetProbeAcks) != 1 || len(valid.TargetProbes) != 0 {
		t.Fatal("matching report was not acknowledged")
	}
	select {
	case report := <-job.result:
		if report.Status != "success" || report.LatencyMS != 2 {
			t.Fatalf("wrong report: %+v", report)
		}
	default:
		t.Fatal("matching result was not delivered")
	}
}
