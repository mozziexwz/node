package control

import (
	"errors"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/mozziexwz/node/internal/relayruntime"
)

func (a *App) relayRenewEnrollment(w http.ResponseWriter, r *http.Request) {
	admin, err := a.Admin(r)
	if err != nil {
		commerceError(w, 403, err)
		return
	}
	token := commerceID() + commerceID()
	expires := time.Now().Add(15 * time.Minute).UnixMilli()
	err = a.Store.Update(func(s *State) error {
		agent, ok := LoadDoc[RelayAgent](s, "relay_agents", r.PathValue("id"))
		if !ok {
			return errors.New("Agent不存在")
		}
		agent.EnrollmentHash = commerceHash(token)
		agent.EnrollmentExpires = expires
		agent.TokenHash = ""
		agent.LastSeen = 0
		if err := SaveDoc(s, "relay_agents", agent.ID, agent); err != nil {
			return err
		}
		return commerceAudit(s, admin.ID, "relay_agent.reenroll", agent.ID)
	})
	if err != nil {
		commerceError(w, 409, err)
		return
	}
	WriteJSON(w, 200, map[string]any{"enrollmentToken": token, "expiresAt": expires, "previousLeaseExpiresAt": time.Now().UnixMilli() + relayLeaseMS, "installArgs": a.relayInstallationInstructions(), "message": "旧令牌已撤销。请停止旧进程，最迟45秒旧租约失效后使用新注册令牌启动。"})
}

func (a *App) relayRegisterAgent(w http.ResponseWriter, r *http.Request) {
	var in struct {
		EnrollmentToken string `json:"enrollmentToken"`
	}
	if err := Decode(r, &in); err != nil {
		commerceError(w, 400, err)
		return
	}
	if len(in.EnrollmentToken) < 40 || len(in.EnrollmentToken) > 200 {
		commerceError(w, 401, errors.New("注册令牌无效"))
		return
	}
	token := commerceID() + commerceID()
	id := ""
	err := a.Store.Update(func(s *State) error {
		for _, agent := range ListDocs[RelayAgent](s, "relay_agents") {
			if agent.EnrollmentHash == commerceHash(in.EnrollmentToken) && agent.EnrollmentExpires > time.Now().UnixMilli() && agent.Enabled {
				agent.TokenHash = commerceHash(token)
				agent.EnrollmentHash = ""
				agent.EnrollmentExpires = 0
				id = agent.ID
				return SaveDoc(s, "relay_agents", agent.ID, agent)
			}
		}
		return errors.New("注册令牌无效、已使用或已过期")
	})
	if err != nil {
		commerceError(w, 401, err)
		return
	}
	WriteJSON(w, 200, map[string]any{"agentId": id, "token": token, "capability": "relay"})
}
func (a *App) relayAgentIdentity(s *State, r *http.Request) (RelayAgent, error) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == r.Header.Get("Authorization") || len(token) < 40 || len(token) > 200 {
		return RelayAgent{}, errors.New("relay鉴权失败")
	}
	hash := commerceHash(token)
	for _, agent := range ListDocs[RelayAgent](s, "relay_agents") {
		if agent.TokenHash == hash && agent.Capability == "relay" {
			return agent, nil
		}
	}
	return RelayAgent{}, errors.New("relay令牌不存在")
}

// applyRelayTraffic accounts only the designated ingress segment. Cumulative
// counters are de-duplicated across request retries and process epochs.
func applyRelayTraffic(s *State, agent RelayAgent, report relayruntime.Traffic, now int64) error {
	if report.Sequence < 1 || report.InputBytes < 0 || report.OutputBytes < 0 || len(report.Epoch) < 16 || len(report.Epoch) > 100 {
		return errors.New("流量样本无效")
	}
	var rule UserRule
	found := false
	for _, candidate := range ListDocs[UserRule](s, "user_rules") {
		if candidate.ID == report.ID {
			rule = candidate
			found = true
			break
		}
	}
	if !found {
		rule, found = LoadDoc[UserRule](s, "relay_rule_archive", report.ID)
		if !found {
			return nil
		}
	}
	_, activeRule := LoadDoc[UserRule](s, "user_rules", rule.UserID+":"+rule.RouteID)
	if current, ok := LoadDoc[UserRule](s, "user_rules", rule.UserID+":"+rule.RouteID); ok && current.ID != rule.ID {
		activeRule = false
	}
	owns, billing := false, false
	for _, seg := range rule.Segments {
		if seg.AgentID == agent.ID && report.Version > 0 && report.Version <= seg.Runtime.Version {
			owns = true
			billing = seg.Runtime.Billing
			break
		}
	}
	if !owns {
		return nil
	}
	key := agent.ID + ":" + rule.ID + ":" + report.Epoch
	cursor, _ := LoadDoc[TrafficCursor](s, "traffic_cursors", key)
	if report.Sequence <= cursor.Sequence {
		return nil
	}
	if report.InputBytes < cursor.InputBytes || report.OutputBytes < cursor.OutputBytes {
		return errors.New("同启动周期累计流量不得回退")
	}
	inputDelta, outputDelta := report.InputBytes-cursor.InputBytes, report.OutputBytes-cursor.OutputBytes
	delta, remainder, err := relayWeightedTraffic(inputDelta, outputDelta, rule.TrafficMode, rule.TrafficMultiplierPermille, cursor.Remainder)
	if err != nil {
		return err
	}
	if billing {
		user := s.Users[rule.UserID]
		if user != nil && report.EntitlementVersion == entitlementVersion(s, rule.UserID) {
			if user.TrafficUsed > math.MaxInt64-delta {
				return errors.New("流量累计溢出")
			}
			user.TrafficUsed += delta
		}
		if rule.TrafficBytes > math.MaxInt64-delta {
			return errors.New("规则流量溢出")
		}
		rule.TrafficBytes += delta
		monthKey := rule.UserID + ":" + time.UnixMilli(now).UTC().Format("2006-01")
		month, _ := LoadDoc[int64](s, "traffic_months", monthKey)
		if month > math.MaxInt64-delta {
			return errors.New("月流量溢出")
		}
		if err := SaveDoc(s, "traffic_months", monthKey, month+delta); err != nil {
			return err
		}
		collection, key := "relay_rule_archive", rule.ID
		if activeRule {
			collection = "user_rules"
			key = rule.UserID + ":" + rule.RouteID
		}
		if err := SaveDoc(s, collection, key, rule); err != nil {
			return err
		}
	}
	return SaveDoc(s, "traffic_cursors", key, TrafficCursor{Sequence: report.Sequence, InputBytes: report.InputBytes, OutputBytes: report.OutputBytes, Remainder: remainder})
}

func relayWeightedTraffic(input, output int64, mode string, multiplier, remainder int64) (int64, int64, error) {
	if multiplier == 0 {
		multiplier = 1000
	}
	if input < 0 || output < 0 || multiplier < 1 || multiplier > 100000 || remainder < 0 || remainder >= 1000 {
		return 0, 0, errors.New("流量倍率或样本无效")
	}
	base := input
	switch mode {
	case "upload":
	case "download":
		base = output
	case "", "both":
		if input > math.MaxInt64-output {
			return 0, 0, errors.New("流量数据溢出")
		}
		base = input + output
	default:
		return 0, 0, errors.New("流量模式无效")
	}
	if base/1000 > math.MaxInt64/multiplier {
		return 0, 0, errors.New("倍率流量溢出")
	}
	whole := base / 1000 * multiplier
	fraction := (base%1000)*multiplier + remainder
	if whole > math.MaxInt64-fraction/1000 {
		return 0, 0, errors.New("倍率流量溢出")
	}
	return whole + fraction/1000, fraction % 1000, nil
}

func (a *App) relaySyncAgent(w http.ResponseWriter, r *http.Request) {
	var in relayruntime.SyncRequest
	if err := Decode(r, &in); err != nil {
		commerceError(w, 400, err)
		return
	}
	if len(in.BootID) < 16 || len(in.BootID) > 100 || in.Sequence < 1 || len(in.Acks) > 10000 || len(in.Traffic) > 10000 || len(in.Version) > 100 {
		commerceError(w, 400, errors.New("无效同步数据"))
		return
	}
	now := time.Now().UnixMilli()
	out := relayruntime.SyncResponse{ServerTime: now, LeaseSeconds: relayLeaseMS / 1000, Rules: []relayruntime.Rule{}}
	err := a.Store.Update(func(s *State) error {
		agent, err := a.relayAgentIdentity(s, r)
		if err != nil {
			return err
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
			if !ok || !route.Enabled {
				continue
			}
			rotateTLS := rule.TLSExpiresAt > 0 && rule.TLSExpiresAt-now < 7*commerceDay
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
		commerceError(w, 401, err)
		return
	}
	WriteJSON(w, 200, out)
}
