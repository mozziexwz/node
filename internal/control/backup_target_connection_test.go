package control

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBackupTargetConnectionTestGatesPlanAndInvalidatesChangedCredentials(t *testing.T) {
	b, server, target := backupUploadFixture(t)
	target.ID = "test-target"
	target.Name = "SFTP"
	target.Enabled = true
	var admin *User
	if err := b.app.Store.Update(func(s *State) error {
		for _, u := range s.Users {
			if u.Role == "admin" {
				admin = u
			}
		}
		return SaveDoc(s, "backup_targets", target.ID, target)
	}); err != nil {
		t.Fatal(err)
	}
	plan := BackupPlan{Enabled: true, Mode: "daily", Time: "02:30", Timezone: "Asia/Shanghai", RetentionDays: 3, MinCopies: 2, Targets: []string{target.ID}}
	w := httptest.NewRecorder()
	b.savePlan(w, taskRequest(t, admin, "PUT", "/api/admin/backup-plan", "", plan))
	if w.Code != 400 {
		t.Fatal("untested target enabled")
	}
	r := taskRequest(t, admin, "POST", "/api/admin/backup-targets/test-target/test", "", map[string]any{})
	r.SetPathValue("id", target.ID)
	w = httptest.NewRecorder()
	b.testTarget(w, r)
	if w.Code != 200 {
		t.Fatalf("probe: %d %s", w.Code, w.Body.String())
	}
	sf := server.connect(t)
	files, err := sf.ReadDir(target.Path)
	sf.Close()
	if err != nil || len(files) != 0 {
		t.Fatalf("probe files left: %v %v", files, err)
	}
	w = httptest.NewRecorder()
	b.savePlan(w, taskRequest(t, admin, "PUT", "/api/admin/backup-plan", "", plan))
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	// Identity fields or credentials invalidate the last successful test. Client
	// supplied verification flags cannot re-enable a target.
	target.Password = "wrong-backup-password"
	target.TestStatus = "passed"
	target.TestedAt = 1
	r = taskRequest(t, admin, "PUT", "/api/admin/backup-targets/test-target", "", target)
	r.SetPathValue("id", target.ID)
	w = httptest.NewRecorder()
	b.saveTarget(w, r)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	stored := backupStoredTarget(t, b.app, target.ID)
	if stored.TestStatus != "untested" || stored.TestedAt != 0 {
		t.Fatal("stale probe reused")
	}
	r = taskRequest(t, admin, "POST", "/api/admin/backup-targets/test-target/test", "", map[string]any{})
	r.SetPathValue("id", target.ID)
	w = httptest.NewRecorder()
	b.testTarget(w, r)
	if w.Code != 502 || strings.Contains(w.Body.String(), target.Password) {
		t.Fatal("failed authentication accepted or secret returned")
	}
	if stored = backupStoredTarget(t, b.app, target.ID); stored.TestStatus != "failed" {
		t.Fatal("failed test not recorded")
	}
}
