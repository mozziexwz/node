package control

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"time"
)

// Restore uses an allowlist, never a list of known financial tables. Newly added
// collections stay current until their ownership and references are reviewed.
var safeRestoreCollections = []string{"articles", "attachments", "routes", "relay_agents", "executors"}
var preservedCommerceSettings = []string{"paidCreate", "planSale", "cards", "paywx", "payali", "purchaseRequireVerifiedEmail"}

type RestoreDifference struct {
	Collection string `json:"collection"`
	Current    int    `json:"current"`
	Snapshot   int    `json:"snapshot"`
	Added      int    `json:"added"`
	Changed    int    `json:"changed"`
	Removed    int    `json:"removed"`
}
type RestorePreflight struct {
	CurrentSHA256        string              `json:"currentSha256"`
	CurrentUsers         int                 `json:"currentUsers"`
	Differences          []RestoreDifference `json:"differences"`
	PreservedCollections []string            `json:"preservedCollections"`
	PreservedRoutes      []string            `json:"preservedRoutes"`
	PreservedNodes       []string            `json:"preservedNodes"`
	Blockers             []string            `json:"blockers"`
	SettingKeys          []string            `json:"settingKeys"`
}

func cloneState(s *State) (*State, error) {
	raw, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	var result State
	err = json.Unmarshal(raw, &result)
	return &result, err
}

func restoreScopeFingerprint(s *State) (string, error) {
	// Money/traffic may continue to arrive during maintenance. We merge the latest
	// complete current state inside the write transaction rather than rejecting it.
	scope := map[string]any{"settings": s.Settings}
	for _, name := range append(append([]string{}, safeRestoreCollections...), "user_rules") {
		documents := map[string]any{}
		for id, raw := range s.Docs[name] {
			var document map[string]any
			if err := json.Unmarshal(raw, &document); err != nil {
				return "", errors.New("预检范围中存在无效数据")
			}
			switch name {
			case "relay_agents":
				for _, key := range []string{"lastSeen", "online", "bootId", "version"} {
					delete(document, key)
				}
			case "executors":
				for _, key := range []string{"lastSeenAt", "lastIP", "ip", "online", "version"} {
					delete(document, key)
				}
			case "user_rules":
				// Acknowledgements, leases and cumulative meters are not a topology
				// edit. Rule identity/version and configured target remain bound.
				delete(document, "trafficBytes")
				delete(document, "segments")
			}
			documents[id] = document
		}
		scope[name] = documents
	}
	raw, err := json.Marshal(scope)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:]), nil
}

func validateRestoreIdle(s *State) error {
	if !boolSetting(s, "maintenance") {
		return errors.New("请先开启维护模式，然后重新预检")
	}
	for _, raw := range s.Docs["tasks"] {
		var task map[string]any
		if json.Unmarshal(raw, &task) != nil {
			return errors.New("当前任务记录无效，不能安全恢复")
		}
		if task["state"] == "running" || task["state"] == "queued" {
			return errors.New("存在未结束的任务")
		}
	}
	for _, rule := range ListDocs[UserRule](s, "user_rules") {
		for _, segment := range rule.Segments {
			if segment.LastLease > time.Now().UnixMilli() {
				return errors.New("请先暂停全部本站转发并等待租约失效")
			}
		}
	}
	return nil
}

func mergeSafeRestore(current, snapshot *State) (*State, RestorePreflight, error) {
	report := RestorePreflight{CurrentUsers: len(current.Users), Differences: []RestoreDifference{}, PreservedCollections: []string{}, PreservedRoutes: []string{}, PreservedNodes: []string{}, Blockers: []string{}}
	merged, err := cloneState(current)
	if err != nil {
		return nil, report, err
	}
	report.CurrentSHA256, err = restoreScopeFingerprint(current)
	if err != nil {
		return nil, report, err
	}
	copySnapshot, err := cloneState(snapshot)
	if err != nil {
		return nil, report, err
	}
	merged.Settings = copySnapshot.Settings
	for _, key := range preservedCommerceSettings {
		delete(merged.Settings, key)
		if value, ok := current.Settings[key]; ok {
			merged.Settings[key] = value
		}
	}
	settingNames := map[string]bool{}
	for key := range current.Settings {
		settingNames[key] = true
	}
	for key := range merged.Settings {
		settingNames[key] = true
	}
	report.SettingKeys = []string{}
	for key := range settingNames {
		if !reflect.DeepEqual(current.Settings[key], merged.Settings[key]) {
			report.SettingKeys = append(report.SettingKeys, key)
		}
	}
	sort.Strings(report.SettingKeys)
	allowed := map[string]bool{}
	for _, name := range safeRestoreCollections {
		allowed[name] = true
		merged.Docs[name] = copySnapshot.Docs[name]
		if merged.Docs[name] == nil {
			merged.Docs[name] = map[string]json.RawMessage{}
		}
	}
	// Existing per-user configurations and their metering cannot be rewound.
	// Keep their route topology even if the backup predates or differs from it.
	protectedRoutes, protectedNodes := map[string]bool{}, map[string]bool{}
	for _, rule := range ListDocs[UserRule](current, "user_rules") {
		route, ok := LoadDoc[Route](current, "routes", rule.RouteID)
		if !ok {
			return nil, report, fmt.Errorf("当前用户规则 %s 引用的线路不存在，请先修复关联", rule.ID)
		}
		if !reflect.DeepEqual(current.Docs["routes"][route.ID], merged.Docs["routes"][route.ID]) {
			protectedRoutes[route.ID] = true
		}
		merged.Docs["routes"][route.ID] = current.Docs["routes"][route.ID]
		for _, id := range relayRouteAgents(route) {
			if id == "" {
				continue
			}
			node, ok := current.Docs["relay_agents"][id]
			if !ok {
				return nil, report, fmt.Errorf("当前线路 %s 引用的节点 %s 不存在，请先修复关联", route.ID, id)
			}
			if !reflect.DeepEqual(node, merged.Docs["relay_agents"][id]) {
				protectedNodes[id] = true
			}
			merged.Docs["relay_agents"][id] = node
		}
	}
	// A snapshot may refer to a current node that was not in its backup manifest.
	for _, route := range ListDocs[Route](merged, "routes") {
		for _, id := range relayRouteAgents(route) {
			if id == "" {
				continue
			}
			if _, ok := merged.Docs["relay_agents"][id]; !ok {
				if node, found := current.Docs["relay_agents"][id]; found {
					merged.Docs["relay_agents"][id] = node
					protectedNodes[id] = true
				}
			}
		}
	}
	// Preserve executor identity references in current audit/task history.
	for id, raw := range current.Docs["executors"] {
		if _, ok := merged.Docs["executors"][id]; !ok {
			merged.Docs["executors"][id] = raw
		}
	}
	for id := range protectedRoutes {
		report.PreservedRoutes = append(report.PreservedRoutes, id)
	}
	for id := range protectedNodes {
		report.PreservedNodes = append(report.PreservedNodes, id)
	}
	sort.Strings(report.PreservedRoutes)
	sort.Strings(report.PreservedNodes)
	for name := range current.Docs {
		if !allowed[name] {
			report.PreservedCollections = append(report.PreservedCollections, name)
		}
	}
	sort.Strings(report.PreservedCollections)
	for _, name := range safeRestoreCollections {
		diff := RestoreDifference{Collection: name, Current: len(current.Docs[name]), Snapshot: len(snapshot.Docs[name])}
		for id, raw := range merged.Docs[name] {
			previous, found := current.Docs[name][id]
			if !found {
				diff.Added++
			} else if !reflect.DeepEqual(raw, previous) {
				diff.Changed++
			}
		}
		for id := range current.Docs[name] {
			if _, found := merged.Docs[name][id]; !found {
				diff.Removed++
			}
		}
		report.Differences = append(report.Differences, diff)
	}
	if err := validateRestoreReferences(merged); err != nil {
		return nil, report, err
	}
	return merged, report, nil
}

func validateRestoreReferences(s *State) error {
	for id, raw := range s.Docs["routes"] {
		var route Route
		if json.Unmarshal(raw, &route) != nil || id != route.ID {
			return errors.New("备份线路数据无效")
		}
		for _, node := range relayRouteAgents(route) {
			if node != "" {
				if _, ok := LoadDoc[RelayAgent](s, "relay_agents", node); !ok {
					return fmt.Errorf("线路 %s 缺少节点 %s；请使用包含节点定义的备份", id, node)
				}
			}
		}
	}
	for id, raw := range s.Docs["relay_agents"] {
		var agent RelayAgent
		if json.Unmarshal(raw, &agent) != nil || agent.ID != id {
			return errors.New("备份节点数据无效")
		}
	}
	for id, raw := range s.Docs["user_rules"] {
		var rule UserRule
		if json.Unmarshal(raw, &rule) != nil || rule.ID == "" || rule.UserID == "" || id != rule.UserID+":"+rule.RouteID {
			return errors.New("用户转发身份关联无效")
		}
		if _, ok := LoadDoc[Route](s, "routes", rule.RouteID); !ok {
			return errors.New("用户转发引用的线路不存在")
		}
	}
	for id, raw := range s.Docs["articles"] {
		var article Article
		if json.Unmarshal(raw, &article) != nil || article.ID != id {
			return errors.New("备份文章数据无效")
		}
		for _, attachment := range article.Attachments {
			object, ok := LoadDoc[attachmentObject](s, "attachments", attachment.ID)
			if !ok || object.ArticleID != id || object.Metadata != attachment || len(object.Bytes) != attachment.Size {
				return errors.New("备份文章附件关联或大小不一致")
			}
		}
	}
	for id, raw := range s.Docs["attachments"] {
		var object attachmentObject
		if json.Unmarshal(raw, &object) != nil || object.Metadata.ID != id || object.Metadata.Size != len(object.Bytes) {
			return errors.New("备份附件数据无效")
		}
		article, ok := LoadDoc[Article](s, "articles", object.ArticleID)
		found := false
		for _, meta := range article.Attachments {
			if meta.ID == id {
				found = true
			}
		}
		if !ok || !found {
			return errors.New("备份含有无文章引用的附件")
		}
	}
	return nil
}

func isolateRestoredState(s *State, disaster bool) error {
	s.Sessions = map[string]*Session{}
	s.Settings["maintenance"] = true
	delete(s.Docs, "email_challenges")
	delete(s.Docs, "executor_agents")
	for _, agent := range ListDocs[RelayAgent](s, "relay_agents") {
		agent.Enabled, agent.Online = false, false
		agent.TokenHash, agent.EnrollmentHash, agent.BootID = "", "", ""
		agent.EnrollmentExpires, agent.LastSeen = 0, 0
		if err := SaveDoc(s, "relay_agents", agent.ID, agent); err != nil {
			return err
		}
	}
	for id, raw := range s.Docs["executors"] {
		var agent map[string]any
		if json.Unmarshal(raw, &agent) != nil {
			return errors.New("执行机定义无效")
		}
		agent["tokenHash"], agent["status"], agent["lastSeenAt"] = "", "disabled", 0
		if err := SaveDoc(s, "executors", id, agent); err != nil {
			return err
		}
	}
	if err := prepareRestoredRelay(s, time.Now().UnixMilli()); err != nil {
		return err
	}
	for key, raw := range s.Docs["tasks"] {
		var task map[string]any
		if json.Unmarshal(raw, &task) != nil {
			return errors.New("任务记录无效")
		}
		if task["state"] == "running" || task["state"] == "queued" {
			task["state"], task["message"] = "interrupted", "备份恢复后任务不自动重放"
			if err := SaveDoc(s, "tasks", key, task); err != nil {
				return err
			}
		}
	}
	p, _ := LoadDoc[BackupPlan](s, "backup_plans", "default")
	p.Enabled = false
	if err := SaveDoc(s, "backup_plans", "default", p); err != nil {
		return err
	}
	if disaster {
		for _, key := range []string{"paidCreate", "planSale", "cards", "paywx", "payali"} {
			s.Settings[key] = false
		}
		for _, channel := range ListDocs[PaymentChannel](s, "payment_channels") {
			channel.Enabled = false
			if err := SaveDoc(s, "payment_channels", channel.ID, channel); err != nil {
				return err
			}
		}
		// Snapshot backup records refer to files on the old machine; keep their
		// history without presenting them as locally downloadable copies.
		for _, record := range ListDocs[BackupRecord](s, "backups") {
			if record.Targets == nil {
				record.Targets = map[string]string{}
			}
			record.Targets["local"], record.Status = "not_restored", "local_missing"
			if err := SaveDoc(s, "backups", record.ID, record); err != nil {
				return err
			}
		}
	}
	return nil
}
