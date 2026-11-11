package control

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mozziexwz/node/internal/executor"
)

// A deployed node gets fail2ban with every executor in service on its
// allowlist. The website decides that, never the browser, and the list does
// not take part in the request's idempotency.
func TestDeployAndUpgradeCarryTheExecutorAllowlist(t *testing.T) {
	a, service, u := taskFixture(t)
	token := nodeExecutor(t, a, executor.NodeUpgradeCapability)
	if err := a.Store.Update(func(s *State) error {
		first, _ := LoadDoc[ExecutorRecord](s, "executors", "executor-test")
		first.IP = "8.8.8.8"
		if err := SaveDoc(s, "executors", first.ID, first); err != nil {
			return err
		}
		for id, record := range map[string]ExecutorRecord{
			"draining":   {Status: "draining", IP: "::ffff:8.8.4.4"},
			"ipv6":       {Status: "active", IP: "2606:4700:4700::1111"},
			"same-exit":  {Status: "active", IP: "8.8.8.8"},
			"internal":   {Status: "active", IP: "10.0.0.7"},
			"never-seen": {Status: "active"},
			"garbage":    {Status: "active", IP: "not an address"},
			"disabled":   {Status: "disabled", IP: "9.9.9.9"},
		} {
			record.ID, record.Name = id, id
			if err := SaveDoc(s, "executors", id, record); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := &executor.Fail2banOptions{Allow: []string{"2606:4700:4700::1111", "8.8.4.4", "8.8.8.8"}, ClientBits: 32}

	create := func(key string, request executor.Request) (*httptest.ResponseRecorder, Task) {
		t.Helper()
		service.mu.Lock()
		service.probes[taskProbeKey(u.ID, request.SSH)] = taskProbe{Fingerprint: request.SSH.Fingerprint, Expires: time.Now().Add(time.Hour)}
		service.mu.Unlock()
		w := httptest.NewRecorder()
		service.create(w, taskRequest(t, u, "POST", "/api/tasks", key, request))
		var job Task
		_ = json.Unmarshal(w.Body.Bytes(), &job)
		return w, job
	}
	server := func(host string) executor.SSH {
		ssh := taskSSHFixture()
		ssh.Host = host
		return ssh
	}
	carried := func(id string) *executor.Fail2banOptions {
		service.mu.Lock()
		defer service.mu.Unlock()
		return service.envelopes[id].Job.Request.Fail2ban
	}

	deploy := executor.Request{Kind: "deploy", Mode: "fresh", SSH: server("8.8.4.10")}
	w, deployJob := create("fail2ban-deploy", deploy)
	if w.Code != 202 || !reflect.DeepEqual(carried(deployJob.ID), want) {
		t.Fatalf("deploy: %d %s %+v", w.Code, w.Body.String(), carried(deployJob.ID))
	}
	w, upgradeJob := create("fail2ban-upgrade", executor.Request{Kind: "upgrade", SSH: server("8.8.4.11")})
	if w.Code != 202 || !reflect.DeepEqual(carried(upgradeJob.ID), want) {
		t.Fatalf("upgrade: %d %s %+v", w.Code, w.Body.String(), carried(upgradeJob.ID))
	}
	w, fetchJob := create("fail2ban-fetch", executor.Request{Kind: "fetch-config", SSH: server("8.8.4.12")})
	if w.Code != 202 || carried(fetchJob.ID) != nil {
		t.Fatalf("reading a configuration must not change the server: %d %+v", w.Code, carried(fetchJob.ID))
	}

	supplied := executor.Request{Kind: "deploy", Mode: "fresh", SSH: server("8.8.4.13"), Fail2ban: &executor.Fail2banOptions{Allow: []string{"1.1.1.1"}}}
	if w, _ = create("fail2ban-browser", supplied); w.Code != 400 || !strings.Contains(w.Body.String(), "SSH 防护由网站安排") {
		t.Fatalf("a browser chose the allowlist: %d %s", w.Code, w.Body.String())
	}

	// The same submission stays the same task after the executors change.
	if err := a.Store.Update(func(s *State) error {
		return SaveDoc(s, "executors", "added", ExecutorRecord{ID: "added", Name: "added", Status: "active", IP: "9.9.9.9"})
	}); err != nil {
		t.Fatal(err)
	}
	if w, again := create("fail2ban-deploy", deploy); w.Code != 200 || again.ID != deployJob.ID || !reflect.DeepEqual(carried(deployJob.ID), want) {
		t.Fatalf("replayed deploy: %d %s", w.Code, w.Body.String())
	}

	// Each task reaches the executor with its allowlist; what comes back is
	// kept only where it was asked for, and only from the known vocabulary.
	reports := map[string][2]string{"deploy": {"enabled", "enabled"}, "upgrade": {"rm -rf /", "unknown"}, "fetch-config": {"enabled", ""}}
	for range reports {
		poll := httptest.NewRequest(http.MethodGet, "/api/executor/next", nil)
		poll.Header.Set("Authorization", "Bearer "+token)
		poll.Header.Set(executor.NodeUpgradeHeader, "1")
		w = httptest.NewRecorder()
		service.next(w, poll)
		var job executor.Job
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &job) != nil {
			t.Fatalf("claim: %d %s", w.Code, w.Body.String())
		}
		kind := job.Request.Kind
		delivered := strings.Contains(w.Body.String(), `"fail2ban":{"allow":["2606:4700:4700::1111","8.8.4.4","8.8.8.8"],"clientBits":32}`)
		if delivered != (kind != "fetch-config") {
			t.Fatalf("%s delivered as %s", kind, w.Body.String())
		}
		result := executor.Result{ID: job.ID, Lease: job.Lease, State: "succeeded", Phase: "complete", Config: json.RawMessage(nodeTestConfig),
			Health: &executor.Health{Service: "running", LocalSelfTest: "passed", PublicTCP: "reachable", Game: "not_tested", BBR: "enabled", Fail2ban: reports[kind][0]}}
		data, _ := json.Marshal(result)
		r := httptest.NewRequest(http.MethodPost, "/api/executor/result", bytes.NewReader(data))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		w = httptest.NewRecorder()
		service.result(w, r)
		if w.Code != 200 {
			t.Fatalf("%s result refused: %d %s", kind, w.Code, w.Body.String())
		}
		_ = a.Store.View(func(s *State) error {
			stored, _ := LoadDoc[Task](s, "tasks", job.ID)
			if stored.State != "succeeded" || stored.Health == nil || stored.Health.Fail2ban != reports[kind][1] || stored.Health.BBR != "enabled" {
				t.Fatalf("%s stored %+v", kind, stored.Health)
			}
			return nil
		})
	}
}

func TestTaskHealthKeepsOnlyKnownSSHProtectionStates(t *testing.T) {
	for reported, want := range map[string]string{
		"":                "",
		"enabled":         "enabled",
		"existing":        "existing",
		"unavailable":     "unavailable",
		"review_required": "review_required",
		"partial":         "unknown",
		"<script>":        "unknown",
	} {
		if got := cleanTaskHealth(&executor.Health{Fail2ban: reported}).Fail2ban; got != want {
			t.Errorf("%q kept as %q, want %q", reported, got, want)
		}
	}
	// A result from an executor that predates the step has no such line.
	if raw, _ := json.Marshal(cleanTaskHealth(&executor.Health{Service: "running"})); strings.Contains(string(raw), "fail2ban") {
		t.Fatalf("health invented an SSH protection state: %s", raw)
	}
}
