package control

import (
	"encoding/json"
	"sort"
)

// Audit collections live in the shared state document. Without a bound they
// eventually push the state past backupMaxBytes and every growing write fails.
const (
	auditRetain     = 5000
	auditPruneSlack = 500
)

// Pruning only runs after the slack is exceeded, so the sort is amortised over
// hundreds of writes instead of paid on each one.
func pruneAudit(s *State, collection string) {
	docs := s.Docs[collection]
	if len(docs) <= auditRetain+auditPruneSlack {
		return
	}
	type entry struct {
		id        string
		createdAt int64
	}
	entries := make([]entry, 0, len(docs))
	for id, raw := range docs {
		entries = append(entries, entry{id: id, createdAt: auditCreatedAt(raw)})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].createdAt != entries[j].createdAt {
			return entries[i].createdAt < entries[j].createdAt
		}
		return entries[i].id < entries[j].id
	})
	for _, e := range entries[:len(entries)-auditRetain] {
		delete(docs, e.id)
	}
}

func auditCreatedAt(raw json.RawMessage) int64 {
	var v struct {
		CreatedAt int64 `json:"createdAt"`
	}
	_ = json.Unmarshal(raw, &v)
	return v.CreatedAt
}
