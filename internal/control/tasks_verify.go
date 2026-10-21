package control

import (
	"context"
	"errors"
	"time"

	"github.com/mozziexwz/node/internal/executor"
)

// VerifyRoute is the internal adapter that checks a paid route's entry. It is
// not browser-callable; the scheduler supplies the route configuration, which
// lives only in this job's memory and the executor's.
func (t *TaskService) VerifyRoute(ctx context.Context, userID string, config []byte, options executor.VerifyOptions) (executor.VerifyReport, error) {
	var zero executor.VerifyReport
	info, err := executor.ParseClientConfig(config)
	if err != nil {
		return zero, err
	}
	request := executor.Request{Kind: "relay-verify", ClientConfig: append([]byte(nil), config...), Verify: &options}
	if err := executor.ValidateRequest(request); err != nil {
		return zero, err
	}
	now := time.Now()
	result := make(chan executor.Result, 1)
	job := Task{ID: ID(), UserID: userID, Kind: "relay-verify", Host: info.TargetHost, State: "queued", Phase: "queued", Message: "等待执行机校验中转目标", CreatedAt: now.UnixMilli(), UpdatedAt: now.UnixMilli()}
	envelope := &taskEnvelope{Job: executor.Job{ID: job.ID, Lease: ID(), Request: request, Deadline: now.Add(10 * time.Minute).UnixMilli()}, UserID: userID, QueuedAt: now, Result: result}
	err = t.app.Store.Update(func(s *State) error {
		if maintenance, _ := s.Settings["maintenance"].(bool); maintenance {
			return errors.New("系统维护中，暂停校验")
		}
		if !t.hasExecutorFor(s, request) {
			return t.executorUnavailable(s, request)
		}
		if err := SaveDoc(s, "tasks", job.ID, job); err != nil {
			return err
		}
		return saveTaskOperation(s, envelope)
	})
	if err != nil {
		clear(request.ClientConfig)
		return zero, err
	}
	t.mu.Lock()
	t.envelopes[job.ID] = envelope
	t.mu.Unlock()
	select {
	case out := <-result:
		if out.State != "succeeded" || out.Verify == nil {
			return zero, errors.New("校验任务未完成")
		}
		return *out.Verify, nil
	case <-ctx.Done():
		return zero, errors.New("校验任务超时")
	}
}
