package control

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestExecutorTokenRotationRequiresAdminAndIdleAgent(t *testing.T) {
	a, service, member := taskFixture(t)
	invoke := func(user *User) *httptest.ResponseRecorder {
		r := taskRequest(t, user, "POST", "/api/admin/executors/executor-test/enrollment", "", nil)
		r.SetPathValue("id", "executor-test")
		w := httptest.NewRecorder()
		service.renewExecutor(w, r)
		return w
	}
	if w := invoke(member); w.Code != 403 {
		t.Fatalf("member rotated token: %d", w.Code)
	}
	admin := *member
	admin.Role = "admin"
	service.envelopes["running"] = &taskEnvelope{AgentID: "executor-test"}
	if w := invoke(&admin); w.Code != 409 {
		t.Fatalf("rotated a busy executor: %d", w.Code)
	}
	delete(service.envelopes, "running")
	w := invoke(&admin)
	if w.Code != 200 {
		t.Fatalf("rotation: %d %s", w.Code, w.Body.String())
	}
	var result struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || len(result.Token) < 32 {
		t.Fatal("missing secure token")
	}
	auth := httptest.NewRequest("GET", "/api/executor/next", nil)
	auth.Header.Set("Authorization", "Bearer "+result.Token)
	if _, err := service.executorAuth(auth); err != nil {
		t.Fatalf("new token rejected: %v", err)
	}
	if w = invoke(&admin); w.Code != 200 {
		t.Fatalf("second rotation: %d", w.Code)
	}
	if _, err := service.executorAuth(auth); err == nil {
		t.Fatal("old token still accepted")
	}
	_ = a.Store.View(func(state *State) error {
		row, _ := LoadDoc[ExecutorRecord](state, "executors", "executor-test")
		if row.LastSeenAt != 0 || row.TokenHash == result.Token {
			t.Fatal("rotation did not reset heartbeat or persisted plaintext")
		}
		return nil
	})
}
