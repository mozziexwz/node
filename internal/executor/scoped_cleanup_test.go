package executor

import (
	"strings"
	"testing"
)

func TestScopedCleanupReportRejectsCrossTaskPaths(t *testing.T) {
	report := &CleanupReport{Scope: "relay", ManagedTaskID: "onep", Digest: strings.Repeat("a", 64), Items: []CleanupItem{{Kind: "directory", Path: "/etc/msboost-free/onep"}}}
	if !ValidCleanupReport(report, "relay", false) {
		t.Fatal("valid scoped inventory rejected")
	}
	report.Items[0].Path = "/etc/msboost-free/otherp"
	if ValidCleanupReport(report, "relay", false) {
		t.Fatal("other operation could be removed")
	}
	report.Items = nil
	report.ManagedTaskID = "../onep"
	if ValidCleanupReport(report, "relay", false) {
		t.Fatal("invalid identity accepted in empty inventory")
	}
	report.ManagedTaskID = "onep"
	if !ValidCleanupReport(report, "relay", false) {
		t.Fatal("empty verified operation must be confirmable")
	}
}
