package control

import (
	"testing"
	"time"
)

func TestRelayFirewallFailureDoesNotAdvertiseReady(t *testing.T) {
	f := newRelayV2Fixture(t)
	commands := f.ready(t)
	ack := v2Ready(commands[0])
	ack.State = "persisted"
	ack.FirewallStatus = "error"
	f.sync(t, 0, ack)
	check := func(expected string) {
		t.Helper()
		if err := f.app.Store.View(func(s *State) error {
			rule, _ := LoadDoc[UserRule](s, "user_rules", f.user.ID+":route")
			view := relayRuleView(s, rule, false, time.Now().UnixMilli())
			if view.SyncState != expected {
				t.Fatalf("want %s got %s", expected, view.SyncState)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	check("firewall_pending")
	ack.State = "ready"
	ack.FirewallStatus = "ready"
	f.sync(t, 0, ack)
	check("active")
	if err := f.app.Store.View(func(s *State) error {
		for _, status := range []string{"error", "pending", "arbitrary text"} {
			bad := ack
			bad.FirewallStatus = status
			if relayV2KnownAck(s, f.agents[0], bad) {
				t.Fatal("accepted false ready", status)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
