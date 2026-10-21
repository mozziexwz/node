package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mozziexwz/node/internal/executor"
)

func verifyReport(code string) executor.VerifyReport {
	if code == executor.VerifyOK {
		return executor.VerifyReport{Code: code, Game: "ok", Filter: "ok"}
	}
	return executor.VerifyReport{Code: code}
}

func verifyRuleState(t *testing.T) (*State, *UserRule) {
	t.Helper()
	s := &State{Users: map[string]*User{"u": {ID: "u", Status: "active", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}}, Settings: map[string]any{}, Docs: map[string]map[string]json.RawMessage{}}
	rule := &UserRule{ID: "rule", UserID: "u", RouteID: "route", Version: 1, State: "active", SealedConfig: "sealed", Segments: []RelaySegment{{AgentID: "a"}}}
	return s, rule
}

func TestRuleVerifyStateMachine(t *testing.T) {
	logging := defaultRelayVerifySettings()
	enforcing := defaultRelayVerifySettings()
	enforcing.Donation = "enforce"
	now := time.Now().UnixMilli()
	minute := time.Minute.Milliseconds()

	t.Run("a new rule gets two quick retries, then is recorded or withdrawn", func(t *testing.T) {
		for _, v := range []RelayVerifySettings{logging, enforcing} {
			s, rule := verifyRuleState(t)
			rule.Verify = &RuleVerify{State: "pending"}
			for i := 1; i <= verifyCreateRetries; i++ {
				if events := applyRuleVerify(s, rule, verifyReport(executor.VerifyNotMSBOOST), v, now); events != nil || rule.Verify.Retries != i || rule.Verify.NextAt != now+minute {
					t.Fatalf("retry %d: %+v %v", i, rule.Verify, events)
				}
			}
			events := applyRuleVerify(s, rule, verifyReport(executor.VerifyTunnelFailed), v, now)
			if rule.Verify.State != "failed" || rule.Verify.FailedSince != now {
				t.Fatalf("after retries: %+v", rule.Verify)
			}
			if v.Donation == "enforce" {
				if rule.State != "revoking" || rule.SealedConfig != "" || len(events) != 1 || events[0] != "relay.verify.refused" {
					t.Fatalf("enforced creation not withdrawn: %s %v", rule.State, events)
				}
			} else if rule.State != "active" || len(events) != 1 || events[0] != "relay.verify.failed" {
				t.Fatalf("logged creation changed the rule: %s %v", rule.State, events)
			}
		}
		// A stopped node blocks a new rule too, but never an existing one.
		s, rule := verifyRuleState(t)
		rule.Verify = &RuleVerify{State: "pending", Retries: verifyCreateRetries}
		applyRuleVerify(s, rule, verifyReport(executor.VerifyTargetDown), logging, now)
		if rule.Verify.State != "failed" {
			t.Fatalf("a new rule to a stopped node: %+v", rule.Verify)
		}
	})

	t.Run("an existing rule fails only after three retries and is paused after the grace period", func(t *testing.T) {
		s, rule := verifyRuleState(t)
		rule.Verify = &RuleVerify{State: "ok", PassedAt: now - minute}
		for i := 1; i <= verifyQuickRetries; i++ {
			applyRuleVerify(s, rule, verifyReport(executor.VerifyNotMSBOOST), enforcing, now+int64(i)*minute)
			if rule.Verify.State != "ok" || rule.Verify.Retries != i || rule.Verify.FailedSince != now+minute {
				t.Fatalf("retry %d: %+v", i, rule.Verify)
			}
		}
		failedAt := now + 40*minute
		events := applyRuleVerify(s, rule, verifyReport(executor.VerifyNotMSBOOST), enforcing, failedAt)
		if rule.Verify.State != "failed" || len(events) != 1 || events[0] != "relay.verify.failed" || rule.State != "active" {
			t.Fatalf("after retries: %+v %v", rule.Verify, events)
		}
		pauseAt := rule.Verify.FailedSince + 48*time.Hour.Milliseconds()
		if rule.Verify.NextAt != failedAt+verifyFailedEvery.Milliseconds() {
			t.Fatalf("grace checks: %d", rule.Verify.NextAt)
		}
		// Still within the grace period: nothing happens to the rule.
		applyRuleVerify(s, rule, verifyReport(executor.VerifyTunnelFailed), enforcing, pauseAt-time.Hour.Milliseconds())
		if rule.State != "active" || rule.Verify.NextAt != pauseAt {
			t.Fatalf("paused early or check after the deadline: %s %d", rule.State, rule.Verify.NextAt)
		}
		events = applyRuleVerify(s, rule, verifyReport(executor.VerifyTunnelFailed), enforcing, pauseAt)
		if rule.State != "paused" || rule.Verify.State != "paused" || rule.Verify.PausedAt != pauseAt || len(events) != 1 || events[0] != "relay.verify.paused" {
			t.Fatalf("not paused after the grace period: %s %+v %v", rule.State, rule.Verify, events)
		}
		version := rule.Version
		events = applyRuleVerify(s, rule, verifyReport(executor.VerifyOK), enforcing, pauseAt+time.Hour.Milliseconds())
		if rule.State != "pending" || rule.Version != version+1 || rule.Verify.State != "ok" || rule.Verify.FailedSince != 0 || len(events) != 1 || events[0] != "relay.verify.resumed" {
			t.Fatalf("not resumed after passing: %s %+v %v", rule.State, rule.Verify, events)
		}
		// Recording never pauses.
		rule.Verify = &RuleVerify{State: "failed", FailedSince: now - 100*time.Hour.Milliseconds()}
		rule.State = "active"
		applyRuleVerify(s, rule, verifyReport(executor.VerifyNotMSBOOST), logging, now)
		if rule.State != "active" || rule.Verify.State != "failed" {
			t.Fatalf("a logged failure paused the rule: %s %+v", rule.State, rule.Verify)
		}
	})

	t.Run("problems that are not the user's never count", func(t *testing.T) {
		for _, code := range []string{executor.VerifyTargetDown, executor.VerifyEntryUnreachable, executor.VerifyUnavailable} {
			s, rule := verifyRuleState(t)
			rule.Verify = &RuleVerify{State: "ok", PassedAt: now - minute}
			events := applyRuleVerify(s, rule, verifyReport(code), enforcing, now)
			if rule.Verify.State != "ok" || rule.Verify.Retries != 0 || rule.Verify.FailedSince != 0 || rule.Verify.Code != code || rule.Verify.NextAt != now+verifyLaterEvery.Milliseconds() {
				t.Fatalf("%s counted: %+v", code, rule.Verify)
			}
			if (code == executor.VerifyEntryUnreachable) != (len(events) == 1 && events[0] == "relay.verify.route_problem") {
				t.Fatalf("%s events: %v", code, events)
			}
		}
	})

	t.Run("a pass is rechecked after about a day", func(t *testing.T) {
		s, rule := verifyRuleState(t)
		rule.Verify = &RuleVerify{State: "unchecked"}
		applyRuleVerify(s, rule, verifyReport(executor.VerifyOK), logging, now)
		day := 24 * time.Hour.Milliseconds()
		if rule.Verify.State != "ok" || rule.Verify.PassedAt != now || rule.Verify.NextAt < now+day*9/10 || rule.Verify.NextAt > now+day*11/10 || rule.Verify.Game != "ok" {
			t.Fatalf("pass: %+v", rule.Verify)
		}
	})
}

func TestRuleVerifyMaintenance(t *testing.T) {
	now := time.Now().UnixMilli()
	s, rule := verifyRuleState(t)
	logging := defaultRelayVerifySettings()
	// Rules from before verification are spread over the first interval.
	if changed, _ := relayVerifyMaintain(s, rule, logging, now); !changed || rule.Verify == nil || rule.Verify.State != "unchecked" || rule.Verify.NextAt < now || rule.Verify.NextAt > now+24*time.Hour.Milliseconds() {
		t.Fatalf("legacy rule: %+v", rule.Verify)
	}
	off := logging
	off.Donation = "off"
	other := &UserRule{ID: "x", State: "active", SealedConfig: "sealed"}
	if changed, _ := relayVerifyMaintain(s, other, off, now); changed || other.Verify != nil {
		t.Fatal("scheduled while switched off")
	}
	// A verification pause ends as soon as nothing is enforced.
	rule.State = "paused"
	rule.Verify = &RuleVerify{State: "paused", PausedAt: now - 1}
	changed, events := relayVerifyMaintain(s, rule, logging, now)
	if !changed || rule.State != "pending" || rule.Verify.State != "failed" || len(events) != 1 || events[0] != "relay.verify.resumed" {
		t.Fatalf("pause kept without enforcement: %s %+v", rule.State, rule.Verify)
	}
	// Switching verification off entirely lifts its pauses too: nothing would
	// ever check the rule again, and members cannot resume such a pause.
	rule.State = "paused"
	rule.Verify = &RuleVerify{State: "paused", PausedAt: now - 1}
	changed, events = relayVerifyMaintain(s, rule, off, now)
	if !changed || rule.State != "pending" || rule.Verify.State != "failed" || len(events) != 1 || events[0] != "relay.verify.resumed" {
		t.Fatalf("pause kept with verification off: %s %+v", rule.State, rule.Verify)
	}
	// A member's view hides an old result while verification is off.
	s.Settings["relayVerify"] = off
	var member, admin RelayRuleView
	relayVerifyView(s, &member, *rule, false)
	relayVerifyView(s, &admin, *rule, true)
	if member.Verify != nil || member.VerifyMode != "" || admin.Verify == nil || admin.VerifyMode != "off" {
		t.Fatalf("view with verification off: member %+v, admin %+v", member.Verify, admin.Verify)
	}
}

func TestRuleVerifyBreaker(t *testing.T) {
	var w relayVerifyWork
	now := time.Now().UnixMilli()
	for i := 0; i < 4; i++ {
		w.breaker(now, false, 20)
	}
	if tripped, _ := w.breaker(now, true, 20); tripped {
		t.Fatal("one failure in five is exactly 20% and must not trip")
	}
	if tripped, alert := w.breaker(now, true, 20); !tripped || !alert {
		t.Fatal("two failures in six are above 20% and must trip")
	}
	if tripped, alert := w.breaker(now, true, 20); !tripped || alert {
		t.Fatal("tripped again but alerted twice within an hour")
	}
	var fresh relayVerifyWork
	if tripped, _ := fresh.breaker(now, true, 20); tripped {
		t.Fatal("a single result is not a pattern")
	}
	if tripped, _ := w.breaker(now+2*time.Hour.Milliseconds(), true, 20); tripped {
		t.Fatal("old outcomes were not forgotten")
	}
}

type stubRouteVerifier struct {
	mu      sync.Mutex
	codes   []string
	configs []executor.ConfigInfo
	users   []string
}

func (v *stubRouteVerifier) verify(_ context.Context, userID string, config []byte, options executor.VerifyOptions) (executor.VerifyReport, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	info, err := executor.ParseClientConfig(config)
	if err != nil || options.Mode != "log" {
		return executor.VerifyReport{}, errors.New("bad request")
	}
	v.configs = append(v.configs, info)
	v.users = append(v.users, userID)
	code := executor.VerifyOK
	if len(v.codes) > 0 {
		code, v.codes = v.codes[0], v.codes[1:]
	}
	return verifyReport(code), nil
}

func (f *relayV2Fixture) verifiedRule(t *testing.T, check *RuleVerify) string {
	t.Helper()
	key := f.user.ID + ":route"
	if err := f.app.Store.Update(func(s *State) error {
		rule, _ := LoadDoc[UserRule](s, "user_rules", key)
		rule.RouteName, rule.EntryAddress, rule.EntryPort = "测试线路", f.agents[0].Address, rule.Segments[0].Runtime.ListenPort
		rule.TargetHost, rule.TargetPort = "1.1.1.1", 443
		config, err := executor.RewriteClientConfig([]byte(relayTestConfig), rule.EntryAddress, rule.EntryPort)
		if err != nil {
			return err
		}
		if rule.SealedConfig, err = f.app.Seal(config); err != nil {
			return err
		}
		rule.Verify = check
		return SaveDoc(s, "user_rules", key, rule)
	}); err != nil {
		t.Fatal(err)
	}
	return key
}

func (f *relayV2Fixture) runVerify(t *testing.T, key string) {
	t.Helper()
	f.app.relayVerifyTick(context.Background())
	deadline := time.Now().Add(5 * time.Second)
	for f.app.relayVerify.busy(key) {
		if time.Now().After(deadline) {
			t.Fatal("verification did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (f *relayV2Fixture) rule(t *testing.T, key string) UserRule {
	t.Helper()
	var rule UserRule
	_ = f.app.Store.View(func(s *State) error { rule, _ = LoadDoc[UserRule](s, "user_rules", key); return nil })
	return rule
}

func TestRouteVerificationThroughTheEntryAndAtThePausedNode(t *testing.T) {
	f := newRelayV2Fixture(t)
	f.ready(t)
	stub := &stubRouteVerifier{}
	f.app.SetRouteVerifier(stub.verify)
	key := f.verifiedRule(t, &RuleVerify{State: "pending"})
	_ = f.app.Store.Update(func(s *State) error {
		s.Settings["relayVerify"] = RelayVerifySettings{Free: "selftest", Code: "log", Donation: "enforce", RecheckHours: 24, GraceHours: 48, BreakerPercent: 20, GameTarget: "35.155.204.207:8585", FilterBlocked: "www.youtube.com", FilterAllowed: "www.nexon.com"}
		return nil
	})
	// Enforced: nothing to download before the target has passed.
	if w := commerceTestRequest(f.mux, f.user, "GET", "/api/user/routes/route/config", nil); w.Code != 403 || !strings.Contains(w.Body.String(), "校验") {
		t.Fatalf("download before verification: %d %s", w.Code, w.Body.String())
	}
	f.runVerify(t, key)
	rule := f.rule(t, key)
	if len(stub.configs) != 1 || stub.users[0] != f.user.ID || stub.configs[0].TargetHost != f.agents[0].Address || stub.configs[0].TargetPort != rule.EntryPort {
		t.Fatalf("checked something other than the route entry: %+v", stub.configs)
	}
	if rule.Verify.State != "ok" || rule.Verify.PassedAt == 0 {
		t.Fatalf("rule after passing: %+v", rule.Verify)
	}
	if w := commerceTestRequest(f.mux, f.user, "GET", "/api/user/routes/route/config", nil); w.Code != 200 {
		t.Fatalf("download after verification: %d %s", w.Code, w.Body.String())
	}
	// The owner can ask again, but not twice in a row.
	if w := commerceTestRequest(f.mux, f.user, "POST", "/api/user/routes/route/verify", nil); w.Code != 409 || !strings.Contains(w.Body.String(), "两分钟") {
		t.Fatalf("immediate recheck: %d %s", w.Code, w.Body.String())
	}
	_ = f.app.Store.Update(func(s *State) error {
		rule, _ := LoadDoc[UserRule](s, "user_rules", key)
		rule.Verify.CheckedAt -= verifyManualGap.Milliseconds()
		return SaveDoc(s, "user_rules", key, rule)
	})
	if w := commerceTestRequest(f.mux, f.user, "POST", "/api/user/routes/route/verify", nil); w.Code != 202 {
		t.Fatalf("recheck: %d %s", w.Code, w.Body.String())
	}
	if rule = f.rule(t, key); rule.Verify.NextAt > time.Now().UnixMilli() {
		t.Fatal("recheck not scheduled")
	}
	// A verification pause cannot be lifted by the user, only by passing.
	_ = f.app.Store.Update(func(s *State) error {
		rule, _ := LoadDoc[UserRule](s, "user_rules", key)
		rule.State = "paused"
		rule.Verify = &RuleVerify{State: "paused", PausedAt: time.Now().UnixMilli(), FailedSince: 1}
		return SaveDoc(s, "user_rules", key, rule)
	})
	if w := commerceTestRequest(f.mux, f.user, "PATCH", "/api/user/routes/route/rules", map[string]bool{"paused": false}); w.Code != 409 || !strings.Contains(w.Body.String(), "重新校验") {
		t.Fatalf("user lifted a verification pause: %d %s", w.Code, w.Body.String())
	}
	// A paused rule is checked at the node itself.
	f.runVerify(t, key)
	rule = f.rule(t, key)
	last := stub.configs[len(stub.configs)-1]
	if last.TargetHost != "1.1.1.1" || last.TargetPort != 443 {
		t.Fatalf("paused rule checked through its stopped entry: %+v", last)
	}
	if rule.State != "pending" || rule.Verify.State != "ok" {
		t.Fatalf("passing did not resume: %s %+v", rule.State, rule.Verify)
	}
}

func TestRouteVerificationExemption(t *testing.T) {
	f := newRelayV2Fixture(t)
	f.ready(t)
	key := f.verifiedRule(t, &RuleVerify{State: "failed", FailedSince: 1, Code: executor.VerifyNotMSBOOST})
	admin := &User{ID: "admin-verify", Email: "admin@example.com", Role: "admin", Status: "active"}
	_ = f.app.Store.Update(func(s *State) error { s.Users[admin.ID] = admin; return nil })
	path := "/api/admin/user-rules/" + f.ruleID + "/verify"
	if w := commerceTestRequest(f.mux, admin, "POST", path, map[string]string{"action": "exempt", "reason": " "}); w.Code != 400 {
		t.Fatalf("exemption without a reason: %d %s", w.Code, w.Body.String())
	}
	if w := commerceTestRequest(f.mux, f.user, "POST", path, map[string]string{"action": "exempt", "reason": "客户自建节点，已人工核查"}); w.Code != 403 {
		t.Fatalf("member exempted a rule: %d", w.Code)
	}
	w := commerceTestRequest(f.mux, admin, "POST", path, map[string]string{"action": "exempt", "reason": "客户自建节点，已人工核查"})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "已人工核查") {
		t.Fatalf("exempt: %d %s", w.Code, w.Body.String())
	}
	rule := f.rule(t, key)
	if rule.Verify.State != "exempt" || rule.Verify.ExemptReason != "客户自建节点，已人工核查" {
		t.Fatalf("exemption: %+v", rule.Verify)
	}
	audited := false
	_ = f.app.Store.View(func(s *State) error {
		for _, raw := range s.Docs["commerce_audit"] {
			audited = audited || (strings.Contains(string(raw), "relay.verify.exempt") && strings.Contains(string(raw), admin.ID))
		}
		return nil
	})
	if user := commerceTestRequest(f.mux, f.user, "GET", "/api/user/rules", nil); user.Code != 200 || strings.Contains(user.Body.String(), "已人工核查") || !strings.Contains(user.Body.String(), `"state":"exempt"`) {
		t.Fatalf("member view: %d %s", user.Code, user.Body.String())
	}
	if !audited {
		t.Fatal("exemption not audited")
	}
	if relayVerifyDueFor(f, key) {
		t.Fatal("an exempt rule is still checked")
	}
	if w := commerceTestRequest(f.mux, admin, "POST", path, map[string]string{"action": "unexempt"}); w.Code != 200 {
		t.Fatalf("unexempt: %d %s", w.Code, w.Body.String())
	}
	if rule = f.rule(t, key); rule.Verify.State != "unchecked" || !relayVerifyDueFor(f, key) {
		t.Fatalf("after unexempt: %+v", rule.Verify)
	}
}

func relayVerifyDueFor(f *relayV2Fixture, key string) bool {
	due := false
	_ = f.app.Store.View(func(s *State) error {
		rule, _ := LoadDoc[UserRule](s, "user_rules", key)
		due = relayVerifyDue(s, rule, relayVerifySettings(s), time.Now().UnixMilli())
		return nil
	})
	return due
}

func TestVerifyRouteRunsOnlyOnVerifyingExecutors(t *testing.T) {
	h := newRelayCodeHarness(t)
	options := executor.VerifyOptions{Mode: "log"}
	config, _ := executor.RewriteClientConfig([]byte(relayTestConfig), "8.8.4.4", 20000)
	if _, err := h.service.VerifyRoute(context.Background(), h.friend.ID, config, options); err == nil || !strings.Contains(err.Error(), "升级") {
		t.Fatalf("queued for executors that cannot verify: %v", err)
	}
	h.verifyCapable(true)
	type answer struct {
		report executor.VerifyReport
		err    error
	}
	done := make(chan answer, 1)
	go func() {
		report, err := h.service.VerifyRoute(context.Background(), h.friend.ID, config, options)
		done <- answer{report, err}
	}()
	var job executor.Job
	for i := 0; i < 50 && job.ID == ""; i++ {
		h.service.mu.Lock()
		queued := len(h.service.envelopes) > 0
		h.service.mu.Unlock()
		if queued {
			job = h.claimVerifying()
		} else {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if job.Request.Kind != "relay-verify" || len(job.Request.ClientConfig) == 0 || job.Request.Verify == nil || job.Request.SSH.Host != "" {
		t.Fatalf("route check job: %+v", job.Request)
	}
	report := verifyReport(executor.VerifyOK)
	h.report(executor.Result{ID: job.ID, Lease: job.Lease, State: "succeeded", Phase: "complete", Verify: &report})
	got := <-done
	if got.err != nil || got.report.Code != executor.VerifyOK {
		t.Fatalf("adapter: %+v", got)
	}
	// The member never sees the internal check in their task list.
	list := h.call(h.service.list, h.friend, "GET", "/api/tasks", "", nil)
	if strings.Contains(list.Body.String(), job.ID) {
		t.Fatal("relay-verify task listed for the member")
	}
	h.state(func(s *State) {
		o, _ := LoadDoc[taskOperation](s, "task_operations", job.ID)
		if len(o.Request.ClientConfig) != 0 {
			t.Fatal("route configuration persisted with the operation")
		}
	})
	_ = httptest.NewRecorder()
}

func TestRuleVerifyEmailsRender(t *testing.T) {
	for kind, subject := range map[string]string{"relay_verify_failed": "[MSBOOST] 中转线路校验未通过", "relay_verify_paused": "[MSBOOST] 中转线路已暂停"} {
		m, err := RenderEmail(kind, EmailData{EventAtLabel: "2026-10-21 10:00:00 UTC+8", SiteURL: "https://msboost.de", Year: 2026})
		if err != nil || m.Subject != subject {
			t.Fatalf("%s: %v %q", kind, err, m.Subject)
		}
		for _, banned := range []string{"转发", "冒险岛", "SSH", "root"} {
			if strings.Contains(m.TextBody, banned) {
				t.Fatalf("%s mentions %s", kind, banned)
			}
		}
		if !strings.Contains(m.TextBody, "重新校验") {
			t.Fatalf("%s does not say how to recover", kind)
		}
	}
}

func TestRuleVerifyMailsOnlyWhenEnforced(t *testing.T) {
	for _, test := range []struct{ event, mode, want string }{
		{"relay.verify.failed", "log", ""},
		{"relay.verify.failed", "enforce", "relay_verify_failed"},
		{"relay.verify.refused", "enforce", "relay_verify_failed"},
		{"relay.verify.paused", "enforce", "relay_verify_paused"},
		{"relay.verify.resumed", "enforce", ""},
		{"relay.verify.route_problem", "enforce", ""},
	} {
		if got := ruleVerifyMail(test.event, test.mode); got != test.want {
			t.Errorf("%s in %s mails %q; want %q", test.event, test.mode, got, test.want)
		}
	}
}
