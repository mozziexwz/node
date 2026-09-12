package control

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mozziexwz/node/internal/executor"
)

func taskFixture(t *testing.T) (*App, *TaskService, *User) {
	t.Helper()
	a, err := New(Config{DataDir: t.TempDir(), MasterKey: strings.Repeat("12", 32), AdminEmail: "12345678@qq.com", AdminPassword: "Test-admin-Password-789!", NodeScript: filepath.Join("..", "..", "installers", "node", "msboost.sh")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	u := &User{ID: "task-test-user", Email: "10000001@qq.com", Role: "user", Status: "active", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
	if err = a.Store.Update(func(s *State) error {
		s.Users[u.ID] = u
		s.Settings["deploy"] = true
		s.Settings["relay"] = true
		s.Settings["dd"] = true
		s.Settings["freeToolsRequireVerifiedEmail"] = false
		return SaveDoc(s, "executors", "executor-test", ExecutorRecord{ID: "executor-test", Name: "Test", Status: "active", LastSeenAt: time.Now().UnixMilli()})
	}); err != nil {
		t.Fatal(err)
	}
	service := NewTaskService(a)
	if service.initErr != nil {
		t.Fatal(service.initErr)
	}
	s := taskSSHFixture()
	service.probes[taskProbeKey(u.ID, s)] = taskProbe{Fingerprint: s.Fingerprint, Expires: time.Now().Add(time.Hour)}
	return a, service, u
}
func taskSSHFixture() executor.SSH {
	return executor.SSH{Host: "8.8.8.8", Port: 22, User: "root", Password: "ONE_TIME_SSH_DO_NOT_PERSIST", Fingerprint: "SHA256:" + strings.Repeat("A", 43)}
}
func taskRequest(t *testing.T, u *User, method, path, key string, body any) *http.Request {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(data))
	r.Header.Set("Content-Type", "application/json")
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	return r.WithContext(context.WithValue(r.Context(), authContextKey{}, &requestIdentity{user: u}))
}

type unreadableBody struct{ t *testing.T }

func (b unreadableBody) Read([]byte) (int, error) {
	b.t.Error("SSH request body read before task gate")
	return 0, io.EOF
}
func (unreadableBody) Close() error { return nil }
func TestTaskEmailGateBeforeSecretsAndQuota(t *testing.T) {
	a, s, u := taskFixture(t)
	_ = a.Store.Update(func(state *State) error { state.Settings["freeToolsRequireVerifiedEmail"] = true; return nil })
	r := taskRequest(t, u, "POST", "/api/tasks", "gate-test-key", nil)
	r.Body = unreadableBody{t}
	w := httptest.NewRecorder()
	s.create(w, r)
	if w.Code != 403 {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
	_ = a.Store.View(func(state *State) error {
		if len(ListDocs[Task](state, "tasks")) != 0 {
			t.Fatal("email gate consumed quota")
		}
		return nil
	})
}

func TestTaskMaintenanceBlocksAllRolesBeforeSecretsAndQuota(t *testing.T) {
	a, service, member := taskFixture(t)
	_ = a.Store.Update(func(s *State) error { s.Settings["maintenance"] = true; return nil })
	for _, role := range []string{"user", "admin"} {
		u := *member
		u.Role = role
		for _, kind := range []string{"deploy", "relay", "dd"} {
			r := taskRequest(t, &u, "POST", "/api/tasks", "maintenance-key", executor.Request{Kind: kind, SSH: taskSSHFixture()})
			r.Body = unreadableBody{t}
			w := httptest.NewRecorder()
			service.create(w, r)
			if w.Code != 403 || !strings.Contains(w.Body.String(), "维护") {
				t.Fatalf("%s/%s maintenance bypass: %d %s", role, kind, w.Code, w.Body.String())
			}
		}
	}
	_, err := service.ProvisionFront(context.Background(), member.ID, taskSSHFixture(), "1.1.1.1", 12345)
	if err == nil || !strings.Contains(err.Error(), "维护") {
		t.Fatalf("paid front maintenance gate: %v", err)
	}
	_ = a.Store.View(func(s *State) error {
		if len(ListDocs[Task](s, "tasks")) != 0 {
			t.Fatal("maintenance consumed quota or persisted a new task")
		}
		return nil
	})
	if len(service.envelopes) != 0 {
		t.Fatal("maintenance queued remote execution")
	}
}

func TestTaskAdminMetadataNeverGrantsAnotherOwnersConfiguration(t *testing.T) {
	a, service, owner := taskFixture(t)
	admin := &User{ID: "task-auditor", Role: "admin", Status: "active"}
	other := &User{ID: "task-other", Role: "user", Status: "active"}
	job := Task{ID: "metadata-task", UserID: owner.ID, Kind: "deploy", Host: "8.8.8.8", State: "succeeded", Message: taskPublicMessage("succeeded"), Idempotency: "hidden-key", RequestDigest: "hidden-digest", ConfigAvailable: true}
	if err := a.Store.Update(func(s *State) error {
		s.Settings["maintenance"] = true
		return SaveDoc(s, "tasks", job.ID, job)
	}); err != nil {
		t.Fatal(err)
	}
	secret := []byte(`{"password":"owner-only-secret"}`)
	service.configs[job.ID] = taskConfig{Data: secret, UserID: owner.ID, Expires: time.Now().Add(time.Minute)}
	mux := http.NewServeMux()
	service.Register(mux)
	for _, test := range []struct {
		user      *User
		want      int
		available bool
	}{{owner, 200, true}, {admin, 200, false}, {other, 404, false}} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, taskRequest(t, test.user, "GET", "/api/tasks/"+job.ID, "", nil))
		if w.Code != test.want {
			t.Fatalf("metadata %s: %d %s", test.user.ID, w.Code, w.Body.String())
		}
		if test.want != 200 {
			continue
		}
		var got Task
		_ = json.Unmarshal(w.Body.Bytes(), &got)
		if got.ID != job.ID || got.ConfigAvailable != test.available {
			t.Fatalf("wrong metadata/config availability for %s: %+v", test.user.ID, got)
		}
		for _, forbidden := range []string{"owner-only-secret", "password", "hidden-key", "hidden-digest"} {
			if strings.Contains(w.Body.String(), forbidden) {
				t.Fatalf("metadata disclosed %s", forbidden)
			}
		}
	}
	for _, test := range []struct {
		user *User
		want int
	}{{owner, 200}, {admin, 404}, {other, 404}} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, taskRequest(t, test.user, "GET", "/api/tasks/"+job.ID+"/config", "", nil))
		if w.Code != test.want {
			t.Fatalf("config %s: %d", test.user.ID, w.Code)
		}
		if test.want == 200 && !bytes.Equal(w.Body.Bytes(), secret) {
			t.Fatal("owner configuration unavailable during maintenance")
		}
		if test.want != 200 && bytes.Contains(w.Body.Bytes(), secret) {
			t.Fatal("another user's config disclosed")
		}
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, taskRequest(t, admin, "GET", "/api/admin/tasks", "", nil))
	if w.Code != 200 {
		t.Fatalf("admin audit list: %d %s", w.Code, w.Body.String())
	}
	var audit struct {
		Tasks []Task `json:"tasks"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &audit)
	if len(audit.Tasks) != 1 || audit.Tasks[0].ConfigAvailable {
		t.Fatal("admin audit list advertised another owner's configuration")
	}
}
func TestTaskQuotaAtomicAndIndependent(t *testing.T) {
	a, s, u := taskFixture(t)
	var wg sync.WaitGroup
	codes := make(chan int, 12)
	for i := 0; i < 12; i++ {
		ssh := taskSSHFixture()
		ssh.Host = "8.8.4." + strconv.Itoa(10+i)
		s.mu.Lock()
		s.probes[taskProbeKey(u.ID, ssh)] = taskProbe{Fingerprint: ssh.Fingerprint, Expires: time.Now().Add(time.Hour)}
		s.mu.Unlock()
		wg.Add(1)
		go func(ssh executor.SSH) {
			defer wg.Done()
			r := taskRequest(t, u, "POST", "/api/tasks", ID(), executor.Request{Kind: "deploy", Mode: "fresh", SSH: ssh})
			w := httptest.NewRecorder()
			s.create(w, r)
			codes <- w.Code
		}(ssh)
	}
	wg.Wait()
	close(codes)
	accepted := 0
	for c := range codes {
		if c == 202 {
			accepted++
		} else if c != 409 {
			t.Fatalf("unexpected response %d", c)
		}
	}
	if accepted != 5 {
		t.Fatalf("accepted %d; want 5", accepted)
	}
	_ = a.Store.View(func(state *State) error {
		now := time.Now().UnixMilli()
		if toolLimit(state, "deploy", u.ID, now).Remaining != 0 || toolLimit(state, "relay", u.ID, now).Remaining != 5 || toolLimit(state, "dd", u.ID, now).Remaining != 5 {
			t.Fatal("limits are not independent")
		}
		return nil
	})
}
func TestTaskIdempotencyAndNoPersistedCredentials(t *testing.T) {
	a, s, u := taskFixture(t)
	request := executor.Request{Kind: "deploy", Mode: "fresh", SSH: taskSSHFixture()}
	firstID := ""
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		s.create(w, taskRequest(t, u, "POST", "/api/tasks", "same-key-123", request))
		expected := 202
		if i == 1 {
			expected = 200
		}
		if w.Code != expected {
			t.Fatalf("got %d: %s", w.Code, w.Body.String())
		}
		var job Task
		_ = json.Unmarshal(w.Body.Bytes(), &job)
		if i == 0 {
			firstID = job.ID
		} else if job.ID != firstID {
			t.Fatal("idempotency replay created another job")
		}
	}
	request.Mode = "repair"
	w := httptest.NewRecorder()
	s.create(w, taskRequest(t, u, "POST", "/api/tasks", "same-key-123", request))
	if w.Code != 409 {
		t.Fatal("same key accepted changed mode")
	}
	_ = a.Store.View(func(state *State) error {
		data, _ := json.Marshal(state)
		if bytes.Contains(data, []byte("ONE_TIME_SSH_DO_NOT_PERSIST")) {
			t.Fatal("SSH password persisted")
		}
		if len(ListDocs[Task](state, "tasks")) != 1 {
			t.Fatal("idempotency counted twice")
		}
		return nil
	})
}
func TestExecutorClaimIsSingleDeliveryAndLeaseBound(t *testing.T) {
	a, s, u := taskFixture(t)
	token := strings.Repeat("t", 48)
	sum := sha256.Sum256([]byte(token))
	hash := hex.EncodeToString(sum[:])
	_ = a.Store.Update(func(state *State) error {
		x, _ := LoadDoc[ExecutorRecord](state, "executors", "executor-test")
		x.TokenHash = hash
		return SaveDoc(state, "executors", x.ID, x)
	})
	w := httptest.NewRecorder()
	s.create(w, taskRequest(t, u, "POST", "/api/tasks", "lease-test-key", executor.Request{Kind: "deploy", Mode: "fresh", SSH: taskSSHFixture()}))
	if w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	var task Task
	_ = json.Unmarshal(w.Body.Bytes(), &task)
	r := httptest.NewRequest("GET", "/api/executor/next", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	r.RemoteAddr = "8.8.4.4:33333"
	r.Header.Set("X-Forwarded-For", "1.1.1.1")
	w = httptest.NewRecorder()
	s.next(w, r)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var job executor.Job
	_ = json.Unmarshal(w.Body.Bytes(), &job)
	if job.Request.SSH.Password != "ONE_TIME_SSH_DO_NOT_PERSIST" {
		t.Fatal("envelope missing credential")
	}
	_ = a.Store.View(func(state *State) error {
		record, _ := LoadDoc[ExecutorRecord](state, "executors", "executor-test")
		if record.IP != "8.8.4.4" {
			t.Fatal("executor source IP accepted untrusted forwarding header")
		}
		return nil
	})
	s.mu.Lock()
	if s.envelopes[job.ID].Job.Request.SSH.Password != "" {
		t.Fatal("credential retained after delivery")
	}
	s.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r = httptest.NewRequest("GET", "/api/executor/next", nil).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+token)
	w = httptest.NewRecorder()
	s.next(w, r)
	if strings.Contains(w.Body.String(), job.ID) {
		t.Fatal("delivered claimed job twice")
	}
	result := executor.Result{ID: job.ID, Lease: "wrong", State: "failed"}
	data, _ := json.Marshal(result)
	r = httptest.NewRequest("POST", "/api/executor/result", bytes.NewReader(data))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	s.result(w, r)
	if w.Code != 409 {
		t.Fatal("accepted result with wrong lease")
	}
	result.Lease = job.Lease
	result.Message = "secret in malicious diagnostic: ONE_TIME_SSH_DO_NOT_PERSIST"
	result.NextStep = "ONE_TIME_SSH_DO_NOT_PERSIST"
	result.ErrorCode = "ssh_auth"
	result.Phase = "ONE_TIME_SSH_DO_NOT_PERSIST"
	data, _ = json.Marshal(result)
	r = httptest.NewRequest("POST", "/api/executor/result", bytes.NewReader(data))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	s.result(w, r)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	_ = a.Store.View(func(state *State) error {
		stored, _ := LoadDoc[Task](state, "tasks", job.ID)
		if strings.Contains(stored.Message, "DO_NOT_PERSIST") {
			t.Fatal("untrusted diagnostic persisted")
		}
		if stored.ErrorCode != "ssh_auth" || stored.Phase != "ssh_auth" || strings.Contains(stored.NextStep, "DO_NOT_PERSIST") || !strings.Contains(stored.Message, "认证失败") {
			t.Fatalf("diagnostic vocabulary mismatch: %+v", stored)
		}
		return nil
	})
}
func TestRestartMarksDDUnknownWithoutReplay(t *testing.T) {
	a, _, u := taskFixture(t)
	_ = a.Store.Update(func(s *State) error {
		for _, kind := range []string{"dd", "deploy"} {
			job := Task{ID: kind, UserID: u.ID, Kind: kind, State: "running"}
			_ = SaveDoc(s, "tasks", job.ID, job)
		}
		return nil
	})
	service := NewTaskService(a)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service.Start(ctx)
	_ = a.Store.View(func(s *State) error {
		dd, _ := LoadDoc[Task](s, "tasks", "dd")
		deploy, _ := LoadDoc[Task](s, "tasks", "deploy")
		if dd.State != "unknown" || deploy.State != "interrupted" {
			t.Fatal("restart resurrected or falsely completed a task")
		}
		return nil
	})
	if len(service.envelopes) != 0 {
		t.Fatal("persisted secrets replayed")
	}
}
