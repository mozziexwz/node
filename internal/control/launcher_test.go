package control

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestLauncherSettingsValidation(t *testing.T) {
	valid := LauncherSettings{Version: "v1.0.2", ReleasedAt: "2026-10-17", DownloadURL: "https://file.msboost.de/#/?code=AbC123", BackupURL: "https://file.msboost.de/#/?code=Backup", SHA256: strings.Repeat("AB", 32), Notes: "- 修复\r\n- 整理\t名单"}
	got, err := normalizeLauncher(valid)
	if err != nil {
		t.Fatal(err)
	}
	if got.DownloadURL != valid.DownloadURL || got.SHA256 != strings.Repeat("ab", 32) || got.Notes != "- 修复\n- 整理\t名单" {
		t.Fatalf("normalized launcher: %+v", got)
	}
	if empty, err := normalizeLauncher(LauncherSettings{}); err != nil || empty.DownloadURL != "" {
		t.Fatalf("an empty launcher must stay unpublished: %+v %v", empty, err)
	}
	for name, mutate := range map[string]func(*LauncherSettings){
		"plain http":          func(l *LauncherSettings) { l.DownloadURL = "http://file.msboost.de/#/?code=x" },
		"javascript":          func(l *LauncherSettings) { l.DownloadURL = "javascript:alert(1)" },
		"credentials":         func(l *LauncherSettings) { l.DownloadURL = "https://user:pass@file.msboost.de/" },
		"whitespace":          func(l *LauncherSettings) { l.DownloadURL = "https://file.msboost.de/a b" },
		"too long":            func(l *LauncherSettings) { l.DownloadURL = "https://file.msboost.de/" + strings.Repeat("a", 500) },
		"backup http":         func(l *LauncherSettings) { l.BackupURL = "http://mirror.test/x" },
		"backup only":         func(l *LauncherSettings) { l.DownloadURL = "" },
		"version characters":  func(l *LauncherSettings) { l.Version = "v1.0 beta" },
		"version too long":    func(l *LauncherSettings) { l.Version = "v" + strings.Repeat("1", 32) },
		"missing version":     func(l *LauncherSettings) { l.Version = "" },
		"missing date":        func(l *LauncherSettings) { l.ReleasedAt = "" },
		"invalid date":        func(l *LauncherSettings) { l.ReleasedAt = "2026-13-01" },
		"short sha256":        func(l *LauncherSettings) { l.SHA256 = "abc" },
		"non-hex sha256":      func(l *LauncherSettings) { l.SHA256 = strings.Repeat("zz", 32) },
		"notes too long":      func(l *LauncherSettings) { l.Notes = strings.Repeat("字", 5001) },
		"notes control chars": func(l *LauncherSettings) { l.Notes = "a\u0000b" },
	} {
		bad := valid
		mutate(&bad)
		if _, err := normalizeLauncher(bad); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

func TestLauncherIsPublicAndOnlyAdminsChangeIt(t *testing.T) {
	a, mux, h, admin, member := publicFixture(t)
	a.RegisterIdentity(mux)
	var public struct {
		Launcher LauncherSettings `json:"launcher"`
	}
	if w := publicGet(t, h, "/api/public/launcher"); w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &public) != nil || public.Launcher.DownloadURL != "" {
		t.Fatalf("unpublished launcher: %d %s", w.Code, w.Body.String())
	}
	launcher := map[string]any{"version": "v1.0.2", "releasedAt": "2026-10-17", "downloadUrl": "https://file.msboost.de/#/?code=XXXX", "sha256": strings.Repeat("0f", 32), "notes": "- 修复 Steam 登录"}
	if w := contentCall(t, mux, member, "PUT", "/api/admin/settings", map[string]any{"launcher": launcher}); w.Code != 403 {
		t.Fatalf("member changed the launcher: %d", w.Code)
	}
	bad := map[string]any{"version": "v1.0.2", "releasedAt": "2026-10-17", "downloadUrl": "http://file.msboost.de/"}
	if w := contentCall(t, mux, admin, "PUT", "/api/admin/settings", map[string]any{"launcher": bad}); w.Code != 400 {
		t.Fatalf("http download accepted: %d %s", w.Code, w.Body.String())
	}
	w := contentCall(t, mux, admin, "PUT", "/api/admin/settings", map[string]any{"launcher": launcher})
	var saved struct {
		Launcher LauncherSettings `json:"launcher"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &saved) != nil || saved.Launcher.Version != "v1.0.2" || saved.Launcher.UpdatedAt == 0 {
		t.Fatalf("save launcher: %d %s", w.Code, w.Body.String())
	}
	// The public snapshot is refreshed at once, for the console page and the homepage.
	if w := publicGet(t, h, "/api/public/launcher"); json.Unmarshal(w.Body.Bytes(), &public) != nil || public.Launcher.DownloadURL != "https://file.msboost.de/#/?code=XXXX" {
		t.Fatalf("published launcher not served: %s", w.Body.String())
	}
	if w := publicGet(t, h, "/api/public/home"); !strings.Contains(w.Body.String(), `"downloadUrl":"https://file.msboost.de/#/?code=XXXX"`) {
		t.Fatalf("homepage misses the launcher: %s", w.Body.String())
	}
	audited := false
	_ = a.Store.View(func(s *State) error {
		for _, entry := range s.Docs["audit"] {
			audited = audited || strings.Contains(string(entry), `"action":"settings.update"`) && strings.Contains(string(entry), admin.ID)
		}
		return nil
	})
	if !audited {
		t.Fatal("launcher change was not audited")
	}
	// A tampered stored value is dropped instead of reaching visitors.
	_ = a.Store.Update(func(s *State) error {
		s.Settings["launcher"] = map[string]any{"version": "v9", "releasedAt": "2026-10-17", "downloadUrl": "javascript:alert(1)"}
		return nil
	})
	a.invalidatePublic()
	if w := publicGet(t, h, "/api/public/launcher"); strings.Contains(w.Body.String(), "javascript:") {
		t.Fatalf("invalid stored launcher served: %s", w.Body.String())
	}
}

func TestLocalAppIsPublishedSeparately(t *testing.T) {
	a, mux, h, admin, member := publicFixture(t)
	a.RegisterIdentity(mux)
	var public struct {
		Launcher LauncherSettings `json:"launcher"`
		LocalApp LauncherSettings `json:"localApp"`
	}
	if w := publicGet(t, h, "/api/public/launcher"); w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &public) != nil || public.LocalApp.DownloadURL != "" {
		t.Fatalf("unpublished local app: %d %s", w.Code, w.Body.String())
	}
	local := map[string]any{"version": "v1.0.0", "releasedAt": "2026-11-05", "downloadUrl": "https://file.msboost.de/#/?code=Local", "sha256": strings.Repeat("1a", 32)}
	if w := contentCall(t, mux, member, "PUT", "/api/admin/settings", map[string]any{"localApp": local}); w.Code != 403 {
		t.Fatalf("member changed the local app: %d", w.Code)
	}
	if w := contentCall(t, mux, admin, "PUT", "/api/admin/settings", map[string]any{"localApp": "v1"}); w.Code != 400 || !strings.Contains(w.Body.String(), "本地版设置格式无效") {
		t.Fatalf("malformed local app: %d %s", w.Code, w.Body.String())
	}
	bad := map[string]any{"version": "v1.0.0", "releasedAt": "2026-11-05", "downloadUrl": "javascript:alert(1)"}
	if w := contentCall(t, mux, admin, "PUT", "/api/admin/settings", map[string]any{"localApp": bad}); w.Code != 400 {
		t.Fatalf("javascript download accepted: %d %s", w.Code, w.Body.String())
	}
	w := contentCall(t, mux, admin, "PUT", "/api/admin/settings", map[string]any{"localApp": local})
	var saved struct {
		Launcher LauncherSettings `json:"launcher"`
		LocalApp LauncherSettings `json:"localApp"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &saved) != nil || saved.LocalApp.Version != "v1.0.0" || saved.LocalApp.UpdatedAt == 0 || saved.Launcher.DownloadURL != "" {
		t.Fatalf("save local app: %d %s", w.Code, w.Body.String())
	}
	if w := publicGet(t, h, "/api/public/launcher"); json.Unmarshal(w.Body.Bytes(), &public) != nil || public.LocalApp.DownloadURL != "https://file.msboost.de/#/?code=Local" || public.Launcher.DownloadURL != "" {
		t.Fatalf("published local app not served: %s", w.Body.String())
	}
	if w := publicGet(t, h, "/api/public/home"); !strings.Contains(w.Body.String(), `"localApp":{"version":"v1.0.0"`) {
		t.Fatalf("homepage misses the local app: %s", w.Body.String())
	}
	// Feature cards may scroll to the local app section.
	home := defaultHomeConfig()
	home.Features = append([]HomeFeature{}, home.Features...)
	home.Features[2].Target = "#local"
	if w := contentCall(t, mux, admin, "PUT", "/api/admin/site", map[string]any{"home": home}); w.Code != 200 {
		t.Fatalf("#local card target refused: %s", w.Body.String())
	}
}

func TestHomeCardTargetsAndExplanations(t *testing.T) {
	_, mux, h, admin, _ := publicFixture(t)
	var home struct {
		Home HomeConfig `json:"home"`
	}
	if w := publicGet(t, h, "/api/public/home"); json.Unmarshal(w.Body.Bytes(), &home) != nil {
		t.Fatal(w.Body.String())
	}
	if home.Home.TitleLine1 != "一键部署你的MapleStory" || !home.Home.ShowExplain || !home.Home.ShowLauncher || len(home.Home.Relay.Via) != 4 {
		t.Fatalf("default home: %+v", home.Home)
	}
	targets := []string{}
	for _, feature := range home.Home.Features {
		targets = append(targets, feature.Target)
	}
	if strings.Join(targets, ",") != "#msboost,#relay,#launcher" {
		t.Fatalf("default card targets: %v", targets)
	}
	copyHome := func() HomeConfig {
		c := defaultHomeConfig()
		c.Features = append([]HomeFeature{}, c.Features...)
		c.MSBOOST.Nodes = append([]HomeItem{}, c.MSBOOST.Nodes...)
		c.MSBOOST.Points = append([]HomeItem{}, c.MSBOOST.Points...)
		c.Relay.Via = append([]HomeItem{}, c.Relay.Via...)
		c.Relay.Points = append([]HomeItem{}, c.Relay.Points...)
		return c
	}
	for name, mutate := range map[string]func(*HomeConfig){
		"unknown section":     func(c *HomeConfig) { c.Features[0].Target = "#dd" },
		"javascript target":   func(c *HomeConfig) { c.Features[1].Target = "javascript:alert(1)" },
		"http target":         func(c *HomeConfig) { c.Features[2].Target = "http://evil.test" },
		"two diagram boxes":   func(c *HomeConfig) { c.MSBOOST.Nodes = c.MSBOOST.Nodes[:2] },
		"five relayed boxes":  func(c *HomeConfig) { c.Relay.Via = append(c.Relay.Via, HomeItem{Title: "多一个"}) },
		"box without title":   func(c *HomeConfig) { c.Relay.Via[1] = HomeItem{Desc: "只有说明"} },
		"no points":           func(c *HomeConfig) { c.MSBOOST.Points = nil },
		"four points":         func(c *HomeConfig) { c.Relay.Points = append(c.Relay.Points, HomeItem{Title: "第四条"}) },
		"long point":          func(c *HomeConfig) { c.MSBOOST.Points[0].Desc = strings.Repeat("长", 121) },
		"empty section title": func(c *HomeConfig) { c.Relay.Title = " " },
		"long lead":           func(c *HomeConfig) { c.MSBOOST.Lead = strings.Repeat("字", 301) },
		"control character":   func(c *HomeConfig) { c.Relay.Truth = "a\u0007b" },
	} {
		bad := copyHome()
		mutate(&bad)
		if w := contentCall(t, mux, admin, "PUT", "/api/admin/site", map[string]any{"home": bad}); w.Code != 400 {
			t.Fatalf("%s accepted: %d %s", name, w.Code, w.Body.String())
		}
	}
	good := copyHome()
	good.Features[2].Target = "/news"
	good.ShowExplain = false
	good.Relay.Myth, good.Relay.Truth = "", ""
	if w := contentCall(t, mux, admin, "PUT", "/api/admin/site", map[string]any{"home": good}); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w := publicGet(t, h, "/api/public/home"); json.Unmarshal(w.Body.Bytes(), &home) != nil || home.Home.ShowExplain || home.Home.Features[2].Target != "/news" || home.Home.Version != homeConfigVersion {
		t.Fatalf("saved home: %s", w.Body.String())
	}
}

func TestLegacyHomeUpgradesOnlyUntouchedDefaults(t *testing.T) {
	a, mux, h, admin, _ := publicFixture(t)
	legacy := map[string]any{
		"eyebrow":    "SELF-HOSTED · MAPLESTORY",
		"titleLine1": "一键部署你的",
		"titleLine2": "独立IP游戏节点",
		"subtitle":   "我们自己改过的副标题",
		"features": []map[string]any{
			{"title": "一键部署 MSBOOST", "desc": "部署你的独立IP游戏节点"},
			{"title": "一键配置中转服务器", "desc": "改过的中转说明"},
			{"title": "一键DD系统", "desc": "在线重装系统"},
			{"title": "工单", "desc": "联系我们"},
		},
		"showLatest":  true,
		"latestCount": 3,
	}
	_ = a.Store.Update(func(s *State) error { s.Settings["homepage"] = legacy; return nil })
	a.invalidatePublic()
	var home struct {
		Home HomeConfig `json:"home"`
	}
	if w := publicGet(t, h, "/api/public/home"); json.Unmarshal(w.Body.Bytes(), &home) != nil {
		t.Fatal(w.Body.String())
	}
	got := home.Home
	if got.TitleLine1 != "一键部署你的MapleStory" || got.Subtitle != "我们自己改过的副标题" || !got.ShowExplain || !got.ShowLauncher {
		t.Fatalf("legacy texts: %+v", got)
	}
	want := []HomeFeature{
		{Title: "一键部署 MSBOOST", Desc: "部署你的VPS独立IP游戏节点", Target: "#msboost"},
		{Title: "一键配置中转服务器", Desc: "改过的中转说明", Target: "#relay"},
		{Title: "MSBOOST 启动器", Desc: "一键加速 MapleStory", Target: "#launcher"},
		{Title: "工单", Desc: "联系我们"},
	}
	if len(got.Features) != len(want) {
		t.Fatalf("legacy cards: %+v", got.Features)
	}
	for i := range want {
		if got.Features[i] != want[i] {
			t.Fatalf("legacy card %d: %+v, want %+v", i, got.Features[i], want[i])
		}
	}
	// Once saved, the administrator's choice stands even if it matches an old default.
	saved := got
	saved.TitleLine1 = "一键部署你的"
	if w := contentCall(t, mux, admin, "PUT", "/api/admin/site", map[string]any{"home": saved}); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w := publicGet(t, h, "/api/public/home"); json.Unmarshal(w.Body.Bytes(), &home) != nil || home.Home.TitleLine1 != "一键部署你的" {
		t.Fatalf("saved title upgraded again: %s", w.Body.String())
	}
}
