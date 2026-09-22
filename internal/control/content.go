package control

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
)

const attachmentMaxBytes = 10 << 20
const ticketMaxMessages = 200

type Article struct {
	ID          string       `json:"id"`
	Title       string       `json:"title"`
	Category    string       `json:"category"`
	Body        string       `json:"body"`
	Published   bool         `json:"published"`
	Pinned      bool         `json:"pinned"`
	Sort        int          `json:"sort"`
	CreatedAt   int64        `json:"createdAt"`
	UpdatedAt   int64        `json:"updatedAt"`
	Attachments []Attachment `json:"attachments"`
}
type Attachment struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Size int    `json:"size"`
	Type string `json:"type"`
}
type attachmentObject struct {
	ArticleID string     `json:"articleId"`
	Metadata  Attachment `json:"metadata"`
	Bytes     []byte     `json:"bytes"`
}
type TicketReply struct {
	ID        string `json:"id"`
	Author    string `json:"author"`
	Admin     bool   `json:"admin"`
	Body      string `json:"body"`
	CreatedAt int64  `json:"createdAt"`
}
type Ticket struct {
	ID              string        `json:"id"`
	UserID          string        `json:"userId"`
	Title           string        `json:"title"`
	Status          string        `json:"status"`
	CreatedAt       int64         `json:"createdAt"`
	Replies         []TicketReply `json:"replies"`
	UpdatedAt       int64         `json:"updatedAt"`
	LastReplyRole   string        `json:"lastReplyRole"`
	LastReplyAuthor string        `json:"lastReplyAuthor"`
	LastReplyAt     int64         `json:"lastReplyAt"`
	LastActivityAt  int64         `json:"lastActivityAt"`
	ReplyCount      int           `json:"replyCount"`
	MemberReadAt    int64         `json:"memberReadAt,omitempty"`
	AdminReadAt     int64         `json:"adminReadAt,omitempty"`
	UnreadCount     int           `json:"unreadCount"`
}

func (a *App) RegisterContent(m *http.ServeMux) {
	m.HandleFunc("GET /api/articles", a.listArticles)
	m.HandleFunc("GET /api/admin/articles", a.listArticles)
	m.HandleFunc("POST /api/admin/articles", a.saveArticle)
	m.HandleFunc("PUT /api/admin/articles/{id}", a.saveArticle)
	m.HandleFunc("POST /api/admin/articles/{id}/move", a.moveArticle)
	m.HandleFunc("POST /api/admin/articles/{id}/pin", a.pinArticle)
	m.HandleFunc("DELETE /api/admin/articles/{id}", a.deleteArticle)
	m.HandleFunc("POST /api/admin/articles/{id}/attachments", a.uploadAttachment)
	m.HandleFunc("GET /api/articles/{article}/attachments/{id}", a.downloadAttachment)
	m.HandleFunc("DELETE /api/admin/articles/{article}/attachments/{id}", a.deleteAttachment)
	m.HandleFunc("GET /api/tickets", a.listTickets)
	m.HandleFunc("POST /api/tickets", a.createTicket)
	m.HandleFunc("POST /api/tickets/{id}/replies", a.replyTicket)
	m.HandleFunc("POST /api/tickets/{id}/read", a.readTicket)
	m.HandleFunc("PATCH /api/tickets/{id}", a.editTicket)
	m.HandleFunc("GET /api/admin/overview", a.overview)
	m.HandleFunc("GET /api/admin/audit", a.auditList)
}
func (a *App) listArticles(w http.ResponseWriter, r *http.Request) {
	admin := strings.HasPrefix(r.URL.Path, "/api/admin/")
	user, _ := a.User(r)
	if admin {
		if user == nil || user.Role != "admin" {
			Fail(w, 403, "仅管理员可访问")
			return
		}
	}
	out := []Article{}
	err := a.Store.View(func(s *State) error {
		if !admin && !boolSetting(s, "publicArticles") && user == nil {
			return errors.New("请登录后阅读")
		}
		for _, v := range ListDocs[Article](s, "articles") {
			if admin || v.Published {
				out = append(out, v)
			}
		}
		return nil
	})
	if err != nil {
		Fail(w, 401, err.Error())
		return
	}
	sortArticles(out)
	WriteJSON(w, 200, map[string]any{"articles": out})
}
func (a *App) saveArticle(w http.ResponseWriter, r *http.Request) {
	u, err := a.Admin(r)
	if err != nil {
		Fail(w, 403, err.Error())
		return
	}
	var v Article
	if Decode(r, &v) != nil || len(strings.TrimSpace(v.Title)) < 1 || len(v.Title) > 240 || len(v.Category) > 80 || len(v.Body) > 512000 {
		Fail(w, 400, "文章标题或正文格式无效")
		return
	}
	v.Title = strings.TrimSpace(v.Title)
	v.Category = strings.TrimSpace(v.Category)
	v.ID = r.PathValue("id")
	if v.ID == "" {
		v.ID = ID()
	}
	v.UpdatedAt = time.Now().UnixMilli()
	err = a.Store.Update(func(s *State) error {
		old, ok := LoadDoc[Article](s, "articles", v.ID)
		if r.Method == "PUT" && !ok {
			return errors.New("文章不存在")
		}
		v.Attachments = old.Attachments
		if ok {
			// An unrelated edit or stale editor must not undo a newer reordering.
			v.Sort = old.Sort
			v.Pinned = old.Pinned
			v.CreatedAt = old.CreatedAt
		} else {
			v.Pinned = false // Pinning is an explicit, independent operation.
			v.CreatedAt = v.UpdatedAt
			articles := ListDocs[Article](s, "articles")
			sortArticles(articles)
			// Preserve existing manual/legacy order, but put every new article at
			// the beginning of the unpinned group (newest first by default).
			index := 0
			for index < len(articles) && articles[index].Pinned {
				index++
			}
			articles = append(articles, Article{})
			copy(articles[index+1:], articles[index:])
			articles[index] = v
			if err := saveArticleOrder(s, articles); err != nil {
				return err
			}
			v = articles[index]
		}
		contentAudit(s, u.ID, "article.save", v.ID)
		return SaveDoc(s, "articles", v.ID, v)
	})
	if err != nil {
		Fail(w, 409, err.Error())
		return
	}
	WriteJSON(w, 200, v)
}

func sortArticles(articles []Article) {
	sort.Slice(articles, func(i, j int) bool {
		if articles[i].Pinned != articles[j].Pinned {
			return articles[i].Pinned
		}
		if articles[i].Sort != articles[j].Sort {
			return articles[i].Sort < articles[j].Sort
		}
		if articleCreationTime(articles[i]) != articleCreationTime(articles[j]) {
			return articleCreationTime(articles[i]) > articleCreationTime(articles[j])
		}
		return articles[i].ID < articles[j].ID
	})
}

func articleCreationTime(article Article) int64 {
	if article.CreatedAt != 0 {
		return article.CreatedAt
	}
	return article.UpdatedAt // Old backups do not contain createdAt.
}

func saveArticleOrder(s *State, articles []Article) error {
	for i := range articles {
		articles[i].Sort = i
		if err := SaveDoc(s, "articles", articles[i].ID, articles[i]); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) moveArticle(w http.ResponseWriter, r *http.Request) {
	u, err := a.Admin(r)
	if err != nil {
		Fail(w, 403, err.Error())
		return
	}
	var in struct {
		Direction string `json:"direction"`
	}
	if Decode(r, &in) != nil || (in.Direction != "up" && in.Direction != "down" && in.Direction != "top" && in.Direction != "bottom") {
		Fail(w, 400, "请选择上移、下移、移到最顶或移到最底")
		return
	}
	var out []Article
	err = a.Store.Update(func(s *State) error {
		out = ListDocs[Article](s, "articles")
		sortArticles(out)
		index := -1
		for i, v := range out {
			if v.ID == r.PathValue("id") {
				index = i
				break
			}
		}
		if index < 0 {
			return errors.New("文章不存在")
		}
		first, last := index, index
		for first > 0 && out[first-1].Pinned == out[index].Pinned {
			first--
		}
		for last+1 < len(out) && out[last+1].Pinned == out[index].Pinned {
			last++
		}
		target := index
		switch in.Direction {
		case "up":
			target = max(first, index-1)
		case "down":
			target = min(last, index+1)
		case "top":
			target = first
		case "bottom":
			target = last
		}
		moveArticleIndex(out, index, target)
		if err := saveArticleOrder(s, out); err != nil {
			return err
		}
		contentAudit(s, u.ID, "article.move."+in.Direction, r.PathValue("id"))
		return nil
	})
	if err != nil {
		Fail(w, 409, err.Error())
		return
	}
	WriteJSON(w, 200, map[string]any{"articles": out})
}

// Shifting (rather than swapping endpoints) retains every other article's order.
func moveArticleIndex(articles []Article, from, to int) {
	article := articles[from]
	if from < to {
		copy(articles[from:to], articles[from+1:to+1])
	} else if from > to {
		copy(articles[to+1:from+1], articles[to:from])
	}
	articles[to] = article
}

func (a *App) pinArticle(w http.ResponseWriter, r *http.Request) {
	u, err := a.Admin(r)
	if err != nil {
		Fail(w, 403, err.Error())
		return
	}
	var in struct {
		Pinned *bool `json:"pinned"`
	}
	if Decode(r, &in) != nil || in.Pinned == nil {
		Fail(w, 400, "请选择是否固定到顶部")
		return
	}
	var out []Article
	err = a.Store.Update(func(s *State) error {
		out = ListDocs[Article](s, "articles")
		sortArticles(out)
		index := -1
		for i := range out {
			if out[i].ID == r.PathValue("id") {
				index = i
				break
			}
		}
		if index < 0 {
			return errors.New("文章不存在")
		}
		if out[index].Pinned != *in.Pinned {
			out[index].Pinned = *in.Pinned
			// A changed pin status enters the top of its new group. Repeated
			// identical requests are idempotent and do not reorder that group.
			target := 0
			if !*in.Pinned {
				for _, article := range out {
					if article.Pinned {
						target++
					}
				}
			}
			moveArticleIndex(out, index, target)
		}
		if err := saveArticleOrder(s, out); err != nil {
			return err
		}
		contentAudit(s, u.ID, "article.pin", r.PathValue("id"))
		return nil
	})
	if err != nil {
		Fail(w, 409, err.Error())
		return
	}
	WriteJSON(w, 200, map[string]any{"articles": out})
}
func (a *App) deleteArticle(w http.ResponseWriter, r *http.Request) {
	u, err := a.Admin(r)
	if err != nil {
		Fail(w, 403, err.Error())
		return
	}
	err = a.Store.Update(func(s *State) error {
		v, ok := LoadDoc[Article](s, "articles", r.PathValue("id"))
		if !ok {
			return errors.New("文章不存在")
		}
		for _, f := range v.Attachments {
			// A restored/stale association must not delete another article's file.
			if object, ok := LoadDoc[attachmentObject](s, "attachments", f.ID); ok && object.ArticleID == v.ID {
				DeleteDoc(s, "attachments", f.ID)
			}
		}
		DeleteDoc(s, "articles", v.ID)
		contentAudit(s, u.ID, "article.delete", v.ID)
		return nil
	})
	if err != nil {
		Fail(w, 404, err.Error())
		return
	}
	WriteJSON(w, 200, map[string]bool{"ok": true})
}
func (a *App) uploadAttachment(w http.ResponseWriter, r *http.Request) {
	u, err := a.Admin(r)
	if err != nil {
		Fail(w, 403, err.Error())
		return
	}
	// Multipart framing is outside the file-size allowance. Large files spill to
	// private temporary storage and are always removed after this request.
	r.Body = http.MaxBytesReader(w, r.Body, attachmentMaxBytes+(64<<10))
	parseErr := r.ParseMultipartForm(1 << 20)
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	if parseErr != nil {
		Fail(w, 400, "附件不得超过 10 MiB，且请求必须是有效的文件上传")
		return
	}
	files := 0
	for _, items := range r.MultipartForm.File {
		files += len(items)
	}
	if files != 1 || len(r.MultipartForm.File["file"]) != 1 {
		Fail(w, 400, "每次只能上传一个附件")
		return
	}
	f, h, err := r.FormFile("file")
	if err != nil {
		Fail(w, 400, "请选择附件")
		return
	}
	defer f.Close()
	name := filepath.Base(strings.ReplaceAll(h.Filename, "\\", "/"))
	ext := strings.ToLower(filepath.Ext(name))
	allow := map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".pdf": true, ".txt": true, ".md": true, ".zip": true, ".docx": true}
	if !allow[ext] || len(name) > 180 || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		Fail(w, 400, "允许图片、PDF、文本、DOCX 与审核后的 ZIP 文件")
		return
	}
	b, err := io.ReadAll(io.LimitReader(f, attachmentMaxBytes+1))
	if err != nil {
		Fail(w, 400, "无法读取附件")
		return
	}
	if len(b) > attachmentMaxBytes {
		Fail(w, http.StatusRequestEntityTooLarge, "附件不得超过 10 MiB")
		return
	}
	v := Attachment{ID: ID(), Name: name, Size: len(b), Type: http.DetectContentType(b)}
	err = a.Store.Update(func(s *State) error {
		article, ok := LoadDoc[Article](s, "articles", r.PathValue("id"))
		if !ok {
			return errors.New("文章不存在")
		}
		if len(article.Attachments) >= 20 {
			return errors.New("每篇文章最多20个附件")
		}
		article.Attachments = append(article.Attachments, v)
		if err := SaveDoc(s, "attachments", v.ID, attachmentObject{ArticleID: article.ID, Metadata: v, Bytes: b}); err != nil {
			return err
		}
		contentAudit(s, u.ID, "attachment.upload", v.ID)
		return SaveDoc(s, "articles", article.ID, article)
	})
	if err != nil {
		Fail(w, 409, err.Error())
		return
	}
	WriteJSON(w, 201, v)
}
func (a *App) downloadAttachment(w http.ResponseWriter, r *http.Request) {
	u, _ := a.User(r)
	var file attachmentObject
	err := a.Store.View(func(s *State) error {
		admin := u != nil && u.Role == "admin"
		if !admin && !boolSetting(s, "attachments") {
			return errors.New("附件下载已关闭")
		}
		if u == nil && !boolSetting(s, "publicArticles") {
			return errors.New("请登录")
		}
		article, ok := LoadDoc[Article](s, "articles", r.PathValue("article"))
		if !ok || (!article.Published && !admin) {
			return errors.New("文章不存在")
		}
		file, ok = LoadDoc[attachmentObject](s, "attachments", r.PathValue("id"))
		if !ok || file.ArticleID != article.ID {
			return errors.New("附件不存在")
		}
		return nil
	})
	if err != nil {
		Fail(w, 403, err.Error())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": file.Metadata.Name}))
	w.Write(file.Bytes)
}
func (a *App) deleteAttachment(w http.ResponseWriter, r *http.Request) {
	u, err := a.Admin(r)
	if err != nil {
		Fail(w, 403, err.Error())
		return
	}
	err = a.Store.Update(func(s *State) error {
		article, ok := LoadDoc[Article](s, "articles", r.PathValue("article"))
		if !ok {
			return errors.New("文章不存在")
		}
		kept := []Attachment{}
		found := false
		for _, v := range article.Attachments {
			if v.ID != r.PathValue("id") {
				kept = append(kept, v)
			} else {
				object, ok := LoadDoc[attachmentObject](s, "attachments", v.ID)
				if ok && object.ArticleID != article.ID {
					return errors.New("附件不属于此文章")
				}
				found = true
				DeleteDoc(s, "attachments", v.ID)
			}
		}
		if !found {
			return errors.New("附件不存在")
		}
		article.Attachments = kept
		contentAudit(s, u.ID, "attachment.delete", r.PathValue("id"))
		return SaveDoc(s, "articles", article.ID, article)
	})
	if err != nil {
		Fail(w, 404, err.Error())
		return
	}
	WriteJSON(w, 200, map[string]bool{"ok": true})
}
func (a *App) listTickets(w http.ResponseWriter, r *http.Request) {
	u, err := a.User(r)
	if err != nil {
		Fail(w, 401, err.Error())
		return
	}
	out := []Ticket{}
	unreadCount := 0
	err = a.Store.View(func(s *State) error {
		for _, t := range ListDocs[Ticket](s, "tickets") {
			if u.Role == "admin" || t.UserID == u.ID {
				ticketMetadata(&t)
				t.UnreadCount = ticketUnreadCount(t, u.Role == "admin")
				unreadCount += t.UnreadCount
				out = append(out, t)
			}
		}
		return nil
	})
	if err != nil {
		Fail(w, 500, "读取失败")
		return
	}
	sort.Slice(out, func(i, j int) bool {
		left, right := ticketPriority(out[i]), ticketPriority(out[j])
		if left != right {
			return left < right
		}
		if out[i].LastActivityAt != out[j].LastActivityAt {
			return out[i].LastActivityAt > out[j].LastActivityAt
		}
		return out[i].ID < out[j].ID
	})
	WriteJSON(w, 200, map[string]any{"tickets": out, "unreadCount": unreadCount})
}
func (a *App) createTicket(w http.ResponseWriter, r *http.Request) {
	u, err := a.User(r)
	if err != nil {
		Fail(w, 401, err.Error())
		return
	}
	if u.Role == "admin" {
		Fail(w, 403, "管理员不能新建工单，请直接回复会员工单")
		return
	}
	var in struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	if Decode(r, &in) != nil || len(strings.TrimSpace(in.Title)) < 1 || len(in.Title) > 180 || len(strings.TrimSpace(in.Body)) < 1 || len(in.Body) > 10000 {
		Fail(w, 400, "请填写标题和内容")
		return
	}
	in.Title = strings.TrimSpace(in.Title)
	if !a.allow("ticket:"+u.ID, 10, time.Hour) {
		Fail(w, 429, "提交过于频繁")
		return
	}
	now := time.Now().UnixMilli()
	v := Ticket{ID: ID(), UserID: u.ID, Title: in.Title, Status: "open", CreatedAt: now, UpdatedAt: now, MemberReadAt: now, Replies: []TicketReply{{ID: ID(), Author: u.Email, Body: in.Body, CreatedAt: now}}}
	ticketMetadata(&v)
	if err := a.Store.Update(func(s *State) error { return SaveDoc(s, "tickets", v.ID, v) }); err != nil {
		Fail(w, 500, "保存失败")
		return
	}
	WriteJSON(w, 201, v)
}
func (a *App) replyTicket(w http.ResponseWriter, r *http.Request) {
	u, err := a.User(r)
	if err != nil {
		Fail(w, 401, err.Error())
		return
	}
	var in struct {
		Body string `json:"body"`
	}
	if Decode(r, &in) != nil || len(strings.TrimSpace(in.Body)) < 1 || len(in.Body) > 10000 {
		Fail(w, 400, "回复内容无效")
		return
	}
	if !a.allow("ticket-reply:"+u.ID, 20, time.Minute) {
		Fail(w, 429, "回复过于频繁，请稍后重试")
		return
	}
	err = a.Store.Update(func(s *State) error {
		v, ok := LoadDoc[Ticket](s, "tickets", r.PathValue("id"))
		if !ok || (u.Role != "admin" && v.UserID != u.ID) {
			return errors.New("工单不存在")
		}
		if v.Status == "closed" {
			return errors.New("工单已关闭")
		}
		if len(v.Replies) >= ticketMaxMessages {
			return errors.New("单个工单最多200条消息，请新建工单继续")
		}
		v.UpdatedAt = max(time.Now().UnixMilli(), v.UpdatedAt+1)
		v.Replies = append(v.Replies, TicketReply{ID: ID(), Author: u.Email, Admin: u.Role == "admin", Body: in.Body, CreatedAt: v.UpdatedAt})
		if u.Role == "admin" {
			v.AdminReadAt = v.UpdatedAt
		} else {
			v.MemberReadAt = v.UpdatedAt
		}
		ticketMetadata(&v)
		return SaveDoc(s, "tickets", v.ID, v)
	})
	if err != nil {
		Fail(w, 409, err.Error())
		return
	}
	WriteJSON(w, 200, map[string]bool{"ok": true})
}

func (a *App) readTicket(w http.ResponseWriter, r *http.Request) {
	u, err := a.User(r)
	if err != nil {
		Fail(w, 401, err.Error())
		return
	}
	err = a.Store.Update(func(s *State) error {
		v, ok := LoadDoc[Ticket](s, "tickets", r.PathValue("id"))
		if !ok || u.Role != "admin" && v.UserID != u.ID {
			return errors.New("工单不存在")
		}
		now := time.Now().UnixMilli()
		readThrough := max(now, v.UpdatedAt)
		if len(v.Replies) > 0 {
			readThrough = max(readThrough, v.Replies[len(v.Replies)-1].CreatedAt)
		}
		if u.Role == "admin" {
			v.AdminReadAt = max(v.AdminReadAt, readThrough)
		} else {
			v.MemberReadAt = max(v.MemberReadAt, readThrough)
		}
		return SaveDoc(s, "tickets", v.ID, v)
	})
	if err != nil {
		Fail(w, 404, err.Error())
		return
	}
	WriteJSON(w, 200, map[string]any{"ok": true, "unreadCount": 0})
}
func (a *App) editTicket(w http.ResponseWriter, r *http.Request) {
	u, err := a.User(r)
	if err != nil {
		Fail(w, 401, err.Error())
		return
	}
	var in struct {
		Status string `json:"status"`
	}
	if Decode(r, &in) != nil || (in.Status != "closed" && in.Status != "open") {
		Fail(w, 400, "工单状态无效")
		return
	}
	err = a.Store.Update(func(s *State) error {
		v, ok := LoadDoc[Ticket](s, "tickets", r.PathValue("id"))
		if !ok || (u.Role != "admin" && v.UserID != u.ID) {
			return errors.New("工单不存在")
		}
		v.Status = in.Status
		v.UpdatedAt = time.Now().UnixMilli()
		ticketMetadata(&v)
		return SaveDoc(s, "tickets", v.ID, v)
	})
	if err != nil {
		Fail(w, 404, err.Error())
		return
	}
	WriteJSON(w, 200, map[string]bool{"ok": true})
}

// Legacy tickets derive these fields from their existing replies, so no
// migration or fabricated reply timestamps are needed.
func ticketMetadata(t *Ticket) {
	t.ReplyCount = max(0, len(t.Replies)-1)
	t.LastReplyAt = 0
	t.LastReplyRole = ""
	t.LastReplyAuthor = ""
	t.LastActivityAt = max(t.CreatedAt, t.UpdatedAt)
	if len(t.Replies) > 0 {
		last := t.Replies[len(t.Replies)-1]
		t.LastReplyAt = last.CreatedAt
		t.LastReplyRole = "user"
		if last.Admin {
			t.LastReplyRole = "admin"
		}
		t.LastReplyAuthor = last.Author
		t.LastActivityAt = max(t.LastActivityAt, last.CreatedAt)
	}
}
func ticketUnreadCount(t Ticket, admin bool) int {
	readAt := t.MemberReadAt
	if admin {
		readAt = t.AdminReadAt
	}
	count := 0
	for _, reply := range t.Replies {
		if reply.CreatedAt > readAt && reply.Admin != admin {
			count++
		}
	}
	return count
}
func ticketPriority(t Ticket) int {
	if t.Status == "closed" {
		return 3
	}
	if t.ReplyCount == 0 {
		return 2
	}
	if t.LastReplyRole == "admin" {
		return 0
	}
	return 1
}
func contentAudit(s *State, uid, action, id string) {
	_ = SaveDoc(s, "content_audit", ID(), map[string]any{"userId": uid, "action": action, "objectId": id, "createdAt": time.Now().UnixMilli()})
}
func (a *App) overview(w http.ResponseWriter, r *http.Request) {
	if _, err := a.Admin(r); err != nil {
		Fail(w, 403, err.Error())
		return
	}
	out := map[string]any{}
	err := a.Store.View(func(s *State) error {
		out["users"] = len(s.Users)
		out["tasks"] = len(s.Docs["tasks"])
		out["routes"] = len(s.Docs["routes"])
		out["orders"] = len(s.Docs["orders"])
		out["tickets"] = len(s.Docs["tickets"])
		out["version"] = a.Config.Version
		out["database"] = "transactional"
		return nil
	})
	if err != nil {
		Fail(w, 500, "读取失败")
		return
	}
	WriteJSON(w, 200, out)
}
func (a *App) auditList(w http.ResponseWriter, r *http.Request) {
	if _, err := a.Admin(r); err != nil {
		Fail(w, 403, err.Error())
		return
	}
	items := []json.RawMessage{}
	err := a.Store.View(func(s *State) error {
		for _, col := range []string{"content_audit", "audit", "commerce_audit"} {
			for _, raw := range s.Docs[col] {
				items = append(items, raw)
			}
		}
		return nil
	})
	if err != nil {
		Fail(w, 500, "审计记录读取失败")
		return
	}
	WriteJSON(w, 200, map[string]any{"events": items})
}
