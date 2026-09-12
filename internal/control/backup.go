package control

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mozziexwz/node/internal/executor"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	_ "time/tzdata"
)

type BackupPlan struct {
	Enabled       bool     `json:"enabled"`
	Mode          string   `json:"mode"`
	Time          string   `json:"time"`
	Hours         int      `json:"hours"`
	Weekday       int      `json:"weekday"`
	Timezone      string   `json:"timezone"`
	RetentionDays int      `json:"retentionDays"`
	MinCopies     int      `json:"minCopies"`
	Targets       []string `json:"targets"`
	NextAt        int64    `json:"nextAt"`
	LastAt        int64    `json:"lastAt"`
}
type BackupTarget struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
	User        string `json:"user"`
	Path        string `json:"path"`
	Fingerprint string `json:"fingerprint"`
	PrivateKey  string `json:"privateKey,omitempty"`
	SealedKey   string `json:"sealedKey,omitempty"`
	Enabled     bool   `json:"enabled"`
}
type BackupRecord struct {
	ID        string            `json:"id"`
	CreatedAt int64             `json:"createdAt"`
	Size      int               `json:"size"`
	SHA256    string            `json:"sha256"`
	Status    string            `json:"status"`
	Targets   map[string]string `json:"targets"`
	Error     string            `json:"error,omitempty"`
}
type BackupEnvelope struct {
	Version   int    `json:"version"`
	CreatedAt int64  `json:"createdAt"`
	SHA256    string `json:"sha256"`
	Sealed    string `json:"sealed"`
}
type BackupService struct {
	app *App
	mu  sync.Mutex
}

func NewBackupService(a *App) *BackupService { return &BackupService{app: a} }
func (b *BackupService) Register(m *http.ServeMux) {
	m.HandleFunc("GET /api/admin/backups", b.list)
	m.HandleFunc("POST /api/admin/backups", b.create)
	m.HandleFunc("GET /api/admin/backups/{id}/download", b.download)
	m.HandleFunc("GET /api/admin/backup-plan", b.getPlan)
	m.HandleFunc("PUT /api/admin/backup-plan", b.savePlan)
	m.HandleFunc("GET /api/admin/backup-targets", b.targets)
	m.HandleFunc("POST /api/admin/backup-targets", b.saveTarget)
	m.HandleFunc("PUT /api/admin/backup-targets/{id}", b.saveTarget)
	m.HandleFunc("DELETE /api/admin/backup-targets/{id}", b.deleteTarget)
	m.HandleFunc("POST /api/admin/backups/preflight", b.preflight)
	m.HandleFunc("POST /api/admin/backups/restore", b.restore)
	m.HandleFunc("POST /api/admin/backups/retention", b.retention)
}
func (b *BackupService) admin(w http.ResponseWriter, r *http.Request) bool {
	if _, err := b.app.Admin(r); err != nil {
		Fail(w, 403, err.Error())
		return false
	}
	return true
}
func backupNext(p BackupPlan, now time.Time) (int64, error) {
	loc, err := time.LoadLocation(p.Timezone)
	if err != nil {
		return 0, errors.New("时区无效")
	}
	now = now.In(loc)
	if p.Mode == "hours" {
		if p.Hours < 1 || p.Hours > 168 {
			return 0, errors.New("间隔须为1–168小时")
		}
		return now.Add(time.Duration(p.Hours) * time.Hour).UnixMilli(), nil
	}
	hm, err := time.Parse("15:04", p.Time)
	if err != nil {
		return 0, errors.New("时间须为HH:MM")
	}
	candidate := time.Date(now.Year(), now.Month(), now.Day(), hm.Hour(), hm.Minute(), 0, 0, loc)
	if !candidate.After(now) {
		candidate = candidate.AddDate(0, 0, 1)
	}
	if p.Mode == "weekly" {
		if p.Weekday < 0 || p.Weekday > 6 {
			return 0, errors.New("星期无效")
		}
		for int(candidate.Weekday()) != p.Weekday {
			candidate = candidate.AddDate(0, 0, 1)
		}
	} else if p.Mode != "daily" {
		return 0, errors.New("备份频率无效")
	}
	return candidate.UnixMilli(), nil
}
func (b *BackupService) getPlan(w http.ResponseWriter, r *http.Request) {
	if !b.admin(w, r) {
		return
	}
	var p BackupPlan
	_ = b.app.Store.View(func(s *State) error { p, _ = LoadDoc[BackupPlan](s, "backup_plans", "default"); return nil })
	if p.Mode == "" {
		p = BackupPlan{Mode: "daily", Time: "02:30", Timezone: "Asia/Shanghai", Hours: 6, RetentionDays: 30, MinCopies: 3, Targets: []string{}}
	}
	WriteJSON(w, 200, p)
}
func (b *BackupService) savePlan(w http.ResponseWriter, r *http.Request) {
	if !b.admin(w, r) {
		return
	}
	var p BackupPlan
	if Decode(r, &p) != nil || p.RetentionDays < 1 || p.MinCopies < 1 || p.MinCopies > 1000 {
		Fail(w, 400, "备份计划字段无效")
		return
	}
	next, err := backupNext(p, time.Now())
	if err != nil {
		Fail(w, 400, err.Error())
		return
	}
	p.NextAt = next
	err = b.app.Store.Update(func(s *State) error {
		for _, id := range p.Targets {
			if _, ok := LoadDoc[BackupTarget](s, "backup_targets", id); !ok {
				return errors.New("备份目标不存在")
			}
		}
		return SaveDoc(s, "backup_plans", "default", p)
	})
	if err != nil {
		Fail(w, 400, err.Error())
		return
	}
	WriteJSON(w, 200, p)
}
func (b *BackupService) targets(w http.ResponseWriter, r *http.Request) {
	if !b.admin(w, r) {
		return
	}
	out := []BackupTarget{}
	_ = b.app.Store.View(func(s *State) error { out = ListDocs[BackupTarget](s, "backup_targets"); return nil })
	for i := range out {
		out[i].SealedKey = ""
		out[i].PrivateKey = ""
	}
	WriteJSON(w, 200, map[string]any{"targets": out})
}
func (b *BackupService) saveTarget(w http.ResponseWriter, r *http.Request) {
	if !b.admin(w, r) {
		return
	}
	var v BackupTarget
	if Decode(r, &v) != nil || executor.PublicIP(v.Host) != nil || v.Port < 1 || v.Port > 65535 || v.User == "" || !strings.HasPrefix(v.Fingerprint, "SHA256:") || !path.IsAbs(v.Path) || path.Clean(v.Path) == "/" || len(v.Name) > 100 {
		Fail(w, 400, "SFTP 目标、目录或指纹格式无效")
		return
	}
	v.ID = r.PathValue("id")
	if v.ID == "" {
		v.ID = ID()
	}
	v.Path = path.Clean(v.Path)
	err := b.app.Store.Update(func(s *State) error {
		old, exists := LoadDoc[BackupTarget](s, "backup_targets", v.ID)
		if r.Method == "PUT" && !exists {
			return errors.New("目标不存在")
		}
		v.SealedKey = old.SealedKey
		if v.PrivateKey != "" {
			if _, err := ssh.ParsePrivateKey([]byte(v.PrivateKey)); err != nil {
				return errors.New("SSH 私钥格式无效（请使用备份专用密钥）")
			}
			sealed, err := b.app.Seal([]byte(v.PrivateKey))
			if err != nil {
				return err
			}
			v.SealedKey = sealed
		}
		if v.SealedKey == "" {
			return errors.New("请填写备份专用 SSH 私钥")
		}
		v.PrivateKey = ""
		return SaveDoc(s, "backup_targets", v.ID, v)
	})
	if err != nil {
		Fail(w, 400, err.Error())
		return
	}
	v.SealedKey = ""
	WriteJSON(w, 200, v)
}
func (b *BackupService) deleteTarget(w http.ResponseWriter, r *http.Request) {
	if !b.admin(w, r) {
		return
	}
	err := b.app.Store.Update(func(s *State) error {
		p, _ := LoadDoc[BackupPlan](s, "backup_plans", "default")
		for _, id := range p.Targets {
			if id == r.PathValue("id") {
				return errors.New("先从自动备份计划移除该目标")
			}
		}
		DeleteDoc(s, "backup_targets", r.PathValue("id"))
		return nil
	})
	if err != nil {
		Fail(w, 409, err.Error())
		return
	}
	WriteJSON(w, 200, map[string]bool{"ok": true})
}
func (b *BackupService) pack(s *State) ([]byte, error) {
	raw, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	sealed, err := b.app.Seal(raw)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	return json.Marshal(BackupEnvelope{Version: 1, CreatedAt: time.Now().UnixMilli(), SHA256: hex.EncodeToString(sum[:]), Sealed: sealed})
}
func (b *BackupService) unpack(raw []byte) (*State, error) {
	if len(raw) > 100<<20 {
		return nil, errors.New("备份超过100 MB")
	}
	var envelope BackupEnvelope
	if json.Unmarshal(raw, &envelope) != nil || envelope.Version != 1 {
		return nil, errors.New("备份格式或版本不受支持")
	}
	clear, err := b.app.Open(envelope.Sealed)
	if err != nil {
		return nil, errors.New("备份解密失败，请使用创建该备份时的主密钥")
	}
	sum := sha256.Sum256(clear)
	if hex.EncodeToString(sum[:]) != envelope.SHA256 {
		return nil, errors.New("备份完整性校验失败")
	}
	var s State
	if json.Unmarshal(clear, &s) != nil || s.Users == nil || s.Docs == nil || s.Settings == nil {
		return nil, errors.New("备份数据结构不完整")
	}
	hasAdmin := false
	for _, u := range s.Users {
		if u.Role == "admin" && u.Status == "active" {
			hasAdmin = true
		}
	}
	if !hasAdmin {
		return nil, errors.New("备份中没有可用管理员")
	}
	return &s, nil
}
func (b *BackupService) write(raw []byte) (BackupRecord, error) {
	id := ID()
	dir := filepath.Join(b.app.Config.DataDir, "backups")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return BackupRecord{}, err
	}
	file := filepath.Join(dir, id+".msb")
	f, err := os.OpenFile(file, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return BackupRecord{}, err
	}
	_, err = f.Write(raw)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return BackupRecord{}, err
	}
	sum := sha256.Sum256(raw)
	return BackupRecord{ID: id, CreatedAt: time.Now().UnixMilli(), Size: len(raw), SHA256: hex.EncodeToString(sum[:]), Status: "verified", Targets: map[string]string{"local": "verified"}}, nil
}
func (b *BackupService) run(ctx context.Context) (BackupRecord, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var raw []byte
	targets := []BackupTarget{}
	err := b.app.Store.View(func(s *State) error {
		var e error
		raw, e = b.pack(s)
		p, _ := LoadDoc[BackupPlan](s, "backup_plans", "default")
		for _, id := range p.Targets {
			v, ok := LoadDoc[BackupTarget](s, "backup_targets", id)
			if ok && v.Enabled {
				targets = append(targets, v)
			}
		}
		return e
	})
	if err != nil {
		return BackupRecord{}, err
	}
	record, err := b.write(raw)
	if err != nil {
		return record, err
	}
	for _, target := range targets {
		if err := b.upload(ctx, target, record.ID, raw); err != nil {
			record.Targets[target.ID] = "failed"
			record.Status = "partial"
			record.Error = "远程上传或完整性校验失败"
		} else {
			record.Targets[target.ID] = "verified"
		}
	}
	err = b.app.Store.Update(func(s *State) error { return SaveDoc(s, "backups", record.ID, record) })
	return record, err
}
func (b *BackupService) upload(ctx context.Context, t BackupTarget, id string, raw []byte) error {
	if err := executor.PublicIP(t.Host); err != nil {
		return err
	}
	plain, err := b.app.Open(t.SealedKey)
	if err != nil {
		return err
	}
	signer, err := ssh.ParsePrivateKey(plain)
	if err != nil {
		return err
	}
	addr := net.JoinHostPort(t.Host, fmt.Sprint(t.Port))
	cfg := &ssh.ClientConfig{User: t.User, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, Timeout: 15 * time.Second, HostKeyCallback: func(_ string, _ net.Addr, k ssh.PublicKey) error {
		if ssh.FingerprintSHA256(k) != t.Fingerprint {
			return errors.New("SFTP host key changed")
		}
		return nil
	}}
	conn, err := (&net.Dialer{Timeout: 15 * time.Second}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Minute))
	cc, ch, rq, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		return err
	}
	client := ssh.NewClient(cc, ch, rq)
	defer client.Close()
	sf, err := sftp.NewClient(client)
	if err != nil {
		return err
	}
	defer sf.Close()
	info, err := sf.Stat(t.Path)
	if err != nil || !info.IsDir() {
		return errors.New("remote backup directory unavailable")
	}
	temporary := path.Join(t.Path, "msboost-"+id+".partial")
	f, err := sf.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY)
	if err != nil {
		return err
	}
	if _, err = f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	read, err := sf.Open(temporary)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(h, io.LimitReader(read, int64(len(raw))+1))
	read.Close()
	sum := sha256.Sum256(raw)
	if err != nil || hex.EncodeToString(h.Sum(nil)) != hex.EncodeToString(sum[:]) {
		return errors.New("remote checksum mismatch")
	}
	return sf.Rename(temporary, path.Join(t.Path, "msboost-"+id+".msb"))
}
func (b *BackupService) list(w http.ResponseWriter, r *http.Request) {
	if !b.admin(w, r) {
		return
	}
	out := []BackupRecord{}
	_ = b.app.Store.View(func(s *State) error { out = ListDocs[BackupRecord](s, "backups"); return nil })
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	WriteJSON(w, 200, map[string]any{"backups": out})
}
func (b *BackupService) create(w http.ResponseWriter, r *http.Request) {
	if !b.admin(w, r) {
		return
	}
	record, err := b.run(r.Context())
	if err != nil {
		Fail(w, 500, "备份创建失败，请检查存储空间与权限")
		return
	}
	WriteJSON(w, 201, record)
}
func (b *BackupService) download(w http.ResponseWriter, r *http.Request) {
	if !b.admin(w, r) {
		return
	}
	var record BackupRecord
	_ = b.app.Store.View(func(s *State) error { record, _ = LoadDoc[BackupRecord](s, "backups", r.PathValue("id")); return nil })
	if record.ID == "" {
		Fail(w, 404, "备份不存在")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", "attachment; filename=msboost-"+record.ID+".msb")
	http.ServeFile(w, r, filepath.Join(b.app.Config.DataDir, "backups", record.ID+".msb"))
}
func (b *BackupService) readUpload(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	r.Body = http.MaxBytesReader(w, r.Body, 100<<20)
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		return nil, err
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, 100<<20))
}
func (b *BackupService) preflight(w http.ResponseWriter, r *http.Request) {
	if !b.admin(w, r) {
		return
	}
	raw, err := b.readUpload(w, r)
	if err != nil {
		Fail(w, 400, "读取备份失败")
		return
	}
	s, err := b.unpack(raw)
	if err != nil {
		Fail(w, 400, err.Error())
		return
	}
	hash := sha256.Sum256(raw)
	WriteJSON(w, 200, map[string]any{"valid": true, "sha256": hex.EncodeToString(hash[:]), "users": len(s.Users), "collections": len(s.Docs), "message": "恢复将覆盖平台数据，退出全部会话、撤销Agent令牌并暂停线路；需维护模式且无运行任务。"})
}
func (b *BackupService) restore(w http.ResponseWriter, r *http.Request) {
	if !b.admin(w, r) {
		return
	}
	raw, err := b.readUpload(w, r)
	if err != nil {
		Fail(w, 400, "读取备份失败")
		return
	}
	hash := sha256.Sum256(raw)
	if r.FormValue("confirm") != "RESTORE" || r.FormValue("sha256") != hex.EncodeToString(hash[:]) {
		Fail(w, 400, "请先预检，并明确确认该备份的SHA256")
		return
	}
	restored, err := b.unpack(raw)
	if err != nil {
		Fail(w, 400, err.Error())
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	var rollback BackupRecord
	err = b.app.Store.Update(func(s *State) error {
		if !boolSetting(s, "maintenance") {
			return errors.New("请先开启维护模式")
		}
		if err := validateFinancialRestore(s, restored); err != nil {
			return err
		}
		for _, raw := range s.Docs["tasks"] {
			var t map[string]any
			_ = json.Unmarshal(raw, &t)
			if t["state"] == "running" || t["state"] == "queued" {
				return errors.New("存在未结束的任务")
			}
		}
		for _, r := range ListDocs[UserRule](s, "user_rules") {
			for _, seg := range r.Segments {
				if seg.LastLease > time.Now().UnixMilli() {
					return errors.New("请先暂停全部本站转发并等待租约失效")
				}
			}
		}
		current, e := b.pack(s)
		if e != nil {
			return e
		}
		rollback, e = b.write(current)
		if e != nil {
			return e
		}
		restored.Sessions = map[string]*Session{}
		restored.Settings["maintenance"] = true
		for _, name := range []string{"executor_agents", "executors", "relay_agents"} {
			delete(restored.Docs, name)
		}
		if err := prepareRestoredRelay(restored, time.Now().UnixMilli()); err != nil {
			return err
		}
		for key, raw := range restored.Docs["tasks"] {
			var task map[string]any
			if json.Unmarshal(raw, &task) == nil && (task["state"] == "running" || task["state"] == "queued") {
				task["state"] = "interrupted"
				task["message"] = "备份恢复后任务不自动重放"
				if err := SaveDoc(restored, "tasks", key, task); err != nil {
					return err
				}
			}
		}
		p, _ := LoadDoc[BackupPlan](restored, "backup_plans", "default")
		p.Enabled = false
		if err := SaveDoc(restored, "backup_plans", "default", p); err != nil {
			return err
		}
		*s = *restored
		if err := SaveDoc(s, "backups", rollback.ID, rollback); err != nil {
			return err
		}
		contentAudit(s, "system", "backup.restore", rollback.ID)
		return nil
	})
	if err != nil {
		Fail(w, 409, err.Error())
		return
	}
	WriteJSON(w, 200, map[string]any{"ok": true, "rollbackId": rollback.ID, "message": "恢复完成。请重新登录，核对支付流水并重新关联Agent后解除维护。"})
}

// Online restore cannot rewind settled money, issued cards, order idempotency
// or metered entitlements. A differing snapshot needs an offline reconciliation
// workflow; silently merging individual financial rows is not safe either.
func validateFinancialRestore(current, restored *State) error {
	collections := []string{"orders", "ledger", "cards", "order_requests", "redeem_requests", "payment_trades", "payment_callbacks", "entitlement_versions", "payment_channels", "traffic_cursors", "traffic_months"}
	for _, name := range collections {
		if len(current.Docs[name]) == 0 && len(restored.Docs[name]) == 0 {
			continue
		}
		before, err := json.Marshal(current.Docs[name])
		if err != nil {
			return err
		}
		after, err := json.Marshal(restored.Docs[name])
		if err != nil {
			return err
		}
		if !bytes.Equal(before, after) {
			return errors.New("备份与当前财务或流量记录不一致，禁止在线回滚余额、卡密、订单或计量；请保留当前数据并执行离线对账恢复")
		}
	}
	type financialUser struct{ BalanceCents, ExpiresAt, TrafficTotal, TrafficUsed, RateMbps int64 }
	financialUsers := func(s *State) map[string]financialUser {
		out := map[string]financialUser{}
		for id, u := range s.Users {
			if u != nil && (u.BalanceCents != 0 || u.ExpiresAt != 0 || u.TrafficTotal != 0 || u.TrafficUsed != 0) {
				out[id] = financialUser{u.BalanceCents, u.ExpiresAt, u.TrafficTotal, u.TrafficUsed, u.RateMbps}
			}
		}
		return out
	}
	before, err := json.Marshal(financialUsers(current))
	if err != nil {
		return err
	}
	after, err := json.Marshal(financialUsers(restored))
	if err != nil {
		return err
	}
	if !bytes.Equal(before, after) {
		return errors.New("用户余额或套餐权益已变化，旧快照不能在线覆盖，请先离线对账")
	}
	return nil
}

func prepareRestoredRelay(s *State, now int64) error {
	for _, rule := range ListDocs[UserRule](s, "user_rules") {
		key := rule.UserID + ":" + rule.RouteID
		u := s.Users[rule.UserID]
		if u == nil || u.ExpiresAt <= now || rule.SealedConfig == "" {
			DeleteDoc(s, "user_rules", key)
			continue
		}
		rule.State = "paused"
		rule.Segments = nil
		rule.DeleteAfter = 0
		rule.Version++
		if err := SaveDoc(s, "user_rules", key, rule); err != nil {
			return err
		}
	}
	for _, route := range ListDocs[Route](s, "routes") {
		route.Enabled = false
		route.Online = false
		if err := SaveDoc(s, "routes", route.ID, route); err != nil {
			return err
		}
	}
	for user := range s.Docs["user_targets"] {
		u := s.Users[user]
		if u == nil || u.ExpiresAt <= now {
			DeleteDoc(s, "user_targets", user)
		}
	}
	return nil
}
func (b *BackupService) retention(w http.ResponseWriter, r *http.Request) {
	if !b.admin(w, r) {
		return
	}
	var in struct {
		Confirm bool `json:"confirm"`
	}
	if Decode(r, &in) != nil {
		Fail(w, 400, "请求无效")
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	candidates := []string{}
	err := b.app.Store.Update(func(s *State) error {
		p, ok := LoadDoc[BackupPlan](s, "backup_plans", "default")
		if !ok || p.MinCopies < 1 {
			return errors.New("请先配置保留策略")
		}
		all := ListDocs[BackupRecord](s, "backups")
		sort.Slice(all, func(i, j int) bool { return all[i].CreatedAt > all[j].CreatedAt })
		protected := 0
		for _, v := range all {
			if v.Status != "verified" {
				continue
			}
			if !b.verifiedLocalBackup(v) {
				continue
			}
			protected++
			if protected <= p.MinCopies || v.CreatedAt > time.Now().AddDate(0, 0, -p.RetentionDays).UnixMilli() {
				continue
			}
			candidates = append(candidates, v.ID)
			if in.Confirm {
				file := filepath.Join(b.app.Config.DataDir, "backups", v.ID+".msb")
				if filepath.Base(file) != v.ID+".msb" {
					return errors.New("invalid backup ID")
				}
				if err := os.Remove(file); err != nil && !os.IsNotExist(err) {
					return err
				}
				v.Targets["local"] = "removed"
				v.Status = "local_removed"
				if err := SaveDoc(s, "backups", v.ID, v); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		Fail(w, 409, err.Error())
		return
	}
	WriteJSON(w, 200, map[string]any{"ids": candidates, "deleted": in.Confirm, "scope": "local"})
}
func (b *BackupService) verifiedLocalBackup(record BackupRecord) bool {
	if record.ID == "" {
		return false
	}
	for _, c := range record.ID {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	file := filepath.Join(b.app.Config.DataDir, "backups", record.ID+".msb")
	info, err := os.Lstat(file)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 100<<20 {
		return false
	}
	f, err := os.Open(file)
	if err != nil {
		return false
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 100<<20+1))
	if err != nil || len(raw) > 100<<20 {
		return false
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != record.SHA256 {
		return false
	}
	_, err = b.unpack(raw)
	return err == nil
}
func (b *BackupService) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				var due bool
				_ = b.app.Store.Update(func(s *State) error {
					p, ok := LoadDoc[BackupPlan](s, "backup_plans", "default")
					if !ok || !p.Enabled || p.NextAt > time.Now().UnixMilli() {
						return nil
					}
					next, err := backupNext(p, time.Now())
					if err != nil {
						return err
					}
					p.LastAt = time.Now().UnixMilli()
					p.NextAt = next
					due = true
					return SaveDoc(s, "backup_plans", "default", p)
				})
				if due {
					if _, err := b.run(ctx); err != nil {
						_ = b.app.Store.Update(func(s *State) error {
							return SaveDoc(s, "backups", ID(), BackupRecord{ID: ID(), CreatedAt: time.Now().UnixMilli(), Status: "failed", Error: "备份失败，请检查存储与远程目标"})
						})
					}
				}
			}
		}
	}()
}
