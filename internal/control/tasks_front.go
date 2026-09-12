package control

import (
	"context"
	"errors"
	"time"

	"github.com/mozziexwz/node/internal/executor"
)

// ProvisionFront is an internal paid-route adapter, not a browser-callable action.
// The caller must check ownership and entitlement and revoke station rules on error.
func (t *TaskService) ProvisionFront(ctx context.Context, userID string, ssh executor.SSH, targetHost string, targetPort int) (executor.Hop, error) {
	var zero executor.Hop
	if err := executor.ValidateSSH(ssh); err != nil {
		return zero, err
	}
	if targetPort < 1 || targetPort > 65535 {
		return zero, errors.New("站内入口端口无效")
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	now := time.Now()
	result := make(chan executor.Result, 1)
	job := Task{ID: ID(), UserID: userID, Kind: "front", Host: ssh.Host, State: "queued", Phase: "paid_front", Message: "等待配置此线路的客户前置机", CreatedAt: now.UnixMilli(), UpdatedAt: now.UnixMilli()}
	t.mu.Lock()
	err := t.app.Store.Update(func(s *State) error {
		if maintenance, _ := s.Settings["maintenance"].(bool); maintenance {
			return errors.New("系统维护中，暂停创建新的前置机任务")
		}
		u := s.Users[userID]
		if u == nil || u.Status != "active" || u.ExpiresAt <= now.UnixMilli() {
			return errors.New("套餐权益已失效")
		}
		if !t.confirmedProbe(userID, ssh) {
			return errors.New("请先检查并确认前置机的真实 SSH 指纹")
		}
		if t.targetBusy(ssh.Host) {
			return errors.New("该前置机已有未结束任务")
		}
		if !t.hasExecutor(s) {
			return errors.New("没有在线 executor 执行机")
		}
		return SaveDoc(s, "tasks", job.ID, job)
	})
	if err == nil {
		t.envelopes[job.ID] = &taskEnvelope{Job: executor.Job{ID: job.ID, Lease: ID(), Request: executor.Request{Kind: "front", SSH: ssh, ForwardTarget: &executor.Target{Host: targetHost, Port: targetPort}}, Deadline: now.Add(4 * time.Minute).UnixMilli()}, UserID: userID, QueuedAt: now, Result: result}
	}
	t.mu.Unlock()
	if err != nil {
		return zero, err
	}
	select {
	case out := <-result:
		if out.State != "succeeded" || len(out.Hops) != 1 {
			return zero, errors.New("前置机安装失败；请检查其 SSH、固定 GOST 资源和站内入口可达性")
		}
		h := out.Hops[0]
		if h.FromHost != ssh.Host || h.ToHost != targetHost || h.ToPort != targetPort || h.FromPort < 1 || h.FromPort > 65535 {
			return zero, errors.New("前置机结果与此线路不匹配")
		}
		return h, nil
	case <-ctx.Done():
		return zero, errors.New("前置机任务超时，站内规则应撤销；请核实自备服务器状态后重试")
	}
}
