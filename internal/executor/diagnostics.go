package executor

import (
	"context"
	"errors"
	"net"
	"strings"
)

// Diagnostics are a closed vocabulary. Neither remote stderr nor an agent's
// supplied message is an acceptable public diagnostic (they may contain keys).
type Diagnostic struct{ Code, Phase, Message, NextStep string }

func (d Diagnostic) Error() string { return d.Message }

var diagnostics = map[string]Diagnostic{
	"ssh_timeout":            {"ssh_timeout", "ssh_connect", "SSH 连接超时", "检查 VPS 是否开机、SSH 端口和安全组是否允许执行机访问。"},
	"ssh_refused":            {"ssh_refused", "ssh_connect", "SSH 端口拒绝连接", "通过 VPS 控制台检查 SSH 服务及实际监听端口。"},
	"ssh_connect":            {"ssh_connect", "ssh_connect", "无法连接 SSH 服务", "检查公网 IP、端口、VPS 网络和安全组。"},
	"ssh_host_changed":       {"ssh_host_changed", "ssh_host_key", "SSH 主机指纹已变化，操作已中止", "先通过 VPS 控制台核实是否重装或更换主机，再在高级 SSH 设置中明确确认新指纹。"},
	"ssh_auth":               {"ssh_auth", "ssh_auth", "SSH 认证失败", "检查用户名和密码，以及 SSH 是否允许 root 密码登录；此结果不一定表示密码错误。"},
	"ssh_handshake":          {"ssh_handshake", "ssh_handshake", "SSH 握手失败", "确认端口提供的是 SSH 服务，并检查服务端算法配置和网络连接。"},
	"ssh_session":            {"ssh_session", "ssh_session", "SSH 会话建立失败", "检查 VPS 的会话数、资源限制和 SSH 服务状态。"},
	"task_timeout":           {"task_timeout", "execution", "任务执行超时或连接中断", "先核实 VPS 状态和服务是否已经创建；不会自动重试操作。"},
	"unsupported_system":     {"unsupported_system", "preflight", "服务器系统或架构不受支持", "使用支持的完整 systemd VPS；MSBOOST 建议 Debian 12 amd64。"},
	"missing_dependency":     {"missing_dependency", "dependencies", "必需的系统依赖不可用", "检查 apt 软件源、网络和磁盘空间，再安装缺失的依赖。"},
	"download_failed":        {"download_failed", "download", "远端下载失败", "检查 VPS 到 GitHub 和软件源的 DNS、HTTPS 网络及代理设置。"},
	"integrity_failed":       {"integrity_failed", "integrity", "安装资源完整性校验失败", "停止使用该资源，检查下载是否完整并联系管理员核对固定版本校验值。"},
	"target_unreachable":     {"target_unreachable", "target", "转发目标 TCP 连接失败", "检查目标节点地址、监听端口及节点安全组。"},
	"service_failed":         {"service_failed", "service", "远端服务启动或监听检查失败", "检查服务监听端口是否被占用，以及 VPS 的 systemd 和资源限制。"},
	"executor_configuration": {"executor_configuration", "executor", "执行机缺少固定版本下载配置", "请管理员升级执行机，确认当前架构的 GOST 地址和 SHA256 已配置。"},
	"ownership_failed":       {"ownership_failed", "ownership", "受管文件所有权检查失败", "检测到未知安装、符号链接或第三方修改；请人工检查，系统不会删除或覆盖这些文件。"},
	"cleanup_changed":        {"cleanup_changed", "cleanup", "清理范围在预览后发生变化", "重新预览并核对新的清理范围后再确认。"},
	"cleanup_failed":         {"cleanup_failed", "cleanup", "清理未全部完成", "部分服务可能已停止或文件已移除；请先重新预览检查，不能假定所有内容已清理。"},
	"invalid_config":         {"invalid_config", "config", "客户端配置校验或生成失败", "检查上传文件是否为单节点 TCP 配置；如果安装已运行，请先核实 VPS 上的服务状态。"},
	"invalid_request":        {"invalid_request", "validate", "任务参数校验失败", "核对服务器地址、SSH 参数和所选操作。"},
	"execution_failed":       {"execution_failed", "execution", "远端步骤执行失败，未获得足够证据确定原因", "请管理员根据失败阶段检查 VPS；不要提供密码、密钥或完整配置，系统不会自动重试。"},
	"executor_offline":       {"executor_offline", "executor", "执行机响应中断", "联系管理员检查执行机在线状态，并核实 VPS 上的任务结果；系统不会自动重放。"},
}

func PublicDiagnostic(code, phase string) Diagnostic {
	d, ok := diagnostics[code]
	if !ok {
		d = diagnostics["execution_failed"]
	}
	if d.Code == "execution_failed" && ValidPhase(phase) {
		d.Phase = phase
	}
	return d
}
func ValidPhase(phase string) bool {
	switch phase {
	case "queued", "executing", "complete", "fingerprint", "validate", "install", "config", "prepare", "submitted", "submission_unknown", "preflight", "target", "relay", "front", "integrity", "dependencies", "download", "service", "self_test", "execution", "ssh_connect", "ssh_host_key", "ssh_auth", "ssh_handshake", "ssh_session", "executor", "ownership", "cleanup":
		return true
	}
	return false
}
func diagnosticError(code string) error { return PublicDiagnostic(code, "") }
func remoteDiagnostic(err error, out []byte, phase string) Diagnostic {
	var d Diagnostic
	if errors.As(err, &d) {
		return d
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return PublicDiagnostic("task_timeout", phase)
	}
	code := marker(out, "MSBOOST_ERROR_CODE")
	remotePhase := marker(out, "MSBOOST_ERROR_PHASE")
	if code == "execution_failed" {
		switch remotePhase {
		case "download":
			code = "download_failed"
		case "target":
			code = "target_unreachable"
		case "integrity":
			code = "integrity_failed"
		case "dependencies":
			code = "missing_dependency"
		case "service":
			code = "service_failed"
		}
	}
	if _, ok := diagnostics[code]; ok {
		return PublicDiagnostic(code, remotePhase)
	}
	return PublicDiagnostic("execution_failed", phase)
}
func failRemote(r Result, phase string, out []byte, err error) Result {
	d := remoteDiagnostic(err, out, phase)
	r.State, r.Phase, r.ErrorCode, r.Message, r.NextStep = "failed", d.Phase, d.Code, d.Message, d.NextStep
	return r
}
func connectDiagnostic(err error) error {
	var n net.Error
	if errors.As(err, &n) && n.Timeout() {
		return diagnosticError("ssh_timeout")
	}
	if strings.Contains(strings.ToLower(err.Error()), "refused") {
		return diagnosticError("ssh_refused")
	}
	return diagnosticError("ssh_connect")
}

// Only fixed markers leave the remote machine. Installer logs remain private
// in a mode-0700 temporary directory and are removed by its existing EXIT trap.
const diagnosticPrelude = `
export LC_ALL=C
msboost_phase=preflight
msboost_diagnostic() {
  local code=execution_failed
  if [ -n "${work:-}" ] && [ -f "$work/private.log" ]; then
    if grep -Eq 'curl: \([0-9]+\)|wget: unable|Could not resolve|Temporary failure resolving|Failed to fetch' "$work/private.log"; then code=download_failed
    elif grep -Eq 'SHA256.*(mismatch|failed)|checksum.*(mismatch|failed)|FAILED$' "$work/private.log"; then code=integrity_failed
    elif grep -Eq 'not marked as an msboost|refusing to overwrite|ownership|symbolic link' "$work/private.log"; then code=ownership_failed
    elif grep -Eq 'Unsupported (distribution|architecture|operating)|running systemd instance is required' "$work/private.log"; then code=unsupported_system
    elif grep -Eq 'command not found|Unable to locate package|Unmet dependencies' "$work/private.log"; then code=missing_dependency
    fi
  fi
  printf 'MSBOOST_ERROR_CODE=%s\nMSBOOST_ERROR_PHASE=%s\n' "$code" "$msboost_phase"
}
trap msboost_diagnostic ERR
`
