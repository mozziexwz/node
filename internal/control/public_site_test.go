package control

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func publicFixture(t *testing.T) (*App, *http.ServeMux, http.Handler, *User, *User) {
	t.Helper()
	a, mux, admin, member := contentFixture(t)
	a.Config.PublicURL = "https://news.test"
	a.RegisterPublicSite(mux)
	return a, mux, a.Authenticate(mux), admin, member
}

func publicGet(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("GET", path, nil)
	r.RemoteAddr = "198.51.100.7:4000"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func publicArticleIDs(t *testing.T, w *httptest.ResponseRecorder) []string {
	t.Helper()
	var out struct {
		Enabled  bool                   `json:"enabled"`
		Articles []publicArticleSummary `json:"articles"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatalf("public list: %d %s", w.Code, w.Body.String())
	}
	ids := []string{}
	for _, article := range out.Articles {
		ids = append(ids, article.ID)
	}
	return ids
}

func savePublicArticle(t *testing.T, a *App, article Article) {
	t.Helper()
	if err := a.Store.Update(func(s *State) error { return SaveDoc(s, "articles", article.ID, article) }); err != nil {
		t.Fatal(err)
	}
	a.invalidatePublic()
}

func TestPublicArticlesExposeOnlyPublishedPublicArticles(t *testing.T) {
	a, _, h, _, _ := publicFixture(t)
	savePublicArticle(t, a, Article{ID: "draft-public", Title: "Draft", Public: true, Body: "secret draft"})
	savePublicArticle(t, a, Article{ID: "members-only", Title: "Members", Published: true, Body: "members body"})
	savePublicArticle(t, a, Article{ID: "open", Title: "Open", Published: true, Public: true, Body: "## Heading\n\nHello **world** [link](https://example.test)", Attachments: []Attachment{{ID: "f", Name: "a.txt"}}})
	if ids := publicArticleIDs(t, publicGet(t, h, "/api/public/articles")); fmt.Sprint(ids) != "[open]" {
		t.Fatalf("public list exposed %v", ids)
	}
	for _, id := range []string{"draft-public", "members-only", "missing"} {
		if w := publicGet(t, h, "/api/public/articles/"+id); w.Code != 404 {
			t.Fatalf("%s: %d %s", id, w.Code, w.Body.String())
		}
	}
	w := publicGet(t, h, "/api/public/articles/open")
	var detail struct {
		Article publicArticle `json:"article"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &detail) != nil || detail.Article.Body == "" || detail.Article.Summary != "Heading Hello world link" || len(detail.Article.Attachments) != 1 {
		t.Fatalf("public detail: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "secret draft") || strings.Contains(w.Body.String(), "members body") {
		t.Fatal("private body leaked through public detail")
	}
	// The guest master switch hides the whole area.
	_ = a.Store.Update(func(s *State) error { s.Settings["publicArticles"] = false; return nil })
	a.invalidatePublic()
	if ids := publicArticleIDs(t, publicGet(t, h, "/api/public/articles")); len(ids) != 0 {
		t.Fatalf("disabled article area listed %v", ids)
	}
	if w := publicGet(t, h, "/api/public/articles/open"); w.Code != 404 {
		t.Fatal("disabled article area served a detail page")
	}
}

func TestPublicVisibilityEndpointAndImmediateInvalidation(t *testing.T) {
	a, mux, h, admin, member := publicFixture(t)
	savePublicArticle(t, a, Article{ID: "post", Title: "Post", Published: true, Body: "body"})
	// Warm the cache so the test proves invalidation rather than TTL expiry.
	if ids := publicArticleIDs(t, publicGet(t, h, "/api/public/articles")); len(ids) != 0 {
		t.Fatalf("members-only article listed %v", ids)
	}
	if w := contentCall(t, mux, member, "POST", "/api/admin/articles/post/visibility", map[string]bool{"public": true}); w.Code != 403 {
		t.Fatal("member changed article visibility")
	}
	if w := contentCall(t, mux, admin, "POST", "/api/admin/articles/post/visibility", map[string]any{}); w.Code != 400 {
		t.Fatal("empty visibility request accepted")
	}
	if w := contentCall(t, mux, admin, "POST", "/api/admin/articles/post/visibility", map[string]bool{"public": true}); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if ids := publicArticleIDs(t, publicGet(t, h, "/api/public/articles")); fmt.Sprint(ids) != "[post]" {
		t.Fatalf("public flag not visible immediately: %v", ids)
	}
	if w := contentCall(t, mux, admin, "POST", "/api/admin/articles/post/visibility", map[string]bool{"published": false}); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if ids := publicArticleIDs(t, publicGet(t, h, "/api/public/articles")); len(ids) != 0 {
		t.Fatalf("unpublished article stayed public: %v", ids)
	}
	_ = a.Store.View(func(s *State) error {
		v, _ := LoadDoc[Article](s, "articles", "post")
		if v.Published || !v.Public || v.Body != "body" {
			t.Fatalf("visibility update changed unrelated fields: %+v", v)
		}
		return nil
	})
}

func TestPublicReadsSkipBackupGateAndSession(t *testing.T) {
	a, _, h, _, _ := publicFixture(t)
	savePublicArticle(t, a, Article{ID: "open", Title: "Open", Published: true, Public: true, Body: "body"})
	if err := a.Store.Update(func(s *State) error {
		s.Docs[backupPauseCollection] = map[string]json.RawMessage{"default": json.RawMessage(`{}`)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if w := publicGet(t, h, "/api/public/articles"); w.Code != 200 {
		t.Fatalf("public read blocked by backup gate: %d", w.Code)
	}
	if w := publicGet(t, h, "/api/articles"); w.Code != 503 {
		t.Fatalf("private API skipped the backup gate: %d", w.Code)
	}
}

func TestPublicSiteSettingsValidationAndDefaults(t *testing.T) {
	a, mux, h, admin, member := publicFixture(t)
	var home struct {
		Home HomeConfig     `json:"home"`
		Site publicSiteInfo `json:"site"`
	}
	if w := publicGet(t, h, "/api/public/home"); w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &home) != nil || home.Home.TitleLine2 != "独立IP游戏节点" || home.Site.SiteName != "MSBOOST" {
		t.Fatalf("default home: %d %s", w.Code, w.Body.String())
	}
	if w := contentCall(t, mux, member, "GET", "/api/admin/site", nil); w.Code != 403 {
		t.Fatal("member read site settings")
	}
	valid := defaultHomeConfig()
	valid.TitleLine1 = "新的主页"
	for name, mutate := range map[string]func(*HomeConfig){
		"javascript href":   func(c *HomeConfig) { c.PrimaryCTA.Href = "javascript:alert(1)" },
		"protocol relative": func(c *HomeConfig) { c.SecondaryCTA.Href = "//evil.test" },
		"plain http":        func(c *HomeConfig) { c.NoticeHref, c.Notice = "http://evil.test", "notice" },
		"too many points":   func(c *HomeConfig) { c.Points = []string{"1", "2", "3", "4", "5"} },
		"latest count":      func(c *HomeConfig) { c.LatestCount = 5 },
		"empty title":       func(c *HomeConfig) { c.TitleLine1 = "  " },
		"control char":      func(c *HomeConfig) { c.Eyebrow = "a\u0000b" },
	} {
		bad := valid
		bad.Points = append([]string{}, valid.Points...)
		mutate(&bad)
		if w := contentCall(t, mux, admin, "PUT", "/api/admin/site", map[string]any{"home": bad}); w.Code != 400 {
			t.Fatalf("%s accepted: %d %s", name, w.Code, w.Body.String())
		}
	}
	seo := defaultSEOConfig()
	seo.OGImage = "data:image/png;base64,AAAA"
	if w := contentCall(t, mux, admin, "PUT", "/api/admin/site", map[string]any{"seo": seo}); w.Code != 400 {
		t.Fatal("data: share image accepted")
	}
	if w := contentCall(t, mux, admin, "PUT", "/api/admin/site", map[string]any{"home": valid}); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w := publicGet(t, h, "/api/public/home"); !strings.Contains(w.Body.String(), "新的主页") {
		t.Fatalf("saved home not served immediately: %s", w.Body.String())
	}
	// A tampered stored value falls back to defaults instead of bypassing checks.
	_ = a.Store.Update(func(s *State) error {
		s.Settings["homepage"] = map[string]any{"titleLine1": "x", "primaryCta": map[string]any{"text": "go", "href": "javascript:alert(1)"}}
		return nil
	})
	a.invalidatePublic()
	if w := publicGet(t, h, "/api/public/home"); strings.Contains(w.Body.String(), "javascript:") || !strings.Contains(w.Body.String(), "一键部署你的") {
		t.Fatalf("invalid stored home served: %s", w.Body.String())
	}
}

func TestPublicPagesInjectEscapedMetaRobotsAndSitemap(t *testing.T) {
	a, _, _, _, _ := publicFixture(t)
	webDir := t.TempDir()
	index := `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="description" content="old"><title>Old</title></head><body><div id="root"></div></body></html>`
	if err := os.WriteFile(filepath.Join(webDir, "index.html"), []byte(index), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(webDir, "assets"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(webDir, "assets", "app-1.js"), []byte("console.log(1)"), 0o600); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	a.RegisterWeb(mux, webDir)
	h := a.Authenticate(mux)
	savePublicArticle(t, a, Article{ID: "xss", Title: `<script>alert("x")</script>`, Summary: `"quoted" & <b>`, Published: true, Public: true, Body: "body", UpdatedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).UnixMilli()})
	w := publicGet(t, h, "/news/xss")
	body := w.Body.String()
	if w.Code != 200 || strings.Contains(body, "<script>alert") || !strings.Contains(body, "&lt;script&gt;alert(&#34;x&#34;)&lt;/script&gt; · MSBOOST") {
		t.Fatalf("article page not escaped: %d %s", w.Code, body)
	}
	if !strings.Contains(body, `<link rel="canonical" href="https://news.test/news/xss">`) || !strings.Contains(body, `property="og:type" content="article"`) || strings.Contains(body, `content="old"`) {
		t.Fatalf("article meta missing: %s", body)
	}
	if w := publicGet(t, h, "/news/missing"); w.Code != 404 || !strings.Contains(w.Body.String(), "noindex") {
		t.Fatalf("missing article: %d %s", w.Code, w.Body.String())
	}
	if w := publicGet(t, h, "/home"); w.Code != 200 || !strings.Contains(w.Body.String(), "<title>MSBOOST · 你的专属游戏线路</title>") {
		t.Fatalf("home meta: %s", w.Body.String())
	}
	if w := publicGet(t, h, "/"); strings.Contains(w.Body.String(), "og:title") || w.Header().Get("Cache-Control") != "no-cache" {
		t.Fatal("console entry changed")
	}
	if w := publicGet(t, h, "/assets/app-1.js"); w.Code != 200 || !strings.Contains(w.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("hashed asset cache: %d %q", w.Code, w.Header().Get("Cache-Control"))
	}
	if w := publicGet(t, h, "/assets/stale-0.js"); w.Code != 404 {
		t.Fatalf("missing asset served HTML: %d", w.Code)
	}
	if w := publicGet(t, h, "/assets/"); strings.Contains(w.Body.String(), "app-1.js") {
		t.Fatal("asset directory listed")
	}
	if w := publicGet(t, h, "/robots.txt"); !strings.Contains(w.Body.String(), "Sitemap: https://news.test/sitemap.xml") || !strings.Contains(w.Body.String(), "Disallow: /api/") {
		t.Fatalf("robots: %s", w.Body.String())
	}
	if w := publicGet(t, h, "/sitemap.xml"); !strings.Contains(w.Body.String(), "<loc>https://news.test/news/xss</loc><lastmod>2026-10-01</lastmod>") {
		t.Fatalf("sitemap: %s", w.Body.String())
	}
	seo := defaultSEOConfig()
	seo.AllowIndexing = false
	_ = a.Store.Update(func(s *State) error { s.Settings["seo"] = seo; return nil })
	a.invalidatePublic()
	if w := publicGet(t, h, "/robots.txt"); !strings.Contains(w.Body.String(), "Disallow: /\n") {
		t.Fatalf("indexing switch ignored: %s", w.Body.String())
	}
	if w := publicGet(t, h, "/sitemap.xml"); w.Code != 404 {
		t.Fatal("sitemap served while indexing is disabled")
	}
	if w := publicGet(t, h, "/home"); !strings.Contains(w.Body.String(), `<meta name="robots" content="noindex,nofollow">`) {
		t.Fatal("noindex missing")
	}
}

func TestLoginFailuresDoNotLockOwnerFromAnotherSource(t *testing.T) {
	_, h := identityFixture(t, true)
	login := func(addr, password string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(map[string]string{"email": "99999999@qq.com", "password": password})
		r := httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(string(raw)))
		r.RemoteAddr = addr
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "https://identity.test")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for i := 0; i < 10; i++ {
		if w := login("203.0.113.9:1000", "wrong-password"); w.Code != 401 {
			t.Fatalf("failure %d: %d", i, w.Code)
		}
	}
	if w := login("203.0.113.9:1000", "test-admin-password-123"); w.Code != 429 {
		t.Fatalf("attacking source not limited: %d", w.Code)
	}
	if w := login("192.0.2.77:1000", "test-admin-password-123"); w.Code != 200 {
		t.Fatalf("owner locked out by another source: %d %s", w.Code, w.Body.String())
	}
	// IPv6 sources inside one /64 share a bucket.
	for i := 0; i < 10; i++ {
		login(fmt.Sprintf("[2001:db8:1:2::%x]:1000", i+1), "wrong-password")
	}
	if w := login("[2001:db8:1:2::ffff]:1000", "test-admin-password-123"); w.Code != 429 {
		t.Fatalf("IPv6 /64 rotation bypassed the limit: %d", w.Code)
	}
}

func TestRateTableFullNeverRefusesEveryone(t *testing.T) {
	a, _ := identityFixture(t, false)
	now := time.Now().UnixMilli()
	for i := 0; i < rateMaxKeys+10; i++ {
		a.rates[fmt.Sprintf("flood:%d", i)] = []int64{now}
	}
	if !a.allow("login-ip:192.0.2.1", 20, 15*time.Minute) {
		t.Fatal("full rate table refused an unrelated key")
	}
	if len(a.rates) > rateMaxKeys {
		t.Fatalf("rate table not compacted: %d", len(a.rates))
	}
}

func TestStoreSkipsUnchangedWritesAndAuditIsBounded(t *testing.T) {
	a, _, _, _ := contentFixture(t)
	revision := func() int64 {
		var value int64
		if err := a.Store.db.QueryRow("SELECT revision FROM control_state WHERE id=1").Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	before := revision()
	if err := a.Store.Update(func(s *State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if revision() != before {
		t.Fatal("unchanged state was rewritten")
	}
	if err := a.Store.Update(func(s *State) error { return SaveDoc(s, "counter", "one", 1) }); err != nil {
		t.Fatal(err)
	}
	if revision() != before+1 {
		t.Fatal("changed state was not persisted")
	}
	if err := a.Store.Update(func(s *State) error {
		s.Docs["content_audit"] = map[string]json.RawMessage{}
		for i := 0; i < auditRetain+auditPruneSlack; i++ {
			s.Docs["content_audit"][fmt.Sprintf("e%05d", i)] = json.RawMessage(fmt.Sprintf(`{"createdAt":%d}`, i+1))
		}
		contentAudit(s, "admin", "test.latest", "x")
		if len(s.Docs["content_audit"]) != auditRetain {
			t.Fatalf("audit not pruned: %d", len(s.Docs["content_audit"]))
		}
		if _, ok := s.Docs["content_audit"]["e00000"]; ok {
			t.Fatal("oldest audit event kept")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAuditListIsNewestFirstAndBounded(t *testing.T) {
	a, mux, admin, _ := contentFixture(t)
	_ = a.Store.Update(func(s *State) error {
		s.Docs["audit"] = map[string]json.RawMessage{"old": json.RawMessage(`{"createdAt":1}`), "new": json.RawMessage(`{"createdAt":3}`)}
		s.Docs["commerce_audit"] = map[string]json.RawMessage{"mid": json.RawMessage(`{"createdAt":2}`)}
		s.Docs["content_audit"] = map[string]json.RawMessage{}
		return nil
	})
	w := contentCall(t, mux, admin, "GET", "/api/admin/audit?limit=2", nil)
	var out struct {
		Events []map[string]int64 `json:"events"`
		Total  int                `json:"total"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.Total != 3 || len(out.Events) != 2 || out.Events[0]["createdAt"] != 3 || out.Events[1]["createdAt"] != 2 {
		t.Fatalf("audit list: %d %s", w.Code, w.Body.String())
	}
	if w := contentCall(t, mux, admin, "GET", "/api/admin/audit?limit=5000", nil); w.Code != 400 {
		t.Fatal("unbounded audit limit accepted")
	}
}
