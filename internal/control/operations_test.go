package control

import (
	"archive/tar"
	"bytes"
	"context"
	"github.com/mozziexwz/node/internal/disaster"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDisasterHistoryIsAdminOnly(t *testing.T) {
	a, _, member := commerceTestApp(t)
	m := http.NewServeMux()
	NewBackupService(a).Register(m)
	if r := commerceTestRequest(m, member, "GET", "/api/admin/disaster-backups", nil); r.Code != 403 {
		t.Fatal("member accessed operational history", r.Code)
	}
}

func TestReconnectPlanTransferIsPrivateAndIdempotent(t *testing.T) {
	f := newBackupSSHServer(t)
	sf := f.connect(t)
	if err := sf.MkdirAll("/recovery"); err != nil {
		t.Fatal(err)
	}
	if err := sf.Chmod("/recovery", 0700); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"token":"local-fixture-only"}`)
	if err := writeReconnectRemote(sf, "/recovery/plan.json", raw); err != nil {
		t.Fatal(err)
	}
	f.files.mu.Lock()
	writes := f.files.writes
	unsafe := f.files.unsafeWrite
	f.files.mu.Unlock()
	if unsafe {
		t.Fatal("plan sent before private permissions")
	}
	if err := writeReconnectRemote(sf, "/recovery/plan.json", raw); err != nil {
		t.Fatal(err)
	}
	f.files.mu.Lock()
	after := f.files.writes
	f.files.mu.Unlock()
	if after != writes {
		t.Fatal("retry rewrote already verified plan")
	}
	if err := writeReconnectRemote(sf, "/recovery/plan.json", []byte("different")); err == nil {
		t.Fatal("replaced different plan")
	}
	rows, err := sf.ReadDir("/recovery")
	if err != nil || len(rows) != 1 {
		t.Fatal("partial file leaked", err, rows)
	}
}

func TestRelayUpgradeChecksEvidenceWithoutChangingState(t *testing.T) {
	s := newState()
	a := RelayAgent{ID: "node", Name: "Node", Address: "8.8.8.8"}
	_ = SaveDoc(s, "relay_agents", a.ID, a)
	if len(legacyRelayUpgradeBlockers(s)) != 0 {
		t.Fatal("empty unregistered node blocked")
	}
	a.TokenHash = "hash"
	_ = SaveDoc(s, "relay_agents", a.ID, a)
	if len(legacyRelayUpgradeBlockers(s)) != 1 {
		t.Fatal("legacy credentials passed")
	}
	a.ProtocolVersion = 2
	a.OfflinePolicy = "keep_last"
	_ = SaveDoc(s, "relay_agents", a.ID, a)
	if len(legacyRelayUpgradeBlockers(s)) != 0 {
		t.Fatal("modern node blocked")
	}
	rule := UserRule{ID: "rule", Segments: []RelaySegment{{AgentID: a.ID, ProtocolVersion: 1}}}
	_ = SaveDoc(s, "user_rules", "user:route", rule)
	if len(legacyRelayUpgradeBlockers(s)) != 1 {
		t.Fatal("unproven legacy segment passed")
	}
}

func TestReconnectSelectionsRequireExplicitStopForDifferences(t *testing.T) {
	report := RelayRecoveryReport{Differences: []RelayRecoveryDifference{{RuleID: "same", CanAdopt: true}, {RuleID: "different"}}}
	calls := 0
	selected, err := reconnectSelections(report, func(d RelayRecoveryDifference) (string, error) {
		calls++
		if d.RuleID != "different" {
			t.Fatal("unexpected prompt")
		}
		return "stop", nil
	})
	if err != nil || calls != 1 || selected[0].Action != "adopt" || selected[1].Action != "stop" {
		t.Fatal(selected, err)
	}
	if _, err = reconnectSelections(report, func(RelayRecoveryDifference) (string, error) { return "hold", nil }); err == nil {
		t.Fatal("hold ignored")
	}
	report.MissingOnNode = []string{"unknown"}
	if _, err = reconnectSelections(report, func(RelayRecoveryDifference) (string, error) {
		t.Fatal("must not infer missing stop")
		return "stop", nil
	}); err == nil {
		t.Fatal("missing accepted")
	}
}

func TestOnlineKeyArchiveIsMinimalAndPreservesOwnership(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.tar")
	key := bytes.Repeat([]byte{42}, 32)
	if err := writeOnlineRecoveryKey(path, key); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tr := tar.NewReader(f)
	h, err := tr.Next()
	if err != nil || h.Name != "." || h.Uid != 10001 || h.Mode != 0700 {
		t.Fatal(h, err)
	}
	h, err = tr.Next()
	if err != nil || h.Name != "master.key" || h.Uid != 10001 || h.Mode != 0600 {
		t.Fatal(h, err)
	}
	data, _ := io.ReadAll(tr)
	if !bytes.Equal(data, key) {
		t.Fatal("key changed")
	}
	if _, err = tr.Next(); err != io.EOF {
		t.Fatal("unexpected historical files")
	}
	if err = writeOnlineRecoveryKey(path, key); err == nil {
		t.Fatal("overwrote existing snapshot")
	}
}

func TestDisasterFailureEmailOptInAndDedup(t *testing.T) {
	a, _, _ := commerceTestApp(t)
	b := NewBackupService(a)
	secret, _ := a.Seal([]byte("private-smtp"))
	_ = a.Store.Update(func(s *State) error {
		s.Settings["smtp"] = true
		s.Settings["smtpConfig"] = SMTPConfig{Secret: secret, TestedAt: 1}
		return nil
	})
	sent := 0
	a.mailSender = func(_ context.Context, _ SMTPConfig, p, recipient string, msg EmailMessage) error {
		if p != "private-smtp" || recipient != "123456789@qq.com" || strings.Contains(msg.TextBody, "sensitive-archive") {
			t.Fatal("secret/recipient leak")
		}
		sent++
		return nil
	}
	r := disaster.BackupRun{ID: "test", Stage: "remote_failed", LocalOK: true, Archive: "sensitive-archive", UpdatedAt: time.Now().UnixMilli()}
	b.notifyDisasterRun(context.Background(), r)
	if sent != 0 {
		t.Fatal("mail not opt-in")
	}
	_ = a.Store.Update(func(s *State) error { s.Settings["disasterBackupEmailEnabled"] = true; return nil })
	b.notifyDisasterRun(context.Background(), r)
	b.notifyDisasterRun(context.Background(), r)
	if sent != 1 {
		t.Fatal("missing/duplicate mail", sent)
	}
	r.Stage = "complete"
	r.UpdatedAt++
	b.notifyDisasterRun(context.Background(), r)
	if sent != 1 {
		t.Fatal("success mailed")
	}
}
