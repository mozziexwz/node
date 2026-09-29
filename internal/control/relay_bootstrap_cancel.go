package control

import (
	"encoding/json"
	"strings"
	"time"
)

// This exception is deliberately narrower than "no current user rules".
// A catalogue that never accepted an instance or issued a revision can only
// represent a rejected first handshake. Missing or damaged history is not
// accepted as evidence that a previously running relay stopped.
func relayRejectedBootstrap(s *State, agent RelayAgent) (RelayV2Catalog, bool) {
	var catalog RelayV2Catalog
	if agent.ID == "" || agent.LastSeen != 0 || agent.BootID != "" || agent.KeepLastConfirmed || agent.AccountingDegraded || agent.ProtocolVersion != 2 || agent.OfflinePolicy != "keep_last" || agent.ReconcileState != "recovery_required" {
		return catalog, false
	}
	if !backupPauseDecode(s.Docs["relay_v2_catalogs"][agent.ID], &catalog) || catalog.AgentID != agent.ID || !relayV2Identifier(catalog.ControlEpoch) || catalog.InstanceID != "" || catalog.Sequence != 0 || catalog.Revision != 0 || catalog.DispatchCursor != 0 || catalog.ReconcileState != "recovery_required" {
		return catalog, false
	}
	var references func(any) bool
	references = func(value any) bool {
		switch v := value.(type) {
		case string:
			return v == agent.ID
		case []any:
			for _, item := range v {
				if references(item) {
					return true
				}
			}
		case map[string]any:
			for key, item := range v {
				if key == agent.ID || strings.HasPrefix(key, agent.ID+":") || references(item) {
					return true
				}
			}
		}
		return false
	}
	for collection, rows := range s.Docs {
		if collection == "relay_agents" || collection == "relay_v2_catalogs" || collection == "relay_v2_control" || collection == relayRetirementCollection {
			continue
		}
		if !strings.HasPrefix(collection, "relay_") && collection != "routes" && collection != "user_rules" && collection != "traffic_cursors" {
			continue
		}
		for key, raw := range rows {
			switch collection {
			case "relay_v2_commands", "relay_v2_command_history":
				var command RelayV2Command
				if !backupPauseDecode(raw, &command) || !backupPauseCommandValid(command) {
					return catalog, false
				}
			case "user_rules", "relay_rule_archive":
				var rule UserRule
				if !backupPauseDecode(raw, &rule) || rule.ID == "" || rule.UserID == "" || rule.RouteID == "" {
					return catalog, false
				}
			case "routes":
				var route Route
				if !backupPauseDecode(raw, &route) || route.ID != key || key == "" || route.EntryAgentID == "" {
					return catalog, false
				}
			}
			var value any
			if json.Unmarshal(raw, &value) != nil || value == nil || key == agent.ID || strings.HasPrefix(key, agent.ID+":") || references(value) {
				return catalog, false
			}
		}
	}
	return catalog, true
}

// Correct the v2.0.1 empty-restore latch only when the complete relay domain
// contains nothing except positively cancelled, never-activated registrations.
// No runtime inventory, billing history or unknown record is discarded.
func relayClearEmptyRestoreLatch(s *State) error {
	control, ok := LoadDoc[RelayV2Control](s, "relay_v2_control", "default")
	if !ok || !control.RecoveryRequired {
		return nil
	}
	retired, err := validateRelayRetirements(s, time.Now().UnixMilli())
	if err != nil {
		return err
	}
	for _, record := range retired {
		if record.Version != 2 {
			return nil
		}
	}
	for collection, rows := range s.Docs {
		if len(rows) == 0 {
			continue
		}
		switch collection {
		case "relay_v2_control", relayRetirementCollection:
		case "relay_v2_catalogs":
			for id := range rows {
				if _, ok := retired[id]; !ok {
					return nil
				}
			}
		case "relay_agents":
			for _, raw := range rows {
				var agent RelayAgent
				if !backupPauseDecode(raw, &agent) || restoreAgentNeedsRecovery(s, agent) || agent.LastSeen != 0 || agent.BootID != "" {
					return nil
				}
			}
		case "user_rules", "relay_rule_archive", "traffic_cursors":
			return nil
		default:
			if strings.HasPrefix(collection, "relay_v2_") {
				return nil
			}
		}
	}
	control.RecoveryRequired = false
	return SaveDoc(s, "relay_v2_control", "default", control)
}
