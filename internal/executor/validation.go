package executor

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"strings"
)

var userPattern = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_.-]{0,63}$`)
var domainPattern = regexp.MustCompile(`^(?:[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?\.)+[a-zA-Z]{2,63}$`)
var fingerprintPattern = regexp.MustCompile(`^SHA256:[A-Za-z0-9+/]{43}$`)
var blockedPrefixes = []string{"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/3", "::/96", "::ffff:0:0/96", "64:ff9b::/96", "100::/64", "2001::/32", "2001:2::/48", "2001:10::/28", "2001:20::/28", "2001:db8::/32", "2002::/16", "3fff::/20", "5f00::/16", "fc00::/7", "fe80::/10", "fec0::/10", "ff00::/8"}

func PublicIP(host string) error {
	ip, err := netip.ParseAddr(host)
	if err != nil || ip.Zone() != "" || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() {
		return errors.New("服务器地址必须是公网 IP")
	}
	for _, p := range blockedPrefixes {
		if netip.MustParsePrefix(p).Contains(ip) {
			return errors.New("不允许内部、保留或非公网地址")
		}
	}
	return nil
}
func ValidateSSH(s SSH) error {
	if err := PublicIP(s.Host); err != nil {
		return err
	}
	if s.Port < 1 || s.Port > 65535 {
		return errors.New("SSH 端口必须为 1–65535")
	}
	if !userPattern.MatchString(s.User) {
		return errors.New("SSH 用户名格式无效")
	}
	// The supplied installer requires root; sudo credentials are not silently inferred.
	if s.User != "root" {
		return errors.New("当前安装器要求 root SSH 登录")
	}
	if len(s.Password) < 1 || len(s.Password) > 512 || strings.ContainsAny(s.Password, "\x00\r\n") {
		return errors.New("SSH 密码无效")
	}
	if !fingerprintPattern.MatchString(s.Fingerprint) {
		return errors.New("请先检查并核对真实 SSH SHA256 指纹")
	}
	return nil
}
func ValidateRequest(r Request) error {
	if err := ValidateSSH(r.SSH); err != nil {
		return err
	}
	if r.ForwardTarget != nil {
		return errors.New("用户任务不接受内部转发参数")
	}
	switch r.Kind {
	case "deploy":
		if ip, err := netip.ParseAddr(r.SSH.Host); err != nil || !ip.Is4() {
			return errors.New("当前 MSBOOST 安装脚本要求公网 IPv4 服务器")
		}
		if r.Mode != "fresh" && r.Mode != "repair" {
			return errors.New("请选择全新安装或安全修复")
		}
		if r.Front != nil || r.DD != nil || len(r.ClientConfig) > 0 {
			return errors.New("部署参数不匹配")
		}
	case "relay":
		if _, err := ParseClientConfig(r.ClientConfig); err != nil {
			return err
		}
		if r.Front != nil {
			if err := ValidateSSH(*r.Front); err != nil {
				return err
			}
			if net.ParseIP(r.Front.Host).Equal(net.ParseIP(r.SSH.Host)) {
				return errors.New("前置机和中转机必须是两台服务器")
			}
		}
	case "dd":
		if r.DD == nil || !r.DD.ConfirmErase {
			return errors.New("必须确认重装会清除原系统和数据")
		}
		if r.DD.PortMode != "keep" && r.DD.PortMode != "new" {
			return errors.New("DD SSH 端口策略无效")
		}
		if r.DD.PortMode == "new" && (r.DD.NewPort < 1 || r.DD.NewPort > 65535) {
			return errors.New("新的 SSH 端口无效")
		}
		if r.DD.PasswordMode != "keep" && r.DD.PasswordMode != "new" {
			return errors.New("DD 密码策略无效")
		}
		p := r.SSH.Password
		if r.DD.PasswordMode == "new" {
			p = r.DD.NewPassword
		}
		if len(p) < 1 || len(p) > 512 || strings.ContainsAny(p, "\x00\r\n") {
			return errors.New("重装后的 SSH 密码不能为空、超过 512 个字符或包含换行")
		}
	default:
		return errors.New("不支持的任务类型")
	}
	return nil
}

type ConfigInfo struct {
	TargetHost string
	TargetPort int
	Username   string
	Password   string
	Document   map[string]any
}

func ParseClientConfig(data []byte) (ConfigInfo, error) {
	var out ConfigInfo
	if len(data) == 0 || len(data) > 128*1024 {
		return out, errors.New("配置文件为空或超过 128 KiB")
	}
	if err := json.Unmarshal(data, &out.Document); err != nil {
		return out, errors.New("客户端 JSON 格式无效")
	}
	p, ok := out.Document["profiles"].([]any)
	if !ok || len(p) != 1 {
		return out, errors.New("配置必须仅包含一个 profile，不能静默选择多个目标")
	}
	profile, ok := p[0].(map[string]any)
	if !ok {
		return out, errors.New("profile 格式无效")
	}
	servers, ok := profile["servers"].([]any)
	if !ok || len(servers) != 1 {
		return out, errors.New("配置必须仅包含一个 server")
	}
	s, ok := servers[0].(map[string]any)
	if !ok {
		return out, errors.New("server 格式无效")
	}
	ports, ok := s["portBindings"].([]any)
	if !ok || len(ports) != 1 {
		return out, errors.New("配置必须仅包含一个 TCP 端口")
	}
	pb, ok := ports[0].(map[string]any)
	if !ok {
		return out, errors.New("portBindings 格式无效")
	}
	n, ok := pb["port"].(float64)
	if !ok || n < 1 || n > 65535 || float64(int(n)) != n {
		return out, errors.New("目标端口无效")
	}
	if pb["protocol"] != "TCP" {
		return out, errors.New("MSBOOST 自备中转仅接受 TCP 配置")
	}
	out.TargetPort = int(n)
	ip, _ := s["ipAddress"].(string)
	domain, _ := s["domainName"].(string)
	if ip != "" && domain != "" {
		return out, errors.New("目标 IP 和域名不能同时设置")
	}
	if ip != "" {
		if err := PublicIP(ip); err != nil {
			return out, err
		}
		out.TargetHost = ip
	} else {
		if !domainPattern.MatchString(domain) {
			return out, errors.New("目标域名无效")
		}
		out.TargetHost = strings.ToLower(domain)
	}
	u, ok := profile["user"].(map[string]any)
	if !ok {
		return out, errors.New("配置缺少目标节点认证")
	}
	out.Username, _ = u["name"].(string)
	out.Password, _ = u["password"].(string)
	if out.Username == "" || out.Password == "" || len(out.Username) > 256 || len(out.Password) > 1024 {
		return out, errors.New("目标节点认证无效")
	}
	return out, nil
}
func RewriteClientConfig(data []byte, host string, port int) ([]byte, error) {
	info, err := ParseClientConfig(data)
	if err != nil {
		return nil, err
	}
	if port < 1 || port > 65535 {
		return nil, errors.New("入口端口无效")
	}
	profiles := info.Document["profiles"].([]any)
	p := profiles[0].(map[string]any)
	s := p["servers"].([]any)[0].(map[string]any)
	if net.ParseIP(host) != nil {
		if err := PublicIP(host); err != nil {
			return nil, err
		}
		s["ipAddress"] = host
		s["domainName"] = ""
	} else {
		if !domainPattern.MatchString(host) {
			return nil, fmt.Errorf("入口地址无效")
		}
		s["ipAddress"] = ""
		s["domainName"] = host
	}
	s["portBindings"].([]any)[0].(map[string]any)["port"] = port
	info.Document["socks5Port"] = 10086
	return json.MarshalIndent(info.Document, "", "  ")
}
