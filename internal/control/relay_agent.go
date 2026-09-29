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
		if _, retired := s.Docs[relayRetirementCollection][r.PathValue("id")]; retired {
			return errRelayRetirement
		}
		agent, ok := LoadDoc[RelayAgent](s, "relay_agents", r.PathValue("id"))
		if !ok {
			return errors.New("Agent不存在")
		}
		if reason := relayEnrollmentBlockReason(s, agent); reason != "" {
			return errors.New(reason)
		}
		agent.EnrollmentHash = commerceHash(token)
		clearRelayEnrollmentRetry(&agent)
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
	WriteJSON(w, 200, map[string]any{"enrollmentToken": token, "expiresAt": expires, "installArgs": a.relayInstallationInstructions(), "message": "安装令牌已生成，请在目标 VPS 使用；已有节点日常升级请使用保留配置的升级入口。"})
}

// Fresh reset is an explicit replacement, never an in-place token rotation.
// The old identity is retired under the same terminal-proof gate as DELETE,
// then a distinct identity is created in the same transaction. The old Agent
// may still be running until the operator stops it on the node VPS; no active
// route or unproven keep_last rule is allowed through this path.
func (a *App) relayFreshResetAgent(w http.ResponseWriter, r *http.Request) {
	admin, err := a.Admin(r)
	if err != nil {
		commerceError(w, 403, err)
		return
	}
	var in struct {
		Confirm string `json:"confirm"`
	}
	if err := Decode(r, &in); err != nil || in.Confirm != "INTERRUPT_AND_REPLACE_RELAY" {
		commerceError(w, 400, errors.New("须明确确认旧节点身份失效、服务重启及现有连接可能中断"))
		return
	}
	oldID := r.PathValue("id")
	token := commerceID() + commerceID()
	newID := commerceID()
	expires := time.Now().Add(15 * time.Minute).UnixMilli()
	var replacement RelayAgent
	err = a.Store.Update(func(s *State) error {
		old, ok := LoadDoc[RelayAgent](s, "relay_agents", oldID)
		if !ok || old.ID != oldID {
			return errors.New("节点不存在或已退役")
		}
		if !old.Enabled {
			return errors.New("节点已停用；请先核对原因并启用，再申请全新重装")
		}
		if _, exists := s.Docs["relay_agents"][newID]; exists {
			return errors.New("新节点 ID 冲突，请重试")
		}
		if _, exists := s.Docs[relayRetirementCollection][newID]; exists {
			return errors.New("新节点 ID 已被退役，请重试")
		}
		if err := retireRelayAgent(s, oldID, time.Now().UnixMilli()); err != nil {
			return err
		}
		replacement = RelayAgent{
			ID: newID, Name: old.Name, Address: old.Address,
			Addresses:  append([]string(nil), old.Addresses...),
			PortRanges: append([]PortRange(nil), old.PortRanges...),
			Enabled:    true, Capability: "relay",
			EnrollmentHash: commerceHash(token), EnrollmentExpires: expires,
		}
		if control, ok := LoadDoc[RelayV2Control](s, "relay_v2_control", "default"); ok {
			replacement.EnrollmentEpoch = control.Epoch
		}
		if err := SaveDoc(s, "relay_agents", newID, replacement); err != nil {
			return err
		}
		return commerceAudit(s, admin.ID, "relay_agent.fresh_reset", oldID+":"+newID)
	})
	if err != nil {
		commerceError(w, 409, err)
		return
	}
	replacement.EnrollmentHash = ""
	WriteJSON(w, 200, map[string]any{
		"oldAgentId": oldID, "agent": replacement,
		"enrollmentToken": token, "expiresAt": expires,
		"message": "旧节点身份及管理凭据已失效。请到原 VPS 在维护窗口运行全新重装命令；脚本执行前旧本地进程可能仍在运行。",
	})
}

// Enrollment is only a bootstrap operation. A confirmed v2 identity or a
// referenced node needs the separately authenticated recovery workflow; simply
// replacing its bearer token can strand a healthy keep_last relay.
func relayEnrollmentBlockReason(s *State, agent RelayAgent) string {
	if agent.Archived {
		return "此节点已封存，请先恢复显示并核查原业务；不会自动恢复旧凭据"
	}
	if restoreAgentNeedsRecovery(s, agent) {
		if agent.ReconcileState == "recovery_required" {
			return "备份恢复后需要重新连接此节点，请使用恢复管理连接向导"
		}
		return "此节点已安装，请使用升级程序或检查连接；无需重新生成安装令牌"
	}
	malformed := false
	routes := backupPauseReadDocs[Route](s, "routes", func(string) { malformed = true }, func(k string, v Route) bool { return k != "" && k == v.ID })
	rules := backupPauseReadDocs[UserRule](s, "user_rules", func(string) { malformed = true }, func(k string, v UserRule) bool { return k != "" && k == v.UserID+":"+v.RouteID && v.ID != "" })
	if malformed {
		return "线路或用户中转记录异常，禁止轮换节点令牌；请先核对业务数据"
	}
	for _, route := range routes {
		for _, id := range relayRouteAgents(route) {
			if id == agent.ID {
				return "节点仍被隧道“" + route.Name + "”引用（包括已停用的隧道）；日常升级请保留配置，重新部署前须解除该隧道的节点关联"
			}
		}
	}
	for _, rule := range rules {
		for _, segment := range rule.Segments {
			if segment.AgentID == agent.ID {
				return "节点仍有用户转发，请先处理关联转发后再重新部署"
			}
		}
	}
	return ""
}

func (a *App) relayRegisterAgent(w http.ResponseWriter, r *http.Request) {
	var in struct {
		EnrollmentToken string `json:"enrollmentToken"`
		RequestID       string `json:"requestId,omitempty"`
	}
	if err := Decode(r, &in); err != nil {
		commerceError(w, 400, err)
		return
	}
	if len(in.EnrollmentToken) < 40 || len(in.EnrollmentToken) > 200 {
		commerceError(w, 401, errors.New("注册令牌无效"))
		return
	}
	if in.RequestID != "" && !relayV2Identifier(in.RequestID) {
		Fail(w, 400, "安装请求编号无效")
		return
	}
	token := commerceID() + commerceID()
	id := ""
	err := a.Store.Update(func(s *State) error {
		for _, agent := range ListDocs[RelayAgent](s, "relay_agents") {
			if _, retired := s.Docs[relayRetirementCollection][agent.ID]; retired {
				continue
			}
			if in.RequestID != "" && agent.Enabled && agent.LastSeen == 0 && agent.EnrollmentAttemptID == in.RequestID && agent.EnrollmentRetryHash == commerceHash(in.EnrollmentToken) && agent.EnrollmentRetryUntil > time.Now().UnixMilli() {
				if relayAgentRecoveryRequired(s, agent) {
					return errors.New("此节点需要重新关联，注册未执行")
				}
				plain, err := a.Open(agent.EnrollmentSealedToken)
				if err != nil || commerceHash(string(plain)) != agent.TokenHash {
					return errors.New("注册恢复记录需要核查")
				}
				token, id = string(plain), agent.ID
				clear(plain)
				return nil
			}
			if agent.EnrollmentHash == commerceHash(in.EnrollmentToken) && agent.EnrollmentExpires > time.Now().UnixMilli() && agent.Enabled {
				if relayAgentRecoveryRequired(s, agent) {
					return errors.New("此节点需要重新关联，注册未执行")
				}
				agent.TokenHash = commerceHash(token)
				if in.RequestID != "" {
					sealed, err := a.Seal([]byte(token))
					if err != nil {
						return err
					}
					agent.EnrollmentAttemptID, agent.EnrollmentRetryHash = in.RequestID, agent.EnrollmentHash
					agent.EnrollmentRetryUntil, agent.EnrollmentSealedToken = agent.EnrollmentExpires, sealed
				}
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

func clearRelayEnrollmentRetry(agent *RelayAgent) {
	agent.EnrollmentAttemptID, agent.EnrollmentRetryHash, agent.EnrollmentRetryUntil, agent.EnrollmentSealedToken = "", "", 0, ""
}
func (a *App) relayAgentIdentity(s *State, r *http.Request) (RelayAgent, error) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == r.Header.Get("Authorization") || len(token) < 40 || len(token) > 200 {
		return RelayAgent{}, errors.New("relay鉴权失败")
	}
	hash := commerceHash(token)
	for _, agent := range ListDocs[RelayAgent](s, "relay_agents") {
		if _, retired := s.Docs[relayRetirementCollection][agent.ID]; retired {
			continue
		}
		if agent.TokenHash == hash && agent.Capability == "relay" && !agent.Archived {
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
	cursorKey := agent.ID + ":" + rule.ID + ":" + report.Epoch
	cursor, _ := LoadDoc[TrafficCursor](s, "traffic_cursors", cursorKey)
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
		currentTrafficPeriod := report.EntitlementVersion == ruleTrafficEntitlementVersion(rule)
		if currentTrafficPeriod {
			if rule.TrafficBytes > math.MaxInt64-delta || rule.InputBytes > math.MaxInt64-inputDelta || rule.OutputBytes > math.MaxInt64-outputDelta {
				return errors.New("规则方向流量溢出")
			}
			rule.TrafficBytes += delta
			rule.InputBytes += inputDelta
			rule.OutputBytes += outputDelta
		}
		monthKey := rule.UserID + ":" + time.UnixMilli(now).UTC().Format("2006-01")
		month, _ := LoadDoc[int64](s, "traffic_months", monthKey)
		if month > math.MaxInt64-delta {
			return errors.New("月流量溢出")
		}
		if err := SaveDoc(s, "traffic_months", monthKey, month+delta); err != nil {
			return err
		}
		collection, ruleKey := "relay_rule_archive", rule.ID
		if activeRule {
			collection = "user_rules"
			ruleKey = rule.UserID + ":" + rule.RouteID
		}
		if currentTrafficPeriod {
			if err := SaveDoc(s, collection, ruleKey, rule); err != nil {
				return err
			}
		}
	}
	return SaveDoc(s, "traffic_cursors", cursorKey, TrafficCursor{Sequence: report.Sequence, InputBytes: report.InputBytes, OutputBytes: report.OutputBytes, Remainder: remainder})
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
