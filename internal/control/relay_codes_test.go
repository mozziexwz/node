package control

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mozziexwz/node/internal/executor"
)

const relayCodeOwnerPassword = "ONE_TIME_SSH_DO_NOT_PERSIST"

func testPrivateKey(t *testing.T) string {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(private, "msboost-test")
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(block))
}

type relayCodeHarness struct {
	t       *testing.T
	app     *App
	service *TaskService
	owner   *User
	friend  *User
	token   string
}

func newRelayCodeHarness(t *testing.T) *relayCodeHarness {
	t.Helper()
	a, service, owner := taskFixture(t)
	friend := &User{ID: "relay-code-friend", Email: "20000002@qq.com", Role: "user", Status: "active"}
	token := strings.Repeat("r", 48)
	if err := a.Store.Update(func(s *State) error {
		s.Users[friend.ID] = friend
		record, _ := LoadDoc[ExecutorRecord](s, "executors", "executor-test")
		record.TokenHash = taskHash([]byte(token))
		record.Capabilities = []string{executor.FreeRelayGuardCapability, executor.RelayCodeCapability, executor.SSHKeyCapability, executor.ScopedCleanupCapability}
		return SaveDoc(s, "executors", record.ID, record)
	}); err != nil {
		t.Fatal(err)
	}
	return &relayCodeHarness{t: t, app: a, service: service, owner: owner, friend: friend, token: token}
}

func (h *relayCodeHarness) call(handler http.HandlerFunc, u *User, method, path, key string, body any, pathValues ...string) *httptest.ResponseRecorder {
	h.t.Helper()
	r := taskRequest(h.t, u, method, path, key, body)
	for i := 0; i+1 < len(pathValues); i += 2 {
		r.SetPathValue(pathValues[i], pathValues[i+1])
	}
	w := httptest.NewRecorder()
	handler(w, r)
	return w
}

func (h *relayCodeHarness) claim() executor.Job {
	h.t.Helper()
	r := httptest.NewRequest("GET", "/api/executor/next", nil)
	r.Header.Set("Authorization", "Bearer "+h.token)
	r.Header.Set("X-MSBOOST-Executor-Capabilities", executor.FreeRelayGuardCapability)
	r.Header.Set("X-MSBOOST-Scoped-Cleanup", "1")
	r.Header.Set(executor.SSHKeyHeader, "1")
	r.Header.Set(executor.RelayCodeHeader, "1")
	w := httptest.NewRecorder()
	h.service.next(w, r)
	var job executor.Job
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &job) != nil {
		h.t.Fatalf("claim: %d %s", w.Code, w.Body.String())
	}
	return job
}

func (h *relayCodeHarness) report(out executor.Result) {
	h.t.Helper()
	r := taskRequest(h.t, h.owner, "POST", "/api/executor/result", "", out)
	r.Header.Set("Authorization", "Bearer "+h.token)
	w := httptest.NewRecorder()
	h.service.result(w, r)
	if w.Code != 200 {
		h.t.Fatalf("result: %d %s", w.Code, w.Body.String())
	}
}

func relayCodeInput(maxUses int) map[string]any {
	return map[string]any{"ssh": taskSSHFixture(), "maxUses": maxUses, "ttlHours": 24, "rateMbps": 7, "note": "给小明", "acknowledge": true}
}

// generate creates a code and completes its server check with the given result.
func (h *relayCodeHarness) generate(maxUses int, checkOK bool) (RelayCode, string) {
	h.t.Helper()
	w := h.call(h.service.createRelayCode, h.owner, "POST", "/api/relay-codes", ID(), relayCodeInput(maxUses))
	if w.Code != 202 {
		h.t.Fatalf("generate: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		Code relayCodeView `json:"code"`
		Task Task          `json:"task"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	job := h.claim()
	if job.Request.Kind != "relay-check" || job.ID != created.Task.ID || job.Request.SSH.Password != relayCodeOwnerPassword {
		h.t.Fatalf("server check job: %+v", job.Request)
	}
	out := executor.Result{ID: job.ID, Lease: job.Lease, State: "succeeded", Phase: "complete", OS: "Debian 12"}
	if !checkOK {
		out = executor.Result{ID: job.ID, Lease: job.Lease, State: "failed", Phase: "ssh_auth", ErrorCode: "ssh_auth"}
	}
	h.report(out)
	var code RelayCode
	_ = h.app.Store.View(func(s *State) error {
		code, _ = LoadDoc[RelayCode](s, relayCodesCollection, created.Code.ID)
		return nil
	})
	plain := ""
	if code.ID != "" {
		plain = h.service.relayCodeString(code)
	}
	return code, plain
}

func (h *relayCodeHarness) redeem(code string, front *executor.SSH) *httptest.ResponseRecorder {
	h.t.Helper()
	body := map[string]any{"code": strings.ToLower(code), "clientConfig": json.RawMessage(relayTestConfig), "remark": "朋友的中转", "consent": true}
	if front != nil {
		body["front"] = front
	}
	return h.call(h.service.redeemRelayCode, h.friend, "POST", "/api/relay-codes/redeem", ID(), body)
}

func (h *relayCodeHarness) state(fn func(*State)) {
	h.t.Helper()
	if err := h.app.Store.View(func(s *State) error { fn(s); return nil }); err != nil {
		h.t.Fatal(err)
	}
}

func TestRelayCodeGenerationRulesAndCheck(t *testing.T) {
	h := newRelayCodeHarness(t)
	for name, mutate := range map[string]func(map[string]any){
		"uses below range": func(in map[string]any) { in["maxUses"] = 0 },
		"uses above range": func(in map[string]any) { in["maxUses"] = 11 },
		"ttl below range":  func(in map[string]any) { in["ttlHours"] = 0 },
		"ttl above range":  func(in map[string]any) { in["ttlHours"] = 73 },
		"rate below range": func(in map[string]any) { in["rateMbps"] = 0 },
		"rate above range": func(in map[string]any) { in["rateMbps"] = 11 },
		"no consent":       func(in map[string]any) { in["acknowledge"] = false },
	} {
		t.Run(name, func(t *testing.T) {
			in := relayCodeInput(1)
			mutate(in)
			if w := h.call(h.service.createRelayCode, h.owner, "POST", "/api/relay-codes", ID(), in); w.Code < 400 {
				t.Fatalf("accepted %s: %d %s", name, w.Code, w.Body.String())
			}
		})
	}
	// A failed check stores no SSH and leaves no code behind.
	failed, _ := h.generate(1, false)
	if failed.ID != "" {
		t.Fatal("failed check left a code")
	}
	h.state(func(s *State) {
		if len(s.Docs[relayCodeSecretsCollection]) != 0 || len(s.Docs[relayCodesCollection]) != 0 {
			t.Fatal("failed check stored SSH or a code")
		}
	})
	code, plain := h.generate(2, true)
	if code.State != "active" || code.OS != "Debian 12" || code.RateMbps != 7 || len(plain) != 18 || !strings.HasPrefix(plain, "MSB-") {
		t.Fatalf("code not active after check: %+v %q", code, plain)
	}
	h.state(func(s *State) {
		secret, ok := LoadDoc[relayCodeSecret](s, relayCodeSecretsCollection, code.ID)
		if !ok || strings.Contains(secret.SealedSSH, relayCodeOwnerPassword) || secret.ExpiresAt != code.ExpiresAt {
			t.Fatal("SSH not sealed with the code's expiry")
		}
	})
	// The check counted against the shared relay quota.
	h.state(func(s *State) {
		if toolLimit(s, "relay", h.owner.ID, time.Now().UnixMilli()).Remaining != 3 {
			t.Fatal("server checks must count toward the relay quota")
		}
	})
}

func TestRelayCodeActiveLimit(t *testing.T) {
	h := newRelayCodeHarness(t)
	_ = h.app.Store.Update(func(s *State) error {
		s.Settings["limits"] = map[string]any{"relay": map[string]any{"minutes": 15, "count": 100}}
		s.Settings["relayCodes"] = RelayCodeSettings{Enabled: true, ManageWithSecret: true, MaxTTLHours: 72, MaxUses: 10, MaxActive: 2, DefaultRateMbps: 5, RateMinMbps: 1, RateMaxMbps: 10, RedeemPerAccount: 10, RedeemPerIP: 30, ManageMinutes: 15, ManageCount: 20}
		return nil
	})
	h.generate(1, true)
	h.generate(1, true)
	w := h.call(h.service.createRelayCode, h.owner, "POST", "/api/relay-codes", ID(), relayCodeInput(1))
	if w.Code != 409 || !strings.Contains(w.Body.String(), "最多 2 个") {
		t.Fatalf("active limit not enforced: %d %s", w.Code, w.Body.String())
	}
}

func TestRelayCodeRedeemCountsReservesAndExhausts(t *testing.T) {
	h := newRelayCodeHarness(t)
	code, plain := h.generate(1, true)
	preview := h.call(h.service.previewRelayCode, h.friend, "POST", "/api/relay-codes/preview", "", map[string]string{"code": " " + strings.ToLower(plain) + " "})
	if preview.Code != 200 || !strings.Contains(preview.Body.String(), `"serverMasked":"8.*.*.8"`) || !strings.Contains(preview.Body.String(), `"ownerMasked":"1000****@qq.com"`) || strings.Contains(preview.Body.String(), "8.8.8.8") {
		t.Fatalf("preview leaked or wrong: %d %s", preview.Code, preview.Body.String())
	}
	w := h.redeem(plain, nil)
	if w.Code != 202 || strings.Contains(w.Body.String(), "8.8.8.8") || strings.Contains(w.Body.String(), relayCodeOwnerPassword) {
		t.Fatalf("redeem: %d %s", w.Code, w.Body.String())
	}
	// The only use is reserved while the first relay is being built.
	if second := h.redeem(plain, nil); second.Code != 410 {
		t.Fatalf("reserved use was handed out twice: %d %s", second.Code, second.Body.String())
	}
	job := h.claim()
	request := job.Request
	if request.Kind != "relay-code" || request.SSH.Password != relayCodeOwnerPassword || request.SSH.Fingerprint != code.Fingerprint || request.RateMbps != 7 || request.Meta == nil || request.Meta.Source != "code" || request.Meta.CodeTail != code.Tail || request.Meta.User != h.friend.Email || request.Meta.Remark != "给小明" || request.Remark != "朋友的中转" {
		t.Fatalf("relay-code job: %+v %+v", request, request.Meta)
	}
	config, _ := executor.RewriteClientConfig([]byte(relayTestConfig), "8.8.8.8", 34127)
	h.report(executor.Result{ID: job.ID, Lease: job.Lease, State: "succeeded", Phase: "complete", Config: config, Hops: []executor.Hop{{FromHost: "8.8.8.8", FromPort: 34127, ToHost: "1.1.1.1", ToPort: 12345}}})
	h.state(func(s *State) {
		current, _ := LoadDoc[RelayCode](s, relayCodesCollection, code.ID)
		if current.UsedCount != 1 || current.Reserved != 0 || current.State != "exhausted" || current.DeleteReason != "exhausted" {
			t.Fatalf("use not counted or code not exhausted: %+v", current)
		}
		if _, held := s.Docs[relayCodeSecretsCollection][code.ID]; held {
			t.Fatal("SSH kept after the last use")
		}
		use, _ := LoadDoc[RelayCodeUse](s, relayCodeUsesCollection, job.ID)
		if use.Result != "succeeded" || use.ListenPort != 34127 || use.UserEmail != h.friend.Email {
			t.Fatalf("use record: %+v", use)
		}
	})
	used := h.call(h.service.usedRelayCodes, h.friend, "GET", "/api/relay-codes/used", "", nil)
	if !strings.Contains(used.Body.String(), `"entryPort":34127`) || strings.Contains(used.Body.String(), `"8.8.8.8"`) {
		t.Fatalf("used list: %s", used.Body.String())
	}
	if after := h.redeem(plain, nil); after.Code != 410 {
		t.Fatalf("exhausted code accepted: %d %s", after.Code, after.Body.String())
	}
}

func TestRelayCodeFailedUseIsNotCounted(t *testing.T) {
	h := newRelayCodeHarness(t)
	code, plain := h.generate(2, true)
	front := executor.SSH{Host: "9.9.9.9", Port: 22, User: "root", Password: "front-secret", Fingerprint: "SHA256:" + strings.Repeat("F", 43)}
	h.service.probes[taskProbeKey(h.friend.ID, front)] = taskProbe{Fingerprint: front.Fingerprint, Expires: time.Now().Add(time.Hour)}
	if w := h.redeem(plain, &front); w.Code != 202 {
		t.Fatalf("redeem with front: %d %s", w.Code, w.Body.String())
	}
	job := h.claim()
	if job.Request.Front == nil || job.Request.Front.Password != "front-secret" {
		t.Fatal("front SSH missing from the job")
	}
	// The executor removed the owner-side relay when the front failed.
	h.report(executor.Result{ID: job.ID, Lease: job.Lease, State: "failed", Phase: "ssh_auth", ErrorCode: "ssh_auth", FailedRole: "front"})
	h.state(func(s *State) {
		current, _ := LoadDoc[RelayCode](s, relayCodesCollection, code.ID)
		if current.UsedCount != 0 || current.Reserved != 0 || current.State != "active" {
			t.Fatalf("failed use counted: %+v", current)
		}
		task, _ := LoadDoc[Task](s, "tasks", job.ID)
		if !strings.Contains(task.NextStep, "前置机") || !strings.Contains(task.NextStep, "没有计次") {
			t.Fatalf("user guidance: %q", task.NextStep)
		}
	})
	// An interrupted use is released by the deletion check.
	if w := h.redeem(plain, nil); w.Code != 202 {
		t.Fatalf("second redeem: %d %s", w.Code, w.Body.String())
	}
	second := h.claim()
	_ = h.app.Store.Update(func(s *State) error {
		task, _ := LoadDoc[Task](s, "tasks", second.ID)
		task.State = "interrupted"
		return SaveDoc(s, "tasks", task.ID, task)
	})
	h.service.sweepRelayCodes()
	h.state(func(s *State) {
		current, _ := LoadDoc[RelayCode](s, relayCodesCollection, code.ID)
		if current.Reserved != 0 || current.UsedCount != 0 {
			t.Fatalf("interrupted use kept its reservation: %+v", current)
		}
	})
}

func TestRelayCodeSecretDeletionTriggers(t *testing.T) {
	cases := map[string]struct {
		act    func(h *relayCodeHarness, code RelayCode)
		reason string
	}{
		"expiry": {func(h *relayCodeHarness, code RelayCode) {
			_ = h.app.Store.Update(func(s *State) error {
				code.ExpiresAt = time.Now().Add(-time.Minute).UnixMilli()
				return SaveDoc(s, relayCodesCollection, code.ID, code)
			})
			h.service.sweepRelayCodes()
		}, "expired"},
		"owner revocation": {func(h *relayCodeHarness, code RelayCode) {
			if w := h.call(h.service.revokeRelayCode, h.owner, "POST", "/api/relay-codes/"+code.ID+"/revoke", "", map[string]bool{"deleteRelays": false}, "id", code.ID); w.Code != 200 {
				h.t.Fatal(w.Body.String())
			}
		}, "revoked"},
		"owner suspended": {func(h *relayCodeHarness, code RelayCode) {
			_ = h.app.Store.Update(func(s *State) error { s.Users[h.owner.ID].Status = "disabled"; return nil })
			h.service.sweepRelayCodes()
		}, "owner_disabled"},
		"owner suspended by an administrator": {func(h *relayCodeHarness, code RelayCode) {
			// Immediate: no deletion check runs in this case.
			admin := &User{ID: "relay-admin", Role: "admin", Status: "active", Email: "12345678@qq.com"}
			w := h.call(h.app.adminUser, admin, "PATCH", "/api/admin/users/"+h.owner.ID, "", map[string]string{"status": "suspended"}, "id", h.owner.ID)
			if w.Code != 200 {
				h.t.Fatal(w.Body.String())
			}
		}, "owner_disabled"},
		"feature disabled for owner": {func(h *relayCodeHarness, code RelayCode) {
			admin := &User{ID: "relay-admin", Role: "admin", Status: "active", Email: "12345678@qq.com"}
			if w := h.call(h.service.adminRelayCodeFeature, admin, "POST", "/api/admin/relay-codes/users/"+h.owner.ID+"/feature", "", map[string]any{"enabled": false, "reason": "公开出售中转码"}, "id", h.owner.ID); w.Code != 200 {
				h.t.Fatal(w.Body.String())
			}
		}, "admin_disabled"},
		"emergency purge": {func(h *relayCodeHarness, code RelayCode) {
			admin := &User{ID: "relay-admin", Role: "admin", Status: "active", Email: "12345678@qq.com"}
			if w := h.call(h.service.adminPurgeRelayCodeSecrets, admin, "POST", "/api/admin/relay-codes/purge", "", map[string]string{"confirm": "DELETE_ALL_RELAY_CODE_SSH"}); w.Code != 200 {
				h.t.Fatal(w.Body.String())
			}
		}, "admin_purge"},
		"restore": {func(h *relayCodeHarness, code RelayCode) {
			_ = h.app.Store.Update(func(s *State) error { return isolateRestoredState(s, false) })
		}, "restore"},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			h := newRelayCodeHarness(t)
			code, plain := h.generate(3, true)
			test.act(h, code)
			h.state(func(s *State) {
				current, _ := LoadDoc[RelayCode](s, relayCodesCollection, code.ID)
				if _, held := s.Docs[relayCodeSecretsCollection][code.ID]; held || current.DeleteReason != test.reason || current.SecretDeletedAt == 0 || usableRelayCode(current) {
					t.Fatalf("SSH not deleted for %s: %+v", name, current)
				}
				found := false
				for _, event := range ListDocs[relayCodeEvent](s, relayCodeAuditCollection) {
					raw, _ := json.Marshal(event)
					if strings.Contains(string(raw), relayCodeOwnerPassword) {
						t.Fatal("SSH in audit")
					}
					found = found || (event.Kind == "secret_deleted" && event.CodeID == code.ID && event.Reason == test.reason)
				}
				if !found {
					t.Fatal("deletion not audited")
				}
			})
			if !strings.HasPrefix(name, "owner suspended") && name != "feature disabled for owner" && name != "restore" {
				if w := h.redeem(plain, nil); w.Code != 410 {
					t.Fatalf("ended code usable: %d %s", w.Code, w.Body.String())
				}
			}
		})
	}
}

func TestRelayCodeRevokeCanDeleteBuiltRelays(t *testing.T) {
	h := newRelayCodeHarness(t)
	code, plain := h.generate(3, true)
	if w := h.redeem(plain, nil); w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	job := h.claim()
	config, _ := executor.RewriteClientConfig([]byte(relayTestConfig), "8.8.8.8", 34127)
	h.report(executor.Result{ID: job.ID, Lease: job.Lease, State: "succeeded", Phase: "complete", Config: config, Hops: []executor.Hop{{FromHost: "8.8.8.8", FromPort: 34127, ToHost: "1.1.1.1", ToPort: 12345}}})
	w := h.call(h.service.revokeRelayCode, h.owner, "POST", "/api/relay-codes/"+code.ID+"/revoke", "", map[string]bool{"deleteRelays": true}, "id", code.ID)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"items":1`) {
		t.Fatalf("revoke: %d %s", w.Code, w.Body.String())
	}
	// SSH is gone from storage at once; only the delete job carries it.
	h.state(func(s *State) {
		if _, held := s.Docs[relayCodeSecretsCollection][code.ID]; held {
			t.Fatal("SSH kept while deleting relays")
		}
	})
	manage := h.claim()
	if manage.Request.Kind != "relay-manage" || manage.Request.Manage.Action != "delete" || len(manage.Request.Manage.Items) != 1 || manage.Request.Manage.Items[0] != job.ID+"r" || manage.Request.SSH.Password != relayCodeOwnerPassword {
		t.Fatalf("delete job: %+v", manage.Request)
	}
	h.report(executor.Result{ID: manage.ID, Lease: manage.Lease, State: "succeeded", Phase: "complete", Managed: []executor.RelayManageResult{{ID: job.ID + "r", OK: true}}})
	result := h.call(h.service.relayServerResult, h.owner, "GET", "/api/relay-servers/results/"+manage.ID, "", nil, "id", manage.ID)
	if result.Code != 200 || !strings.Contains(result.Body.String(), `"ok":true`) {
		t.Fatalf("manage result: %d %s", result.Code, result.Body.String())
	}
	if other := h.call(h.service.relayServerResult, h.friend, "GET", "/api/relay-servers/results/"+manage.ID, "", nil, "id", manage.ID); other.Code != 404 {
		t.Fatal("another user read the owner's relay results")
	}
}

func TestRelayCodeOwnershipAndSecretsNeverReturned(t *testing.T) {
	h := newRelayCodeHarness(t)
	code, plain := h.generate(2, true)
	responses := []*httptest.ResponseRecorder{
		h.call(h.service.listRelayCodes, h.owner, "GET", "/api/relay-codes", "", nil),
		h.call(h.service.relayCodeDetail, h.owner, "GET", "/api/relay-codes/"+code.ID, "", nil, "id", code.ID),
		h.call(h.service.list, h.owner, "GET", "/api/tasks", "", nil),
	}
	for _, w := range responses {
		if w.Code != 200 || strings.Contains(w.Body.String(), relayCodeOwnerPassword) || strings.Contains(w.Body.String(), "sealed") {
			t.Fatalf("owner response leaked SSH: %d %s", w.Code, w.Body.String())
		}
	}
	if !strings.Contains(responses[0].Body.String(), plain) {
		t.Fatal("owner cannot copy the code again")
	}
	for _, w := range []*httptest.ResponseRecorder{
		h.call(h.service.relayCodeDetail, h.friend, "GET", "/api/relay-codes/"+code.ID, "", nil, "id", code.ID),
		h.call(h.service.adjustRelayCode, h.friend, "PATCH", "/api/relay-codes/"+code.ID, "", map[string]any{"note": "x"}, "id", code.ID),
		h.call(h.service.revokeRelayCode, h.friend, "POST", "/api/relay-codes/"+code.ID+"/revoke", "", map[string]bool{}, "id", code.ID),
		h.call(h.service.relayServerInventory, h.friend, "POST", "/api/relay-servers/inventory", "", map[string]string{"codeId": code.ID}),
	} {
		if w.Code < 400 {
			t.Fatalf("another user managed the owner's code: %d %s", w.Code, w.Body.String())
		}
	}
	// Nothing persisted outside the sealed secret contains the SSH.
	h.state(func(s *State) {
		for name, docs := range s.Docs {
			for _, raw := range docs {
				if strings.Contains(string(raw), relayCodeOwnerPassword) {
					t.Fatalf("SSH persisted in plain text in %s", name)
				}
			}
		}
	})
	// Backups and exports never contain the stored SSH.
	backups := NewBackupService(h.app)
	var packed []byte
	h.state(func(s *State) {
		var err error
		if packed, err = backups.pack(s); err != nil {
			t.Fatal(err)
		}
	})
	restored, err := backups.unpack(packed)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored.Docs[relayCodeSecretsCollection]) != 0 || len(restored.Docs[relayCodesCollection]) != 1 {
		t.Fatal("backup kept relay-code SSH")
	}
	// The live state still has it until the code ends.
	h.state(func(s *State) {
		if len(s.Docs[relayCodeSecretsCollection]) != 1 {
			t.Fatal("pack modified the live state")
		}
	})
}

func TestRelayCodeAdjustRules(t *testing.T) {
	h := newRelayCodeHarness(t)
	code, _ := h.generate(3, true)
	patch := func(body map[string]any) *httptest.ResponseRecorder {
		return h.call(h.service.adjustRelayCode, h.owner, "PATCH", "/api/relay-codes/"+code.ID, "", body, "id", code.ID)
	}
	if w := patch(map[string]any{"maxUses": 2}); w.Code != 409 {
		t.Fatalf("uses decreased: %d %s", w.Code, w.Body.String())
	}
	if w := patch(map[string]any{"maxUses": 11}); w.Code != 409 {
		t.Fatalf("uses above 10: %d %s", w.Code, w.Body.String())
	}
	if w := patch(map[string]any{"maxUses": 5, "extendHours": 72, "note": "公会群"}); w.Code != 200 {
		t.Fatalf("adjust: %d %s", w.Code, w.Body.String())
	}
	h.state(func(s *State) {
		current, _ := LoadDoc[RelayCode](s, relayCodesCollection, code.ID)
		secret, _ := LoadDoc[relayCodeSecret](s, relayCodeSecretsCollection, code.ID)
		limit := current.CreatedAt + 72*int64(time.Hour/time.Millisecond)
		if current.MaxUses != 5 || current.Note != "公会群" || current.ExpiresAt != limit || secret.ExpiresAt != limit {
			t.Fatalf("adjusted code: %+v", current)
		}
	})
	if w := patch(map[string]any{"extendHours": 1}); w.Code != 409 {
		t.Fatalf("extended past 72 hours: %d %s", w.Code, w.Body.String())
	}
}

func TestRelayCodeAttemptLimit(t *testing.T) {
	h := newRelayCodeHarness(t)
	for i := 0; i < 10; i++ {
		h.call(h.service.previewRelayCode, h.friend, "POST", "/api/relay-codes/preview", "", map[string]string{"code": "MSB-2222-2222-2222"})
	}
	w := h.call(h.service.previewRelayCode, h.friend, "POST", "/api/relay-codes/preview", "", map[string]string{"code": "MSB-2222-2222-2222"})
	if w.Code != 429 || !strings.Contains(w.Body.String(), "每小时最多 10 次") {
		t.Fatalf("attempt limit: %d %s", w.Code, w.Body.String())
	}
}

func TestRelayServerManageQuotaIsSeparate(t *testing.T) {
	h := newRelayCodeHarness(t)
	code, _ := h.generate(1, true)
	h.state(func(s *State) {
		if toolLimit(s, "relay-manage", h.owner.ID, time.Now().UnixMilli()).Count != 20 {
			t.Fatal("default relay server quota must be 20 per 15 minutes")
		}
	})
	w := h.call(h.service.relayServerInventory, h.owner, "POST", "/api/relay-servers/inventory", "", map[string]string{"codeId": code.ID})
	if w.Code != 202 {
		t.Fatalf("inventory: %d %s", w.Code, w.Body.String())
	}
	job := h.claim()
	if job.Request.Kind != "relay-inventory" || job.Request.SSH.Password != relayCodeOwnerPassword {
		t.Fatalf("inventory job: %+v", job.Request)
	}
	h.report(executor.Result{ID: job.ID, Lease: job.Lease, State: "succeeded", Phase: "complete", OS: "Debian 12", Inventory: []executor.RelayEntry{
		{ID: "abcr", Role: "relay", Port: 41872, RateMbps: 8, Target: "1.1.1.1:12345", State: "running", Connections: 3, CreatedAt: 1, Record: &executor.RelayRecord{Version: 1, Source: "code", CodeTail: code.Tail, User: "55661234@qq.com", Target: "203.0.113.45:23456", Port: 41872, RateMbps: 8, Remark: "公会群", CreatedAt: "2026-10-16T14:05:00Z"}},
		{ID: "oldr", Role: "relay", Port: 52210, RateMbps: 5, Target: "192.0.2.88:30000", State: "stopped", CreatedAt: 2},
	}})
	result := h.call(h.service.relayServerResult, h.owner, "GET", "/api/relay-servers/results/"+job.ID, "", nil, "id", job.ID)
	body := result.Body.String()
	if result.Code != 200 || !strings.Contains(body, `"user":"55661234@qq.com"`) || !strings.Contains(body, `"maskedIp":"203.*.*.45"`) || !strings.Contains(body, `"source":"legacy"`) || strings.Contains(body, "203.0.113.45") || strings.Contains(body, "192.0.2.88") {
		t.Fatalf("inventory rows: %d %s", result.Code, body)
	}
	h.state(func(s *State) {
		if toolLimit(s, "relay-manage", h.owner.ID, time.Now().UnixMilli()).Remaining != 19 || toolLimit(s, "relay", h.owner.ID, time.Now().UnixMilli()).Remaining != 4 {
			t.Fatal("relay server reads must use their own quota")
		}
	})
}

func TestRelayCodeManualRelayBandwidthRange(t *testing.T) {
	h := newRelayCodeHarness(t)
	for rate, want := range map[int]int{0: 202, 1: 202, 10: 202, 11: 400} {
		request := executor.Request{Kind: "relay", SSH: taskSSHFixture(), ClientConfig: json.RawMessage(relayTestConfig), RateMbps: rate}
		w := h.call(h.service.create, h.owner, "POST", "/api/tasks", ID(), request)
		if w.Code != want {
			t.Fatalf("rate %d: %d %s", rate, w.Code, w.Body.String())
		}
		if w.Code == 202 {
			job := h.claim()
			expected := rate
			if rate == 0 {
				expected = 5
			}
			if job.Request.RateMbps != expected || job.Request.Meta.Source != "manual" {
				t.Fatalf("rate %d job: %+v", rate, job.Request)
			}
			h.report(executor.Result{ID: job.ID, Lease: job.Lease, State: "failed", Phase: "relay", ErrorCode: "service_failed"})
		}
	}
	// The browser cannot supply the user record itself.
	request := executor.Request{Kind: "relay", SSH: taskSSHFixture(), ClientConfig: json.RawMessage(relayTestConfig), Meta: &executor.RelayMeta{Source: "manual", User: "someone@qq.com"}}
	if w := h.call(h.service.create, h.owner, "POST", "/api/tasks", ID(), request); w.Code != 400 {
		t.Fatalf("browser-supplied user record accepted: %d %s", w.Code, w.Body.String())
	}
	for _, kind := range []string{"relay-code", "relay-check", "relay-inventory", "relay-manage"} {
		request := executor.Request{Kind: kind, SSH: taskSSHFixture()}
		if w := h.call(h.service.create, h.owner, "POST", "/api/tasks", ID(), request); w.Code != 400 {
			t.Fatalf("%s accepted through the generic task endpoint: %d", kind, w.Code)
		}
	}
}

func TestRelayCodeKeyTasksNeedKeyCapableExecutor(t *testing.T) {
	h := newRelayCodeHarness(t)
	_ = h.app.Store.Update(func(s *State) error {
		record, _ := LoadDoc[ExecutorRecord](s, "executors", "executor-test")
		record.Capabilities = []string{executor.FreeRelayGuardCapability, executor.RelayCodeCapability}
		return SaveDoc(s, "executors", record.ID, record)
	})
	ssh := taskSSHFixture()
	ssh.AuthMethod, ssh.Password, ssh.PrivateKey = "key", "", testPrivateKey(t)
	h.service.probes[taskProbeKey(h.owner.ID, ssh)] = taskProbe{Fingerprint: ssh.Fingerprint, Expires: time.Now().Add(time.Hour)}
	w := h.call(h.service.create, h.owner, "POST", "/api/tasks", ID(), executor.Request{Kind: "deploy", Mode: "fresh", SSH: ssh})
	if w.Code != 409 || !strings.Contains(w.Body.String(), "密钥") {
		t.Fatalf("key task accepted without a key-capable executor: %d %s", w.Code, w.Body.String())
	}
	_ = h.app.Store.Update(func(s *State) error {
		record, _ := LoadDoc[ExecutorRecord](s, "executors", "executor-test")
		record.Capabilities = append(record.Capabilities, executor.SSHKeyCapability)
		return SaveDoc(s, "executors", record.ID, record)
	})
	if w = h.call(h.service.create, h.owner, "POST", "/api/tasks", ID(), executor.Request{Kind: "deploy", Mode: "fresh", SSH: ssh}); w.Code != 202 {
		t.Fatalf("key task: %d %s", w.Code, w.Body.String())
	}
	var created Task
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	// An executor that did not declare key support never receives the job.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	poll := httptest.NewRequest("GET", "/api/executor/next", nil).WithContext(ctx)
	poll.Header.Set("Authorization", "Bearer "+h.token)
	poll.Header.Set("X-MSBOOST-Executor-Capabilities", executor.FreeRelayGuardCapability)
	poll.Header.Set(executor.RelayCodeHeader, "1")
	recorder := httptest.NewRecorder()
	h.service.next(recorder, poll)
	h.service.mu.Lock()
	claimed := h.service.envelopes[created.ID].AgentID != ""
	h.service.mu.Unlock()
	if claimed || strings.Contains(recorder.Body.String(), created.ID) {
		t.Fatal("key job delivered to an executor without key support")
	}
	job := h.claim()
	if job.Request.SSH.PrivateKey == "" {
		t.Fatal("key missing from the delivered job")
	}
	h.state(func(s *State) {
		for name, docs := range s.Docs {
			for _, raw := range docs {
				if strings.Contains(string(raw), "PRIVATE KEY") {
					t.Fatalf("private key persisted in %s", name)
				}
			}
		}
	})
	h.service.mu.Lock()
	stored := h.service.envelopes[job.ID].Job.Request.SSH
	h.service.mu.Unlock()
	if stored.PrivateKey != "" || stored.Password != "" {
		t.Fatal("private key kept in memory after delivery")
	}
}
