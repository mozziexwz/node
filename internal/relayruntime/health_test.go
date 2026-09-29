package relayruntime

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRelayHealthRequiresCurrentInvocationAndFreshAuthenticatedSync(t *testing.T) {
	dir := t.TempDir()
	invocation := strings.Repeat("a", 32)
	origin := "https://panel.example"
	base := RelayHealth{AgentID: randomID(), InstanceID: randomID(), InvocationID: invocation, ServerURL: origin, Status: "online", ObservedAt: time.Now().UnixMilli()}
	for _, scenario := range []string{"valid", "old_process", "stale", "auth_error", "recovery_required", "other_panel", "future"} {
		t.Run(scenario, func(t *testing.T) {
			report := base
			switch scenario {
			case "old_process":
				report.InvocationID = strings.Repeat("b", 32)
			case "stale":
				report.ObservedAt -= 60000
			case "auth_error", "recovery_required":
				report.Status = scenario
			case "other_panel":
				report.ServerURL = "https://other.example"
			case "future":
				report.ObservedAt += 60000
			}
			if err := atomicPrivateJSON(filepath.Join(dir, relayHealthFile), report); err != nil {
				t.Fatal(err)
			}
			err := CheckRelayHealth(dir, invocation, origin)
			if (err == nil) != (scenario == "valid") {
				t.Fatalf("health=%v", err)
			}
		})
	}
}
