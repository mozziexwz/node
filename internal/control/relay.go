package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mozziexwz/node/internal/executor"
	"github.com/mozziexwz/node/internal/relayruntime"
)

const relayLeaseMS = int64(45000)

type PortRange struct {
	Start int `json:"start"`
	End   int `json:"end"`
}
type RelayAgent struct {
	ID                string      `json:"id"`
	Name              string      `json:"name"`
	Address           string      `json:"address"`
	Addresses         []string    `json:"addresses,omitempty"`
	Enabled           bool        `json:"enabled"`
	RequireFront      bool        `json:"requireFront"`
	PortRanges        []PortRange `json:"portRanges"`
	Capability        string      `json:"capability"`
	EnrollmentHash    string      `json:"enrollmentHash,omitempty"`
	EnrollmentExpires int64       `json:"enrollmentExpires,omitempty"`
	TokenHash         string      `json:"tokenHash,omitempty"`
	LastSeen          int64       `json:"lastSeen"`
	Version           string      `json:"version"`
	BootID            string      `json:"bootId,omitempty"`
	Online            bool        `json:"online"`
}
type RouteStage struct {
	AgentIDs  []string `json:"agentIds"`
	Protocol  string   `json:"protocol"`
	Strategy  string   `json:"strategy"`
	ConnectIP string   `json:"connectIp,omitempty"`
}
type Route struct {
	ID                        string       `json:"id"`
	Name                      string       `json:"name"`
	EntryAgentID              string       `json:"entryAgentId"`
	EntryAddress              string       `json:"entryAddress"`
	Hops                      []RouteStage `json:"hops"`
	Exit                      RouteStage   `json:"exit"`
	RequireFront              bool         `json:"requireFront"`
	Enabled                   bool         `json:"enabled"`
	Version                   int64        `json:"version"`
	Online                    bool         `json:"online"`
	RateMbps                  int64        `json:"rateMbps"`
	Type                      string       `json:"type"`
	TrafficMode               string       `json:"trafficMode"`
	TrafficMultiplierPermille int64        `json:"trafficMultiplierPermille"`
	AddressPreference         string       `json:"addressPreference"`
	EntryAddresses            []string     `json:"entryAddresses,omitempty"`
	EntryAuto                 bool         `json:"entryAuto"`
}
type RelaySegment struct {
	AgentID      string            `json:"agentId"`
	Runtime      relayruntime.Rule `json:"runtime"`
	AckState     string            `json:"ackState"`
	AckAt        int64             `json:"ackAt"`
	LastLease    int64             `json:"lastLease"`
	SealedTLSKey string            `json:"sealedTlsKey,omitempty"`
}
type UserRule struct {
	ID                        string         `json:"id"`
	UserID                    string         `json:"userId"`
	RouteID                   string         `json:"routeId"`
	RouteName                 string         `json:"routeName"`
	TargetHash                string         `json:"targetHash"`
	TargetHost                string         `json:"targetHost"`
	TargetPort                int            `json:"targetPort"`
	Version                   int64          `json:"version"`
	State                     string         `json:"state"`
	EntryAddress              string         `json:"entryAddress,omitempty"`
	EntryPort                 int            `json:"entryPort,omitempty"`
	SealedConfig              string         `json:"sealedConfig,omitempty"`
	Segments                  []RelaySegment `json:"segments,omitempty"`
	CreatedAt                 int64          `json:"createdAt"`
	DeleteAfter               int64          `json:"deleteAfter,omitempty"`
	HasFront                  bool           `json:"hasFront"`
	FrontTaskID               string         `json:"frontTaskId,omitempty"`
	RequestID                 string         `json:"requestId"`
	TrafficBytes              int64          `json:"trafficBytes"`
	EntitlementVersion        int64          `json:"entitlementVersion"`
	TrafficMode               string         `json:"trafficMode"`
	TrafficMultiplierPermille int64          `json:"trafficMultiplierPermille"`
	TLSExpiresAt              int64          `json:"tlsExpiresAt,omitempty"`
}
type UserTarget struct {
	Hash       string `json:"hash"`
	Host       string `json:"host"`
	Port       int    `json:"port"`
	ResetUntil int64  `json:"resetUntil"`
}
type TrafficCursor struct {
	Sequence    int64 `json:"sequence"`
	InputBytes  int64 `json:"inputBytes"`
	OutputBytes int64 `json:"outputBytes"`
	Remainder   int64 `json:"remainder,omitempty"`
}

// Rates are per forwarding rule and per direction, never a shared account pool.
func relayRate(user *User, route Route) int64 {
	if user == nil {
		return 0
	}
	return max(1, min(user.RateMbps, route.RateMbps))
}

type RelayRuleView struct {
	UserRule
	UserEmail         string `json:"userEmail,omitempty"`
	EffectiveRateMbps int64  `json:"effectiveRateMbps"`
	AppliedRateMbps   int64  `json:"appliedRateMbps"`
	SyncState         string `json:"syncState"`
	ReadySegments     int    `json:"readySegments"`
	TotalSegments     int    `json:"totalSegments"`
	StopDeadline      int64  `json:"stopDeadline,omitempty"`
}

func relaySegmentReady(s *State, seg RelaySegment, now int64) bool {
	agent, ok := LoadDoc[RelayAgent](s, "relay_agents", seg.AgentID)
	return ok && seg.AckState == "ready" && seg.LastLease > now && agent.Enabled && agent.LastSeen > now-relayLeaseMS
}

func relayRuleView(s *State, rule UserRule, admin bool, now int64) RelayRuleView {
	u := s.Users[rule.UserID]
	route, _ := LoadDoc[Route](s, "routes", rule.RouteID)
	out := RelayRuleView{UserRule: relayPublicRule(rule), EffectiveRateMbps: relayRate(u, route), SyncState: rule.State, TotalSegments: len(rule.Segments)}
	if admin {
		out.UserRule = relaySanitizedRule(rule)
		out.TargetHash = ""
		if u != nil {
			out.UserEmail = u.Email
		}
	}
	policyCurrent := rule.EntitlementVersion == entitlementVersion(s, rule.UserID)
	stopped := true
	for _, seg := range rule.Segments {
		current := seg.Runtime.RateMbps == out.EffectiveRateMbps && seg.Runtime.Version == rule.Version
		policyCurrent = policyCurrent && current
		if current && relaySegmentReady(s, seg, now) {
			out.ReadySegments++
		}
		if seg.AckState != "stopped" && seg.LastLease > now {
			stopped = false
			out.StopDeadline = max(out.StopDeadline, seg.LastLease)
		}
	}
	if !policyCurrent {
		out.ReadySegments = 0
	}
	switch rule.State {
	case "active", "pending":
		if !relayEntitled(u, now) || !route.Enabled {
			out.SyncState = "unavailable"
		} else if !policyCurrent || out.TotalSegments == 0 || out.ReadySegments != out.TotalSegments {
			out.SyncState, out.State = "syncing", "pending"
		} else {
			out.SyncState = "active"
			out.AppliedRateMbps = out.EffectiveRateMbps
		}
	case "paused":
		if !stopped {
			out.SyncState = "pausing"
		}
	case "revoking":
		out.StopDeadline = rule.DeleteAfter
	}
	return out
}

// Invalidate every hop together, before accepting ACKs for the previous policy.
// Called by both cleanup and sync, including paused/failed rules that may resume.
func relayRefreshPolicy(s *State, rule *UserRule) {
	u := s.Users[rule.UserID]
	route, ok := LoadDoc[Route](s, "routes", rule.RouteID)
	if !ok || u == nil || rule.State == "revoking" || len(rule.Segments) == 0 {
		return
	}
	rate, entitlement := relayRate(u, route), entitlementVersion(s, rule.UserID)
	changed := rule.EntitlementVersion != entitlement
	for _, seg := range rule.Segments {
		changed = changed || seg.Runtime.RateMbps != rate
	}
	if !changed {
		return
	}
	rule.Version++
	rule.EntitlementVersion = entitlement
	for i := range rule.Segments {
		seg := &rule.Segments[i]
		seg.Runtime.Version, seg.Runtime.RateMbps, seg.Runtime.EntitlementVersion = rule.Version, rate, entitlement
		seg.AckState, seg.AckAt = "pending", 0
	}
	if rule.State == "active" || rule.State == "pending" {
		rule.State = "pending"
	}
}

func relayRuleAssociation(s *State, rule UserRule) string {
	email := rule.UserID
	if u := s.Users[rule.UserID]; u != nil {
		email = u.Email
	}
	return fmt.Sprintf("%s / %s（规则 %s，%s）", email, rule.RouteName, rule.ID, rule.State)
}

func validatePortRanges(ranges []PortRange) error {
	if len(ranges) == 0 || len(ranges) > 50 {
		return errors.New("请设置1–50个可用端口区间")
	}
	copyRanges := append([]PortRange{}, ranges...)
	sort.Slice(copyRanges, func(i, j int) bool { return copyRanges[i].Start < copyRanges[j].Start })
	for i, r := range copyRanges {
		if r.Start < 1 || r.End > 65535 || r.Start > r.End {
			return errors.New("端口范围须为1–65535且起点不大于终点")
		}
		if i > 0 && r.Start <= copyRanges[i-1].End {
			return errors.New("端口范围不能重叠")
		}
	}
	return nil
}
func relayPortAllowed(ranges []PortRange, port int) bool {
	for _, r := range ranges {
		if port >= r.Start && port <= r.End {
			return true
		}
	}
	return false
}
func relayRouteAgents(route Route) []string {
	out := []string{route.EntryAgentID}
	for _, hop := range route.Hops {
		out = append(out, hop.AgentIDs...)
	}
	out = append(out, route.Exit.AgentIDs...)
	return out
}
func relayEffective(s *State, route Route) (bool, bool) {
	required, online := route.RequireFront, true
	now := time.Now().UnixMilli()
	for _, id := range relayRouteAgents(route) {
		agent, ok := LoadDoc[RelayAgent](s, "relay_agents", id)
		if !ok || !agent.Enabled || agent.LastSeen < now-relayLeaseMS {
			online = false
		}
	}
	return required, online
}
func relayValidateRoute(s *State, route Route) error {
	if route.Name == "" || len(route.Name) > 100 || route.EntryAgentID == "" || len(route.Hops) > 8 || route.RateMbps < 1 || route.RateMbps > 100000 {
		return errors.New("线路名称、入口、速率或跳数无效")
	}
	if err := relayAddress(route.EntryAddress); err != nil {
		return err
	}
	stages := append(append([]RouteStage{}, route.Hops...), route.Exit)
	if route.Type == "port_forward" {
		stages = nil
	}
	seen := map[string]bool{route.EntryAgentID: true}
	if _, ok := LoadDoc[RelayAgent](s, "relay_agents", route.EntryAgentID); !ok {
		return errors.New("入口Agent不存在")
	}
	for index, stage := range stages {
		if len(stage.AgentIDs) < 1 || len(stage.AgentIDs) > 8 {
			return errors.New("每跳须选择1–8个Agent")
		}
		if stage.Protocol != "tcp" && stage.Protocol != "tls" {
			return errors.New("跳间协议只支持 TCP 或 TLS")
		}
		if stage.Strategy != "round" && stage.Strategy != "rand" && stage.Strategy != "fifo" {
			return errors.New("负载策略须为round/rand/fifo")
		}
		for _, id := range stage.AgentIDs {
			ag, ok := LoadDoc[RelayAgent](s, "relay_agents", id)
			if !ok || ag.Capability != "relay" {
				return errors.New("仅可选择relay能力Agent")
			}
			single := route.Type != "tunnel" && len(route.Hops) == 0 && index == 0 && len(stage.AgentIDs) == 1 && id == route.EntryAgentID
			if seen[id] && !single {
				return errors.New("线路不能包含重复节点或环路")
			}
			seen[id] = true
			if stage.ConnectIP != "" && !relayNodeHasAddress(ag, stage.ConnectIP) {
				return errors.New("显式连接IP必须是所有候选节点共同拥有的已配置地址")
			}
			if _, err := relayConnectionAddress(ag, route.AddressPreference, stage.ConnectIP); err != nil {
				return err
			}
		}
	}
	return nil
}
func relayAddress(address string) error {
	if ip := net.ParseIP(address); ip != nil {
		return executor.PublicIP(address)
	}
	if strings.ContainsAny(address, "/:@ \\?#\r\n") || len(address) > 253 || !strings.Contains(address, ".") {
		return errors.New("入口须为公网IP或合法域名")
	}
	for _, label := range strings.Split(address, ".") {
		if label == "" || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return errors.New("域名格式无效")
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return errors.New("域名格式无效")
			}
		}
	}
	return nil
}
func relayPublicRule(rule UserRule) UserRule {
	rule = relaySanitizedRule(rule)
	rule.EntryAddress, rule.EntryPort, rule.TargetHash = "", 0, ""
	rule.Segments = nil
	return rule
}
func relaySanitizedRule(rule UserRule) UserRule {
	rule.SealedConfig = ""
	rule.Segments = append([]RelaySegment(nil), rule.Segments...)
	for i := range rule.Segments {
		rule.Segments[i].Runtime.Targets = nil
		rule.Segments[i].Runtime.AllowedSources = nil
		rule.Segments[i].Runtime.TLSCertificate = ""
		rule.Segments[i].Runtime.TLSPrivateKey = ""
		rule.Segments[i].Runtime.TargetTLS = nil
		rule.Segments[i].SealedTLSKey = ""
	}
	return rule
}
func relayReservePort(s *State, agent RelayAgent) (int, error) {
	used := map[int]bool{}
	now := time.Now().UnixMilli()
	for _, rule := range ListDocs[UserRule](s, "user_rules") {
		for _, seg := range rule.Segments {
			if seg.AgentID == agent.ID && (rule.DeleteAfter == 0 || rule.DeleteAfter > now) {
				used[seg.Runtime.ListenPort] = true
			}
		}
	}
	for _, pr := range agent.PortRanges {
		for p := pr.Start; p <= pr.End; p++ {
			if !used[p] {
				return p, nil
			}
		}
	}
	return 0, errors.New("Agent端口池已耗尽")
}
func relayEntitled(u *User, now int64) bool {
	return u != nil && u.Status == "active" && u.ExpiresAt > now && u.TrafficUsed < u.TrafficTotal
}
func relayRevoke(rule *UserRule, now int64, deleteConfig bool) {
	rule.State = "revoking"
	if deleteConfig {
		rule.SealedConfig = ""
	}
	rule.DeleteAfter = now + relayLeaseMS
	for _, seg := range rule.Segments {
		if seg.LastLease > rule.DeleteAfter {
			rule.DeleteAfter = seg.LastLease
		}
	}
}

func (a *App) RegisterRelay(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/routes", a.relayRoutes)
	mux.HandleFunc("GET /api/user/rules", a.relayUserRules)
	mux.HandleFunc("POST /api/user/routes/{route}/rules", a.relayCreateRule)
	mux.HandleFunc("PATCH /api/user/routes/{route}/rules", a.relayEditRule)
	mux.HandleFunc("DELETE /api/user/routes/{route}/rules", a.relayDeleteRule)
	mux.HandleFunc("GET /api/user/routes/{route}/config", a.relayDownload)
	mux.HandleFunc("POST /api/user/target/reset", a.relayResetTarget)
	mux.HandleFunc("GET /api/user/traffic", a.relayTraffic)
	mux.HandleFunc("GET /api/admin/relay-agents", a.relayAgents)
	mux.HandleFunc("POST /api/admin/relay-agents", a.relaySaveAgent)
	mux.HandleFunc("PUT /api/admin/relay-agents/{id}", a.relaySaveAgent)
	mux.HandleFunc("DELETE /api/admin/relay-agents/{id}", a.relayDeleteAgent)
	mux.HandleFunc("POST /api/admin/relay-agents/{id}/enrollment", a.relayRenewEnrollment)
	mux.HandleFunc("GET /api/admin/routes", a.relayRoutes)
	mux.HandleFunc("POST /api/admin/routes", a.relaySaveRoute)
	mux.HandleFunc("PUT /api/admin/routes/{id}", a.relaySaveRoute)
	mux.HandleFunc("DELETE /api/admin/routes/{id}", a.relayDeleteRoute)
	mux.HandleFunc("GET /api/admin/user-rules", a.relayUserRules)
	mux.HandleFunc("GET /api/admin/user-rules/{id}", a.relayAdminRule)
	mux.HandleFunc("PATCH /api/admin/user-rules/{id}", a.relayEditRule)
	mux.HandleFunc("DELETE /api/admin/user-rules/{id}", a.relayDeleteRule)
	mux.HandleFunc("POST /api/relay-agent/register", a.relayRegisterAgent)
	mux.HandleFunc("POST /api/relay-agent/sync", a.relaySyncAgent)
}
func (a *App) relayRoutes(w http.ResponseWriter, r *http.Request) {
	admin := strings.Contains(r.URL.Path, "/admin/")
	var user *User
	if admin {
		if _, err := a.Admin(r); err != nil {
			commerceError(w, 403, err)
			return
		}
	} else {
		var err error
		user, err = a.User(r)
		if err != nil {
			commerceError(w, 401, err)
			return
		}
	}
	out := []Route{}
	var userRate int64
	err := a.Store.View(func(s *State) error {
		if user != nil && s.Users[user.ID] != nil {
			user = s.Users[user.ID]
			userRate = user.RateMbps
		}
		for _, route := range ListDocs[Route](s, "routes") {
			if err := normalizeRelayRoute(s, &route); err != nil && !admin {
				continue
			}
			if admin || route.Enabled {
				route.RequireFront, route.Online = relayEffective(s, route)
				if !admin {
					route.RateMbps = relayRate(user, route)
					route.Hops = nil
					route.Exit = RouteStage{}
					route.EntryAgentID = ""
					route.EntryAddress = ""
					route.EntryAddresses = nil
				}
				out = append(out, route)
			}
		}
		return nil
	})
	if err != nil {
		commerceError(w, 500, err)
		return
	}
	WriteJSON(w, 200, map[string]any{"routes": out, "userRateMbps": userRate, "rateScope": "per_rule_per_direction", "protocols": []string{"tcp", "tls"}, "strategies": []string{"round", "rand", "fifo"}, "leaseSeconds": relayLeaseMS / 1000})
}
func (a *App) relayAgents(w http.ResponseWriter, r *http.Request) {
	if _, err := a.Admin(r); err != nil {
		commerceError(w, 403, err)
		return
	}
	out := []RelayAgent{}
	err := a.Store.View(func(s *State) error {
		for _, agent := range ListDocs[RelayAgent](s, "relay_agents") {
			agent.RequireFront = false
			agent.TokenHash = ""
			agent.EnrollmentHash = ""
			agent.Online = agent.Enabled && agent.LastSeen > time.Now().UnixMilli()-relayLeaseMS
			out = append(out, agent)
		}
		return nil
	})
	if err != nil {
		commerceError(w, 500, err)
		return
	}
	WriteJSON(w, 200, map[string]any{"agents": out})
}
func (a *App) relaySaveAgent(w http.ResponseWriter, r *http.Request) {
	if _, err := a.Admin(r); err != nil {
		commerceError(w, 403, err)
		return
	}
	var in RelayAgent
	if err := Decode(r, &in); err != nil {
		commerceError(w, 400, err)
		return
	}
	if in.Name == "" || len(in.Name) > 100 {
		commerceError(w, 400, errors.New("请输入Agent名称"))
		return
	}
	if err := executor.PublicIP(in.Address); err != nil {
		commerceError(w, 400, err)
		return
	}
	if err := normalizeRelayAddresses(&in); err != nil {
		commerceError(w, 400, err)
		return
	}
	in.RequireFront = false
	if err := validatePortRanges(in.PortRanges); err != nil {
		commerceError(w, 400, err)
		return
	}
	in.ID = r.PathValue("id")
	enrollment := ""
	err := a.Store.Update(func(s *State) error {
		if in.ID != "" {
			old, ok := LoadDoc[RelayAgent](s, "relay_agents", in.ID)
			if !ok {
				return errors.New("Agent不存在")
			}
			for _, rule := range ListDocs[UserRule](s, "user_rules") {
				for _, seg := range rule.Segments {
					if seg.AgentID == in.ID {
						if !relayPortAllowed(in.PortRanges, seg.Runtime.ListenPort) {
							return errors.New("范围修改排除了正在使用的端口，请先删除关联规则")
						}
						if strings.Join(relayNodeAddresses(in), ",") != strings.Join(relayNodeAddresses(old), ",") {
							return errors.New("Agent被用户规则引用，变更IP前请先删除规则")
						}
					}
				}
			}
			in.TokenHash = old.TokenHash
			in.EnrollmentHash = old.EnrollmentHash
			in.EnrollmentExpires = old.EnrollmentExpires
			in.LastSeen = old.LastSeen
			in.Version = old.Version
			in.BootID = old.BootID
		} else {
			in.ID = commerceID()
			enrollment = commerceID() + commerceID()
			in.EnrollmentHash = commerceHash(enrollment)
			in.EnrollmentExpires = time.Now().Add(15 * time.Minute).UnixMilli()
			in.LastSeen = 0
			in.TokenHash = ""
		}
		in.Capability = "relay"
		in.Online = false
		return SaveDoc(s, "relay_agents", in.ID, in)
	})
	if err != nil {
		commerceError(w, 409, err)
		return
	}
	in.TokenHash = ""
	in.EnrollmentHash = ""
	WriteJSON(w, 200, map[string]any{"agent": in, "enrollmentToken": enrollment, "installArgs": a.relayInstallationInstructions()})
}

func (a *App) relayInstallationInstructions() map[string]any {
	return map[string]any{
		"installer": "deploy/install-agent.sh",
		"args": []string{"--capability", "relay", "--server", a.Config.PublicURL,
			"--agent", "/absolute/path/to/reviewed-msboost-agent", "--agent-sha256", "REPLACE_WITH_REVIEWED_SHA256",
			"--token-file", "/root/msboost-relay-token", "--gost-version", "3.3.0"},
		"tokenEnvironment": "MSBOOST_RELAY_ENROLLMENT_TOKEN",
		"instructions":     "先将一次性注册令牌保存到 root 所有、权限 0600 的 token-file；替换实际 Agent 路径与审核后的 SHA256，再以 sudo bash 运行 installer。不要把令牌放入命令行参数。",
	}
}

func (a *App) relayDeleteAgent(w http.ResponseWriter, r *http.Request) {
	if _, err := a.Admin(r); err != nil {
		commerceError(w, 403, err)
		return
	}
	err := a.Store.Update(func(s *State) error {
		associations := []string{}
		for _, route := range ListDocs[Route](s, "routes") {
			for _, id := range relayRouteAgents(route) {
				if id == r.PathValue("id") {
					associations = append(associations, fmt.Sprintf("线路 %s（%s）", route.Name, route.ID))
					break
				}
			}
		}
		for _, rule := range ListDocs[UserRule](s, "user_rules") {
			for _, seg := range rule.Segments {
				if seg.AgentID == r.PathValue("id") {
					associations = append(associations, relayRuleAssociation(s, rule))
					break
				}
			}
		}
		if len(associations) > 0 {
			return errors.New("节点仍有关联，请前往隧道管理 / 用户中转处理：" + strings.Join(associations, "；"))
		}
		DeleteDoc(s, "relay_agents", r.PathValue("id"))
		return nil
	})
	if err != nil {
		commerceError(w, 409, err)
		return
	}
	WriteJSON(w, 200, map[string]bool{"ok": true})
}
func (a *App) relaySaveRoute(w http.ResponseWriter, r *http.Request) {
	if _, err := a.Admin(r); err != nil {
		commerceError(w, 403, err)
		return
	}
	var route Route
	if err := Decode(r, &route); err != nil {
		commerceError(w, 400, err)
		return
	}
	route.ID = r.PathValue("id")
	err := a.Store.Update(func(s *State) error {
		if err := normalizeRelayRoute(s, &route); err != nil {
			return err
		}
		if err := relayValidateRoute(s, route); err != nil {
			return err
		}
		if route.ID != "" {
			old, ok := LoadDoc[Route](s, "routes", route.ID)
			if !ok {
				return errors.New("线路不存在")
			}
			for _, rule := range ListDocs[UserRule](s, "user_rules") {
				if rule.RouteID == route.ID {
					return errors.New("线路仍有用户规则，请先在用户中转处理：" + relayRuleAssociation(s, rule))
				}
			}
			route.Version = old.Version + 1
		} else {
			route.ID = commerceID()
			route.Version = 1
		}
		route.Online = false
		return SaveDoc(s, "routes", route.ID, route)
	})
	if err != nil {
		commerceError(w, 409, err)
		return
	}
	WriteJSON(w, 200, route)
}
func (a *App) relayDeleteRoute(w http.ResponseWriter, r *http.Request) {
	if _, err := a.Admin(r); err != nil {
		commerceError(w, 403, err)
		return
	}
	err := a.Store.Update(func(s *State) error {
		associations := []string{}
		for _, rule := range ListDocs[UserRule](s, "user_rules") {
			if rule.RouteID == r.PathValue("id") {
				associations = append(associations, relayRuleAssociation(s, rule))
			}
		}
		if len(associations) > 0 {
			return errors.New("线路仍被用户规则引用，请前往用户中转处理（撤销租约到期后才能删除）：" + strings.Join(associations, "；"))
		}
		DeleteDoc(s, "routes", r.PathValue("id"))
		return nil
	})
	if err != nil {
		commerceError(w, 409, err)
		return
	}
	WriteJSON(w, 200, map[string]bool{"ok": true})
}
func (a *App) relayUserRules(w http.ResponseWriter, r *http.Request) {
	admin := strings.Contains(r.URL.Path, "/admin/")
	u, err := a.User(r)
	if admin {
		u, err = a.Admin(r)
	}
	if err != nil {
		commerceError(w, 401, err)
		return
	}
	out := []RelayRuleView{}
	var userRate int64
	err = a.Store.View(func(s *State) error {
		if s.Users[u.ID] != nil {
			userRate = s.Users[u.ID].RateMbps
		}
		for _, rule := range ListDocs[UserRule](s, "user_rules") {
			if rule.UserID == u.ID || admin {
				view := relayRuleView(s, rule, admin, time.Now().UnixMilli())
				if admin {
					q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
					if q != "" && !strings.Contains(strings.ToLower(view.UserEmail+" "+rule.UserID+" "+rule.ID+" "+rule.RouteName+" "+rule.TargetHost), q) {
						continue
					}
					if state := r.URL.Query().Get("state"); state != "" && view.State != state {
						continue
					}
					if routeID := r.URL.Query().Get("routeId"); routeID != "" && rule.RouteID != routeID {
						continue
					}
				}
				out = append(out, view)
			}
		}
		return nil
	})
	if err != nil {
		commerceError(w, 500, err)
		return
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	WriteJSON(w, 200, map[string]any{"rules": out, "userRateMbps": userRate, "rateScope": "per_rule_per_direction"})
}

func (a *App) relayAdminRule(w http.ResponseWriter, r *http.Request) {
	if _, err := a.Admin(r); err != nil {
		commerceError(w, 403, err)
		return
	}
	var out RelayRuleView
	found := false
	err := a.Store.View(func(s *State) error {
		for _, rule := range ListDocs[UserRule](s, "user_rules") {
			if rule.ID == r.PathValue("id") {
				out, found = relayRuleView(s, rule, true, time.Now().UnixMilli()), true
				break
			}
		}
		return nil
	})
	if err != nil {
		commerceError(w, 500, err)
	} else if !found {
		commerceError(w, 404, errors.New("规则不存在或已撤销归档"))
	} else {
		WriteJSON(w, 200, out)
	}
}

func relayRequestRule(s *State, r *http.Request, actor *User, admin bool) (string, UserRule, bool) {
	if !admin {
		key := actor.ID + ":" + r.PathValue("route")
		rule, ok := LoadDoc[UserRule](s, "user_rules", key)
		return key, rule, ok
	}
	for _, rule := range ListDocs[UserRule](s, "user_rules") {
		if rule.ID == r.PathValue("id") {
			return rule.UserID + ":" + rule.RouteID, rule, true
		}
	}
	return "", UserRule{}, false
}
func (a *App) relayCreateRule(w http.ResponseWriter, r *http.Request) {
	u, err := a.User(r)
	if err != nil {
		commerceError(w, 401, err)
		return
	}
	var in struct {
		Config    json.RawMessage `json:"config"`
		RequestID string          `json:"requestId"`
		Front     *executor.SSH   `json:"front,omitempty"`
	}
	if err := Decode(r, &in); err != nil {
		commerceError(w, 400, err)
		return
	}
	if err := commerceReqID(in.RequestID); err != nil {
		commerceError(w, 400, err)
		return
	}
	if in.Front != nil {
		if err := executor.ValidateSSH(*in.Front); err != nil {
			commerceError(w, 400, err)
			return
		}
		if _, ok := relayFrontProvisioners.Load(a); !ok {
			commerceError(w, 503, errors.New("前置执行服务未配置"))
			return
		}
	}
	info, err := executor.ParseClientConfig(in.Config)
	if err != nil {
		commerceError(w, 400, err)
		return
	}
	targetAddresses, err := resolveRelayTarget(r.Context(), info.TargetHost, info.TargetPort)
	if err != nil {
		commerceError(w, 400, err)
		return
	}
	normalizedHost := strings.ToLower(strings.TrimSuffix(info.TargetHost, "."))
	if ip := net.ParseIP(normalizedHost); ip != nil {
		normalizedHost = ip.String()
	}
	identityBytes, _ := json.Marshal([]any{normalizedHost, info.TargetPort, "tcp", info.Username, info.Password})
	targetHash := commerceHash(string(identityBytes))
	now := time.Now().UnixMilli()
	var out UserRule
	created := false
	err = a.Store.Update(func(s *State) error {
		user := s.Users[u.ID]
		if boolSetting(s, "maintenance") {
			return errors.New("站点维护期间暂停新线路配置")
		}
		if !boolSetting(s, "paidCreate") {
			return errors.New("增值线路新建暂时关闭")
		}
		if !relayEntitled(user, now) {
			return errors.New("套餐无效、流量耗尽或账号已暂停")
		}
		route, ok := LoadDoc[Route](s, "routes", r.PathValue("route"))
		if !ok || !route.Enabled {
			return errors.New("线路不可用")
		}
		if err := normalizeRelayRoute(s, &route); err != nil {
			return err
		}
		if err := relayValidateRoute(s, route); err != nil {
			return err
		}
		required, online := relayEffective(s, route)
		if required && in.Front == nil {
			return errors.New("此线路要求自备前置机，请填写SSH信息并完成主机信任检查")
		}
		if !online {
			return errors.New("线路有离线Agent，请稍后再试")
		}
		for _, agent := range ListDocs[RelayAgent](s, "relay_agents") {
			for _, targetAddress := range targetAddresses {
				host, _, _ := net.SplitHostPort(targetAddress)
				if relayNodeHasAddress(agent, host) {
					return errors.New("目标节点不得指向本站托管中转Agent，避免回环与控制网访问")
				}
			}
		}
		key := u.ID + ":" + route.ID
		if old, ok := LoadDoc[UserRule](s, "user_rules", key); ok {
			if old.RequestID == in.RequestID && old.TargetHash == targetHash {
				out = old
				return nil
			}
			return errors.New("此线路已配置或仍在撤销，请先删除原规则")
		}
		target, found := LoadDoc[UserTarget](s, "user_targets", u.ID)
		if found && target.ResetUntil > now {
			return errors.New("旧目标规则仍在撤销租约内，请稍后重试")
		}
		if found && target.ResetUntil == 0 && target.Hash != targetHash {
			return errors.New("所有线路必须使用同一个MSBOOST目标及认证，请先更换统一目标")
		}
		if in.Front != nil {
			for _, agentID := range relayRouteAgents(route) {
				agent, _ := LoadDoc[RelayAgent](s, "relay_agents", agentID)
				if relayNodeHasAddress(agent, in.Front.Host) {
					return errors.New("客户前置机不能使用本站链路节点地址")
				}
			}
		}
		out = UserRule{ID: commerceID(), UserID: u.ID, RouteID: route.ID, RouteName: route.Name, TargetHash: targetHash, TargetHost: info.TargetHost, TargetPort: info.TargetPort, Version: 1, State: "pending", EntryAddress: route.EntryAddress, CreatedAt: now, RequestID: in.RequestID}
		out.EntitlementVersion = entitlementVersion(s, u.ID)
		out.TrafficMode, out.TrafficMultiplierPermille = route.TrafficMode, route.TrafficMultiplierPermille
		if in.Front != nil {
			out.State = "awaiting_front"
		}
		created = true
		stages := []RouteStage{{AgentIDs: []string{route.EntryAgentID}, Protocol: "tcp", Strategy: "round"}}
		if route.Type != "port_forward" && (len(route.Hops) > 0 || len(route.Exit.AgentIDs) != 1 || route.Exit.AgentIDs[0] != route.EntryAgentID) {
			stages = append(stages, route.Hops...)
			stages = append(stages, route.Exit)
		}
		portMap := map[string]int{}
		for _, stage := range stages {
			for _, id := range stage.AgentIDs {
				agent, _ := LoadDoc[RelayAgent](s, "relay_agents", id)
				port, err := relayReservePort(s, agent)
				if err != nil {
					return err
				}
				portMap[id] = port
			}
		}
		rate := relayRate(user, route)
		for i, stage := range stages {
			for _, id := range stage.AgentIDs {
				targets := []string{}
				strategy := "round"
				if i == len(stages)-1 {
					targets = append(targets, targetAddresses...)
				} else {
					strategy = stages[i+1].Strategy
					for _, nextID := range stages[i+1].AgentIDs {
						next, _ := LoadDoc[RelayAgent](s, "relay_agents", nextID)
						address, err := relayConnectionAddress(next, route.AddressPreference, stages[i+1].ConnectIP)
						if err != nil {
							return err
						}
						targets = append(targets, net.JoinHostPort(address, strconv.Itoa(portMap[nextID])))
					}
				}
				sources := []string{}
				if i > 0 {
					for _, previousID := range stages[i-1].AgentIDs {
						previous, _ := LoadDoc[RelayAgent](s, "relay_agents", previousID)
						sources = append(sources, relayNodeAddresses(previous)...)
					}
				} else if in.Front != nil {
					sources = append(sources, in.Front.Host)
				}
				out.Segments = append(out.Segments, RelaySegment{AgentID: id, Runtime: relayruntime.Rule{ID: out.ID, Version: out.Version, ListenPort: portMap[id], Targets: targets, AllowedSources: sources, Strategy: strategy, Protocol: stage.Protocol, RateMbps: rate, Billing: i == 0, EntitlementVersion: out.EntitlementVersion}, AckState: "pending"})
			}
		}
		if err := a.configureRelayTLS(&out, stages, user.ExpiresAt); err != nil {
			return err
		}
		out.EntryPort = portMap[route.EntryAgentID]
		raw, err := executor.RewriteClientConfig(in.Config, out.EntryAddress, out.EntryPort)
		if err != nil {
			return err
		}
		out.SealedConfig, err = a.Seal(raw)
		if err != nil {
			return err
		}
		if in.Front != nil {
			out.SealedConfig = ""
		}
		if err := SaveDoc(s, "user_targets", u.ID, UserTarget{Hash: targetHash, Host: info.TargetHost, Port: info.TargetPort}); err != nil {
			return err
		}
		return SaveDoc(s, "user_rules", key, out)
	})
	if err != nil {
		commerceError(w, 409, err)
		return
	}
	if created && in.Front != nil {
		key := u.ID + ":" + out.RouteID
		err = a.relayFrontReady(r.Context(), key)
		var hop executor.Hop
		if err == nil {
			value, _ := relayFrontProvisioners.Load(a)
			hop, err = value.(frontProvisioner)(r.Context(), u.ID, *in.Front, out.EntryAddress, out.EntryPort)
		}
		if err == nil && (hop.FromHost != in.Front.Host || hop.FromPort < 1 || hop.FromPort > 65535 || hop.ToHost != out.EntryAddress || hop.ToPort != out.EntryPort) {
			err = errors.New("前置执行返回的链路与已分配入口不匹配")
		}
		if err == nil {
			var raw []byte
			raw, err = executor.RewriteClientConfig(in.Config, hop.FromHost, hop.FromPort)
			if err == nil {
				var sealed string
				sealed, err = a.Seal(raw)
				if err == nil {
					err = a.Store.Update(func(s *State) error {
						current, ok := LoadDoc[UserRule](s, "user_rules", key)
						if !ok || current.ID != out.ID || current.State != "awaiting_front" || !relayEntitled(s.Users[u.ID], time.Now().UnixMilli()) {
							return errors.New("配置期间本站授权已改变，前置规则不再生效")
						}
						current.SealedConfig = sealed
						current.HasFront = true
						current.EntryAddress = hop.FromHost
						current.EntryPort = hop.FromPort
						current.State = "pending"
						out = current
						return SaveDoc(s, "user_rules", key, current)
					})
				}
			}
		}
		if err != nil {
			_ = a.Store.Update(func(s *State) error {
				current, ok := LoadDoc[UserRule](s, "user_rules", key)
				if ok && current.ID == out.ID {
					relayRevoke(&current, time.Now().UnixMilli(), true)
					return SaveDoc(s, "user_rules", key, current)
				}
				return nil
			})
			commerceError(w, 502, err)
			return
		}
	}
	WriteJSON(w, 202, relayPublicRule(out))
}
func (a *App) relayEditRule(w http.ResponseWriter, r *http.Request) {
	admin := strings.Contains(r.URL.Path, "/admin/")
	u, err := a.User(r)
	if admin {
		u, err = a.Admin(r)
	}
	if err != nil {
		commerceError(w, 403, err)
		return
	}
	var in struct {
		Paused *bool `json:"paused"`
	}
	if err := Decode(r, &in); err != nil {
		commerceError(w, 400, err)
		return
	}
	if in.Paused == nil {
		commerceError(w, 400, errors.New("请明确指定暂停或恢复"))
		return
	}
	var out RelayRuleView
	err = a.Store.Update(func(s *State) error {
		key, rule, ok := relayRequestRule(s, r, u, admin)
		if !ok || rule.State == "revoking" || rule.State == "awaiting_front" {
			return errors.New("规则不存在或已在撤销")
		}
		if *in.Paused {
			rule.State = "paused"
		} else {
			if !relayEntitled(s.Users[rule.UserID], time.Now().UnixMilli()) {
				return errors.New("套餐不可用")
			}
			route, exists := LoadDoc[Route](s, "routes", rule.RouteID)
			if !exists || !route.Enabled {
				return errors.New("线路已停用或不存在，不能恢复")
			}
			if len(rule.Segments) == 0 {
				return errors.New("这是备份恢复保留的下载配置，请删除后重新配置线路以绑定新的Agent")
			}
			// Repeated resume of an already running/syncing rule is idempotent.
			if rule.State != "active" && rule.State != "pending" {
				rule.State = "pending"
				rule.Version++
				for i := range rule.Segments {
					rule.Segments[i].Runtime.Version = rule.Version
					rule.Segments[i].AckState, rule.Segments[i].AckAt = "pending", 0
				}
			}
		}
		relayRefreshPolicy(s, &rule)
		out = relayRuleView(s, rule, admin, time.Now().UnixMilli())
		if err := SaveDoc(s, "user_rules", key, rule); err != nil {
			return err
		}
		action := "relay_rule.resume"
		if *in.Paused {
			action = "relay_rule.pause"
		}
		return commerceAudit(s, u.ID, action, rule.ID)
	})
	if err != nil {
		commerceError(w, 409, err)
		return
	}
	WriteJSON(w, 200, out)
}
func (a *App) relayDeleteRule(w http.ResponseWriter, r *http.Request) {
	admin := strings.Contains(r.URL.Path, "/admin/")
	u, err := a.User(r)
	if admin {
		u, err = a.Admin(r)
	}
	if err != nil {
		commerceError(w, 403, err)
		return
	}
	err = a.Store.Update(func(s *State) error {
		key, rule, ok := relayRequestRule(s, r, u, admin)
		if !ok {
			return nil
		}
		if rule.State != "revoking" {
			relayRevoke(&rule, time.Now().UnixMilli(), true)
		}
		if err := SaveDoc(s, "user_rules", key, rule); err != nil {
			return err
		}
		return commerceAudit(s, u.ID, "relay_rule.delete", rule.ID)
	})
	if err != nil {
		commerceError(w, 409, err)
		return
	}
	WriteJSON(w, 202, map[string]any{"state": "revoking", "maximumLeaseSeconds": relayLeaseMS / 1000})
}
func (a *App) relayDownload(w http.ResponseWriter, r *http.Request) {
	u, err := a.User(r)
	if err != nil {
		commerceError(w, 401, err)
		return
	}
	var raw []byte
	err = a.Store.View(func(s *State) error {
		user := s.Users[u.ID]
		if user == nil || user.ExpiresAt <= time.Now().UnixMilli() {
			return errors.New("套餐已到期，服务器配置已失效")
		}
		rule, ok := LoadDoc[UserRule](s, "user_rules", u.ID+":"+r.PathValue("route"))
		if !ok || rule.SealedConfig == "" || rule.State == "revoking" {
			return errors.New("配置不存在或已撤销")
		}
		var err error
		raw, err = a.Open(rule.SealedConfig)
		return err
	})
	if err != nil {
		commerceError(w, 403, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=msboost-relay.json; filename*=UTF-8''%E5%A2%9E%E5%80%BC%E4%B8%AD%E8%BD%AC.json")
	w.Header().Set("Cache-Control", "no-store, private")
	w.Write(raw)
}
func (a *App) relayResetTarget(w http.ResponseWriter, r *http.Request) {
	u, err := a.User(r)
	if err != nil {
		commerceError(w, 401, err)
		return
	}
	var in struct {
		Confirm bool `json:"confirm"`
	}
	if err := Decode(r, &in); err != nil || !in.Confirm {
		commerceError(w, 400, errors.New("请明确确认撤销所有线路的旧目标"))
		return
	}
	until := time.Now().UnixMilli() + relayLeaseMS
	err = a.Store.Update(func(s *State) error {
		for _, rule := range ListDocs[UserRule](s, "user_rules") {
			if rule.UserID == u.ID {
				relayRevoke(&rule, time.Now().UnixMilli(), true)
				if rule.DeleteAfter > until {
					until = rule.DeleteAfter
				}
				if err := SaveDoc(s, "user_rules", u.ID+":"+rule.RouteID, rule); err != nil {
					return err
				}
			}
		}
		target, _ := LoadDoc[UserTarget](s, "user_targets", u.ID)
		target.ResetUntil = until
		return SaveDoc(s, "user_targets", u.ID, target)
	})
	if err != nil {
		commerceError(w, 500, err)
		return
	}
	WriteJSON(w, 202, map[string]any{"state": "revoking", "retryAfter": until})
}
func (a *App) relayTraffic(w http.ResponseWriter, r *http.Request) {
	u, err := a.User(r)
	if err != nil {
		commerceError(w, 401, err)
		return
	}
	months := map[string]int64{}
	err = a.Store.View(func(s *State) error {
		if !boolSetting(s, "monitor") && u.Role != "admin" {
			return errors.New("用户流量监控暂时关闭")
		}
		for key, raw := range s.Docs["traffic_months"] {
			if strings.HasPrefix(key, u.ID+":") {
				var n int64
				if err := json.Unmarshal(raw, &n); err != nil {
					return err
				}
				months[strings.TrimPrefix(key, u.ID+":")] = n
			}
		}
		return nil
	})
	if err != nil {
		commerceError(w, 500, err)
		return
	}
	WriteJSON(w, 200, map[string]any{"trafficUsed": u.TrafficUsed, "trafficTotal": u.TrafficTotal, "months": months, "accounting": "入口节点按每条隧道的上传/下载方向和倍率计费；十进制GB；多跳只计一次"})
}
func (a *App) StartRelay(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				_ = a.Store.Update(func(s *State) error { return relayCleanup(s, now.UnixMilli()) })
			}
		}
	}()
}
func relayCleanup(s *State, now int64) error {
	for _, rule := range ListDocs[UserRule](s, "user_rules") {
		key := rule.UserID + ":" + rule.RouteID
		u := s.Users[rule.UserID]
		relayRefreshPolicy(s, &rule)
		if u == nil || u.ExpiresAt <= now {
			if rule.State != "revoking" {
				relayRevoke(&rule, now, true)
			}
		}
		if rule.State == "awaiting_front" && rule.CreatedAt+5*60*1000 <= now {
			relayRevoke(&rule, now, true)
		}
		if rule.State != "revoking" && rule.State != "paused" && rule.State != "failed" && rule.State != "awaiting_front" {
			if u != nil && u.Status != "active" {
				rule.State = "suspended"
			} else if u != nil && u.TrafficUsed >= u.TrafficTotal {
				rule.State = "quota_exhausted"
			} else if rule.State == "suspended" || rule.State == "quota_exhausted" {
				rule.State = "pending"
			}
		}
		if rule.State == "revoking" && rule.DeleteAfter <= now {
			archived := relaySanitizedRule(rule)
			archived.SealedConfig = ""
			archived.State = "revoked"
			if err := SaveDoc(s, "relay_rule_archive", rule.ID, archived); err != nil {
				return err
			}
			DeleteDoc(s, "user_rules", key)
			continue
		}
		if err := SaveDoc(s, "user_rules", key, rule); err != nil {
			return err
		}
	}
	for id := range s.Docs["user_targets"] {
		target, _ := LoadDoc[UserTarget](s, "user_targets", id)
		if target.ResetUntil > 0 && target.ResetUntil <= now {
			DeleteDoc(s, "user_targets", id)
		}
	}
	return nil
}
