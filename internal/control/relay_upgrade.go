package control

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
)

// Checks historical data, without migrating it or granting unproven keep_last
// capability. Called from the NEW image before replacing the running server.
func CheckRelayUpgrade(databaseURL string, output io.Writer) error {
	if !strings.HasPrefix(databaseURL, "postgres://") && !strings.HasPrefix(databaseURL, "postgresql://") {
		return errors.New("升级检查需要现有 PostgreSQL 数据库")
	}
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return errors.New("升级检查无法连接数据库")
	}
	defer db.Close()
	store := &Store{db: db, dialect: "postgres"}
	var blocked []string
	if err = store.View(func(s *State) error { blocked = legacyRelayUpgradeBlockers(s); return nil }); err != nil {
		return errors.New("升级检查无法读取站点数据，未替换当前版本")
	}
	if len(blocked) > 0 {
		_ = json.NewEncoder(output).Encode(map[string]any{"需要先升级的节点": blocked})
		return errors.New("以上节点尚未确认使用新版转发；请先在旧面板升级并确认在线，再升级网站。未停止现有服务")
	}
	_, err = io.WriteString(output, "节点协议检查通过，可升级网站。\n")
	return err
}

func legacyRelayUpgradeBlockers(s *State) []string {
	ids := map[string]bool{}
	for _, a := range ListDocs[RelayAgent](s, "relay_agents") {
		if a.TokenHash != "" && (a.ProtocolVersion != 2 || a.OfflinePolicy != "keep_last") {
			ids[a.ID] = true
		}
	}
	for _, r := range ListDocs[UserRule](s, "user_rules") {
		for _, seg := range r.Segments {
			if seg.ProtocolVersion != 2 && !seg.StopConfirmed {
				ids[seg.AgentID] = true
			}
		}
	}
	out := []string{}
	for id := range ids {
		a, ok := LoadDoc[RelayAgent](s, "relay_agents", id)
		if ok {
			out = append(out, a.Name+" ["+a.Address+"]")
		} else {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}
