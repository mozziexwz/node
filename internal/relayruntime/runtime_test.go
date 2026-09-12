package relayruntime

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestGostConfigConvertsMbpsAndRestrictsSource(t *testing.T) {
	raw, err := GostConfig(Rule{ID: "test-rule", ListenPort: 21000, Protocol: "tcp", RateMbps: 5, Targets: []string{"1.1.1.1:1234"}, AllowedSources: []string{"8.8.8.8"}, Strategy: "round"}, "http://127.0.0.1:8888/observer")
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err = json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "$ 625000B 625000B") {
		t.Fatal("5 Mbps must be 625000 B/s per direction")
	}
	admission := cfg["admissions"].([]any)[0].(map[string]any)
	if admission["whitelist"] != true {
		t.Fatal("upstream must use allowlist")
	}
	if strings.Contains(string(raw), "password") {
		t.Fatal("target credentials must not reach relay")
	}
}
func TestGostConfigRejectsInvalidRule(t *testing.T) {
	for _, rule := range []Rule{{Protocol: "udp", ListenPort: 20000, RateMbps: 5, Targets: []string{"1.1.1.1:80"}, Strategy: "round"}, {Protocol: "tcp", ListenPort: 0, RateMbps: 5, Targets: []string{"1.1.1.1:80"}, Strategy: "round"}, {Protocol: "tcp", ListenPort: 20000, RateMbps: 0, Targets: []string{"1.1.1.1:80"}, Strategy: "round"}, {Protocol: "tcp", ListenPort: 20000, RateMbps: 5, Targets: []string{"https://target"}, Strategy: "round"}} {
		if _, err := GostConfig(rule, ""); err == nil {
			t.Errorf("invalid rule accepted %+v", rule)
		}
	}
}
func TestObserverCumulativeDuplicateDoesNotAddBytes(t *testing.T) {
	s := &runtimeState{processes: map[string]*process{"rule": {epoch: "epoch", traffic: Traffic{ID: "rule", Epoch: "epoch"}}}, pending: map[string]Traffic{}, journal: filepath.Join(t.TempDir(), "journal.json"), observerToken: "token"}
	body := `{"events":[{"kind":"service","service":"rule","type":"stats","stats":{"inputBytes":100,"outputBytes":200,"currentConns":1}}]}`
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "http://127.0.0.1/observer?token=token&epoch=epoch", strings.NewReader(body))
		s.observer(w, r)
		if w.Code != 200 {
			t.Fatalf("observer %d %s", w.Code, w.Body.String())
		}
	}
	if s.pending["epoch"].InputBytes != 100 || s.pending["epoch"].OutputBytes != 200 {
		t.Fatal("cumulative stats were doubled")
	}
	w := httptest.NewRecorder()
	s.observer(w, httptest.NewRequest("POST", "http://127.0.0.1/observer?token=wrong", strings.NewReader(body)))
	if w.Code != 403 {
		t.Fatal("unauthenticated observer accepted")
	}
}
