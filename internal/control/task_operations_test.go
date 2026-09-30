package control

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mozziexwz/node/internal/executor"
)

func operationExecutor(t *testing.T, a *App) (string, *User) {
	t.Helper()
	token := strings.Repeat("x", 48)
	var admin *User
	if err := a.Store.Update(func(s *State) error {
		for _, u := range s.Users {
			if u.Role == "admin" {
				admin = u
			}
		}
		record, _ := LoadDoc[ExecutorRecord](s, "executors", "executor-test")
		record.TokenHash = taskHash([]byte(token))
		return SaveDoc(s, "executors", record.ID, record)
	}); err != nil {
		t.Fatal(err)
	}
	return token, admin
}

func operationClaim(t *testing.T, s *TaskService, u *User, token string, request executor.Request) executor.Job {
	t.Helper()
	w := httptest.NewRecorder()
	s.create(w, taskRequest(t, u, "POST", "/api/tasks", ID(), request))
	if w.Code != 202 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	r := httptest.NewRequest("GET", "/api/executor/next", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("X-MSBOOST-Executor-Capabilities", executor.FreeRelayGuardCapability)
	w = httptest.NewRecorder()
	s.next(w, r)
	var job executor.Job
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &job) != nil {
		t.Fatalf("claim: %d %s", w.Code, w.Body.String())
	}
	return job
}

func operationResult(t *testing.T, s *TaskService, u *User, token string, out executor.Result) *httptest.ResponseRecorder {
	t.Helper()
	r := taskRequest(t, u, "POST", "/api/executor/result", "", out)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	s.result(w, r)
	return w
}

func TestTaskOperationRestartKeepsHostsAndAcceptsExactlyOnceResult(t *testing.T) {
	a, old, u := taskFixture(t)
	token, _ := operationExecutor(t, a)
	request := executor.Request{Kind: "deploy", Mode: "fresh", SSH: taskSSHFixture()}
	job := operationClaim(t, old, u, token, request)
	_ = a.Store.View(func(s *State) error {
		if bytes.Contains(s.Docs["task_operations"][job.ID], []byte(request.SSH.Password)) {
			t.Fatal("SSH password persisted")
		}
		return nil
	})
	restarted := NewTaskService(a)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	restarted.Start(ctx)
	restarted.probes[taskProbeKey(u.ID, request.SSH)] = taskProbe{Fingerprint: request.SSH.Fingerprint, Expires: time.Now().Add(time.Hour)}
	w := httptest.NewRecorder()
	restarted.create(w, taskRequest(t, u, "POST", "/api/tasks", ID(), request))
	if w.Code != 409 || !strings.Contains(w.Body.String(), "未结束任务") {
		t.Fatalf("restart lost fence: %d %s", w.Code, w.Body.String())
	}
	other := request
	other.SSH.Host = "1.1.1.1"
	restarted.probes[taskProbeKey(u.ID, other.SSH)] = taskProbe{Fingerprint: other.SSH.Fingerprint, Expires: time.Now().Add(time.Hour)}
	w = httptest.NewRecorder()
	restarted.create(w, taskRequest(t, u, "POST", "/api/tasks", ID(), other))
	if w.Code != 202 {
		t.Fatalf("unrelated host blocked: %d %s", w.Code, w.Body.String())
	}
	out := executor.Result{ID: job.ID, Lease: job.Lease, State: "failed", Phase: "ssh", ErrorCode: "ssh_auth"}
	for i := 0; i < 2; i++ {
		w = operationResult(t, restarted, u, token, out)
		if w.Code != 200 {
			t.Fatalf("completion/ACK retry: %d %s", w.Code, w.Body.String())
		}
	}
	out.State = "succeeded"
	if w = operationResult(t, restarted, u, token, out); w.Code != 409 {
		t.Fatal("different result overwrote receipt")
	}
	w = httptest.NewRecorder()
	restarted.create(w, taskRequest(t, u, "POST", "/api/tasks", ID(), request))
	if w.Code != 202 {
		t.Fatalf("completion did not release host: %d %s", w.Code, w.Body.String())
	}
}

func TestTaskOperationFrontHostFenceAndExplicitReconciliation(t *testing.T) {
	a, service, u := taskFixture(t)
	token, _ := operationExecutor(t, a)
	job := operationClaim(t, service, u, token, executor.Request{Kind: "deploy", Mode: "fresh", SSH: taskSSHFixture()})
	if err := a.Store.Update(func(s *State) error {
		o, _ := LoadDoc[taskOperation](s, "task_operations", job.ID)
		o.Hosts = append(o.Hosts, "1.1.1.1")
		return SaveDoc(s, "task_operations", o.ID, o)
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	restarted := NewTaskService(a)
	restarted.Start(ctx)
	_ = a.Store.View(func(s *State) error {
		if !restarted.targetBusy(s, "1.1.1.1") {
			t.Fatal("front host unlocked")
		}
		return nil
	})
	other := *u
	other.ID = "not-owner"
	for _, actor := range []*User{&other, u} {
		r := taskRequest(t, actor, "POST", "/api/tasks/"+job.ID+"/reconcile", "", map[string]string{"confirm": "CONFIRM_TASK_ENDED"})
		r.SetPathValue("id", job.ID)
		w := httptest.NewRecorder()
		restarted.reconcile(w, r)
		if actor == u && w.Code != 200 || actor != u && w.Code != 409 {
			t.Fatalf("review auth: %d %s", w.Code, w.Body.String())
		}
	}
	_ = a.Store.View(func(s *State) error {
		if restarted.targetBusy(s, "1.1.1.1") {
			t.Fatal("confirmed operation still fenced")
		}
		return nil
	})
	if w := operationResult(t, restarted, u, token, executor.Result{ID: job.ID, Lease: job.Lease, State: "failed"}); w.Code != 409 {
		t.Fatal("late result changed manually reconciled task")
	}
}

func TestExecutorDrainKeepsResultChannelAndBlocksDeleteAndRotation(t *testing.T) {
	a, s, u := taskFixture(t)
	token, admin := operationExecutor(t, a)
	job := operationClaim(t, s, u, token, executor.Request{Kind: "deploy", Mode: "fresh", SSH: taskSSHFixture()})
	r := taskRequest(t, admin, "PATCH", "/api/admin/executors/executor-test", "", map[string]string{"status": "disabled"})
	r.SetPathValue("id", "executor-test")
	w := httptest.NewRecorder()
	s.editExecutor(w, r)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	_ = a.Store.View(func(state *State) error {
		e, _ := LoadDoc[ExecutorRecord](state, "executors", "executor-test")
		if e.Status != "draining" {
			t.Fatal("busy executor disabled")
		}
		return nil
	})
	w = httptest.NewRecorder()
	s.deleteExecutor(w, r)
	if w.Code != 409 {
		t.Fatal("busy executor deleted")
	}
	w = httptest.NewRecorder()
	s.renewExecutor(w, r)
	if w.Code != 409 {
		t.Fatal("busy executor rotated")
	}
	for i := 0; i < 2; i++ {
		w = operationResult(t, s, u, token, executor.Result{ID: job.ID, Lease: job.Lease, State: "failed"})
		if w.Code != 200 {
			t.Fatalf("drain result/ACK: %d %s", w.Code, w.Body.String())
		}
	}
	_ = a.Store.View(func(state *State) error {
		e, _ := LoadDoc[ExecutorRecord](state, "executors", "executor-test")
		if e.Status != "disabled" {
			t.Fatal("drain not finalized")
		}
		return nil
	})
	w = httptest.NewRecorder()
	s.deleteExecutor(w, r)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
}

func TestExecutorDrainFinishesReadOnlyProbeAfterRestart(t *testing.T) {
	a, _, _ := taskFixture(t)
	if err := a.Store.Update(func(s *State) error {
		agent, _ := LoadDoc[ExecutorRecord](s, "executors", "executor-test")
		agent.Status = "draining"
		if err := SaveDoc(s, "executors", agent.ID, agent); err != nil {
			return err
		}
		if err := SaveDoc(s, "tasks", "probe", Task{ID: "probe", Kind: "fingerprint", State: "running"}); err != nil {
			return err
		}
		return SaveDoc(s, "task_operations", "probe", taskOperation{ID: "probe", AgentID: agent.ID, Status: "claimed", Request: executor.Request{Kind: "fingerprint"}})
	}); err != nil {
		t.Fatal(err)
	}
	service := NewTaskService(a)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service.Start(ctx)
	if err := a.Store.View(func(s *State) error {
		agent, _ := LoadDoc[ExecutorRecord](s, "executors", "executor-test")
		if agent.Status != "disabled" {
			t.Fatal("completed read-only probe left executor draining")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestTaskOperationDuplicateRestoresOnlyUnexpiredConfig(t *testing.T) {
	a, service, u := taskFixture(t)
	token, _ := operationExecutor(t, a)
	job := operationClaim(t, service, u, token, executor.Request{Kind: "deploy", Mode: "fresh", SSH: taskSSHFixture()})
	out := executor.Result{ID: job.ID, Lease: job.Lease, State: "succeeded", Config: json.RawMessage(relayTestConfig)}
	if w := operationResult(t, service, u, token, out); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	restarted := NewTaskService(a)
	if w := operationResult(t, restarted, u, token, out); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	c, ok := restarted.configs[job.ID]
	if !ok || !bytes.Equal(c.Data, out.Config) {
		t.Fatal("ACK retry did not restore lost config handoff")
	}
	if err := a.Store.Update(func(s *State) error {
		v, _ := LoadDoc[Task](s, "tasks", job.ID)
		v.UpdatedAt = time.Now().Add(-11 * time.Minute).UnixMilli()
		return SaveDoc(s, "tasks", job.ID, v)
	}); err != nil {
		t.Fatal(err)
	}
	restarted = NewTaskService(a)
	if w := operationResult(t, restarted, u, token, out); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if _, ok := restarted.configs[job.ID]; ok {
		t.Fatal("retry extended config retention")
	}
}

func TestTaskOperationRestoreKeepsReviewAndRevokesOldReceipts(t *testing.T) {
	a, service, u := taskFixture(t)
	token, _ := operationExecutor(t, a)
	job := operationClaim(t, service, u, token, executor.Request{Kind: "deploy", Mode: "fresh", SSH: taskSSHFixture()})
	if err := a.Store.Update(func(s *State) error {
		if err := isolateRestoredState(s, true); err != nil {
			return err
		}
		op, _ := LoadDoc[taskOperation](s, "task_operations", job.ID)
		task, _ := LoadDoc[Task](s, "tasks", job.ID)
		if op.Status != "review" || op.LeaseHash != "" || op.Receipt != "" || !task.NeedsReview {
			t.Fatal("restored task lost review or retained old result authorization")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if w := operationResult(t, service, u, token, executor.Result{ID: job.ID, Lease: job.Lease, State: "failed"}); w.Code != 401 {
		t.Fatal("restored executor remained authenticated")
	}
	r := taskRequest(t, u, "POST", "/api/tasks/"+job.ID+"/reconcile", "", map[string]string{"confirm": "CONFIRM_TASK_ENDED"})
	r.SetPathValue("id", job.ID)
	w := httptest.NewRecorder()
	service.reconcile(w, r)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
}

func TestTaskOperationRevocationReconciliationReleasesMemoryFence(t *testing.T) {
	a, service, u := taskFixture(t)
	token, admin := operationExecutor(t, a)
	job := operationClaim(t, service, u, token, executor.Request{Kind: "deploy", Mode: "fresh", SSH: taskSSHFixture()})
	r := taskRequest(t, admin, "POST", "/api/admin/executors/executor-test/revoke", "", map[string]string{"confirm": "REVOKE_EXECUTOR"})
	r.SetPathValue("id", "executor-test")
	w := httptest.NewRecorder()
	service.revokeExecutor(w, r)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	_ = a.Store.View(func(s *State) error {
		if !service.targetBusy(s, job.Request.SSH.Host) {
			t.Fatal("revocation alone released an unverified remote operation")
		}
		return nil
	})
	r = taskRequest(t, u, "POST", "/api/tasks/"+job.ID+"/reconcile", "", map[string]string{"confirm": "CONFIRM_TASK_ENDED"})
	r.SetPathValue("id", job.ID)
	w = httptest.NewRecorder()
	service.reconcile(w, r)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	_ = a.Store.View(func(s *State) error {
		if service.targetBusy(s, job.Request.SSH.Host) {
			t.Fatal("confirmed revoked operation retained a stale in-memory host fence")
		}
		return nil
	})
	if w := operationResult(t, service, u, token, executor.Result{ID: job.ID, Lease: job.Lease, State: "failed"}); w.Code != 401 {
		t.Fatal("revoked executor was reauthorized by reconciliation")
	}
}

func TestTaskOperationSanitizesBothHostsWithoutMutatingEnvelope(t *testing.T) {
	s := &State{Docs: map[string]map[string]json.RawMessage{}}
	front := taskSSHFixture()
	front.Host = "1.1.1.1"
	front.Password = "private-front-secret"
	e := &taskEnvelope{Job: executor.Job{ID: "sanitized", Lease: "private-lease", Request: executor.Request{Kind: "relay", SSH: taskSSHFixture(), Front: &front, ClientConfig: json.RawMessage(relayTestConfig), DD: &executor.DDOptions{}}}}
	if err := saveTaskOperation(s, e); err != nil {
		t.Fatal(err)
	}
	op, _ := LoadDoc[taskOperation](s, "task_operations", e.Job.ID)
	if len(op.Hosts) != 2 || op.Hosts[1] != "1.1.1.1" || op.Request.SSH.Password != "" || op.Request.Front.Password != "" || op.Request.ClientConfig != nil || op.Request.DD != nil {
		t.Fatal("unsafe persistent envelope")
	}
	if e.Job.Request.Front.Password != front.Password || e.Job.Request.SSH.Password == "" || e.Job.Request.ClientConfig == nil {
		t.Fatal("sanitizing destroyed one-time execution credentials")
	}
}
