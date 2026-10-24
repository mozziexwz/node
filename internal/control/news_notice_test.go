package control

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNewsNoticeValidation(t *testing.T) {
	valid := NewsNotice{Enabled: true, Title: " MSBOOST 本地版 v1.0.0 已发布 ", Body: "第一行\r\n**第二行**", LinkText: "去下载本地版", LinkHref: "/#local", Public: true}
	got, err := normalizeNewsNotice(valid)
	if err != nil || got.Title != "MSBOOST 本地版 v1.0.0 已发布" || got.Body != "第一行\n**第二行**" {
		t.Fatalf("normalized notice: %+v %v", got, err)
	}
	if off, err := normalizeNewsNotice(NewsNotice{}); err != nil || off.Enabled {
		t.Fatalf("an empty notice must stay off: %+v %v", off, err)
	}
	for name, mutate := range map[string]func(*NewsNotice){
		"nothing to show":   func(n *NewsNotice) { n.Title, n.Body = "", "" },
		"long title":        func(n *NewsNotice) { n.Title = strings.Repeat("字", 41) },
		"long body":         func(n *NewsNotice) { n.Body = strings.Repeat("字", 601) },
		"control character": func(n *NewsNotice) { n.Body = "a\u0000b" },
		"javascript link":   func(n *NewsNotice) { n.LinkHref = "javascript:alert(1)" },
		"protocol relative": func(n *NewsNotice) { n.LinkHref = "//evil.test" },
		"text without link": func(n *NewsNotice) { n.LinkHref = "" },
		"link without text": func(n *NewsNotice) { n.LinkText = "" },
	} {
		bad := valid
		mutate(&bad)
		if _, err := normalizeNewsNotice(bad); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

func TestNewsNoticeReachesOnlyItsReaders(t *testing.T) {
	a, mux, h, admin, member := publicFixture(t)
	a.RegisterIdentity(mux)
	_ = a.Store.Update(func(s *State) error { s.Settings["publicArticles"] = true; return nil })
	notices := func(path string, user *User) *NewsNotice {
		t.Helper()
		var out struct {
			Notice *NewsNotice `json:"notice"`
		}
		var body []byte
		if user == nil {
			body = publicGet(t, h, path).Body.Bytes()
		} else {
			body = contentCall(t, mux, user, "GET", path, nil).Body.Bytes()
		}
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("%s: %s", path, body)
		}
		return out.Notice
	}
	notice := map[string]any{"enabled": false, "title": "维护通知", "body": "今晚维护", "public": false}
	if w := contentCall(t, mux, member, "PUT", "/api/admin/settings", map[string]any{"newsNotice": notice}); w.Code != 403 {
		t.Fatalf("member changed the notice: %d", w.Code)
	}
	if w := contentCall(t, mux, admin, "PUT", "/api/admin/settings", map[string]any{"newsNotice": "x"}); w.Code != 400 || !strings.Contains(w.Body.String(), "页面公告格式无效") {
		t.Fatalf("malformed notice: %d %s", w.Code, w.Body.String())
	}
	if w := contentCall(t, mux, admin, "PUT", "/api/admin/settings", map[string]any{"newsNotice": notice}); w.Code != 200 {
		t.Fatalf("save notice: %d %s", w.Code, w.Body.String())
	}
	// A notice that is off stays with the administrator, who edits it.
	if got := notices("/api/admin/articles", admin); got == nil || got.Title != "维护通知" || got.UpdatedAt == 0 {
		t.Fatalf("administrator lost the notice that is off: %+v", got)
	}
	if notices("/api/articles", member) != nil || notices("/api/public/articles", nil) != nil {
		t.Fatal("a notice that is off was shown")
	}
	notice["enabled"] = true
	contentCall(t, mux, admin, "PUT", "/api/admin/settings", map[string]any{"newsNotice": notice})
	if got := notices("/api/articles", member); got == nil || got.Body != "今晚维护" {
		t.Fatalf("members miss the notice: %+v", got)
	}
	if notices("/api/public/articles", nil) != nil {
		t.Fatal("a members-only notice reached visitors")
	}
	notice["public"] = true
	contentCall(t, mux, admin, "PUT", "/api/admin/settings", map[string]any{"newsNotice": notice})
	if got := notices("/api/public/articles", nil); got == nil || got.Title != "维护通知" {
		t.Fatalf("visitors miss the public notice: %+v", got)
	}
}
