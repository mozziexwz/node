package control

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sync"
	"testing"
)

func articleManagementCreate(t *testing.T, mux *http.ServeMux, admin *User, title string, published bool) Article {
	t.Helper()
	w := contentCall(t, mux, admin, "POST", "/api/admin/articles", Article{Title: title, Category: "资讯", Body: "content", Published: published, Sort: -999, Pinned: true, CreatedAt: 1})
	var article Article
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &article) != nil {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	if article.Category != "资讯" || article.Pinned || article.CreatedAt <= 1 {
		t.Fatalf("invalid creation defaults: %+v", article)
	}
	return article
}

func articleManagementList(t *testing.T, mux *http.ServeMux, user *User) []Article {
	t.Helper()
	path := "/api/articles"
	if user != nil && user.Role == "admin" {
		path = "/api/admin/articles"
	}
	w := contentCall(t, mux, user, "GET", path, nil)
	var out struct {
		Articles []Article `json:"articles"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	return out.Articles
}

func articleManagementOrder(t *testing.T, mux *http.ServeMux, user *User, want ...string) {
	t.Helper()
	articles := articleManagementList(t, mux, user)
	ids := []string{}
	for _, article := range articles {
		ids = append(ids, article.ID)
	}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("order %v, want %v", ids, want)
	}
}

func TestArticleManagementNewestFirstAndLegacyCompatibility(t *testing.T) {
	a, mux, admin, _ := contentFixture(t)
	// These pre-existing JSON documents have neither pinned nor createdAt. Their
	// sort may have been deliberate, so reading/upgrading must not reverse it.
	if err := a.Store.Update(func(s *State) error {
		for i, id := range []string{"legacy-first", "legacy-second"} {
			if s.Docs["articles"] == nil {
				s.Docs["articles"] = map[string]json.RawMessage{}
			}
			s.Docs["articles"][id] = json.RawMessage(fmt.Sprintf(`{"id":%q,"title":%q,"sort":%d,"published":true,"updatedAt":%d}`, id, id, i, i+10))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	articleManagementOrder(t, mux, nil, "legacy-first", "legacy-second")
	first := articleManagementCreate(t, mux, admin, "first new", true)
	second := articleManagementCreate(t, mux, admin, "second new", true)
	articleManagementOrder(t, mux, nil, second.ID, first.ID, "legacy-first", "legacy-second")
	first.Title = "edited old article"
	first.CreatedAt = 1
	first.Sort = -100
	first.Pinned = true
	w := contentCall(t, mux, admin, "PUT", "/api/admin/articles/"+first.ID, first)
	var edited Article
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &edited) != nil || edited.CreatedAt <= 1 || edited.Pinned {
		t.Fatalf("edit: %s", w.Body.String())
	}
	articleManagementOrder(t, mux, nil, second.ID, first.ID, "legacy-first", "legacy-second")
}

func TestArticleManagementPinAndGroupEndpointMoves(t *testing.T) {
	_, mux, admin, member := contentFixture(t)
	a := articleManagementCreate(t, mux, admin, "old pinned", true)
	b := articleManagementCreate(t, mux, admin, "middle", true)
	c := articleManagementCreate(t, mux, admin, "new", true)
	d := articleManagementCreate(t, mux, admin, "newest", true)
	act := func(id, operation string, body any) {
		t.Helper()
		w := contentCall(t, mux, admin, "POST", "/api/admin/articles/"+id+"/"+operation, body)
		if w.Code != 200 {
			t.Fatalf("%s: %s", operation, w.Body.String())
		}
	}
	act(a.ID, "pin", map[string]bool{"pinned": true})
	articleManagementOrder(t, mux, nil, a.ID, d.ID, c.ID, b.ID)
	act(b.ID, "move", map[string]string{"direction": "top"})
	articleManagementOrder(t, mux, nil, a.ID, b.ID, d.ID, c.ID)
	act(b.ID, "move", map[string]string{"direction": "up"}) // cannot cross the pinned boundary
	act(a.ID, "move", map[string]string{"direction": "bottom"})
	articleManagementOrder(t, mux, nil, a.ID, b.ID, d.ID, c.ID)
	act(b.ID, "move", map[string]string{"direction": "bottom"})
	articleManagementOrder(t, mux, nil, a.ID, d.ID, c.ID, b.ID)
	act(c.ID, "pin", map[string]bool{"pinned": true})
	articleManagementOrder(t, mux, nil, c.ID, a.ID, d.ID, b.ID)
	act(c.ID, "move", map[string]string{"direction": "bottom"})
	act(c.ID, "pin", map[string]bool{"pinned": true}) // idempotent, not moved to the top again
	articleManagementOrder(t, mux, nil, a.ID, c.ID, d.ID, b.ID)
	e := articleManagementCreate(t, mux, admin, "after pinned", true)
	articleManagementOrder(t, mux, nil, a.ID, c.ID, e.ID, d.ID, b.ID)
	// A stale edit opened before pinning cannot remove the pin or undo movement.
	a.Body = "updated body"
	if w := contentCall(t, mux, admin, "PUT", "/api/admin/articles/"+a.ID, a); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	articleManagementOrder(t, mux, nil, a.ID, c.ID, e.ID, d.ID, b.ID)
	act(a.ID, "pin", map[string]bool{"pinned": false})
	articleManagementOrder(t, mux, nil, c.ID, a.ID, e.ID, d.ID, b.ID)
	for _, operation := range []string{"pin", "move"} {
		for _, user := range []*User{nil, member} {
			if w := contentCall(t, mux, user, "POST", "/api/admin/articles/"+a.ID+"/"+operation, map[string]any{"pinned": true, "direction": "top"}); w.Code != 403 {
				t.Fatalf("unauthorized %s: %d", operation, w.Code)
			}
		}
	}
	for _, invalid := range []struct {
		path   string
		body   any
		status int
	}{
		{a.ID + "/pin", map[string]any{}, 400}, {a.ID + "/pin", map[string]string{"pinned": "yes"}, 400},
		{a.ID + "/move", map[string]string{"direction": "invalid"}, 400}, {"missing/pin", map[string]bool{"pinned": true}, 409},
	} {
		if w := contentCall(t, mux, admin, "POST", "/api/admin/articles/"+invalid.path, invalid.body); w.Code != invalid.status {
			t.Fatalf("invalid request: %d", w.Code)
		}
	}
}

func TestArticleManagementStablePinnedTies(t *testing.T) {
	items := []Article{{ID: "b", Pinned: true, UpdatedAt: 100}, {ID: "a", Pinned: true, UpdatedAt: 100}, {ID: "new", CreatedAt: 9999, Sort: -100}}
	for i := 0; i < 20; i++ {
		sortArticles(items)
		if items[0].ID != "a" || items[1].ID != "b" || items[2].ID != "new" {
			t.Fatalf("unstable pin tie: %+v", items)
		}
		items[0], items[2] = items[2], items[0]
	}
}

func TestArticleManagementDraftAttachmentsStayPrivateUntilExplicitPublish(t *testing.T) {
	_, mux, admin, member := contentFixture(t)
	draft := articleManagementCreate(t, mux, admin, "未命名草稿", false)
	w := contentUpload(t, mux, admin, draft.ID, "draft.txt", []byte("private draft"), false)
	var attachment Attachment
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &attachment) != nil {
		t.Fatal(w.Body.String())
	}
	path := "/api/articles/" + draft.ID + "/attachments/" + attachment.ID
	for _, user := range []*User{nil, member} {
		if len(articleManagementList(t, mux, user)) != 0 {
			t.Fatal("draft listed publicly")
		}
		if w := contentCall(t, mux, user, "GET", path, nil); w.Code != 403 {
			t.Fatal("private draft attachment was public")
		}
	}
	if w := contentCall(t, mux, admin, "GET", path, nil); w.Code != 200 {
		t.Fatal("admin cannot inspect draft")
	}
	if w := contentUpload(t, mux, member, draft.ID, "member.txt", []byte("blocked"), false); w.Code != 403 {
		t.Fatal("member upload allowed")
	}
	if w := contentUpload(t, mux, admin, draft.ID, "script.html", []byte("blocked"), false); w.Code != 400 {
		t.Fatal("unsafe draft attachment allowed")
	}
	draft.Published = true
	// A stale editor's empty attachment list must not discard the uploaded file.
	if w := contentCall(t, mux, admin, "PUT", "/api/admin/articles/"+draft.ID, draft); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	visible := articleManagementList(t, mux, nil)
	if len(visible) != 1 || len(visible[0].Attachments) != 1 || visible[0].Attachments[0].ID != attachment.ID {
		t.Fatal("draft publish lost attachment")
	}
	if w := contentCall(t, mux, nil, "GET", path, nil); w.Code != 200 || w.Body.String() != "private draft" {
		t.Fatal("explicit publish unavailable")
	}
}

func TestArticleManagementConcurrentPinMoveAndSave(t *testing.T) {
	_, mux, admin, _ := contentFixture(t)
	articles := []Article{}
	for i := 0; i < 6; i++ {
		articles = append(articles, articleManagementCreate(t, mux, admin, fmt.Sprint(i), true))
	}
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			article := articles[n%len(articles)]
			path, method, body := "/api/admin/articles/"+article.ID+"/pin", "POST", any(map[string]bool{"pinned": n%2 == 0})
			if n%3 == 1 {
				path, body = "/api/admin/articles/"+article.ID+"/move", map[string]string{"direction": "bottom"}
			}
			if n%3 == 2 {
				path, method, body = "/api/admin/articles/"+article.ID, "PUT", article
			}
			if w := contentCall(t, mux, admin, method, path, body); w.Code != 200 {
				t.Errorf("concurrent operation: %d", w.Code)
			}
		}(i)
	}
	wg.Wait()
	out := articleManagementList(t, mux, admin)
	if len(out) != len(articles) {
		t.Fatal("lost article")
	}
	seenNormal := false
	for i, article := range out {
		if article.Sort != i || article.Body != "content" {
			t.Fatal("invalid permutation/content")
		}
		if !article.Pinned {
			seenNormal = true
		} else if seenNormal {
			t.Fatal("pinned article below normal group")
		}
	}
}
