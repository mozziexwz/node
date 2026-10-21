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

// ValidateSSH also parses a private key, so a malformed key or a wrong
// passphrase is rejected before any task is queued.
func ValidateSSH(s SSH) error {
	if err := validateSSHFields(s); err != nil {
		return err
	}
	if s.UsesKey() {
		_, err := ParsePrivateKey(s.PrivateKey, s.Passphrase)
		return err
	}
	return nil
}

func validateSSHFields(s SSH) error {
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
	switch s.AuthMethod {
	case "", "password":
		if len(s.Password) < 1 || len(s.Password) > 512 || strings.ContainsAny(s.Password, "\x00\r\n") {
			return errors.New("SSH 密码无效")
		}
		if s.PrivateKey != "" || s.Passphrase != "" {
			return errors.New("密码登录不能同时提交私钥")
		}
	case "key":
		if s.Password != "" {
			return errors.New("密钥登录不能同时提交密码")
		}
		if len(s.PrivateKey) == 0 || len(s.PrivateKey) > maxPrivateKeyBytes {
			return errors.New("SSH 私钥为空或超过 16 KB")
		}
	default:
		return errors.New("SSH 登录方式无效")
	}
	if !fingerprintPattern.MatchString(s.Fingerprint) {
		return errors.New("请先检查并核对真实 SSH SHA256 指纹")
	}
	if s.TrustMode != "" && s.TrustMode != "strict" && s.TrustMode != "tofu" {
		return errors.New("SSH 信任模式无效")
	}
	if s.ReplaceFingerprint != "" && !fingerprintPattern.MatchString(s.ReplaceFingerprint) {
		return errors.New("待替换指纹格式无效")
	}
	return nil
}
func ValidateRequest(r Request) error {
	if r.Kind == "relay-verify" {
		return validateVerifyRequest(r)
	}
	if err := ValidateSSH(r.SSH); err != nil {
		return err
	}
	if r.Verify != nil {
		if r.Kind != "relay" && r.Kind != "relay-code" {
			return errors.New("校验参数只用于中转任务")
		}
		if err := ValidateVerifyOptions(r.Verify); err != nil {
			return err
		}
	}
	if r.ReportVerified && r.Kind != "relay-inventory" {
		return errors.New("校验记录参数只用于读取中转列表")
	}
	if r.ForwardTarget != nil {
		return errors.New("用户任务不接受内部中转参数")
	}
	if len([]rune(r.Remark)) > 60 || strings.ContainsAny(r.Remark, "\x00\r\n") {
		return errors.New("备注最多 60 个字符，不能包含换行")
	}
	if r.Kind != "cleanup" && r.Kind != "cleanup-preview" && r.Cleanup != nil {
		return errors.New("清理参数不匹配")
	}
	relayInstall := r.Kind == "relay" || r.Kind == "relay-code"
	if !relayInstall && (r.RateMbps != 0 || r.Meta != nil) {
		return errors.New("中转带宽和使用者记录只用于中转任务")
	}
	if r.Kind != "relay-manage" && r.Manage != nil {
		return errors.New("中转管理参数不匹配")
	}
	switch r.Kind {
	case "cleanup", "cleanup-preview":
		if r.Cleanup == nil || (r.Cleanup.Scope != "msboost" && r.Cleanup.Scope != "relay") || r.Front != nil || r.DD != nil || len(r.ClientConfig) > 0 {
			return errors.New("清理参数无效")
		}
		if r.Cleanup.ManagedTaskID != "" && (r.Cleanup.Scope != "relay" || !regexp.MustCompile(`^[A-Za-z0-9_-]{1,100}$`).MatchString(r.Cleanup.ManagedTaskID)) {
			return errors.New("定向清理任务编号无效")
		}
		if r.Kind == "cleanup" && (!r.Cleanup.Confirm || r.Cleanup.PreviewID == "" || !validSHA(r.Cleanup.Digest)) {
			return errors.New("请先预览并明确确认清理范围")
		}
		if r.Kind == "cleanup-preview" && (r.Cleanup.Confirm || r.Cleanup.PreviewID != "" || r.Cleanup.Digest != "") {
			return errors.New("预览任务不能携带清理确认")
		}
	case "deploy":
		if ip, err := netip.ParseAddr(r.SSH.Host); err != nil || !ip.Is4() {
			return errors.New("当前 MSBOOST 安装脚本要求公网 IPv4 服务器")
		}
		if r.Mode != "fresh" {
			return errors.New("部署 MSBOOST 仅支持全新安装")
		}
		if r.Front != nil || r.DD != nil || len(r.ClientConfig) > 0 {
			return errors.New("部署参数不匹配")
		}
	case "relay", "relay-code":
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
		if r.RateMbps < 0 || r.RateMbps > MaxRelayRateMbps || (r.Kind == "relay-code" && r.RateMbps == 0) {
			return errors.New("中转带宽必须为 1–10 Mbps")
		}
		if err := validateRelayMeta(r.Meta, r.Kind); err != nil {
			return err
		}
		if r.Mode != "" || r.DD != nil {
			return errors.New("中转参数不匹配")
		}
	case "relay-check", "relay-inventory":
		if r.Front != nil || r.DD != nil || len(r.ClientConfig) > 0 || r.Mode != "" || r.Remark != "" {
			return errors.New("中转服务器读取参数不匹配")
		}
	case "relay-manage":
		if r.Front != nil || r.DD != nil || len(r.ClientConfig) > 0 || r.Mode != "" || r.Remark != "" {
			return errors.New("中转管理参数不匹配")
		}
		if err := validateRelayManage(r.Manage); err != nil {
			return err
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
		// The pinned reinstall script cannot set a password and a key at once.
		if r.SSH.UsesKey() {
			switch r.DD.LoginMode {
			case "key":
				if r.DD.PasswordMode != "keep" || r.DD.NewPassword != "" {
					return errors.New("继续使用当前密钥时不能设置新的 root 密码")
				}
				return nil
			case "password":
				if r.DD.PasswordMode != "new" {
					return errors.New("改用密码登录时请填写新的 root 密码")
				}
			default:
				return errors.New("请选择重装后的登录方式")
			}
		} else if r.DD.LoginMode != "" && r.DD.LoginMode != "password" {
			return errors.New("密码登录的服务器只能用密码重装")
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

// A paid-route verification connects only to the route's own entry, which the
// panel has already written into the configuration; it needs no SSH.
func validateVerifyRequest(r Request) error {
	if r.Verify == nil {
		return errors.New("缺少校验参数")
	}
	if err := ValidateVerifyOptions(r.Verify); err != nil {
		return err
	}
	if _, err := ParseClientConfig(r.ClientConfig); err != nil {
		return err
	}
	if r.SSH != (SSH{}) || r.Front != nil || r.DD != nil || r.ForwardTarget != nil || r.Cleanup != nil || r.Manage != nil || r.Meta != nil || r.RateMbps != 0 || r.Mode != "" || r.Remark != "" || r.ReportVerified {
		return errors.New("校验任务参数不匹配")
	}
	return nil
}

const (
	DefaultRelayRateMbps = 5
	MaxRelayRateMbps     = 10
)

var managedRelayID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,100}$`)
var codeTailPattern = regexp.MustCompile(`^[A-Z0-9]{4}$`)

// RelayRateBytes converts the per-direction Mbps setting for the GOST limiter.
func RelayRateBytes(mbps int) int {
	if mbps == 0 {
		mbps = DefaultRelayRateMbps
	}
	return mbps * 125000
}

func validateRelayMeta(m *RelayMeta, kind string) error {
	if m == nil {
		if kind == "relay-code" {
			return errors.New("中转码任务缺少使用者记录")
		}
		return nil
	}
	switch m.Source {
	case "manual":
		if kind != "relay" || m.CodeTail != "" {
			return errors.New("使用者记录来源无效")
		}
	case "code":
		if kind != "relay-code" || !codeTailPattern.MatchString(m.CodeTail) {
			return errors.New("使用者记录来源无效")
		}
	default:
		return errors.New("使用者记录来源无效")
	}
	if len(m.User) < 3 || len(m.User) > 254 || !strings.Contains(m.User, "@") || strings.ContainsAny(m.User, "\x00\r\n\t ") {
		return errors.New("使用者邮箱无效")
	}
	if len(m.Panel) > 200 || strings.ContainsAny(m.Panel, "\x00\r\n\t ") || (m.Panel != "" && !strings.HasPrefix(m.Panel, "https://") && !strings.HasPrefix(m.Panel, "http://")) {
		return errors.New("网站地址无效")
	}
	if len([]rune(m.Remark)) > 60 || strings.ContainsAny(m.Remark, "\x00\r\n") {
		return errors.New("备注最多 60 个字符，不能包含换行")
	}
	return nil
}

func validateRelayManage(m *RelayManage) error {
	if m == nil {
		return errors.New("中转管理参数无效")
	}
	switch m.Action {
	case "rate":
		if m.RateMbps < 1 || m.RateMbps > MaxRelayRateMbps {
			return errors.New("中转带宽必须为 1–10 Mbps")
		}
	case "pause", "resume", "restart", "delete":
		if m.RateMbps != 0 {
			return errors.New("只有修改带宽需要带宽参数")
		}
	default:
		return errors.New("不支持的中转管理操作")
	}
	if len(m.Items) < 1 || len(m.Items) > 100 {
		return errors.New("请选择 1–100 条中转")
	}
	seen := map[string]bool{}
	for _, id := range m.Items {
		if !managedRelayID.MatchString(id) || seen[id] {
			return errors.New("中转编号无效")
		}
		seen[id] = true
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
