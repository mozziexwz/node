package control

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mozziexwz/node/internal/executor"
)

// verifyCapable gives the test executor the verification capability.
func (h *relayCodeHarness) verifyCapable(on bool) {
	h.t.Helper()
	_ = h.app.Store.Update(func(s *State) error {
		record, _ := LoadDoc[ExecutorRecord](s, "executors", "executor-test")
		record.Capabilities = []string{executor.FreeRelayGuardCapability, executor.RelayCodeCapability, executor.SSHKeyCapability, executor.ScopedCleanupCapability}
		if on {
			record.Capabilities = append(record.Capabilities, executor.RelayVerifyCapability)
		}
		return SaveDoc(s, "executors", record.ID, record)
	})
}

func (h *relayCodeHarness) claimVerifying() executor.Job {
	h.t.Helper()
	r := httptest.NewRequest("GET", "/api/executor/next", nil)
	r.Header.Set("Authorization", "Bearer "+h.token)
	r.Header.Set("X-MSBOOST-Executor-Capabilities", executor.FreeRelayGuardCapability)
	r.Header.Set("X-MSBOOST-Scoped-Cleanup", "1")
	r.Header.Set(executor.SSHKeyHeader, "1")
	r.Header.Set(executor.RelayCodeHeader, "1")
	r.Header.Set(executor.RelayVerifyHeader, "1")
	w := httptest.NewRecorder()
	h.service.next(w, r)
	var job executor.Job
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &job) != nil {
		h.t.Fatalf("claim: %d %s", w.Code, w.Body.String())
	}
	return job
}

func (h *relayCodeHarness) reportStatus(out executor.Result) *httptest.ResponseRecorder {
	h.t.Helper()
	r := taskRequest(h.t, h.owner, "POST", "/api/executor/result", "", out)
	r.Header.Set("Authorization", "Bearer "+h.token)
	w := httptest.NewRecorder()
	h.service.result(w, r)
	return w
}

func (h *relayCodeHarness) setVerify(change func(*RelayVerifySettings)) {
	h.t.Helper()
	_ = h.app.Store.Update(func(s *State) error {
		v := relayVerifySettings(s)
		change(&v)
		s.Settings["relayVerify"] = v
		return nil
	})
}

func TestRelayVerifySettingsStayInBounds(t *testing.T) {
	d := defaultRelayVerifySettings()
	if d.Free != "selftest" || d.Code != "log" || d.Donation != "log" || d.RecheckHours != 24 || d.GraceHours != 48 || validateRelayVerifySettings(d) != nil {
		t.Fatalf("defaults: %+v", d)
	}
	for name, change := range map[string]func(*RelayVerifySettings){
		"free mode":  func(v *RelayVerifySettings) { v.Free = "enforce" },
		"code mode":  func(v *RelayVerifySettings) { v.Code = "strict" },
		"recheck":    func(v *RelayVerifySettings) { v.RecheckHours = 0 },
		"grace":      func(v *RelayVerifySettings) { v.GraceHours = 400 },
		"breaker":    func(v *RelayVerifySettings) { v.BreakerPercent = 1 },
		"game":       func(v *RelayVerifySettings) { v.GameTarget = "192.168.1.1:80" },
		"filter":     func(v *RelayVerifySettings) { v.FilterBlocked = "not a host" },
		"no allowed": func(v *RelayVerifySettings) { v.FilterAllowed = "x" },
	} {
		v := defaultRelayVerifySettings()
		change(&v)
		if validateRelayVerifySettings(v) == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	s := &State{Settings: map[string]any{"relayVerify": map[string]any{"code": "enforce", "recheckHours": 9999}}}
	if got := relayVerifySettings(s); got != defaultRelayVerifySettings() {
		t.Fatalf("an out-of-bounds stored value was used: %+v", got)
	}
	s.Settings["relayVerify"] = map[string]any{"code": "enforce"}
	if got := relayVerifySettings(s); got.Code != "enforce" || got.RecheckHours != 24 {
		t.Fatalf("a partial stored value: %+v", got)
	}
}

func TestFreeRelaySelfTestNeedsAVerifyingExecutor(t *testing.T) {
	h := newRelayCodeHarness(t)
	create := func() executor.Job {
		request := executor.Request{Kind: "relay", SSH: taskSSHFixture(), ClientConfig: json.RawMessage(relayTestConfig)}
		if w := h.call(h.service.create, h.owner, "POST", "/api/tasks", ID(), request); w.Code != 202 {
			t.Fatalf("relay: %d %s", w.Code, w.Body.String())
		}
		return h.claimVerifying()
	}
	// No executor can verify yet: the relay is still created, unchecked.
	job := create()
	if job.Request.Verify != nil {
		t.Fatal("self-test requested from an executor that cannot run it")
	}
	h.report(executor.Result{ID: job.ID, Lease: job.Lease, State: "failed", Phase: "relay", ErrorCode: "service_failed"})
	h.verifyCapable(true)
	job = create()
	if job.Request.Verify == nil || job.Request.Verify.Mode != "log" || job.Request.Verify.GameTarget != "35.155.204.207:8585" {
		t.Fatalf("self-test options: %+v", job.Request.Verify)
	}
	config, _ := executor.RewriteClientConfig([]byte(relayTestConfig), "8.8.8.8", 34127)
	report := executor.VerifyReport{Code: executor.VerifyNotMSBOOST, ElapsedMS: 900}
	if w := h.reportStatus(executor.Result{ID: job.ID, Lease: job.Lease, State: "succeeded", Phase: "complete", Config: config, Hops: []executor.Hop{{FromHost: "8.8.8.8", FromPort: 34127, ToHost: "1.1.1.1", ToPort: 12345}}, Verify: &report}); w.Code != 200 {
		t.Fatalf("self-test result: %d %s", w.Code, w.Body.String())
	}
	h.state(func(s *State) {
		task, _ := LoadDoc[Task](s, "tasks", job.ID)
		if task.State != "succeeded" || task.Verify == nil || task.Verify.Code != executor.VerifyNotMSBOOST {
			t.Fatalf("a free relay is never refused, only shown: %+v", task)
		}
	})
	// The browser cannot ask for, or skip, the check itself.
	request := executor.Request{Kind: "relay", SSH: taskSSHFixture(), ClientConfig: json.RawMessage(relayTestConfig), Verify: &executor.VerifyOptions{Mode: "log"}}
	if w := h.call(h.service.create, h.owner, "POST", "/api/tasks", ID(), request); w.Code != 400 {
		t.Fatalf("browser-supplied check accepted: %d %s", w.Code, w.Body.String())
	}
	h.setVerify(func(v *RelayVerifySettings) { v.Free = "off" })
	if job = create(); job.Request.Verify != nil {
		t.Fatal("self-test sent while switched off")
	}
}

func TestRelayCodeVerificationIsRecordedForTheOwner(t *testing.T) {
	h := newRelayCodeHarness(t)
	code, plain := h.generate(2, true)
	// A poll declares what the executor supports; generate() polled without it.
	h.verifyCapable(true)
	if w := h.redeem(plain, nil); w.Code != 202 {
		t.Fatalf("redeem: %d %s", w.Code, w.Body.String())
	}
	job := h.claimVerifying()
	if job.Request.Verify == nil || job.Request.Verify.Mode != "log" {
		t.Fatalf("relay-code check: %+v", job.Request.Verify)
	}
	config, _ := executor.RewriteClientConfig([]byte(relayTestConfig), "8.8.8.8", 34127)
	hops := []executor.Hop{{FromHost: "8.8.8.8", FromPort: 34127, ToHost: "1.1.1.1", ToPort: 12345}}
	// Reports outside the closed vocabulary are refused.
	for _, bad := range []executor.VerifyReport{{Code: "target_down"}, {Code: "trusted"}, {Code: "ok", Game: "fast", Filter: "ok"}, {Code: "not_msboost", Game: "ok"}, {Code: "ok", Game: "ok", Filter: "ok", ElapsedMS: -1}} {
		report := bad
		if w := h.reportStatus(executor.Result{ID: job.ID, Lease: job.Lease, State: "succeeded", Phase: "complete", Config: config, Hops: hops, Verify: &report}); w.Code != 400 {
			t.Fatalf("report %+v accepted: %d", bad, w.Code)
		}
	}
	report := executor.VerifyReport{Code: executor.VerifyOK, Game: "ok", Filter: "ok", ElapsedMS: 4200}
	h.report(executor.Result{ID: job.ID, Lease: job.Lease, State: "succeeded", Phase: "complete", Config: config, Hops: hops, Verify: &report})
	h.state(func(s *State) {
		use, _ := LoadDoc[RelayCodeUse](s, relayCodeUsesCollection, job.ID)
		if use.Result != "succeeded" || use.Verify == nil || use.Verify.Code != executor.VerifyOK || use.VerifiedAt == 0 {
			t.Fatalf("use record: %+v", use)
		}
		rows := relayCodeUses(s, code.ID)
		if len(rows) != 1 || rows[0].Verify == nil || rows[0].Verify.Code != "ok" {
			t.Fatalf("owner's view: %+v", rows)
		}
	})
	// Without a verifying executor a log-only use is not held back, and a
	// report the job did not ask for is refused.
	h.verifyCapable(false)
	if w := h.redeem(plain, nil); w.Code != 202 {
		t.Fatalf("second redeem: %d %s", w.Code, w.Body.String())
	}
	second := h.claim()
	if second.Request.Verify != nil {
		t.Fatal("a log-only check was required of an old executor")
	}
	if w := h.reportStatus(executor.Result{ID: second.ID, Lease: second.Lease, State: "succeeded", Phase: "complete", Config: config, Hops: hops, Verify: &report}); w.Code != 400 {
		t.Fatalf("unrequested report accepted: %d", w.Code)
	}
}

func TestEnforcedRelayCodeRefusesTargetsThatAreNotMSBOOST(t *testing.T) {
	h := newRelayCodeHarness(t)
	h.setVerify(func(v *RelayVerifySettings) { v.Code = "enforce" })
	code, plain := h.generate(2, true)
	// Enforcing never falls back to an unchecked relay.
	if w := h.redeem(plain, nil); w.Code == 202 || !strings.Contains(w.Body.String(), "升级") {
		t.Fatalf("enforced check without a verifying executor: %d %s", w.Code, w.Body.String())
	}
	h.verifyCapable(true)
	if w := h.redeem(plain, nil); w.Code != 202 {
		t.Fatalf("redeem: %d %s", w.Code, w.Body.String())
	}
	job := h.claimVerifying()
	if job.Request.Verify == nil || job.Request.Verify.Mode != "enforce" {
		t.Fatalf("enforced options: %+v", job.Request.Verify)
	}
	report := executor.VerifyReport{Code: executor.VerifyNotMSBOOST, ElapsedMS: 3000}
	h.report(executor.Result{ID: job.ID, Lease: job.Lease, State: "failed", Phase: "verify", ErrorCode: "verify_not_msboost", FailedRole: "target", Verify: &report})
	h.state(func(s *State) {
		current, _ := LoadDoc[RelayCode](s, relayCodesCollection, code.ID)
		if current.UsedCount != 0 || current.Reserved != 0 {
			t.Fatalf("a refused use was counted: %+v", current)
		}
		task, _ := LoadDoc[Task](s, "tasks", job.ID)
		if task.State != "failed" || !strings.Contains(task.Message, "不是 MSBOOST 节点") || !strings.Contains(task.NextStep, "重新部署MSBOOST节点") || !strings.Contains(task.NextStep, "没有计次") {
			t.Fatalf("refusal guidance: %+v", task)
		}
		use, _ := LoadDoc[RelayCodeUse](s, relayCodeUsesCollection, job.ID)
		if use.Result != "failed" || use.Verify == nil || use.Verify.Code != executor.VerifyNotMSBOOST {
			t.Fatalf("use record: %+v", use)
		}
	})
}
