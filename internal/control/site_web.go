package control

import (
	"bytes"
	"html"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const webContentSecurityPolicy = "default-src 'self'; script-src 'self' https://challenges.cloudflare.com; style-src 'self' 'unsafe-inline'; img-src 'self' https: data: blob:; connect-src 'self'; frame-src https://challenges.cloudflare.com; object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'"

var (
	indexTitlePattern       = regexp.MustCompile(`(?is)<title>.*?</title>`)
	indexDescriptionPattern = regexp.MustCompile(`(?is)<meta\s+name="description"[^>]*>`)
	indexHeadEndPattern     = regexp.MustCompile(`(?i)</head>`)
	publicArticlePath       = regexp.MustCompile(`^/news/([A-Za-z0-9_-]{1,80})$`)
)

// RegisterWeb serves the built single-page app. Public pages receive their own
// title and share tags from the public snapshot, so link previews and crawlers
// see real content without server-side rendering the React tree.
func (a *App) RegisterWeb(mux *http.ServeMux, webDir string) {
	mux.HandleFunc("GET /robots.txt", a.robotsTxt)
	mux.HandleFunc("GET /sitemap.xml", a.sitemapXML)
	mux.Handle("/", a.webHandler(webDir))
}

type indexFile struct {
	path string
	mu   sync.Mutex
	mod  time.Time
	size int64
	body []byte
}

// The template is re-read only when the build output changes on disk.
func (f *indexFile) read() ([]byte, error) {
	info, err := os.Stat(f.path)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.body == nil || !info.ModTime().Equal(f.mod) || info.Size() != f.size {
		body, err := os.ReadFile(f.path)
		if err != nil {
			return nil, err
		}
		f.body, f.mod, f.size = body, info.ModTime(), info.Size()
	}
	return f.body, nil
}

func (a *App) webHandler(webDir string) http.Handler {
	files := http.FileServer(http.Dir(webDir))
	index := &indexFile{path: filepath.Join(webDir, "index.html")}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" && r.Method != "HEAD" {
			http.Error(w, "Method not allowed", 405)
			return
		}
		w.Header().Set("Content-Security-Policy", webContentSecurityPolicy)
		w.Header().Set("X-Frame-Options", "DENY")
		clean := path.Clean("/" + r.URL.Path)
		info, err := os.Stat(filepath.Join(webDir, filepath.FromSlash(clean)))
		if err != nil || info.IsDir() {
			// A missing hashed asset is a stale page after an upgrade; answering
			// with HTML would only produce a confusing script MIME error.
			if err != nil && strings.HasPrefix(clean, "/assets/") {
				http.NotFound(w, r)
				return
			}
			if a.servePublicPage(w, r, clean, index) {
				return
			}
			// Unknown paths and directories fall back to the app instead of a listing.
			r.URL.Path = "/"
		} else if strings.HasPrefix(clean, "/assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		if r.URL.Path == "/" {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}

type pageMeta struct {
	title, description, keywords, siteName string
	canonical, image, kind                 string
	noindex                                bool
}

func (a *App) absoluteURL(p string) string {
	if a.Config.PublicURL == "" {
		return ""
	}
	return a.Config.PublicURL + p
}

func (a *App) publicPageMeta(snap *publicSnapshot, clean string) (pageMeta, int, bool) {
	seo := snap.seo
	meta := pageMeta{description: seo.Description, keywords: seo.Keywords, siteName: seo.SiteName, kind: "website", noindex: !seo.AllowIndexing}
	if strings.HasPrefix(seo.OGImage, "https://") {
		meta.image = seo.OGImage
	} else if seo.OGImage != "" {
		meta.image = a.absoluteURL(seo.OGImage)
	}
	switch {
	case clean == "/home":
		meta.title, meta.canonical = seo.HomeTitle, a.absoluteURL("/home")
		return meta, 200, true
	case clean == "/news":
		meta.title, meta.canonical = seo.NewsTitle, a.absoluteURL("/news")
		meta.noindex = meta.noindex || !snap.newsEnabled
		return meta, 200, true
	}
	match := publicArticlePath.FindStringSubmatch(clean)
	if match == nil {
		return meta, 0, false
	}
	article, ok := snap.details[match[1]]
	if !ok {
		meta.title, meta.noindex = "文章不存在或未公开 · "+seo.SiteName, true
		return meta, 404, true
	}
	meta.title = article.Title + " · " + seo.SiteName
	if article.Summary != "" {
		meta.description = article.Summary
	}
	meta.canonical = a.absoluteURL("/news/" + article.ID)
	meta.kind = "article"
	return meta, 200, true
}

func (a *App) servePublicPage(w http.ResponseWriter, r *http.Request, clean string, index *indexFile) bool {
	if clean != "/home" && clean != "/news" && !strings.HasPrefix(clean, "/news/") {
		return false
	}
	body, err := index.read()
	if err != nil {
		return false
	}
	snap, err := a.publicSnapshot()
	if err != nil {
		return false
	}
	meta, status, ok := a.publicPageMeta(snap, clean)
	if !ok {
		return false
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(status)
	if r.Method != "HEAD" {
		_, _ = w.Write(renderIndexMeta(body, meta))
	}
	return true
}

// Every value is HTML-escaped; administrator text never becomes markup.
func renderIndexMeta(index []byte, meta pageMeta) []byte {
	esc := html.EscapeString
	out := indexTitlePattern.ReplaceAllLiteral(index, []byte("<title>"+esc(meta.title)+"</title>"))
	var tags strings.Builder
	description := `<meta name="description" content="` + esc(meta.description) + `">`
	if indexDescriptionPattern.Match(out) {
		out = indexDescriptionPattern.ReplaceAllLiteral(out, []byte(description))
	} else {
		tags.WriteString(description)
	}
	property := func(name, value string) {
		if value != "" {
			tags.WriteString(`<meta property="` + name + `" content="` + esc(value) + `">`)
		}
	}
	if meta.keywords != "" {
		tags.WriteString(`<meta name="keywords" content="` + esc(meta.keywords) + `">`)
	}
	if meta.noindex {
		tags.WriteString(`<meta name="robots" content="noindex,nofollow">`)
	}
	if meta.canonical != "" {
		tags.WriteString(`<link rel="canonical" href="` + esc(meta.canonical) + `">`)
	}
	property("og:type", meta.kind)
	property("og:site_name", meta.siteName)
	property("og:title", meta.title)
	property("og:description", meta.description)
	property("og:url", meta.canonical)
	property("og:image", meta.image)
	card := "summary"
	if meta.image != "" {
		card = "summary_large_image"
	}
	tags.WriteString(`<meta name="twitter:card" content="` + card + `">`)
	location := indexHeadEndPattern.FindIndex(out)
	if location == nil {
		return out
	}
	return bytes.Join([][]byte{out[:location[0]], []byte(tags.String()), out[location[0]:]}, nil)
}

func (a *App) robotsTxt(w http.ResponseWriter, r *http.Request) {
	snap, err := a.publicSnapshot()
	if err != nil {
		http.Error(w, "Service unavailable", 503)
		return
	}
	var body strings.Builder
	body.WriteString("User-agent: *\n")
	if !snap.seo.AllowIndexing {
		body.WriteString("Disallow: /\n")
	} else {
		body.WriteString("Allow: /home\nAllow: /news\nDisallow: /api/\n")
		if a.Config.PublicURL != "" {
			body.WriteString("Sitemap: " + a.Config.PublicURL + "/sitemap.xml\n")
		}
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write([]byte(body.String()))
}

func (a *App) sitemapXML(w http.ResponseWriter, r *http.Request) {
	snap, err := a.publicSnapshot()
	if err != nil {
		http.Error(w, "Service unavailable", 503)
		return
	}
	if a.Config.PublicURL == "" || !snap.seo.AllowIndexing {
		http.NotFound(w, r)
		return
	}
	var body strings.Builder
	entry := func(p string, updatedAt int64) {
		body.WriteString("<url><loc>" + html.EscapeString(a.Config.PublicURL+p) + "</loc>")
		if updatedAt > 0 {
			body.WriteString("<lastmod>" + time.UnixMilli(updatedAt).UTC().Format("2006-01-02") + "</lastmod>")
		}
		body.WriteString("</url>")
	}
	body.WriteString(`<?xml version="1.0" encoding="UTF-8"?><urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">`)
	entry("/home", 0)
	if snap.newsEnabled {
		entry("/news", 0)
		for _, article := range snap.articles {
			entry("/news/"+article.ID, article.UpdatedAt)
		}
	}
	body.WriteString("</urlset>")
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write([]byte(body.String()))
}
