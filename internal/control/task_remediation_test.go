package control

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mozziexwz/node/internal/executor"
)

func TestReadOnlyFingerprintRemainsAvailableForUnverifiedPaidUsersAndMaintenance(t *testing.T) {
	for _, maintenance := range []bool{false, true} {
		a, service, user := taskFixture(t)
		if user.EmailVerifiedAt != 0 || user.ExpiresAt <= time.Now().UnixMilli() {
			t.Fatal("fixture must be an unverified active paid user")
		}
		token := strings.Repeat("r", 48)
		sum := sha256.Sum256([]byte(token))
		_ = a.Store.Update(func(s *State) error {
			s.Settings["freeToolsRequireVerifiedEmail"] = true
			s.Settings["maintenance"] = maintenance
			record, _ := LoadDoc[ExecutorRecord](s, "executors", "executor-test")
			record.TokenHash = hex.EncodeToString(sum[:])
			return SaveDoc(s, "executors", record.ID, record)
		})
		probeResponse := httptest.NewRecorder()
		probeFinished := make(chan struct{})
		go func() {
			defer close(probeFinished)
			service.fingerprint(probeResponse, taskRequest(t, user, "POST", "/api/fingerprints", "", map[string]any{"host": "8.8.8.8", "port": 22}))
		}()
		// Exercise the real delivery endpoint rather than only a gate helper.
		next := httptest.NewRequest("GET", "/api/executor/next", nil)
		next.Header.Set("Authorization", "Bearer "+token)
		delivery := httptest.NewRecorder()
		service.next(delivery, next)
		if delivery.Code != 200 {
			t.Fatalf("maintenance=%v failed to deliver read-only probe: %d %s", maintenance, delivery.Code, delivery.Body.String())
		}
		var job executor.Job
		_ = json.Unmarshal(delivery.Body.Bytes(), &job)
		if job.Request.Kind != "fingerprint" || job.Request.SSH.Password != "" {
			t.Fatal("probe carried credentials or execution operation")
		}
		result := executor.Result{ID: job.ID, Lease: job.Lease, State: "succeeded", Fingerprint: taskSSHFixture().Fingerprint, Algorithm: "ssh-ed25519"}
		body, _ := json.Marshal(result)
		r := httptest.NewRequest("POST", "/api/executor/result", bytes.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		ack := httptest.NewRecorder()
		service.result(ack, r)
		if ack.Code != 200 {
			t.Fatal(ack.Body.String())
		}
		select {
		case <-probeFinished:
		case <-time.After(time.Second):
			t.Fatal("probe response did not complete")
		}
		if probeResponse.Code != 200 || !strings.Contains(probeResponse.Body.String(), `"authenticationChecked":false`) {
			t.Fatalf("read-only probe blocked: %d %s", probeResponse.Code, probeResponse.Body.String())
		}
		// Free execution remains blocked despite obtaining a valid public key.
		denied := httptest.NewRecorder()
		service.create(denied, taskRequest(t, user, "POST", "/api/tasks", "read-only-probe-guard", executor.Request{Kind: "deploy", Mode: "repair", SSH: taskSSHFixture()}))
		if denied.Code != 403 {
			t.Fatal("read-only probe bypassed execution gate")
		}
	}
}

func TestSSHTrustPersistsAndRequiresExplicitChangedKey(t *testing.T) {
	a, service, user := taskFixture(t)
	connection := taskSSHFixture()
	connection.TrustMode = "tofu"
	w := httptest.NewRecorder()
	service.create(w, taskRequest(t, user, "POST", "/api/tasks", "tofu-first", executor.Request{Kind: "deploy", Mode: "repair", SSH: connection}))
	if w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	_ = a.Store.View(func(s *State) error {
		record, ok := LoadDoc[sshTrust](s, "ssh_trust", sshTrustID(user.ID, connection))
		if !ok || record.Fingerprint != connection.Fingerprint {
			t.Fatal("host trust not stored")
		}
		return nil
	})
	// A new task service simulates loss of in-memory credentials/probe cache.
	restarted := NewTaskService(a)
	old := connection.Fingerprint
	connection.Fingerprint = "SHA256:" + strings.Repeat("B", 43)
	restarted.probes[taskProbeKey(user.ID, connection)] = taskProbe{Fingerprint: connection.Fingerprint, Expires: time.Now().Add(time.Minute)}
	w = httptest.NewRecorder()
	restarted.create(w, taskRequest(t, user, "POST", "/api/tasks", "tofu-changed", executor.Request{Kind: "deploy", Mode: "repair", SSH: connection}))
	if w.Code != 409 || !strings.Contains(w.Body.String(), "指纹已变化") {
		t.Fatalf("changed key not blocked %d %s", w.Code, w.Body.String())
	}
	connection.ReplaceFingerprint = old
	w = httptest.NewRecorder()
	restarted.create(w, taskRequest(t, user, "POST", "/api/tasks", "tofu-confirmed", executor.Request{Kind: "deploy", Mode: "repair", SSH: connection}))
	if w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	_ = a.Store.View(func(s *State) error {
		record, _ := LoadDoc[sshTrust](s, "ssh_trust", sshTrustID(user.ID, connection))
		if record.Fingerprint != connection.Fingerprint {
			t.Fatal("confirmed key not replaced")
		}
		if _, ok := LoadDoc[sshTrust](s, "ssh_trust", sshTrustID("other-user", connection)); ok {
			t.Fatal("trust leaked to another user")
		}
		return nil
	})
}

func TestCleanupRequiresMatchingFreshSingleUsePreview(t *testing.T) {
	for _, change := range []string{"valid", "foreign", "expired", "host", "fingerprint", "scope", "digest", "used"} {
		t.Run(change, func(t *testing.T) {
			a, service, user := taskFixture(t)
			connection := taskSSHFixture()
			preview := Task{ID: "preview", UserID: user.ID, Kind: "cleanup-preview", Host: connection.Host, SSHPort: connection.Port, SSHFingerprint: connection.Fingerprint, State: "succeeded", UpdatedAt: time.Now().UnixMilli(), Cleanup: &executor.CleanupReport{Scope: "relay", Digest: strings.Repeat("a", 64)}}
			request := executor.Request{Kind: "cleanup", SSH: connection, Cleanup: &executor.CleanupOptions{Scope: "relay", PreviewID: preview.ID, Digest: preview.Cleanup.Digest, Confirm: true}}
			switch change {
			case "foreign":
				preview.UserID = "another"
			case "expired":
				preview.UpdatedAt = time.Now().Add(-11 * time.Minute).UnixMilli()
			case "host":
				preview.Host = "1.1.1.1"
			case "fingerprint":
				preview.SSHFingerprint = "SHA256:" + strings.Repeat("B", 43)
			case "scope":
				preview.Cleanup.Scope = "msboost"
			case "digest":
				preview.Cleanup.Digest = strings.Repeat("b", 64)
			}
			_ = a.Store.Update(func(s *State) error {
				if change == "used" {
					_ = SaveDoc(s, "cleanup_consumed", preview.ID, taskIdempotency{TaskID: "old-cleanup"})
				}
				return SaveDoc(s, "tasks", preview.ID, preview)
			})
			w := httptest.NewRecorder()
			service.create(w, taskRequest(t, user, "POST", "/api/tasks", "cleanup-test-1", request))
			if change == "valid" {
				if w.Code != 202 {
					t.Fatal(w.Body.String())
				}
				var job Task
				_ = json.Unmarshal(w.Body.Bytes(), &job)
				service.mu.Lock()
				delete(service.envelopes, job.ID)
				service.mu.Unlock()
				w = httptest.NewRecorder()
				service.create(w, taskRequest(t, user, "POST", "/api/tasks", "cleanup-test-2", request))
				if w.Code != 409 || !strings.Contains(w.Body.String(), "不能再次执行") {
					t.Fatal("preview replay accepted")
				}
			} else if w.Code != 409 {
				t.Fatalf("unsafe preview accepted %s %d", change, w.Code)
			}
		})
	}
}

func TestExecutorIPUsesTrustedProxyBoundary(t *testing.T) {
	a, service, user := taskFixture(t)
	_ = a.Store.Update(func(s *State) error {
		record, _ := LoadDoc[ExecutorRecord](s, "executors", "executor-test")
		record.IP = "8.8.8.8"
		return SaveDoc(s, "executors", record.ID, record)
	})
	admin := *user
	admin.Role = "admin"
	w := httptest.NewRecorder()
	service.listExecutors(w, taskRequest(t, &admin, "GET", "/api/admin/executors", "", nil))
	var response struct {
		Executors []ExecutorRecord `json:"executors"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &response)
	if len(response.Executors) != 1 || !response.Executors[0].Online || response.Executors[0].IP != "8.8.8.8" || response.Executors[0].TokenHash != "" {
		t.Fatal(w.Body.String())
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "8.8.4.4:12345"
	r.Header.Set("X-Forwarded-For", "1.1.1.1")
	if a.clientIP(r) != "8.8.4.4" {
		t.Fatal("untrusted proxy IP accepted")
	}
}
