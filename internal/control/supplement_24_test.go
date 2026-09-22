package control

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mozziexwz/node/internal/relayruntime"
)

func TestSupplement24PasswordPolicy(t *testing.T) {
	valid := []struct {
		password string
		email    string
	}{
		{"correct horse battery", "person@example.com"},
		{strings.Repeat("密", 8), "12345678@qq.com"},
		{"Ab1!Ab2@Ab", "someone@example.com"},
	}
	for _, test := range valid {
		if err := validPassword(test.password, test.email); err != nil {
			t.Fatalf("valid password rejected: %v", err)
		}
	}
	invalid := []struct {
		password string
		email    string
	}{
		{"short7", "person@example.com"},
		{strings.Repeat("界", 25), "person@example.com"},
		{"PERSON@EXAMPLE.COM", "person@example.com"},
		{"LoNgPeRsOn", "longperson@example.com"},
		{"abc123456xyz", "person@example.com"},
	}
	for _, test := range invalid {
		if err := validPassword(test.password, test.email); err == nil {
			t.Fatalf("invalid password accepted: %q", test.password)
		}
	}
}

func TestSupplement24CommerceWholeLeavesAndLevel(t *testing.T) {
	a, mux, user := commerceTestApp(t)
	admin := &User{ID: "commerce-admin", Role: "admin", Status: "active"}
	response := commerceTestRequest(mux, admin, http.MethodPost, "/api/admin/plans", Plan{Name: "小数权益", PriceCents: 150, Days: 1, TrafficBytes: commerceGB, RateMbps: 1, Enabled: true, Level: 1})
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "整数") {
		t.Fatalf("fractional plan leaves accepted: %d %s", response.Code, response.Body.String())
	}
	response = commerceTestRequest(mux, admin, http.MethodPost, "/api/admin/cards", map[string]any{"count": 1, "amountCents": 150, "batch": "fraction"})
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "整数") {
		t.Fatalf("fractional card leaves accepted: %d %s", response.Code, response.Body.String())
	}
	legacy := Plan{ID: "legacy-fractional", Name: "旧小数权益", PriceCents: 150, Days: 1, TrafficBytes: commerceGB, RateMbps: 1, Enabled: true, Level: 1}
	if err := a.Store.Update(func(s *State) error { return SaveDoc(s, "plans", legacy.ID, legacy) }); err != nil {
		t.Fatal(err)
	}
	response = commerceTestRequest(mux, user, http.MethodGet, "/api/plans", nil)
	var listing struct {
		Plans []commercePlanView `json:"plans"`
	}
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &listing) != nil || len(listing.Plans) != 1 || listing.Plans[0].Eligible || !strings.Contains(listing.Plans[0].Reason, "配置无效") {
		t.Fatalf("legacy fractional plan appeared purchasable: %d %s", response.Code, response.Body.String())
	}
}

func TestSupplement24PlanPoliciesAndLegacyHolderInference(t *testing.T) {
	a, mux, user := commerceTestApp(t)
	admin := &User{ID: "policy-admin", Role: "admin", Status: "active"}
	now := time.Now().UnixMilli()
	holder := Plan{ID: "holder-plan", Name: "繁忙期续兑", PriceCents: 0, Days: 3, TrafficBytes: commerceGB, RateMbps: 5, Enabled: true, Level: 2, CurrentHoldersOnly: true, CreatedAt: 1}
	trial := Plan{ID: "trial-plan", Name: "体验权益", PriceCents: 0, Days: 1, TrafficBytes: commerceGB, RateMbps: 5, Enabled: true, Level: 1, Trial: true, CreatedAt: 2}
	trial2 := Plan{ID: "trial-plan-2", Name: "另一体验权益", PriceCents: 0, Days: 1, TrafficBytes: commerceGB, RateMbps: 5, Enabled: true, Level: 1, Trial: true, CreatedAt: 3}
	legacyOrder := Order{ID: "legacy-paid", UserID: user.ID, Plan: holder, State: "paid", CreatedAt: now - 2000, PaidAt: now - 1000}
	outsider := &User{ID: "outsider", Email: "outsider@example.com", Role: "member", Status: "active", ExpiresAt: now - 1, TrafficTotal: commerceGB, RateMbps: 5, Level: 1, PlanID: holder.ID}
	if err := a.Store.Update(func(s *State) error {
		s.Users[user.ID].ExpiresAt, s.Users[user.ID].TrafficTotal, s.Users[user.ID].TrafficUsed = now+5*commerceDay, commerceGB, 0
		s.Users[user.ID].PlanID = ""
		s.Users[outsider.ID] = outsider
		if err := SaveDoc(s, "plans", holder.ID, holder); err != nil {
			return err
		}
		if err := SaveDoc(s, "plans", trial.ID, trial); err != nil {
			return err
		}
		if err := SaveDoc(s, "plans", trial2.ID, trial2); err != nil {
			return err
		}
		return SaveDoc(s, "orders", legacyOrder.ID, legacyOrder)
	}); err != nil {
		t.Fatal(err)
	}
	legacyTrialUser := &User{ID: "legacy-trial-user", Email: "legacy-trial@example.com", Role: "member", Status: "active", ExpiresAt: now + commerceDay, TrafficTotal: commerceGB, RateMbps: 5, Level: 1}
	legacyTrialOrder := Order{ID: "legacy-trial-paid", UserID: legacyTrialUser.ID, Plan: Plan{ID: trial2.ID, Name: trial2.Name}, State: "paid", CreatedAt: now - 4000, PaidAt: now - 3000}
	if err := a.Store.Update(func(s *State) error {
		s.Users[legacyTrialUser.ID] = legacyTrialUser
		return SaveDoc(s, "orders", legacyTrialOrder.ID, legacyTrialOrder)
	}); err != nil {
		t.Fatal(err)
	}
	response := commerceTestRequest(mux, legacyTrialUser, http.MethodPost, "/api/orders", map[string]any{"planId": trial.ID, "channelId": "balance", "requestId": "legacy-other-trial", "confirmReplace": true})
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "终身只能") {
		t.Fatalf("legacy paid order bypassed global trial guard: %d %s", response.Code, response.Body.String())
	}

	response = commerceTestRequest(mux, user, http.MethodGet, "/api/plans", nil)
	var listing struct {
		Plans []Plan `json:"plans"`
	}
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &listing) != nil || len(listing.Plans) != 3 {
		t.Fatalf("legacy current holder did not see restricted renewal: %d %s", response.Code, response.Body.String())
	}
	response = commerceTestRequest(mux, outsider, http.MethodGet, "/api/plans", nil)
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &listing) != nil || len(listing.Plans) != 2 {
		t.Fatalf("expired/non-current holder saw restricted plan: %d %s", response.Code, response.Body.String())
	}
	response = commerceTestRequest(mux, outsider, http.MethodPost, "/api/orders", map[string]any{"planId": holder.ID, "channelId": "balance", "requestId": "outsider-holder-01", "confirmReplace": true})
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "当前有效持有者") {
		t.Fatalf("non-current holder renewed restricted plan: %d %s", response.Code, response.Body.String())
	}

	response = commerceTestRequest(mux, user, http.MethodPost, "/api/orders", map[string]any{"planId": holder.ID, "channelId": "balance", "requestId": "legacy-renew-0001", "confirmReplace": true})
	if response.Code != http.StatusCreated {
		t.Fatalf("legacy holder renewal failed: %d %s", response.Code, response.Body.String())
	}
	if err := a.Store.View(func(s *State) error {
		if s.Users[user.ID].PlanID != holder.ID || s.Users[user.ID].Level != 2 {
			t.Fatalf("fulfilled plan identity/level not persisted: %+v", s.Users[user.ID])
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	statuses := make(chan int, 2)
	for _, requestID := range []string{"trial-race-000001", "trial-race-000002"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			statuses <- commerceTestRequest(mux, user, http.MethodPost, "/api/orders", map[string]any{"planId": trial.ID, "channelId": "balance", "requestId": id, "confirmReplace": true}).Code
		}(requestID)
	}
	wg.Wait()
	close(statuses)
	created, rejected := 0, 0
	for status := range statuses {
		if status == http.StatusCreated {
			created++
		} else if status == http.StatusConflict {
			rejected++
		}
	}
	if created != 1 || rejected != 1 {
		t.Fatalf("trial lifetime guard was not transaction-safe: created=%d rejected=%d", created, rejected)
	}
	response = commerceTestRequest(mux, user, http.MethodPost, "/api/orders", map[string]any{"planId": trial.ID, "channelId": "balance", "requestId": "trial-again-0001", "confirmReplace": true})
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "终身只能") {
		t.Fatalf("trial was redeemable twice: %d %s", response.Code, response.Body.String())
	}
	response = commerceTestRequest(mux, user, http.MethodPost, "/api/orders", map[string]any{"planId": trial2.ID, "channelId": "balance", "requestId": "other-trial-0001", "confirmReplace": true})
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "终身只能") {
		t.Fatalf("second trial plan bypassed lifetime guard: %d %s", response.Code, response.Body.String())
	}
	response = commerceTestRequest(mux, user, http.MethodGet, "/api/plans", nil)
	var policyListing struct {
		Plans []commercePlanView `json:"plans"`
	}
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &policyListing) != nil {
		t.Fatal(response.Body.String())
	}
	trialViews := 0
	for _, plan := range policyListing.Plans {
		if plan.Trial {
			trialViews++
			if plan.Eligible || !strings.Contains(plan.Reason, "终身只能") {
				t.Fatalf("redeemed trial not exposed as disabled with reason: %+v", plan)
			}
		}
		if plan.CurrentHoldersOnly {
			t.Fatalf("holder-only plan remained visible after current plan changed: %+v", plan)
		}
	}
	if trialViews != 2 {
		t.Fatalf("all trial choices should remain listed: %+v", policyListing.Plans)
	}
	response = commerceTestRequest(mux, admin, http.MethodPost, "/api/admin/plans", Plan{Name: "冲突策略", PriceCents: 0, Days: 1, TrafficBytes: commerceGB, RateMbps: 1, Enabled: true, Level: 1, Trial: true, CurrentHoldersOnly: true})
	if response.Code != http.StatusBadRequest {
		t.Fatalf("conflicting plan policies accepted: %d %s", response.Code, response.Body.String())
	}
	normal := Plan{ID: "formerly-normal", Name: "已有历史的普通权益", PriceCents: 100, Days: 1, TrafficBytes: commerceGB, RateMbps: 1, Enabled: true, Level: 1}
	if err := a.Store.Update(func(s *State) error {
		if err := SaveDoc(s, "plans", normal.ID, normal); err != nil {
			return err
		}
		return SaveDoc(s, "orders", "formerly-normal-pending", Order{ID: "formerly-normal-pending", UserID: outsider.ID, Plan: normal, State: "pending", CreatedAt: now})
	}); err != nil {
		t.Fatal(err)
	}
	normal.Trial = true
	response = commerceTestRequest(mux, admin, http.MethodPut, "/api/admin/plans/"+normal.ID, normal)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "不能改为体验权益") {
		t.Fatalf("historical normal plan was converted to trial: %d %s", response.Code, response.Body.String())
	}
}

func TestSupplement24AdminBalanceRequiresWholeLeaves(t *testing.T) {
	_, handler := identityFixture(t, true)
	adminCookie, csrf := identityLoginAdmin(t, handler)
	_, _, member := identityRegister(t, handler, "55667788@qq.com")
	response := identityRequest(handler, http.MethodPatch, "/api/admin/users/"+member["id"].(string), map[string]any{"balanceCents": 150, "reason": "fraction"}, adminCookie, csrf)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "枫叶必须为整数") {
		t.Fatalf("fractional admin balance accepted: %d %s", response.Code, response.Body.String())
	}
}

func TestSupplement24CurrentHolderPolicyRecheckedAtPaymentCallback(t *testing.T) {
	a, mux, user := commerceTestApp(t)
	secret := paymentSecrets{MerchantKey: "holder-policy-callback"}
	raw, _ := json.Marshal(secret)
	sealed, err := a.Seal(raw)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	plan := Plan{ID: "callback-holder-plan", Name: "旧用户续兑", PriceCents: 100, Days: 1, TrafficBytes: commerceGB, RateMbps: 5, Enabled: true, Level: 2, CurrentHoldersOnly: true}
	order := Order{ID: "callback-holder-order", UserID: user.ID, Plan: plan, AmountCents: 100, Currency: "CNY", ChannelID: "callback-holder-channel", State: "pending", CreatedAt: now, ExpiresAt: now + 60000, EntitlementVersion: 0}
	if err = a.Store.Update(func(s *State) error {
		s.Users[user.ID].ExpiresAt = now + commerceDay
		s.Users[user.ID].TrafficTotal = commerceGB
		s.Users[user.ID].PlanID = "another-plan"
		if err := SaveDoc(s, "plans", plan.ID, plan); err != nil {
			return err
		}
		if err := SaveDoc(s, "payment_channels", order.ChannelID, PaymentChannel{ID: order.ChannelID, Version: "v1", Type: "alipay", MerchantID: "merchant", Enabled: true, SealedSecrets: sealed}); err != nil {
			return err
		}
		return SaveDoc(s, "orders", order.ID, order)
	}); err != nil {
		t.Fatal(err)
	}
	values := url.Values{"pid": {"merchant"}, "out_trade_no": {order.ID}, "money": {"1.00"}, "type": {"alipay"}, "trade_no": {"holder-trade"}, "trade_status": {"TRADE_SUCCESS"}, "sign_type": {"MD5"}}
	signature, err := paymentSign(values, "v1", secret)
	if err != nil {
		t.Fatal(err)
	}
	values.Set("sign", signature)
	response := commerceTestRequest(mux, nil, http.MethodGet, "/api/payments/epay/"+order.ChannelID+"/notify?"+values.Encode(), nil)
	if response.Code != http.StatusOK {
		t.Fatalf("valid callback was not acknowledged: %d %s", response.Code, response.Body.String())
	}
	if err = a.Store.View(func(s *State) error {
		stored, ok := LoadDoc[Order](s, "orders", order.ID)
		if !ok || stored.State != "paid_review" || !strings.Contains(stored.ReviewReason, "兑换策略") || s.Users[user.ID].PlanID != "another-plan" {
			t.Fatalf("callback bypassed current-holder policy: %+v", stored)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSupplement24RouteLevelPurgeAndDiagnose(t *testing.T) {
	a, mux, user := commerceTestApp(t)
	admin := &User{ID: "route-admin", Role: "admin", Status: "active"}
	now := time.Now().UnixMilli()
	agent := RelayAgent{ID: "route-agent", Name: "节点", Address: "8.8.8.8", Addresses: []string{"8.8.8.8"}, Capability: "relay", Enabled: true, LastSeen: now, PortRanges: []PortRange{{Start: 24000, End: 24010}}}
	route1 := Route{ID: "route-l1", Name: "L1线路", EntryAgentID: agent.ID, EntryAddress: agent.Address, EntryAddresses: []string{agent.Address}, Type: "port_forward", Enabled: true, RateMbps: 5, Level: 1}
	route2 := Route{ID: "route-l2", Name: "L2线路", EntryAgentID: agent.ID, EntryAddress: agent.Address, EntryAddresses: []string{agent.Address}, Type: "port_forward", Enabled: true, RateMbps: 5, Level: 2}
	if err := a.Store.Update(func(s *State) error {
		u := s.Users[user.ID]
		u.ExpiresAt, u.TrafficTotal, u.Level = now+commerceDay, commerceGB, 1
		if err := SaveDoc(s, "relay_agents", agent.ID, agent); err != nil {
			return err
		}
		if err := SaveDoc(s, "routes", route1.ID, route1); err != nil {
			return err
		}
		return SaveDoc(s, "routes", route2.ID, route2)
	}); err != nil {
		t.Fatal(err)
	}

	response := commerceTestRequest(mux, user, http.MethodGet, "/api/routes", nil)
	var listed struct {
		Routes []Route `json:"routes"`
	}
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &listed) != nil || len(listed.Routes) != 1 || listed.Routes[0].ID != route1.ID {
		t.Fatalf("L1 route access incorrect: %d %s", response.Code, response.Body.String())
	}
	response = commerceTestRequest(mux, user, http.MethodPost, "/api/user/routes/"+route2.ID+"/rules", map[string]any{"config": json.RawMessage(relayTestConfig), "requestId": "level-denied-001"})
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "等级不足") {
		t.Fatalf("L1 user configured L2 route: %d %s", response.Code, response.Body.String())
	}
	if err := a.Store.Update(func(s *State) error { s.Users[user.ID].Level = 3; return nil }); err != nil {
		t.Fatal(err)
	}
	response = commerceTestRequest(mux, user, http.MethodGet, "/api/routes", nil)
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &listed) != nil || len(listed.Routes) != 2 {
		t.Fatalf("L3 route inheritance incorrect: %d %s", response.Code, response.Body.String())
	}
	response = commerceTestRequest(mux, admin, http.MethodPost, "/api/admin/routes/"+route2.ID+"/move", map[string]string{"direction": "up"})
	if response.Code != http.StatusOK {
		t.Fatalf("route move failed: %d %s", response.Code, response.Body.String())
	}
	response = commerceTestRequest(mux, admin, http.MethodGet, "/api/admin/routes", nil)
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &listed) != nil || len(listed.Routes) != 2 || listed.Routes[0].ID != route2.ID || listed.Routes[0].Order != 1 || listed.Routes[1].Order != 2 {
		t.Fatalf("route order is not deterministic: %d %s", response.Code, response.Body.String())
	}

	rule := UserRule{ID: "purge-rule", UserID: user.ID, RouteID: route1.ID, RouteName: route1.Name, State: "active", SealedConfig: "secret", Segments: []RelaySegment{{AgentID: agent.ID, ProtocolVersion: relayruntime.ProtocolV2, Runtime: relayruntime.Rule{ID: "purge-rule", Version: 1, ListenPort: 24000}}}}
	if err := a.Store.Update(func(s *State) error { return SaveDoc(s, "user_rules", user.ID+":"+route1.ID, rule) }); err != nil {
		t.Fatal(err)
	}
	response = commerceTestRequest(mux, admin, http.MethodPost, "/api/admin/routes/"+route1.ID+"/purge-rules", map[string]bool{"confirm": true})
	if response.Code != http.StatusAccepted {
		t.Fatalf("route purge failed: %d %s", response.Code, response.Body.String())
	}
	if err := a.Store.View(func(s *State) error {
		stored, ok := LoadDoc[UserRule](s, "user_rules", user.ID+":"+route1.ID)
		if !ok || stored.State != "revoking" || stored.SealedConfig != "" || stored.DeleteAfter == 0 {
			t.Fatalf("purge bypassed safe stop/tombstone state: %+v", stored)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	targetListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer targetListener.Close()
	entryListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer entryListener.Close()
	go func() {
		for {
			connection, acceptErr := targetListener.Accept()
			if acceptErr != nil {
				return
			}
			go func() { _, _ = io.Copy(io.Discard, connection); _ = connection.Close() }()
		}
	}()
	go func() {
		for {
			upstream, acceptErr := entryListener.Accept()
			if acceptErr != nil {
				return
			}
			downstream, dialErr := net.Dial("tcp", targetListener.Addr().String())
			if dialErr != nil {
				_ = upstream.Close()
				continue
			}
			go func() { _, _ = io.Copy(downstream, upstream); _ = downstream.Close() }()
			go func() { _, _ = io.Copy(upstream, downstream); _ = upstream.Close() }()
		}
	}()
	a.relayProbe = func(ctx context.Context, _, _ string) (int64, error) {
		return relayCompositeDiagnosticProbe(ctx, entryListener.Addr().String(), targetListener.Addr().String())
	}
	diagnosticRoute := Route{ID: "diagnostic", Name: "诊断线路", EntryAgentID: agent.ID, EntryAddress: agent.Address, Enabled: true, RateMbps: 5, Level: 1}
	diagnosticRule := UserRule{ID: "diagnostic-rule", UserID: user.ID, RouteID: diagnosticRoute.ID, RouteName: diagnosticRoute.Name, Version: 1, State: "active", EntryAddress: "8.8.8.8", EntryPort: 24009, TargetHost: "1.1.1.1", TargetPort: targetListener.Addr().(*net.TCPAddr).Port, Segments: []RelaySegment{{AgentID: agent.ID, AckState: "ready", LastLease: now + relayLeaseMS, Runtime: relayruntime.Rule{ID: "diagnostic-rule", Version: 1, ListenPort: 24009, Targets: []string{net.JoinHostPort("1.1.1.1", strconv.Itoa(targetListener.Addr().(*net.TCPAddr).Port))}, Protocol: "tcp", Strategy: "round", RateMbps: 5, Billing: true}}}}
	if err = a.Store.Update(func(s *State) error {
		if err := SaveDoc(s, "routes", diagnosticRoute.ID, diagnosticRoute); err != nil {
			return err
		}
		return SaveDoc(s, "user_rules", user.ID+":"+diagnosticRoute.ID, diagnosticRule)
	}); err != nil {
		t.Fatal(err)
	}
	response = commerceTestRequest(mux, user, http.MethodPost, "/api/user/routes/"+diagnosticRoute.ID+"/diagnose", nil)
	var diagnosis struct {
		Path              string `json:"path"`
		Status            string `json:"status"`
		PacketLossPercent int    `json:"packetLossPercent"`
		Scope             string `json:"scope"`
	}
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &diagnosis) != nil || diagnosis.Status != "success" || diagnosis.PacketLossPercent != 0 || diagnosis.Path != "入口(诊断线路)->目标(MSBOOST)" || diagnosis.Scope != "tcp_path" {
		t.Fatalf("successful fixed-endpoint diagnosis failed: %d %s", response.Code, response.Body.String())
	}
	_ = targetListener.Close()
	response = commerceTestRequest(mux, user, http.MethodPost, "/api/user/routes/"+diagnosticRoute.ID+"/diagnose", nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"status":"failed"`) || !strings.Contains(response.Body.String(), `"packetLossPercent":100`) || !strings.Contains(response.Body.String(), "目标 MSBOOST 连接失败") {
		t.Fatalf("downstream target failure was misreported: %d %s", response.Code, response.Body.String())
	}
}

func TestSupplement24RandomPortAndTrafficResetBaseline(t *testing.T) {
	a, _, user := commerceTestApp(t)
	now := time.Now().UnixMilli()
	epoch := "traffic-reset-epoch-0001"
	agent := RelayAgent{ID: "random-agent", PortRanges: []PortRange{{Start: 30000, End: 30003}}}
	rule := UserRule{ID: "traffic-reset-rule", UserID: user.ID, RouteID: "traffic-route", State: "active", TrafficBytes: 300, InputBytes: 100, OutputBytes: 200, EntitlementVersion: 1, TrafficEntitlementVersion: 1, TrafficMode: "both", TrafficMultiplierPermille: 1000, Segments: []RelaySegment{{AgentID: agent.ID, Runtime: relayruntime.Rule{ID: "traffic-reset-rule", Version: 1, ListenPort: 30001, Billing: true, EntitlementVersion: 1}}}}
	if err := a.Store.Update(func(s *State) error {
		s.Users[user.ID].TrafficUsed = 300
		if err := SaveDoc(s, "entitlement_versions", user.ID, int64(1)); err != nil {
			return err
		}
		if err := SaveDoc(s, "user_rules", user.ID+":"+rule.RouteID, rule); err != nil {
			return err
		}
		unconfirmedArchive := UserRule{ID: "unconfirmed-archive", State: "revoked", Segments: []RelaySegment{{AgentID: agent.ID, ProtocolVersion: relayruntime.ProtocolV2, Runtime: relayruntime.Rule{ID: "unconfirmed-archive", ListenPort: 30003}, LastCommandAction: "revoke", AckState: "pending"}}}
		if err := SaveDoc(s, "relay_rule_archive", unconfirmedArchive.ID, unconfirmedArchive); err != nil {
			return err
		}
		stoppedArchive := UserRule{ID: "stopped-archive", State: "revoked", Segments: []RelaySegment{{AgentID: agent.ID, ProtocolVersion: relayruntime.ProtocolV2, Runtime: relayruntime.Rule{ID: "stopped-archive", ListenPort: 30000}, LastCommandAction: "revoke", AckState: "stopped", StopConfirmed: true, ConfigGeneration: 1, AppliedGeneration: 1}}}
		if err := SaveDoc(s, "relay_rule_archive", stoppedArchive.ID, stoppedArchive); err != nil {
			return err
		}
		return SaveDoc(s, "traffic_cursors", agent.ID+":"+rule.ID+":"+epoch, TrafficCursor{Sequence: 1, InputBytes: 100, OutputBytes: 200})
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.Store.View(func(s *State) error {
		port, err := relayReservePortWithReader(s, agent, bytes.NewReader([]byte{1}))
		if err != nil || port != 30002 {
			t.Fatalf("secure random allocator ignored deterministic random index or collision: port=%d err=%v", port, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.Store.Update(func(s *State) error {
		s.Users[user.ID].TrafficUsed = 0
		if err := resetUserRuleTraffic(s, user.ID, 2); err != nil {
			return err
		}
		if err := SaveDoc(s, "entitlement_versions", user.ID, int64(2)); err != nil {
			return err
		}
		oldReport := relayruntime.Traffic{ID: rule.ID, Version: 1, Epoch: epoch, Sequence: 2, InputBytes: 120, OutputBytes: 240, EntitlementVersion: 1}
		if err := applyRelayTraffic(s, agent, oldReport, now); err != nil {
			return err
		}
		stored, _ := LoadDoc[UserRule](s, "user_rules", user.ID+":"+rule.RouteID)
		if s.Users[user.ID].TrafficUsed != 0 || stored.TrafficBytes != 0 || stored.InputBytes != 0 || stored.OutputBytes != 0 {
			t.Fatalf("old cumulative traffic flowed back after reset: user=%d rule=%+v", s.Users[user.ID].TrafficUsed, stored)
		}
		newReport := relayruntime.Traffic{ID: rule.ID, Version: 1, Epoch: epoch, Sequence: 3, InputBytes: 130, OutputBytes: 250, EntitlementVersion: 2}
		if err := applyRelayTraffic(s, agent, newReport, now+1); err != nil {
			return err
		}
		stored, _ = LoadDoc[UserRule](s, "user_rules", user.ID+":"+rule.RouteID)
		if s.Users[user.ID].TrafficUsed != 20 || stored.TrafficBytes != 20 || stored.InputBytes != 10 || stored.OutputBytes != 10 {
			t.Fatalf("post-reset traffic delta incorrect: user=%d rule=%+v", s.Users[user.ID].TrafficUsed, stored)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// V2 rotates the process epoch when BillingPeriodID changes. Preserve the
	// old cursor so delayed old-period samples advance only historical ledgers;
	// the new epoch starts at zero and cannot replay the old cumulative total.
	v2Epoch := "v2-reset-process-epoch"
	v2NewEpoch := "v2-new-process-epoch"
	oldGrant := RelayBillingGrantV2{ID: "v2-old-grant", AgentID: agent.ID, RuleID: rule.ID, UserID: user.ID, EntitlementVersion: 2, Billing: true, TrafficMode: "both", Multiplier: 1000, FirstVersion: 1, LastVersion: 1, CreatedAt: now - 2000}
	newGrant := oldGrant
	newGrant.ID, newGrant.EntitlementVersion = "v2-new-grant", 3
	if err := a.Store.Update(func(s *State) error {
		if err := SaveDoc(s, "relay_v2_billing_grants", oldGrant.ID, oldGrant); err != nil {
			return err
		}
		if err := SaveDoc(s, "relay_v2_billing_grants", newGrant.ID, newGrant); err != nil {
			return err
		}
		cursorKey := agent.ID + ":" + rule.ID + ":" + v2Epoch
		cursor := relayTrafficCursorV2{TrafficCursor: TrafficCursor{Sequence: 1, InputBytes: 100, OutputBytes: 200}, GrantID: oldGrant.ID, CollectedFrom: now - 1000, CollectedUntil: now - 500}
		if err := SaveDoc(s, "relay_v2_traffic_cursors", cursorKey, cursor); err != nil {
			return err
		}
		s.Users[user.ID].TrafficUsed = 0
		if err := resetUserRuleTraffic(s, user.ID, 3); err != nil {
			return err
		}
		if err := SaveDoc(s, "entitlement_versions", user.ID, int64(3)); err != nil {
			return err
		}
		stale := relayruntime.V2Traffic{Traffic: relayruntime.Traffic{ID: rule.ID, Version: 1, Epoch: v2Epoch, Sequence: 2, InputBytes: 110, OutputBytes: 220, EntitlementVersion: 2}, BillingPeriodID: oldGrant.ID, CollectedFrom: now - 1000, CollectedUntil: now}
		if _, err := applyRelayTrafficV2(s, agent, stale, now); err != nil {
			return err
		}
		stored, _ := LoadDoc[UserRule](s, "user_rules", user.ID+":"+rule.RouteID)
		if s.Users[user.ID].TrafficUsed != 0 || stored.TrafficBytes != 0 || stored.InputBytes != 0 || stored.OutputBytes != 0 {
			t.Fatalf("delayed old v2 report flowed into reset entitlement: user=%d rule=%+v", s.Users[user.ID].TrafficUsed, stored)
		}
		fresh := relayruntime.V2Traffic{Traffic: relayruntime.Traffic{ID: rule.ID, Version: 1, Epoch: v2NewEpoch, Sequence: 1, InputBytes: 10, OutputBytes: 20, EntitlementVersion: 3}, BillingPeriodID: newGrant.ID, CollectedFrom: now, CollectedUntil: now}
		if _, err := applyRelayTrafficV2(s, agent, fresh, now); err != nil {
			return err
		}
		stored, _ = LoadDoc[UserRule](s, "user_rules", user.ID+":"+rule.RouteID)
		if s.Users[user.ID].TrafficUsed != 30 || stored.TrafficBytes != 30 || stored.InputBytes != 10 || stored.OutputBytes != 20 {
			t.Fatalf("v2 reset replayed lifetime totals or lost directional delta: user=%d rule=%+v", s.Users[user.ID].TrafficUsed, stored)
		}
		oldCursor, _ := LoadDoc[relayTrafficCursorV2](s, "relay_v2_traffic_cursors", cursorKey)
		newCursor, _ := LoadDoc[relayTrafficCursorV2](s, "relay_v2_traffic_cursors", agent.ID+":"+rule.ID+":"+v2NewEpoch)
		if oldCursor.GrantID != oldGrant.ID || oldCursor.InputBytes != 110 || oldCursor.OutputBytes != 220 || newCursor.GrantID != newGrant.ID || newCursor.InputBytes != 10 || newCursor.OutputBytes != 20 {
			t.Fatalf("v2 rotated epochs mixed entitlement periods: old=%+v new=%+v", oldCursor, newCursor)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSupplement24TicketUnreadAndAdminCannotCreate(t *testing.T) {
	_, mux, admin, member := contentFixture(t)
	response := contentCall(t, mux, admin, http.MethodPost, "/api/tickets", map[string]string{"title": "admin", "body": "self"})
	if response.Code != http.StatusForbidden {
		t.Fatalf("administrator self-created a ticket: %d %s", response.Code, response.Body.String())
	}
	response = contentCall(t, mux, member, http.MethodPost, "/api/tickets", map[string]string{"title": "help", "body": "member message"})
	if response.Code != http.StatusCreated {
		t.Fatal(response.Body.String())
	}
	var ticket Ticket
	if err := json.Unmarshal(response.Body.Bytes(), &ticket); err != nil {
		t.Fatal(err)
	}
	assertUnread := func(actor *User, want int) {
		t.Helper()
		listed := contentCall(t, mux, actor, http.MethodGet, "/api/tickets", nil)
		var body struct {
			UnreadCount int `json:"unreadCount"`
		}
		if listed.Code != http.StatusOK || json.Unmarshal(listed.Body.Bytes(), &body) != nil || body.UnreadCount != want {
			t.Fatalf("unread count got %d want %d: %s", body.UnreadCount, want, listed.Body.String())
		}
	}
	assertUnread(admin, 1)
	response = contentCall(t, mux, admin, http.MethodPost, "/api/tickets/"+ticket.ID+"/read", nil)
	if response.Code != http.StatusOK {
		t.Fatal(response.Body.String())
	}
	assertUnread(admin, 0)
	response = contentCall(t, mux, admin, http.MethodPost, "/api/tickets/"+ticket.ID+"/replies", map[string]string{"body": "administrator response"})
	if response.Code != http.StatusOK {
		t.Fatal(response.Body.String())
	}
	assertUnread(member, 1)
	response = contentCall(t, mux, member, http.MethodPost, "/api/tickets/"+ticket.ID+"/read", nil)
	if response.Code != http.StatusOK {
		t.Fatal(response.Body.String())
	}
	assertUnread(member, 0)
}
