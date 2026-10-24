package control

import (
	"errors"
	"strings"
)

// NewsNotice is the announcement at the top of 公告/教程. Administrators write
// it on that page; with Public it also opens the public news page. Turning it
// off keeps the text for later.
type NewsNotice struct {
	Enabled   bool   `json:"enabled"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	LinkText  string `json:"linkText"`
	LinkHref  string `json:"linkHref"`
	Public    bool   `json:"public"`
	UpdatedAt int64  `json:"updatedAt"`
}

func normalizeNewsNotice(in NewsNotice) (NewsNotice, error) {
	out := NewsNotice{Enabled: in.Enabled, Public: in.Public, UpdatedAt: in.UpdatedAt}
	var err error
	if out.Title, err = siteText("公告标题", in.Title, 40, false, false); err != nil {
		return out, err
	}
	if out.Body, err = siteText("公告内容", strings.ReplaceAll(in.Body, "\r\n", "\n"), 600, false, true); err != nil {
		return out, err
	}
	if out.LinkText, err = siteText("按钮文字", in.LinkText, 12, false, false); err != nil {
		return out, err
	}
	if out.LinkHref, err = siteHref("按钮链接", in.LinkHref); err != nil {
		return out, err
	}
	if (out.LinkText == "") != (out.LinkHref == "") {
		return out, errors.New("按钮文字和按钮链接要一起填写")
	}
	if out.Enabled && out.Title == "" && out.Body == "" {
		return out, errors.New("请填写公告标题或内容")
	}
	return out, nil
}

func loadNewsNotice(s *State) NewsNotice {
	return loadSiteSetting(s, "newsNotice", NewsNotice{}, normalizeNewsNotice)
}

// visibleTo returns what a reader may see: members see the notice while it
// is on, visitors only when it is also public. Nil hides it.
func (n NewsNotice) visibleTo(visitor bool) *NewsNotice {
	if !n.Enabled || (visitor && !n.Public) {
		return nil
	}
	return &n
}
