package control

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"mime"
	"net/http"
	"net/netip"
	"os"
	"slices"
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
	NeedsReview     bool                    `json:"needsReview,omitempty"`
	RateMbps        int                     `json:"rateMbps,omitempty"`
	CodeTail        string                  `json:"codeTail,omitempty"`
	// Verify is the relay target check reported with this task, if any.
	Verify *executor.VerifyReport `json:"verify,omitempty"`
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
	Version      string   `json:"version,omitempty"`
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
	// Sealed SSH of a relay code whose server check is still running. It is
	// stored only if that check succeeds and is never written before then.
	codeSecrets map[string]pendingCodeSecret
	// Relay server lists and action reports, held only for their requester.
	relayResults map[string]relayResult
	digestKey    []byte
	initErr      error
	// An executor obtains a job once. Lost responses cannot replay a destructive action.
}

func NewTaskService(app *App) *TaskService {
	t := &TaskService{app: app, envelopes: map[string]*taskEnvelope{}, configs: map[string]taskConfig{}, probes: map[string]taskProbe{}, codeSecrets: map[string]pendingCodeSecret{}, relayResults: map[string]relayResult{}}
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
	mux.HandleFunc("POST /api/tasks/{id}/reconcile", t.reconcile)
	mux.HandleFunc("POST /api/fingerprints", t.fingerprint)
	mux.HandleFunc("GET /api/admin/tasks", t.adminTasks)
	mux.HandleFunc("GET /api/admin/executors", t.listExecutors)
	mux.HandleFunc("POST /api/admin/executors", t.addExecutor)
	mux.HandleFunc("PATCH /api/admin/executors/{id}", t.editExecutor)
	mux.HandleFunc("POST /api/admin/executors/{id}/enrollment", t.renewExecutor)
	mux.HandleFunc("DELETE /api/admin/executors/{id}", t.deleteExecutor)
	mux.HandleFunc("POST /api/admin/executors/{id}/revoke", t.revokeExecutor)
	mux.HandleFunc("GET /api/executor/next", t.next)
	mux.HandleFunc("POST /api/executor/heartbeat", t.heartbeat)
	mux.HandleFunc("POST /api/executor/result", t.result)
	t.registerRelayCodes(mux)
}
func (t *TaskService) Start(ctx context.Context) {
	// A restart deliberately loses SSH/password/config envelopes, never replays them.
	err := t.app.Store.Update(func(s *State) error {
		for _, job := range ListDocs[Task](s, "tasks") {
			if _, known := LoadDoc[taskOperation](s, "task_operations", job.ID); !known && (job.State == "interrupted" || job.State == "unknown" || job.State == "executed") && !readOnlyTaskKind(job.Kind) {
				job.NeedsReview = true
				if err := SaveDoc(s, "task_operations", job.ID, taskOperation{ID: job.ID, Hosts: []string{taskHost(job.Host)}, Request: executor.Request{Kind: job.Kind, SSH: executor.SSH{Host: job.Host}}, Status: "review"}); err != nil {
					return err
				}
				if err := SaveDoc(s, "tasks", job.ID, job); err != nil {
					return err
				}
			}
			if job.State == "queued" || job.State == "running" {
				wasRunning := job.State == "running"
				o, exists := LoadDoc[taskOperation](s, "task_operations", job.ID)
				if !exists {
					o = taskOperation{ID: job.ID, Hosts: []string{taskHost(job.Host)}, Request: executor.Request{Kind: job.Kind, SSH: executor.SSH{Host: job.Host}}}
				}
				o.Status = "resolved"
				if wasRunning && !readOnlyTaskKind(job.Kind) {
					o.Status = "review"
					job.NeedsReview = true
				}
				if err := SaveDoc(s, "task_operations", job.ID, o); err != nil {
					return err
				}
				job.State = "interrupted"
				if job.Kind == "dd" {
					job.State = "unknown"
				}
				job.Phase = "server_restart"
				job.Message = "面板已重启，一次性凭据已丢弃，任务不会重复执行"
				if job.NeedsReview {
					job.Message = "面板已重启，原执行机可能仍在操作 VPS，等待结果补报"
					job.NextStep = "先检查 VPS；仅在确认原操作结束后解除占用"
				}
				job.UpdatedAt = time.Now().UnixMilli()
				if err := SaveDoc(s, "tasks", job.ID, job); err != nil {
					return err
				}
			}
		}
		for _, agent := range ListDocs[ExecutorRecord](s, "executors") {
			if err := finishExecutorDrain(s, agent.ID); err != nil {
				return err
			}
		}
		// Checks and uses interrupted above can never report; settle them now.
		return sweepRelayCodes(s, time.Now())
	})
	if err != nil {
		t.initErr = err
		return
	}
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		// Stored relay-code SSH is deleted on expiry within a minute.
		codes := time.NewTicker(time.Minute)
		defer codes.Stop()
		for {
			select {
			case <-ctx.Done():
				t.mu.Lock()
				clear(t.envelopes)
				clear(t.configs)
				clear(t.codeSecrets)
				clear(t.relayResults)
				t.mu.Unlock()
				return
			case <-ticker.C:
				t.expire()
			case <-codes.C:
				t.sweepRelayCodes()
			}
		}
	}()
}

type expiredEnvelope struct {
	id        string
	envelope  *taskEnvelope
	agentID   string
	claimedAt time.Time
}

// expire decides under t.mu, persists without it, and forgets an envelope only
// after its durable state is settled. A failed write (for example during a
// whole-site backup) keeps the envelope so the next tick retries instead of
// leaving the task running until a restart.
func (t *TaskService) expire() {
	now := time.Now()
	var due []expiredEnvelope
	t.mu.Lock()
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
	for id, p := range t.codeSecrets {
		if now.After(p.Expires) {
			delete(t.codeSecrets, id)
		}
	}
	for id, result := range t.relayResults {
		if now.After(result.Expires) {
			delete(t.relayResults, id)
		}
	}
	for id, e := range t.envelopes {
		limit := 5 * time.Minute
		if !e.ClaimedAt.IsZero() {
			limit = 45 * time.Minute
		}
		started := e.QueuedAt
		if !e.ClaimedAt.IsZero() {
			started = e.ClaimedAt
		}
		if now.Sub(started) > limit {
			due = append(due, expiredEnvelope{id: id, envelope: e, agentID: e.AgentID, claimedAt: e.ClaimedAt})
		}
	}
	t.mu.Unlock()
	for _, item := range due {
		claimed := !item.claimedAt.IsZero()
		superseded := false
		err := t.app.Store.Update(func(s *State) error {
			v, ok := LoadDoc[Task](s, "tasks", item.id)
			if !ok || (v.State != "running" && v.State != "queued") {
				return nil
			}
			// Decided as unclaimed, but an executor claimed it meanwhile: the
			// longer claimed deadline now applies, so leave both records alone.
			if !claimed && v.State != "queued" {
				superseded = true
				return nil
			}
			o, exists := LoadDoc[taskOperation](s, "task_operations", item.id)
			if exists {
				o.Status = "resolved"
				if claimed && !readOnlyTaskKind(v.Kind) {
					o.Status = "review"
					v.NeedsReview = true
				}
				if err := SaveDoc(s, "task_operations", item.id, o); err != nil {
					return err
				}
			}
			v.State = "interrupted"
			if v.Kind == "dd" {
				v.State = "unknown"
			}
			v.Phase = "executor_timeout"
			d := executor.PublicDiagnostic("executor_offline", "executor")
			v.ErrorCode, v.Message, v.NextStep = d.Code, d.Message, d.NextStep
			v.UpdatedAt = now.UnixMilli()
			if err := SaveDoc(s, "tasks", item.id, v); err != nil {
				return err
			}
			if _, err := settleRelayCodeTask(s, item.id, nil, now); err != nil {
				return err
			}
			return finishExecutorDrain(s, item.agentID)
		})
		if err != nil {
			log.Printf("task %s expiry deferred to the next check: %v", item.id, err)
			continue
		}
		if superseded {
			continue
		}
		t.mu.Lock()
		if current := t.envelopes[item.id]; current == item.envelope && current.ClaimedAt.Equal(item.claimedAt) {
			delete(t.envelopes, item.id)
		}
		t.mu.Unlock()
	}
}

type TaskLimit struct {
	Minutes   int   `json:"minutes"`
	Count     int   `json:"count"`
	Remaining int   `json:"remaining"`
	NextAt    int64 `json:"nextAt"`
}

// limitKinds lists the task kinds that share one limit. Upgrading a node and
// fetching its configuration use the deploy quota. Every relay setup,
// including the server check that precedes a relay code, uses the relay
// quota; reading and managing a relay server has its own separate quota.
func limitKinds(kind string) []string {
	switch kind {
	case "deploy":
		return []string{"deploy", "upgrade", "fetch-config"}
	case "relay":
		return []string{"relay", "relay-code", "relay-check"}
	case "relay-manage":
		return []string{"relay-inventory", "relay-manage"}
	}
	return []string{kind}
}

// toolKind is the tool a task belongs to: its feature switch and its quota.
// Upgrading and fetching a node's configuration are part of the deploy page.
func toolKind(kind string) string {
	if kind == "upgrade" || kind == "fetch-config" {
		return "deploy"
	}
	return kind
}

func toolLimit(s *State, kind, user string, now int64) TaskLimit {
	v := TaskLimit{Minutes: 15, Count: 5}
	if kind == "fingerprint" {
		v.Minutes = 30
		v.Count = 10
	}
	if kind == "relay-manage" {
		settings := relayCodeSettings(s)
		v.Minutes, v.Count = settings.ManageMinutes, settings.ManageCount
	} else if limits, ok := s.Settings["limits"].(map[string]any); ok {
		if q, ok := limits[kind].(map[string]any); ok {
			if n, ok := q["minutes"].(float64); ok && n >= 1 && n <= 1440 {
				v.Minutes = int(n)
			}
			if n, ok := q["count"].(float64); ok && n >= 1 && n <= 1000 {
				v.Count = int(n)
			}
		}
	}
	kinds := limitKinds(kind)
	var times []int64
	for _, job := range ListDocs[Task](s, "tasks") {
		if job.UserID == user && job.CreatedAt > now-int64(v.Minutes)*60000 && slices.Contains(kinds, job.Kind) {
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
	out := []string{}
	if r.Header.Get("X-MSBOOST-Executor-Capabilities") == executor.FreeRelayGuardCapability {
		out = append(out, executor.FreeRelayGuardCapability)
	}
	if r.Header.Get("X-MSBOOST-Scoped-Cleanup") == "1" {
		out = append(out, executor.ScopedCleanupCapability)
	}
	if r.Header.Get(executor.SSHKeyHeader) == "1" {
		out = append(out, executor.SSHKeyCapability)
	}
	if r.Header.Get(executor.RelayCodeHeader) == "1" {
		out = append(out, executor.RelayCodeCapability)
	}
	if r.Header.Get(executor.RelayVerifyHeader) == "1" {
		out = append(out, executor.RelayVerifyCapability)
	}
	if r.Header.Get(executor.NodeUpgradeHeader) == "1" {
		out = append(out, executor.NodeUpgradeCapability)
	}
	return out
}

// requiredCapabilities is the single rule for both task creation and claim:
// an executor receives a job only when it declared everything the job needs.
func requiredCapabilities(r executor.Request) []string {
	var out []string
	switch r.Kind {
	case "relay", "front", "relay-code":
		out = append(out, executor.FreeRelayGuardCapability)
	}
	switch r.Kind {
	case "relay", "relay-code", "relay-check", "relay-inventory", "relay-manage":
		// Customer relays carry a bandwidth choice and the user record.
		out = append(out, executor.RelayCodeCapability)
	}
	if r.Cleanup != nil && r.Cleanup.ManagedTaskID != "" {
		out = append(out, executor.ScopedCleanupCapability)
	}
	if r.SSH.UsesKey() || (r.Front != nil && r.Front.UsesKey()) {
		out = append(out, executor.SSHKeyCapability)
	}
	if r.Verify != nil || r.Kind == "relay-verify" {
		out = append(out, executor.RelayVerifyCapability)
	}
	if r.Kind == "upgrade" || r.Kind == "fetch-config" {
		out = append(out, executor.NodeUpgradeCapability)
	}
	return out
}

func capabilitiesSatisfy(have, need []string) bool {
	for _, capability := range need {
		if !slices.Contains(have, capability) {
			return false
		}
	}
	return true
}

func (t *TaskService) hasExecutorFor(s *State, request executor.Request) bool {
	need := requiredCapabilities(request)
	for _, record := range ListDocs[ExecutorRecord](s, "executors") {
		if record.Status == "active" && record.LastSeenAt > time.Now().Add(-90*time.Second).UnixMilli() && capabilitiesSatisfy(record.Capabilities, need) {
			return true
		}
	}
	return false
}

// executorUnavailable explains which upgrade is missing, most specific first.
func (t *TaskService) executorUnavailable(s *State, request executor.Request) error {
	online := false
	declared := map[string]bool{}
	for _, record := range ListDocs[ExecutorRecord](s, "executors") {
		if record.Status == "active" && record.LastSeenAt > time.Now().Add(-90*time.Second).UnixMilli() {
			online = true
			for _, capability := range record.Capabilities {
				declared[capability] = true
			}
		}
	}
	need := requiredCapabilities(request)
	switch {
	case !online:
		return errors.New("当前没有在线的 executor 执行机，请联系管理员")
	case slices.Contains(need, executor.FreeRelayGuardCapability) && !declared[executor.FreeRelayGuardCapability]:
		return errors.New("没有已确认默认 SOCKS 屏蔽的在线执行机；请联系管理员升级 Executor Agent")
	case slices.Contains(need, executor.SSHKeyCapability) && !declared[executor.SSHKeyCapability]:
		return errors.New("当前在线执行机不支持密钥登录，请联系管理员升级执行机，或改用密码登录")
	case slices.Contains(need, executor.ScopedCleanupCapability) && !declared[executor.ScopedCleanupCapability]:
		return errors.New("请先升级在线执行机后使用按任务清理；旧执行机不会接收此操作")
	case slices.Contains(need, executor.RelayVerifyCapability) && !declared[executor.RelayVerifyCapability]:
		return errors.New("在线执行机需要升级后才能校验中转目标，请联系管理员升级执行机")
	case slices.Contains(need, executor.NodeUpgradeCapability) && !declared[executor.NodeUpgradeCapability]:
		return errors.New("在线执行机需要升级后才能升级节点或取回配置文件，请联系管理员升级执行机")
	default:
		return errors.New("当前在线执行机版本过旧，不支持此中转功能，请联系管理员升级执行机")
	}
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
	// Relay-code work uses SSH the user never sees; it has its own endpoints.
	switch request.Kind {
	case "deploy", "upgrade", "fetch-config", "relay", "dd", "cleanup", "cleanup-preview":
	default:
		Fail(w, 400, "不支持的任务类型")
		return
	}
	if request.Meta != nil {
		Fail(w, 400, "使用者记录由网站生成")
		return
	}
	if request.Verify != nil || request.ReportVerified {
		Fail(w, 400, "中转校验由网站安排")
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
	if request.Kind == "deploy" || request.Kind == "upgrade" {
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
	if path != "" && len(asset.Data) == 0 {
		Fail(w, 503, "固定版本安装脚本尚未配置")
		return
	}
	now := time.Now()
	job := Task{ID: ID(), UserID: u.ID, Kind: request.Kind, Host: request.SSH.Host, Mode: request.Mode, State: "queued", Phase: "queued", Message: "等待执行机领取一次性任务", CreatedAt: now.UnixMilli(), UpdatedAt: now.UnixMilli(), Remark: request.Remark, SSHFingerprint: request.SSH.Fingerprint, SSHPort: request.SSH.Port}
	envelope := &taskEnvelope{Job: executor.Job{ID: job.ID, Lease: ID(), Request: request, Script: asset, Deadline: now.Add(45 * time.Minute).UnixMilli()}, UserID: u.ID, QueuedAt: now}
	raw, _ := json.Marshal(request)
	mac := hmac.New(sha256.New, t.digestKey)
	_, _ = mac.Write(raw)
	digest := hex.EncodeToString(mac.Sum(nil))
	keySum := sha256.Sum256([]byte(u.ID + ":" + key))
	idemID := hex.EncodeToString(keySum[:])
	duplicate := false
	// Capture the in-memory facts first so the transaction runs without t.mu.
	// Concurrent submissions are still serialized by the durable busy check.
	t.mu.Lock()
	probed := t.confirmedProbe(u.ID, request.SSH) && (request.Front == nil || t.confirmedProbe(u.ID, *request.Front))
	memoryBusy := t.envelopesBusyLocked(request.SSH.Host) || (request.Front != nil && t.envelopesBusyLocked(request.Front.Host))
	t.mu.Unlock()
	err = t.app.Store.Update(func(s *State) error {
		fresh, ok := s.Users[u.ID]
		if !ok {
			return errors.New("账号不存在")
		}
		if err := taskGate(s, fresh, toolKind(request.Kind)); err != nil {
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
		if !probed {
			return errors.New("请先检查并确认每台 SSH 服务器的真实指纹，指纹检查有效期为 30 分钟")
		}
		if request.Cleanup != nil {
			if request.Cleanup.ManagedTaskID != "" {
				available := false
				for _, record := range ListDocs[ExecutorRecord](s, "executors") {
					if record.Status != "active" || record.LastSeenAt <= now.Add(-90*time.Second).UnixMilli() {
						continue
					}
					for _, capability := range record.Capabilities {
						if capability == executor.ScopedCleanupCapability {
							available = true
						}
					}
				}
				if !available {
					return errors.New("请先升级在线执行机后使用按任务清理；旧执行机不会接收此操作")
				}
			}
			tool := "deploy"
			if request.Cleanup.Scope == "relay" {
				tool = "relay"
			}
			if err := taskGate(s, fresh, tool); err != nil {
				return err
			}
			if request.Kind == "cleanup" {
				preview, ok := LoadDoc[Task](s, "tasks", request.Cleanup.PreviewID)
				if !ok || preview.UserID != u.ID || preview.Kind != "cleanup-preview" || preview.State != "succeeded" || preview.Host != job.Host || preview.SSHPort != job.SSHPort || preview.SSHFingerprint != job.SSHFingerprint || preview.UpdatedAt < now.Add(-10*time.Minute).UnixMilli() || preview.Cleanup == nil || preview.Cleanup.Scope != request.Cleanup.Scope || preview.Cleanup.Digest != request.Cleanup.Digest || preview.Cleanup.ManagedTaskID != request.Cleanup.ManagedTaskID {
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
		if memoryBusy || operationsBusy(s, request.SSH.Host) || (request.Front != nil && operationsBusy(s, request.Front.Host)) {
			return errors.New("该服务器已有未结束任务，请先核实其结果")
		}
		if request.Kind == "relay" {
			// The bandwidth range is an administrator setting shared with relay
			// codes. The user record is written by the website, never the browser.
			settings := relayCodeSettings(s)
			rate := request.RateMbps
			if rate == 0 {
				rate = settings.DefaultRateMbps
			}
			if rate < settings.RateMinMbps || rate > settings.RateMaxMbps {
				return fmt.Errorf("中转带宽需为 %d–%d Mbps", settings.RateMinMbps, settings.RateMaxMbps)
			}
			envelope.Job.Request.RateMbps = rate
			envelope.Job.Request.Meta = &executor.RelayMeta{Source: "manual", User: fresh.Email, Panel: t.app.Config.PublicURL, Remark: request.Remark}
			job.RateMbps = rate
			// A free relay only shows its self-test; it is never refused.
			t.withRelayVerify(s, &envelope.Job.Request, relayVerifySettings(s).Free)
		}
		if !t.hasExecutorFor(s, envelope.Job.Request) {
			return t.executorUnavailable(s, envelope.Job.Request)
		}
		if tool := toolKind(request.Kind); !(fresh.Role == "admin" && tool == "deploy") && toolLimit(s, tool, u.ID, now.UnixMilli()).Remaining == 0 {
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
		if err := saveTaskOperation(s, envelope); err != nil {
			return err
		}
		return SaveDoc(s, "task_idempotency", idemID, taskIdempotency{TaskID: job.ID, Digest: digest})
	})
	if err == nil && !duplicate {
		t.mu.Lock()
		t.envelopes[job.ID] = envelope
		t.mu.Unlock()
	}
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

// operationsBusy is the durable busy check. It runs inside the creating
// transaction, which serializes concurrent submissions for one server.
func operationsBusy(s *State, host string) bool {
	for _, o := range ListDocs[taskOperation](s, "task_operations") {
		if operationBusy(o) {
			for _, target := range o.Hosts {
				if taskHost(host) == target {
					return true
				}
			}
		}
	}
	return false
}

// envelopesBusyLocked also treats in-memory envelopes as busy, covering one
// left behind after its durable operation was resolved. Callers hold t.mu.
func (t *TaskService) envelopesBusyLocked(host string) bool {
	parsed, _ := netip.ParseAddr(host)
	for _, e := range t.envelopes {
		if readOnlyTaskKind(e.Job.Request.Kind) {
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
			// Relay server reads and actions are audited separately, not listed.
			if v.UserID == u.ID && v.Kind != "fingerprint" && v.Kind != "relay-inventory" && v.Kind != "relay-manage" && v.Kind != "relay-verify" {
				rows = append(rows, publicTask(v))
			}
		}
		for _, k := range []string{"deploy", "relay", "dd", "fingerprint", "cleanup", "cleanup-preview", "relay-manage"} {
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
	if u.Role != "admin" {
		job = publicTask(job)
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
	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	kind := strings.TrimSpace(r.URL.Query().Get("kind"))
	if !validTaskAuditKind(kind) {
		Fail(w, 400, "任务类型筛选无效")
		return
	}
	rows := []adminTask{}
	if err := t.app.Store.View(func(s *State) error {
		for _, task := range ListDocs[Task](s, "tasks") {
			if kind != "" && task.Kind != kind {
				continue
			}
			row := adminTask{Task: task, UserName: adminTaskUserName(s.Users[task.UserID])}
			if adminTaskMatchesQuery(row, query) {
				rows = append(rows, row)
			}
		}
		return nil
	}); err != nil {
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
	envelope := &taskEnvelope{Job: executor.Job{ID: job.ID, Lease: ID(), Request: executor.Request{Kind: "fingerprint", SSH: executor.SSH{Host: input.Host, Port: input.Port}}, Deadline: now.Add(25 * time.Second).UnixMilli()}, UserID: u.ID, QueuedAt: now, Result: result}
	err = t.app.Store.Update(func(s *State) error {
		if !t.hasExecutor(s) {
			return errors.New("没有在线执行机，无法检查真实 SSH 指纹")
		}
		if u.Role != "admin" && toolLimit(s, "fingerprint", u.ID, now.UnixMilli()).Remaining == 0 {
			return errors.New("SSH 指纹检查达到次数限制")
		}
		if err := SaveDoc(s, "tasks", job.ID, job); err != nil {
			return err
		}
		return saveTaskOperation(s, envelope)
	})
	if err != nil {
		Fail(w, 409, err.Error())
		return
	}
	t.mu.Lock()
	t.envelopes[job.ID] = envelope
	t.mu.Unlock()
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
	token, ok := bearerToken(r)
	if !ok {
		t.app.agentAuthFailed(r)
		return found, errors.New("unauthorized")
	}
	sum := sha256.Sum256([]byte(token))
	hash := hex.EncodeToString(sum[:])
	resultPath := r.URL.Path == "/api/executor/result"
	// Unknown tokens never reach the store; the read below stays authoritative.
	if err := t.app.agentCredential(r, func(i *credentialIndex) bool {
		status, known := i.executor[hash]
		return known && (status == "active" || status == "draining" || (status == "disabled" && resultPath))
	}); err != nil {
		return found, err
	}
	err := t.app.Store.View(func(s *State) error {
		for _, a := range ListDocs[ExecutorRecord](s, "executors") {
			if (a.Status == "active" || a.Status == "draining" || (a.Status == "disabled" && r.URL.Path == "/api/executor/result")) && a.TokenHash != "" && subtle.ConstantTimeCompare([]byte(hash), []byte(a.TokenHash)) == 1 {
				found = a
				break
			}
		}
		return nil
	})
	if err != nil {
		return found, errAgentAuthUnavailable
	}
	if found.ID == "" {
		t.app.agentAuthFailed(r)
		return found, errors.New("unauthorized")
	}
	return found, nil
}
func (t *TaskService) heartbeat(w http.ResponseWriter, r *http.Request) {
	a, err := t.executorAuth(r)
	if err != nil {
		agentAuthFail(w, err, "执行机令牌无效")
		return
	}
	var in struct {
		TaskID        string `json:"taskId"`
		Lease         string `json:"lease"`
		WaitingResult bool   `json:"waitingResult"`
	}
	if r.ContentLength != 0 && Decode(r, &in) != nil {
		Fail(w, 400, "心跳格式无效")
		return
	}
	err = t.app.Store.Update(func(s *State) error {
		current, ok := LoadDoc[ExecutorRecord](s, "executors", a.ID)
		if !ok || (current.Status != "active" && current.Status != "draining") {
			return errors.New("令牌已撤销")
		}
		current.LastSeenAt = time.Now().UnixMilli()
		current.IP = t.app.clientIP(r)
		current.Capabilities = executorCapabilitiesFromRequest(r)
		if v := r.Header.Get("X-MSBOOST-Agent-Version"); len(v) <= 64 && v != "" && !strings.ContainsAny(v, "\x00\r\n\t ") {
			current.Version = v
		}
		if in.WaitingResult {
			o, ok := LoadDoc[taskOperation](s, "task_operations", in.TaskID)
			if ok && o.AgentID == a.ID && o.LeaseHash == taskHash([]byte(in.Lease)) && o.Status != "resolved" {
				job, found := LoadDoc[Task](s, "tasks", in.TaskID)
				if found {
					job.Phase, job.Message = "waiting_result", "执行机报告操作已结束，正在补报结果；请勿重复部署"
					if err := SaveDoc(s, "tasks", job.ID, job); err != nil {
						return err
					}
				}
			}
		}
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
		agentAuthFail(w, err, "执行机令牌无效")
		return
	}
	if a.Status == "draining" {
		w.WriteHeader(204)
		return
	}
	_ = t.app.Store.Update(func(s *State) error {
		current, ok := LoadDoc[ExecutorRecord](s, "executors", a.ID)
		if ok {
			current.LastSeenAt = time.Now().UnixMilli()
			current.IP = t.app.clientIP(r)
			current.Capabilities = executorCapabilitiesFromRequest(r)
			if v := r.Header.Get("X-MSBOOST-Agent-Version"); len(v) <= 64 && v != "" && !strings.ContainsAny(v, "\x00\r\n\t ") {
				current.Version = v
			}
			return SaveDoc(s, "executors", a.ID, current)
		}
		return nil
	})
	timer := time.NewTimer(25 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	declared := executorCapabilitiesFromRequest(r)
	for {
		t.mu.Lock()
		var next *taskEnvelope
		for _, e := range t.envelopes {
			if !capabilitiesSatisfy(declared, requiredCapabilities(e.Job.Request)) {
				continue
			}
			if e.AgentID == "" && time.Now().UnixMilli() < e.Job.Deadline && (next == nil || e.QueuedAt.Before(next.QueuedAt)) {
				next = e
			}
		}
		if next != nil {
			// Reserve the envelope so other executors skip it, then claim it
			// durably without holding t.mu across the transaction.
			next.AgentID = a.ID
			job := next.Job
			t.mu.Unlock()
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
				o, exists := LoadDoc[taskOperation](s, "task_operations", v.ID)
				if !exists {
					if err := saveTaskOperation(s, next); err != nil {
						return err
					}
					o, _ = LoadDoc[taskOperation](s, "task_operations", v.ID)
				}
				o.AgentID, o.Status, o.UpdatedAt = a.ID, "claimed", v.UpdatedAt
				if err := SaveDoc(s, "task_operations", v.ID, o); err != nil {
					return err
				}
				return SaveDoc(s, "tasks", v.ID, v)
			})
			t.mu.Lock()
			if err != nil {
				if next.AgentID == a.ID && next.ClaimedAt.IsZero() {
					next.AgentID = ""
				}
				t.mu.Unlock()
				Fail(w, 409, "任务授权状态已变化，未交付执行")
				return
			}
			next.ClaimedAt = time.Now()
			// The delivered copy is the only one that keeps credentials.
			next.Job.Request.SSH = next.Job.Request.SSH.WithoutSecrets()
			if next.Job.Request.Front != nil {
				front := next.Job.Request.Front.WithoutSecrets()
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
		agentAuthFail(w, err, "执行机令牌无效")
		return
	}
	var out executor.Result
	if Decode(r, &out) != nil {
		Fail(w, 400, "结果格式无效")
		return
	}
	// t.mu guards only the memory maps. The transactions below settle races
	// between concurrent deliveries, expiry and revocation, so the lock is
	// never held while waiting for the store.
	t.mu.Lock()
	memory, ok := t.envelopes[out.ID]
	var envelope *taskEnvelope
	if ok {
		snapshot := *memory
		envelope = &snapshot
	}
	t.mu.Unlock()
	var operation taskOperation
	var task Task
	if err = t.app.Store.View(func(s *State) error {
		operation, _ = LoadDoc[taskOperation](s, "task_operations", out.ID)
		task, _ = LoadDoc[Task](s, "tasks", out.ID)
		return nil
	}); err != nil {
		Fail(w, 503, "结果核对暂不可用，请稍后补报")
		return
	}
	encoded, _ := json.Marshal(out)
	receipt := taskHash(encoded)
	if operation.ID != "" && operation.AgentID == a.ID && operation.LeaseHash == taskHash([]byte(out.Lease)) && operation.Receipt == receipt && operation.Receipt != "" {
		// Replenish a lost memory handoff after an ACK/COMMIT restart gap, but
		// never extend the original ten-minute sensitive-config retention.
		expires := time.UnixMilli(task.UpdatedAt).Add(10 * time.Minute)
		if len(out.Config) > 0 && task.State == "succeeded" && time.Now().Before(expires) {
			t.mu.Lock()
			t.configs[out.ID] = taskConfig{Data: append([]byte(nil), out.Config...), UserID: task.UserID, Expires: expires}
			t.mu.Unlock()
		}
		if task.State == "succeeded" && time.Now().Before(expires) {
			t.keepRelayResult(task, out, expires)
		}
		WriteJSON(w, 200, map[string]bool{"ok": true, "duplicate": true})
		return
	}
	if operation.ID == "" && ok && envelope.AgentID == a.ID && envelope.Job.Lease == out.Lease {
		// Old in-memory jobs can finish during a rolling upgrade, but cannot be replayed.
		err = t.app.Store.Update(func(s *State) error {
			if err := saveTaskOperation(s, envelope); err != nil {
				return err
			}
			operation, _ = LoadDoc[taskOperation](s, "task_operations", out.ID)
			operation.AgentID, operation.Status = a.ID, "claimed"
			return SaveDoc(s, "task_operations", out.ID, operation)
		})
		if err != nil {
			Fail(w, 503, "结果核对暂不可用")
			return
		}
	}
	if operation.AgentID != a.ID || operation.LeaseHash != taskHash([]byte(out.Lease)) || (operation.Status != "claimed" && operation.Status != "review") || task.ID == "" || a.Status == "disabled" {
		Fail(w, 409, "任务已核对或授权失效；没有再次执行远程命令，请检查原任务记录")
		return
	}
	if !ok {
		envelope = &taskEnvelope{Job: executor.Job{ID: out.ID, Lease: out.Lease, Request: operation.Request}, UserID: task.UserID, AgentID: a.ID}
	}
	if out.State != "succeeded" && out.State != "failed" && out.State != "executed" && out.State != "unknown" {
		Fail(w, 400, "结果状态无效")
		return
	}
	if envelope.Job.Request.Kind == "dd" && out.State == "succeeded" {
		Fail(w, 400, "DD 只能报告已提交、失败或提交不明确，不能声称系统安装完成")
		return
	}
	configKind := slices.Contains([]string{"deploy", "upgrade", "fetch-config", "relay", "relay-code"}, envelope.Job.Request.Kind)
	if configKind && out.State == "succeeded" && len(out.Config) == 0 {
		Fail(w, 400, "成功结果缺少客户端配置")
		return
	}
	if err := validRelayServerResult(envelope.Job.Request, out); err != nil {
		Fail(w, 400, err.Error())
		return
	}
	if err := validVerifyResult(envelope.Job.Request, out); err != nil {
		Fail(w, 400, err.Error())
		return
	}
	// A probe is remembered only once its result has been committed.
	var probe *taskProbe
	if envelope.Job.Request.Kind == "fingerprint" && out.State == "succeeded" {
		if len(out.Fingerprint) != 50 || !strings.HasPrefix(out.Fingerprint, "SHA256:") || len(out.Algorithm) > 80 {
			Fail(w, 400, "主机指纹格式无效")
			return
		}
		probe = &taskProbe{Fingerprint: out.Fingerprint, Expires: time.Now().Add(30 * time.Minute)}
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
		if !configKind {
			Fail(w, 400, "该任务不生成配置")
			return
		}
		if _, err := executor.ParseClientConfig(out.Config); err != nil {
			Fail(w, 400, "结果配置校验失败")
			return
		}
	}
	if envelope.Job.Request.Kind == "cleanup" || envelope.Job.Request.Kind == "cleanup-preview" {
		if out.State == "succeeded" && (!executor.ValidCleanupReport(out.Cleanup, envelope.Job.Request.Cleanup.Scope, envelope.Job.Request.Kind == "cleanup") || out.Cleanup.ManagedTaskID != envelope.Job.Request.Cleanup.ManagedTaskID) {
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
	t.mu.Lock()
	pendingSecret := t.codeSecrets[out.ID]
	t.mu.Unlock()
	var notice *relayCodeNotice
	err = t.app.Store.Update(func(s *State) error {
		current, exists := LoadDoc[taskOperation](s, "task_operations", out.ID)
		// An overlapping retry of the same delivery committed first.
		if exists && current.AgentID == a.ID && current.LeaseHash == operation.LeaseHash && current.Receipt == receipt {
			return errTaskResultDuplicate
		}
		// Revocation may commit between authentication and this transaction.
		if record, found := LoadDoc[ExecutorRecord](s, "executors", a.ID); !found || record.TokenHash != a.TokenHash || record.Status == "disabled" {
			return errTaskResultRevoked
		}
		if !exists || current.AgentID != a.ID || current.LeaseHash != operation.LeaseHash || current.Status == "resolved" {
			return errors.New("任务核对状态已变化")
		}
		job, ok := LoadDoc[Task](s, "tasks", out.ID)
		if !ok {
			return errors.New("任务不存在")
		}
		job.State = out.State
		job.NeedsReview = false
		job.ErrorCode, job.NextStep = "", ""
		job.Phase = "complete"
		if executor.ValidPhase(out.Phase) {
			job.Phase = out.Phase
		}
		job.Message = taskPublicMessage(out.State)
		if out.State == "succeeded" {
			switch job.Kind {
			case "upgrade":
				job.Message = "节点已升级，用户名、密码和端口没有变化，原来的配置文件可以继续使用"
			case "fetch-config":
				job.Message = "已从节点取回配置，服务器没有改动"
			}
		}
		if out.State == "failed" {
			d := executor.PublicDiagnostic(out.ErrorCode, out.Phase)
			job.ErrorCode, job.Phase, job.Message, job.NextStep = d.Code, d.Phase, d.Message, d.NextStep
		}
		if out.State == "succeeded" && out.Cleanup != nil {
			job.Cleanup = out.Cleanup
			job.Message = "清理预览已完成，尚未删除任何服务或文件；请核对清单并再次确认。"
			if out.Cleanup.Removed {
				job.Message = "清单内受管服务、文件和本项目创建的防火墙规则已清理。备份、系统账户、依赖及共享二进制缓存仍保留。"
				if out.Cleanup.Scope == "relay" && out.Cleanup.ManagedTaskID != "" {
					for _, rule := range ListDocs[UserRule](s, "user_rules") {
						front, found := LoadDoc[Task](s, "tasks", rule.FrontTaskID)
						if !found || rule.UserID != job.UserID || rule.FrontTaskID+"p" != out.Cleanup.ManagedTaskID || front.Kind != "front" || front.Host != job.Host || front.SSHPort != job.SSHPort || front.SSHFingerprint != job.SSHFingerprint || front.UpdatedAt > job.CreatedAt {
							continue
						}
						rule.FrontStatus = "cleaned"
						relayRevoke(&rule, time.Now().UnixMilli(), true)
						if err := SaveDoc(s, "user_rules", rule.UserID+":"+rule.RouteID, rule); err != nil {
							return err
						}
					}
				}
			}
		}
		job.UpdatedAt = time.Now().UnixMilli()
		job.Health = cleanTaskHealth(out.Health)
		if out.Verify != nil {
			report := *out.Verify
			job.Verify = &report
		}
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
		if err := SaveDoc(s, "tasks", job.ID, job); err != nil {
			return err
		}
		current.Status, current.Receipt, current.UpdatedAt = "resolved", receipt, job.UpdatedAt
		if job.Kind == "dd" && (out.State == "executed" || out.State == "unknown") {
			current.Status, job.NeedsReview = "review", true
			job.NextStep = "请等待重装结束并通过 VPS 控制台核实系统；确认原操作结束后解除占用，再部署 MSBOOST"
			if err := SaveDoc(s, "tasks", job.ID, job); err != nil {
				return err
			}
		}
		if err := SaveDoc(s, "task_operations", job.ID, current); err != nil {
			return err
		}
		// Relay-code counting and SSH storage commit with the result itself.
		now := time.Now()
		if job.Kind == "relay-check" && out.State == "succeeded" {
			if err := activateRelayCode(s, job.ID, pendingSecret, out.OS, now); err != nil {
				return err
			}
		}
		var err error
		if notice, err = settleRelayCodeTask(s, job.ID, &out, now); err != nil {
			return err
		}
		return finishExecutorDrain(s, a.ID)
	})
	switch {
	case errors.Is(err, errTaskResultDuplicate):
		WriteJSON(w, 200, map[string]bool{"ok": true, "duplicate": true})
		return
	case errors.Is(err, errTaskResultRevoked):
		Fail(w, 409, "任务已核对或授权失效；没有再次执行远程命令，请检查原任务记录")
		return
	case err != nil:
		Fail(w, 500, "结果保存失败")
		return
	}
	if out.State == "succeeded" {
		t.keepRelayResult(Task{ID: out.ID, UserID: envelope.UserID, Kind: envelope.Job.Request.Kind}, out, time.Now().Add(10*time.Minute))
	}
	if notice != nil {
		t.notifyRelayCodeOwner(*notice)
	}
	t.mu.Lock()
	delete(t.codeSecrets, out.ID)
	if len(out.Config) > 0 {
		t.configs[out.ID] = taskConfig{Data: append([]byte(nil), out.Config...), UserID: envelope.UserID, Expires: time.Now().Add(10 * time.Minute)}
	}
	if probe != nil {
		t.probes[taskProbeKey(envelope.UserID, envelope.Job.Request.SSH)] = *probe
	}
	if envelope.Result != nil {
		select {
		case envelope.Result <- out:
		default:
		}
	}
	if ok && t.envelopes[out.ID] == memory {
		delete(t.envelopes, out.ID)
	}
	t.mu.Unlock()
	WriteJSON(w, 200, map[string]bool{"ok": true})
}

var (
	errTaskResultDuplicate = errors.New("result already recorded")
	errTaskResultRevoked   = errors.New("executor credential no longer valid")
)

func cleanTaskHealth(in *executor.Health) *executor.Health {
	if in == nil {
		return nil
	}
	clean := func(s string) string {
		switch s {
		case "running", "stopped", "passed", "reachable", "unreachable", "not_tested", "target_tcp_reachable":
			return s
		default:
			return "unknown"
		}
	}
	bbr := "unknown"
	switch in.BBR {
	case "enabled", "unavailable", "partial", "review_required":
		bbr = in.BBR
	}
	return &executor.Health{Service: clean(in.Service), LocalSelfTest: clean(in.LocalSelfTest), PublicTCP: clean(in.PublicTCP), Game: clean(in.Game), BBR: bbr}
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
		rows[i].Online = (rows[i].Status == "active" || rows[i].Status == "draining") && rows[i].LastSeenAt > 0 && time.Now().UnixMilli()-rows[i].LastSeenAt < 90000
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
	busy := false
	for _, envelope := range t.envelopes {
		if envelope.AgentID == id {
			busy = true
			break
		}
	}
	t.mu.Unlock()
	// A claim racing this check is caught by executorPending in the transaction.
	if busy {
		Fail(w, 409, "执行机仍有待确认任务，请等待任务结束后重置令牌")
		return
	}
	token := ID() + ID()
	sum := sha256.Sum256([]byte(token))
	err := t.app.Store.Update(func(s *State) error {
		if len(executorPending(s, id)) > 0 {
			return errors.New("执行机仍有待确认任务，请先核对任务再重置令牌")
		}
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
		if in.Status == "active" || in.Status == "disabled" || in.Status == "draining" {
			if in.Status == "active" && a.TokenHash == "" {
				return errors.New("执行机凭据已撤销，请先核对原任务并重新生成安装令牌")
			}
			a.Status = in.Status
			if in.Status != "active" {
				a.Status = "disabled"
				if len(executorPending(s, a.ID)) > 0 {
					a.Status = "draining"
				}
			}
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
	err := t.app.Store.Update(func(s *State) error {
		if pending := executorPending(s, r.PathValue("id")); len(pending) > 0 {
			return errors.New("执行机有未结束或待核对任务，不能删除；请先查看任务记录，或停用以等待现有任务完成")
		}
		DeleteDoc(s, "executors", r.PathValue("id"))
		return nil
	})
	if err != nil {
		Fail(w, 409, err.Error())
		return
	}
	WriteJSON(w, 200, map[string]bool{"ok": true})
}
