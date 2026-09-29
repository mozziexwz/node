package control

import (
	"net/http"
	"time"
)

// Read-only admission check, performed before an installer interrupts the old
// process. It never consumes or rotates an enrollment credential.
func (a *App) relayEnrollmentCheck(w http.ResponseWriter, r *http.Request) {
	if !a.allow("relay-enrollment-check:"+a.clientIP(r), 30, time.Minute) {
		Fail(w, 429, "安装检查过于频繁")
		return
	}
	var in struct {
		EnrollmentToken string `json:"enrollmentToken"`
	}
	if Decode(r, &in) != nil || len(in.EnrollmentToken) < 40 || len(in.EnrollmentToken) > 200 {
		Fail(w, 400, "注册令牌格式无效")
		return
	}
	status := "invalid_token"
	err := a.Store.View(func(s *State) error {
		for _, agent := range ListDocs[RelayAgent](s, "relay_agents") {
			if agent.EnrollmentHash != commerceHash(in.EnrollmentToken) || agent.EnrollmentExpires <= time.Now().UnixMilli() {
				continue
			}
			if _, retired := s.Docs[relayRetirementCollection][agent.ID]; retired {
				continue
			}
			switch {
			case !agent.Enabled:
				status = "disabled"
			case relayAgentRecoveryRequired(s, agent):
				status = "recovery_required"
			case restoreAgentNeedsRecovery(s, agent):
				status = "identity_exists"
			default:
				status = "ready"
			}
			break
		}
		return nil
	})
	if err != nil {
		Fail(w, 503, "无法核对节点安装状态")
		return
	}
	WriteJSON(w, 200, map[string]string{"status": status})
}
