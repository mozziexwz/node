package control

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func contentFixture(t *testing.T) (*App, *http.ServeMux, *User, *User) {
	t.Helper()
	a, err := New(Config{DataDir: t.TempDir(), MasterKey: strings.Repeat("23", 32), AdminEmail: "22334455@qq.com", AdminPassword: "Content-tests-Password-123!"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	admin := &User{ID: "content-admin", Email: "22334455@qq.com", Role: "admin", Status: "active"}
	member := &User{ID: "content-member", Email: "33445566@qq.com", Role: "user", Status: "active"}
	err = a.Store.Update(func(s *State) error {
		s.Users[admin.ID] = admin
		s.Users[member.ID] = member
		s.Settings["publicArticles"] = true
		s.Settings["attachments"] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	a.RegisterContent(mux)
	return a, mux, admin, member
}
func contentCall(t *testing.T, mux *http.ServeMux, user *User, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(raw))
	r.Header.Set("Content-Type", "application/json")
	if user != nil {
		r = r.WithContext(context.WithValue(r.Context(), authContextKey{}, &requestIdentity{user: user}))
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}
func contentArticle(t *testing.T, a *App, id string, published bool) {
	t.Helper()
	if err := a.Store.Update(func(s *State) error {
		return SaveDoc(s, "articles", id, Article{ID: id, Title: "Article " + id, Body: "Markdown content", Published: published, Attachments: []Attachment{}})
	}); err != nil {
		t.Fatal(err)
	}
}
func contentUpload(t *testing.T, mux *http.ServeMux, user *User, id, name string, data []byte, extra bool) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	file, err := writer.CreateFormFile("file", name)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.Write(data)
	if extra {
		file, _ = writer.CreateFormFile("file", "extra.txt")
		_, _ = file.Write([]byte("extra"))
	}
	_ = writer.Close()
	r := httptest.NewRequest("POST", "/api/admin/articles/"+id+"/attachments", &buf)
	r.Header.Set("Content-Type", writer.FormDataContentType())
	if user != nil {
		r = r.WithContext(context.WithValue(r.Context(), authContextKey{}, &requestIdentity{user: user}))
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}
func TestOverviewUsesRuntimeReleaseVersion(t *testing.T) {
	a, mux, admin, member := contentFixture(t)
	if a.Config.Version != "dev" {
		t.Fatal("unversioned development build must not claim a release")
	}
	a.Config.Version = "v9.8.7-test"
	w := contentCall(t, mux, admin, "GET", "/api/admin/overview", nil)
	var body map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil || body["version"] != a.Config.Version {
		t.Fatal("overview does not use the actual server build version")
	}
	if denied := contentCall(t, mux, member, "GET", "/api/admin/overview", nil); denied.Code != 403 {
		t.Fatal("version reporting bypassed admin authorization")
	}
}

func TestContentArticleVisibilityAndAdminMutation(t *testing.T) {
	a, mux, admin, member := contentFixture(t)
	contentArticle(t, a, "visible", true)
	contentArticle(t, a, "draft", false)
	for _, test := range []struct {
		user  *User
		path  string
		want  int
		count int
	}{{nil, "/api/articles", 200, 1}, {member, "/api/articles", 200, 1}, {admin, "/api/admin/articles", 200, 2}, {member, "/api/admin/articles", 403, 0}} {
		w := contentCall(t, mux, test.user, "GET", test.path, nil)
		if w.Code != test.want {
			t.Fatalf("%s: %d %s", test.path, w.Code, w.Body.String())
		}
		if test.count > 0 {
			var data struct {
				Articles []Article `json:"articles"`
			}
			_ = json.Unmarshal(w.Body.Bytes(), &data)
			if len(data.Articles) != test.count {
				t.Fatal("draft was exposed or missing")
			}
		}
	}
	w := contentCall(t, mux, member, "DELETE", "/api/admin/articles/visible", nil)
	if w.Code != 403 {
		t.Fatal("ordinary member deleted article")
	}
	_ = a.Store.Update(func(s *State) error { s.Settings["publicArticles"] = false; return nil })
	w = contentCall(t, mux, nil, "GET", "/api/articles", nil)
	if w.Code != 401 {
		t.Fatal("anonymous article gate bypassed")
	}
}
func TestContentAttachmentLimitAndAssociation(t *testing.T) {
	a, mux, admin, member := contentFixture(t)
	contentArticle(t, a, "files", true)
	contentArticle(t, a, "other", true)
	if w := contentUpload(t, mux, member, "files", "test.txt", []byte("private"), false); w.Code != 403 {
		t.Fatal("member uploaded administrative attachment")
	}
	if w := contentUpload(t, mux, admin, "files", "test.html", []byte("<script>alert(1)</script>"), false); w.Code != 400 {
		t.Fatal("disallowed extension accepted")
	}
	if w := contentUpload(t, mux, admin, "files", "test.txt", []byte("file"), true); w.Code != 400 {
		t.Fatal("multiple uploads silently accepted")
	}
	data := bytes.Repeat([]byte("a"), attachmentMaxBytes)
	w := contentUpload(t, mux, admin, "files", "boundary.txt", data, false)
	if w.Code != 201 {
		t.Fatalf("exact 10 MiB file was rejected: %d %s", w.Code, w.Body.String())
	}
	var attached Attachment
	_ = json.Unmarshal(w.Body.Bytes(), &attached)
	if attached.Size != attachmentMaxBytes {
		t.Fatal("attachment was truncated")
	}
	w = contentCall(t, mux, nil, "GET", "/api/articles/files/attachments/"+attached.ID, nil)
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), data) {
		t.Fatal("download bytes changed")
	}
	if w.Header().Get("Content-Type") != "application/octet-stream" || w.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment;") {
		t.Fatal("attachment could be rendered as same-origin executable content")
	}
	w = contentCall(t, mux, nil, "GET", "/api/articles/other/attachments/"+attached.ID, nil)
	if w.Code != 403 {
		t.Fatal("attachment was accessible through unrelated article")
	}
	w = contentUpload(t, mux, admin, "files", "too-large.txt", append(data, byte('x')), false)
	if w.Code != 413 {
		t.Fatalf("over-limit file: %d", w.Code)
	}
	w = contentCall(t, mux, admin, "DELETE", "/api/admin/articles/files/attachments/missing", nil)
	if w.Code != 404 {
		t.Fatal("nonexistent attachment reported deletion success")
	}
	w = contentCall(t, mux, admin, "DELETE", "/api/admin/articles/files/attachments/"+attached.ID, nil)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = contentCall(t, mux, nil, "GET", "/api/articles/files/attachments/"+attached.ID, nil)
	if w.Code != 403 {
		t.Fatal("deleted attachment still downloadable")
	}
}
func TestContentDeletePreservesForeignAttachment(t *testing.T) {
	a, mux, admin, _ := contentFixture(t)
	f := Attachment{ID: "foreign-file", Name: "file.txt", Type: "text/plain", Size: 4}
	_ = a.Store.Update(func(s *State) error {
		_ = SaveDoc(s, "articles", "a", Article{ID: "a", Title: "A", Published: true, Attachments: []Attachment{f}})
		_ = SaveDoc(s, "articles", "b", Article{ID: "b", Title: "B", Published: true, Attachments: []Attachment{f}})
		return SaveDoc(s, "attachments", f.ID, attachmentObject{ArticleID: "b", Metadata: f, Bytes: []byte("data")})
	})
	w := contentCall(t, mux, admin, "DELETE", "/api/admin/articles/a/attachments/foreign-file", nil)
	if w.Code != 404 {
		t.Fatal("cross-article attachment deletion accepted")
	}
	w = contentCall(t, mux, admin, "DELETE", "/api/admin/articles/a", nil)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = contentCall(t, mux, nil, "GET", "/api/articles/b/attachments/foreign-file", nil)
	if w.Code != 200 || w.Body.String() != "data" {
		t.Fatal("deleting stale article reference removed another article's file")
	}
}
func TestContentTicketOwnershipRateAndSizeBound(t *testing.T) {
	a, mux, _, member := contentFixture(t)
	w := contentCall(t, mux, member, "POST", "/api/tickets", map[string]string{"title": "Help", "body": "Please investigate"})
	if w.Code != 201 {
		t.Fatal(w.Body.String())
	}
	var ticket Ticket
	_ = json.Unmarshal(w.Body.Bytes(), &ticket)
	other := &User{ID: "other-user", Role: "user", Status: "active"}
	w = contentCall(t, mux, other, "POST", "/api/tickets/"+ticket.ID+"/replies", map[string]string{"body": "not mine"})
	if w.Code != 409 {
		t.Fatal("ticket ownership bypassed")
	}
	for i := 0; i < 21; i++ {
		w = contentCall(t, mux, member, "POST", "/api/tickets/"+ticket.ID+"/replies", map[string]string{"body": "bounded reply"})
		expected := 200
		if i == 20 {
			expected = 429
		}
		if w.Code != expected {
			t.Fatalf("reply %d: %d %s", i, w.Code, w.Body.String())
		}
	}
	_ = a.Store.Update(func(s *State) error {
		v, _ := LoadDoc[Ticket](s, "tickets", ticket.ID)
		if len(v.Replies) != 21 {
			t.Fatalf("unexpected persisted reply count %d", len(v.Replies))
		}
		return nil
	})
}
func TestContentTicketCannotGrowWithoutBound(t *testing.T) {
	a, mux, admin, member := contentFixture(t)
	ticket := Ticket{ID: "full-ticket", UserID: member.ID, Status: "open", Replies: make([]TicketReply, ticketMaxMessages)}
	_ = a.Store.Update(func(s *State) error { return SaveDoc(s, "tickets", ticket.ID, ticket) })
	w := contentCall(t, mux, admin, "POST", "/api/tickets/full-ticket/replies", map[string]string{"body": "too many"})
	if w.Code != 409 {
		t.Fatal("unbounded reply array accepted")
	}
	w = contentCall(t, mux, member, "POST", "/api/tickets", map[string]string{"title": "   ", "body": "message"})
	if w.Code != 400 {
		t.Fatal("empty title accepted")
	}
}

func TestContentArticleMoveAtomicAndStaleEditPreservesOrder(t *testing.T) {
	a, mux, admin, member := contentFixture(t)
	for i, id := range []string{"a", "b", "c"} {
		if err := a.Store.Update(func(s *State) error {
			return SaveDoc(s, "articles", id, Article{ID: id, Title: id, Sort: i, Published: true, Body: "original"})
		}); err != nil {
			t.Fatal(err)
		}
	}
	if w := contentCall(t, mux, member, "POST", "/api/admin/articles/b/move", map[string]string{"direction": "up"}); w.Code != 403 {
		t.Fatal("member reordered articles")
	}
	w := contentCall(t, mux, admin, "POST", "/api/admin/articles/b/move", map[string]string{"direction": "up"})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = contentCall(t, mux, admin, "PUT", "/api/admin/articles/b", Article{Title: "updated title", Sort: 99, Published: true, Body: "new body"})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = contentCall(t, mux, nil, "GET", "/api/articles", nil)
	var out struct {
		Articles []Article `json:"articles"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Articles) != 3 || out.Articles[0].ID != "b" || out.Articles[1].ID != "a" || out.Articles[0].Sort != 0 || out.Articles[0].Body != "new body" {
		t.Fatalf("stale save overwrote order: %+v", out.Articles)
	}
	if w = contentCall(t, mux, admin, "POST", "/api/admin/articles/b/move", map[string]string{"direction": "up"}); w.Code != 200 {
		t.Fatal("first row boundary should be a safe no-op")
	}
	if w = contentCall(t, mux, admin, "POST", "/api/admin/articles/missing/move", map[string]string{"direction": "down"}); w.Code != 409 {
		t.Fatal("missing article moved")
	}
}

func TestContentConcurrentArticleMovesRetainPermutation(t *testing.T) {
	a, mux, admin, _ := contentFixture(t)
	for i := 0; i < 8; i++ {
		id := fmt.Sprintf("article-%d", i)
		if err := a.Store.Update(func(s *State) error {
			return SaveDoc(s, "articles", id, Article{ID: id, Title: id, Sort: i, Body: "unchanged"})
		}); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			direction := "up"
			if n%2 == 0 {
				direction = "down"
			}
			w := contentCall(t, mux, admin, "POST", fmt.Sprintf("/api/admin/articles/article-%d/move", n%8), map[string]string{"direction": direction})
			if w.Code != 200 {
				t.Errorf("move: %d %s", w.Code, w.Body.String())
			}
		}(i)
	}
	wg.Wait()
	if err := a.Store.View(func(s *State) error {
		items := ListDocs[Article](s, "articles")
		sortArticles(items)
		if len(items) != 8 {
			t.Fatal("reorder lost articles")
		}
		for i, item := range items {
			if item.Sort != i || item.Body != "unchanged" {
				t.Fatalf("invalid final permutation: %+v", items)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestContentTicketPriorityAndReplyMetadataMatchBothRoles(t *testing.T) {
	a, mux, admin, member := contentFixture(t)
	fixtures := []Ticket{
		{ID: "admin-old", UserID: member.ID, Status: "open", CreatedAt: 1, Replies: []TicketReply{{CreatedAt: 1}, {CreatedAt: 20, Admin: true, Author: admin.Email}}},
		{ID: "admin-new", UserID: member.ID, Status: "open", CreatedAt: 1, Replies: []TicketReply{{CreatedAt: 1}, {CreatedAt: 30, Admin: true, Author: admin.Email}}},
		{ID: "user-old", UserID: member.ID, Status: "open", CreatedAt: 2, Replies: []TicketReply{{CreatedAt: 2}, {CreatedAt: 300, Admin: true}, {CreatedAt: 400, Author: member.Email}}},
		{ID: "user-new", UserID: member.ID, Status: "open", CreatedAt: 2, Replies: []TicketReply{{CreatedAt: 2}, {CreatedAt: 500, Author: member.Email}}},
		{ID: "new", UserID: member.ID, Status: "open", CreatedAt: 2000, Replies: []TicketReply{{CreatedAt: 2000, Author: member.Email}}},
		{ID: "closed", UserID: member.ID, Status: "closed", CreatedAt: 5000, UpdatedAt: 9999, Replies: []TicketReply{{CreatedAt: 5000}, {CreatedAt: 6000, Admin: true}}},
	}
	if err := a.Store.Update(func(s *State) error {
		for _, v := range fixtures {
			if err := SaveDoc(s, "tickets", v.ID, v); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{"admin-new", "admin-old", "user-new", "user-old", "new", "closed"}
	for _, actor := range []*User{admin, member} {
		w := contentCall(t, mux, actor, "GET", "/api/tickets", nil)
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
		var out struct {
			Tickets []Ticket `json:"tickets"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if len(out.Tickets) != len(want) {
			t.Fatal("ticket list missing records")
		}
		for i, id := range want {
			if out.Tickets[i].ID != id {
				t.Fatalf("role %s index %d: got %s expected %s", actor.Role, i, out.Tickets[i].ID, id)
			}
		}
		if out.Tickets[0].LastReplyRole != "admin" || out.Tickets[0].LastReplyAuthor != admin.Email || out.Tickets[0].LastReplyAt != 30 || out.Tickets[0].CreatedAt != 1 {
			t.Fatal("administrator reply metadata inaccurate")
		}
		if out.Tickets[3].LastReplyRole != "user" || out.Tickets[3].ReplyCount != 2 {
			t.Fatal("latest user reply did not override historical admin reply")
		}
		if out.Tickets[5].LastActivityAt != 9999 || out.Tickets[5].LastReplyAt != 6000 {
			t.Fatal("close activity and last reply timestamps conflated")
		}
	}
}
