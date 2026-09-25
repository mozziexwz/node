package executor

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

type Engine struct {
	Remote   Remote
	TCPProbe func(context.Context, string, int) string
	// GOST archives are administrator-pinned; unsupported architectures fail closed.
	GostAMD64URL    string
	GostAMD64SHA256 string
	GostARM64URL    string
	GostARM64SHA256 string
}

func NewEngine() *Engine { return &Engine{Remote: SSHRemote{}} }
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func encodedAssignment(name, value string) string {
	return name + "=$(printf '%s' '" + base64.StdEncoding.EncodeToString([]byte(value)) + "' | base64 -d)\n"
}
func verifyAsset(a Asset) error {
	sum := sha256.Sum256(a.Data)
	if len(a.Data) == 0 || a.SHA256 != hex.EncodeToString(sum[:]) {
		return errors.New("安装脚本完整性校验失败")
	}
	return nil
}
func prepareAsset(a Asset) string {
	return "set -Eeuo pipefail\numask 077\n" + diagnosticPrelude + "[ \"$(id -u)\" = 0 ]\nwork=$(mktemp -d /run/msboost-task.XXXXXX)\ntrap 'rm -rf -- \"$work\"' EXIT\nprintf '%s' '" + base64.StdEncoding.EncodeToString(a.Data) + "' | base64 -d > \"$work/installer.sh\"\nprintf '%s  %s\\n' '" + a.SHA256 + "' \"$work/installer.sh\" | sha256sum -c - >/dev/null\n"
}

// Debian eligibility is checked on every customer VPS before BBR, package
// installation, or any other managed-state mutation. A rejected host receives
// only this read-only SSH command.
const debianPreflightScript = `set -Eeuo pipefail
export LC_ALL=C
if [ ! -r /etc/os-release ]; then
  printf 'MSBOOST_ERROR_CODE=unsupported_debian\nMSBOOST_ERROR_PHASE=preflight\n'
  exit 1
fi
. /etc/os-release
if [ "${ID:-}" != debian ] || [[ ! "${VERSION_ID:-}" =~ ^[0-9]+$ ]] || (( 10#${VERSION_ID:-0} < 11 )); then
  printf 'MSBOOST_ERROR_CODE=unsupported_debian\nMSBOOST_ERROR_PHASE=preflight\n'
  exit 1
fi
[ "$(id -u)" = 0 ]
[ -d /run/systemd/system ]
command -v systemctl >/dev/null
printf 'MSBOOST_READY=1\n'
`

func (e *Engine) preflightDebian(ctx context.Context, host SSH, r Result) (Result, bool) {
	out, err := e.Remote.Run(ctx, host, debianPreflightScript)
	if err != nil {
		return failRemote(r, "preflight", out, err), false
	}
	if marker(out, "MSBOOST_READY") != "1" {
		return failRemote(r, "preflight", out, diagnosticError("execution_failed")), false
	}
	return r, true
}
func marker(out []byte, key string) string {
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, key+"=") {
			return strings.TrimSpace(strings.TrimPrefix(line, key+"="))
		}
	}
	return ""
}
func failed(r Result, phase, message string) Result {
	code := "execution_failed"
	switch phase {
	case "validate":
		code = "invalid_request"
	case "integrity":
		code = "integrity_failed"
	case "config":
		code = "invalid_config"
	}
	return failRemote(r, phase, nil, PublicDiagnostic(code, phase))
}
func (e *Engine) Execute(ctx context.Context, job Job) Result {
	r := Result{ID: job.ID, Lease: job.Lease}
	if e.Remote == nil {
		e.Remote = SSHRemote{}
	}
	if e.TCPProbe == nil {
		e.TCPProbe = probeTCP
	}
	if job.Request.Kind == "fingerprint" {
		fp, alg, err := e.Remote.Probe(ctx, job.Request.SSH.Host, job.Request.SSH.Port)
		if err != nil {
			return failRemote(r, "fingerprint", nil, err)
		}
		r.State = "succeeded"
		r.Fingerprint = fp
		r.Algorithm = alg
		return r
	}
	if job.Request.Kind == "front" {
		return e.front(ctx, job, r)
	}
	if err := ValidateRequest(job.Request); err != nil {
		return failed(r, "validate", err.Error())
	}
	switch job.Request.Kind {
	case "deploy":
		return e.deploy(ctx, job, r)
	case "dd":
		return e.dd(ctx, job, r)
	case "relay":
		return e.relay(ctx, job, r)
	case "cleanup", "cleanup-preview":
		return e.cleanup(ctx, job, r)
	}
	return failed(r, "validate", "不支持的任务")
}
func (e *Engine) deploy(ctx context.Context, job Job, r Result) Result {
	if err := verifyAsset(job.Script); err != nil {
		return failed(r, "integrity", err.Error())
	}
	if checked, ok := e.preflightDebian(ctx, job.Request.SSH, r); !ok {
		return checked
	}
	script := prepareAsset(job.Script) + bbrTuneScript + encodedAssignment("MSBOOST_SERVER_IP", job.Request.SSH.Host) + "export MSBOOST_SERVER_IP\nmsboost_phase=install\n"
	script += encodedAssignment("node_user", "msboost-"+randomHex(8)) + encodedAssignment("node_pass", randomHex(24))
	// Fresh mode replaces the prior managed authentication and port. The
	// installer still snapshots managed files so a failed replacement can roll
	// back without deleting an existing working node.
	script += "MSBOOST_FORCE_FRESH=1 bash \"$work/installer.sh\" \"$node_user\" \"$node_pass\" >\"$work/private.log\" 2>&1\n"
	script += "msboost_phase=service\nsystemctl is-active --quiet msboost.service\nmsboost_phase=config\nprintf 'MSBOOST_CONFIG='\nbase64 -w0 /root/直连.json\nprintf '\\n'\n"
	out, err := e.Remote.Run(ctx, job.Request.SSH, script)
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
	if info.TargetHost != job.Request.SSH.Host {
		return failed(r, "config", "安装结果入口与已确认的服务器不一致")
	}
	r.State = "succeeded"
	r.Phase = "complete"
	r.Message = "服务运行及本地代理自测通过；公网 TCP 单独检查，游戏连通性尚未验证"
	r.Config = data
	r.Health = &Health{Service: "running", LocalSelfTest: "passed", PublicTCP: e.TCPProbe(ctx, info.TargetHost, info.TargetPort), Game: "not_tested"}
	return r
}
func probeTCP(ctx context.Context, host string, port int) string {
	conn, err := (&net.Dialer{Timeout: 7 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return "unreachable"
	}
	_ = conn.Close()
	return "reachable"
}
func (e *Engine) dd(ctx context.Context, job Job, r Result) Result {
	if err := verifyAsset(job.Script); err != nil {
		return failed(r, "integrity", err.Error())
	}
	opts := job.Request.DD
	p := job.Request.SSH.Password
	port := job.Request.SSH.Port
	if opts.PasswordMode == "new" {
		p = opts.NewPassword
	}
	if opts.PortMode == "new" {
		port = opts.NewPort
	}
	script := prepareAsset(job.Script) + encodedAssignment("new_password", p) + encodedAssignment("new_port", strconv.Itoa(port))
	// The pinned upstream prompts for a username even when password is supplied.
	// Supply it explicitly and detach stdin so an unexpected prompt cannot read
	// the remaining outer bash script (including the preparation success marker).
	script += "msboost_phase=prepare\nbash \"$work/installer.sh\" debian 12 --username root --password \"$new_password\" --ssh-port \"$new_port\" </dev/null >\"$work/private.log\" 2>&1\nprintf 'MSBOOST_PREPARED=1\\n'\n"
	out, err := e.Remote.Run(ctx, job.Request.SSH, script)
	if err != nil || marker(out, "MSBOOST_PREPARED") != "1" {
		return failRemote(r, "prepare", out, err)
	}
	commitCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	out, err = e.Remote.Run(commitCtx, job.Request.SSH, "set -e\n[ \"$(id -u)\" = 0 ]\nsystemctl reboot\nprintf 'MSBOOST_REBOOT_SUBMITTED=1\\n'\nsleep 120\n")
	var missing *ssh.ExitMissingError
	disconnected := errors.As(err, &missing) || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed)
	if marker(out, "MSBOOST_REBOOT_SUBMITTED") == "1" && disconnected {
		r.State = "executed"
		r.Phase = "submitted"
		r.Message = "已执行 DD 操作，请等待15分钟以上，再执行部署 MSBOOST。此状态仅表示操作已提交，不表示系统安装完成。"
		return r
	}
	r.State = "unknown"
	r.Phase = "submission_unknown"
	r.Message = "提交结果不明确：准备已成功，但未同时确认重启提交与预期断线。请从 VPS 控制台核实，禁止自动重试。"
	return r
}
func resolvePublicTarget(ctx context.Context, host string) (string, error) {
	if ip, err := netip.ParseAddr(host); err == nil {
		if err := PublicIP(ip.String()); err != nil {
			return "", err
		}
		return ip.String(), nil
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(ips) == 0 {
		return "", errors.New("目标域名解析失败")
	}
	for _, ip := range ips {
		if err := PublicIP(ip.String()); err != nil {
			return "", errors.New("目标域名包含内部或保留地址")
		}
	}
	// Freeze resolution into runtime config to prevent later DNS rebinding.
	return ips[0].String(), nil
}

func (e *Engine) relay(ctx context.Context, job Job, r Result) Result {
	info, err := ParseClientConfig(job.Request.ClientConfig)
	if err != nil {
		return failed(r, "config", err.Error())
	}
	target, err := resolvePublicTarget(ctx, info.TargetHost)
	if err != nil {
		return failed(r, "target", err.Error())
	}
	// Validate both SSH endpoints before any mutation, including their confirmed host keys.
	for _, s := range []*SSH{&job.Request.SSH, job.Request.Front} {
		if s == nil {
			continue
		}
		if checked, ok := e.preflightDebian(ctx, *s, r); !ok {
			return checked
		}
	}
	port, err := e.installRelay(ctx, job.Request.SSH, target, info.TargetPort, job.ID+"r", 625000)
	if err != nil {
		return failRemote(r, "relay", nil, err)
	}
	r.Hops = []Hop{{FromHost: job.Request.SSH.Host, FromPort: port, ToHost: info.TargetHost, ToPort: info.TargetPort}}
	entryHost, entryPort := job.Request.SSH.Host, port
	if job.Request.Front != nil {
		frontPort, err := e.installRelay(ctx, *job.Request.Front, job.Request.SSH.Host, port, job.ID+"f", 625000)
		if err != nil {
			e.cleanupRelay(ctx, job.Request.SSH, job.ID+"r")
			return failRemote(r, "front", nil, err)
		}
		r.Hops = append(r.Hops, Hop{FromHost: job.Request.Front.Host, FromPort: frontPort, ToHost: job.Request.SSH.Host, ToPort: port})
		entryHost = job.Request.Front.Host
		entryPort = frontPort
	}
	r.Config, err = RewriteClientConfig(job.Request.ClientConfig, entryHost, entryPort)
	if err != nil {
		return failed(r, "config", "入口配置生成失败")
	}
	if job.Request.Remark != "" {
		var doc map[string]any
		if json.Unmarshal(r.Config, &doc) == nil {
			profile := doc["profiles"].([]any)[0].(map[string]any)
			profile["profileName"] = job.Request.Remark
			doc["activeProfile"] = job.Request.Remark
			r.Config, _ = json.MarshalIndent(doc, "", "  ")
		}
	}
	r.State = "succeeded"
	r.Phase = "complete"
	r.Message = "TCP 中转服务已真实绑定端口；每端口上下行各 5 Mbps，游戏连接尚未验证"
	r.Health = &Health{Service: "running", LocalSelfTest: "target_tcp_reachable", PublicTCP: e.TCPProbe(ctx, entryHost, entryPort), Game: "not_tested"}
	return r
}
func (e *Engine) front(ctx context.Context, job Job, r Result) Result {
	if err := ValidateSSH(job.Request.SSH); err != nil {
		return failed(r, "validate", err.Error())
	}
	target := job.Request.ForwardTarget
	if target == nil || target.Port < 1 || target.Port > 65535 {
		return failed(r, "validate", "内部前置任务目标无效")
	}
	host, err := resolvePublicTarget(ctx, target.Host)
	if err != nil {
		return failed(r, "target", err.Error())
	}
	port, err := e.installRelay(ctx, job.Request.SSH, host, target.Port, job.ID+"p", 0)
	if err != nil {
		return failRemote(r, "front", nil, err)
	}
	r.State = "succeeded"
	r.Hops = []Hop{{FromHost: job.Request.SSH.Host, FromPort: port, ToHost: target.Host, ToPort: target.Port}}
	r.Health = &Health{Service: "running", LocalSelfTest: "target_tcp_reachable", PublicTCP: e.TCPProbe(ctx, job.Request.SSH.Host, port), Game: "not_tested"}
	return r
}
func validSHA(s string) bool { b, e := hex.DecodeString(s); return e == nil && len(b) == 32 }
func (e *Engine) installRelay(ctx context.Context, s SSH, target string, targetPort int, id string, rateBytes int) (int, error) {
	if len(id) > 100 || strings.ContainsAny(id, "/\\. \n\r'\"") {
		return 0, errors.New("任务标识无效")
	}
	archOut, err := e.Remote.Run(ctx, s, "uname -m\n")
	if err != nil {
		return 0, remoteDiagnostic(err, archOut, "preflight")
	}
	url, digest := e.GostAMD64URL, e.GostAMD64SHA256
	switch strings.TrimSpace(string(archOut)) {
	case "x86_64":
	case "aarch64", "arm64":
		url, digest = e.GostARM64URL, e.GostARM64SHA256
	default:
		return 0, diagnosticError("unsupported_system")
	}
	if !strings.HasPrefix(url, "https://github.com/go-gost/gost/releases/download/v") || !validSHA(digest) {
		return 0, diagnosticError("executor_configuration")
	}
	script := "set -Eeuo pipefail\numask 077\n" + diagnosticPrelude + encodedAssignment("gost_url", url) + encodedAssignment("gost_sha", digest) + encodedAssignment("task_id", id) + encodedAssignment("target_host", target) + encodedAssignment("target_port", strconv.Itoa(targetPort)) + encodedAssignment("rate_bytes", strconv.Itoa(rateBytes)) + relayInstallScript
	out, err := e.Remote.Run(ctx, s, script)
	if err != nil {
		return 0, remoteDiagnostic(err, out, "relay")
	}
	port, err := strconv.Atoi(marker(out, "MSBOOST_RELAY_PORT"))
	if err != nil || port < 20000 || port > 59999 {
		return 0, errors.New("未获得真实绑定的中转端口")
	}
	return port, nil
}
func (e *Engine) cleanupRelay(ctx context.Context, s SSH, id string) {
	if len(id) > 100 || strings.ContainsAny(id, "/\\. \n\r'\"") {
		return
	}
	_, _ = e.Remote.Run(ctx, s, "set -e\n"+encodedAssignment("task_id", id)+relayCleanupScript)
}
