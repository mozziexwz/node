package control

import (
	"errors"
	"net/http"
)

// Archiving revokes management credentials but deliberately retains every
// identity, route, reservation and runtime record. It is NOT stop confirmation.
func (a *App) relayArchiveAgent(w http.ResponseWriter, r *http.Request) {
	admin, err := a.Admin(r)
	if err != nil {
		commerceError(w, 403, err)
		return
	}
	var in struct {
		Archived bool   `json:"archived"`
		Confirm  string `json:"confirm"`
	}
	if Decode(r, &in) != nil || in.Confirm != "KEEP_REMOTE_STATE_AND_RESERVATIONS" {
		Fail(w, 400, "请确认封存不代表远端转发停止，端口和业务记录仍会保留")
		return
	}
	err = a.Store.Update(func(s *State) error {
		agent, ok := LoadDoc[RelayAgent](s, "relay_agents", r.PathValue("id"))
		if !ok {
			return errors.New("节点不存在")
		}
		if agent.Archived == in.Archived {
			return nil
		}
		agent.Archived = in.Archived
		if in.Archived {
			if restoreAgentNeedsRecovery(s, agent) {
				agent.ReconcileState = "recovery_required"
			}
			agent.Enabled, agent.Online, agent.LastSeen = false, false, 0
			agent.TokenHash, agent.EnrollmentHash, agent.EnrollmentExpires = "", "", 0
			clearRelayEnrollmentRetry(&agent)
			for _, route := range ListDocs[Route](s, "routes") {
				for _, id := range relayRouteAgents(route) {
					if id == agent.ID {
						route.AdmissionsClosed = true
						if err := SaveDoc(s, "routes", route.ID, route); err != nil {
							return err
						}
						break
					}
				}
			}
		}
		if err := SaveDoc(s, "relay_agents", agent.ID, agent); err != nil {
			return err
		}
		action := "relay.agent.unarchive"
		if in.Archived {
			action = "relay.agent.archive"
		}
		return commerceAudit(s, admin.ID, action, agent.ID)
	})
	if err != nil {
		commerceError(w, 409, err)
		return
	}
	WriteJSON(w, 200, map[string]any{"ok": true, "message": "列表状态已更新。此操作未停止远端转发，也未释放端口；恢复显示不会自动恢复管理连接。"})
}
