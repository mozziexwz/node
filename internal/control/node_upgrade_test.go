package control

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mozziexwz/node/internal/executor"
)

const nodeTestConfig = `{"profiles":[{"user":{"name":"msboost-node","password":"node-password"},"servers":[{"ipAddress":"8.8.8.8","portBindings":[{"port":23456,"protocol":"TCP"}]}]}],"socks5Port":10086}`

func TestNodeUpgradeNotesAreValidatedAndPublic(t *testing.T) {
	for name, bad := range map[string]NodeUpgradeSettings{
		"notes without a date": {Notes: "- 更新"},
		"invalid date":         {ReleasedAt: "2026-13-01"},
		"notes too long":       {ReleasedAt: "2026-10-24", Notes: strings.Repeat("字", 5001)},
		"control characters":   {ReleasedAt: "2026-10-24", Notes: "a\u0000b"},
	} {
		if _, err := normalizeNodeUpgrade(bad); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	got, err := normalizeNodeUpgrade(NodeUpgradeSettings{ReleasedAt: " 2026-10-24 ", Notes: "- 更新\r\n- 修复"})
	if err != nil || got.ReleasedAt != "2026-10-24" || got.Notes != "- 更新\n- 修复" {
		t.Fatalf("normalized notes: %+v %v", got, err)
	}
	a, mux, _, admin, member := publicFixture(t)
	a.RegisterIdentity(mux)
	notes := map[string]any{"releasedAt": "2026-10-24", "notes": "- 节点组件更新到新版"}
	if w := contentCall(t, mux, member, "PUT", "/api/admin/settings", map[string]any{"nodeUpgrade": notes}); w.Code != 403 {
		t.Fatalf("member changed the upgrade notes: %d", w.Code)
	}
	if w := contentCall(t, mux, admin, "PUT", "/api/admin/settings", map[string]any{"nodeUpgrade": "v1"}); w.Code != 400 || !strings.Contains(w.Body.String(), "升级节点设置格式无效") {
		t.Fatalf("malformed upgrade notes: %d %s", w.Code, w.Body.String())
	}
	w := contentCall(t, mux, admin, "PUT", "/api/admin/settings", map[string]any{"nodeUpgrade": notes})
	var saved struct {
		NodeUpgrade NodeUpgradeSettings `json:"nodeUpgrade"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &saved) != nil || saved.NodeUpgrade.Notes != "- 节点组件更新到新版" || saved.NodeUpgrade.UpdatedAt == 0 {
		t.Fatalf("save upgrade notes: %d %s", w.Code, w.Body.String())
	}
	w = contentCall(t, mux, member, "GET", "/api/settings", nil)
	var public struct {
		NodeUpgrade NodeUpgradeSettings `json:"nodeUpgrade"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &public) != nil || public.NodeUpgrade.ReleasedAt != "2026-10-24" || public.NodeUpgrade.Notes == "" {
		t.Fatalf("members cannot read the upgrade notes: %d %s", w.Code, w.Body.String())
	}
}

// nodeExecutor gives the fixture's executor a token and the listed capabilities.
func nodeExecutor(t *testing.T, a *App, capabilities ...string) string {
	t.Helper()
	token := strings.Repeat("n", 48)
	sum := sha256.Sum256([]byte(token))
	if err := a.Store.Update(func(s *State) error {
		record, _ := LoadDoc[ExecutorRecord](s, "executors", "executor-test")
		record.TokenHash = hex.EncodeToString(sum[:])
		record.Capabilities = capabilities
		return SaveDoc(s, "executors", record.ID, record)
	}); err != nil {
		t.Fatal(err)
	}
	return token
}

func nodeTask(t *testing.T, service *TaskService, u *User, kind, host string) (*httptest.ResponseRecorder, Task) {
	t.Helper()
	ssh := taskSSHFixture()
	ssh.Host = host
	service.mu.Lock()
	service.probes[taskProbeKey(u.ID, ssh)] = taskProbe{Fingerprint: ssh.Fingerprint, Expires: time.Now().Add(time.Hour)}
	service.mu.Unlock()
	w := httptest.NewRecorder()
	service.create(w, taskRequest(t, u, "POST", "/api/tasks", ID(), executor.Request{Kind: kind, SSH: ssh}))
	var job Task
	_ = json.Unmarshal(w.Body.Bytes(), &job)
	return w, job
}

func TestUpgradeAndFetchBelongToTheDeployTool(t *testing.T) {
	a, service, u := taskFixture(t)
	nodeExecutor(t, a, executor.NodeUpgradeCapability)
	upgrade, upgradeJob := nodeTask(t, service, u, "upgrade", "8.8.4.20")
	fetch, fetchJob := nodeTask(t, service, u, "fetch-config", "8.8.4.21")
	if upgrade.Code != 202 || fetch.Code != 202 {
		t.Fatalf("upgrade %d %s; fetch %d %s", upgrade.Code, upgrade.Body.String(), fetch.Code, fetch.Body.String())
	}
	service.mu.Lock()
	upgradeScript, fetchScript := service.envelopes[upgradeJob.ID].Job.Script, service.envelopes[fetchJob.ID].Job.Script
	service.mu.Unlock()
	if len(upgradeScript.Data) == 0 || upgradeScript.SHA256 == "" || len(fetchScript.Data) != 0 {
		t.Fatal("upgrade must carry the pinned installer and fetching nothing")
	}
	_ = a.Store.View(func(s *State) error {
		now := time.Now().UnixMilli()
		if toolLimit(s, "deploy", u.ID, now).Remaining != 3 || toolLimit(s, "relay", u.ID, now).Remaining != 5 {
			t.Fatal("upgrade and fetch do not use the deploy quota")
		}
		return nil
	})
	_ = a.Store.Update(func(s *State) error { s.Settings["deploy"] = false; return nil })
	for _, kind := range []string{"upgrade", "fetch-config"} {
		if w, _ := nodeTask(t, service, u, kind, "8.8.4.22"); w.Code != 409 || !strings.Contains(w.Body.String(), "管理员关闭") {
			t.Fatalf("%s ignored the deploy switch: %d %s", kind, w.Code, w.Body.String())
		}
	}
}

func TestOldExecutorsNeverTakeUpgradeOrFetch(t *testing.T) {
	a, service, u := taskFixture(t)
	token := nodeExecutor(t, a)
	if w, _ := nodeTask(t, service, u, "upgrade", "8.8.8.8"); w.Code != 409 || !strings.Contains(w.Body.String(), "升级节点或取回配置文件") {
		t.Fatalf("an executor without the capability was offered an upgrade: %d %s", w.Code, w.Body.String())
	}
	nodeExecutor(t, a, executor.NodeUpgradeCapability)
	w, job := nodeTask(t, service, u, "fetch-config", "8.8.8.8")
	if w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	old := httptest.NewRequest(http.MethodGet, "/api/executor/next", nil).WithContext(ctx)
	old.Header.Set("Authorization", "Bearer "+token)
	service.next(httptest.NewRecorder(), old)
	service.mu.Lock()
	claimedByOld := service.envelopes[job.ID].AgentID != ""
	service.mu.Unlock()
	if claimedByOld {
		t.Fatal("an old executor claimed a configuration fetch")
	}
	current := httptest.NewRequest(http.MethodGet, "/api/executor/next", nil)
	current.Header.Set("Authorization", "Bearer "+token)
	current.Header.Set(executor.NodeUpgradeHeader, "1")
	w = httptest.NewRecorder()
	service.next(w, current)
	if w.Code != 200 || !strings.Contains(w.Body.String(), job.ID) {
		t.Fatalf("a current executor could not claim the fetch: %d %s", w.Code, w.Body.String())
	}
}

func TestUpgradeAndFetchHandOverTheConfiguration(t *testing.T) {
	for kind, message := range map[string]string{
		"upgrade":      "节点已升级，用户名、密码和端口没有变化，原来的配置文件可以继续使用",
		"fetch-config": "已从节点取回配置，服务器没有改动",
	} {
		t.Run(kind, func(t *testing.T) {
			a, service, u := taskFixture(t)
			token := nodeExecutor(t, a, executor.NodeUpgradeCapability)
			if w, _ := nodeTask(t, service, u, kind, "8.8.8.8"); w.Code != 202 {
				t.Fatal(w.Body.String())
			}
			poll := httptest.NewRequest(http.MethodGet, "/api/executor/next", nil)
			poll.Header.Set("Authorization", "Bearer "+token)
			poll.Header.Set(executor.NodeUpgradeHeader, "1")
			w := httptest.NewRecorder()
			service.next(w, poll)
			var job executor.Job
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &job) != nil {
				t.Fatalf("claim: %d %s", w.Code, w.Body.String())
			}
			result := executor.Result{ID: job.ID, Lease: job.Lease, State: "succeeded", Phase: "complete", Message: "executor text is not shown", Config: json.RawMessage(nodeTestConfig), Health: &executor.Health{Service: "stopped", LocalSelfTest: "not_tested", PublicTCP: "reachable", Game: "not_tested"}}
			data, _ := json.Marshal(result)
			r := httptest.NewRequest(http.MethodPost, "/api/executor/result", bytes.NewReader(data))
			r.Header.Set("Authorization", "Bearer "+token)
			r.Header.Set("Content-Type", "application/json")
			w = httptest.NewRecorder()
			service.result(w, r)
			if w.Code != 200 {
				t.Fatalf("result refused: %d %s", w.Code, w.Body.String())
			}
			_ = a.Store.View(func(s *State) error {
				stored, _ := LoadDoc[Task](s, "tasks", job.ID)
				if stored.Message != message || stored.ConfigHost != "8.8.8.8" || stored.ConfigPort != 23456 || stored.Health == nil || stored.Health.Service != "stopped" {
					t.Fatalf("stored result: %+v", stored)
				}
				return nil
			})
			get := taskRequest(t, u, http.MethodGet, "/api/tasks/"+job.ID, "", nil)
			get.SetPathValue("id", job.ID)
			w = httptest.NewRecorder()
			service.get(w, get)
			var shown Task
			if json.Unmarshal(w.Body.Bytes(), &shown) != nil || !shown.ConfigAvailable {
				t.Fatalf("the owner cannot download the configuration: %s", w.Body.String())
			}
		})
	}
}
