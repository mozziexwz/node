package control

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/netip"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mozziexwz/node/internal/executor"
)

// Relay codes let a server owner share one relay server without handing over
// SSH. The website keeps that SSH sealed for a bounded number of uses and
// hours, and deletes it on expiry, exhaustion, revocation or suspension.
// Relays already built from a code keep running; they no longer need the SSH.
const (
	relayCodesCollection = "relay_codes"
	// relayCodeSecretsCollection is excluded from every backup and export.
	relayCodeSecretsCollection = "relay_code_secrets"
	relayCodeUsesCollection    = "relay_code_uses"
	relayCodeTasksCollection   = "relay_code_tasks"
	relayCodeFlagsCollection   = "relay_code_user_flags"
	relayCodeAuditCollection   = "relay_code_audit"
)

type RelayCode struct {
	ID              string `json:"id"`
	OwnerID         string `json:"ownerId"`
	Digest          string `json:"digest"`
	SealedCode      string `json:"sealedCode"`
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
	TTLHours        int    `json:"ttlHours"`
	CreatedAt       int64  `json:"createdAt"`
	ExpiresAt       int64  `json:"expiresAt"`
	State           string `json:"state"`
	SecretDeletedAt int64  `json:"secretDeletedAt,omitempty"`
	DeleteReason    string `json:"deleteReason,omitempty"`
	DeletedBy       string `json:"deletedBy,omitempty"`
	Note            string `json:"note"`
	CheckTaskID     string `json:"checkTaskId,omitempty"`
}

type relayCodeSecret struct {
	CodeID    string `json:"codeId"`
	SealedSSH string `json:"sealedSSH"`
	ExpiresAt int64  `json:"expiresAt"`
}

// RelayCodeUse records one attempt to build a relay from a code. Owner and
// server details are copied in masked form so the user's list survives the
// code's own pruning.
type RelayCodeUse struct {
	ID          string `json:"id"`
	CodeID      string `json:"codeId"`
	CodeTail    string `json:"codeTail"`
	OwnerID     string `json:"ownerId"`
	OwnerMasked string `json:"ownerMasked"`
	ServerHost  string `json:"serverHost"`
	UserID      string `json:"userId"`
	UserEmail   string `json:"userEmail"`
	TargetHost  string `json:"targetHost"`
	TargetPort  int    `json:"targetPort"`
	ListenPort  int    `json:"listenPort,omitempty"`
	ViaFront    bool   `json:"viaFront"`
	FrontHost   string `json:"frontHost,omitempty"`
	EntryPort   int    `json:"entryPort,omitempty"`
	RateMbps    int    `json:"rateMbps"`
	TaskID      string `json:"taskId"`
	Remark      string `json:"remark,omitempty"`
	Result      string `json:"result"`
	FailReason  string `json:"failReason,omitempty"`
	CreatedAt   int64  `json:"createdAt"`
	UpdatedAt   int64  `json:"updatedAt"`
	// Verify holds the target check of this use; never node credentials.
	Verify     *executor.VerifyReport `json:"verify,omitempty"`
	VerifiedAt int64                  `json:"verifiedAt,omitempty"`
}

// relayCodeTaskLink ties a server check or a use task to its code so every
// way a task can end (result, expiry, restart, revocation) settles it once.
type relayCodeTaskLink struct {
	TaskID    string `json:"taskId"`
	CodeID    string `json:"codeId"`
	OwnerID   string `json:"ownerId"`
	Purpose   string `json:"purpose"`
	Settled   bool   `json:"settled"`
	Outcome   string `json:"outcome,omitempty"`
	CreatedAt int64  `json:"createdAt"`
}

type relayCodeUserFlag struct {
	UserID   string `json:"userId"`
	Disabled bool   `json:"disabled"`
	Reason   string `json:"reason,omitempty"`
	At       int64  `json:"at"`
	By       string `json:"by"`
}

// relayCodeEvent never contains SSH material, only what happened and why.
type relayCodeEvent struct {
	ID        string `json:"id"`
	CreatedAt int64  `json:"createdAt"`
	Kind      string `json:"kind"`
	OwnerID   string `json:"ownerId,omitempty"`
	CodeID    string `json:"codeId,omitempty"`
	CodeTail  string `json:"codeTail,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Actor     string `json:"actor"`
	Host      string `json:"host,omitempty"`
	Action    string `json:"action,omitempty"`
	Count     int    `json:"count,omitempty"`
}

type pendingCodeSecret struct {
	CodeID  string
	Sealed  string
	Expires time.Time
}

type relayCodeNotice struct {
	OwnerID string
	At      int64
}

type RelayCodeSettings struct {
	Enabled          bool `json:"enabled"`
	ManageWithSecret bool `json:"manageWithSecret"`
	NotifyOwner      bool `json:"notifyOwner"`
	MaxTTLHours      int  `json:"maxTtlHours"`
	MaxUses          int  `json:"maxUses"`
	MaxActive        int  `json:"maxActive"`
	DefaultRateMbps  int  `json:"defaultRateMbps"`
	RateMinMbps      int  `json:"rateMinMbps"`
	RateMaxMbps      int  `json:"rateMaxMbps"`
	RedeemPerAccount int  `json:"redeemPerAccount"`
	RedeemPerIP      int  `json:"redeemPerIp"`
	ManageMinutes    int  `json:"manageMinutes"`
	ManageCount      int  `json:"manageCount"`
}

const (
	relayCodeMaxTTLHours = 72
	relayCodeMaxUses     = 10
)

func defaultRelayCodeSettings() RelayCodeSettings {
	return RelayCodeSettings{Enabled: true, ManageWithSecret: true, MaxTTLHours: relayCodeMaxTTLHours, MaxUses: relayCodeMaxUses, MaxActive: 5, DefaultRateMbps: executor.DefaultRelayRateMbps, RateMinMbps: 1, RateMaxMbps: executor.MaxRelayRateMbps, RedeemPerAccount: 10, RedeemPerIP: 30, ManageMinutes: 15, ManageCount: 20}
}

// relayCodeSettings never returns a value outside the documented bounds,
// even if the stored document predates a field or was edited by hand.
func relayCodeSettings(s *State) RelayCodeSettings {
	v := defaultRelayCodeSettings()
	if raw, err := json.Marshal(s.Settings["relayCodes"]); err == nil && string(raw) != "null" {
		_ = json.Unmarshal(raw, &v)
	}
	if validateRelayCodeSettings(v) != nil {
		d := defaultRelayCodeSettings()
		d.Enabled, d.ManageWithSecret, d.NotifyOwner = v.Enabled, v.ManageWithSecret, v.NotifyOwner
		return d
	}
	return v
}

func validateRelayCodeSettings(v RelayCodeSettings) error {
	switch {
	case v.MaxTTLHours < 1 || v.MaxTTLHours > relayCodeMaxTTLHours:
		return errors.New("有效期上限需为 1–72 小时")
	case v.MaxUses < 1 || v.MaxUses > relayCodeMaxUses:
		return errors.New("每个中转码最多次数需为 1–10 次")
	case v.MaxActive < 1 || v.MaxActive > 20:
		return errors.New("每人同时有效的中转码需为 1–20 个")
	case v.RateMinMbps < 1 || v.RateMaxMbps > executor.MaxRelayRateMbps || v.RateMinMbps > v.RateMaxMbps:
		return errors.New("可选带宽范围需在 1–10 Mbps 内")
	case v.DefaultRateMbps < v.RateMinMbps || v.DefaultRateMbps > v.RateMaxMbps:
		return errors.New("默认带宽需在可选范围内")
	case v.RedeemPerAccount < 1 || v.RedeemPerAccount > 100 || v.RedeemPerIP < 1 || v.RedeemPerIP > 500:
		return errors.New("使用尝试限流需为每账号 1–100 次、每 IP 1–500 次")
	case v.ManageMinutes < 1 || v.ManageMinutes > 1440 || v.ManageCount < 1 || v.ManageCount > 100:
		return errors.New("中转服务器管理限额需为 1–1440 分钟、1–100 次")
	}
	return nil
}

func relayCodeGate(s *State, u *User) error {
	if err := taskGate(s, u, "relay"); err != nil {
		return err
	}
	if !relayCodeSettings(s).Enabled {
		return errors.New("中转码功能已被管理员关闭")
	}
	if flag, ok := LoadDoc[relayCodeUserFlag](s, relayCodeFlagsCollection, u.ID); ok && flag.Disabled {
		return errors.New("你的中转码功能已被管理员停用")
	}
	return nil
}

const relayCodeAlphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"

// newRelayCode returns 12 symbols (60 bits) from an alphabet without 0/O/1/I.
func newRelayCode() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	for i := range b {
		b[i] = relayCodeAlphabet[b[i]&31]
	}
	return string(b)
}

func formatRelayCode(raw string) string {
	if len(raw) != 12 {
		return ""
	}
	return "MSB-" + raw[:4] + "-" + raw[4:8] + "-" + raw[8:]
}

func maskedRelayCode(tail string) string { return "MSB-••••-••••-" + tail }

// normalizeRelayCode accepts any case, spaces and hyphens, with or without the prefix.
func normalizeRelayCode(input string) (string, bool) {
	value := strings.Map(func(r rune) rune {
		if r == '-' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, strings.ToUpper(strings.TrimSpace(input)))
	if len(value) == 15 && strings.HasPrefix(value, "MSB") {
		value = value[3:]
	}
	if len(value) != 12 {
		return "", false
	}
	for _, r := range value {
		if !strings.ContainsRune(relayCodeAlphabet, r) {
			return "", false
		}
	}
	return value, true
}

func (t *TaskService) relayCodeDigest(normalized string) string {
	mac := hmac.New(sha256.New, t.digestKey)
	_, _ = mac.Write([]byte("relay-code:" + normalized))
	return hex.EncodeToString(mac.Sum(nil))
}

// maskIP keeps the first and last groups, e.g. 203.*.*.24.
func maskIP(host string) string {
	addr, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		return "***"
	}
	addr = addr.Unmap()
	if addr.Is4() {
		parts := strings.Split(addr.String(), ".")
		return parts[0] + ".*.*." + parts[3]
	}
	parts := strings.Split(addr.StringExpanded(), ":")
	return parts[0] + ":*:*:" + parts[len(parts)-1]
}

// maskTargetIP masks the host part of host:port.
func maskTargetIP(target string) string {
	if addr, err := netip.ParseAddrPort(target); err == nil {
		return maskIP(addr.Addr().String())
	}
	if i := strings.LastIndex(target, ":"); i > 0 {
		return maskIP(target[:i])
	}
	return maskIP(target)
}

// maskOwnerEmail shows the first four characters, e.g. 5566****@qq.com.
func maskOwnerEmail(email string) string {
	local, domain, ok := strings.Cut(email, "@")
	if !ok || local == "" {
		return "****"
	}
	runes := []rune(local)
	keep := 4
	if len(runes) <= 4 {
		keep = 1
	}
	return string(runes[:keep]) + "****@" + domain
}

func relayCodeAuditEvent(s *State, event relayCodeEvent) {
	event.ID, event.CreatedAt = ID(), time.Now().UnixMilli()
	_ = SaveDoc(s, relayCodeAuditCollection, event.ID, event)
	pruneAudit(s, relayCodeAuditCollection)
}

// deleteRelayCodeSecret is the only place stored SSH is removed. The first
// removal is recorded with its reason; the code document keeps no SSH.
func deleteRelayCodeSecret(s *State, code *RelayCode, reason, actor string, now time.Time) {
	DeleteDoc(s, relayCodeSecretsCollection, code.ID)
	if code.SecretDeletedAt != 0 {
		return
	}
	code.SecretDeletedAt, code.DeleteReason, code.DeletedBy = now.UnixMilli(), reason, actor
	relayCodeAuditEvent(s, relayCodeEvent{Kind: "secret_deleted", OwnerID: code.OwnerID, CodeID: code.ID, CodeTail: code.Tail, Reason: reason, Actor: actor})
}

// endRelayCode moves a usable code to a terminal state and deletes its SSH.
func endRelayCode(s *State, code *RelayCode, reason, actor string, now time.Time) error {
	if code.State != "active" && code.State != "checking" {
		return nil
	}
	switch reason {
	case "expired":
		code.State = "expired"
	case "exhausted":
		code.State = "exhausted"
	default:
		code.State = "revoked"
	}
	deleteRelayCodeSecret(s, code, reason, actor, now)
	return SaveDoc(s, relayCodesCollection, code.ID, *code)
}

// endOwnerRelayCodes ends every usable code of one owner, or of all owners.
func endOwnerRelayCodes(s *State, ownerID, reason, actor string, now time.Time) (int, error) {
	count := 0
	for _, code := range ListDocs[RelayCode](s, relayCodesCollection) {
		if (ownerID != "" && code.OwnerID != ownerID) || (code.State != "active" && code.State != "checking") {
			continue
		}
		if err := endRelayCode(s, &code, reason, actor, now); err != nil {
			return count, err
		}
		count++
	}
	// Nothing may outlive its code, including a secret left by an interrupted write.
	if ownerID == "" {
		delete(s.Docs, relayCodeSecretsCollection)
	}
	return count, nil
}

func activateRelayCode(s *State, taskID string, pending pendingCodeSecret, os string, now time.Time) error {
	link, ok := LoadDoc[relayCodeTaskLink](s, relayCodeTasksCollection, taskID)
	if !ok || link.Purpose != "check" || link.Settled {
		return nil
	}
	code, ok := LoadDoc[RelayCode](s, relayCodesCollection, link.CodeID)
	if !ok || code.State != "checking" || pending.Sealed == "" || pending.CodeID != code.ID {
		return nil
	}
	owner := s.Users[code.OwnerID]
	if owner == nil || relayCodeGate(s, owner) != nil {
		return nil
	}
	// The time limit starts when the code becomes usable.
	code.State, code.OS = "active", os
	code.CreatedAt = now.UnixMilli()
	code.ExpiresAt = now.Add(time.Duration(code.TTLHours) * time.Hour).UnixMilli()
	if err := SaveDoc(s, relayCodeSecretsCollection, code.ID, relayCodeSecret{CodeID: code.ID, SealedSSH: pending.Sealed, ExpiresAt: code.ExpiresAt}); err != nil {
		return err
	}
	if err := SaveDoc(s, relayCodesCollection, code.ID, code); err != nil {
		return err
	}
	link.Settled, link.Outcome = true, "succeeded"
	return SaveDoc(s, relayCodeTasksCollection, taskID, link)
}

func relayTaskEnded(state string) bool { return state != "queued" && state != "running" }

// settleRelayCodeTask runs in the same transaction that ends a task. A failed
// check never stores SSH; a failed or interrupted use returns its reserved use.
func settleRelayCodeTask(s *State, taskID string, out *executor.Result, now time.Time) (*relayCodeNotice, error) {
	link, ok := LoadDoc[relayCodeTaskLink](s, relayCodeTasksCollection, taskID)
	if !ok {
		return nil, nil
	}
	task, taskFound := LoadDoc[Task](s, "tasks", taskID)
	if taskFound && !relayTaskEnded(task.State) {
		return nil, nil
	}
	succeeded := taskFound && task.State == "succeeded"
	// An executor may still report success after a restart marked the task
	// interrupted; that use is then counted after all, exactly once.
	lateSuccess := link.Settled && link.Purpose == "use" && link.Outcome == "failed" && succeeded
	if link.Settled && !lateSuccess {
		return nil, nil
	}
	var notice *relayCodeNotice
	switch link.Purpose {
	case "check":
		if code, found := LoadDoc[RelayCode](s, relayCodesCollection, link.CodeID); found && code.State == "checking" {
			DeleteDoc(s, relayCodesCollection, code.ID)
		}
		DeleteDoc(s, relayCodeSecretsCollection, link.CodeID)
		link.Outcome = "failed"
	case "use":
		use, useFound := LoadDoc[RelayCodeUse](s, relayCodeUsesCollection, taskID)
		if out != nil && out.Verify != nil {
			report := *out.Verify
			use.Verify, use.VerifiedAt = &report, now.UnixMilli()
		}
		code, codeFound := LoadDoc[RelayCode](s, relayCodesCollection, link.CodeID)
		if codeFound && !lateSuccess && code.Reserved > 0 {
			code.Reserved--
		}
		if succeeded {
			link.Outcome = "succeeded"
			use.Result, use.FailReason = "succeeded", ""
			if len(task.Hops) > 0 {
				use.ListenPort, use.EntryPort = task.Hops[0].FromPort, task.Hops[0].FromPort
			}
			if len(task.Hops) > 1 {
				use.EntryPort = task.Hops[1].FromPort
			}
			if codeFound {
				code.UsedCount++
				if code.UsedCount >= code.MaxUses && code.State == "active" {
					if err := endRelayCode(s, &code, "exhausted", "system", now); err != nil {
						return nil, err
					}
				}
				notice = &relayCodeNotice{OwnerID: code.OwnerID, At: now.UnixMilli()}
			}
		} else {
			link.Outcome = "failed"
			use.Result = "failed"
			use.FailReason = relayUseFailure(task, taskFound)
			// An interrupted task keeps its review instructions unchanged.
			if taskFound && task.State == "failed" {
				failedRole := ""
				if out != nil {
					failedRole = out.FailedRole
				}
				task.NextStep = relayUseNextStep(task, failedRole)
				if err := SaveDoc(s, "tasks", task.ID, task); err != nil {
					return nil, err
				}
			}
		}
		use.UpdatedAt = now.UnixMilli()
		if useFound {
			if err := SaveDoc(s, relayCodeUsesCollection, use.ID, use); err != nil {
				return nil, err
			}
		}
		if codeFound && code.State != "exhausted" {
			if err := SaveDoc(s, relayCodesCollection, code.ID, code); err != nil {
				return nil, err
			}
		}
	}
	link.Settled = true
	return notice, SaveDoc(s, relayCodeTasksCollection, taskID, link)
}

func relayUseFailure(task Task, found bool) string {
	if !found {
		return "任务记录缺失"
	}
	if task.State == "interrupted" || task.State == "unknown" {
		return "任务中断"
	}
	if task.ErrorCode != "" {
		return executor.PublicDiagnostic(task.ErrorCode, task.Phase).Message
	}
	return "建立失败"
}

// The user cannot inspect the owner's server, so point to the right person.
func relayUseNextStep(task Task, failedRole string) string {
	if step := verifyNextStep(task.ErrorCode); step != "" {
		return step
	}
	switch {
	case failedRole == "front":
		return "自备前置机连接或安装失败；中转服务器上没有保留这次的中转。请检查前置机后重试，本次没有计次。"
	case strings.HasPrefix(task.ErrorCode, "ssh_"):
		return "中转服务器登录失败或身份变化：请联系拥有者。本次没有计次。"
	case task.ErrorCode == "invalid_config":
		return "节点配置无效：请重新导出节点配置。本次没有计次。"
	case task.ErrorCode == "target_unreachable":
		return "中转服务器连不上你的节点：请检查节点端口和防火墙。本次没有计次。"
	}
	if task.NextStep != "" {
		return task.NextStep + " 本次没有计次。"
	}
	return "建立失败，本次没有计次。任务记录里有失败阶段。"
}

// sweepRelayCodes is the automatic deletion check, run every minute and at
// start. It ends codes whose time is up or whose owner may no longer share,
// settles tasks that ended without a result, and bounds history.
func sweepRelayCodes(s *State, now time.Time) error {
	for id := range s.Docs[relayCodeTasksCollection] {
		if _, err := settleRelayCodeTask(s, id, nil, now); err != nil {
			return err
		}
	}
	settings := relayCodeSettings(s)
	nowMs := now.UnixMilli()
	for _, code := range ListDocs[RelayCode](s, relayCodesCollection) {
		switch code.State {
		case "active":
			owner := s.Users[code.OwnerID]
			_, hasSecret := s.Docs[relayCodeSecretsCollection][code.ID]
			flag, flagged := LoadDoc[relayCodeUserFlag](s, relayCodeFlagsCollection, code.OwnerID)
			reason := ""
			switch {
			case !hasSecret:
				// Backups never contain SSH, so a restored code cannot be used.
				reason = "restore"
			case owner == nil || owner.Status != "active":
				reason = "owner_disabled"
			case flagged && flag.Disabled:
				reason = "admin_disabled"
			case !settings.Enabled:
				reason = "feature_disabled"
			case nowMs >= code.ExpiresAt:
				reason = "expired"
			}
			if reason != "" {
				if err := endRelayCode(s, &code, reason, "system", now); err != nil {
					return err
				}
			}
		case "checking":
			// A check runs for minutes at most; its task settles it normally.
			if code.CreatedAt < nowMs-int64(30*time.Minute/time.Millisecond) {
				DeleteDoc(s, relayCodesCollection, code.ID)
				DeleteDoc(s, relayCodeSecretsCollection, code.ID)
			}
		default:
			if code.SecretDeletedAt > 0 && code.SecretDeletedAt < nowMs-int64(90*24*time.Hour/time.Millisecond) {
				DeleteDoc(s, relayCodesCollection, code.ID)
			}
		}
	}
	// A secret without a usable code is never kept.
	for id := range s.Docs[relayCodeSecretsCollection] {
		code, ok := LoadDoc[RelayCode](s, relayCodesCollection, id)
		if !ok || (code.State != "active" && code.State != "checking") {
			DeleteDoc(s, relayCodeSecretsCollection, id)
		}
	}
	for _, use := range ListDocs[RelayCodeUse](s, relayCodeUsesCollection) {
		age := nowMs - use.CreatedAt
		if (use.Result == "failed" && age > int64(30*24*time.Hour/time.Millisecond)) || age > int64(365*24*time.Hour/time.Millisecond) {
			DeleteDoc(s, relayCodeUsesCollection, use.ID)
		}
	}
	for _, link := range ListDocs[relayCodeTaskLink](s, relayCodeTasksCollection) {
		if link.Settled && link.CreatedAt < nowMs-int64(7*24*time.Hour/time.Millisecond) {
			DeleteDoc(s, relayCodeTasksCollection, link.TaskID)
		}
	}
	pruneAudit(s, relayCodeAuditCollection)
	return nil
}

func (t *TaskService) sweepRelayCodes() {
	if err := t.app.Store.Update(func(s *State) error { return sweepRelayCodes(s, time.Now()) }); err != nil {
		// A whole-site backup pause defers deletion to the next minute.
		log.Printf("relay code deletion check deferred: %v", err)
	}
}

func (t *TaskService) openRelayCodeSSH(s *State, code RelayCode) (executor.SSH, error) {
	var ssh executor.SSH
	secret, ok := LoadDoc[relayCodeSecret](s, relayCodeSecretsCollection, code.ID)
	if !ok || code.State != "active" {
		return ssh, errors.New("这个中转码暂存的 SSH 已删除")
	}
	plain, err := t.app.Open(secret.SealedSSH)
	if err != nil {
		return ssh, errors.New("暂存的 SSH 无法解密")
	}
	defer clear(plain)
	if err = json.Unmarshal(plain, &ssh); err != nil {
		return ssh, errors.New("暂存的 SSH 无法解密")
	}
	// The fingerprint pinned at generation is authoritative.
	ssh.Fingerprint, ssh.TrustMode, ssh.ReplaceFingerprint = code.Fingerprint, "", ""
	return ssh, nil
}

func (t *TaskService) relayCodeString(code RelayCode) string {
	plain, err := t.app.Open(code.SealedCode)
	if err != nil {
		return ""
	}
	return formatRelayCode(string(plain))
}

func validRelayCodeNote(note string) error {
	if utf8.RuneCountInString(note) > 40 || strings.ContainsAny(note, "\x00\r\n") {
		return errors.New("备注最多 40 个字符，不能包含换行")
	}
	return nil
}

func ownerRelayCodes(s *State, ownerID string) []RelayCode {
	rows := []RelayCode{}
	for _, code := range ListDocs[RelayCode](s, relayCodesCollection) {
		if code.OwnerID == ownerID {
			rows = append(rows, code)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].CreatedAt > rows[j].CreatedAt })
	return rows
}

func usableRelayCode(code RelayCode) bool { return code.State == "active" || code.State == "checking" }
