package control

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestRelayRecoveryManualRequestContracts(t *testing.T) {
	// The inspect display-only review is part of the editable prepare request;
	// retaining it must not make the documented inspect->prepare flow fail strict
	// input parsing. This test deliberately does not grant any DB authorization.
	request := RelayRecoveryRequest{Fingerprint: strings.Repeat("a", 64), Review: &RelayRecoveryReport{AgentID: "node", RecoveryID: "recovery"}, Decisions: []RelayRecoverySelection{{RuleID: "rule", Action: "hold"}}}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var decoded RelayRecoveryRequest
	if !backupPauseDecode(raw, &decoded) || decoded.Review == nil || decoded.Decisions[0].Action != "hold" {
		t.Fatal("inspect template is not a valid editable prepare request")
	}

	// TLS inspection returns a REPORT, not that editable request shape. Verify
	// the manual's exact JSON block is accepted after placeholder substitution,
	// and that pasting the full inspection report remains fail-closed.
	doc, err := os.ReadFile("../../docs/relay-recovery.md")
	if err != nil {
		t.Fatal(err)
	}
	const fence = "```json\n"
	text := strings.ReplaceAll(string(doc), "\r\n", "\n")
	start := strings.Index(text, fence)
	if start < 0 {
		t.Fatal("manual TLS request block missing")
	}
	text = text[start+len(fence):]
	end := strings.Index(text, "\n```")
	if end < 0 {
		t.Fatal("manual TLS request fence missing")
	}
	frame := strings.NewReplacer("<报告中的真实规则ID>", "rule-123", "<报告中的完整64位指纹>", strings.Repeat("a", 64), "<同一个真实规则ID>", "rule-123").Replace(text[:end])
	var tls RelayTLSMaintenanceRequest
	if !backupPauseDecode([]byte(frame), &tls) || tls.RuleID != "rule-123" || tls.Fingerprint != strings.Repeat("a", 64) || tls.Confirmation != "ROTATE_TLS_INTERRUPTS_RULE rule-123" {
		t.Fatal("manual TLS three-field request is incompatible with strict parser")
	}
	raw, err = json.Marshal(RelayTLSMaintenanceReport{RuleID: tls.RuleID, Fingerprint: tls.Fingerprint, TLSExpiresAt: 123, Segments: 2, Message: "report only"})
	if err != nil {
		t.Fatal(err)
	}
	if backupPauseDecode(raw, &tls) {
		t.Fatal("report-only fields accepted as authorization request")
	}
}
