package control

import (
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/mozziexwz/node/internal/relayruntime"
)

const targetProbeWait = 20 * time.Second

type relayDiagnosticJob struct {
	id, userID, ruleID, agentID, target string
	expires                             time.Time
	result                              chan relayruntime.TargetProbeResult
	completed                           bool
}

// Diagnostics are deliberately ephemeral. They are never forwarding intents,
// and loss of a panel process can only make a pending UI request time out.
type relayDiagnosticBroker struct {
	mu   sync.Mutex
	jobs map[string]*relayDiagnosticJob
}

func (b *relayDiagnosticBroker) begin(userID, ruleID, agentID, target string) (*relayDiagnosticJob, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	if b.jobs == nil {
		b.jobs = make(map[string]*relayDiagnosticJob)
	}
	perAgent := 0
	for id, job := range b.jobs {
		if now.After(job.expires) {
			delete(b.jobs, id)
			continue
		}
		if job.userID == userID {
			return nil, errors.New("已有诊断正在进行，请等待结果")
		}
		if job.agentID == agentID && !job.completed {
			perAgent++
		}
	}
	if perAgent >= relayruntime.MaxTargetProbes {
		return nil, errors.New("节点诊断繁忙，请稍后重试")
	}
	job := &relayDiagnosticJob{id: commerceID(), userID: userID, ruleID: ruleID, agentID: agentID, target: target, expires: now.Add(targetProbeWait), result: make(chan relayruntime.TargetProbeResult, 1)}
	b.jobs[job.id] = job
	return job, nil
}

func (b *relayDiagnosticBroker) end(id string) {
	b.mu.Lock()
	delete(b.jobs, id)
	b.mu.Unlock()
}

// exchange runs only after the normal v2 bearer authentication and state
// transaction have succeeded. A report is accepted solely for the exact
// challenge and authenticated agent; stale or unsolicited reports are ignored.
func (b *relayDiagnosticBroker) exchange(agentID string, reports []relayruntime.TargetProbeResult, out *relayruntime.V2SyncResponse) {
	if out.Status != "ready" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	for id, job := range b.jobs {
		if now.After(job.expires) {
			delete(b.jobs, id)
		}
	}
	for _, report := range reports {
		job := b.jobs[report.ID]
		if job == nil || job.agentID != agentID || job.ruleID != report.RuleID {
			continue
		}
		alreadyAcked := false
		for _, ack := range out.TargetProbeAcks {
			if ack == report.ID {
				alreadyAcked = true
				break
			}
		}
		if !alreadyAcked && len(out.TargetProbeAcks) < relayruntime.MaxTargetProbes {
			out.TargetProbeAcks = append(out.TargetProbeAcks, report.ID)
		}
		if job.completed {
			continue
		}
		if report.Status != "success" && report.Status != "failed" && report.Status != "unavailable" || report.LatencyMS < 0 || report.LatencyMS > 3000 {
			report.Status, report.LatencyMS = "unavailable", 0
		}
		job.completed = true
		job.result <- report
	}
	ids := make([]string, 0, len(b.jobs))
	for id, job := range b.jobs {
		if job.agentID == agentID && !job.completed {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		if len(out.TargetProbes) >= relayruntime.MaxTargetProbes {
			break
		}
		job := b.jobs[id]
		out.TargetProbes = append(out.TargetProbes, relayruntime.TargetProbeRequest{ID: job.id, RuleID: job.ruleID, Target: job.target, ExpiresAt: job.expires.UnixMilli()})
	}
}

func relayTargetProbeCapable(agent RelayAgent, now int64) bool {
	if !agent.Enabled || agent.ProtocolVersion != relayruntime.ProtocolV2 || agent.ReconcileState == "recovery_required" || agent.LastSeen <= now-relayLeaseMS {
		return false
	}
	for _, capability := range agent.Capabilities {
		if capability == relayruntime.TargetProbeCapability {
			return true
		}
	}
	return false
}
