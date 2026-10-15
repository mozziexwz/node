package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// Public pages may receive crawler and anonymous traffic. They are served from
// this in-memory snapshot instead of decoding the whole control state per hit.
const publicCacheTTL = 15 * time.Second

type HomeCTA struct {
	Text string `json:"text"`
	Href string `json:"href"`
}

type HomeFeature struct {
	Title string `json:"title"`
	Desc  string `json:"desc"`
}

type HomeConfig struct {
	Eyebrow      string        `json:"eyebrow"`
	TitleLine1   string        `json:"titleLine1"`
	TitleLine2   string        `json:"titleLine2"`
	Subtitle     string        `json:"subtitle"`
	Points       []string      `json:"points"`
	PrimaryCTA   HomeCTA       `json:"primaryCta"`
	SecondaryCTA HomeCTA       `json:"secondaryCta"`
	Features     []HomeFeature `json:"features"`
	ShowLatest   bool          `json:"showLatest"`
	LatestCount  int           `json:"latestCount"`
	Notice       string        `json:"notice"`
	NoticeHref   string        `json:"noticeHref"`
	ContactEmail string        `json:"contactEmail"`
}

type SEOConfig struct {
	SiteName      string `json:"siteName"`
	HomeTitle     string `json:"homeTitle"`
	NewsTitle     string `json:"newsTitle"`
	Description   string `json:"description"`
	Keywords      string `json:"keywords"`
	OGImage       string `json:"ogImage"`
	AllowIndexing bool   `json:"allowIndexing"`
}

// Defaults reproduce the existing sign-in landing copy, so /home is complete
// before an administrator edits anything.
func defaultHomeConfig() HomeConfig {
	return HomeConfig{
		Eyebrow:      "SELF-HOSTED · MAPLESTORY",
		TitleLine1:   "一键部署你的",
		TitleLine2:   "独立IP游戏节点",
		Subtitle:     "部署节点、配置中转、重装系统。\n把复杂的服务器操作，放进一个清晰的工作空间。",
		Points:       []string{"三项免费核心工具", "需自备 VPS", "Windows 启动器"},
		PrimaryCTA:   HomeCTA{Text: "进入控制台", Href: "/"},
		SecondaryCTA: HomeCTA{Text: "阅读教程", Href: "/news"},
		Features: []HomeFeature{
			{Title: "一键部署 MSBOOST", Desc: "部署你的独立IP游戏节点"},
			{Title: "一键配置中转服务器", Desc: "与朋友共享你的VPS线路，但不共享你的IP"},
			{Title: "一键DD系统", Desc: "在线重装系统"},
		},
		ShowLatest:   true,
		LatestCount:  3,
		ContactEmail: "admin@msboost.de",
	}
}

func defaultSEOConfig() SEOConfig {
	return SEOConfig{
		SiteName:      "MSBOOST",
		HomeTitle:     "MSBOOST · 你的专属游戏线路",
		NewsTitle:     "公告与教程 · MSBOOST",
		Description:   "MSBOOST 自备 VPS 游戏线路部署与管理",
		AllowIndexing: true,
	}
}

func siteText(label, value string, max int, required, multiline bool) (string, error) {
	value = strings.TrimSpace(value)
	if required && value == "" {
		return "", fmt.Errorf("%s不能为空", label)
	}
	if utf8.RuneCountInString(value) > max {
		return "", fmt.Errorf("%s不能超过 %d 字", label, max)
	}
	if strings.IndexFunc(value, func(r rune) bool { return unicode.IsControl(r) && !(multiline && r == '\n') }) >= 0 {
		return "", fmt.Errorf("%s不能包含控制字符", label)
	}
	return value, nil
}

// Links are limited to same-site paths or HTTPS so stored settings cannot inject
// javascript:, data: or protocol-relative destinations into public pages.
func validSiteHref(value string) bool {
	if value == "" || len(value) > 300 || strings.ContainsAny(value, " \t\r\n\"'<>\\`") {
		return false
	}
	if strings.HasPrefix(value, "/") {
		return !strings.HasPrefix(value, "//")
	}
	u, err := url.Parse(value)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil
}

func siteHref(label, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value != "" && !validSiteHref(value) {
		return "", fmt.Errorf("%s只能是以 / 开头的站内路径或 https:// 链接", label)
	}
	return value, nil
}

func normalizeCTA(label string, in HomeCTA) (HomeCTA, error) {
	text, err := siteText(label+"文字", in.Text, 12, false, false)
	if err != nil {
		return in, err
	}
	href, err := siteHref(label+"链接", in.Href)
	if err != nil {
		return in, err
	}
	if text == "" {
		return HomeCTA{}, nil
	}
	if href == "" {
		return in, fmt.Errorf("请填写%s链接", label)
	}
	return HomeCTA{Text: text, Href: href}, nil
}

func normalizeHome(in HomeConfig) (HomeConfig, error) {
	var out HomeConfig
	var err error
	if out.Eyebrow, err = siteText("小标题", in.Eyebrow, 60, false, false); err != nil {
		return out, err
	}
	if out.TitleLine1, err = siteText("主标题第一行", in.TitleLine1, 40, true, false); err != nil {
		return out, err
	}
	if out.TitleLine2, err = siteText("主标题第二行", in.TitleLine2, 40, false, false); err != nil {
		return out, err
	}
	if out.Subtitle, err = siteText("副标题", in.Subtitle, 300, false, true); err != nil {
		return out, err
	}
	out.Points = []string{}
	for _, point := range in.Points {
		point, err = siteText("要点", point, 20, false, false)
		if err != nil {
			return out, err
		}
		if point != "" {
			out.Points = append(out.Points, point)
		}
	}
	if len(out.Points) > 4 {
		return out, errors.New("要点最多 4 项")
	}
	if out.PrimaryCTA, err = normalizeCTA("主按钮", in.PrimaryCTA); err != nil {
		return out, err
	}
	if out.SecondaryCTA, err = normalizeCTA("次按钮", in.SecondaryCTA); err != nil {
		return out, err
	}
	out.Features = []HomeFeature{}
	for _, feature := range in.Features {
		title, err := siteText("功能卡片标题", feature.Title, 20, false, false)
		if err != nil {
			return out, err
		}
		desc, err := siteText("功能卡片说明", feature.Desc, 60, false, false)
		if err != nil {
			return out, err
		}
		if title == "" && desc == "" {
			continue
		}
		if title == "" {
			return out, errors.New("功能卡片需要标题")
		}
		out.Features = append(out.Features, HomeFeature{Title: title, Desc: desc})
	}
	if len(out.Features) > 6 {
		return out, errors.New("功能卡片最多 6 张")
	}
	out.ShowLatest = in.ShowLatest
	switch in.LatestCount {
	case 0, 3:
		out.LatestCount = 3
	case 6:
		out.LatestCount = 6
	default:
		return out, errors.New("最新文章数量只能是 3 或 6")
	}
	if out.Notice, err = siteText("公告横幅", in.Notice, 120, false, false); err != nil {
		return out, err
	}
	if out.NoticeHref, err = siteHref("公告横幅链接", in.NoticeHref); err != nil {
		return out, err
	}
	if out.Notice == "" {
		out.NoticeHref = ""
	}
	if out.ContactEmail, err = siteText("联系邮箱", in.ContactEmail, 120, false, false); err != nil {
		return out, err
	}
	if out.ContactEmail != "" {
		address, parseErr := mail.ParseAddress(out.ContactEmail)
		if parseErr != nil || address.Address != out.ContactEmail {
			return out, errors.New("联系邮箱无效")
		}
	}
	return out, nil
}

func normalizeSEO(in SEOConfig) (SEOConfig, error) {
	var out SEOConfig
	var err error
	if out.SiteName, err = siteText("站点名称", in.SiteName, 40, true, false); err != nil {
		return out, err
	}
	if out.HomeTitle, err = siteText("主页标题", in.HomeTitle, 80, true, false); err != nil {
		return out, err
	}
	if out.NewsTitle, err = siteText("文章页标题", in.NewsTitle, 80, true, false); err != nil {
		return out, err
	}
	if out.Description, err = siteText("站点描述", in.Description, 200, false, false); err != nil {
		return out, err
	}
	if out.Keywords, err = siteText("关键词", in.Keywords, 200, false, false); err != nil {
		return out, err
	}
	if out.OGImage, err = siteHref("分享图片", in.OGImage); err != nil {
		return out, err
	}
	out.AllowIndexing = in.AllowIndexing
	return out, nil
}

// Stored values are merged over defaults and re-validated on every read, so a
// restored or hand-edited snapshot can never bypass the write-time checks.
func loadSiteSetting[T any](s *State, key string, fallback T, normalize func(T) (T, error)) T {
	raw, ok := s.Settings[key]
	if !ok || raw == nil {
		return fallback
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return fallback
	}
	next := fallback
	if json.Unmarshal(encoded, &next) != nil {
		return fallback
	}
	normalized, err := normalize(next)
	if err != nil {
		return fallback
	}
	return normalized
}

func loadHomeConfig(s *State) HomeConfig {
	return loadSiteSetting(s, "homepage", defaultHomeConfig(), normalizeHome)
}

func loadSEOConfig(s *State) SEOConfig {
	return loadSiteSetting(s, "seo", defaultSEOConfig(), normalizeSEO)
}

type publicArticleSummary struct {
	ID              string `json:"id"`
	Title           string `json:"title"`
	Category        string `json:"category"`
	Summary         string `json:"summary"`
	Pinned          bool   `json:"pinned"`
	CreatedAt       int64  `json:"createdAt"`
	UpdatedAt       int64  `json:"updatedAt"`
	AttachmentCount int    `json:"attachmentCount"`
}

type publicArticle struct {
	ID          string       `json:"id"`
	Title       string       `json:"title"`
	Category    string       `json:"category"`
	Summary     string       `json:"summary"`
	Body        string       `json:"body"`
	Pinned      bool         `json:"pinned"`
	CreatedAt   int64        `json:"createdAt"`
	UpdatedAt   int64        `json:"updatedAt"`
	Attachments []Attachment `json:"attachments"`
}

type publicSiteInfo struct {
	SiteName  string `json:"siteName"`
	HomeTitle string `json:"homeTitle"`
	NewsTitle string `json:"newsTitle"`
}

type publicSnapshot struct {
	home               HomeConfig
	seo                SEOConfig
	newsEnabled        bool
	attachmentsEnabled bool
	articles           []publicArticleSummary
	details            map[string]publicArticle
	builtAt            time.Time
}

func (p *publicSnapshot) site() publicSiteInfo {
	return publicSiteInfo{SiteName: p.seo.SiteName, HomeTitle: p.seo.HomeTitle, NewsTitle: p.seo.NewsTitle}
}

type publicCache struct {
	mu   sync.Mutex
	snap *publicSnapshot
}

// Holding the cache lock while building means an invalidation issued after a
// committed write waits for any in-flight build and then discards it.
func (a *App) publicSnapshot() (*publicSnapshot, error) {
	a.public.mu.Lock()
	defer a.public.mu.Unlock()
	if a.public.snap != nil && time.Since(a.public.snap.builtAt) < publicCacheTTL {
		return a.public.snap, nil
	}
	snap := &publicSnapshot{articles: []publicArticleSummary{}, details: map[string]publicArticle{}}
	err := a.Store.View(func(s *State) error {
		snap.home = loadHomeConfig(s)
		snap.seo = loadSEOConfig(s)
		snap.newsEnabled = boolSetting(s, "publicArticles")
		snap.attachmentsEnabled = boolSetting(s, "attachments")
		if !snap.newsEnabled {
			return nil
		}
		articles := ListDocs[Article](s, "articles")
		sortArticles(articles)
		for _, v := range articles {
			if !v.Published || !v.Public {
				continue
			}
			summary := v.Summary
			if summary == "" {
				summary = articleExcerpt(v.Body, 120)
			}
			attachments := append([]Attachment{}, v.Attachments...)
			snap.articles = append(snap.articles, publicArticleSummary{ID: v.ID, Title: v.Title, Category: v.Category, Summary: summary, Pinned: v.Pinned, CreatedAt: articleCreationTime(v), UpdatedAt: v.UpdatedAt, AttachmentCount: len(attachments)})
			snap.details[v.ID] = publicArticle{ID: v.ID, Title: v.Title, Category: v.Category, Summary: summary, Body: v.Body, Pinned: v.Pinned, CreatedAt: articleCreationTime(v), UpdatedAt: v.UpdatedAt, Attachments: attachments}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	snap.builtAt = time.Now()
	a.public.snap = snap
	return snap, nil
}

func (a *App) invalidatePublic() {
	a.public.mu.Lock()
	a.public.snap = nil
	a.public.mu.Unlock()
}

var (
	excerptFence     = regexp.MustCompile("(?s)```.*?```")
	excerptImage     = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)`)
	excerptLink      = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	excerptTag       = regexp.MustCompile(`<[^>]*>`)
	excerptPrefix    = regexp.MustCompile(`(?m)^[ \t]*(#{1,6}|>|[-*+]|\d+\.)[ \t]+`)
	excerptTableRule = regexp.MustCompile(`(?m)^[ \t]*\|?[ \t:|-]*-[ \t:|-]*\|?[ \t]*$`)
	excerptMarks     = strings.NewReplacer("**", "", "__", "", "~~", "", "`", "", "|", " ", "*", "")
)

// Excerpts are plain text derived from administrator Markdown; the page escapes
// them like any other text and never renders them as HTML.
func articleExcerpt(body string, limit int) string {
	text := excerptFence.ReplaceAllString(body, " ")
	text = excerptImage.ReplaceAllString(text, " ")
	text = excerptLink.ReplaceAllString(text, "$1")
	text = excerptTag.ReplaceAllString(text, " ")
	text = excerptTableRule.ReplaceAllString(text, " ")
	text = excerptPrefix.ReplaceAllString(text, "")
	text = excerptMarks.Replace(text)
	text = strings.Join(strings.Fields(text), " ")
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return strings.TrimSpace(string(runes[:limit])) + "…"
}

func (a *App) RegisterPublicSite(m *http.ServeMux) {
	m.HandleFunc("GET /api/public/home", a.publicHome)
	m.HandleFunc("GET /api/public/articles", a.publicArticles)
	m.HandleFunc("GET /api/public/articles/{id}", a.publicArticle)
	m.HandleFunc("GET /api/admin/site", a.adminSite)
	m.HandleFunc("PUT /api/admin/site", a.adminSite)
}

func (a *App) publicRequest(w http.ResponseWriter, r *http.Request) (*publicSnapshot, bool) {
	if !a.allow("public:"+a.rateIP(r), 300, time.Minute) {
		Fail(w, 429, "访问过于频繁，请稍后重试")
		return nil, false
	}
	snap, err := a.publicSnapshot()
	if err != nil {
		Fail(w, 503, "内容暂不可用，请稍后重试")
		return nil, false
	}
	return snap, true
}

func (a *App) publicHome(w http.ResponseWriter, r *http.Request) {
	snap, ok := a.publicRequest(w, r)
	if !ok {
		return
	}
	latest := []publicArticleSummary{}
	if snap.home.ShowLatest && snap.newsEnabled {
		latest = snap.articles[:min(snap.home.LatestCount, len(snap.articles))]
	}
	WriteJSON(w, 200, map[string]any{"home": snap.home, "site": snap.site(), "newsEnabled": snap.newsEnabled, "articles": latest})
}

func (a *App) publicArticles(w http.ResponseWriter, r *http.Request) {
	snap, ok := a.publicRequest(w, r)
	if !ok {
		return
	}
	WriteJSON(w, 200, map[string]any{"enabled": snap.newsEnabled, "site": snap.site(), "contactEmail": snap.home.ContactEmail, "articles": snap.articles})
}

func (a *App) publicArticle(w http.ResponseWriter, r *http.Request) {
	snap, ok := a.publicRequest(w, r)
	if !ok {
		return
	}
	article, found := snap.details[r.PathValue("id")]
	if !found {
		Fail(w, 404, "文章不存在或未公开")
		return
	}
	WriteJSON(w, 200, map[string]any{"article": article, "attachmentsEnabled": snap.attachmentsEnabled, "site": snap.site(), "contactEmail": snap.home.ContactEmail})
}

func (a *App) adminSite(w http.ResponseWriter, r *http.Request) {
	actor, err := a.Admin(r)
	if err != nil {
		Fail(w, 403, err.Error())
		return
	}
	if r.Method == "PUT" {
		var in struct {
			Home *HomeConfig `json:"home"`
			SEO  *SEOConfig  `json:"seo"`
		}
		if err := Decode(r, &in); err != nil || (in.Home == nil && in.SEO == nil) {
			Fail(w, 400, "请提交主页或 SEO 设置")
			return
		}
		var home HomeConfig
		var seo SEOConfig
		if in.Home != nil {
			if home, err = normalizeHome(*in.Home); err != nil {
				Fail(w, 400, err.Error())
				return
			}
		}
		if in.SEO != nil {
			if seo, err = normalizeSEO(*in.SEO); err != nil {
				Fail(w, 400, err.Error())
				return
			}
		}
		err = a.Store.Update(func(s *State) error {
			if in.Home != nil {
				s.Settings["homepage"] = home
				contentAudit(s, actor.ID, "site.home.save", "homepage")
			}
			if in.SEO != nil {
				s.Settings["seo"] = seo
				contentAudit(s, actor.ID, "site.seo.save", "seo")
			}
			return nil
		})
		if err != nil {
			Fail(w, 409, err.Error())
			return
		}
		a.invalidatePublic()
	}
	var out map[string]any
	err = a.Store.View(func(s *State) error {
		out = map[string]any{"home": loadHomeConfig(s), "seo": loadSEOConfig(s), "defaults": map[string]any{"home": defaultHomeConfig(), "seo": defaultSEOConfig()}}
		return nil
	})
	if err != nil {
		Fail(w, 500, "读取失败")
		return
	}
	WriteJSON(w, 200, out)
}
