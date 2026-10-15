package control

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mozziexwz/node/internal/executor"
	"github.com/mozziexwz/node/internal/relayruntime"
)

func relayV2Request(t *testing.T, in relayruntime.V2SyncRequest, token, remote string) *http.Request {
	t.Helper()
	in.Sequence++
	in.RequestID = commerceID()
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/relay-agent/v2/sync", bytes.NewReader(raw))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)
	if remote != "" {
		r.RemoteAddr = remote
	}
	return r
}

func agentJSONRequest(t *testing.T, path string, body any) *http.Request {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	r.Header.Set("Content-Type", "application/json")
	return r
}

// whileStoreLocked runs requests while every store transaction would block.
// Handlers that answer from memory finish; anything touching the store hangs.
func whileStoreLocked(t *testing.T, a *App, run func() []int) []int {
	t.Helper()
	a.Store.mu.Lock()
	done := make(chan []int, 1)
	go func() { done <- run() }()
	select {
	case codes := <-done:
		a.Store.mu.Unlock()
		return codes
	case <-time.After(3 * time.Second):
		a.Store.mu.Unlock()
		<-done
		t.Fatal("an unknown agent credential waited for the serialized store")
		return nil
	}
}

func TestAgentUnknownCredentialsNeverReachTheStore(t *testing.T) {
	f := newRelayV2Fixture(t)
	f.sync(t, 0)
	service := NewTaskService(f.app)
	junk := strings.Repeat("j", 64)
	// Settle the index after the warm-up write, then lock the store.
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, relayV2Request(t, f.requests[0], junk, "198.51.100.1:1"))
	if w.Code != 401 {
		t.Fatalf("junk relay token: %d", w.Code)
	}
	writes := f.app.Store.Writes()
	codes := whileStoreLocked(t, f.app, func() []int {
		out := []int{}
		serve := func(r *http.Request) {
			w := httptest.NewRecorder()
			f.mux.ServeHTTP(w, r)
			out = append(out, w.Code)
		}
		serve(relayV2Request(t, f.requests[0], junk, "198.51.100.1:1"))
		serve(agentJSONRequest(t, "/api/relay-agent/register", map[string]string{"enrollmentToken": junk}))
		check := httptest.NewRecorder()
		f.mux.ServeHTTP(check, agentJSONRequest(t, "/api/relay-agent/enrollment-check", map[string]string{"enrollmentToken": junk}))
		out = append(out, check.Code)
		if !strings.Contains(check.Body.String(), "invalid_token") {
			out = append(out, -1)
		}
		next := httptest.NewRequest("GET", "/api/executor/next", nil)
		next.Header.Set("Authorization", "Bearer "+junk)
		w := httptest.NewRecorder()
		service.next(w, next)
		out = append(out, w.Code)
		return out
	})
	if want := []int{401, 401, 200, 401}; len(codes) != len(want) || codes[0] != want[0] || codes[1] != want[1] || codes[2] != want[2] || codes[3] != want[3] {
		t.Fatalf("unknown credential responses %v, want %v", codes, want)
	}
	if f.app.Store.Writes() != writes {
		t.Fatal("unknown credentials caused a store write")
	}
	// The real token still works and is checked authoritatively.
	f.sync(t, 0)
}

func TestAgentAuthFailureLimitBlocksOnlyUnknownTokens(t *testing.T) {
	f := newRelayV2Fixture(t)
	junk := strings.Repeat("k", 64)
	for i := 0; i < agentAuthFailureLimit; i++ {
		w := httptest.NewRecorder()
		f.mux.ServeHTTP(w, relayV2Request(t, f.requests[0], junk, ""))
		if w.Code != 401 {
			t.Fatalf("failure %d: %d", i, w.Code)
		}
	}
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, relayV2Request(t, f.requests[0], junk, ""))
	if w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatalf("repeated unknown tokens not limited: %d", w.Code)
	}
	// The same source with a valid token is never limited by failures.
	f.sync(t, 0)
	w = httptest.NewRecorder()
	f.mux.ServeHTTP(w, relayV2Request(t, f.requests[0], junk, "203.0.113.50:1"))
	if w.Code != 401 {
		t.Fatalf("another source inherited the limit: %d", w.Code)
	}
}

func TestAgentCredentialAcceptsNewTokensImmediately(t *testing.T) {
	f := newRelayV2Fixture(t)
	f.sync(t, 0)
	token := commerceID() + commerceID()
	agent := RelayAgent{ID: commerceID(), Name: "new", Address: "9.9.9.9", Capability: "relay", Enabled: true, TokenHash: commerceHash(token), PortRanges: []PortRange{{Start: 21000, End: 21000}}}
	if err := f.app.Store.Update(func(s *State) error { return SaveDoc(s, "relay_agents", agent.ID, agent) }); err != nil {
		t.Fatal(err)
	}
	in := relayruntime.V2SyncRequest{ProtocolVersion: 2, AgentID: agent.ID, AgentInstanceID: commerceID(), Capabilities: append([]string(nil), relayruntime.V2Capabilities...)}
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, relayV2Request(t, in, token, ""))
	if w.Code != 200 {
		t.Fatalf("token written moments ago was rejected: %d %s", w.Code, w.Body.String())
	}
	// A token bound to another agent ID is rejected even though it is known.
	other := in
	other.AgentID = f.agents[0].ID
	w = httptest.NewRecorder()
	f.mux.ServeHTTP(w, relayV2Request(t, other, token, ""))
	if w.Code != 401 {
		t.Fatalf("token accepted for a different agent: %d", w.Code)
	}
}

func expiryFixture(t *testing.T, state string) (*App, *TaskService, string, *taskEnvelope) {
	t.Helper()
	a, service, u := taskFixture(t)
	id := ID()
	envelope := &taskEnvelope{Job: executor.Job{ID: id, Lease: ID(), Request: executor.Request{Kind: "deploy", SSH: taskSSHFixture()}}, UserID: u.ID, QueuedAt: time.Now().Add(-10 * time.Minute)}
	if err := a.Store.Update(func(s *State) error {
		if err := SaveDoc(s, "tasks", id, Task{ID: id, UserID: u.ID, Kind: "deploy", Host: "8.8.8.8", State: state}); err != nil {
			return err
		}
		return saveTaskOperation(s, envelope)
	}); err != nil {
		t.Fatal(err)
	}
	service.envelopes[id] = envelope
	return a, service, id, envelope
}

func expiryTaskState(t *testing.T, a *App, id string) string {
	t.Helper()
	var state string
	_ = a.Store.View(func(s *State) error {
		job, _ := LoadDoc[Task](s, "tasks", id)
		state = job.State
		return nil
	})
	return state
}

func TestTaskExpiryRetriesWhenTheStoreRefusesWrites(t *testing.T) {
	a, service, id, _ := expiryFixture(t, "queued")
	if err := a.Store.Update(func(s *State) error {
		s.Docs[backupPauseCollection] = map[string]json.RawMessage{"default": json.RawMessage(`{}`)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	service.expire()
	if _, kept := service.envelopes[id]; !kept || expiryTaskState(t, a, id) != "queued" {
		t.Fatal("a refused expiry write dropped the envelope or changed the task")
	}
	if err := a.Store.transactionWithBackupPause(true, true, func(s *State) error {
		delete(s.Docs, backupPauseCollection)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	service.expire()
	if _, kept := service.envelopes[id]; kept || expiryTaskState(t, a, id) != "interrupted" {
		t.Fatal("the retried expiry did not settle the task")
	}
}

func TestTaskExpiryLeavesATaskClaimedAfterTheDecision(t *testing.T) {
	a, service, id, _ := expiryFixture(t, "running")
	// The envelope still looks unclaimed, but the durable claim already ran.
	service.expire()
	if _, kept := service.envelopes[id]; !kept || expiryTaskState(t, a, id) != "running" {
		t.Fatal("expiry interrupted a task an executor had just claimed")
	}
}

func TestTaskExpiryDoesNotHoldTheTaskLockWhileWaitingForTheStore(t *testing.T) {
	a, service, _, _ := expiryFixture(t, "queued")
	a.Store.mu.Lock()
	done := make(chan struct{})
	go func() {
		service.expire()
		close(done)
	}()
	// Give expire time to reach the store, where it must now be waiting.
	time.Sleep(200 * time.Millisecond)
	free := service.mu.TryLock()
	if free {
		service.mu.Unlock()
	}
	finished := false
	select {
	case <-done:
		finished = true
	default:
	}
	a.Store.mu.Unlock()
	<-done
	if finished {
		t.Fatal("expire completed without waiting for the locked store")
	}
	if !free {
		t.Fatal("expire held the task lock while waiting for the store")
	}
}

func TestOverlappingResultDeliveriesBothSucceed(t *testing.T) {
	a, service, u := taskFixture(t)
	token, _ := operationExecutor(t, a)
	for round := 0; round < 4; round++ {
		job := operationClaim(t, service, u, token, executor.Request{Kind: "deploy", Mode: "fresh", SSH: taskSSHFixture()})
		out := executor.Result{ID: job.ID, Lease: job.Lease, State: "failed", ErrorCode: "ssh_auth_failed"}
		var wg sync.WaitGroup
		responses := make([]*httptest.ResponseRecorder, 2)
		for i := range responses {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				responses[i] = operationResult(t, service, u, token, out)
			}(i)
		}
		wg.Wait()
		duplicates := 0
		for _, w := range responses {
			if w.Code != 200 {
				t.Fatalf("round %d: overlapping delivery %d %s", round, w.Code, w.Body.String())
			}
			if strings.Contains(w.Body.String(), `"duplicate":true`) {
				duplicates++
			}
		}
		if duplicates != 1 {
			t.Fatalf("round %d: %d duplicate acknowledgements, want 1", round, duplicates)
		}
	}
}
