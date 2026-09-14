package control

// These predicates deliberately accept evidence of a possibly retained process,
// not only a fresh keep_last acknowledgement. Restore cannot infer stop from an
// expired lease, absent capability flag, or revoked management credential.
func restoreAgentNeedsRecovery(s *State, agent RelayAgent) bool {
	if agent.ProtocolVersion >= 2 || agent.OfflinePolicy == "keep_last" || agent.KeepLastConfirmed || agent.ReconcileState == "recovery_required" {
		return true
	}
	if _, ok := s.Docs["relay_v2_catalogs"][agent.ID]; ok {
		return true
	}
	for _, collection := range []string{"relay_v2_commands", "relay_v2_command_history"} {
		for _, command := range ListDocs[RelayV2Command](s, collection) {
			if agent.ID != "" && command.AgentID == agent.ID {
				return true
			}
		}
	}
	return false
}

func restoreSegmentMayKeepLast(s *State, segment RelaySegment) bool {
	if segment.ProtocolVersion >= 2 || segment.ConfigGeneration > 0 || segment.LastCommandID != "" {
		return true
	}
	agent, _ := LoadDoc[RelayAgent](s, "relay_agents", segment.AgentID)
	return restoreAgentNeedsRecovery(s, agent)
}

func restoreRuleNeedsRecovery(s *State, rule UserRule) bool {
	if rule.ReconcileState == "recovery_required" || restoredRelayRecoveryRequired(s) && len(rule.Segments) > 0 {
		return true
	}
	for _, segment := range rule.Segments {
		if restoreSegmentMayKeepLast(s, segment) {
			return true
		}
	}
	for _, collection := range []string{"relay_v2_commands", "relay_v2_command_history"} {
		for _, command := range ListDocs[RelayV2Command](s, collection) {
			if rule.ID != "" && command.RuleID == rule.ID {
				return true
			}
		}
	}
	return false
}

func restoredRelayRecoveryRequired(s *State) bool {
	control, _ := LoadDoc[RelayV2Control](s, "relay_v2_control", "default")
	return control.RecoveryRequired
}

func restoreRelayNeedsRecovery(s *State) bool {
	if restoredRelayRecoveryRequired(s) || len(s.Docs["relay_v2_catalogs"]) > 0 || len(s.Docs["relay_v2_commands"]) > 0 || len(s.Docs["relay_v2_command_history"]) > 0 {
		return true
	}
	for _, agent := range ListDocs[RelayAgent](s, "relay_agents") {
		if restoreAgentNeedsRecovery(s, agent) {
			return true
		}
	}
	for _, rule := range ListDocs[UserRule](s, "user_rules") {
		if restoreRuleNeedsRecovery(s, rule) {
			return true
		}
	}
	return false
}
