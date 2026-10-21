package control

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/mozziexwz/node/internal/executor"
)

func (t *TaskService) registerRelayCodes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/relay-codes", t.createRelayCode)
	mux.HandleFunc("GET /api/relay-codes", t.listRelayCodes)
	mux.HandleFunc("GET /api/relay-codes/used", t.usedRelayCodes)
	mux.HandleFunc("GET /api/relay-codes/{id}", t.relayCodeDetail)
	mux.HandleFunc("PATCH /api/relay-codes/{id}", t.adjustRelayCode)
	mux.HandleFunc("POST /api/relay-codes/{id}/revoke", t.revokeRelayCode)
	mux.HandleFunc("POST /api/relay-codes/preview", t.previewRelayCode)
	mux.HandleFunc("POST /api/relay-codes/redeem", t.redeemRelayCode)
	mux.HandleFunc("POST /api/relay-servers/inventory", t.relayServerInventory)
	mux.HandleFunc("POST /api/relay-servers/actions", t.relayServerActions)
	mux.HandleFunc("GET /api/relay-servers/results/{id}", t.relayServerResult)
	mux.HandleFunc("GET /api/admin/relay-codes", t.adminRelayCodes)
	mux.HandleFunc("GET /api/admin/relay-codes/users/{id}", t.adminRelayCodeUser)
	mux.HandleFunc("POST /api/admin/relay-codes/users/{id}/revoke", t.adminRevokeUserRelayCodes)
	mux.HandleFunc("POST /api/admin/relay-codes/users/{id}/feature", t.adminRelayCodeFeature)
	mux.HandleFunc("POST /api/admin/relay-codes/purge", t.adminPurgeRelayCodeSecrets)
}

type relayCodeView struct {
	ID              string `json:"id"`
	Code            string `json:"code,omitempty"`
	Masked          string `json:"masked"`
	Tail            string `json:"tail"`
	ServerHost      string `json:"serverHost"`
	ServerPort      int    `json:"serverPort"`
	Fingerprint     string `json:"fingerprint"`
	AuthMethod      string `json:"authMethod"`
	KeyType         string `json:"keyType,omitempty"`
	OS              string `json:"os,omitempty"`
	MaxUses         int    `json:"maxUses"`
	UsedCount       int    `json:"usedCount"`
	Reserved        int    `json:"reserved"`
	RateMbps        int    `json:"rateMbps"`
	CreatedAt       int64  `json:"createdAt"`
	ExpiresAt       int64  `json:"expiresAt"`
	MaxExpiresAt    int64  `json:"maxExpiresAt"`
	State           string `json:"state"`
	SecretHeld      bool   `json:"secretHeld"`
	SecretDeletedAt int64  `json:"secretDeletedAt,omitempty"`
	DeleteReason    string `json:"deleteReason,omitempty"`
	Note            string `json:"note"`
	Relays          int    `json:"relays"`
	Failures        int    `json:"failures"`
}

type relayCodeUseView struct {
	ID         string `json:"id"`
	UserEmail  string `json:"userEmail"`
	MaskedIP   string `json:"maskedIp"`
	ListenPort int    `json:"listenPort,omitempty"`
	ViaFront   bool   `json:"viaFront"`
	Result     string `json:"result"`
	FailReason string `json:"failReason,omitempty"`
	CreatedAt  int64  `json:"createdAt"`
	// The target check of this use: whether it led to an MSBOOST node.
	Verify     *executor.VerifyReport `json:"verify,omitempty"`
	VerifiedAt int64                  `json:"verifiedAt,omitempty"`
}

func (t *TaskService) relayCodeView(s *State, code RelayCode, withCode bool) relayCodeView {
	settings := relayCodeSettings(s)
	_, held := s.Docs[relayCodeSecretsCollection][code.ID]
	v := relayCodeView{ID: code.ID, Masked: maskedRelayCode(code.Tail), Tail: code.Tail, ServerHost: code.ServerHost, ServerPort: code.ServerPort, Fingerprint: code.Fingerprint, AuthMethod: code.AuthMethod, KeyType: code.KeyType, OS: code.OS, MaxUses: code.MaxUses, UsedCount: code.UsedCount, Reserved: code.Reserved, RateMbps: code.RateMbps, CreatedAt: code.CreatedAt, ExpiresAt: code.ExpiresAt, State: code.State, SecretHeld: held && code.State == "active", SecretDeletedAt: code.SecretDeletedAt, DeleteReason: code.DeleteReason, Note: code.Note}
	v.MaxExpiresAt = code.CreatedAt + int64(settings.MaxTTLHours)*int64(time.Hour/time.Millisecond)
	if v.MaxExpiresAt < code.ExpiresAt {
		v.MaxExpiresAt = code.ExpiresAt
	}
	if withCode && code.State != "checking" {
		v.Code = t.relayCodeString(code)
	}
	for _, use := range ListDocs[RelayCodeUse](s, relayCodeUsesCollection) {
		if use.CodeID != code.ID {
			continue
		}
		switch use.Result {
		case "succeeded":
			v.Relays++
		case "failed":
			v.Failures++
		}
	}
	return v
}

func relayCodeUses(s *State, codeID string) []relayCodeUseView {
	rows := []relayCodeUseView{}
	for _, use := range ListDocs[RelayCodeUse](s, relayCodeUsesCollection) {
		if use.CodeID == codeID {
			rows = append(rows, relayCodeUseView{ID: use.ID, UserEmail: use.UserEmail, MaskedIP: maskIP(use.TargetHost), ListenPort: use.ListenPort, ViaFront: use.ViaFront, Result: use.Result, FailReason: use.FailReason, CreatedAt: use.CreatedAt, Verify: use.Verify, VerifiedAt: use.VerifiedAt})
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].CreatedAt > rows[j].CreatedAt })
	return rows
}

func (t *TaskService) relayIdempotency(userID, purpose, key string, input any) (string, string, error) {
	if len(key) < 8 || len(key) > 128 {
		return "", "", errors.New("请提供 8–128 字符 Idempotency-Key")
	}
	raw, _ := json.Marshal(input)
	mac := hmac.New(sha256.New, t.digestKey)
	_, _ = mac.Write([]byte(purpose + ":"))
	_, _ = mac.Write(raw)
	sum := sha256.Sum256([]byte(userID + ":" + purpose + ":" + key))
	return hex.EncodeToString(sum[:]), hex.EncodeToString(mac.Sum(nil)), nil
}

func relayTask(u *User, kind string, ssh executor.SSH, now time.Time) Task {
	return Task{ID: ID(), UserID: u.ID, Kind: kind, Host: ssh.Host, State: "queued", Phase: "queued", Message: "等待执行机领取一次性任务", CreatedAt: now.UnixMilli(), UpdatedAt: now.UnixMilli(), SSHFingerprint: ssh.Fingerprint, SSHPort: ssh.Port}
}

func (t *TaskService) publishEnvelope(envelope *taskEnvelope, secret *pendingCodeSecret) {
	t.mu.Lock()
	t.envelopes[envelope.Job.ID] = envelope
	if secret != nil {
		t.codeSecrets[envelope.Job.ID] = *secret
	}
	t.mu.Unlock()
}

func (t *TaskService) createRelayCode(w http.ResponseWriter, r *http.Request) {
	if t.initErr != nil {
		Fail(w, 503, "任务服务密钥不可用")
		return
	}
	u, err := t.app.User(r)
	if err != nil {
		Fail(w, 401, "请先登录")
		return
	}
	if err = t.app.Store.View(func(s *State) error { return relayCodeGate(s, u) }); err != nil {
		Fail(w, 403, err.Error())
		return
	}
	var in struct {
		SSH         executor.SSH `json:"ssh"`
		MaxUses     int          `json:"maxUses"`
		TTLHours    int          `json:"ttlHours"`
		RateMbps    int          `json:"rateMbps"`
		Note        string       `json:"note"`
		Acknowledge bool         `json:"acknowledge"`
	}
	if err = Decode(r, &in); err != nil {
		Fail(w, 400, "中转码参数格式无效")
		return
	}
	in.Note = strings.TrimSpace(in.Note)
	if !in.Acknowledge {
		Fail(w, 400, "请先确认网站会在有效期内加密暂存这台服务器的 SSH 信息")
		return
	}
	if err = validRelayCodeNote(in.Note); err != nil {
		Fail(w, 400, err.Error())
		return
	}
	request := executor.Request{Kind: "relay-check", SSH: in.SSH}
	if err = executor.ValidateRequest(request); err != nil {
		Fail(w, 400, err.Error())
		return
	}
	idemID, digest, err := t.relayIdempotency(u.ID, "relay-code", r.Header.Get("Idempotency-Key"), in)
	if err != nil {
		Fail(w, 400, err.Error())
		return
	}
	plain, _ := json.Marshal(in.SSH)
	sealed, err := t.app.Seal(plain)
	clear(plain)
	if err != nil {
		Fail(w, 500, "SSH 加密失败")
		return
	}
	raw := newRelayCode()
	sealedCode, err := t.app.Seal([]byte(raw))
	if err != nil {
		Fail(w, 500, "中转码加密失败")
		return
	}
	now := time.Now()
	job := relayTask(u, "relay-check", in.SSH, now)
	envelope := &taskEnvelope{Job: executor.Job{ID: job.ID, Lease: ID(), Request: request, Deadline: now.Add(5 * time.Minute).UnixMilli()}, UserID: u.ID, QueuedAt: now}
	code := RelayCode{ID: ID(), OwnerID: u.ID, Digest: t.relayCodeDigest(raw), SealedCode: sealedCode, Tail: raw[8:], ServerHost: in.SSH.Host, ServerPort: in.SSH.Port, Fingerprint: in.SSH.Fingerprint, AuthMethod: "password", MaxUses: in.MaxUses, RateMbps: in.RateMbps, TTLHours: in.TTLHours, CreatedAt: now.UnixMilli(), State: "checking", Note: in.Note, CheckTaskID: job.ID}
	if in.SSH.UsesKey() {
		code.AuthMethod, code.KeyType = "key", executor.KeyLabel(in.SSH)
	}
	code.ExpiresAt = now.Add(time.Duration(in.TTLHours) * time.Hour).UnixMilli()
	job.RateMbps, job.CodeTail = in.RateMbps, code.Tail
	t.mu.Lock()
	probed := t.confirmedProbe(u.ID, in.SSH)
	memoryBusy := t.envelopesBusyLocked(in.SSH.Host)
	t.mu.Unlock()
	duplicate := false
	err = t.app.Store.Update(func(s *State) error {
		fresh := s.Users[u.ID]
		if err := relayCodeGate(s, fresh); err != nil {
			return err
		}
		if prev, ok := LoadDoc[taskIdempotency](s, "task_idempotency", idemID); ok {
			if prev.Digest != digest {
				return errors.New("同一幂等键不能提交不同参数")
			}
			duplicate = true
			job, _ = LoadDoc[Task](s, "tasks", prev.TaskID)
			if link, found := LoadDoc[relayCodeTaskLink](s, relayCodeTasksCollection, prev.TaskID); found {
				code, _ = LoadDoc[RelayCode](s, relayCodesCollection, link.CodeID)
			}
			return nil
		}
		settings := relayCodeSettings(s)
		switch {
		case in.MaxUses < 1 || in.MaxUses > settings.MaxUses:
			return fmt.Errorf("可使用次数需为 1–%d 次", settings.MaxUses)
		case in.TTLHours < 1 || in.TTLHours > settings.MaxTTLHours:
			return fmt.Errorf("有效期需为 1–%d 小时", settings.MaxTTLHours)
		case in.RateMbps < settings.RateMinMbps || in.RateMbps > settings.RateMaxMbps:
			return fmt.Errorf("中转带宽需为 %d–%d Mbps", settings.RateMinMbps, settings.RateMaxMbps)
		case !probed:
			return errors.New("请先检查并确认中转服务器的真实指纹，指纹检查有效期为 30 分钟")
		case memoryBusy || operationsBusy(s, in.SSH.Host):
			return errors.New("该服务器已有未结束任务，请先核实其结果")
		case !t.hasExecutorFor(s, request):
			return t.executorUnavailable(s, request)
		case toolLimit(s, "relay", u.ID, now.UnixMilli()).Remaining == 0:
			return errors.New("本工具已达到次数限制，请等待下一次可用时间")
		}
		active := 0
		for _, other := range ownerRelayCodes(s, u.ID) {
			if usableRelayCode(other) {
				active++
			}
		}
		if active >= settings.MaxActive {
			return fmt.Errorf("同时有效的中转码最多 %d 个。作废不再使用的中转码，或等它们到期后再生成。", settings.MaxActive)
		}
		if err := trustSSH(s, u.ID, in.SSH); err != nil {
			return err
		}
		if err := SaveDoc(s, relayCodesCollection, code.ID, code); err != nil {
			return err
		}
		if err := SaveDoc(s, "tasks", job.ID, job); err != nil {
			return err
		}
		if err := saveTaskOperation(s, envelope); err != nil {
			return err
		}
		if err := SaveDoc(s, relayCodeTasksCollection, job.ID, relayCodeTaskLink{TaskID: job.ID, CodeID: code.ID, OwnerID: u.ID, Purpose: "check", CreatedAt: now.UnixMilli()}); err != nil {
			return err
		}
		return SaveDoc(s, "task_idempotency", idemID, taskIdempotency{TaskID: job.ID, Digest: digest})
	})
	if err != nil {
		Fail(w, 409, err.Error())
		return
	}
	if !duplicate {
		t.publishEnvelope(envelope, &pendingCodeSecret{CodeID: code.ID, Sealed: sealed, Expires: now.Add(10 * time.Minute)})
	}
	var view relayCodeView
	_ = t.app.Store.View(func(s *State) error { view = t.relayCodeView(s, code, false); return nil })
	status := 202
	if duplicate {
		status = 200
	}
	WriteJSON(w, status, map[string]any{"code": view, "task": publicTask(job)})
}

func (t *TaskService) listRelayCodes(w http.ResponseWriter, r *http.Request) {
	u, err := t.app.User(r)
	if err != nil {
		Fail(w, 401, "请先登录")
		return
	}
	out := map[string]any{}
	err = t.app.Store.View(func(s *State) error {
		settings := relayCodeSettings(s)
		codes := []relayCodeView{}
		active, held, built, failures, earliest := 0, 0, 0, 0, int64(0)
		for _, code := range ownerRelayCodes(s, u.ID) {
			view := t.relayCodeView(s, code, true)
			codes = append(codes, view)
			if usableRelayCode(code) {
				active++
			}
			if view.SecretHeld {
				held++
				if earliest == 0 || code.ExpiresAt < earliest {
					earliest = code.ExpiresAt
				}
			}
			built += view.Relays
			failures += view.Failures
		}
		used := 0
		for _, use := range ListDocs[RelayCodeUse](s, relayCodeUsesCollection) {
			if use.UserID == u.ID && use.Result == "succeeded" {
				used++
			}
		}
		disabled := false
		if flag, ok := LoadDoc[relayCodeUserFlag](s, relayCodeFlagsCollection, u.ID); ok {
			disabled = flag.Disabled
		}
		out["codes"] = codes
		out["stats"] = map[string]any{"active": active, "maxActive": settings.MaxActive, "secrets": held, "earliestDeletion": earliest, "relays": built, "failures": failures, "used": used}
		out["settings"] = publicRelayCodeSettings(settings)
		out["disabled"] = disabled
		out["limit"] = toolLimit(s, "relay", u.ID, time.Now().UnixMilli())
		out["manageLimit"] = toolLimit(s, "relay-manage", u.ID, time.Now().UnixMilli())
		return nil
	})
	if err != nil {
		Fail(w, 500, "中转码读取失败")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	WriteJSON(w, 200, out)
}

func (t *TaskService) relayCodeDetail(w http.ResponseWriter, r *http.Request) {
	u, err := t.app.User(r)
	if err != nil {
		Fail(w, 401, "请先登录")
		return
	}
	var view relayCodeView
	var uses []relayCodeUseView
	found := false
	_ = t.app.Store.View(func(s *State) error {
		code, ok := LoadDoc[RelayCode](s, relayCodesCollection, r.PathValue("id"))
		if !ok || code.OwnerID != u.ID {
			return nil
		}
		found = true
		view, uses = t.relayCodeView(s, code, true), relayCodeUses(s, code.ID)
		return nil
	})
	if !found {
		Fail(w, 404, "中转码不存在")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	WriteJSON(w, 200, map[string]any{"code": view, "uses": uses})
}

func (t *TaskService) adjustRelayCode(w http.ResponseWriter, r *http.Request) {
	u, err := t.app.User(r)
	if err != nil {
		Fail(w, 401, "请先登录")
		return
	}
	var in struct {
		ExtendHours int     `json:"extendHours"`
		MaxUses     *int    `json:"maxUses"`
		Note        *string `json:"note"`
	}
	if err = Decode(r, &in); err != nil {
		Fail(w, 400, "调整参数格式无效")
		return
	}
	if in.ExtendHours < 0 || in.ExtendHours > relayCodeMaxTTLHours {
		Fail(w, 400, "延长时间无效")
		return
	}
	var view relayCodeView
	err = t.app.Store.Update(func(s *State) error {
		code, ok := LoadDoc[RelayCode](s, relayCodesCollection, r.PathValue("id"))
		if !ok || code.OwnerID != u.ID {
			return errors.New("中转码不存在")
		}
		secret, held := LoadDoc[relayCodeSecret](s, relayCodeSecretsCollection, code.ID)
		if code.State != "active" || !held {
			return errors.New("只有 SSH 仍在暂存期的中转码可以调整")
		}
		if err := relayCodeGate(s, s.Users[u.ID]); err != nil {
			return err
		}
		settings := relayCodeSettings(s)
		if in.ExtendHours > 0 {
			limit := code.CreatedAt + int64(settings.MaxTTLHours)*int64(time.Hour/time.Millisecond)
			next := code.ExpiresAt + int64(in.ExtendHours)*int64(time.Hour/time.Millisecond)
			if next > limit {
				next = limit
			}
			if next <= code.ExpiresAt {
				return fmt.Errorf("从生成时算起总长不超过 %d 小时，已无法再延长", settings.MaxTTLHours)
			}
			code.ExpiresAt, secret.ExpiresAt = next, next
		}
		if in.MaxUses != nil {
			if *in.MaxUses < code.MaxUses || *in.MaxUses > settings.MaxUses {
				return fmt.Errorf("可使用次数只能增加，最多 %d 次", settings.MaxUses)
			}
			code.MaxUses = *in.MaxUses
		}
		if in.Note != nil {
			note := strings.TrimSpace(*in.Note)
			if err := validRelayCodeNote(note); err != nil {
				return err
			}
			code.Note = note
		}
		if err := SaveDoc(s, relayCodeSecretsCollection, code.ID, secret); err != nil {
			return err
		}
		if err := SaveDoc(s, relayCodesCollection, code.ID, code); err != nil {
			return err
		}
		view = t.relayCodeView(s, code, true)
		return nil
	})
	if err != nil {
		Fail(w, 409, err.Error())
		return
	}
	WriteJSON(w, 200, map[string]any{"code": view})
}

// revokeRelayCode ends the code and deletes its SSH in the same transaction.
// When asked, the relays built from it are deleted by one task that carries
// the SSH in memory only, so storage never outlives the revocation.
func (t *TaskService) revokeRelayCode(w http.ResponseWriter, r *http.Request) {
	u, err := t.app.User(r)
	if err != nil {
		Fail(w, 401, "请先登录")
		return
	}
	var in struct {
		DeleteRelays bool `json:"deleteRelays"`
	}
	if err = Decode(r, &in); err != nil {
		Fail(w, 400, "作废参数格式无效")
		return
	}
	now := time.Now()
	var view relayCodeView
	var job Task
	var envelope *taskEnvelope
	warning := ""
	items := []string{}
	err = t.app.Store.Update(func(s *State) error {
		code, ok := LoadDoc[RelayCode](s, relayCodesCollection, r.PathValue("id"))
		if !ok || code.OwnerID != u.ID {
			return errors.New("中转码不存在")
		}
		if code.State == "checking" {
			return errors.New("中转码正在检查服务器，请稍后再作废")
		}
		if code.State != "active" {
			return errors.New("中转码已失效")
		}
		if in.DeleteRelays {
			for _, use := range ListDocs[RelayCodeUse](s, relayCodeUsesCollection) {
				if use.CodeID == code.ID && use.Result == "succeeded" {
					items = append(items, use.TaskID+"r")
				}
			}
			sort.Strings(items)
		}
		if len(items) > 0 {
			ssh, openErr := t.openRelayCodeSSH(s, code)
			request := executor.Request{Kind: "relay-manage", SSH: ssh, Manage: &executor.RelayManage{Action: "delete", Items: items}}
			t.mu.Lock()
			memoryBusy := t.envelopesBusyLocked(code.ServerHost)
			t.mu.Unlock()
			switch {
			case openErr != nil:
				warning = openErr.Error()
			case executor.ValidateRequest(request) != nil:
				warning = "暂存的 SSH 无效"
			case memoryBusy || operationsBusy(s, code.ServerHost):
				warning = "中转服务器正在处理其他任务"
			case !t.hasExecutorFor(s, request):
				warning = t.executorUnavailable(s, request).Error()
			default:
				job = relayTask(u, "relay-manage", ssh, now)
				job.CodeTail = code.Tail
				envelope = &taskEnvelope{Job: executor.Job{ID: job.ID, Lease: ID(), Request: request, Deadline: now.Add(10 * time.Minute).UnixMilli()}, UserID: u.ID, QueuedAt: now}
				if err := SaveDoc(s, "tasks", job.ID, job); err != nil {
					return err
				}
				if err := saveTaskOperation(s, envelope); err != nil {
					return err
				}
				relayCodeAuditEvent(s, relayCodeEvent{Kind: "manage", OwnerID: u.ID, CodeID: code.ID, CodeTail: code.Tail, Actor: u.ID, Host: code.ServerHost, Action: "delete", Count: len(items)})
			}
		}
		if err := endRelayCode(s, &code, "revoked", u.ID, now); err != nil {
			return err
		}
		view = t.relayCodeView(s, code, true)
		return nil
	})
	if err != nil {
		Fail(w, 409, err.Error())
		return
	}
	out := map[string]any{"code": view, "items": len(items)}
	if envelope != nil {
		t.publishEnvelope(envelope, nil)
		out["task"] = publicTask(job)
	}
	if warning != "" {
		out["warning"] = "中转码已作废，SSH 已删除；但没能删除已建立的中转：" + warning + "。需要时请在「中转服务器管理」手动输入 SSH 处理。"
	}
	WriteJSON(w, 200, out)
}

func (t *TaskService) relayCodeAttempt(w http.ResponseWriter, r *http.Request, u *User) bool {
	limits := defaultRelayCodeSettings()
	_ = t.app.Store.View(func(s *State) error { limits = relayCodeSettings(s); return nil })
	if !t.app.allow("relay-code-user:"+u.ID, limits.RedeemPerAccount, time.Hour) || !t.app.allow("relay-code-ip:"+t.app.rateIP(r), limits.RedeemPerIP, time.Hour) {
		Fail(w, 429, fmt.Sprintf("尝试太频繁：每小时最多 %d 次，请稍后再试", limits.RedeemPerAccount))
		return false
	}
	return true
}

// findRelayCode returns the code for an exact normalized match. Unknown codes
// and codes of a disabled feature are indistinguishable to the caller.
func (t *TaskService) findRelayCode(s *State, input string) (RelayCode, error) {
	normalized, ok := normalizeRelayCode(input)
	if !ok {
		return RelayCode{}, errors.New("中转码无效：请核对后重试")
	}
	digest := t.relayCodeDigest(normalized)
	for _, code := range ListDocs[RelayCode](s, relayCodesCollection) {
		if hmac.Equal([]byte(code.Digest), []byte(digest)) {
			switch {
			case code.State == "checking":
				return code, errors.New("中转码无效：请核对后重试")
			case code.State != "active" || time.Now().UnixMilli() >= code.ExpiresAt:
				return code, errors.New("中转码已用完、已过期或已作废：请联系拥有者重新生成")
			}
			return code, nil
		}
	}
	return RelayCode{}, errors.New("中转码无效：请核对后重试")
}

func (t *TaskService) previewRelayCode(w http.ResponseWriter, r *http.Request) {
	u, err := t.app.User(r)
	if err != nil {
		Fail(w, 401, "请先登录")
		return
	}
	var in struct {
		Code string `json:"code"`
	}
	if err = Decode(r, &in); err != nil {
		Fail(w, 400, "请填写中转码")
		return
	}
	if err = t.app.Store.View(func(s *State) error { return relayCodeGate(s, u) }); err != nil {
		Fail(w, 403, err.Error())
		return
	}
	if !t.relayCodeAttempt(w, r, u) {
		return
	}
	var out map[string]any
	status := 200
	err = t.app.Store.View(func(s *State) error {
		code, err := t.findRelayCode(s, in.Code)
		if err != nil {
			status = 404
			if code.ID != "" && code.State != "checking" {
				status = 410
			}
			return err
		}
		owner := s.Users[code.OwnerID]
		ownerEmail := ""
		if owner != nil {
			ownerEmail = owner.Email
		}
		remaining := code.MaxUses - code.UsedCount - code.Reserved
		if remaining < 0 {
			remaining = 0
		}
		out = map[string]any{"ownerMasked": maskOwnerEmail(ownerEmail), "serverMasked": maskIP(code.ServerHost), "os": code.OS, "maxUses": code.MaxUses, "remaining": remaining, "expiresAt": code.ExpiresAt, "rateMbps": code.RateMbps, "tail": code.Tail, "usable": remaining > 0}
		return nil
	})
	if err != nil {
		Fail(w, status, err.Error())
		return
	}
	WriteJSON(w, 200, out)
}

func (t *TaskService) redeemRelayCode(w http.ResponseWriter, r *http.Request) {
	if t.initErr != nil {
		Fail(w, 503, "任务服务密钥不可用")
		return
	}
	u, err := t.app.User(r)
	if err != nil {
		Fail(w, 401, "请先登录")
		return
	}
	if err = t.app.Store.View(func(s *State) error { return relayCodeGate(s, u) }); err != nil {
		Fail(w, 403, err.Error())
		return
	}
	var in struct {
		Code         string          `json:"code"`
		ClientConfig json.RawMessage `json:"clientConfig"`
		Remark       string          `json:"remark"`
		Front        *executor.SSH   `json:"front"`
		Consent      bool            `json:"consent"`
	}
	if err = Decode(r, &in); err != nil {
		Fail(w, 400, "使用中转码的参数格式无效")
		return
	}
	if !in.Consent {
		Fail(w, 400, "请先同意在中转服务器上记录你的 QQ 邮箱和节点 IP")
		return
	}
	info, err := executor.ParseClientConfig(in.ClientConfig)
	if err != nil {
		Fail(w, 400, err.Error())
		return
	}
	if len([]rune(in.Remark)) > 60 || strings.ContainsAny(in.Remark, "\x00\r\n") {
		Fail(w, 400, "备注最多 60 个字符，不能包含换行")
		return
	}
	if in.Front != nil {
		if err = executor.ValidateSSH(*in.Front); err != nil {
			Fail(w, 400, err.Error())
			return
		}
	}
	idemID, digest, err := t.relayIdempotency(u.ID, "relay-code-use", r.Header.Get("Idempotency-Key"), in)
	if err != nil {
		Fail(w, 400, err.Error())
		return
	}
	if !t.relayCodeAttempt(w, r, u) {
		return
	}
	now := time.Now()
	probed := true
	if in.Front != nil {
		t.mu.Lock()
		probed = t.confirmedProbe(u.ID, *in.Front)
		t.mu.Unlock()
	}
	var job Task
	var envelope *taskEnvelope
	duplicate := false
	status := 409
	err = t.app.Store.Update(func(s *State) error {
		fresh := s.Users[u.ID]
		if err := relayCodeGate(s, fresh); err != nil {
			return err
		}
		if prev, ok := LoadDoc[taskIdempotency](s, "task_idempotency", idemID); ok {
			if prev.Digest != digest {
				return errors.New("同一幂等键不能提交不同参数")
			}
			duplicate = true
			job, _ = LoadDoc[Task](s, "tasks", prev.TaskID)
			return nil
		}
		code, err := t.findRelayCode(s, in.Code)
		if err != nil {
			status = 404
			if code.ID != "" && code.State != "checking" {
				status = 410
			}
			return err
		}
		if code.MaxUses-code.UsedCount-code.Reserved <= 0 {
			status = 410
			return errors.New("中转码的可用次数已被占满：请稍后再试或联系拥有者")
		}
		ssh, err := t.openRelayCodeSSH(s, code)
		if err != nil {
			status = 410
			return err
		}
		owner := s.Users[code.OwnerID]
		if owner == nil {
			status = 410
			return errors.New("中转码已用完、已过期或已作废：请联系拥有者重新生成")
		}
		// The owner's private note is recorded only on the owner's server.
		request := executor.Request{Kind: "relay-code", SSH: ssh, ClientConfig: in.ClientConfig, Front: in.Front, Remark: in.Remark, RateMbps: code.RateMbps, Meta: &executor.RelayMeta{Source: "code", CodeTail: code.Tail, User: fresh.Email, Panel: t.app.Config.PublicURL, Remark: code.Note}}
		// The owner's line must lead to a real MSBOOST node.
		t.withRelayVerify(s, &request, relayVerifySettings(s).Code)
		if err := executor.ValidateRequest(request); err != nil {
			return err
		}
		t.mu.Lock()
		memoryBusy := t.envelopesBusyLocked(ssh.Host) || (in.Front != nil && t.envelopesBusyLocked(in.Front.Host))
		t.mu.Unlock()
		switch {
		case !probed:
			return errors.New("请先检查并确认前置机的真实指纹，指纹检查有效期为 30 分钟")
		case memoryBusy || operationsBusy(s, ssh.Host) || (in.Front != nil && operationsBusy(s, in.Front.Host)):
			return errors.New("这台中转服务器正在处理其他任务，请稍后再试（不计次）")
		case !t.hasExecutorFor(s, request):
			return t.executorUnavailable(s, request)
		case toolLimit(s, "relay", u.ID, now.UnixMilli()).Remaining == 0:
			return errors.New("本工具已达到次数限制，请等待下一次可用时间")
		}
		if in.Front != nil {
			if err := trustSSH(s, u.ID, *in.Front); err != nil {
				return err
			}
		}
		job = relayTask(u, "relay-code", ssh, now)
		// The owner's SSH port and host key are not the user's to see.
		job.SSHPort, job.SSHFingerprint = 0, ""
		job.Remark, job.RateMbps, job.CodeTail = in.Remark, code.RateMbps, code.Tail
		envelope = &taskEnvelope{Job: executor.Job{ID: job.ID, Lease: ID(), Request: request, Deadline: now.Add(45 * time.Minute).UnixMilli()}, UserID: u.ID, QueuedAt: now}
		code.Reserved++
		use := RelayCodeUse{ID: job.ID, CodeID: code.ID, CodeTail: code.Tail, OwnerID: code.OwnerID, OwnerMasked: maskOwnerEmail(owner.Email), ServerHost: code.ServerHost, UserID: u.ID, UserEmail: fresh.Email, TargetHost: info.TargetHost, TargetPort: info.TargetPort, ViaFront: in.Front != nil, RateMbps: code.RateMbps, TaskID: job.ID, Remark: in.Remark, Result: "pending", CreatedAt: now.UnixMilli(), UpdatedAt: now.UnixMilli()}
		if in.Front != nil {
			use.FrontHost = in.Front.Host
		}
		if err := SaveDoc(s, relayCodesCollection, code.ID, code); err != nil {
			return err
		}
		if err := SaveDoc(s, "tasks", job.ID, job); err != nil {
			return err
		}
		if err := SaveDoc(s, relayCodeUsesCollection, use.ID, use); err != nil {
			return err
		}
		if err := SaveDoc(s, relayCodeTasksCollection, job.ID, relayCodeTaskLink{TaskID: job.ID, CodeID: code.ID, OwnerID: code.OwnerID, Purpose: "use", CreatedAt: now.UnixMilli()}); err != nil {
			return err
		}
		if err := saveTaskOperation(s, envelope); err != nil {
			return err
		}
		return SaveDoc(s, "task_idempotency", idemID, taskIdempotency{TaskID: job.ID, Digest: digest})
	})
	if err != nil {
		Fail(w, status, err.Error())
		return
	}
	if duplicate {
		WriteJSON(w, 200, publicTask(job))
		return
	}
	t.publishEnvelope(envelope, nil)
	WriteJSON(w, 202, publicTask(job))
}

type relayCodeUsedView struct {
	ID           string `json:"id"`
	TaskID       string `json:"taskId"`
	ServerMasked string `json:"serverMasked"`
	OwnerMasked  string `json:"ownerMasked"`
	EntryPort    int    `json:"entryPort"`
	ViaFront     bool   `json:"viaFront"`
	FrontHost    string `json:"frontHost,omitempty"`
	Target       string `json:"target"`
	RateMbps     int    `json:"rateMbps"`
	CodeTail     string `json:"codeTail"`
	Remark       string `json:"remark,omitempty"`
	CreatedAt    int64  `json:"createdAt"`
}

func (t *TaskService) usedRelayCodes(w http.ResponseWriter, r *http.Request) {
	u, err := t.app.User(r)
	if err != nil {
		Fail(w, 401, "请先登录")
		return
	}
	rows := []relayCodeUsedView{}
	_ = t.app.Store.View(func(s *State) error {
		for _, use := range ListDocs[RelayCodeUse](s, relayCodeUsesCollection) {
			if use.UserID != u.ID || use.Result != "succeeded" {
				continue
			}
			rows = append(rows, relayCodeUsedView{ID: use.ID, TaskID: use.TaskID, ServerMasked: maskIP(use.ServerHost), OwnerMasked: use.OwnerMasked, EntryPort: use.EntryPort, ViaFront: use.ViaFront, FrontHost: use.FrontHost, Target: fmt.Sprintf("%s:%d", use.TargetHost, use.TargetPort), RateMbps: use.RateMbps, CodeTail: use.CodeTail, Remark: use.Remark, CreatedAt: use.CreatedAt})
		}
		return nil
	})
	sort.Slice(rows, func(i, j int) bool { return rows[i].CreatedAt > rows[j].CreatedAt })
	WriteJSON(w, 200, map[string]any{"relays": rows})
}

// relayServerSSH resolves the connection for a read or an action: either
// the SSH a code still stores, or SSH typed into the page for this request.
func (t *TaskService) relayServerSSH(s *State, u *User, codeID string, typed *executor.SSH) (executor.SSH, string, error) {
	if codeID != "" {
		if err := relayCodeGate(s, u); err != nil {
			return executor.SSH{}, "", err
		}
		if !relayCodeSettings(s).ManageWithSecret {
			return executor.SSH{}, "", errors.New("管理员未允许用中转码暂存的 SSH 管理中转服务器，请输入 SSH")
		}
		code, ok := LoadDoc[RelayCode](s, relayCodesCollection, codeID)
		if !ok || code.OwnerID != u.ID {
			return executor.SSH{}, "", errors.New("中转码不存在")
		}
		ssh, err := t.openRelayCodeSSH(s, code)
		return ssh, code.Tail, err
	}
	if typed == nil {
		return executor.SSH{}, "", errors.New("请选择中转码或输入 SSH")
	}
	if err := taskGate(s, u, "relay"); err != nil {
		return executor.SSH{}, "", err
	}
	t.mu.Lock()
	probed := t.confirmedProbe(u.ID, *typed)
	t.mu.Unlock()
	if !probed {
		return executor.SSH{}, "", errors.New("请先检查并确认中转服务器的真实指纹，指纹检查有效期为 30 分钟")
	}
	if err := trustSSH(s, u.ID, *typed); err != nil {
		return executor.SSH{}, "", err
	}
	return *typed, "", nil
}

func (t *TaskService) queueRelayServerTask(w http.ResponseWriter, r *http.Request, u *User, codeID string, typed *executor.SSH, manage *executor.RelayManage) {
	if typed != nil && codeID != "" {
		Fail(w, 400, "请只选择一种 SSH 来源")
		return
	}
	if typed != nil {
		if err := executor.ValidateSSH(*typed); err != nil {
			Fail(w, 400, err.Error())
			return
		}
	}
	kind := "relay-inventory"
	if manage != nil {
		kind = "relay-manage"
	}
	now := time.Now()
	var job Task
	var envelope *taskEnvelope
	err := t.app.Store.Update(func(s *State) error {
		fresh := s.Users[u.ID]
		if fresh == nil {
			return errors.New("账号不存在")
		}
		ssh, tail, err := t.relayServerSSH(s, fresh, codeID, typed)
		if err != nil {
			return err
		}
		request := executor.Request{Kind: kind, SSH: ssh, Manage: manage, ReportVerified: kind == "relay-inventory"}
		if err := executor.ValidateRequest(request); err != nil {
			return err
		}
		t.mu.Lock()
		memoryBusy := kind == "relay-manage" && t.envelopesBusyLocked(ssh.Host)
		t.mu.Unlock()
		switch {
		case memoryBusy || (kind == "relay-manage" && operationsBusy(s, ssh.Host)):
			return errors.New("该服务器已有未结束任务，请稍后再试")
		case !t.hasExecutorFor(s, request):
			return t.executorUnavailable(s, request)
		case toolLimit(s, "relay-manage", u.ID, now.UnixMilli()).Remaining == 0:
			return errors.New("中转服务器管理已达到次数限制，请等待下一次可用时间")
		}
		job = relayTask(fresh, kind, ssh, now)
		job.CodeTail = tail
		envelope = &taskEnvelope{Job: executor.Job{ID: job.ID, Lease: ID(), Request: request, Deadline: now.Add(5 * time.Minute).UnixMilli()}, UserID: u.ID, QueuedAt: now}
		if err := SaveDoc(s, "tasks", job.ID, job); err != nil {
			return err
		}
		if err := saveTaskOperation(s, envelope); err != nil {
			return err
		}
		event := relayCodeEvent{Kind: "inventory", OwnerID: u.ID, CodeTail: tail, Actor: u.ID, Host: ssh.Host}
		if manage != nil {
			event.Kind, event.Action, event.Count = "manage", manage.Action, len(manage.Items)
		}
		relayCodeAuditEvent(s, event)
		return nil
	})
	if err != nil {
		Fail(w, 409, err.Error())
		return
	}
	t.publishEnvelope(envelope, nil)
	WriteJSON(w, 202, publicTask(job))
}

type relayServerInput struct {
	CodeID   string        `json:"codeId"`
	SSH      *executor.SSH `json:"ssh"`
	Action   string        `json:"action"`
	Items    []string      `json:"items"`
	RateMbps int           `json:"rateMbps"`
}

func (t *TaskService) relayServerInventory(w http.ResponseWriter, r *http.Request) {
	u, err := t.app.User(r)
	if err != nil {
		Fail(w, 401, "请先登录")
		return
	}
	var in relayServerInput
	if err = Decode(r, &in); err != nil || in.Action != "" || len(in.Items) > 0 || in.RateMbps != 0 {
		Fail(w, 400, "读取参数格式无效")
		return
	}
	t.queueRelayServerTask(w, r, u, in.CodeID, in.SSH, nil)
}

func (t *TaskService) relayServerActions(w http.ResponseWriter, r *http.Request) {
	u, err := t.app.User(r)
	if err != nil {
		Fail(w, 401, "请先登录")
		return
	}
	var in relayServerInput
	if err = Decode(r, &in); err != nil {
		Fail(w, 400, "操作参数格式无效")
		return
	}
	t.queueRelayServerTask(w, r, u, in.CodeID, in.SSH, &executor.RelayManage{Action: in.Action, Items: in.Items, RateMbps: in.RateMbps})
}

type relayResult struct {
	UserID    string
	Kind      string
	OS        string
	Inventory []executor.RelayEntry
	Managed   []executor.RelayManageResult
	Expires   time.Time
}

func (t *TaskService) keepRelayResult(task Task, out executor.Result, expires time.Time) {
	if task.Kind != "relay-inventory" && task.Kind != "relay-manage" {
		return
	}
	t.mu.Lock()
	t.relayResults[task.ID] = relayResult{UserID: task.UserID, Kind: task.Kind, OS: out.OS, Inventory: out.Inventory, Managed: out.Managed, Expires: expires}
	t.mu.Unlock()
}

type relayServerRow struct {
	ID          string `json:"id"`
	Role        string `json:"role"`
	Source      string `json:"source"`
	CodeTail    string `json:"codeTail,omitempty"`
	User        string `json:"user,omitempty"`
	MaskedIP    string `json:"maskedIp"`
	Remark      string `json:"remark,omitempty"`
	Port        int    `json:"port"`
	RateMbps    int    `json:"rateMbps"`
	State       string `json:"state"`
	Connections int    `json:"connections"`
	CreatedAt   int64  `json:"createdAt"`
	Recorded    bool   `json:"recorded"`
	// Verified is the last target check written on the relay server.
	Verified *executor.RelayVerified `json:"verified,omitempty"`
}

// relayServerRow masks the user's node IP; the owner sees the full email the
// user agreed to record, but never the node address or its credentials.
func relayServerRowFor(entry executor.RelayEntry) relayServerRow {
	row := relayServerRow{ID: entry.ID, Role: entry.Role, Source: "legacy", MaskedIP: maskTargetIP(entry.Target), Port: entry.Port, RateMbps: entry.RateMbps, State: entry.State, Connections: entry.Connections, CreatedAt: entry.CreatedAt}
	if entry.Role == "route" {
		row.Source = "route"
	}
	if record := entry.Record; record != nil {
		row.Source, row.CodeTail, row.User, row.Remark, row.Recorded, row.Verified = record.Source, record.CodeTail, record.User, record.Remark, true, record.Verified
		if record.Target != "" {
			row.MaskedIP = maskTargetIP(record.Target)
		}
	}
	return row
}

func (t *TaskService) relayServerResult(w http.ResponseWriter, r *http.Request) {
	u, err := t.app.User(r)
	if err != nil {
		Fail(w, 401, "请先登录")
		return
	}
	t.mu.Lock()
	result, ok := t.relayResults[r.PathValue("id")]
	t.mu.Unlock()
	if !ok || result.UserID != u.ID || time.Now().After(result.Expires) {
		Fail(w, 404, "结果已过期，请重新读取")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if result.Kind == "relay-manage" {
		WriteJSON(w, 200, map[string]any{"managed": result.Managed})
		return
	}
	rows := make([]relayServerRow, 0, len(result.Inventory))
	for _, entry := range result.Inventory {
		rows = append(rows, relayServerRowFor(entry))
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].CreatedAt > rows[j].CreatedAt })
	WriteJSON(w, 200, map[string]any{"os": result.OS, "relays": rows})
}

// validRelayServerResult accepts the new result fields only for their kinds.
func validRelayServerResult(request executor.Request, out executor.Result) error {
	kind := request.Kind
	if (out.OS != "" && kind != "relay-check" && kind != "relay-inventory") || (len(out.Inventory) > 0 && kind != "relay-inventory") || (len(out.Managed) > 0 && kind != "relay-manage") || (out.FailedRole != "" && kind != "relay-code") {
		return errors.New("该任务不接受中转服务器结果")
	}
	if out.FailedRole != "" && out.FailedRole != "relay" && out.FailedRole != "front" && !verifyFailedRole(request, out.FailedRole) {
		return errors.New("失败位置无效")
	}
	if kind == "relay-manage" && out.State == "succeeded" {
		if request.Manage == nil || len(out.Managed) != len(request.Manage.Items) {
			return errors.New("中转管理结果与请求不一致")
		}
		for i, item := range out.Managed {
			if item.ID != request.Manage.Items[i] {
				return errors.New("中转管理结果与请求不一致")
			}
		}
	}
	if kind == "relay-check" && out.State == "succeeded" && out.OS == "" {
		return errors.New("检查结果缺少系统版本")
	}
	return nil
}

// publicTask hides the owner's SSH endpoint on a relay-code user's task.
func publicTask(job Task) Task {
	if job.Kind == "relay-code" {
		job.Host = maskIP(job.Host)
		job.SSHPort, job.SSHFingerprint = 0, ""
	}
	return job
}

func publicRelayCodeSettings(v RelayCodeSettings) map[string]any {
	return map[string]any{"enabled": v.Enabled, "manageWithSecret": v.ManageWithSecret, "maxTtlHours": v.MaxTTLHours, "maxUses": v.MaxUses, "maxActive": v.MaxActive, "defaultRateMbps": v.DefaultRateMbps, "rateMinMbps": v.RateMinMbps, "rateMaxMbps": v.RateMaxMbps, "redeemPerAccount": v.RedeemPerAccount}
}

func (t *TaskService) notifyRelayCodeOwner(notice relayCodeNotice) {
	var config SMTPConfig
	email := ""
	send := false
	_ = t.app.Store.View(func(s *State) error {
		owner := s.Users[notice.OwnerID]
		if owner == nil || !relayCodeSettings(s).NotifyOwner || !smtpReady(s) {
			return nil
		}
		var err error
		config, err = getSMTP(s)
		email, send = owner.Email, err == nil
		return nil
	})
	if !send {
		return
	}
	t.app.queuePasswordMail(func(ctx context.Context) {
		_ = t.app.sendAccountEmail(ctx, config, notice.OwnerID, email, "relay_code_used", EmailData{EventAtLabel: emailTimeLabel(notice.At)})
	})
}
