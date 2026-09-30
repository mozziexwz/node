package control

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/netip"
	"time"

	"github.com/mozziexwz/node/internal/executor"
)

// Durable non-secret ownership survives a control-plane restart. It is not a
// runnable envelope: passwords, client config and script bytes never enter it.
type taskOperation struct {
	ID        string           `json:"id"`
	AgentID   string           `json:"agentId,omitempty"`
	LeaseHash string           `json:"leaseHash"`
	Hosts     []string         `json:"hosts"`
	Request   executor.Request `json:"request"`
	Status    string           `json:"status"`
	Receipt   string           `json:"receipt,omitempty"`
	Deadline  int64            `json:"deadline"`
	UpdatedAt int64            `json:"updatedAt"`
}

func taskHash(value []byte) string {
	h := sha256.Sum256(value)
	return hex.EncodeToString(h[:])
}

func taskHost(host string) string {
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.Unmap().String()
	}
	return host
}

func saveTaskOperation(s *State, e *taskEnvelope) error {
	r := e.Job.Request
	r.SSH.Password, r.ClientConfig, r.DD = "", nil, nil
	hosts := []string{taskHost(r.SSH.Host)}
	if r.Front != nil {
		front := *r.Front
		front.Password = ""
		r.Front = &front
		hosts = append(hosts, taskHost(front.Host))
	}
	return SaveDoc(s, "task_operations", e.Job.ID, taskOperation{ID: e.Job.ID, LeaseHash: taskHash([]byte(e.Job.Lease)), Hosts: hosts, Request: r, Status: "queued", Deadline: e.Job.Deadline, UpdatedAt: time.Now().UnixMilli()})
}

func operationBusy(o taskOperation) bool {
	return o.Request.Kind != "fingerprint" && o.Status != "resolved"
}

func executorPending(s *State, id string) []string {
	ids := []string{}
	for _, o := range ListDocs[taskOperation](s, "task_operations") {
		if o.AgentID == id && o.Status != "resolved" {
			ids = append(ids, o.ID)
		}
	}
	return ids
}

func finishExecutorDrain(s *State, id string) error {
	a, ok := LoadDoc[ExecutorRecord](s, "executors", id)
	if ok && a.Status == "draining" && len(executorPending(s, id)) == 0 {
		a.Status = "disabled"
		return SaveDoc(s, "executors", id, a)
	}
	return nil
}

// Reconciliation is an explicit human assertion, not remote cancellation.
// Never allow it while the original envelope is still executing normally.
func (t *TaskService) reconcile(w http.ResponseWriter, r *http.Request) {
	u, err := t.app.User(r)
	if err != nil {
		Fail(w, 401, "请先登录")
		return
	}
	var in struct {
		Confirm string `json:"confirm"`
	}
	if Decode(r, &in) != nil || in.Confirm != "CONFIRM_TASK_ENDED" {
		Fail(w, 400, "请先在 VPS 控制台确认原操作已经结束；此操作不会停止远程命令")
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	err = t.app.Store.Update(func(s *State) error {
		job, ok := LoadDoc[Task](s, "tasks", r.PathValue("id"))
		if !ok || (job.UserID != u.ID && u.Role != "admin") {
			return errors.New("任务不存在")
		}
		o, ok := LoadDoc[taskOperation](s, "task_operations", job.ID)
		if !ok || o.Status != "review" {
			return errors.New("只有需要核实的任务可以解除占用；运行中的任务不能强制放行")
		}
		o.Status, o.UpdatedAt = "resolved", time.Now().UnixMilli()
		job.NeedsReview = false
		job.Phase, job.Message, job.NextStep = "manually_reconciled", "已由用户确认原操作结束，服务器任务占用已解除；不代表部署或重装成功", "请核对 VPS 当前状态再发起新操作"
		job.UpdatedAt = o.UpdatedAt
		if err := SaveDoc(s, "tasks", job.ID, job); err != nil {
			return err
		}
		if err := SaveDoc(s, "task_operations", o.ID, o); err != nil {
			return err
		}
		if err := identityAudit(s, u.ID, "task.reconcile", job.ID, "用户确认原远程操作已经结束；并非停止命令或成功验收"); err != nil {
			return err
		}
		return finishExecutorDrain(s, o.AgentID)
	})
	if err != nil {
		Fail(w, 409, err.Error())
		return
	}
	// A revoked executor can leave a claimed in-memory envelope. Clear it
	// only after the explicit reconciliation transaction commits; otherwise
	// targetBusy's fallback would contradict the durable resolved state.
	delete(t.envelopes, r.PathValue("id"))
	WriteJSON(w, 200, map[string]bool{"ok": true})
}

func (t *TaskService) revokeExecutor(w http.ResponseWriter, r *http.Request) {
	actor, err := t.app.Admin(r)
	if err != nil {
		Fail(w, 403, "需要管理员权限")
		return
	}
	var in struct {
		Confirm string `json:"confirm"`
	}
	if Decode(r, &in) != nil || in.Confirm != "REVOKE_EXECUTOR" {
		Fail(w, 400, "紧急撤销不会停止远端任务，并会使未回传结果失效；请明确确认")
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	err = t.app.Store.Update(func(s *State) error {
		a, ok := LoadDoc[ExecutorRecord](s, "executors", r.PathValue("id"))
		if !ok {
			return errors.New("执行机不存在")
		}
		a.Status, a.TokenHash = "disabled", ""
		for _, id := range executorPending(s, a.ID) {
			o, _ := LoadDoc[taskOperation](s, "task_operations", id)
			o.Status = "review"
			if err := SaveDoc(s, "task_operations", id, o); err != nil {
				return err
			}
			job, _ := LoadDoc[Task](s, "tasks", id)
			job.State, job.Phase, job.NeedsReview = "interrupted", "executor_revoked", true
			job.Message, job.NextStep = "执行机已紧急撤销；远程操作可能仍在运行", "请从 VPS 控制台确认原操作结束后解除占用"
			if err := SaveDoc(s, "tasks", id, job); err != nil {
				return err
			}
		}
		if err := SaveDoc(s, "executors", a.ID, a); err != nil {
			return err
		}
		return identityAudit(s, actor.ID, "executor.revoke", a.ID, "紧急撤销凭据；远程操作未被停止，未完成任务需要核实")
	})
	if err != nil {
		Fail(w, 409, err.Error())
		return
	}
	WriteJSON(w, 200, map[string]bool{"ok": true})
}
