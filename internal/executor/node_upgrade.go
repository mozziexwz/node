package executor

import (
	"context"
	"encoding/base64"
)

// Upgrading a deployed node and reading its configuration back. Both start by
// reading the node's current client configuration, which also proves that the
// server has an MSBOOST node: a server without one is never changed.

// nodeConfigTail follows the installer: both node services must run, then the
// client configuration is printed.
const nodeConfigTail = "msboost_phase=service\nsystemctl is-active --quiet msboost.service\nsystemctl is-active --quiet msboost-tcp-probe.service\nmsboost_phase=config\nprintf 'MSBOOST_CONFIG='\nbase64 -w0 /root/直连.json\nprintf '\\n'\n"

// nodeConfigScript only reads: the installer's state file, the services'
// status and the client configuration.
const nodeConfigScript = `set -Eeuo pipefail
export LC_ALL=C
[ "$(id -u)" = 0 ]
if [ ! -f /etc/msboost/install.env ] || [ ! -f /root/直连.json ] || [ -L /root/直连.json ]; then
  printf 'MSBOOST_ERROR_CODE=not_installed\nMSBOOST_ERROR_PHASE=node_check\n'
  exit 1
fi
printf 'MSBOOST_SERVICE=%s\n' "$(systemctl is-active msboost.service 2>/dev/null || true)"
printf 'MSBOOST_PROBE=%s\n' "$(systemctl is-active msboost-tcp-probe.service 2>/dev/null || true)"
printf 'MSBOOST_CONFIG='
base64 -w0 /root/直连.json
printf '\n'
`

type nodeConfig struct {
	data    []byte
	info    ConfigInfo
	running bool // both node services are active
}

// readNodeConfig returns the node's client configuration, checked the way a
// deploy checks it. A failure comes back as a finished Result.
func (e *Engine) readNodeConfig(ctx context.Context, s SSH, r Result) (nodeConfig, Result, bool) {
	out, err := e.Remote.Run(ctx, s, nodeConfigScript)
	if err != nil {
		return nodeConfig{}, failRemote(r, "node_check", out, err), false
	}
	data, err := base64.StdEncoding.DecodeString(marker(out, "MSBOOST_CONFIG"))
	if err != nil || len(data) == 0 {
		return nodeConfig{}, failed(r, "config", "未取得节点配置"), false
	}
	info, err := ParseClientConfig(data)
	if err != nil {
		return nodeConfig{}, failed(r, "config", "节点配置格式不符合要求"), false
	}
	if info.TargetHost != s.Host {
		return nodeConfig{}, failRemote(r, "node_check", nil, diagnosticError("config_host_mismatch")), false
	}
	running := marker(out, "MSBOOST_SERVICE") == "active" && marker(out, "MSBOOST_PROBE") == "active"
	return nodeConfig{data: data, info: info, running: running}, r, true
}

// fetchConfig hands the node's configuration back without changing the
// server: for a deploy whose configuration never reached the member's
// browser, or a browser that no longer holds it.
func (e *Engine) fetchConfig(ctx context.Context, job Job, r Result) Result {
	node, failure, ok := e.readNodeConfig(ctx, job.Request.SSH, r)
	if !ok {
		return failure
	}
	service := "stopped"
	if node.running {
		service = "running"
	}
	r.State = "succeeded"
	r.Phase = "complete"
	r.Message = "已从节点取回配置；公网 TCP 单独检查，游戏连通性尚未验证"
	r.Config = node.data
	r.Health = &Health{Service: service, LocalSelfTest: "not_tested", PublicTCP: e.TCPProbe(ctx, node.info.TargetHost, node.info.TargetPort), Game: "not_tested"}
	return r
}

// upgrade reruns the pinned installer on a deployed node without
// MSBOOST_FORCE_FRESH, so the installer keeps the recorded user, password,
// port and low-entropy level. The configuration afterwards must keep the same
// entry and credentials: the member's existing configuration file keeps
// working.
func (e *Engine) upgrade(ctx context.Context, job Job, r Result) Result {
	if err := verifyAsset(job.Script); err != nil {
		return failed(r, "integrity", err.Error())
	}
	before, failure, ok := e.readNodeConfig(ctx, job.Request.SSH, r)
	if !ok {
		return failure
	}
	old := before.info
	if checked, ok := e.preflightDebian(ctx, job.Request.SSH, r); !ok {
		return checked
	}
	script := prepareAsset(job.Script) + bbrTuneScript + encodedAssignment("MSBOOST_SERVER_IP", job.Request.SSH.Host) + "export MSBOOST_SERVER_IP\nmsboost_phase=install\n[ -f /etc/msboost/install.env ]\n"
	script += "bash \"$work/installer.sh\" >\"$work/private.log\" 2>&1\n" + nodeConfigTail
	out, err := e.Remote.Run(ctx, job.Request.SSH, script)
	r.Health = &Health{BBR: marker(out, "MSBOOST_BBR")}
	if err != nil {
		return failRemote(r, "install", out, err)
	}
	data, err := base64.StdEncoding.DecodeString(marker(out, "MSBOOST_CONFIG"))
	if err != nil || len(data) == 0 {
		return failed(r, "config", "未取得经过验证的客户端配置")
	}
	info, err := ParseClientConfig(data)
	if err != nil {
		return failed(r, "config", "安装结果的客户端配置格式不符合要求")
	}
	if info.TargetHost != old.TargetHost || info.TargetPort != old.TargetPort || info.Username != old.Username || info.Password != old.Password {
		return failRemote(r, "config", nil, diagnosticError("config_changed"))
	}
	r.State = "succeeded"
	r.Phase = "complete"
	r.Message = "节点已升级，配置没有变化；公网 TCP 单独检查，游戏连通性尚未验证"
	r.Config = data
	r.Health = &Health{Service: "running", LocalSelfTest: "passed", PublicTCP: e.TCPProbe(ctx, info.TargetHost, info.TargetPort), Game: "not_tested", BBR: marker(out, "MSBOOST_BBR")}
	return r
}
