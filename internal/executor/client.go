package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/mozziexwz/node/internal/buildinfo"
)

// Sent on every authenticated poll/heartbeat, not inferred from a release
// number. A v0.3.x executor must never receive a new relay/front task after
// the control plane starts requiring the mandatory SOCKS guard.
const FreeRelayGuardCapability = "free_relay_socks_guard"
const ScopedCleanupCapability = "scoped_relay_cleanup"
const executorCapabilitiesHeader = "X-MSBOOST-Executor-Capabilities"

// Run polls authenticated envelopes. It never persists SSH credentials or logs
// task bodies. A lost completion ACK is safe to retry; a job is never re-executed.
func Run(ctx context.Context, serverURL, token string, engine *Engine) error {
	ctx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	u, err := url.Parse(serverURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("invalid server URL")
	}
	if u.Scheme != "https" {
		ip := net.ParseIP(u.Hostname())
		if u.Scheme != "http" || (u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback())) {
			return errors.New("executor requires HTTPS outside localhost")
		}
	}
	if len(token) < 32 {
		return errors.New("executor token missing or too short")
	}
	if engine == nil {
		engine = NewEngine()
	}
	client := &http.Client{Timeout: 35 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}
	base := strings.TrimRight(serverURL, "/")
	var pending atomic.Pointer[Result]
	go func() {
		for waitContext(ctx, 20*time.Second) {
			status := map[string]any{}
			if out := pending.Load(); out != nil {
				status = map[string]any{"taskId": out.ID, "lease": out.Lease, "waitingResult": true}
			}
			body, _ := json.Marshal(status)
			request, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/executor/heartbeat", bytes.NewReader(body))
			request.Header.Set("Authorization", "Bearer "+token)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set(executorCapabilitiesHeader, FreeRelayGuardCapability)
			request.Header.Set("X-MSBOOST-Scoped-Cleanup", "1")
			request.Header.Set("X-MSBOOST-Agent-Version", buildinfo.Version)
			response, err := client.Do(request)
			if err == nil {
				response.Body.Close()
			}
		}
	}()
	for ctx.Err() == nil {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/executor/next", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set(executorCapabilitiesHeader, FreeRelayGuardCapability)
		req.Header.Set("X-MSBOOST-Scoped-Cleanup", "1")
		req.Header.Set("X-MSBOOST-Agent-Version", buildinfo.Version)
		resp, err := client.Do(req)
		if err != nil {
			if !waitContext(ctx, 3*time.Second) {
				break
			}
			continue
		}
		if resp.StatusCode == http.StatusUnauthorized {
			resp.Body.Close()
			return errors.New("executor token rejected or disabled")
		}
		if resp.StatusCode != 200 {
			resp.Body.Close()
			if !waitContext(ctx, 3*time.Second) {
				break
			}
			continue
		}
		var job Job
		err = json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&job)
		resp.Body.Close()
		if err != nil {
			continue
		}
		deadline := time.UnixMilli(job.Deadline)
		if time.Until(deadline) > 45*time.Minute {
			deadline = time.Now().Add(45 * time.Minute)
		}
		jobCtx, cancel := context.WithDeadline(ctx, deadline)
		result := engine.Execute(jobCtx, job)
		cancel()
		job = RequestlessJob(job)
		body, _ := json.Marshal(result)
		pending.Store(&Result{ID: result.ID, Lease: result.Lease})
		// A one-item memory outbox blocks new work until its ACK. No SSH secrets
		// or result configuration is written to disk. Never replay execution.
		err = deliverResult(ctx, client, base, token, body, resultRetryPolicy{maxAge: 30 * time.Minute, now: time.Now, wait: waitContext})
		pending.Store(nil)
		if err != nil {
			return err
		}
	}
	return ctx.Err()
}

type resultRetryPolicy struct {
	maxAge time.Duration
	now    func() time.Time
	wait   func(context.Context, time.Duration) bool
}

func deliverResult(ctx context.Context, client *http.Client, base, token string, body []byte, p resultRetryPolicy) error {
	deadline := p.now().Add(p.maxAge)
	delay := time.Second
	for ctx.Err() == nil {
		remaining := deadline.Sub(p.now())
		if remaining <= 0 {
			break
		}
		requestCtx, cancel := context.WithTimeout(ctx, remaining)
		req, _ := http.NewRequestWithContext(requestCtx, http.MethodPost, base+"/api/executor/result", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		response, err := client.Do(req)
		cancel()
		if err == nil {
			status := response.StatusCode
			response.Body.Close()
			if status == http.StatusOK {
				return nil
			}
			if status == 401 || status == 403 || status == 409 || status == 400 || status == 413 {
				return errors.New("result delivery rejected; remote operation was not repeated; check the original task and VPS before releasing its hold")
			}
		}
		remaining = deadline.Sub(p.now())
		if delay > remaining {
			delay = remaining
		}
		if delay <= 0 || !p.wait(ctx, delay) {
			break
		}
		delay *= 2
		if delay > 30*time.Second {
			delay = 30 * time.Second
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return errors.New("result delivery expired after 30 minutes; remote operation was not repeated; check the task and VPS, and recover configuration from the VPS if needed")
}
func RequestlessJob(j Job) Job { j.Request = Request{}; j.Script = Asset{}; return j }
func waitContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
