package control

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
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
	ID             string `json:"id"`
	Name           string `json:"name"`
	Host           string `json:"host"`
	Port           int    `json:"port"`
	User           string `json:"user"`
	Path           string `json:"path"`
	Fingerprint    string `json:"fingerprint"`
	AuthMode       string `json:"authMode"`
	PrivateKey     string `json:"privateKey,omitempty"`
	SealedKey      string `json:"sealedKey,omitempty"`
	Password       string `json:"password,omitempty"`
	SealedPassword string `json:"sealedPassword,omitempty"`
	Enabled        bool   `json:"enabled"`
}
type BackupRecord struct {
	ID        string            `json:"id"`
	CreatedAt int64             `json:"createdAt"`
	Size      int               `json:"size"`
	SHA256    string            `json:"sha256"`
	Status    string            `json:"status"`
	Targets   map[string]string `json:"targets"`
	Error     string            `json:"error,omitempty"`
	Protected bool              `json:"protected,omitempty"`
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
	// Tests can redirect the already-validated public address to a loopback SSH
	// fixture. Production always uses the standard context-aware TCP dialer.
	dialContext func(context.Context, string, string) (net.Conn, error)
}

func NewBackupService(a *App) *BackupService { return &BackupService{app: a} }
func (b *BackupService) Register(m *http.ServeMux) {
	m.HandleFunc("GET /api/admin/backups", b.list)
	m.HandleFunc("POST /api/admin/backups", b.create)
	m.HandleFunc("GET /api/admin/backups/{id}/download", b.download)
	m.HandleFunc("DELETE /api/admin/backups/{id}", b.deleteBackup)
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
		out[i] = publicBackupTarget(out[i])
	}
	WriteJSON(w, 200, map[string]any{"targets": out})
}
func (b *BackupService) saveTarget(w http.ResponseWriter, r *http.Request) {
	if !b.admin(w, r) {
		return
	}
	var v BackupTarget
	if Decode(r, &v) != nil {
		Fail(w, 400, "SFTP 目标字段无效")
		return
	}
	if v.User == "" {
		v.User = "root"
	}
	if v.Path == "" {
		v.Path = "/root/msboost-backup"
	}
	v.AuthMode = backupAuthMode(v.AuthMode)
	if validateBackupTarget(v) != nil {
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
		// Encrypted values from the request are never accepted as credentials.
		v.SealedKey, v.SealedPassword = "", ""
		sameMode := exists && backupAuthMode(old.AuthMode) == v.AuthMode
		switch v.AuthMode {
		case "private_key":
			if v.Password != "" {
				return errors.New("私钥认证不能同时提交 SSH 密码")
			}
			if v.PrivateKey != "" {
				if len(v.PrivateKey) > 100<<10 {
					return errors.New("SSH 私钥过大")
				}
				if _, err := ssh.ParsePrivateKey([]byte(v.PrivateKey)); err != nil {
					return errors.New("SSH 私钥格式无效（请使用备份专用密钥）")
				}
				sealed, err := b.app.Seal([]byte(v.PrivateKey))
				if err != nil {
					return errors.New("SSH 凭据加密失败")
				}
				v.SealedKey = sealed
			} else if sameMode {
				v.SealedKey = old.SealedKey
			}
			if v.SealedKey == "" {
				return errors.New("新增或切换私钥认证时，请填写备份专用 SSH 私钥")
			}
		case "password":
			if v.PrivateKey != "" {
				return errors.New("密码认证不能同时提交 SSH 私钥")
			}
			if v.Password != "" {
				if len(v.Password) > 512 || strings.ContainsAny(v.Password, "\x00\r\n") {
					return errors.New("SSH 密码格式无效")
				}
				sealed, err := b.app.Seal([]byte(v.Password))
				if err != nil {
					return errors.New("SSH 凭据加密失败")
				}
				v.SealedPassword = sealed
			} else if sameMode {
				v.SealedPassword = old.SealedPassword
			}
			if v.SealedPassword == "" {
				return errors.New("新增或切换密码认证时，请填写 SSH 密码")
			}
		}
		v.PrivateKey, v.Password = "", ""
		return SaveDoc(s, "backup_targets", v.ID, v)
	})
	if err != nil {
		Fail(w, 400, err.Error())
		return
	}
	WriteJSON(w, 200, publicBackupTarget(v))
}

func backupAuthMode(mode string) string {
	if mode == "" {
		return "private_key"
	}
	return mode
}

func publicBackupTarget(v BackupTarget) BackupTarget {
	v.AuthMode = backupAuthMode(v.AuthMode)
	v.PrivateKey, v.Password, v.SealedKey, v.SealedPassword = "", "", "", ""
	return v
}

func validateBackupTarget(v BackupTarget) error {
	if executor.PublicIP(v.Host) != nil || v.Port < 1 || v.Port > 65535 || v.User == "" || len(v.User) > 64 || strings.ContainsAny(v.User, " \t\r\n\x00/") || len(v.Name) > 100 {
		return errors.New("SFTP 目标无效")
	}
	if mode := backupAuthMode(v.AuthMode); mode != "private_key" && mode != "password" {
		return errors.New("SSH 认证方式无效")
	}
	encoded := strings.TrimPrefix(v.Fingerprint, "SHA256:")
	decoded, err := base64.RawStdEncoding.Strict().DecodeString(encoded)
	if !strings.HasPrefix(v.Fingerprint, "SHA256:") || err != nil || len(decoded) != sha256.Size || base64.RawStdEncoding.EncodeToString(decoded) != encoded {
		return errors.New("SSH 主机指纹必须是完整 SHA256 指纹")
	}
	if !path.IsAbs(v.Path) || path.Clean(v.Path) == "/" || len(v.Path) > 4096 || strings.ContainsAny(v.Path, "\x00\r\n\\") {
		return errors.New("远程备份目录无效")
	}
	for _, part := range strings.Split(v.Path, "/") {
		if part == "." || part == ".." {
			return errors.New("远程备份目录不能包含相对路径")
		}
	}
	return nil
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
	packed, err := json.Marshal(BackupEnvelope{Version: 1, CreatedAt: time.Now().UnixMilli(), SHA256: hex.EncodeToString(sum[:]), Sealed: sealed})
	if len(packed) > 100<<20 {
		return nil, errors.New("加密备份超过当前 100 MB 恢复上限，不能生成不可恢复的副本")
	}
	return packed, err
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
	if json.Unmarshal(clear, &s) != nil || s.Users == nil || s.Docs == nil || s.Settings == nil || s.Sessions == nil {
		return nil, errors.New("备份数据结构不完整")
	}
	hasAdmin := false
	for id, u := range s.Users {
		if u == nil || u.ID != id {
			return nil, errors.New("备份用户身份关系无效")
		}
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
	if err := validateBackupTarget(t); err != nil {
		return err
	}
	if !validBackupID(id) {
		return errors.New("备份标识无效")
	}
	var auth ssh.AuthMethod
	if backupAuthMode(t.AuthMode) == "password" {
		plain, err := b.app.Open(t.SealedPassword)
		if err != nil || len(plain) == 0 {
			return errors.New("SSH 密码解密失败")
		}
		auth = ssh.Password(string(plain))
		clear(plain)
	} else {
		plain, err := b.app.Open(t.SealedKey)
		if err != nil {
			return errors.New("SSH 私钥解密失败")
		}
		signer, err := ssh.ParsePrivateKey(plain)
		clear(plain)
		if err != nil {
			return errors.New("SSH 私钥格式无效")
		}
		auth = ssh.PublicKeys(signer)
	}
	addr := net.JoinHostPort(t.Host, fmt.Sprint(t.Port))
	cfg := &ssh.ClientConfig{User: t.User, Auth: []ssh.AuthMethod{auth}, Timeout: 15 * time.Second, HostKeyCallback: executor.PinnedHostKey(t.Fingerprint)}
	dial := (&net.Dialer{Timeout: 15 * time.Second}).DialContext
	if b.dialContext != nil {
		dial = b.dialContext
	}
	conn, err := dial(ctx, "tcp", addr)
	if err != nil {
		return errors.New("SFTP 连接失败")
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Minute))
	cc, ch, rq, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		return errors.New("SFTP 主机指纹核对或 SSH 认证失败")
	}
	client := ssh.NewClient(cc, ch, rq)
	defer client.Close()
	sf, err := sftp.NewClient(client)
	if err != nil {
		return err
	}
	defer sf.Close()
	var selfUID *uint32
	if t.User == "root" {
		uid := uint32(0)
		selfUID = &uid
	}
	if err := ensureBackupDirectory(sf, t.Path, selfUID); err != nil {
		return err
	}
	final := path.Join(t.Path, "msboost-"+id+".msb")
	if _, err := sf.Lstat(final); !os.IsNotExist(err) {
		return errors.New("remote backup destination already exists or is unavailable")
	}
	temporary := path.Join(t.Path, "msboost-"+id+".partial")
	f, err := sf.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY)
	if err != nil {
		return err
	}
	// Remove only this exclusively created partial file on failure. Existing
	// backup files and directories never enter the cleanup scope.
	published := false
	defer func() {
		if !published && selfUID != nil && ensureBackupDirectory(sf, t.Path, selfUID) == nil {
			_ = sf.Remove(temporary)
		}
	}()
	if err = f.Chmod(0600); err != nil {
		_ = f.Close()
		return errors.New("remote backup file permissions unavailable")
	}
	info, statErr := f.Stat()
	if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() != 0 {
		_ = f.Close()
		return errors.New("remote backup file permissions not private")
	}
	uid, ownerErr := executor.SFTPFileUID(info)
	if ownerErr != nil || selfUID != nil && *selfUID != uid {
		_ = f.Close()
		return errors.New("remote backup account ownership unavailable")
	}
	selfUID = &uid
	if err := ensureBackupDirectory(sf, t.Path, selfUID); err != nil {
		_ = f.Close()
		return err
	}
	if n, writeErr := f.Write(raw); writeErr != nil || n != len(raw) {
		_ = f.Close()
		return errors.New("remote backup write failed")
	}
	if err = f.Close(); err != nil {
		return err
	}
	if info, err := sf.Lstat(temporary); err != nil || !info.Mode().IsRegular() || executor.CheckSFTPOwner(info, uid, true) != nil {
		return errors.New("remote backup file changed")
	}
	read, err := sf.Open(temporary)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(h, io.LimitReader(read, int64(len(raw))+1))
	closeErr := read.Close()
	sum := sha256.Sum256(raw)
	if err != nil || closeErr != nil || hex.EncodeToString(h.Sum(nil)) != hex.EncodeToString(sum[:]) {
		return errors.New("remote checksum mismatch")
	}
	if err := ensureBackupDirectory(sf, t.Path, selfUID); err != nil {
		return err
	}
	if err = sf.Rename(temporary, final); err != nil {
		return err
	}
	published = true
	return nil
}

// SFTP paths are walked with Lstat: MkdirAll/Stat would follow symlinks in
// intermediate components. Only missing directories are chmod'd; an existing
// shared parent is never modified. Reject writable parents to prevent other
// accounts from swapping our destination after the checks.
func ensureBackupDirectory(sf *sftp.Client, directory string, selfUID *uint32) error {
	current := "/"
	if selfUID != nil {
		root, err := sf.Lstat(current)
		if err != nil {
			return errors.New("remote root directory unavailable")
		}
		if err := executor.CheckSFTPOwner(root, *selfUID, false); err != nil {
			return err
		}
	}
	for _, component := range strings.Split(path.Clean(directory), "/") {
		if component == "" {
			continue
		}
		current = path.Join(current, component)
		info, err := sf.Lstat(current)
		created := false
		if os.IsNotExist(err) {
			if err = sf.Mkdir(current); err != nil {
				return errors.New("remote backup directory creation failed")
			}
			if err = sf.Chmod(current, 0700); err != nil {
				return errors.New("remote backup directory permissions unavailable")
			}
			info, err = sf.Lstat(current)
			created = true
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 {
			return errors.New("remote backup path must contain real directories not writable by other accounts")
		}
		if created && info.Mode().Perm() != 0700 {
			return errors.New("remote backup directory permissions not private")
		}
		if selfUID != nil {
			if err := executor.CheckSFTPOwner(info, *selfUID, current == directory); err != nil {
				return err
			}
		}
	}
	return nil
}
func (b *BackupService) list(w http.ResponseWriter, r *http.Request) {
	if !b.admin(w, r) {
		return
	}
	out := []BackupRecord{}
	_ = b.app.Store.View(func(s *State) error { out = ListDocs[BackupRecord](s, "backups"); return nil })
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	WriteJSON(w, 200, map[string]any{"backups": out, "localDirectory": filepath.Join(b.app.Config.DataDir, "backups"), "deleteScope": "仅本机副本；远程文件不删除，历史记录保留"})
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
	if !validBackupID(record.ID) || record.Targets["local"] == "removed" {
		Fail(w, 404, "本机备份文件不存在")
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
	var report RestorePreflight
	err = b.app.Store.View(func(current *State) error {
		_, result, e := mergeSafeRestore(current, s)
		report = result
		if e == nil {
			if guardErr := validateRestoreIdle(current); guardErr != nil {
				report.Blockers = append(report.Blockers, guardErr.Error())
			}
		}
		return e
	})
	if err != nil {
		Fail(w, 409, err.Error())
		return
	}
	hash := sha256.Sum256(raw)
	WriteJSON(w, 200, map[string]any{"valid": true, "mode": "safe", "sha256": hex.EncodeToString(hash[:]), "users": len(s.Users), "collections": len(s.Docs), "report": report, "message": "安全恢复只替换站点设置、文章附件、线路和节点定义；保留当前用户、身份关系、财务、流量及用户转发关联。恢复后保持维护、暂停线路并撤销会话和Agent凭据。"})
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
		if err := validateRestoreIdle(s); err != nil {
			return err
		}
		merged, report, err := mergeSafeRestore(s, restored)
		if err != nil {
			return err
		}
		if r.FormValue("currentSha256") == "" || r.FormValue("currentSha256") != report.CurrentSHA256 {
			return errors.New("当前设置、线路或用户规则已变化，请重新预检后再恢复")
		}
		current, e := b.pack(s)
		if e != nil {
			return e
		}
		rollback, e = b.write(current)
		if e != nil {
			return e
		}
		rollback.Protected = true
		if err := isolateRestoredState(merged, false); err != nil {
			return err
		}
		*s = *merged
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
	WriteJSON(w, 200, map[string]any{"ok": true, "mode": "safe", "rollbackId": rollback.ID, "message": "安全恢复完成；当前用户、财务、流量与转发关联已保留。请重新登录并重新关联Agent，核对线路后解除维护。恢复前的私有回滚副本已保护，不会自动清理。"})
}

func prepareRestoredRelay(s *State, now int64) error {
	for _, rule := range ListDocs[UserRule](s, "user_rules") {
		key := rule.UserID + ":" + rule.RouteID
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
	if !b.mu.TryLock() {
		Fail(w, 409, "备份或恢复正在进行，请稍后再清理")
		return
	}
	defer b.mu.Unlock()
	candidates := []string{}
	err := b.app.Store.View(func(s *State) error {
		p, ok := LoadDoc[BackupPlan](s, "backup_plans", "default")
		if !ok || p.MinCopies < 1 {
			return errors.New("请先配置保留策略")
		}
		all := ListDocs[BackupRecord](s, "backups")
		sort.Slice(all, func(i, j int) bool { return all[i].CreatedAt > all[j].CreatedAt })
		protected := 0
		for _, v := range all {
			if !b.verifiedLocalBackup(v) {
				continue
			}
			protected++
			if v.Protected || protected <= p.MinCopies || v.CreatedAt > time.Now().AddDate(0, 0, -p.RetentionDays).UnixMilli() {
				continue
			}
			candidates = append(candidates, v.ID)
		}
		return nil
	})
	if err == nil && in.Confirm {
		for _, id := range candidates {
			if err = b.removeLocalBackup(id); err != nil {
				break
			}
		}
	}
	if err != nil {
		Fail(w, 409, err.Error())
		return
	}
	WriteJSON(w, 200, map[string]any{"ids": candidates, "deleted": in.Confirm, "scope": "local"})
}
func (b *BackupService) verifiedLocalBackup(record BackupRecord) bool {
	if !validBackupID(record.ID) || record.Targets["local"] == "removed" {
		return false
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
	snapshot, err := b.unpack(raw)
	if err != nil {
		return false
	}
	if err = validateRestoreReferences(snapshot); err != nil {
		return false
	}
	return validateOfflineFinancialReferences(snapshot) == nil
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
