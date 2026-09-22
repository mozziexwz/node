package control

import (
	"errors"
	"math"
	"net"
	"slices"
	"strconv"

	"github.com/mozziexwz/node/internal/relayruntime"
)

type RelayTLSMaintenanceRequest struct {
	RuleID       string `json:"ruleId"`
	Fingerprint  string `json:"fingerprint"`
	Confirmation string `json:"confirmation"`
}
type RelayTLSMaintenanceReport struct {
	RuleID       string `json:"ruleId"`
	Fingerprint  string `json:"fingerprint"`
	TLSExpiresAt int64  `json:"tlsExpiresAt"`
	Segments     int    `json:"segments"`
	Message      string `json:"message"`
}

func relayTLSReviewFingerprint(s *State, rule UserRule) string {
	rule.TrafficBytes = 0
	rule.InputBytes, rule.OutputBytes, rule.TrafficEntitlementVersion = 0, 0, 0
	rule.Segments = append([]RelaySegment(nil), rule.Segments...)
	for i := range rule.Segments {
		seg := &rule.Segments[i]
		seg.AckAt, seg.RuntimeObservedAt, seg.LastLease = 0, 0, 0
		seg.RuntimeState = ""
	}
	route, _ := LoadDoc[Route](s, "routes", rule.RouteID)
	route.Online = false
	return recoveryDigest(struct {
		Rule  UserRule
		Route Route
	}{rule, route})
}

// Certificates must describe the already retained chain, not a stale route
// from an older database. This check never resolves DNS or changes targets.
func relayRetainedTopologyMatches(s *State, rule UserRule) bool {
	route, ok := LoadDoc[Route](s, "routes", rule.RouteID)
	if !ok {
		return false
	}
	stages := []RouteStage{{AgentIDs: []string{route.EntryAgentID}, Protocol: "tcp", Strategy: "round"}}
	if route.Type != "port_forward" && (len(route.Hops) > 0 || len(route.Exit.AgentIDs) != 1 || route.Exit.AgentIDs[0] != route.EntryAgentID) {
		stages = append(stages, route.Hops...)
		stages = append(stages, route.Exit)
	}
	segments := map[string]RelaySegment{}
	for _, seg := range rule.Segments {
		if _, exists := segments[seg.AgentID]; exists {
			return false
		}
		segments[seg.AgentID] = seg
	}
	seen := map[string]bool{}
	for i, stage := range stages {
		for _, id := range stage.AgentIDs {
			seg, ok := segments[id]
			if !ok || seen[id] || seg.Runtime.Protocol != stage.Protocol {
				return false
			}
			seen[id] = true
			if i+1 < len(stages) {
				targets := []string{}
				for _, nextID := range stages[i+1].AgentIDs {
					next, ok := LoadDoc[RelayAgent](s, "relay_agents", nextID)
					if !ok {
						return false
					}
					downstream, ok := segments[nextID]
					if !ok {
						return false
					}
					address, e := relayConnectionAddress(next, route.AddressPreference, stages[i+1].ConnectIP)
					if e != nil {
						return false
					}
					targets = append(targets, net.JoinHostPort(address, strconv.Itoa(downstream.Runtime.ListenPort)))
				}
				if !slices.Equal(targets, seg.Runtime.Targets) || seg.Runtime.Strategy != stages[i+1].Strategy {
					return false
				}
			}
		}
	}
	return len(seen) == len(segments)
}

func (a *App) relayTLSMaintenanceReport(s *State, id string) (RelayTLSMaintenanceReport, error) {
	for _, rule := range ListDocs[UserRule](s, "user_rules") {
		if rule.ID != id {
			continue
		}
		if rule.TLSExpiresAt <= 0 || len(rule.Segments) == 0 {
			return RelayTLSMaintenanceReport{}, errors.New("此规则没有受管 TLS 证书")
		}
		return RelayTLSMaintenanceReport{RuleID: id, Fingerprint: relayTLSReviewFingerprint(s, rule), TLSExpiresAt: rule.TLSExpiresAt, Segments: len(rule.Segments), Message: "证书轮换会改变所选整条规则的运行配置，可能中断其现有连接；不影响其它规则，不关闭 TLS 校验。先安排维护并检查全部节点确认。"}, nil
	}
	return RelayTLSMaintenanceReport{}, errRelayRecovery
}

func (a *App) rotateRelayTLS(input RelayTLSMaintenanceRequest, now int64) (report RelayTLSMaintenanceReport, err error) {
	if input.Confirmation != "ROTATE_TLS_INTERRUPTS_RULE "+input.RuleID || !backupPauseValidToken(input.Fingerprint) {
		return report, errRelayRecovery
	}
	err = a.Store.Update(func(s *State) error {
		if !boolSetting(s, "maintenance") || !backupPausePreflight(s, now).CanPauseControl {
			return errRelayRecovery
		}
		current, e := a.relayTLSMaintenanceReport(s, input.RuleID)
		if e != nil || current.Fingerprint != input.Fingerprint {
			return errRelayRecovery
		}
		for key, rule := range s.Docs["user_rules"] {
			var row UserRule
			if !backupPauseDecode(rule, &row) {
				return errRelayRecovery
			}
			if row.ID != input.RuleID {
				continue
			}
			if relayRecoveryRequired(s, row) || row.State != "active" || row.Version == math.MaxInt64 {
				return errRelayRecovery
			}
			if !relayRetainedTopologyMatches(s, row) {
				return errRelayRecovery
			}
			route, ok := LoadDoc[Route](s, "routes", row.RouteID)
			if !ok {
				return errRelayRecovery
			}
			stages := append([]RouteStage{{AgentIDs: []string{route.EntryAgentID}, Protocol: "tcp", Strategy: "round"}}, route.Hops...)
			stages = append(stages, route.Exit)
			if e := a.configureRelayTLS(&row, stages, now); e != nil {
				return e
			}
			row.Version++
			for i := range row.Segments {
				seg := &row.Segments[i]
				if seg.ProtocolVersion != relayruntime.ProtocolV2 {
					return errRelayRecovery
				}
				agent, ok := LoadDoc[RelayAgent](s, "relay_agents", seg.AgentID)
				if !ok {
					return errRelayRecovery
				}
				catalog, ok := LoadDoc[RelayV2Catalog](s, "relay_v2_catalogs", seg.AgentID)
				if !ok {
					return errRelayRecovery
				}
				action, _ := relayV2Desired(s, agent, row, now)
				if action != "upsert" {
					return errRelayRecovery
				}
				seg.Runtime.Version = row.Version
				if e := a.relayV2Issue(s, &catalog, agent, &row, i, "upsert", "root_tls_maintenance"); e != nil {
					return e
				}
				if e := SaveDoc(s, "relay_v2_catalogs", agent.ID, catalog); e != nil {
					return e
				}
			}
			if e := SaveDoc(s, "user_rules", key, row); e != nil {
				return e
			}
			if e := commerceAudit(s, "local-root", "relay.tls.rotate", row.ID); e != nil {
				return e
			}
			report = RelayTLSMaintenanceReport{RuleID: row.ID, Fingerprint: relayTLSReviewFingerprint(s, row), TLSExpiresAt: row.TLSExpiresAt, Segments: len(row.Segments), Message: "所选规则的新证书与显式更新命令已持久化；尚不代表所有节点已应用。请等待每段新代次确认，不自动轮换其它规则。"}
			return nil
		}
		return errRelayRecovery
	})
	return report, err
}
