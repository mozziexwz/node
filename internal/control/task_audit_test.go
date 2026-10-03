package control

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

func taskAuditFixture(t *testing.T) (*App, *TaskService, *User, *User) {
	t.Helper()
	store, err := openStore(Config{DatabaseURL: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	app := &App{Store: store}
	service := &TaskService{app: app, configs: map[string]taskConfig{}}
	admin := &User{ID: "audit-admin", Email: "admin@example.invalid", Role: "admin", Status: "active"}
	alpha := &User{ID: "operator-alpha", Email: "Alpha.User@example.invalid", Role: "user", Status: "active", PasswordHash: "synthetic-private-account-hash"}
	if err := store.Update(func(s *State) error {
		s.Users[admin.ID] = admin
		s.Users[alpha.ID] = alpha
		s.Users["operator-beta"] = &User{ID: "operator-beta", Email: "beta@example.invalid", Role: "user", Status: "disabled"}
		s.Users["operator-blank"] = &User{ID: "operator-blank", Email: " ", Role: "user", Status: "active"}
		for _, task := range []Task{
			{ID: "deploy-alpha", UserID: alpha.ID, Kind: "deploy", Host: "192.0.2.10", State: "failed", Remark: "西区 Summer 线路", CreatedAt: 10, ConfigAvailable: true},
			{ID: "relay-alpha", UserID: alpha.ID, Kind: "relay", Host: "192.0.2.20", State: "succeeded", CreatedAt: 20},
			{ID: "dd-beta", UserID: "operator-beta", Kind: "dd", Host: "198.51.100.30", State: "executed", CreatedAt: 30},
			{ID: "fingerprint-beta", UserID: "operator-beta", Kind: "fingerprint", Host: "198.51.100.40", State: "succeeded", CreatedAt: 40},
			{ID: "front-beta", UserID: "operator-beta", Kind: "front", Host: "198.51.100.50", State: "queued", CreatedAt: 50},
			{ID: "preview-alpha", UserID: alpha.ID, Kind: "cleanup-preview", Host: "192.0.2.60", State: "running", CreatedAt: 60},
			{ID: "cleanup-deleted", UserID: "historical-operator", Kind: "cleanup", Host: "203.0.113.70", State: "interrupted", CreatedAt: 70},
			{ID: "admin-deploy", UserID: admin.ID, Kind: "deploy", Host: "203.0.113.80", State: "succeeded", CreatedAt: 80},
			{ID: "blank-relay", UserID: "operator-blank", Kind: "relay", Host: "203.0.113.90", State: "succeeded", CreatedAt: 90},
		} {
			if err := SaveDoc(s, "tasks", task.ID, task); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return app, service, admin, alpha
}

func taskAuditGet(t *testing.T, service *TaskService, user *User, query, kind string) *httptest.ResponseRecorder {
	t.Helper()
	values := url.Values{"q": {query}, "kind": {kind}}
	w := httptest.NewRecorder()
	service.adminTasks(w, taskRequest(t, user, "GET", "/api/admin/tasks?"+values.Encode(), "", nil))
	return w
}

func taskAuditRows(t *testing.T, w *httptest.ResponseRecorder) []adminTask {
	t.Helper()
	if w.Code != 200 {
		t.Fatalf("audit status %d: %s", w.Code, w.Body.String())
	}
	var response struct {
		Tasks []adminTask `json:"tasks"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Tasks == nil {
		t.Fatal("tasks must be a JSON array, including an empty result")
	}
	return response.Tasks
}

func TestAdminTaskAuditOperatorNamesAndReadOnlyHistory(t *testing.T) {
	app, service, admin, _ := taskAuditFixture(t)
	before := map[string][]byte{}
	if err := app.Store.View(func(s *State) error {
		for id, raw := range s.Docs["tasks"] {
			before[id] = append([]byte(nil), raw...)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rows := taskAuditRows(t, taskAuditGet(t, service, admin, "", ""))
	names := map[string]string{}
	for _, row := range rows {
		names[row.ID] = row.UserName
	}
	for id, want := range map[string]string{
		"deploy-alpha": "Alpha.User@example.invalid", "dd-beta": "beta@example.invalid",
		"cleanup-deleted": "已删除用户", "blank-relay": "未设置邮箱", "admin-deploy": admin.Email,
	} {
		if names[id] != want {
			t.Errorf("%s operator = %q; want %q", id, names[id], want)
		}
	}
	if err := app.Store.Update(func(s *State) error {
		s.Users["operator-alpha"].Email = "renamed@example.invalid"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rows = taskAuditRows(t, taskAuditGet(t, service, admin, "renamed@example.invalid", "deploy"))
	if len(rows) != 1 || rows[0].UserName != "renamed@example.invalid" || rows[0].UserID != "operator-alpha" {
		t.Fatalf("operator name did not follow current account: %+v", rows)
	}
	if err := app.Store.View(func(s *State) error {
		for id, raw := range s.Docs["tasks"] {
			if !bytes.Equal(before[id], raw) || bytes.Contains(raw, []byte("userName")) {
				t.Errorf("audit rewrote historical task %s", id)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAdminTaskAuditSearchAndKindIntersection(t *testing.T) {
	_, service, admin, _ := taskAuditFixture(t)
	for _, test := range []struct {
		name, query, kind string
		want              []string
	}{
		{"all newest first", "", "", []string{"blank-relay", "admin-deploy", "cleanup-deleted", "preview-alpha", "front-beta", "fingerprint-beta", "dd-beta", "relay-alpha", "deploy-alpha"}},
		{"trim and fold email", "  ALPHA.USER@EXAMPLE.INVALID  ", "", []string{"preview-alpha", "relay-alpha", "deploy-alpha"}},
		{"operator id", "OPERATOR-ALPHA", "", []string{"preview-alpha", "relay-alpha", "deploy-alpha"}},
		{"task id", "DEPLOY-ALPHA", "", []string{"deploy-alpha"}},
		{"host", "192.0.2.20", "", []string{"relay-alpha"}},
		{"unicode remark", "西区 summer", "", []string{"deploy-alpha"}},
		{"state", "FAILED", "", []string{"deploy-alpha"}},
		{"raw kind search", "CLEANUP", "", []string{"cleanup-deleted", "preview-alpha"}},
		{"deleted operator", "已删除用户", "", []string{"cleanup-deleted"}},
		{"deleted operator id", "historical-operator", "cleanup", []string{"cleanup-deleted"}},
		{"empty operator email", "未设置邮箱", "", []string{"blank-relay"}},
		{"email with exact kind", "alpha", "relay", []string{"relay-alpha"}},
		{"trim kind", "", " relay ", []string{"blank-relay", "relay-alpha"}},
		{"no intersection", "alpha", "cleanup", []string{}},
		{"no search match", "missing-search-value", "", []string{}},
		{"deploy", "", "deploy", []string{"admin-deploy", "deploy-alpha"}},
		{"dd", "", "dd", []string{"dd-beta"}},
		{"fingerprint", "", "fingerprint", []string{"fingerprint-beta"}},
		{"front", "", "front", []string{"front-beta"}},
		{"preview", "", "cleanup-preview", []string{"preview-alpha"}},
		{"cleanup", "", "cleanup", []string{"cleanup-deleted"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			rows := taskAuditRows(t, taskAuditGet(t, service, admin, test.query, test.kind))
			ids := []string{}
			for _, row := range rows {
				ids = append(ids, row.ID)
			}
			if !reflect.DeepEqual(ids, test.want) {
				t.Fatalf("got %v; want %v", ids, test.want)
			}
		})
	}
	for _, kind := range []string{"unknown", "DEPLOY", "all", "relay,dd"} {
		w := taskAuditGet(t, service, admin, "", kind)
		if w.Code != 400 || strings.Contains(w.Body.String(), "tasks") {
			t.Fatalf("invalid kind %q: %d %s", kind, w.Code, w.Body.String())
		}
	}
}

func TestAdminTaskAuditPermissionBeforeQueryAndRecordRead(t *testing.T) {
	app, service, admin, member := taskAuditFixture(t)
	// Closing this in-memory store makes a premature audit read fail. Cached
	// request identities still allow testing the permission boundary itself.
	if err := app.Store.Close(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/admin/tasks", "/api/admin/tasks?kind=unknown&q=operator-alpha"} {
		for _, user := range []*User{nil, member} {
			request := httptest.NewRequest(http.MethodGet, path, nil)
			if user != nil {
				request = taskRequest(t, user, "GET", path, "", nil)
			}
			w := httptest.NewRecorder()
			service.adminTasks(w, request)
			if w.Code != 403 || strings.Contains(w.Body.String(), "operator-alpha") {
				t.Fatalf("permission boundary: %d %s", w.Code, w.Body.String())
			}
		}
	}
	if w := taskAuditGet(t, service, admin, "", "unknown"); w.Code != 400 {
		t.Fatalf("authorized invalid query read records: %d %s", w.Code, w.Body.String())
	}
	if w := taskAuditGet(t, service, admin, "", ""); w.Code != 500 || strings.Contains(w.Body.String(), "database") {
		t.Fatalf("record read error was not handled safely: %d %s", w.Code, w.Body.String())
	}
}

func TestAdminTaskAuditProtectsConfigurationAndMemberAPI(t *testing.T) {
	_, service, admin, member := taskAuditFixture(t)
	secret := []byte(`{"password":"synthetic-private-task-config"}`)
	service.configs["deploy-alpha"] = taskConfig{Data: secret, UserID: member.ID, Expires: time.Now().Add(time.Minute)}
	service.configs["admin-deploy"] = taskConfig{Data: []byte("synthetic-admin-config"), UserID: admin.ID, Expires: time.Now().Add(time.Minute)}
	w := taskAuditGet(t, service, admin, "", "")
	for _, row := range taskAuditRows(t, w) {
		if row.ConfigAvailable != (row.ID == "admin-deploy") {
			t.Errorf("configuration availability for %s = %v", row.ID, row.ConfigAvailable)
		}
	}
	for _, forbidden := range []string{"synthetic-private-task-config", "synthetic-private-account-hash", "passwordHash", "synthetic-admin-config"} {
		if strings.Contains(w.Body.String(), forbidden) {
			t.Errorf("audit disclosed %s", forbidden)
		}
		if rows := taskAuditRows(t, taskAuditGet(t, service, admin, forbidden, "")); len(rows) != 0 {
			t.Errorf("search read private content %q", forbidden)
		}
	}
	for _, ownership := range []struct {
		user *User
		want int
	}{{admin, 404}, {member, 200}} {
		w := httptest.NewRecorder()
		request := taskRequest(t, ownership.user, "GET", "/api/tasks/deploy-alpha/config", "", nil)
		request.SetPathValue("id", "deploy-alpha")
		service.download(w, request)
		if w.Code != ownership.want || (ownership.want == 404 && bytes.Contains(w.Body.Bytes(), secret)) {
			t.Fatalf("configuration owner boundary: %d %s", w.Code, w.Body.String())
		}
	}
	for _, path := range []string{"/api/tasks?q=beta&kind=fingerprint", "/api/tasks/deploy-alpha"} {
		w := httptest.NewRecorder()
		request := taskRequest(t, member, "GET", path, "", nil)
		if strings.HasPrefix(path, "/api/tasks?") {
			service.list(w, request)
			var response struct{ Tasks []Task }
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if len(response.Tasks) != 3 {
				t.Fatalf("admin query parameters changed member list: %s", w.Body.String())
			}
			for _, row := range response.Tasks {
				if row.UserID != member.ID || row.Kind == "fingerprint" {
					t.Errorf("member list scope changed: %+v", row)
				}
			}
		} else {
			request.SetPathValue("id", "deploy-alpha")
			service.get(w, request)
		}
		if w.Code != 200 || strings.Contains(w.Body.String(), "userName") || bytes.Contains(w.Body.Bytes(), secret) {
			t.Fatalf("member task response changed or disclosed data: %d %s", w.Code, w.Body.String())
		}
	}
}
