package control

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// LauncherSettings describes the MSBOOST launcher download shown on the public
// homepage and in the console. Every field is public by design; an empty
// DownloadURL means the download has not been published yet.
type LauncherSettings struct {
	Version     string `json:"version"`
	ReleasedAt  string `json:"releasedAt"`
	DownloadURL string `json:"downloadUrl"`
	BackupURL   string `json:"backupUrl"`
	SHA256      string `json:"sha256"`
	Notes       string `json:"notes"`
	UpdatedAt   int64  `json:"updatedAt"`
}

var (
	launcherVersionPattern = regexp.MustCompile(`^[0-9A-Za-z.+-]{1,32}$`)
	launcherSHA256Pattern  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Download links point at the operator's own file share, whose share pages use
// fragments such as "#/?code=XXXX". Only https is accepted, the fragment is kept
// verbatim, and credentials or whitespace are refused.
func launcherURL(label, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if len(value) > 500 || strings.IndexFunc(value, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return "", fmt.Errorf("%s无效", label)
	}
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Opaque != "" {
		return "", fmt.Errorf("%s只接受 https:// 开头的链接", label)
	}
	if u.User != nil {
		return "", fmt.Errorf("%s不能包含用户名或密码", label)
	}
	return value, nil
}

func normalizeLauncher(in LauncherSettings) (LauncherSettings, error) {
	var out LauncherSettings
	var err error
	if out.DownloadURL, err = launcherURL("下载地址", in.DownloadURL); err != nil {
		return out, err
	}
	if out.BackupURL, err = launcherURL("备用下载地址", in.BackupURL); err != nil {
		return out, err
	}
	out.Version = strings.TrimSpace(in.Version)
	if out.Version != "" && !launcherVersionPattern.MatchString(out.Version) {
		return out, errors.New("当前版本只能用字母、数字和 . - +，最多 32 个字符")
	}
	out.ReleasedAt = strings.TrimSpace(in.ReleasedAt)
	if out.ReleasedAt != "" {
		if _, err := time.Parse("2006-01-02", out.ReleasedAt); err != nil {
			return out, errors.New("更新日期格式应为 YYYY-MM-DD")
		}
	}
	if out.DownloadURL != "" && (out.Version == "" || out.ReleasedAt == "") {
		return out, errors.New("填写了下载地址时，请同时填写当前版本和更新日期")
	}
	if out.BackupURL != "" && out.DownloadURL == "" {
		return out, errors.New("请先填写下载地址，再填写备用下载地址")
	}
	out.SHA256 = strings.ToLower(strings.TrimSpace(in.SHA256))
	if out.SHA256 != "" && !launcherSHA256Pattern.MatchString(out.SHA256) {
		return out, errors.New("SHA-256 应为 64 位十六进制")
	}
	notes := strings.TrimSpace(strings.ReplaceAll(in.Notes, "\r\n", "\n"))
	if utf8.RuneCountInString(notes) > 5000 {
		return out, errors.New("更新内容不能超过 5000 字")
	}
	if strings.IndexFunc(notes, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\t' }) >= 0 {
		return out, errors.New("更新内容不能包含控制字符")
	}
	out.Notes = notes
	out.UpdatedAt = in.UpdatedAt
	return out, nil
}

func loadLauncherSettings(s *State) LauncherSettings {
	return loadSiteSetting(s, "launcher", LauncherSettings{}, normalizeLauncher)
}

// The console page reads the same public snapshot as the homepage, so visitors
// and signed-in users always see one consistent download.
func (a *App) publicLauncher(w http.ResponseWriter, r *http.Request) {
	snap, ok := a.publicRequest(w, r)
	if !ok {
		return
	}
	WriteJSON(w, 200, map[string]any{"launcher": snap.launcher})
}
