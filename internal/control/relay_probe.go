package control

import (
	"context"
	"encoding/gob"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/mozziexwz/node/internal/relayruntime"
)

// Route probes (探针): the website regularly asks the exit node of each paid
// route to open a TCP connection to the member's own MSBOOST node and keeps a
// day of results, so the member can tell a node that is down from a route
// problem. It is the manual diagnosis on a schedule: the same bounded probe of
// a target the node already forwards to, so Relay Agents need no upgrade.
//
// Results never enter State. That document is rewritten as a whole on every
// change and is what backups contain; a day of samples per rule would bloat
// both. They live in memory, with a snapshot file so a restart keeps them.

// RelayProbeSettings is the administrator's policy.
type RelayProbeSettings struct {
	Enabled         bool `json:"enabled"`
	IntervalMinutes int  `json:"intervalMinutes"`
}

func defaultRelayProbeSettings() RelayProbeSettings {
	return RelayProbeSettings{Enabled: true, IntervalMinutes: 2}
}

func validateRelayProbeSettings(v RelayProbeSettings) error {
	if v.IntervalMinutes < 1 || v.IntervalMinutes > 30 {
		return errors.New("探测间隔需为 1–30 分钟")
	}
	return nil
}

// relayProbeSettings never returns a value outside the documented bounds.
func relayProbeSettings(s *State) RelayProbeSettings {
	v := defaultRelayProbeSettings()
	if raw, err := json.Marshal(s.Settings["relayProbe"]); err == nil && string(raw) != "null" {
		_ = json.Unmarshal(raw, &v)
	}
	if validateRelayProbeSettings(v) != nil {
		return defaultRelayProbeSettings()
	}
	return v
}

const (
	relayProbeRetention = 24 * time.Hour
	relayProbePlanEvery = 30 * time.Second
	// Every finished probe makes its Agent synchronise once more, and each
	// synchronisation rewrites the stored state. One probe per Agent in the
	// time of one regular synchronisation at most doubles that; with more rules
	// than fit, each rule is simply probed less often than the interval.
	relayProbeAgentGap = 5 * time.Second
	// An Agent that did not answer is left alone for a while.
	relayProbeAgentRest = 30 * time.Second
	relayProbeSaveEvery = 5 * time.Minute
	relayProbeFile      = "relay-probe.gob"
	// Scheduled probes use at most this many of an Agent's probe slots, and
	// only while that many are free, so a manual diagnosis or a verification
	// asking whether a node is down always finds room.
	relayProbeSlots = 2
	// A stretched schedule can never store more than this for one rule.
	relayProbeSeriesLimit = 2000
)

// probeSample is one answered probe. Latency is -1 when the exit could not
// connect to the member's node.
type probeSample struct {
	At      int64
	Latency int32
}

type relayProbeTarget struct {
	ruleID, agentID, target string
}

type relayProbeMonitor struct {
	mu        sync.Mutex
	settings  RelayProbeSettings
	plan      []relayProbeTarget
	plannedAt time.Time
	// due is when each planned rule is probed next; agentAt is the earliest
	// start of the next probe on an Agent.
	due     map[string]int64
	agentAt map[string]int64
	busy    map[string]bool
	samples map[string][]probeSample
	dirty   bool
}

type relayProbeSnapshot struct {
	Version int
	Samples map[string][]probeSample
}

func (m *relayProbeMonitor) initLocked() {
	if m.due == nil {
		m.due, m.agentAt, m.busy = map[string]int64{}, map[string]int64{}, map[string]bool{}
	}
	if m.samples == nil {
		m.samples = map[string][]probeSample{}
	}
}

func (m *relayProbeMonitor) addLocked(ruleID string, sample probeSample) {
	limit := sample.At - relayProbeRetention.Milliseconds()
	series := append(m.samples[ruleID], sample)
	first := 0
	for first < len(series) && series[first].At <= limit {
		first++
	}
	first = max(first, len(series)-relayProbeSeriesLimit)
	m.samples[ruleID] = slices.Clone(series[first:])
	m.dirty = true
}

// pruneLocked forgets rules whose newest result has aged out.
func (m *relayProbeMonitor) pruneLocked(now int64) {
	limit := now - relayProbeRetention.Milliseconds()
	for ruleID, series := range m.samples {
		if len(series) == 0 || series[len(series)-1].At <= limit {
			delete(m.samples, ruleID)
			m.dirty = true
		}
	}
}

// series returns a rule's results of the last day, oldest first.
func (m *relayProbeMonitor) series(ruleID string, now int64) [][2]int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	limit := now - relayProbeRetention.Milliseconds()
	out := [][2]int64{}
	for _, sample := range m.samples[ruleID] {
		if sample.At > limit {
			out = append(out, [2]int64{sample.At, int64(sample.Latency)})
		}
	}
	return out
}

func (m *relayProbeMonitor) load(path string, now int64) {
	file, err := os.Open(path)
	if err != nil {
		return
	}
	defer file.Close()
	var snapshot relayProbeSnapshot
	// An unreadable snapshot only costs the history; it is replaced on the
	// next save.
	if gob.NewDecoder(file).Decode(&snapshot) != nil || snapshot.Version != 1 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.initLocked()
	limit := now - relayProbeRetention.Milliseconds()
	for ruleID, series := range snapshot.Samples {
		if len(ruleID) == 0 || len(ruleID) > 200 || len(m.samples[ruleID]) > 0 {
			continue
		}
		kept := make([]probeSample, 0, len(series))
		for _, sample := range series {
			if sample.At > limit && sample.At <= now && sample.Latency >= -1 && sample.Latency <= 3000 {
				kept = append(kept, sample)
			}
		}
		sort.SliceStable(kept, func(i, j int) bool { return kept[i].At < kept[j].At })
		if len(kept) > relayProbeSeriesLimit {
			kept = kept[len(kept)-relayProbeSeriesLimit:]
		}
		if len(kept) > 0 {
			m.samples[ruleID] = kept
		}
	}
}

// save writes the snapshot beside the database, replacing the previous one
// only once the new file is complete.
func (m *relayProbeMonitor) save(path string) {
	m.mu.Lock()
	if !m.dirty {
		m.mu.Unlock()
		return
	}
	snapshot := relayProbeSnapshot{Version: 1, Samples: make(map[string][]probeSample, len(m.samples))}
	for ruleID, series := range m.samples {
		snapshot.Samples[ruleID] = slices.Clone(series)
	}
	m.dirty = false
	m.mu.Unlock()
	failed := func() {
		m.mu.Lock()
		m.dirty = true
		m.mu.Unlock()
	}
	temporary := path + ".tmp"
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		failed()
		return
	}
	err = gob.NewEncoder(file).Encode(snapshot)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(temporary, path)
	}
	if err != nil {
		_ = os.Remove(temporary)
		failed()
	}
}

// relayProbeTargetFor says whether a rule is probed and, if not, why:
// "active", "paused", "unsupported" (its exit node cannot probe) or "waiting".
// Only the first exit of a route is probed.
func relayProbeTargetFor(s *State, rule UserRule, now int64) (relayProbeTarget, string) {
	none := relayProbeTarget{}
	if rule.State == "paused" {
		return none, "paused"
	}
	route, ok := LoadDoc[Route](s, "routes", rule.RouteID)
	user := s.Users[rule.UserID]
	if rule.State != "active" || !ok || !route.Enabled || !relayEntitled(user, now) || !userCanUseRoute(user, route) || relayRecoveryRequired(s, rule) {
		return none, "waiting"
	}
	view := relayRuleView(s, rule, false, now)
	if view.SyncState != "active" || view.TotalSegments == 0 || view.ReadySegments != view.TotalSegments {
		return none, "waiting"
	}
	exitID := route.EntryAgentID
	if route.Type != "port_forward" && len(route.Exit.AgentIDs) > 0 {
		exitID = route.Exit.AgentIDs[0]
	}
	for _, segment := range rule.Segments {
		if segment.AgentID != exitID {
			continue
		}
		rule.Segments = []RelaySegment{segment}
		target, err := relayDiagnosticTarget(rule)
		if err != nil {
			return none, "waiting"
		}
		agent, found := LoadDoc[RelayAgent](s, "relay_agents", exitID)
		if !found {
			return none, "waiting"
		}
		if !relayTargetProbeCapable(agent, now) {
			// An online node that never offered the probe is an old Agent.
			if agent.Enabled && agent.ProtocolVersion == relayruntime.ProtocolV2 && agent.LastSeen > now-relayLeaseMS && !slices.Contains(agent.Capabilities, relayruntime.TargetProbeCapability) {
				return none, "unsupported"
			}
			return none, "waiting"
		}
		return relayProbeTarget{ruleID: rule.ID, agentID: exitID, target: target}, "active"
	}
	return none, "waiting"
}

// relayProbePlan rereads which rules to probe. The schedule itself runs from
// memory, so the stored state is read twice a minute, not on every tick.
func (a *App) relayProbePlan(now time.Time) {
	m := &a.relayProbe
	nowMS := now.UnixMilli()
	settings := defaultRelayProbeSettings()
	var plan []relayProbeTarget
	err := a.Store.View(func(s *State) error {
		settings = relayProbeSettings(s)
		if !settings.Enabled {
			return nil
		}
		for _, rule := range ListDocs[UserRule](s, "user_rules") {
			if target, state := relayProbeTargetFor(s, rule, nowMS); state == "active" {
				plan = append(plan, target)
			}
		}
		return nil
	})
	m.mu.Lock()
	defer m.mu.Unlock()
	m.initLocked()
	m.plannedAt = now
	if err != nil {
		// Keep the previous plan; an unreachable database also fails the
		// Agents' own synchronisation, and those probes simply go unanswered.
		return
	}
	sort.Slice(plan, func(i, j int) bool { return plan[i].ruleID < plan[j].ruleID })
	m.settings, m.plan = settings, plan
	planned := make(map[string]bool, len(plan))
	for _, target := range plan {
		planned[target.ruleID] = true
		if _, known := m.due[target.ruleID]; !known {
			// A new rule is probed at once; the per-Agent gap spreads a burst.
			m.due[target.ruleID] = nowMS
		}
	}
	for ruleID := range m.due {
		if !planned[ruleID] {
			delete(m.due, ruleID)
		}
	}
	for agentID, at := range m.agentAt {
		if at <= nowMS {
			delete(m.agentAt, agentID)
		}
	}
	m.pruneLocked(nowMS)
}

// relayProbeTick starts the probes that are due.
func (a *App) relayProbeTick(ctx context.Context, now time.Time) {
	m := &a.relayProbe
	m.mu.Lock()
	refresh := m.plannedAt.IsZero() || now.Sub(m.plannedAt) >= relayProbePlanEvery
	m.mu.Unlock()
	if refresh {
		a.relayProbePlan(now)
	}
	nowMS := now.UnixMilli()
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.settings.Enabled {
		return
	}
	interval := time.Duration(m.settings.IntervalMinutes) * time.Minute
	// The longest overdue rule goes first, so every rule gets its turn when
	// an Agent has more rules than fit the interval.
	queue := make([]relayProbeTarget, 0, len(m.plan))
	for _, target := range m.plan {
		if !m.busy[target.ruleID] && m.due[target.ruleID] <= nowMS {
			queue = append(queue, target)
		}
	}
	sort.SliceStable(queue, func(i, j int) bool { return m.due[queue[i].ruleID] < m.due[queue[j].ruleID] })
	for _, target := range queue {
		if m.agentAt[target.agentID] > nowMS {
			continue
		}
		m.agentAt[target.agentID] = nowMS + relayProbeAgentGap.Milliseconds()
		job, err := a.relayDiagnostics.beginLimited("probe:"+target.ruleID, target.ruleID, target.agentID, target.target, relayProbeSlots)
		if err != nil {
			continue
		}
		a.relayWorkMu.Lock()
		if a.relayWorkClosed {
			a.relayWorkMu.Unlock()
			a.relayDiagnostics.end(job.id)
			return
		}
		a.relayWorkWG.Add(1)
		a.relayWorkMu.Unlock()
		m.busy[target.ruleID] = true
		m.due[target.ruleID] = nowMS + interval.Milliseconds()
		go a.relayProbeWait(ctx, target, job)
	}
}

func (a *App) relayProbeWait(ctx context.Context, target relayProbeTarget, job *relayDiagnosticJob) {
	defer a.relayWorkWG.Done()
	timer := time.NewTimer(time.Until(job.expires))
	defer timer.Stop()
	var report relayruntime.TargetProbeResult
	answered := false
	select {
	case report = <-job.result:
		answered = true
	case <-timer.C:
	case <-ctx.Done():
	case <-a.relayWorkCtx.Done():
	}
	a.relayDiagnostics.end(job.id)
	now := time.Now().UnixMilli()
	m := &a.relayProbe
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.busy, target.ruleID)
	if !answered {
		m.agentAt[target.agentID] = max(m.agentAt[target.agentID], now+relayProbeAgentRest.Milliseconds())
		return
	}
	// "unavailable" means the node is not running this rule right now; that
	// says nothing about the member's node, so nothing is recorded.
	switch report.Status {
	case "success":
		m.addLocked(target.ruleID, probeSample{At: now, Latency: int32(report.LatencyMS)})
	case "failed":
		m.addLocked(target.ruleID, probeSample{At: now, Latency: -1})
	}
}

// relayProbeLoop runs until the server stops, then saves the history once
// more. Close waits for it.
func (a *App) relayProbeLoop(ctx context.Context) {
	defer a.relayWorkWG.Done()
	path := filepath.Join(a.Config.DataDir, relayProbeFile)
	a.relayProbe.load(path, time.Now().UnixMilli())
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	save := time.NewTicker(relayProbeSaveEvery)
	defer save.Stop()
	for {
		select {
		case <-ctx.Done():
			a.relayProbe.save(path)
			return
		case <-a.relayWorkCtx.Done():
			a.relayProbe.save(path)
			return
		case now := <-ticker.C:
			a.relayProbeTick(ctx, now)
		case <-save.C:
			a.relayProbe.save(path)
		}
	}
}

func (a *App) startRelayProbe(ctx context.Context) {
	a.relayWorkMu.Lock()
	if a.relayWorkClosed {
		a.relayWorkMu.Unlock()
		return
	}
	a.relayWorkWG.Add(1)
	a.relayWorkMu.Unlock()
	go a.relayProbeLoop(ctx)
}

type relayProbeRouteView struct {
	RouteID   string `json:"routeId"`
	RouteName string `json:"routeName"`
	Level     int    `json:"level"`
	State     string `json:"state"`
	// Samples are [time in ms, connection time in ms]; -1 is a failed connection.
	Samples [][2]int64 `json:"samples"`
}

// relayProbeView returns the caller's own rules with their probe history.
func (a *App) relayProbeView(w http.ResponseWriter, r *http.Request) {
	u, err := a.User(r)
	if err != nil {
		commerceError(w, 401, err)
		return
	}
	if !a.allow("relay-probe:"+u.ID, 60, time.Minute) {
		commerceError(w, 429, errors.New("刷新过于频繁，请稍后重试"))
		return
	}
	now := time.Now().UnixMilli()
	settings := defaultRelayProbeSettings()
	routes := []relayProbeRouteView{}
	ruleIDs := []string{}
	err = a.Store.View(func(s *State) error {
		settings = relayProbeSettings(s)
		if !settings.Enabled {
			return nil
		}
		for _, route := range orderedRoutes(s) {
			rule, ok := LoadDoc[UserRule](s, "user_rules", u.ID+":"+route.ID)
			if !ok || rule.UserID != u.ID || rule.RouteID != route.ID || rule.State == "revoking" {
				continue
			}
			_, state := relayProbeTargetFor(s, rule, now)
			name := rule.RouteName
			if name == "" {
				name = route.Name
			}
			routes = append(routes, relayProbeRouteView{RouteID: route.ID, RouteName: name, Level: routeLevel(route.Level), State: state})
			ruleIDs = append(ruleIDs, rule.ID)
		}
		return nil
	})
	if err != nil {
		commerceError(w, 500, err)
		return
	}
	for i, ruleID := range ruleIDs {
		routes[i].Samples = a.relayProbe.series(ruleID, now)
	}
	WriteJSON(w, 200, map[string]any{"enabled": settings.Enabled, "intervalSeconds": settings.IntervalMinutes * 60, "retentionHours": int(relayProbeRetention / time.Hour), "now": now, "routes": routes})
}
