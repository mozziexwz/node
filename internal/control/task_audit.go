package control

import "strings"

// The operator display name belongs only to the administrator's response. It
// comes from the current account and is never written back to historical tasks.
type adminTask struct {
	Task
	UserName string `json:"userName"`
}

func adminTaskUserName(user *User) string {
	if user == nil {
		return "已删除用户"
	}
	if email := strings.TrimSpace(user.Email); email != "" {
		return email
	}
	return "未设置邮箱"
}

func validTaskAuditKind(kind string) bool {
	switch kind {
	case "", "deploy", "relay", "dd", "fingerprint", "front", "cleanup-preview", "cleanup":
		return true
	default:
		return false
	}
}

func adminTaskMatchesQuery(task adminTask, query string) bool {
	if query == "" {
		return true
	}
	for _, value := range []string{task.UserName, task.UserID, task.ID, task.Host, task.Kind, task.State, task.Remark} {
		if strings.Contains(strings.ToLower(value), query) {
			return true
		}
	}
	return false
}
