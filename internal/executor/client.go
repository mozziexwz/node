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
	"time"
)

// Sent on every authenticated poll/heartbeat, not inferred from a release
// number. A v0.3.x executor must never receive a new relay/front task after
// the control plane starts requiring the mandatory SOCKS guard.
const FreeRelayGuardCapability = "free_relay_socks_guard"
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
	go func() {
		for waitContext(ctx, 20*time.Second) {
			request, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/executor/heartbeat", bytes.NewBufferString("{}"))
			request.Header.Set("Authorization", "Bearer "+token)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set(executorCapabilitiesHeader, FreeRelayGuardCapability)
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
			if resp.StatusCode != 204 {
				if !waitContext(ctx, 3*time.Second) {
					break
				}
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
		// Retry result delivery only. Server rejects replay after the first ACK.
		for attempt := 0; attempt < 5; attempt++ {
			req, _ = http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/executor/result", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", "application/json")
			resp, err = client.Do(req)
			if err == nil {
				status := resp.StatusCode
				resp.Body.Close()
				if status == 200 || status == 409 {
					break
				}
				if status == 401 {
					return errors.New("executor token rejected")
				}
			}
			if !waitContext(ctx, time.Duration(attempt+1)*time.Second) {
				break
			}
		}
	}
	return ctx.Err()
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
