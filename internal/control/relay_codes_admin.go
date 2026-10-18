package control

import (
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

type relayCodeUserRow struct {
	UserID           string `json:"userId"`
	Email            string `json:"email"`
	ActiveCodes      int    `json:"activeCodes"`
	Secrets          int    `json:"secrets"`
	LongestRemaining int64  `json:"longestExpiresAt"`
	UsedByOthers     int    `json:"usedByOthers7d"`
	UsedOthers       int    `json:"usedOthers7d"`
	Disabled         bool   `json:"disabled"`
	DisabledReason   string `json:"disabledReason,omitempty"`
	DisabledAt       int64  `json:"disabledAt,omitempty"`
	AtLimit          bool   `json:"atLimit"`
}

type relayCodeDeletionRow struct {
	ID         string `json:"id"`
	CreatedAt  int64  `json:"createdAt"`
	OwnerEmail string `json:"ownerEmail"`
	CodeTail   string `json:"codeTail"`
	Reason     string `json:"reason"`
	Actor      string `json:"actor"`
}

func relayCodeActor(s *State, actor string) string {
	if actor == "" || actor == "system" {
		return "系统"
	}
	if u := s.Users[actor]; u != nil {
		return u.Email
	}
	return "已删除用户"
}

func (t *TaskService) adminRelayCodes(w http.ResponseWriter, r *http.Request) {
	if _, err := t.app.Admin(r); err != nil {
		Fail(w, 403, "需要管理员权限")
		return
	}
	out := map[string]any{}
	_ = t.app.Store.View(func(s *State) error {
		settings := relayCodeSettings(s)
		now := time.Now().UnixMilli()
		week := now - int64(7*24*time.Hour/time.Millisecond)
		rows := map[string]*relayCodeUserRow{}
		row := func(id string) *relayCodeUserRow {
			if rows[id] == nil {
				email := "已删除用户"
				if u := s.Users[id]; u != nil {
					email = u.Email
				}
				rows[id] = &relayCodeUserRow{UserID: id, Email: email}
			}
			return rows[id]
		}
		activeCodes, secrets, earliest, latest := 0, 0, int64(0), int64(0)
		owners := map[string]bool{}
		for _, code := range ListDocs[RelayCode](s, relayCodesCollection) {
			current := row(code.OwnerID)
			if usableRelayCode(code) {
				current.ActiveCodes++
				activeCodes++
				owners[code.OwnerID] = true
			}
			if _, held := s.Docs[relayCodeSecretsCollection][code.ID]; held && code.State == "active" {
				current.Secrets++
				secrets++
				if code.ExpiresAt > current.LongestRemaining {
					current.LongestRemaining = code.ExpiresAt
				}
				if earliest == 0 || code.ExpiresAt < earliest {
					earliest = code.ExpiresAt
				}
				if code.ExpiresAt > latest {
					latest = code.ExpiresAt
				}
			}
		}
		uses, failures := 0, 0
		for _, use := range ListDocs[RelayCodeUse](s, relayCodeUsesCollection) {
			if use.CreatedAt < week {
				continue
			}
			switch use.Result {
			case "succeeded":
				uses++
				row(use.OwnerID).UsedByOthers++
				row(use.UserID).UsedOthers++
			case "failed":
				failures++
			}
		}
		for _, flag := range ListDocs[relayCodeUserFlag](s, relayCodeFlagsCollection) {
			if flag.Disabled {
				current := row(flag.UserID)
				current.Disabled, current.DisabledReason, current.DisabledAt = true, flag.Reason, flag.At
			}
		}
		deleted := map[string]int{}
		deletions := []relayCodeDeletionRow{}
		for _, event := range ListDocs[relayCodeEvent](s, relayCodeAuditCollection) {
			if event.Kind != "secret_deleted" {
				continue
			}
			if event.CreatedAt >= week {
				deleted[event.Reason]++
			}
			owner := "已删除用户"
			if u := s.Users[event.OwnerID]; u != nil {
				owner = u.Email
			}
			deletions = append(deletions, relayCodeDeletionRow{ID: event.ID, CreatedAt: event.CreatedAt, OwnerEmail: owner, CodeTail: event.CodeTail, Reason: event.Reason, Actor: relayCodeActor(s, event.Actor)})
		}
		sort.Slice(deletions, func(i, j int) bool { return deletions[i].CreatedAt > deletions[j].CreatedAt })
		if len(deletions) > 500 {
			deletions = deletions[:500]
		}
		users := make([]relayCodeUserRow, 0, len(rows))
		for _, current := range rows {
			current.AtLimit = current.ActiveCodes >= settings.MaxActive
			users = append(users, *current)
		}
		sort.Slice(users, func(i, j int) bool {
			if users[i].ActiveCodes != users[j].ActiveCodes {
				return users[i].ActiveCodes > users[j].ActiveCodes
			}
			return users[i].Email < users[j].Email
		})
		deletedTotal := 0
		for _, n := range deleted {
			deletedTotal += n
		}
		out["stats"] = map[string]any{"activeCodes": activeCodes, "activeOwners": len(owners), "secrets": secrets, "earliestDeletion": earliest, "latestDeletion": latest, "uses7d": uses, "failures7d": failures, "deleted7d": deletedTotal, "deletedByReason": deleted}
		out["users"] = users
		out["deletions"] = deletions
		out["settings"] = settings
		return nil
	})
	w.Header().Set("Cache-Control", "no-store")
	WriteJSON(w, 200, out)
}

func (t *TaskService) adminRelayCodeUser(w http.ResponseWriter, r *http.Request) {
	if _, err := t.app.Admin(r); err != nil {
		Fail(w, 403, "需要管理员权限")
		return
	}
	id := r.PathValue("id")
	out := map[string]any{}
	_ = t.app.Store.View(func(s *State) error {
		email := "已删除用户"
		if u := s.Users[id]; u != nil {
			email = u.Email
		}
		codes := []relayCodeView{}
		for _, code := range ownerRelayCodes(s, id) {
			// Administrators review usage; the code itself stays with its owner.
			codes = append(codes, t.relayCodeView(s, code, false))
		}
		out["user"] = map[string]string{"id": id, "email": email}
		out["codes"] = codes
		return nil
	})
	WriteJSON(w, 200, out)
}

func (t *TaskService) adminRevokeUserRelayCodes(w http.ResponseWriter, r *http.Request) {
	actor, err := t.app.Admin(r)
	if err != nil {
		Fail(w, 403, "需要管理员权限")
		return
	}
	count := 0
	err = t.app.Store.Update(func(s *State) error {
		if s.Users[r.PathValue("id")] == nil {
			return errors.New("用户不存在")
		}
		var err error
		count, err = endOwnerRelayCodes(s, r.PathValue("id"), "admin_revoke", actor.ID, time.Now())
		return err
	})
	if err != nil {
		Fail(w, 409, err.Error())
		return
	}
	WriteJSON(w, 200, map[string]any{"ok": true, "revoked": count})
}

func (t *TaskService) adminRelayCodeFeature(w http.ResponseWriter, r *http.Request) {
	actor, err := t.app.Admin(r)
	if err != nil {
		Fail(w, 403, "需要管理员权限")
		return
	}
	var in struct {
		Enabled bool   `json:"enabled"`
		Reason  string `json:"reason"`
	}
	if err = Decode(r, &in); err != nil {
		Fail(w, 400, "参数格式无效")
		return
	}
	in.Reason = strings.TrimSpace(in.Reason)
	if utf8.RuneCountInString(in.Reason) > 60 || strings.ContainsAny(in.Reason, "\x00\r\n") {
		Fail(w, 400, "原因最多 60 个字符，不能包含换行")
		return
	}
	count := 0
	err = t.app.Store.Update(func(s *State) error {
		id := r.PathValue("id")
		if s.Users[id] == nil {
			return errors.New("用户不存在")
		}
		now := time.Now()
		if in.Enabled {
			DeleteDoc(s, relayCodeFlagsCollection, id)
		} else {
			if err := SaveDoc(s, relayCodeFlagsCollection, id, relayCodeUserFlag{UserID: id, Disabled: true, Reason: in.Reason, At: now.UnixMilli(), By: actor.ID}); err != nil {
				return err
			}
			var err error
			if count, err = endOwnerRelayCodes(s, id, "admin_disabled", actor.ID, now); err != nil {
				return err
			}
		}
		action := "enable"
		if !in.Enabled {
			action = "disable"
		}
		relayCodeAuditEvent(s, relayCodeEvent{Kind: "feature", OwnerID: id, Actor: actor.ID, Action: action, Reason: in.Reason, Count: count})
		return nil
	})
	if err != nil {
		Fail(w, 409, err.Error())
		return
	}
	WriteJSON(w, 200, map[string]any{"ok": true, "revoked": count})
}

// adminPurgeRelayCodeSecrets is the emergency switch: every stored SSH is
// deleted and every usable code ends. Relays already built keep running.
func (t *TaskService) adminPurgeRelayCodeSecrets(w http.ResponseWriter, r *http.Request) {
	actor, err := t.app.Admin(r)
	if err != nil {
		Fail(w, 403, "需要管理员权限")
		return
	}
	var in struct {
		Confirm string `json:"confirm"`
	}
	if Decode(r, &in) != nil || in.Confirm != "DELETE_ALL_RELAY_CODE_SSH" {
		Fail(w, 400, "请明确确认删除全部暂存的 SSH")
		return
	}
	count := 0
	err = t.app.Store.Update(func(s *State) error {
		var err error
		count, err = endOwnerRelayCodes(s, "", "admin_purge", actor.ID, time.Now())
		return err
	})
	if err != nil {
		Fail(w, 409, err.Error())
		return
	}
	// Checks still running cannot store their SSH: their codes have ended.
	t.mu.Lock()
	clear(t.codeSecrets)
	t.mu.Unlock()
	WriteJSON(w, 200, map[string]any{"ok": true, "revoked": count})
}
