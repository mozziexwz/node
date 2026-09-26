package relayruntime

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

type process struct {
	rule            Rule
	epoch           string
	cmd             *exec.Cmd
	cancel          context.CancelFunc
	guard           *socksGuard
	ack             Ack
	traffic         Traffic
	expires         time.Time
	done            chan struct{}
	stopping        bool
	startedAt       int64
	v2BillingPeriod string
}
type runtimeState struct {
	mu            sync.Mutex
	cfg           Config
	processes     map[string]*process
	pending       map[string]Traffic
	journal       string
	observerToken string
	observerURL   string
	v2            *v2RuntimeState
	v2Traffic     *v2TrafficState
}

func randomID() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func atomicPrivateJSON(path string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".msboost-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(raw)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(tmp, path)
}
func call(ctx context.Context, cfg Config, token, path string, in, out any) error {
	raw, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(cfg.ServerURL, "/")+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("control server returned HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(out)
}

// GostConfig contains fixed GOST v3 TCP forwarding, service-level byte limits,
// upstream source ACLs and the cumulative observer. No arbitrary config input
// or shell is accepted from an HTTP request.
func GostConfig(rule Rule, observerURL string) ([]byte, error) {
	return gostConfig(rule, observerURL, gostTLSFiles{})
}
func gostConfig(rule Rule, observerURL string, tlsFiles gostTLSFiles) ([]byte, error) {
	return gostConfigAt(rule, observerURL, tlsFiles, fmt.Sprintf(":%d", rule.ListenPort))
}
func gostConfigAt(rule Rule, observerURL string, tlsFiles gostTLSFiles, listenAddress string) ([]byte, error) {
	if (rule.Protocol != "tcp" && rule.Protocol != "tls") || rule.ListenPort < 1 || rule.ListenPort > 65535 || rule.RateMbps < 1 || rule.RateMbps > 100000 || len(rule.Targets) < 1 || len(rule.Targets) > 8 {
		return nil, errors.New("invalid fixed forwarding rule")
	}
	if rule.Protocol == "tls" && (tlsFiles.certificate == "" || tlsFiles.key == "") {
		return nil, errors.New("TLS listener requires provisioned identity")
	}
	if len(rule.TargetTLS) > 0 && (len(rule.TargetTLS) != len(rule.Targets) || len(tlsFiles.cas) != len(rule.Targets)) {
		return nil, errors.New("TLS peer count mismatch")
	}
	if rule.Strategy != "round" && rule.Strategy != "rand" && rule.Strategy != "fifo" {
		return nil, errors.New("invalid selector strategy")
	}
	nodes := []map[string]any{}
	for i, target := range rule.Targets {
		host, p, err := net.SplitHostPort(target)
		if err != nil || host == "" {
			return nil, errors.New("invalid forwarding target")
		}
		port, err := strconv.Atoi(p)
		if err != nil || port < 1 || port > 65535 {
			return nil, errors.New("invalid target port")
		}
		node := map[string]any{"name": fmt.Sprintf("target-%d", i), "addr": target}
		if len(rule.TargetTLS) > 0 {
			node["connector"] = map[string]any{"type": "forward"}
			node["dialer"] = map[string]any{"type": "tls", "tls": map[string]any{"caFile": tlsFiles.cas[i], "secure": true, "serverName": rule.TargetTLS[i].ServerName, "options": map[string]any{"minVersion": "VersionTLS12"}}}
		}
		nodes = append(nodes, node)
	}
	service := map[string]any{"name": rule.ID, "addr": listenAddress, "handler": map[string]any{"type": "tcp"}, "listener": map[string]any{"type": "tcp"}, "forwarder": map[string]any{"nodes": nodes, "selector": map[string]any{"strategy": rule.Strategy, "maxFails": 1, "failTimeout": "10s"}}, "limiter": "rate", "observer": "observer", "metadata": map[string]any{"enableStats": true, "observer.period": "1s", "observer.resetTraffic": false}}
	bytesPerSecond := rule.RateMbps * 1000000 / 8
	config := map[string]any{"services": []any{service}, "limiters": []any{map[string]any{"name": "rate", "limits": []string{fmt.Sprintf("$ %dB %dB", bytesPerSecond, bytesPerSecond)}}}, "observers": []any{map[string]any{"name": "observer", "plugin": map[string]any{"type": "http", "addr": observerURL, "timeout": "3s"}}}}
	if len(rule.TargetTLS) > 0 {
		delete(service, "forwarder")
		service["handler"] = map[string]any{"type": "tcp", "chain": "next"}
		config["chains"] = []any{map[string]any{"name": "next", "hops": []any{map[string]any{"name": "next-hop", "nodes": nodes, "selector": map[string]any{"strategy": rule.Strategy, "maxFails": 1, "failTimeout": "10s"}}}}}
	}
	if rule.Protocol == "tls" {
		service["listener"] = map[string]any{"type": "tls", "tls": map[string]any{"certFile": tlsFiles.certificate, "keyFile": tlsFiles.key, "options": map[string]any{"minVersion": "VersionTLS12"}}}
	}
	if len(rule.AllowedSources) > 0 {
		for _, ip := range rule.AllowedSources {
			if net.ParseIP(ip) == nil {
				return nil, errors.New("invalid upstream source IP")
			}
		}
		service["admission"] = "upstream"
		config["admissions"] = []any{map[string]any{"name": "upstream", "whitelist": true, "matchers": rule.AllowedSources}}
	}
	return json.MarshalIndent(config, "", "  ")
}

func (s *runtimeState) persistLocked() error { return atomicPrivateJSON(s.journal, s.pending) }
func (s *runtimeState) observer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Query().Get("token") != s.observerToken {
		http.Error(w, "forbidden", 403)
		return
	}
	var in struct {
		Events []struct {
			Kind    string `json:"kind"`
			Service string `json:"service"`
			Type    string `json:"type"`
			Status  struct {
				State   string `json:"state"`
				Message string `json:"msg"`
			} `json:"status"`
			Stats struct {
				InputBytes   int64 `json:"inputBytes"`
				OutputBytes  int64 `json:"outputBytes"`
				CurrentConns int64 `json:"currentConns"`
			} `json:"stats"`
		} `json:"events"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
		http.Error(w, "invalid event", 400)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, event := range in.Events {
		p := s.processes[event.Service]
		if p == nil || r.URL.Query().Get("epoch") != p.epoch {
			continue
		}
		if event.Kind != "service" {
			continue
		}
		switch event.Type {
		case "status":
			if p.stopping {
				continue
			}
			switch event.Status.State {
			case "running":
				p.ack.State = "ready"
				p.ack.Message = "GOST listener bound; public reachability not independently verified"
			case "failed":
				p.ack.State = "failed"
				p.ack.Message = "GOST service failed"
			case "closed":
				// A service event cannot prove the child has exited.
				p.ack.State = "failed"
			}
		case "stats":
			if s.cfg.OfflinePolicy == KeepLast {
				s.recordV2TrafficLocked(p, event.Stats.InputBytes, event.Stats.OutputBytes, event.Stats.CurrentConns, time.Now().UnixMilli())
				if event.Stats.InputBytes >= p.traffic.InputBytes && event.Stats.OutputBytes >= p.traffic.OutputBytes && event.Stats.CurrentConns >= 0 {
					p.traffic.InputBytes, p.traffic.OutputBytes, p.traffic.Connections = event.Stats.InputBytes, event.Stats.OutputBytes, event.Stats.CurrentConns
				}
				continue
			}
			if event.Stats.InputBytes < p.traffic.InputBytes || event.Stats.OutputBytes < p.traffic.OutputBytes {
				continue
			}
			p.traffic.Sequence++
			p.traffic.InputBytes = event.Stats.InputBytes
			p.traffic.OutputBytes = event.Stats.OutputBytes
			p.traffic.Connections = event.Stats.CurrentConns
			s.pending[p.epoch] = p.traffic
		}
	}
	if s.cfg.OfflinePolicy != KeepLast {
		if err := s.persistLocked(); err != nil {
			http.Error(w, "journal unavailable", 503)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, `{"ok":true}`)
}
func (s *runtimeState) stopLocked(id string) {
	p := s.processes[id]
	if p == nil {
		return
	}
	if p.stopping {
		return
	}
	select {
	case <-p.done:
		p.stopping = true
		p.ack.State = "stopped"
		return
	default:
	}
	p.cancel()
	if p.guard != nil {
		p.guard.close()
	}
	p.stopping = true
	p.ack.State = "stopping"
	p.ack.Message = "rule removed or lease expired"
}
func (s *runtimeState) startLocked(ctx context.Context, rule Rule) error {
	prepared, err := s.prepareProcess(rule)
	if err != nil {
		return err
	}
	return s.startPreparedLocked(ctx, rule, prepared)
}

type preparedProcess struct {
	epoch, path string
	cleanupTLS  func()
	backend     net.Listener
	backendAddr string
}

func (p *preparedProcess) cleanup() {
	if p.backend != nil {
		_ = p.backend.Close()
	}
	p.cleanupTLS()
	_ = os.Remove(p.path)
}

func (s *runtimeState) prepareProcess(rule Rule) (*preparedProcess, error) {
	epoch := randomID()
	if _, err := parseGuardSources(rule.AllowedSources); err != nil {
		return nil, err
	}
	backend, err := reserveGuardBackend(rule.ListenPort)
	if err != nil {
		return nil, err
	}
	backendAddr := backend.Addr().String()
	backendRule := rule
	backendRule.ListenPort = backend.Addr().(*net.TCPAddr).Port
	if rule.Protocol == "tls" {
		// Terminate inter-node TLS in the Agent guard, then screen the cleartext
		// before passing it to loopback-only GOST. The externally pinned node
		// identity remains unchanged.
		if _, err := tls.X509KeyPair([]byte(rule.TLSCertificate), []byte(rule.TLSPrivateKey)); err != nil {
			_ = backend.Close()
			return nil, errors.New("invalid TLS node identity")
		}
		backendRule.Protocol = "tcp"
		backendRule.TLSCertificate, backendRule.TLSPrivateKey = "", ""
	}
	// The public guard enforces the original source ACL. GOST must be reachable
	// only from that local guard, never directly from the network.
	backendRule.AllowedSources = []string{"127.0.0.1"}
	raw, cleanupTLS, err := prepareGostConfig(backendRule, s.observerURL+"&epoch="+epoch, s.cfg.StateDir, backendAddr)
	if err != nil {
		_ = backend.Close()
		cleanupTLS()
		return nil, err
	}
	path := filepath.Join(s.cfg.StateDir, "rule-"+epoch+".json")
	if err = os.WriteFile(path, raw, 0600); err != nil {
		_ = backend.Close()
		cleanupTLS()
		return nil, err
	}
	return &preparedProcess{epoch: epoch, path: path, cleanupTLS: cleanupTLS, backend: backend, backendAddr: backendAddr}, nil
}

func (s *runtimeState) startPreparedLocked(ctx context.Context, rule Rule, prepared *preparedProcess) error {
	epoch, path, cleanupTLS := prepared.epoch, prepared.path, prepared.cleanupTLS
	guard, err := newSocksGuard(rule, prepared.backendAddr)
	if err != nil {
		prepared.cleanup()
		return err
	}
	_ = prepared.backend.Close()
	prepared.backend = nil
	child, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(child, s.cfg.GostBinary, "-C", path)
	protectChild(cmd)
	p := &process{rule: rule, epoch: epoch, cmd: cmd, cancel: cancel, guard: guard, ack: Ack{ID: rule.ID, Version: rule.Version, State: "pending"}, traffic: Traffic{ID: rule.ID, Version: rule.Version, Epoch: epoch, EntitlementVersion: rule.EntitlementVersion}, expires: time.Now().Add(30 * time.Second), done: make(chan struct{}), startedAt: time.Now().UnixMilli()}
	s.processes[rule.ID] = p
	// Logging is not forwarded to the control plane; target details are private.
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		guard.close()
		cleanupTLS()
		cancel()
		os.Remove(path)
		p.ack.State = "failed"
		p.ack.Message = "cannot start GOST; check binary and permissions"
		close(p.done)
		return err
	}
	guard.serve(child)
	go func() {
		err := cmd.Wait()
		guard.close()
		guard.wait()
		cleanupTLS()
		os.Remove(path)
		s.mu.Lock()
		if p.stopping {
			p.ack.State = "stopped"
		} else {
			p.ack.State = "failed"
			if err == nil {
				p.ack.Message = "GOST exited unexpectedly"
			} else {
				p.ack.Message = "GOST exited; check port conflicts and configuration"
			}
		}
		close(p.done)
		s.mu.Unlock()
	}()
	return nil
}
func (s *runtimeState) apply(ctx context.Context, response SyncResponse) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	desired := map[string]Rule{}
	for _, rule := range response.Rules {
		if len(rule.ID) < 16 || len(rule.ID) > 100 {
			return errors.New("invalid rule identity")
		}
		remaining := rule.LeaseUntil - response.ServerTime
		if remaining <= 0 {
			continue
		}
		if remaining > 60000 {
			return errors.New("invalid lease")
		}
		desired[rule.ID] = rule
	}
	for id, p := range s.processes {
		rule, wanted := desired[id]
		if !wanted || rule.Version != p.rule.Version {
			s.stopLocked(id)
		}
		if !wanted {
			select {
			case <-p.done:
				delete(s.processes, id)
			default:
			}
		}
	}
	for id, rule := range desired {
		p := s.processes[id]
		if p != nil && (p.rule.Version != rule.Version || p.ack.State == "stopped" || p.ack.State == "stopping" || p.ack.State == "failed") {
			select {
			case <-p.done:
				delete(s.processes, id)
				p = nil
			default:
				continue
			}
		}
		if p == nil {
			if err := s.startLocked(ctx, rule); err != nil {
				continue
			}
			p = s.processes[id]
		}
		p.expires = time.Now().Add(time.Duration(rule.LeaseUntil-response.ServerTime) * time.Millisecond)
	}
	return nil
}

func Run(ctx context.Context, cfg Config) error {
	if cfg.OfflinePolicy == "" {
		cfg.OfflinePolicy = KeepLast
	}
	if cfg.OfflinePolicy != "lease" && cfg.OfflinePolicy != KeepLast {
		return errors.New("offline policy must be lease or keep_last")
	}
	if runtime.GOOS != "linux" {
		return errors.New("production relay runtime requires Linux process-death protection")
	}
	parsed, err := url.Parse(cfg.ServerURL)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Scheme != "https" && !(parsed.Scheme == "http" && (parsed.Hostname() == "localhost" || net.ParseIP(parsed.Hostname()) != nil && net.ParseIP(parsed.Hostname()).IsLoopback())) {
		return errors.New("relay control URL must use HTTPS (HTTP only on loopback)")
	}
	if cfg.OfflinePolicy == KeepLast && (parsed.Opaque != "" || parsed.ForceQuery || parsed.RawPath != "" || parsed.Path != "" && parsed.Path != "/") {
		return errors.New("v2 control URL must be an origin")
	}
	if cfg.StateDir == "" {
		return errors.New("relay state directory is required")
	}
	if cfg.OfflinePolicy == "lease" {
		if _, err := os.Lstat(filepath.Join(cfg.StateDir, v2StateFile)); err == nil || !errors.Is(err, os.ErrNotExist) {
			return errors.New("refusing to downgrade a keep_last state directory to lease")
		}
	} else if err := ensureV2StateDir(cfg.StateDir); err != nil {
		return err
	}
	if cfg.OfflinePolicy == KeepLast {
		unlock, err := lockV2StateDir(cfg.StateDir)
		if err != nil {
			return err
		}
		defer unlock()
	}
	if cfg.GostBinary == "" {
		cfg.GostBinary = "gost"
	}
	if _, err = exec.LookPath(cfg.GostBinary); err != nil {
		return errors.New("GOST v3 binary is required")
	}
	if err = os.MkdirAll(cfg.StateDir, 0700); err != nil {
		return err
	}
	tokenPath := filepath.Join(cfg.StateDir, "relay-token.json")
	var token struct {
		Token   string `json:"token"`
		AgentID string `json:"agentId"`
	}
	if cfg.OfflinePolicy == KeepLast {
		// A root-authorized recovery persists credential and epoch together with
		// runtime intent. Prefer that atomic checkpoint over the original token
		// file, including after a crash between persistence and the live swap.
		var trusted v2DiskState
		if err := readV2PrivateJSON(filepath.Join(cfg.StateDir, v2StateFile), &trusted); err == nil {
			if trusted.ManagementToken != "" {
				if trusted.Schema != ProtocolV2 || trusted.OfflinePolicy != KeepLast || !validV2ID(trusted.AgentID) || !validRecoveryOrigin(trusted.ServerURL) || !validRecoveryToken(trusted.ManagementToken) || validateV2RecoveryCheckpoint(trusted) != nil {
					return errors.New("invalid trusted v2 credential checkpoint")
				}
				cfg.ServerURL, token.AgentID, token.Token = trusted.ServerURL, trusted.AgentID, trusted.ManagementToken
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return errors.New("cannot read trusted v2 credential checkpoint")
		}
		if token.Token == "" {
			if err := readV2PrivateJSON(tokenPath, &token); err != nil && !errors.Is(err, os.ErrNotExist) {
				return errors.New("v2 token file is not trusted private state")
			}
		}
	} else if data, err := os.ReadFile(tokenPath); err == nil {
		if err = json.Unmarshal(data, &token); err != nil {
			return err
		}
	}
	if token.Token == "" {
		if cfg.EnrollmentToken == "" {
			return errors.New("first registration needs --enrollment-token")
		}
		if err := call(ctx, cfg, "", "/api/relay-agent/register", map[string]string{"enrollmentToken": cfg.EnrollmentToken}, &token); err != nil {
			return err
		}
		writeToken := atomicPrivateJSON
		if cfg.OfflinePolicy == KeepLast {
			writeToken = writeV2PrivateJSON
		}
		if err := writeToken(tokenPath, token); err != nil {
			return err
		}
	}
	if cfg.OfflinePolicy == KeepLast {
		return runV2(ctx, cfg, token.AgentID, token.Token)
	}
	s := &runtimeState{cfg: cfg, processes: map[string]*process{}, pending: map[string]Traffic{}, journal: filepath.Join(cfg.StateDir, "traffic-journal.json"), observerToken: randomID()}
	if data, err := os.ReadFile(s.journal); err == nil {
		if err = json.Unmarshal(data, &s.pending); err != nil {
			return err
		}
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
	defer func() {
		s.mu.Lock()
		for id := range s.processes {
			s.stopLocked(id)
		}
		s.mu.Unlock()
	}()
	// Lease shutdown runs independently of network operations. A blocked sync can
	// never extend an offline rule beyond its last server-authorized lease.
	watchdogDone := make(chan struct{})
	defer close(watchdogDone)
	go func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-watchdogDone:
				return
			case <-ticker.C:
				s.mu.Lock()
				for id, p := range s.processes {
					if !time.Now().Before(p.expires) {
						s.stopLocked(id)
					}
				}
				s.mu.Unlock()
			}
		}
	}()
	boot := randomID()
	sequence := int64(0)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		sequence++
		in := SyncRequest{BootID: boot, Sequence: sequence, Version: "msboost-relay/1", Capabilities: []string{SocksGuardCapability}}
		s.mu.Lock()
		for _, p := range s.processes {
			in.Acks = append(in.Acks, p.ack)
		}
		for _, sample := range s.pending {
			in.Traffic = append(in.Traffic, sample)
		}
		s.mu.Unlock()
		var response SyncResponse
		requestStarted := time.Now()
		err = call(ctx, cfg, token.Token, "/api/relay-agent/sync", in, &response)
		if err == nil {
			// Deduct the full request round trip conservatively. Network delay
			// must never extend a server lease or overlap a target replacement.
			response.ServerTime += time.Since(requestStarted).Milliseconds()
			if err = s.apply(ctx, response); err != nil {
				return err
			}
			s.mu.Lock()
			for _, ack := range in.Acks {
				if ack.State != "stopped" {
					continue
				}
				p := s.processes[ack.ID]
				if p == nil || p.ack.State != "stopped" || p.rule.Version != ack.Version {
					continue
				}
				select {
				case <-p.done:
					delete(s.processes, ack.ID)
				default:
				}
			}
			for _, sample := range in.Traffic {
				if current, ok := s.pending[sample.Epoch]; ok && current.Sequence == sample.Sequence {
					delete(s.pending, sample.Epoch)
				}
			}
			persistErr := s.persistLocked()
			s.mu.Unlock()
			if persistErr != nil {
				return persistErr
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
