package control

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"net/netip"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mozziexwz/node/internal/executor"
)

type Task struct {
	ID              string                  `json:"id"`
	UserID          string                  `json:"userId"`
	Kind            string                  `json:"kind"`
	Host            string                  `json:"host"`
	Mode            string                  `json:"mode,omitempty"`
	State           string                  `json:"state"`
	Phase           string                  `json:"phase"`
	Message         string                  `json:"message"`
	CreatedAt       int64                   `json:"createdAt"`
	UpdatedAt       int64                   `json:"updatedAt"`
	Idempotency     string                  `json:"-"`
	RequestDigest   string                  `json:"-"`
	Health          *executor.Health        `json:"health,omitempty"`
	Hops            []executor.Hop          `json:"hops,omitempty"`
	ConfigAvailable bool                    `json:"configAvailable"`
	ConfigHost      string                  `json:"configHost,omitempty"`
	ConfigPort      int                     `json:"configPort,omitempty"`
	ErrorCode       string                  `json:"errorCode,omitempty"`
	NextStep        string                  `json:"nextStep,omitempty"`
	Remark          string                  `json:"remark,omitempty"`
	Cleanup         *executor.CleanupReport `json:"cleanup,omitempty"`
	SSHFingerprint  string                  `json:"sshFingerprint,omitempty"`
	SSHPort         int                     `json:"sshPort,omitempty"`
}

// Internal idempotency metadata stays separate because Task is also a public response.
type taskIdempotency struct {
	TaskID string `json:"taskId"`
	Digest string `json:"digest"`
}
type ExecutorRecord struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Status       string   `json:"status"`
	TokenHash    string   `json:"tokenHash,omitempty"`
	CreatedAt    int64    `json:"createdAt"`
	LastSeenAt   int64    `json:"lastSeenAt"`
	IP           string   `json:"ip"`
	Online       bool     `json:"online"`
	Capabilities []string `json:"capabilities,omitempty"`
}
type taskEnvelope struct {
	Job       executor.Job
	UserID    string
	AgentID   string
	QueuedAt  time.Time
	ClaimedAt time.Time
	Result    chan executor.Result
}
type taskConfig struct {
	Data    []byte
	UserID  string
	Expires time.Time
}
type taskProbe struct {
	Fingerprint string
	Expires     time.Time
}
type taskDigestSecret struct {
	Sealed string `json:"sealed"`
}
type sshTrust struct {
	Fingerprint string `json:"fingerprint"`
	UpdatedAt   int64  `json:"updatedAt"`
}

func sshTrustID(user string, s executor.SSH) string {
	sum := sha256.Sum256([]byte(taskProbeKey(user, s)))
	return hex.EncodeToString(sum[:])
}
func trustSSH(s *State, user string, connection executor.SSH) error {
	id := sshTrustID(user, connection)
	previous, exists := LoadDoc[sshTrust](s, "ssh_trust", id)
	if exists && previous.Fingerprint != connection.Fingerprint && connection.ReplaceFingerprint != previous.Fingerprint {
		return errors.New("SSH 主机指纹已变化，请通过 VPS 控制台核实，并在高级 SSH 设置中明确确认替换")
	}
	// Empty mode preserves compatibility with already-confirmed strict clients.
	if !exists && connection.ReplaceFingerprint != "" {
		return errors.New("主机信任记录已变化，请重新检查指纹")
	}
	return SaveDoc(s, "ssh_trust", id, sshTrust{Fingerprint: connection.Fingerprint, UpdatedAt: time.Now().UnixMilli()})
}

type TaskService struct {
	app       *App
	mu        sync.Mutex
	envelopes map[string]*taskEnvelope
	configs   map[string]taskConfig
	probes    map[string]taskProbe
	digestKey []byte
	initErr   error
	// An executor obtains a job once. Lost responses cannot replay a destructive action.
}

func NewTaskService(app *App) *TaskService {
	t := &TaskService{app: app, envelopes: map[string]*taskEnvelope{}, configs: map[string]taskConfig{}, probes: map[string]taskProbe{}}
	t.initErr = app.Store.Update(func(s *State) error {
		secret, ok := LoadDoc[taskDigestSecret](s, "task_internal", "digest_key")
		if !ok {
			var err error
			secret.Sealed, err = app.Seal([]byte(ID()))
			if err != nil {
				return err
			}
			if err = SaveDoc(s, "task_internal", "digest_key", secret); err != nil {
				return err
			}
		}
		var err error
		t.digestKey, err = app.Open(secret.Sealed)
		return err
	})
	return t
}
func taskProbeKey(user string, s executor.SSH) string {
	host := s.Host
	if ip, err := netip.ParseAddr(host); err == nil {
		host = ip.String()
	}
	return user + "|" + host + "|" + strconv.Itoa(s.Port)
}
func (t *TaskService) confirmedProbe(user string, s executor.SSH) bool {
	p, ok := t.probes[taskProbeKey(user, s)]
	return ok && time.Now().Before(p.Expires) && subtle.ConstantTimeCompare([]byte(p.Fingerprint), []byte(s.Fingerprint)) == 1
}
func (t *TaskService) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/tasks", t.list)
	mux.HandleFunc("POST /api/tasks", t.create)
	mux.HandleFunc("GET /api/tasks/{id}", t.get)
	mux.HandleFunc("GET /api/tasks/{id}/config", t.download)
	mux.HandleFunc("POST /api/fingerprints", t.fingerprint)
	mux.HandleFunc("GET /api/admin/tasks", t.adminTasks)
	mux.HandleFunc("GET /api/admin/executors", t.listExecutors)
	mux.HandleFunc("POST /api/admin/executors", t.addExecutor)
	mux.HandleFunc("PATCH /api/admin/executors/{id}", t.editExecutor)
	mux.HandleFunc("POST /api/admin/executors/{id}/enrollment", t.renewExecutor)
	mux.HandleFunc("DELETE /api/admin/executors/{id}", t.deleteExecutor)
	mux.HandleFunc("GET /api/executor/next", t.next)
	mux.HandleFunc("POST /api/executor/heartbeat", t.heartbeat)
	mux.HandleFunc("POST /api/executor/result", t.result)
}
func (t *TaskService) Start(ctx context.Context) {
	// A restart deliberately loses SSH/password/config envelopes, never replays them.
	_ = t.app.Store.Update(func(s *State) error {
		for _, job := range ListDocs[Task](s, "tasks") {
			if job.State == "queued" || job.State == "running" {
				job.State = "interrupted"
				if job.Kind == "dd" {
					job.State = "unknown"
				}
				job.Phase = "server_restart"
				job.Message = "执行状态因服务重启中断；一次性凭据已丢弃，请核实 VPS 状态后决定下一步"
				job.UpdatedAt = time.Now().UnixMilli()
				_ = SaveDoc(s, "tasks", job.ID, job)
			}
		}
		return nil
	})
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				t.mu.Lock()
				clear(t.envelopes)
				clear(t.configs)
				t.mu.Unlock()
				return
			case <-ticker.C:
				t.expire()
			}
		}
	}()
}
func (t *TaskService) expire() {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	for id, c := range t.configs {
		if now.After(c.Expires) {
			delete(t.configs, id)
		}
	}
	for id, p := range t.probes {
		if now.After(p.Expires) {
			delete(t.probes, id)
		}
	}
	for id, e := range t.envelopes {
		limit := 5 * time.Minute
		if !e.ClaimedAt.IsZero() {
			limit = 45 * time.Minute
		}
		if now.Sub(e.QueuedAt) > limit {
			delete(t.envelopes, id)
			_ = t.app.Store.Update(func(s *State) error {
				v, ok := LoadDoc[Task](s, "tasks", id)
				if ok && (v.State == "running" || v.State == "queued") {
					v.State = "interrupted"
					if v.Kind == "dd" {
						v.State = "unknown"
					}
					v.Phase = "executor_timeout"
					d := executor.PublicDiagnostic("executor_offline", "executor")
					v.ErrorCode, v.Message, v.NextStep = d.Code, d.Message, d.NextStep
					v.UpdatedAt = now.UnixMilli()
					return SaveDoc(s, "tasks", id, v)
				}
				return nil
			})
		}
	}
}

type TaskLimit struct {
	Minutes   int   `json:"minutes"`
	Count     int   `json:"count"`
	Remaining int   `json:"remaining"`
	NextAt    int64 `json:"nextAt"`
}

func toolLimit(s *State, kind, user string, now int64) TaskLimit {
	v := TaskLimit{Minutes: 15, Count: 5}
	if kind == "fingerprint" {
		v.Minutes = 30
		v.Count = 10
	}
	if limits, ok := s.Settings["limits"].(map[string]any); ok {
		if q, ok := limits[kind].(map[string]any); ok {
			if n, ok := q["minutes"].(float64); ok && n >= 1 && n <= 1440 {
				v.Minutes = int(n)
			}
			if n, ok := q["count"].(float64); ok && n >= 1 && n <= 1000 {
				v.Count = int(n)
			}
		}
	}
	var times []int64
	for _, job := range ListDocs[Task](s, "tasks") {
		if job.UserID == user && job.Kind == kind && job.CreatedAt > now-int64(v.Minutes)*60000 {
			times = append(times, job.CreatedAt)
		}
	}
	v.Remaining = v.Count - len(times)
	if v.Remaining < 0 {
		v.Remaining = 0
	}
	if v.Remaining == 0 {
		sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
		v.NextAt = times[len(times)-v.Count] + int64(v.Minutes)*60000
	}
	return v
}
func taskGate(s *State, u *User, kind string) error {
	if u == nil || u.Status != "active" {
		return errors.New("账号当前不可使用免费工具")
	}
	if maintenance, _ := s.Settings["maintenance"].(bool); maintenance {
		return errors.New("系统维护中，暂停创建新任务；已有任务记录仍可查看")
	}
	if u.Role != "admin" {
		if required, _ := s.Settings["freeToolsRequireVerifiedEmail"].(bool); required && u.EmailVerifiedAt == 0 {
			return errors.New("请先验证注册邮箱，再使用部署 MSBOOST、自备中转和 DD 系统")
		}
	}
	if enabled, ok := s.Settings[kind].(bool); ok && !enabled {
		return errors.New("该工具已被管理员关闭")
	}
	return nil
}
func (t *TaskService) hasExecutor(s *State) bool {
	for _, a := range ListDocs[ExecutorRecord](s, "executors") {
		if a.Status == "active" && time.Now().UnixMilli()-a.LastSeenAt < 90000 {
			return true
		}
	}
	return false
}

func executorHasFreeRelayGuard(record ExecutorRecord) bool {
	for _, capability := range record.Capabilities {
		if capability == executor.FreeRelayGuardCapability {
			return true
		}
	}
	return false
}

func executorCapabilitiesFromRequest(r *http.Request) []string {
	if r.Header.Get("X-MSBOOST-Executor-Capabilities") == executor.FreeRelayGuardCapability {
		return []string{executor.FreeRelayGuardCapability}
	}
	return nil
}

func (t *TaskService) hasExecutorFor(s *State, kind string) bool {
	for _, record := range ListDocs[ExecutorRecord](s, "executors") {
		if record.Status == "active" && record.LastSeenAt > time.Now().Add(-90*time.Second).UnixMilli() && (kind != "relay" && kind != "front" || executorHasFreeRelayGuard(record)) {
			return true
		}
	}
	return false
}
func taskPublicMessage(state string) string {
	switch state {
	case "succeeded":
		return "任务已完成，请查看服务、本地测试与公网 TCP 的独立状态"
	case "executed":
		return "已执行 DD 操作，请等待15分钟以上，再执行部署 MSBOOST。此状态仅表示操作已提交，不表示系统安装完成。"
	case "unknown":
		return "提交结果不明确；请从 VPS 控制台核实，禁止自动重试"
	default:
		return "任务未成功完成，请查看失败阶段和处理建议；系统不会自动重试"
	}
}
func (t *TaskService) create(w http.ResponseWriter, r *http.Request) {
	if t.initErr != nil {
		Fail(w, 503, "任务服务密钥不可用")
		return
	}
	u, err := t.app.User(r)
	if err != nil {
		Fail(w, 401, "请先登录")
		return
	}
	// Gate before decoding SSH secrets and before quota consumption.
	err = t.app.Store.View(func(s *State) error { return taskGate(s, u, "") })
	if err != nil {
		Fail(w, 403, err.Error())
		return
	}
	var request executor.Request
	if err = Decode(r, &request); err != nil {
		Fail(w, 400, "任务参数格式无效")
		return
	}
	if err = executor.ValidateRequest(request); err != nil {
		Fail(w, 400, err.Error())
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) < 8 || len(key) > 128 {
		Fail(w, 400, "请提供 8–128 字符 Idempotency-Key")
		return
	}
	var asset executor.Asset
	path := ""
	if request.Kind == "deploy" {
		path = t.app.Config.NodeScript
	}
	if request.Kind == "dd" {
		path = t.app.Config.ReinstallScript
	}
	if path != "" {
		asset.Data, err = os.ReadFile(path)
		if err != nil {
			Fail(w, 503, "固定版本安装脚本尚未就绪")
			return
		}
		sum := sha256.Sum256(asset.Data)
		asset.SHA256 = hex.EncodeToString(sum[:])
		if request.Kind == "dd" && (asset.SHA256 != t.app.Config.ReinstallSHA256 || len(t.app.Config.ReinstallSHA256) != 64) {
			Fail(w, 503, "DD 安装脚本完整性校验失败")
			return
		}
	}
	if (request.Kind == "deploy" || request.Kind == "dd") && len(asset.Data) == 0 {
		Fail(w, 503, "固定版本安装脚本尚未配置")
		return
	}
	now := time.Now()
	job := Task{ID: ID(), UserID: u.ID, Kind: request.Kind, Host: request.SSH.Host, Mode: request.Mode, State: "queued", Phase: "queued", Message: "等待执行机领取一次性任务", CreatedAt: now.UnixMilli(), UpdatedAt: now.UnixMilli(), Remark: request.Remark, SSHFingerprint: request.SSH.Fingerprint, SSHPort: request.SSH.Port}
	raw, _ := json.Marshal(request)
	mac := hmac.New(sha256.New, t.digestKey)
	_, _ = mac.Write(raw)
	digest := hex.EncodeToString(mac.Sum(nil))
	keySum := sha256.Sum256([]byte(u.ID + ":" + key))
	idemID := hex.EncodeToString(keySum[:])
	duplicate := false
	t.mu.Lock()
	err = t.app.Store.Update(func(s *State) error {
		fresh, ok := s.Users[u.ID]
		if !ok {
			return errors.New("账号不存在")
		}
		if err := taskGate(s, fresh, request.Kind); err != nil {
			return err
		}
		if prev, ok := LoadDoc[taskIdempotency](s, "task_idempotency", idemID); ok {
			if prev.Digest != digest {
				return errors.New("同一幂等键不能提交不同任务参数")
			}
			job, _ = LoadDoc[Task](s, "tasks", prev.TaskID)
			duplicate = true
			return nil
		}
		if !t.confirmedProbe(u.ID, request.SSH) || (request.Front != nil && !t.confirmedProbe(u.ID, *request.Front)) {
			return errors.New("请先检查并确认每台 SSH 服务器的真实指纹，指纹检查有效期为 30 分钟")
		}
		if request.Cleanup != nil {
			tool := "deploy"
			if request.Cleanup.Scope == "relay" {
				tool = "relay"
			}
			if err := taskGate(s, fresh, tool); err != nil {
				return err
			}
			if request.Kind == "cleanup" {
				preview, ok := LoadDoc[Task](s, "tasks", request.Cleanup.PreviewID)
				if !ok || preview.UserID != u.ID || preview.Kind != "cleanup-preview" || preview.State != "succeeded" || preview.Host != job.Host || preview.SSHPort != job.SSHPort || preview.SSHFingerprint != job.SSHFingerprint || preview.UpdatedAt < now.Add(-10*time.Minute).UnixMilli() || preview.Cleanup == nil || preview.Cleanup.Scope != request.Cleanup.Scope || preview.Cleanup.Digest != request.Cleanup.Digest {
					return errors.New("清理预览已失效或与当前服务器不一致，请重新预览")
				}
				if _, used := LoadDoc[taskIdempotency](s, "cleanup_consumed", preview.ID); used {
					return errors.New("此清理预览已用于提交，不能再次执行；请重新检查 VPS 并预览")
				}
				if err := SaveDoc(s, "cleanup_consumed", preview.ID, taskIdempotency{TaskID: job.ID}); err != nil {
					return err
				}
			}
		}
		if t.targetBusy(request.SSH.Host) || (request.Front != nil && t.targetBusy(request.Front.Host)) {
			return errors.New("该服务器已有未结束任务，请先核实其结果")
		}
		if !t.hasExecutorFor(s, request.Kind) {
			if request.Kind == "relay" {
				return errors.New("没有已确认默认 SOCKS 屏蔽的在线执行机；请联系管理员升级 Executor Agent")
			}
			return errors.New("当前没有在线的 executor 执行机，请联系管理员")
		}
		if !(fresh.Role == "admin" && request.Kind == "deploy") && toolLimit(s, request.Kind, u.ID, now.UnixMilli()).Remaining == 0 {
			return errors.New("本工具已达到次数限制，请等待下一次可用时间")
		}
		if err := trustSSH(s, u.ID, request.SSH); err != nil {
			return err
		}
		if request.Front != nil {
			if err := trustSSH(s, u.ID, *request.Front); err != nil {
				return err
			}
		}
		if err := SaveDoc(s, "tasks", job.ID, job); err != nil {
			return err
		}
		return SaveDoc(s, "task_idempotency", idemID, taskIdempotency{TaskID: job.ID, Digest: digest})
	})
	if err == nil && !duplicate {
		t.envelopes[job.ID] = &taskEnvelope{Job: executor.Job{ID: job.ID, Lease: ID(), Request: request, Script: asset, Deadline: now.Add(45 * time.Minute).UnixMilli()}, UserID: u.ID, QueuedAt: now}
	}
	t.mu.Unlock()
	if err != nil {
		Fail(w, 409, err.Error())
		return
	}
	if duplicate {
		WriteJSON(w, 200, job)
	} else {
		WriteJSON(w, 202, job)
	}
}
func (t *TaskService) targetBusy(host string) bool {
	parsed, _ := netip.ParseAddr(host)
	for _, e := range t.envelopes {
		if e.Job.Request.Kind == "fingerprint" {
			continue
		}
		other, _ := netip.ParseAddr(e.Job.Request.SSH.Host)
		frontHost := netip.Addr{}
		if e.Job.Request.Front != nil {
			frontHost, _ = netip.ParseAddr(e.Job.Request.Front.Host)
		}
		if parsed.IsValid() && (other == parsed || frontHost == parsed) {
			return true
		}
	}
	return false
}
func (t *TaskService) list(w http.ResponseWriter, r *http.Request) {
	u, err := t.app.User(r)
	if err != nil {
		Fail(w, 401, "请先登录")
		return
	}
	rows := []Task{}
	limits := map[string]TaskLimit{}
	_ = t.app.Store.View(func(s *State) error {
		for _, v := range ListDocs[Task](s, "tasks") {
			if v.UserID == u.ID && v.Kind != "fingerprint" {
				rows = append(rows, v)
			}
		}
		for _, k := range []string{"deploy", "relay", "dd", "fingerprint", "cleanup", "cleanup-preview"} {
			limits[k] = toolLimit(s, k, u.ID, time.Now().UnixMilli())
		}
		return nil
	})
	sort.Slice(rows, func(i, j int) bool { return rows[i].CreatedAt > rows[j].CreatedAt })
	t.mu.Lock()
	for i := range rows {
		c, ok := t.configs[rows[i].ID]
		rows[i].ConfigAvailable = ok && time.Now().Before(c.Expires)
	}
	t.mu.Unlock()
	WriteJSON(w, 200, map[string]any{"tasks": rows, "limits": limits})
}
func (t *TaskService) get(w http.ResponseWriter, r *http.Request) {
	u, err := t.app.User(r)
	if err != nil {
		Fail(w, 401, "请先登录")
		return
	}
	var job Task
	if err := t.app.Store.View(func(s *State) error { job, _ = LoadDoc[Task](s, "tasks", r.PathValue("id")); return nil }); err != nil {
		Fail(w, 500, "任务记录读取失败")
		return
	}
	if job.ID == "" || (job.UserID != u.ID && u.Role != "admin") {
		Fail(w, 404, "任务不存在")
		return
	}
	t.mu.Lock()
	c, ok := t.configs[job.ID]
	// Administrators may audit metadata, never another user's temporary config.
	job.ConfigAvailable = job.UserID == u.ID && ok && c.UserID == u.ID && time.Now().Before(c.Expires)
	t.mu.Unlock()
	WriteJSON(w, 200, job)
}
func (t *TaskService) download(w http.ResponseWriter, r *http.Request) {
	u, err := t.app.User(r)
	if err != nil {
		Fail(w, 401, "请先登录")
		return
	}
	t.mu.Lock()
	c, ok := t.configs[r.PathValue("id")]
	t.mu.Unlock()
	if !ok || c.UserID != u.ID || time.Now().After(c.Expires) {
		Fail(w, 404, "服务器临时配置已过期，请从当前浏览器的本机配置库下载")
		return
	}
	w.Header().Set("Cache-Control", "no-store, private")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	name := "msboost"
	_ = t.app.Store.View(func(s *State) error {
		job, ok := LoadDoc[Task](s, "tasks", r.PathValue("id"))
		if ok && job.UserID == u.ID && job.Remark != "" {
			name = job.Remark
		}
		return nil
	})
	name = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 || strings.ContainsRune(`<>:"/\|?*`, r) {
			return '_'
		}
		return r
	}, name)
	name = strings.Trim(name, ". ")
	if name == "" {
		name = "msboost"
	}
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name + ".json"}))
	_, _ = w.Write(c.Data)
}
func (t *TaskService) adminTasks(w http.ResponseWriter, r *http.Request) {
	u, err := t.app.Admin(r)
	if err != nil {
		Fail(w, 403, "需要管理员权限")
		return
	}
	rows := []Task{}
	if err := t.app.Store.View(func(s *State) error { rows = ListDocs[Task](s, "tasks"); return nil }); err != nil {
		Fail(w, 500, "任务记录读取失败")
		return
	}
	t.mu.Lock()
	for i := range rows {
		c, ok := t.configs[rows[i].ID]
		rows[i].ConfigAvailable = rows[i].UserID == u.ID && ok && c.UserID == u.ID && time.Now().Before(c.Expires)
	}
	t.mu.Unlock()
	sort.Slice(rows, func(i, j int) bool { return rows[i].CreatedAt > rows[j].CreatedAt })
	WriteJSON(w, 200, map[string]any{"tasks": rows})
}

func (t *TaskService) fingerprint(w http.ResponseWriter, r *http.Request) {
	u, err := t.app.User(r)
	if err != nil {
		Fail(w, 401, "请先登录")
		return
	}
	// Public-key discovery is read-only and accepts no SSH credentials. Keep
	// diagnostics available during maintenance and independent of free-tool
	// email policy; execution endpoints still enforce their own strict gates.
	if u.Status != "active" {
		Fail(w, 403, "账号当前不可检查 SSH 主机指纹")
		return
	}
	var input struct {
		Host string `json:"host"`
		Port int    `json:"port"`
	}
	if err = Decode(r, &input); err != nil {
		Fail(w, 400, "请求格式无效")
		return
	}
	if executor.PublicIP(input.Host) != nil || input.Port < 1 || input.Port > 65535 {
		Fail(w, 400, "请填写公网 IP 和有效 SSH 端口")
		return
	}
	now := time.Now()
	job := Task{ID: ID(), UserID: u.ID, Host: input.Host, Kind: "fingerprint", State: "queued", CreatedAt: now.UnixMilli(), UpdatedAt: now.UnixMilli()}
	result := make(chan executor.Result, 1)
	t.mu.Lock()
	err = t.app.Store.Update(func(s *State) error {
		if !t.hasExecutor(s) {
			return errors.New("没有在线执行机，无法检查真实 SSH 指纹")
		}
		if u.Role != "admin" && toolLimit(s, "fingerprint", u.ID, now.UnixMilli()).Remaining == 0 {
			return errors.New("SSH 指纹检查达到次数限制")
		}
		return SaveDoc(s, "tasks", job.ID, job)
	})
	if err == nil {
		t.envelopes[job.ID] = &taskEnvelope{Job: executor.Job{ID: job.ID, Lease: ID(), Request: executor.Request{Kind: "fingerprint", SSH: executor.SSH{Host: input.Host, Port: input.Port}}, Deadline: now.Add(25 * time.Second).UnixMilli()}, UserID: u.ID, QueuedAt: now, Result: result}
	}
	t.mu.Unlock()
	if err != nil {
		Fail(w, 409, err.Error())
		return
	}
	select {
	case out := <-result:
		if out.State != "succeeded" {
			d := executor.PublicDiagnostic(out.ErrorCode, out.Phase)
			Fail(w, 502, d.Message+"；"+d.NextStep)
			return
		}
		remembered := ""
		_ = t.app.Store.View(func(s *State) error {
			prior, _ := LoadDoc[sshTrust](s, "ssh_trust", sshTrustID(u.ID, executor.SSH{Host: input.Host, Port: input.Port}))
			remembered = prior.Fingerprint
			return nil
		})
		WriteJSON(w, 200, map[string]any{"host": input.Host, "port": input.Port, "fingerprint": out.Fingerprint, "algorithm": out.Algorithm, "checkedAt": time.Now().UnixMilli(), "rememberedFingerprint": remembered, "authenticationChecked": false})
	case <-time.After(28 * time.Second):
		Fail(w, 504, "SSH 指纹检查超时，请稍后重试")
	case <-r.Context().Done():
	}
}

func (t *TaskService) executorAuth(r *http.Request) (ExecutorRecord, error) {
	var found ExecutorRecord
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == "" || token == r.Header.Get("Authorization") {
		return found, errors.New("unauthorized")
	}
	sum := sha256.Sum256([]byte(token))
	hash := hex.EncodeToString(sum[:])
	_ = t.app.Store.View(func(s *State) error {
		for _, a := range ListDocs[ExecutorRecord](s, "executors") {
			if a.Status == "active" && subtle.ConstantTimeCompare([]byte(hash), []byte(a.TokenHash)) == 1 {
				found = a
				break
			}
		}
		return nil
	})
	if found.ID == "" {
		return found, errors.New("unauthorized")
	}
	return found, nil
}
func (t *TaskService) heartbeat(w http.ResponseWriter, r *http.Request) {
	a, err := t.executorAuth(r)
	if err != nil {
		Fail(w, 401, "执行机令牌无效")
		return
	}
	err = t.app.Store.Update(func(s *State) error {
		current, ok := LoadDoc[ExecutorRecord](s, "executors", a.ID)
		if !ok || current.Status != "active" {
			return errors.New("令牌已撤销")
		}
		current.LastSeenAt = time.Now().UnixMilli()
		current.IP = t.app.clientIP(r)
		current.Capabilities = executorCapabilitiesFromRequest(r)
		return SaveDoc(s, "executors", a.ID, current)
	})
	if err != nil {
		Fail(w, 409, "执行机不可用")
		return
	}
	WriteJSON(w, 200, map[string]bool{"ok": true})
}
func (t *TaskService) next(w http.ResponseWriter, r *http.Request) {
	a, err := t.executorAuth(r)
	if err != nil {
		Fail(w, 401, "执行机令牌无效")
		return
	}
	_ = t.app.Store.Update(func(s *State) error {
		current, ok := LoadDoc[ExecutorRecord](s, "executors", a.ID)
		if ok {
			current.LastSeenAt = time.Now().UnixMilli()
			current.IP = t.app.clientIP(r)
			current.Capabilities = executorCapabilitiesFromRequest(r)
			return SaveDoc(s, "executors", a.ID, current)
		}
		return nil
	})
	timer := time.NewTimer(25 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	for {
		t.mu.Lock()
		var next *taskEnvelope
		for _, e := range t.envelopes {
			if e.AgentID == "" && time.Now().UnixMilli() < e.Job.Deadline && ((e.Job.Request.Kind != "relay" && e.Job.Request.Kind != "front") || r.Header.Get("X-MSBOOST-Executor-Capabilities") == executor.FreeRelayGuardCapability) && (next == nil || e.QueuedAt.Before(next.QueuedAt)) {
				next = e
			}
		}
		if next != nil {
			job := next.Job
			err := t.app.Store.Update(func(s *State) error {
				current, enabled := LoadDoc[ExecutorRecord](s, "executors", a.ID)
				if !enabled || current.Status != "active" || current.TokenHash != a.TokenHash {
					return errors.New("执行机令牌已撤销")
				}
				v, ok := LoadDoc[Task](s, "tasks", job.ID)
				if !ok || v.State != "queued" {
					return errors.New("任务不再等待领取")
				}
				v.State = "running"
				v.Phase = "executing"
				v.UpdatedAt = time.Now().UnixMilli()
				return SaveDoc(s, "tasks", v.ID, v)
			})
			if err != nil {
				t.mu.Unlock()
				Fail(w, 409, "任务授权状态已变化，未交付执行")
				return
			}
			next.AgentID = a.ID
			next.ClaimedAt = time.Now()
			next.Job.Request.SSH.Password = ""
			if next.Job.Request.Front != nil {
				front := *next.Job.Request.Front
				front.Password = ""
				next.Job.Request.Front = &front
			}
			next.Job.Request.ClientConfig = nil
			next.Job.Request.DD = nil
			next.Job.Script.Data = nil
			t.mu.Unlock()
			w.Header().Set("Cache-Control", "no-store")
			WriteJSON(w, 200, job)
			return
		}
		t.mu.Unlock()
		select {
		case <-r.Context().Done():
			return
		case <-timer.C:
			w.WriteHeader(204)
			return
		case <-ticker.C:
		}
	}
}
func (t *TaskService) result(w http.ResponseWriter, r *http.Request) {
	a, err := t.executorAuth(r)
	if err != nil {
		Fail(w, 401, "执行机令牌无效")
		return
	}
	var out executor.Result
	if Decode(r, &out) != nil {
		Fail(w, 400, "结果格式无效")
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	envelope, ok := t.envelopes[out.ID]
	if !ok || envelope.AgentID != a.ID || subtle.ConstantTimeCompare([]byte(envelope.Job.Lease), []byte(out.Lease)) != 1 {
		Fail(w, 409, "任务租约已失效，结果不可重放")
		return
	}
	if out.State != "succeeded" && out.State != "failed" && out.State != "executed" && out.State != "unknown" {
		Fail(w, 400, "结果状态无效")
		return
	}
	if envelope.Job.Request.Kind == "dd" && out.State == "succeeded" {
		Fail(w, 400, "DD 只能报告已提交、失败或提交不明确，不能声称系统安装完成")
		return
	}
	if (envelope.Job.Request.Kind == "deploy" || envelope.Job.Request.Kind == "relay") && out.State == "succeeded" && len(out.Config) == 0 {
		Fail(w, 400, "成功结果缺少客户端配置")
		return
	}
	if envelope.Job.Request.Kind == "fingerprint" && out.State == "succeeded" {
		if len(out.Fingerprint) != 50 || !strings.HasPrefix(out.Fingerprint, "SHA256:") || len(out.Algorithm) > 80 {
			Fail(w, 400, "主机指纹格式无效")
			return
		}
		t.probes[taskProbeKey(envelope.UserID, envelope.Job.Request.SSH)] = taskProbe{Fingerprint: out.Fingerprint, Expires: time.Now().Add(30 * time.Minute)}
	}
	if envelope.Job.Request.Kind != "dd" && (out.State == "executed" || out.State == "unknown") {
		Fail(w, 400, "任务结果类型不匹配")
		return
	}
	if len(out.Config) > 0 {
		if out.State != "succeeded" {
			Fail(w, 400, "失败任务不能交付配置")
			return
		}
		if envelope.Job.Request.Kind != "deploy" && envelope.Job.Request.Kind != "relay" {
			Fail(w, 400, "该任务不生成配置")
			return
		}
		if _, err := executor.ParseClientConfig(out.Config); err != nil {
			Fail(w, 400, "结果配置校验失败")
			return
		}
	}
	if envelope.Job.Request.Kind == "cleanup" || envelope.Job.Request.Kind == "cleanup-preview" {
		if out.State == "succeeded" && !executor.ValidCleanupReport(out.Cleanup, envelope.Job.Request.Cleanup.Scope, envelope.Job.Request.Kind == "cleanup") {
			Fail(w, 400, "清理结果范围校验失败")
			return
		}
		if out.State == "succeeded" && envelope.Job.Request.Kind == "cleanup" && out.Cleanup.Digest != envelope.Job.Request.Cleanup.Digest {
			Fail(w, 400, "清理结果摘要与已确认预览不一致")
			return
		}
	} else if out.Cleanup != nil {
		Fail(w, 400, "该任务不接受清理结果")
		return
	}
	err = t.app.Store.Update(func(s *State) error {
		job, ok := LoadDoc[Task](s, "tasks", out.ID)
		if !ok {
			return errors.New("任务不存在")
		}
		job.State = out.State
		job.Phase = "complete"
		if executor.ValidPhase(out.Phase) {
			job.Phase = out.Phase
		}
		job.Message = taskPublicMessage(out.State)
		if out.State == "failed" {
			d := executor.PublicDiagnostic(out.ErrorCode, out.Phase)
			job.ErrorCode, job.Phase, job.Message, job.NextStep = d.Code, d.Phase, d.Message, d.NextStep
		}
		if out.State == "succeeded" && out.Cleanup != nil {
			job.Cleanup = out.Cleanup
			job.Message = "清理预览已完成，尚未删除任何服务或文件；请核对清单并再次确认。"
			if out.Cleanup.Removed {
				job.Message = "清单内受管服务、文件和本项目创建的防火墙规则已清理。备份、系统账户、依赖及共享二进制缓存仍保留。"
			}
		}
		job.UpdatedAt = time.Now().UnixMilli()
		job.Health = cleanTaskHealth(out.Health)
		if len(out.Config) > 0 {
			info, _ := executor.ParseClientConfig(out.Config)
			job.ConfigHost = info.TargetHost
			job.ConfigPort = info.TargetPort
		}
		for _, h := range out.Hops {
			if executor.PublicIP(h.FromHost) == nil && h.FromPort > 0 && h.FromPort < 65536 && len(h.ToHost) < 254 && h.ToPort > 0 && h.ToPort < 65536 {
				job.Hops = append(job.Hops, h)
			}
		}
		return SaveDoc(s, "tasks", job.ID, job)
	})
	if err != nil {
		Fail(w, 500, "结果保存失败")
		return
	}
	if len(out.Config) > 0 {
		t.configs[out.ID] = taskConfig{Data: append([]byte(nil), out.Config...), UserID: envelope.UserID, Expires: time.Now().Add(10 * time.Minute)}
	}
	if envelope.Result != nil {
		select {
		case envelope.Result <- out:
		default:
		}
	}
	delete(t.envelopes, out.ID)
	WriteJSON(w, 200, map[string]bool{"ok": true})
}
func cleanTaskHealth(in *executor.Health) *executor.Health {
	if in == nil {
		return nil
	}
	clean := func(s string) string {
		switch s {
		case "running", "passed", "reachable", "unreachable", "not_tested", "target_tcp_reachable":
			return s
		default:
			return "unknown"
		}
	}
	return &executor.Health{Service: clean(in.Service), LocalSelfTest: clean(in.LocalSelfTest), PublicTCP: clean(in.PublicTCP), Game: clean(in.Game)}
}

func (t *TaskService) listExecutors(w http.ResponseWriter, r *http.Request) {
	if _, err := t.app.Admin(r); err != nil {
		Fail(w, 403, "需要管理员权限")
		return
	}
	rows := []ExecutorRecord{}
	_ = t.app.Store.View(func(s *State) error { rows = ListDocs[ExecutorRecord](s, "executors"); return nil })
	for i := range rows {
		rows[i].TokenHash = ""
		rows[i].Online = rows[i].Status == "active" && rows[i].LastSeenAt > 0 && time.Now().UnixMilli()-rows[i].LastSeenAt < 90000
	}
	WriteJSON(w, 200, map[string]any{"executors": rows})
}
func (t *TaskService) addExecutor(w http.ResponseWriter, r *http.Request) {
	if _, err := t.app.Admin(r); err != nil {
		Fail(w, 403, "需要管理员权限")
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if Decode(r, &in) != nil || strings.TrimSpace(in.Name) == "" || len(in.Name) > 120 {
		Fail(w, 400, "执行机名称无效")
		return
	}
	token := ID() + ID()
	sum := sha256.Sum256([]byte(token))
	a := ExecutorRecord{ID: ID(), Name: in.Name, Status: "active", TokenHash: hex.EncodeToString(sum[:]), CreatedAt: time.Now().UnixMilli()}
	if err := t.app.Store.Update(func(s *State) error { return SaveDoc(s, "executors", a.ID, a) }); err != nil {
		Fail(w, 500, "保存执行机失败")
		return
	}
	a.TokenHash = ""
	w.Header().Set("Cache-Control", "no-store")
	WriteJSON(w, 201, map[string]any{"executor": a, "token": token})
}
func (t *TaskService) renewExecutor(w http.ResponseWriter, r *http.Request) {
	if _, err := t.app.Admin(r); err != nil {
		Fail(w, 403, "需要管理员权限")
		return
	}
	id := r.PathValue("id")
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, envelope := range t.envelopes {
		if envelope.AgentID == id {
			Fail(w, 409, "执行机仍有待确认任务，请等待任务结束后重置令牌")
			return
		}
	}
	token := ID() + ID()
	sum := sha256.Sum256([]byte(token))
	err := t.app.Store.Update(func(s *State) error {
		record, ok := LoadDoc[ExecutorRecord](s, "executors", id)
		if !ok {
			return errors.New("执行机不存在")
		}
		record.TokenHash = hex.EncodeToString(sum[:])
		record.LastSeenAt = 0
		return SaveDoc(s, "executors", id, record)
	})
	if err != nil {
		Fail(w, 400, err.Error())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	WriteJSON(w, 200, map[string]any{"token": token})
}
func (t *TaskService) editExecutor(w http.ResponseWriter, r *http.Request) {
	if _, err := t.app.Admin(r); err != nil {
		Fail(w, 403, "需要管理员权限")
		return
	}
	var in struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	if Decode(r, &in) != nil {
		Fail(w, 400, "参数无效")
		return
	}
	err := t.app.Store.Update(func(s *State) error {
		a, ok := LoadDoc[ExecutorRecord](s, "executors", r.PathValue("id"))
		if !ok {
			return errors.New("执行机不存在")
		}
		if in.Name != "" && len(in.Name) <= 120 {
			a.Name = in.Name
		}
		if in.Status == "active" || in.Status == "disabled" {
			a.Status = in.Status
		}
		return SaveDoc(s, "executors", a.ID, a)
	})
	if err != nil {
		Fail(w, 400, err.Error())
		return
	}
	WriteJSON(w, 200, map[string]bool{"ok": true})
}
func (t *TaskService) deleteExecutor(w http.ResponseWriter, r *http.Request) {
	if _, err := t.app.Admin(r); err != nil {
		Fail(w, 403, "需要管理员权限")
		return
	}
	_ = t.app.Store.Update(func(s *State) error { DeleteDoc(s, "executors", r.PathValue("id")); return nil })
	WriteJSON(w, 200, map[string]bool{"ok": true})
}
