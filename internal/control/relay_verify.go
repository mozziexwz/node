package control

import (
	"encoding/json"
	"errors"

	"github.com/mozziexwz/node/internal/executor"
)

// Relay target verification: an executor opens the user's own mieru tunnel
// through the real relay entry and completes a hello with the MSBOOST node's
// loopback probe (see internal/executor/verify.go). Customer relays are
// checked when they are created; paid-route rules also on a schedule.

// RelayVerifySettings is the administrator's policy. Modes: "off", "log"
// (record only) and "enforce"; free relays only ever show a self-test.
type RelayVerifySettings struct {
	Free           string `json:"free"`
	Code           string `json:"code"`
	Donation       string `json:"donation"`
	RecheckHours   int    `json:"recheckHours"`
	GraceHours     int    `json:"graceHours"`
	BreakerPercent int    `json:"breakerPercent"`
	GameTarget     string `json:"gameTarget"`
	FilterBlocked  string `json:"filterBlocked"`
	FilterAllowed  string `json:"filterAllowed"`
}

// The defaults are the plan's first stage: every check runs and is recorded,
// nothing is refused until an administrator has reviewed the results.
func defaultRelayVerifySettings() RelayVerifySettings {
	return RelayVerifySettings{Free: "selftest", Code: "log", Donation: "log", RecheckHours: 24, GraceHours: 48, BreakerPercent: 20, GameTarget: "35.155.204.207:8585", FilterBlocked: "www.youtube.com", FilterAllowed: "www.nexon.com"}
}

func validateRelayVerifySettings(v RelayVerifySettings) error {
	mode := func(value string) bool { return value == "off" || value == "log" || value == "enforce" }
	switch {
	case v.Free != "off" && v.Free != "selftest":
		return errors.New("免费自备中转的自测设置无效")
	case !mode(v.Code) || !mode(v.Donation):
		return errors.New("校验模式只能是关闭、只记录或强制")
	case v.RecheckHours < 1 || v.RecheckHours > 168:
		return errors.New("复查间隔需为 1–168 小时")
	case v.GraceHours < 1 || v.GraceHours > 336:
		return errors.New("宽限时长需为 1–336 小时")
	case v.BreakerPercent < 5 || v.BreakerPercent > 100:
		return errors.New("熔断比例需为 5–100%")
	}
	return executor.ValidateVerifyOptions(&executor.VerifyOptions{Mode: "log", GameTarget: v.GameTarget, FilterBlocked: v.FilterBlocked, FilterAllowed: v.FilterAllowed})
}

// relayVerifySettings never returns a value outside the documented bounds,
// even if the stored document predates a field or was edited by hand.
func relayVerifySettings(s *State) RelayVerifySettings {
	v := defaultRelayVerifySettings()
	if raw, err := json.Marshal(s.Settings["relayVerify"]); err == nil && string(raw) != "null" {
		_ = json.Unmarshal(raw, &v)
	}
	if validateRelayVerifySettings(v) != nil {
		return defaultRelayVerifySettings()
	}
	return v
}

// relayVerifyOptions builds a job's options for one mode; nil means no check.
func relayVerifyOptions(s *State, mode string) *executor.VerifyOptions {
	switch mode {
	case "selftest":
		mode = "log"
	case "log", "enforce":
	default:
		return nil
	}
	v := relayVerifySettings(s)
	return &executor.VerifyOptions{Mode: mode, GameTarget: v.GameTarget, FilterBlocked: v.FilterBlocked, FilterAllowed: v.FilterAllowed}
}

// withRelayVerify adds a check to a customer relay request. Recording is
// skipped when no online executor can verify yet; enforcing never is, so the
// request then waits for an upgraded executor instead of going unchecked.
func (t *TaskService) withRelayVerify(s *State, request *executor.Request, mode string) {
	options := relayVerifyOptions(s, mode)
	if options == nil {
		return
	}
	candidate := *request
	candidate.Verify = options
	if options.Mode == "enforce" || t.hasExecutorFor(s, candidate) {
		request.Verify = options
	}
}

var (
	verifyGames   = map[string]bool{"ok": true, "refused": true, "timeout": true, "unreachable": true, "failed": true, "skipped": true}
	verifyFilters = map[string]bool{"ok": true, "missing": true, "inconclusive": true, "skipped": true}
)

// validVerifyResult accepts a report only for a job that asked for one, and
// only in its closed vocabulary.
func validVerifyResult(request executor.Request, out executor.Result) error {
	asked := request.Verify != nil || request.Kind == "relay-verify"
	if out.Verify == nil {
		if request.Kind == "relay-verify" && out.State == "succeeded" {
			return errors.New("校验结果缺失")
		}
		return nil
	}
	v := out.Verify
	switch {
	case !asked:
		return errors.New("该任务不接受校验结果")
	case !executor.ValidVerifyCode(v.Code) || v.Code == executor.VerifyTargetDown:
		return errors.New("校验结果代码无效")
	case v.ElapsedMS < 0 || v.ElapsedMS > 10*60*1000:
		return errors.New("校验耗时无效")
	case v.Code == executor.VerifyOK && (!verifyGames[v.Game] || !verifyFilters[v.Filter]):
		return errors.New("校验检测项无效")
	case v.Code != executor.VerifyOK && (v.Game != "" || v.Filter != ""):
		return errors.New("校验检测项无效")
	}
	return nil
}

// verifyFailedRole is the extra failure position of an enforced check.
func verifyFailedRole(request executor.Request, role string) bool {
	return role == "target" && request.Verify != nil && request.Verify.Mode == "enforce"
}

// verifyNextStep tells a relay-code user what to do after a refused check.
func verifyNextStep(code string) string {
	switch code {
	case "verify_not_msboost":
		return "目标不是 MSBOOST 节点，或节点版本较低：请重新部署MSBOOST节点，再用新的配置文件重新使用中转码。本次没有计次。"
	case "verify_tunnel_failed":
		return "配置文件与节点不匹配：请上传节点当前的配置文件后重试；节点重新部署过的话，旧配置已经失效。本次没有计次。"
	case "verify_entry_unreachable":
		return "新建的中转入口暂时连不上：请稍后重试，一直失败请联系拥有者检查防火墙。本次没有计次。"
	}
	return ""
}
