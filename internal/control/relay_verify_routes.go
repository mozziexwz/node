package control

import (
	"context"
	cryptorand "crypto/rand"
	"errors"
	"math/big"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/mozziexwz/node/internal/executor"
	"github.com/mozziexwz/node/internal/relayruntime"
)

// RuleVerify is a paid-route rule's target verification. States:
//   - pending: a new rule not yet verified; with enforcement it cannot be
//     downloaded until it passes and is withdrawn if it keeps failing;
//   - unchecked: a rule from before verification, checked on the normal path;
//   - ok, failed, paused (enforced after the grace period) and exempt.
//
// Code is the last result, including ones that are never held against the
// user (target_down, entry_unreachable, unavailable).
type RuleVerify struct {
	State        string `json:"state"`
	Code         string `json:"code,omitempty"`
	Game         string `json:"game,omitempty"`
	Filter       string `json:"filter,omitempty"`
	CheckedAt    int64  `json:"checkedAt,omitempty"`
	PassedAt     int64  `json:"passedAt,omitempty"`
	FailedSince  int64  `json:"failedSince,omitempty"`
	Retries      int    `json:"retries,omitempty"`
	NextAt       int64  `json:"nextAt,omitempty"`
	PausedAt     int64  `json:"pausedAt,omitempty"`
	ExemptReason string `json:"exemptReason,omitempty"`
}

const (
	verifyCreateRetries = 2
	verifyCreateRetry   = time.Minute
	verifyQuickRetries  = 3
	verifyQuickRetry    = 10 * time.Minute
	verifyFailedEvery   = 6 * time.Hour
	verifyLaterEvery    = 30 * time.Minute
	verifyConcurrency   = 3
	verifyManualGap     = 2 * time.Minute
)

type routeVerifier func(context.Context, string, []byte, executor.VerifyOptions) (executor.VerifyReport, error)

var relayRouteVerifiers sync.Map

// SetRouteVerifier connects the task service that runs relay-verify jobs.
func (a *App) SetRouteVerifier(fn func(context.Context, string, []byte, executor.VerifyOptions) (executor.VerifyReport, error)) {
	relayRouteVerifiers.Store(a, routeVerifier(fn))
}

type verifyOutcome struct {
	at       int64
	blocking bool
}

// relayVerifyWork is the scheduler's memory: checks in flight, the recent
// outcomes the circuit breaker looks at, and a wake-up for manual checks.
type relayVerifyWork struct {
	mu       sync.Mutex
	inFlight map[string]bool
	recent   []verifyOutcome
	alerted  int64
	kick     chan struct{}
}

func (w *relayVerifyWork) wake() {
	w.mu.Lock()
	if w.kick == nil {
		w.kick = make(chan struct{}, 1)
	}
	kick := w.kick
	w.mu.Unlock()
	select {
	case kick <- struct{}{}:
	default:
	}
}

func (w *relayVerifyWork) begin(key string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.inFlight == nil {
		w.inFlight = map[string]bool{}
	}
	if w.inFlight[key] || len(w.inFlight) >= verifyConcurrency {
		return false
	}
	w.inFlight[key] = true
	return true
}

func (w *relayVerifyWork) end(key string) {
	w.mu.Lock()
	delete(w.inFlight, key)
	w.mu.Unlock()
}

func (w *relayVerifyWork) busy(key string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.inFlight[key]
}

// breaker records one outcome and reports whether the last hour's failure
// share is so high that the checking side, not the users, is at fault.
func (w *relayVerifyWork) breaker(now int64, blocking bool, percent int) (tripped, alert bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	kept := w.recent[:0]
	for _, o := range w.recent {
		if o.at > now-time.Hour.Milliseconds() {
			kept = append(kept, o)
		}
	}
	w.recent = append(kept, verifyOutcome{at: now, blocking: blocking})
	total, failed := len(w.recent), 0
	for _, o := range w.recent {
		if o.blocking {
			failed++
		}
	}
	tripped = blocking && total >= 5 && failed*100 > percent*total
	if tripped && w.alerted <= now-time.Hour.Milliseconds() {
		w.alerted, alert = now, true
	}
	return tripped, alert
}

func jitter(base time.Duration) int64 {
	spread := base.Milliseconds() / 10
	if spread <= 0 {
		return base.Milliseconds()
	}
	n, err := cryptorand.Int(cryptorand.Reader, big.NewInt(2*spread+1))
	if err != nil {
		return base.Milliseconds()
	}
	return base.Milliseconds() - spread + n.Int64()
}

func randomWithin(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	n, err := cryptorand.Int(cryptorand.Reader, big.NewInt(d.Milliseconds()))
	if err != nil {
		return 0
	}
	return n.Int64()
}

// relayResumeRule restarts a paused rule the same way an administrator's
// resume does: a new version every segment has to acknowledge again.
func relayResumeRule(rule *UserRule) {
	if rule.State == "active" || rule.State == "pending" {
		return
	}
	rule.State = "pending"
	rule.Version++
	for i := range rule.Segments {
		rule.Segments[i].Runtime.Version = rule.Version
		rule.Segments[i].AckState, rule.Segments[i].AckAt = "pending", 0
	}
}

// relayVerifyMaintain adapts a rule to the current policy: a pause made by
// verification is lifted as soon as the policy no longer enforces, including
// when it is switched off, and rules from before verification get a first
// check spread over one interval.
func relayVerifyMaintain(s *State, rule *UserRule, v RelayVerifySettings, now int64) (changed bool, events []string) {
	if rule.Verify != nil && rule.Verify.State == "paused" && v.Donation != "enforce" {
		rule.Verify.State, rule.Verify.PausedAt, rule.Verify.NextAt = "failed", 0, now
		if rule.State == "paused" {
			relayResumeRule(rule)
			relayRefreshPolicy(s, rule)
		}
		return true, []string{"relay.verify.resumed"}
	}
	if v.Donation == "off" || rule.Verify != nil || rule.SealedConfig == "" || (rule.State != "active" && rule.State != "pending") {
		return false, nil
	}
	interval := time.Duration(v.RecheckHours) * time.Hour
	rule.Verify = &RuleVerify{State: "unchecked", NextAt: now + randomWithin(interval)}
	return true, nil
}

// relayVerifyDue says whether the scheduler should check a rule now. Paused
// rules are checked directly at the node, since their relay is stopped.
func relayVerifyDue(s *State, rule UserRule, v RelayVerifySettings, now int64) bool {
	check := rule.Verify
	if v.Donation == "off" || check == nil || check.State == "exempt" || check.NextAt > now || rule.SealedConfig == "" || relayRecoveryRequired(s, rule) {
		return false
	}
	user := s.Users[rule.UserID]
	route, ok := LoadDoc[Route](s, "routes", rule.RouteID)
	if !ok || !route.Enabled || !relayEntitled(user, now) || !userCanUseRoute(user, route) {
		return false
	}
	if check.State == "paused" {
		return rule.State == "paused"
	}
	return rule.State == "active" && relayRuleView(s, rule, false, now).SyncState == "active"
}

// applyRuleVerify moves a rule's verification by one result and returns the
// audit events it caused. It also pauses, resumes or withdraws the rule.
func applyRuleVerify(s *State, rule *UserRule, report executor.VerifyReport, v RelayVerifySettings, now int64) []string {
	if rule.Verify == nil {
		rule.Verify = &RuleVerify{State: "unchecked"}
	}
	check := rule.Verify
	check.Code, check.CheckedAt, check.Game, check.Filter = report.Code, now, report.Game, report.Filter
	recheck := time.Duration(v.RecheckHours) * time.Hour
	grace := time.Duration(v.GraceHours) * time.Hour
	failing := executor.VerifyBlocks(report.Code) || (report.Code == executor.VerifyTargetDown && check.State == "pending")
	switch {
	case report.Code == executor.VerifyOK:
		resumed := check.State == "paused"
		check.State, check.PassedAt, check.FailedSince, check.Retries, check.PausedAt = "ok", now, 0, 0, 0
		check.NextAt = now + jitter(recheck)
		if resumed && rule.State == "paused" {
			relayResumeRule(rule)
			relayRefreshPolicy(s, rule)
			return []string{"relay.verify.resumed"}
		}
		return nil
	case !failing:
		// Not the user's fault: the node is down, the route or the executor
		// has a problem. Nothing counts; try again later.
		check.NextAt = now + verifyLaterEvery.Milliseconds()
		if report.Code == executor.VerifyEntryUnreachable {
			return []string{"relay.verify.route_problem"}
		}
		return nil
	}
	switch check.State {
	case "pending":
		if check.Retries < verifyCreateRetries {
			check.Retries++
			check.NextAt = now + verifyCreateRetry.Milliseconds()
			return nil
		}
		check.State, check.FailedSince, check.Retries = "failed", now, 0
		if v.Donation == "enforce" {
			check.NextAt = 0
			relayRevoke(rule, now, true)
			return []string{"relay.verify.refused"}
		}
		check.NextAt = now + verifyFailedEvery.Milliseconds()
		return []string{"relay.verify.failed"}
	case "ok", "unchecked":
		if check.FailedSince == 0 {
			check.FailedSince = now
		}
		if check.Retries < verifyQuickRetries {
			check.Retries++
			check.NextAt = now + verifyQuickRetry.Milliseconds()
			return nil
		}
		check.State, check.Retries = "failed", 0
		check.NextAt = now + verifyFailedEvery.Milliseconds()
		if v.Donation == "enforce" {
			check.NextAt = min(check.NextAt, check.FailedSince+grace.Milliseconds())
		}
		return []string{"relay.verify.failed"}
	case "failed":
		check.NextAt = now + verifyFailedEvery.Milliseconds()
		if v.Donation != "enforce" {
			return nil
		}
		if pauseAt := check.FailedSince + grace.Milliseconds(); now < pauseAt {
			check.NextAt = min(check.NextAt, pauseAt)
			return nil
		}
		check.State, check.PausedAt = "paused", now
		if rule.State == "active" || rule.State == "pending" {
			rule.State = "paused"
			relayRefreshPolicy(s, rule)
		}
		return []string{"relay.verify.paused"}
	default: // paused stays paused until a check passes
		check.NextAt = now + verifyFailedEvery.Milliseconds()
		return nil
	}
}

// relayVerifyLoop schedules rule checks every half minute, or at once when a
// user or administrator asks for one.
func (a *App) relayVerifyLoop(ctx context.Context) {
	a.relayVerify.wake()
	a.relayVerify.mu.Lock()
	kick := a.relayVerify.kick
	a.relayVerify.mu.Unlock()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-kick:
		}
		a.relayVerifyTick(ctx)
	}
}

func (a *App) relayVerifyTick(ctx context.Context) {
	value, ok := relayRouteVerifiers.Load(a)
	if !ok {
		return
	}
	verify := value.(routeVerifier)
	now := time.Now().UnixMilli()
	var due []string
	_ = a.Store.Update(func(s *State) error {
		settings := relayVerifySettings(s)
		for _, rule := range ListDocs[UserRule](s, "user_rules") {
			key := rule.UserID + ":" + rule.RouteID
			changed, events := relayVerifyMaintain(s, &rule, settings, now)
			if changed {
				if err := SaveDoc(s, "user_rules", key, rule); err != nil {
					return err
				}
				for _, event := range events {
					_ = commerceAudit(s, "system", event, rule.ID)
				}
			}
			if relayVerifyDue(s, rule, settings, now) {
				due = append(due, key)
			}
		}
		return relayVerifyPrune(s, now)
	})
	for _, key := range due {
		if !a.relayVerify.begin(key) {
			continue
		}
		go func(key string) {
			defer a.relayVerify.end(key)
			a.verifyRule(ctx, key, verify)
		}(key)
	}
}

// relayVerifyPrune drops relay-verify task records after a week; the rule
// keeps its own result, and the records hold no configuration.
func relayVerifyPrune(s *State, now int64) error {
	limit := now - 7*24*time.Hour.Milliseconds()
	for _, task := range ListDocs[Task](s, "tasks") {
		if task.Kind != "relay-verify" || task.UpdatedAt > limit || !relayTaskEnded(task.State) {
			continue
		}
		if o, ok := LoadDoc[taskOperation](s, "task_operations", task.ID); ok && o.Status != "resolved" {
			continue
		}
		DeleteDoc(s, "tasks", task.ID)
		DeleteDoc(s, "task_operations", task.ID)
	}
	return nil
}

type verifyNotice struct {
	userID, kind string
	at           int64
}

func (a *App) verifyRule(ctx context.Context, key string, verify routeVerifier) {
	var rule UserRule
	var config []byte
	var options *executor.VerifyOptions
	var settings RelayVerifySettings
	direct := false
	err := a.Store.View(func(s *State) error {
		var ok bool
		rule, ok = LoadDoc[UserRule](s, "user_rules", key)
		settings = relayVerifySettings(s)
		if !ok || !relayVerifyDue(s, rule, settings, time.Now().UnixMilli()) {
			return errors.New("not due")
		}
		options = relayVerifyOptions(s, "log")
		raw, err := a.Open(rule.SealedConfig)
		if err != nil {
			return err
		}
		config = raw
		if rule.Verify.State == "paused" {
			// The relay is stopped: check the node itself with the same account.
			target, err := relayDiagnosticTarget(rule)
			if err != nil {
				return err
			}
			host, portText, _ := net.SplitHostPort(target)
			port, _ := strconv.Atoi(portText)
			config, err = executor.RewriteClientConfig(raw, host, port)
			clear(raw)
			if err != nil {
				return err
			}
			direct = true
		}
		return nil
	})
	if err != nil || options == nil {
		clear(config)
		return
	}
	checkCtx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	report, err := verify(checkCtx, rule.UserID, config, *options)
	clear(config)
	if err != nil {
		report = executor.VerifyReport{Code: executor.VerifyUnavailable}
	}
	if report.Code == executor.VerifyTunnelFailed && !direct && a.relayExitReachesTarget(checkCtx, key) == "unreachable" {
		report.Code = executor.VerifyTargetDown
	}
	now := time.Now().UnixMilli()
	tripped, alert := a.relayVerify.breaker(now, executor.VerifyBlocks(report.Code), settings.BreakerPercent)
	if tripped {
		report = executor.VerifyReport{Code: executor.VerifyUnavailable, ElapsedMS: report.ElapsedMS}
	}
	var notices []verifyNotice
	_ = a.Store.Update(func(s *State) error {
		current, ok := LoadDoc[UserRule](s, "user_rules", key)
		if !ok || current.ID != rule.ID {
			return nil
		}
		policy := relayVerifySettings(s)
		events := applyRuleVerify(s, &current, report, policy, now)
		if err := SaveDoc(s, "user_rules", key, current); err != nil {
			return err
		}
		if alert {
			_ = commerceAudit(s, "system", "relay.verify.breaker", current.ID)
		}
		for _, event := range events {
			_ = commerceAudit(s, "system", event, current.ID)
			if kind := ruleVerifyMail(event, policy.Donation); kind != "" {
				notices = append(notices, verifyNotice{userID: current.UserID, kind: kind, at: now})
			}
		}
		return nil
	})
	for _, notice := range notices {
		a.notifyRuleVerify(notice)
	}
}

// ruleVerifyMail is the mail a verification event sends, if any. While the
// policy only records, results are shown on the route page and nobody is
// mailed about a check that may still be wrong.
func ruleVerifyMail(event, mode string) string {
	if mode != "enforce" {
		return ""
	}
	switch event {
	case "relay.verify.failed", "relay.verify.refused":
		return "relay_verify_failed"
	case "relay.verify.paused":
		return "relay_verify_paused"
	}
	return ""
}

// notifyRuleVerify mails a user whose email address is verified; everyone
// also sees the state on the route page.
func (a *App) notifyRuleVerify(notice verifyNotice) {
	var config SMTPConfig
	email := ""
	send := false
	_ = a.Store.View(func(s *State) error {
		user := s.Users[notice.userID]
		if user == nil || user.EmailVerifiedAt == 0 || !smtpReady(s) {
			return nil
		}
		var err error
		config, err = getSMTP(s)
		email, send = user.Email, err == nil
		return nil
	})
	if !send {
		return
	}
	a.queuePasswordMail(func(ctx context.Context) {
		_ = a.sendAccountEmail(ctx, config, email, notice.kind, EmailData{EventAtLabel: emailTimeLabel(notice.at)})
	})
}

// relayExitReachesTarget asks the route's first exit to open a TCP connection
// to the user's node, telling a stopped node apart from a refused handshake.
func (a *App) relayExitReachesTarget(ctx context.Context, key string) string {
	var rule UserRule
	exitAgentID := ""
	capable := false
	now := time.Now().UnixMilli()
	_ = a.Store.View(func(s *State) error {
		var ok bool
		rule, ok = LoadDoc[UserRule](s, "user_rules", key)
		route, routeOK := LoadDoc[Route](s, "routes", rule.RouteID)
		if !ok || !routeOK {
			return nil
		}
		exitIDs := route.Exit.AgentIDs
		if route.Type == "port_forward" || len(exitIDs) == 0 {
			exitIDs = []string{route.EntryAgentID}
		}
		exitAgentID = exitIDs[0]
		for _, segment := range rule.Segments {
			if segment.AgentID == exitAgentID {
				rule.Segments = []RelaySegment{segment}
				agent, found := LoadDoc[RelayAgent](s, "relay_agents", exitAgentID)
				capable = found && relayTargetProbeCapable(agent, now)
				break
			}
		}
		return nil
	})
	if !capable {
		return "unknown"
	}
	target, err := relayDiagnosticTarget(rule)
	if err != nil {
		return "unknown"
	}
	job, err := a.relayDiagnostics.begin("verify:"+rule.ID, rule.ID, exitAgentID, target)
	if err != nil {
		return "unknown"
	}
	defer a.relayDiagnostics.end(job.id)
	timer := time.NewTimer(time.Until(job.expires))
	defer timer.Stop()
	var report relayruntime.TargetProbeResult
	select {
	case report = <-job.result:
	case <-timer.C:
		return "unknown"
	case <-ctx.Done():
		return "unknown"
	}
	switch report.Status {
	case "success":
		return "reachable"
	case "unavailable":
		return "unknown"
	}
	return "unreachable"
}

// relayVerifyView adds what a page needs to explain a rule's verification.
// While verification is off, members see nothing of an earlier result.
func relayVerifyView(s *State, out *RelayRuleView, rule UserRule, admin bool) {
	v := relayVerifySettings(s)
	if rule.Verify == nil || (v.Donation == "off" && !admin) {
		return
	}
	out.VerifyMode = v.Donation
	check := *rule.Verify
	if !admin {
		check.ExemptReason = ""
	}
	if check.State == "failed" && v.Donation == "enforce" && check.FailedSince > 0 {
		out.VerifyPauseAt = check.FailedSince + int64(v.GraceHours)*time.Hour.Milliseconds()
	}
	out.Verify = &check
}

// relayVerifyNow asks for a check of one rule by its owner.
func (a *App) relayVerifyNow(w http.ResponseWriter, r *http.Request) {
	u, err := a.User(r)
	if err != nil {
		commerceError(w, 401, err)
		return
	}
	if !a.allow("relay-verify:"+u.ID, 20, time.Hour) {
		commerceError(w, 429, errors.New("校验过于频繁，请稍后再试"))
		return
	}
	key := u.ID + ":" + r.PathValue("route")
	if a.relayVerify.busy(key) {
		commerceError(w, 409, errors.New("正在校验，请稍候"))
		return
	}
	now := time.Now().UnixMilli()
	err = a.Store.Update(func(s *State) error {
		rule, ok := LoadDoc[UserRule](s, "user_rules", key)
		if !ok || rule.UserID != u.ID || rule.State == "revoking" || rule.Verify == nil {
			return errors.New("没有可校验的线路配置")
		}
		if relayVerifySettings(s).Donation == "off" {
			return errors.New("中转校验未开启")
		}
		if rule.Verify.State == "exempt" {
			return errors.New("此线路不需要校验")
		}
		if rule.Verify.CheckedAt > now-verifyManualGap.Milliseconds() {
			return errors.New("刚刚校验过，请两分钟后再试")
		}
		rule.Verify.NextAt = now
		if rule.Verify.State == "pending" {
			rule.Verify.Retries = 0
		}
		return SaveDoc(s, "user_rules", key, rule)
	})
	if err != nil {
		commerceError(w, 409, err)
		return
	}
	a.relayVerify.wake()
	WriteJSON(w, 202, map[string]any{"ok": true})
}

// relayAdminVerify lets an administrator recheck, exempt (with a reason) or
// stop exempting one rule.
func (a *App) relayAdminVerify(w http.ResponseWriter, r *http.Request) {
	u, err := a.Admin(r)
	if err != nil {
		commerceError(w, 403, err)
		return
	}
	var in struct {
		Action string `json:"action"`
		Reason string `json:"reason"`
	}
	if err := Decode(r, &in); err != nil {
		commerceError(w, 400, err)
		return
	}
	in.Reason = strings.TrimSpace(in.Reason)
	if in.Action == "exempt" && (utf8.RuneCountInString(in.Reason) < 2 || utf8.RuneCountInString(in.Reason) > 200 || strings.ContainsAny(in.Reason, "\x00\r\n")) {
		commerceError(w, 400, errors.New("豁免原因需为 2–200 个字，不能换行"))
		return
	}
	now := time.Now().UnixMilli()
	var out RelayRuleView
	err = a.Store.Update(func(s *State) error {
		key, rule, ok := relayRequestRule(s, r, u, true)
		if !ok || rule.State == "revoking" {
			return errors.New("规则不存在或已在撤销")
		}
		if rule.Verify == nil {
			rule.Verify = &RuleVerify{State: "unchecked"}
		}
		action := ""
		switch in.Action {
		case "recheck":
			if rule.Verify.State == "exempt" {
				return errors.New("请先取消豁免")
			}
			rule.Verify.NextAt, action = now, "relay.verify.recheck"
		case "exempt":
			if rule.Verify.State == "paused" && rule.State == "paused" {
				relayResumeRule(&rule)
				relayRefreshPolicy(s, &rule)
			}
			rule.Verify.State, rule.Verify.ExemptReason, rule.Verify.PausedAt, rule.Verify.NextAt = "exempt", in.Reason, 0, 0
			action = "relay.verify.exempt"
		case "unexempt":
			if rule.Verify.State != "exempt" {
				return errors.New("此规则没有豁免")
			}
			rule.Verify.State, rule.Verify.ExemptReason, rule.Verify.FailedSince, rule.Verify.Retries, rule.Verify.NextAt = "unchecked", "", 0, 0, now
			action = "relay.verify.unexempt"
		default:
			return errors.New("不支持的校验操作")
		}
		if err := SaveDoc(s, "user_rules", key, rule); err != nil {
			return err
		}
		out = relayRuleView(s, rule, true, now)
		return commerceAudit(s, u.ID, action, rule.ID)
	})
	if err != nil {
		commerceError(w, 409, err)
		return
	}
	a.relayVerify.wake()
	WriteJSON(w, 200, out)
}
