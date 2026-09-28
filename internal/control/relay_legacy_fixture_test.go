// Historical-state simulator for migration/accounting fixtures only.
// The released server has no lease synchronization implementation.
package control

import (
	"errors"
	"github.com/mozziexwz/node/internal/relayruntime"
	"net/http"
	"time"
)

func (a *App) legacyRelaySyncFixture(w http.ResponseWriter, r *http.Request) {
	var in relayruntime.SyncRequest
	if err := Decode(r, &in); err != nil {
		commerceError(w, 400, err)
		return
	}
	if len(in.BootID) < 16 || len(in.BootID) > 100 || in.Sequence < 1 || len(in.Acks) > 10000 || len(in.Traffic) > 10000 || len(in.Version) > 100 || len(in.Capabilities) > 16 {
		commerceError(w, 400, errors.New("无效同步数据"))
		return
	}
	seenCapabilities := map[string]bool{}
	for _, capability := range in.Capabilities {
		if capability == "" || len(capability) > 64 || seenCapabilities[capability] {
			commerceError(w, 400, errors.New("无效同步能力"))
			return
		}
		seenCapabilities[capability] = true
	}
	now := time.Now().UnixMilli()
	out := relayruntime.SyncResponse{ServerTime: now, LeaseSeconds: relayLeaseMS / 1000, Rules: []relayruntime.Rule{}}
	errorStatus := http.StatusUnauthorized
	err := a.Store.Update(func(s *State) error {
		agent, err := a.relayAgentIdentity(s, r)
		if err != nil {
			return err
		}
		if agent.ProtocolVersion == relayruntime.ProtocolV2 {
			errorStatus = http.StatusConflict
			return errors.New("该节点已启用 v2，拒绝静默回退到短租约；保留最后配置并使用 v2 同步")
		}
		if agent.ReconcileState == "recovery_required" {
			errorStatus = http.StatusConflict
			return errors.New("该节点处于恢复核对，禁止自动覆盖运行配置")
		}
		if agent.BootID != in.BootID {
			for _, rule := range ListDocs[UserRule](s, "user_rules") {
				changed := false
				for i := range rule.Segments {
					if rule.Segments[i].AgentID == agent.ID {
						rule.Segments[i].AckState = "pending"
						rule.Segments[i].AckAt = 0
						changed = true
					}
				}
				if changed {
					if rule.State == "active" {
						rule.State = "pending"
					}
					if err := SaveDoc(s, "user_rules", rule.UserID+":"+rule.RouteID, rule); err != nil {
						return err
					}
				}
			}
			agent.BootID = in.BootID
		}
		agent.LastSeen = now
		agent.Version = in.Version
		agent.Capabilities = append([]string(nil), in.Capabilities...)
		if err := SaveDoc(s, "relay_agents", agent.ID, agent); err != nil {
			return err
		}
		for _, sample := range in.Traffic {
			if err := applyRelayTraffic(s, agent, sample, now); err != nil {
				return err
			}
		}
		// Policy changes invalidate all rules/hops before old-version ACKs are read.
		if err := relayCleanup(s, now); err != nil {
			return err
		}
		seqKey := agent.ID + ":" + in.BootID
		prev, _ := LoadDoc[int64](s, "relay_sync_sequences", seqKey)
		if in.Sequence > prev {
			for _, rule := range ListDocs[UserRule](s, "user_rules") {
				changed := false
				for i := range rule.Segments {
					seg := &rule.Segments[i]
					if seg.AgentID != agent.ID {
						continue
					}
					for _, ack := range in.Acks {
						if ack.ID == rule.ID && ack.Version == rule.Version && (ack.State == "ready" || ack.State == "failed" || ack.State == "stopped") {
							seg.AckState = ack.State
							seg.AckAt = now
							changed = true
							if ack.State == "failed" && rule.State != "revoking" && rule.State != "paused" {
								rule.State = "failed"
							}
						}
					}
				}
				if changed {
					if err := SaveDoc(s, "user_rules", rule.UserID+":"+rule.RouteID, rule); err != nil {
						return err
					}
				}
			}
			if err := SaveDoc(s, "relay_sync_sequences", seqKey, in.Sequence); err != nil {
				return err
			}
		}
		if err := relayCleanup(s, now); err != nil {
			return err
		}
		for _, rule := range ListDocs[UserRule](s, "user_rules") {
			user := s.Users[rule.UserID]
			if !agent.Enabled || !relayEntitled(user, now) || rule.State == "revoking" || rule.State == "paused" || rule.State == "failed" {
				continue
			}
			route, ok := LoadDoc[Route](s, "routes", rule.RouteID)
			if !ok || !route.Enabled || !userCanUseRoute(user, route) {
				continue
			}
			rotateTLS := !relayRuleUsesV2(s, rule) && rule.TLSExpiresAt > 0 && rule.TLSExpiresAt-now < 7*commerceDay
			if rotateTLS {
				stages := append([]RouteStage{{AgentIDs: []string{route.EntryAgentID}, Protocol: "tcp", Strategy: "round"}}, route.Hops...)
				stages = append(stages, route.Exit)
				if err := a.configureRelayTLS(&rule, stages, user.ExpiresAt); err != nil {
					return err
				}
			}
			required, _ := relayEffective(s, route)
			if required && !rule.HasFront && rule.State != "awaiting_front" {
				continue
			}
			if len(rule.Segments) > 0 && rotateTLS {
				rule.Version++
				for i := range rule.Segments {
					rule.Segments[i].Runtime.Version = rule.Version
					rule.Segments[i].AckState = "pending"
					rule.Segments[i].AckAt = 0
				}
				if rule.State != "awaiting_front" {
					rule.State = "pending"
				}
			}
			allReady, downstreamReady := len(rule.Segments) > 0, true
			for _, seg := range rule.Segments {
				ready := relaySegmentReady(s, seg, now)
				allReady = allReady && ready
				if !seg.Runtime.Billing {
					downstreamReady = downstreamReady && ready
				}
			}
			if allReady && rule.State != "awaiting_front" {
				rule.State = "active"
			} else if rule.State == "active" {
				rule.State = "pending"
			}
			for i := range rule.Segments {
				seg := &rule.Segments[i]
				if seg.AgentID != agent.ID {
					continue
				}
				if seg.Runtime.Billing && !downstreamReady {
					continue
				}
				runtimeRule := seg.Runtime
				if runtimeRule.Protocol == "tls" {
					plain, err := a.Open(seg.SealedTLSKey)
					if err != nil {
						return errors.New("TLS节点密钥解密失败")
					}
					runtimeRule.TLSPrivateKey = string(plain)
				}
				runtimeRule.LeaseUntil = now + relayLeaseMS
				if user.ExpiresAt < runtimeRule.LeaseUntil {
					runtimeRule.LeaseUntil = user.ExpiresAt
				}
				seg.LastLease = runtimeRule.LeaseUntil
				out.Rules = append(out.Rules, runtimeRule)
			}
			if err := SaveDoc(s, "user_rules", rule.UserID+":"+rule.RouteID, rule); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		commerceError(w, errorStatus, err)
		return
	}
	WriteJSON(w, 200, out)
}
