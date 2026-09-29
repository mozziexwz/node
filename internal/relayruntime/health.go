package relayruntime

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"
)

func CheckRelayEnrollment(ctx context.Context, origin, token string) error {
	if !validRecoveryOrigin(origin) || len(token) < 40 || len(token) > 200 {
		return errors.New("安装地址或令牌格式无效")
	}
	var response struct {
		Status string `json:"status"`
	}
	if err := call(ctx, Config{ServerURL: origin}, "", "/api/relay-agent/enrollment-check", map[string]string{"enrollmentToken": token}, &response); err != nil {
		var httpErr controlHTTPError
		var dnsErr *net.DNSError
		var networkErr net.Error
		var certErr x509.UnknownAuthorityError
		var hostErr x509.HostnameError
		switch {
		case errors.As(err, &httpErr):
			switch httpErr.Code {
			case 401:
				return errors.New("面板拒绝安装认证，请重新核对安装令牌；原服务未停止")
			case 403:
				return errors.New("面板或反代拒绝安装请求，请检查访问规则和 WAF 日志；尚不能确定由 WAF 引起，原服务未停止")
			case 404:
				return errors.New("面板缺少新版安装预检接口，请先升级面板或核对地址；原服务未停止")
			case 429:
				return errors.New("安装检查过于频繁，请稍后重试；原服务未停止")
			default:
				return errors.New("面板返回非预期响应，请核对 HTTPS 地址、服务状态及反代规则；原服务未停止")
			}
		case errors.As(err, &dnsErr):
			return errors.New("无法解析面板域名，请检查 VPS 的 DNS；原服务未停止")
		case errors.As(err, &certErr), errors.As(err, &hostErr):
			return errors.New("面板 HTTPS 证书验证失败，请核对域名、证书及系统时间；原服务未停止")
		case errors.As(err, &networkErr) && networkErr.Timeout():
			return errors.New("连接面板超时，请检查 VPS 出站网络与面板访问规则；原服务未停止")
		}
		return errors.New("面板安装预检未通过，请核对面板版本、HTTPS 网络及反代访问限制；原服务未停止")
	}
	switch response.Status {
	case "ready":
		return nil
	case "invalid_token":
		return errors.New("安装令牌已过期、已使用或不属于此面板；原服务未停止")
	case "disabled":
		return errors.New("节点已停用，请先在后台核对；原服务未停止")
	case "recovery_required":
		return errors.New("此节点需要重新关联，请在后台处理；原服务未停止")
	case "identity_exists":
		return errors.New("此节点已有管理身份，请使用保留配置的升级或连接检查")
	default:
		return errors.New("面板未返回有效安装许可，原服务未停止")
	}
}

const relayHealthFile = "relay-health.json"

// Contains no token, target, key or customer configuration. Invocation binds
// the observation to the systemd process started by this installation.
type RelayHealth struct {
	AgentID      string `json:"agentId"`
	InstanceID   string `json:"instanceId"`
	InvocationID string `json:"invocationId"`
	ServerURL    string `json:"serverUrl"`
	Status       string `json:"status"`
	ObservedAt   int64  `json:"observedAt"`
}

func CheckRelayHealth(dir, invocation, origin string) error {
	if len(invocation) != 32 || origin == "" {
		return errors.New("无法取得本次服务启动编号")
	}
	path := filepath.Join(dir, relayHealthFile)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > 4096 {
		return errors.New("尚未取得本次节点连接检查结果")
	}
	f, err := os.Open(path)
	if err != nil {
		return errors.New("无法读取本次节点连接检查结果")
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 4097))
	var report RelayHealth
	if err != nil || len(raw) > 4096 || strictV2JSON(raw, &report) != nil || report.InvocationID != invocation || report.ServerURL != origin || !validV2ID(report.AgentID) || !validV2ID(report.InstanceID) || report.ObservedAt > time.Now().UnixMilli() || time.Now().UnixMilli()-report.ObservedAt > 20000 {
		return errors.New("当前进程尚未完成可核验的管理同步，旧缓存不能作为成功依据")
	}
	if report.Status != "online" {
		switch report.Status {
		case "auth_error":
			return errors.New("管理身份认证失败；恢复旧文件不代表旧身份仍有效")
		case "recovery_required":
			return errors.New("面板要求重新关联；请在节点管理检查恢复状态，不要反复重新安装")
		default:
			return errors.New("当前进程尚未完成认证同步，请检查管理连接")
		}
	}
	return nil
}
