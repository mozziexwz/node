package relayruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type v2Prepared struct {
	commandID string
	process   *preparedProcess
}
type v2RuntimeState struct {
	disk                 v2DiskState
	path                 string
	prepared             map[string]v2Prepared
	retryAt              map[string]time.Time
	controlStatus        string
	sequence             int64
	cacheOnly            bool
	token                string
	wake                 chan struct{}
	credentialGeneration int64
}

func v2RunningAction(action string) bool { return action == "upsert" || action == "resume" }
func validV2ID(value string) bool {
	if len(value) < 1 || len(value) > 200 {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_-.:", r)) {
			return false
		}
	}
	return true
}

func validateV2Command(command V2Command) error {
	if !validV2ID(command.CommandID) || !validV2ID(command.RuleID) || len(command.RuleID) < 16 || command.Generation < 1 || len(command.Reason) > 256 {
		return errors.New("invalid v2 command identity")
	}
	if v2RunningAction(command.Action) {
		if command.Rule == nil || command.Rule.ID != command.RuleID || command.Rule.Version < 1 || command.Rule.EntitlementVersion < 0 || (command.Rule.Billing && !validV2ID(command.BillingPeriodID)) {
			return errors.New("invalid v2 forwarding command")
		}
		hash, err := RuntimeHash(*command.Rule)
		if err != nil || hash != command.RuntimeHash {
			return errors.New("invalid v2 runtime hash or configuration")
		}
	} else if command.Action == "pause" || command.Action == "revoke" {
		if command.Rule != nil || command.BillingPeriodID != "" {
			return errors.New("stop command must not carry new configuration")
		}
	} else {
		return errors.New("unknown v2 command action")
	}
	return nil
}

func normalizedV2Command(command V2Command) V2Command {
	if command.Rule != nil {
		copy := *command.Rule
		copy.LeaseUntil = 0
		command.Rule = &copy
	}
	return command
}

func sameV2Command(left, right V2Command) bool {
	a, _ := json.Marshal(normalizedV2Command(left))
	b, _ := json.Marshal(normalizedV2Command(right))
	return bytes.Equal(a, b)
}

func processDone(p *process) bool {
	if p == nil {
		return true
	}
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func (s *runtimeState) freezeV2Locked(invalidateApproval bool) error {
	if !s.v2.disk.RecoveryRequired {
		// A completed prior recovery does not authorize a future incident.
		s.v2.disk.Recovery = nil
	} else if invalidateApproval && s.v2.disk.Recovery != nil && s.v2.disk.Recovery.PlanSequence > 0 {
		copy := *s.v2.disk.Recovery
		copy.HasHolds = true
		s.v2.disk.Recovery = &copy
	}
	s.v2.disk.RecoveryRequired = true
	_ = writeV2PrivateJSON(s.v2.path, s.v2.disk)
	return errors.New("relay v2 recovery_required; existing forwarding retained")
}

func (s *runtimeState) validateV2EnvelopeLocked(request V2SyncRequest, response V2SyncResponse) error {
	state := s.v2.disk
	if request.localCredentialGeneration != s.v2.credentialGeneration || request.ControlEpoch != state.ControlEpoch || request.AppliedRevision != state.Revision {
		return errors.New("stale control exchange after local state or trusted credential change")
	}
	if response.ProtocolVersion != ProtocolV2 || response.OfflinePolicy != KeepLast || response.AgentID != state.AgentID || response.AgentID != request.AgentID || response.RequestID != request.RequestID || !validV2ID(response.ControlEpoch) {
		return errors.New("v2 response identity or protocol mismatch")
	}
	if response.Status == "recovery_required" || (state.ControlEpoch != "" && response.ControlEpoch != state.ControlEpoch) || response.Revision < state.Revision {
		return s.freezeV2Locked(response.ControlEpoch != state.ControlEpoch || response.Revision < state.Revision)
	}
	trustedRecoveryReady := state.RecoveryRequired && state.Recovery != nil && state.Recovery.PlanSequence > 0 && !state.Recovery.HasHolds && response.Status == "ready" && response.Commands != nil && len(response.Commands) == 0 && s.recoveryLocallyConfirmedLocked()
	if state.RecoveryRequired && !trustedRecoveryReady {
		return errors.New("relay v2 recovery_required; explicit trusted takeover needed")
	}
	if response.Status != "ready" || response.PreviousRevision != request.AppliedRevision || request.AppliedRevision != state.Revision || request.ControlEpoch != state.ControlEpoch || response.Revision < response.PreviousRevision || response.Commands == nil || len(response.Commands) > MaxV2Commands || len(response.TrafficAcks) > MaxV2Traffic {
		return errors.New("incomplete or inconsistent v2 response")
	}
	return nil
}

func (s *runtimeState) applyV2Response(ctx context.Context, request V2SyncRequest, response V2SyncResponse) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.validateV2EnvelopeLocked(request, response); err != nil {
		return err
	}
	// Traffic ACKs refer only to the exact sent batch. Their persistence failure
	// does not turn a valid control command into an implicit stop or vice versa.
	accountingErr := s.acknowledgeV2TrafficLocked(request.Traffic, response.TrafficAcks)
	next := s.v2.disk
	next.Records = make(map[string]v2Record, len(s.v2.disk.Records)+len(response.Commands))
	for id, record := range s.v2.disk.Records {
		next.Records[id] = record
	}
	seenRules, seenCommands := map[string]bool{}, map[string]bool{}
	for _, raw := range response.Commands {
		command := normalizedV2Command(raw)
		if err := validateV2Command(command); err != nil {
			return err
		}
		if seenRules[command.RuleID] || seenCommands[command.CommandID] {
			return errors.New("duplicate v2 command identity")
		}
		seenRules[command.RuleID], seenCommands[command.CommandID] = true, true
		for otherID, other := range next.Records {
			if otherID != command.RuleID && other.Command.CommandID == command.CommandID {
				return errors.New("v2 command identity reused")
			}
		}
		previous, exists := next.Records[command.RuleID]
		if exists {
			if command.Generation < previous.Command.Generation {
				return errors.New("stale v2 rule generation")
			}
			if command.Generation == previous.Command.Generation {
				if !sameV2Command(command, previous.Command) {
					return errors.New("conflicting v2 generation")
				}
				continue
			}
			if command.CommandID == previous.Command.CommandID {
				return errors.New("v2 command identity reused across generations")
			}
			if previous.Command.Action == "revoke" {
				return errors.New("revoked v2 rule identity cannot be reused")
			}
		}
		if v2RunningAction(command.Action) && s.v2AccountingDegradedLocked() {
			return errors.New("new configuration frozen while accounting is degraded")
		}
		next.Records[command.RuleID] = v2Record{Command: command, State: "persisted", LastApplied: previous.LastApplied}
	}
	if len(next.Records) > maxV2Records {
		return errors.New("v2 retained rule catalogue is full; manual review required")
	}
	next.ControlEpoch, next.Revision = response.ControlEpoch, response.Revision
	if next.RecoveryRequired && next.Recovery != nil && next.Recovery.PlanSequence > 0 && !next.Recovery.HasHolds && response.Status == "ready" && len(response.Commands) == 0 {
		// A new trusted controller confirms its isolated catalogue before any
		// ordinary desired-state changes are allowed. Root finalization remains
		// a separate control-plane gate; this does not open global maintenance.
		next.RecoveryRequired = false
	}
	// No process mutation is allowed before the complete candidate intent has
	// passed validation and its private durable state has committed.
	if err := writeV2PrivateJSON(s.v2.path, next); err != nil {
		return errors.New("v2 intent persistence unavailable")
	}
	s.v2.disk = next
	s.v2.cacheOnly = false
	if err := s.reconcileV2Locked(ctx); err != nil {
		return err
	}
	return accountingErr
}

func (s *runtimeState) reconcileV2Locked(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.v2.disk.RecoveryRequired || s.v2.cacheOnly {
		return s.reconcileFrozenV2Locked(ctx)
	}
	var firstErr error
	for id, record := range s.v2.disk.Records {
		command := record.Command
		p := s.processes[id]
		state := record.State
		if !v2RunningAction(command.Action) {
			if prepared, ok := s.v2.prepared[id]; ok {
				prepared.process.cleanup()
				delete(s.v2.prepared, id)
			}
			if processDone(p) {
				delete(s.processes, id)
				state = "stopped"
			} else {
				s.stopLocked(id)
				state = "stopping"
			}
		} else {
			if p != nil && processDone(p) {
				delete(s.processes, id)
				p = nil
			}
			if p != nil {
				hash, err := RuntimeHash(p.rule)
				if err == nil && hash == command.RuntimeHash && !p.stopping && p.ack.State != "failed" {
					// Snapshot old cumulative counters before changing their billing
					// metadata. The traffic module detects subsequent period rolls.
					if p.rule.EntitlementVersion != command.Rule.EntitlementVersion || p.v2BillingPeriod != command.BillingPeriodID {
						s.recordV2TrafficLocked(p, p.traffic.InputBytes, p.traffic.OutputBytes, p.traffic.Connections, time.Now().UnixMilli())
					}
					p.rule = *command.Rule
					p.v2BillingPeriod = command.BillingPeriodID
					p.ack.Version = command.Rule.Version
					if p.ack.State == "ready" {
						state = "ready"
						copy := command
						record.LastApplied = &copy
					} else {
						state = "persisted"
					}
					if prepared, ok := s.v2.prepared[id]; ok {
						prepared.process.cleanup()
						delete(s.v2.prepared, id)
					}
					goto persistResult
				}
			}
			if time.Now().Before(s.v2.retryAt[id]) {
				continue
			}
			prepared, ok := s.v2.prepared[id]
			if ok && prepared.commandID != command.CommandID {
				prepared.process.cleanup()
				delete(s.v2.prepared, id)
				ok = false
			}
			if !ok {
				// Stage secret files BEFORE stopping the last valid listener. A
				// read-only/full disk therefore does not stop an existing rule.
				candidate, err := s.prepareProcess(*command.Rule)
				if err != nil {
					s.v2.retryAt[id] = time.Now().Add(5 * time.Second)
					if firstErr == nil {
						firstErr = errors.New("v2 candidate preparation unavailable")
					}
					continue
				}
				prepared = v2Prepared{commandID: command.CommandID, process: candidate}
				s.v2.prepared[id] = prepared
			}
			if p != nil && !processDone(p) {
				s.stopLocked(id)
				state = "stopping"
			} else {
				delete(s.v2.prepared, id)
				err := s.startPreparedLocked(ctx, *command.Rule, prepared.process)
				if err != nil {
					state = "failed"
					s.v2.retryAt[id] = time.Now().Add(5 * time.Second)
				} else {
					s.processes[id].v2BillingPeriod = command.BillingPeriodID
					state = "persisted"
				}
			}
		}
	persistResult:
		if record.State != state || state == "ready" && (s.v2.disk.Records[id].LastApplied == nil || !sameV2Command(*s.v2.disk.Records[id].LastApplied, command)) {
			record.State = state
			next := s.v2.disk
			next.Records = make(map[string]v2Record, len(s.v2.disk.Records))
			for key, value := range s.v2.disk.Records {
				next.Records[key] = value
			}
			next.Records[id] = record
			if err := writeV2PrivateJSON(s.v2.path, next); err != nil {
				if firstErr == nil {
					firstErr = errors.New("v2 result persistence unavailable")
				}
				continue
			}
			s.v2.disk = next
		}
	}
	return firstErr
}

// Recovery never completes a pending replacement against the last valid
// listener. Cold startup is also cache-only until a trusted control exchange:
// it may reconstruct LastApplied, not speculate that a pending intent applied.
func (s *runtimeState) reconcileFrozenV2Locked(ctx context.Context) error {
	var firstErr error
	for id, prepared := range s.v2.prepared {
		prepared.process.cleanup()
		delete(s.v2.prepared, id)
	}
	for id, record := range s.v2.disk.Records {
		p := s.processes[id]
		if !v2RunningAction(record.Command.Action) {
			// An explicitly persisted stop remains terminal for local recovery.
			if !processDone(p) {
				s.stopLocked(id)
				continue
			}
			delete(s.processes, id)
			if record.State != "stopped" {
				record.State = "stopped"
				next := s.v2.disk
				next.Records = make(map[string]v2Record, len(next.Records))
				for key, value := range s.v2.disk.Records {
					next.Records[key] = value
				}
				next.Records[id] = record
				if err := writeV2PrivateJSON(s.v2.path, next); err != nil {
					if firstErr == nil {
						firstErr = errors.New("v2 frozen stop result persistence unavailable")
					}
				} else {
					s.v2.disk = next
				}
			}
			continue
		}
		if !processDone(p) {
			continue
		} // Never replace an active listener while frozen.
		delete(s.processes, id)
		if record.LastApplied == nil || time.Now().Before(s.v2.retryAt[id]) {
			continue
		}
		last := record.LastApplied
		if err := s.startLocked(ctx, *last.Rule); err != nil {
			s.v2.retryAt[id] = time.Now().Add(5 * time.Second)
			if firstErr == nil {
				firstErr = errors.New("v2 cached configuration restart unavailable")
			}
			continue
		}
		s.processes[id].v2BillingPeriod = last.BillingPeriodID
	}
	return firstErr
}

func (s *runtimeState) v2Request(instanceID string) V2SyncRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.v2RequestLocked(instanceID)
}

func (s *runtimeState) v2RequestLocked(instanceID string) V2SyncRequest {
	s.v2.sequence++
	request := V2SyncRequest{ProtocolVersion: ProtocolV2, AgentID: s.v2.disk.AgentID, AgentInstanceID: instanceID, Sequence: s.v2.sequence, RequestID: randomID(), ControlEpoch: s.v2.disk.ControlEpoch, AppliedRevision: s.v2.disk.Revision, Capabilities: append([]string(nil), V2Capabilities...), Acks: []V2Ack{}, Traffic: s.v2TrafficBatchLocked(), AccountingDegraded: s.v2AccountingDegradedLocked()}
	request.localCredentialGeneration = s.v2.credentialGeneration
	for _, record := range s.v2.disk.Records {
		command := record.Command
		state := record.State
		p := s.processes[command.RuleID]
		// Never advertise a stale persisted ready/stopped state after a local
		// process transition until its new result has itself been persisted.
		if state == "ready" && (p == nil || p.stopping || processDone(p) || p.ack.State != "ready") {
			state = "persisted"
		}
		if state == "stopped" && !processDone(p) {
			state = "stopping"
		}
		request.Acks = append(request.Acks, V2Ack{CommandID: command.CommandID, RuleID: command.RuleID, Generation: command.Generation, RuntimeHash: command.RuntimeHash, State: state})
	}
	sort.Slice(request.Acks, func(i, j int) bool { return request.Acks[i].RuleID < request.Acks[j].RuleID })
	return request
}

func callV2(ctx context.Context, cfg Config, token string, request V2SyncRequest) (V2SyncResponse, error) {
	var response V2SyncResponse
	raw, err := json.Marshal(request)
	if err != nil {
		return response, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(cfg.ServerURL, "/")+"/api/relay-agent/v2/sync", bytes.NewReader(raw))
	if err != nil {
		return response, errors.New("invalid v2 control URL")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		return response, errors.New("v2 control transport unavailable")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		if res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden {
			return response, errors.New("v2 control authentication unavailable")
		}
		return response, errors.New("v2 control HTTP unavailable")
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, 2<<20+1))
	if err != nil || len(data) > 2<<20 {
		return response, errors.New("v2 response truncated or oversized")
	}
	if err = strictV2JSON(data, &response); err != nil {
		return response, err
	}
	return response, nil
}

func runV2(ctx context.Context, cfg Config, agentID, token string) error {
	if !validV2ID(agentID) || token == "" {
		return errors.New("v2 requires a trusted registered agent identity")
	}
	if err := ensureV2StateDir(cfg.StateDir); err != nil {
		return err
	}
	disk, err := loadV2State(cfg, agentID)
	if err != nil {
		return err
	}
	if disk.ManagementToken != "" {
		token = disk.ManagementToken
	} else {
		disk.ManagementToken = token
	}
	s := &runtimeState{cfg: cfg, processes: map[string]*process{}, pending: map[string]Traffic{}, observerToken: randomID(), v2: &v2RuntimeState{disk: disk, path: filepath.Join(cfg.StateDir, v2StateFile), prepared: map[string]v2Prepared{}, retryAt: map[string]time.Time{}, cacheOnly: true, token: token, wake: make(chan struct{}, 1)}}
	if err = s.initV2TrafficLocked(); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	s.observerURL = "http://" + listener.Addr().String() + "/observer?token=" + s.observerToken
	mux := http.NewServeMux()
	mux.HandleFunc("/observer", s.observer)
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go server.Serve(listener)
	defer server.Close()
	closeRecovery, err := serveV2Recovery(ctx, s)
	if err != nil {
		return err
	}
	defer closeRecovery()
	defer func() {
		s.mu.Lock()
		done := make([]<-chan struct{}, 0, len(s.processes))
		for id := range s.processes {
			s.stopLocked(id)
			done = append(done, s.processes[id].done)
		}
		for _, prepared := range s.v2.prepared {
			prepared.process.cleanup()
		}
		s.mu.Unlock()
		// Run owns the state-directory flock until this returns. Do not allow a
		// replacement Agent to start while our previous children still own ports.
		for _, exited := range done {
			<-exited
		}
	}()
	s.mu.Lock()
	_ = s.reconcileV2Locked(ctx)
	s.mu.Unlock()
	// Local process recovery is independent of management connectivity. There
	// is deliberately no lease, expiry watchdog or control-disconnect timer.
	localDone := make(chan struct{})
	localExited := make(chan struct{})
	defer func() { close(localDone); <-localExited }()
	go func() {
		defer close(localExited)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-localDone:
				return
			case <-ticker.C:
				s.mu.Lock()
				_ = s.reconcileV2Locked(ctx)
				s.mu.Unlock()
			}
		}
	}()
	instanceID := randomID()
	backoff := 5 * time.Second
	for {
		s.mu.Lock()
		request := s.v2RequestLocked(instanceID)
		exchangeCfg, exchangeToken := s.cfg, s.v2.token
		s.mu.Unlock()
		response, callErr := callV2(ctx, exchangeCfg, exchangeToken, request)
		if callErr == nil {
			callErr = s.applyV2Response(ctx, request, response)
		}
		status := "online"
		if callErr != nil {
			status = "sync_error"
			if strings.Contains(callErr.Error(), "authentication") {
				status = "auth_error"
			}
		}
		s.mu.Lock()
		if s.v2.disk.RecoveryRequired {
			status = "recovery_required"
		}
		if s.v2.controlStatus != status {
			log.Printf("relay v2 control status: %s; existing forwarding retained", status)
			s.v2.controlStatus = status
		}
		s.mu.Unlock()
		wait := 5 * time.Second
		if callErr != nil {
			wait = backoff
			backoff = min(30*time.Second, backoff*2)
		} else {
			backoff = 5 * time.Second
		}
		// Jitter is only a management retry interval, never forwarding validity.
		wait += time.Duration(time.Now().UnixNano() % int64(500*time.Millisecond))
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		case <-s.v2.wake:
			timer.Stop()
		}
	}
}
