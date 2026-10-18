package executor

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func privateKeyPEM(t *testing.T, key crypto.PrivateKey, passphrase string) string {
	t.Helper()
	var block *pem.Block
	var err error
	if passphrase == "" {
		block, err = ssh.MarshalPrivateKey(key, "msboost-test")
	} else {
		block, err = ssh.MarshalPrivateKeyWithPassphrase(key, "msboost-test", []byte(passphrase))
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(block))
}

// keyServer accepts exactly one public key or one password and answers every
// exec request with fixed output; it never runs the received script.
func keyServer(t *testing.T, authorized ssh.PublicKey, password string) (string, string) {
	t.Helper()
	_, hostPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, _ := ssh.NewSignerFromKey(hostPrivate)
	config := &ssh.ServerConfig{}
	if authorized != nil {
		config.PublicKeyCallback = func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if string(key.Marshal()) == string(authorized.Marshal()) {
				return nil, nil
			}
			return nil, errors.New("unknown key")
		}
	}
	if password != "" {
		config.PasswordCallback = func(_ ssh.ConnMetadata, given []byte) (*ssh.Permissions, error) {
			if string(given) == password {
				return nil, nil
			}
			return nil, errors.New("wrong password")
		}
	}
	config.AddHostKey(hostSigner)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				server, channels, requests, err := ssh.NewServerConn(conn, config)
				if err != nil {
					return
				}
				defer server.Close()
				go ssh.DiscardRequests(requests)
				for incoming := range channels {
					channel, channelRequests, err := incoming.Accept()
					if err != nil {
						return
					}
					for request := range channelRequests {
						if request.Type != "exec" {
							_ = request.Reply(false, nil)
							continue
						}
						_ = request.Reply(true, nil)
						// Read the whole script (until the client's EOF) first,
						// as a real shell would, before answering and closing.
						_, _ = io.Copy(io.Discard, channel)
						_, _ = channel.Write([]byte("MSBOOST_READY=1\n"))
						status := make([]byte, 4)
						binary.BigEndian.PutUint32(status, 0)
						_, _ = channel.SendRequest("exit-status", false, status)
						_ = channel.Close()
						break
					}
				}
			}()
		}
	}()
	return listener.Addr().String(), ssh.FingerprintSHA256(hostSigner.PublicKey())
}

func TestSSHKeyAndPasswordAuthentication(t *testing.T) {
	_, edKey, _ := ed25519.GenerateKey(rand.Reader)
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name       string
		key        crypto.PrivateKey
		public     crypto.PublicKey
		passphrase string
		given      string
		label      string
	}{
		{"ed25519", edKey, edKey.Public(), "", "", "ED25519"},
		{"rsa", rsaKey, &rsaKey.PublicKey, "", "", "RSA"},
		{"ed25519 with passphrase", edKey, edKey.Public(), "correct horse", "correct horse", "ED25519"},
		{"unneeded passphrase is ignored", edKey, edKey.Public(), "", "not needed", "ED25519"},
	} {
		t.Run(test.name, func(t *testing.T) {
			public, err := ssh.NewPublicKey(test.public)
			if err != nil {
				t.Fatal(err)
			}
			address, fingerprint := keyServer(t, public, "")
			remote := SSHRemote{dialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, address)
			}}
			s := SSH{Host: "8.8.8.8", Port: 22, User: "root", AuthMethod: "key", PrivateKey: privateKeyPEM(t, test.key, test.passphrase), Passphrase: test.given, Fingerprint: fingerprint}
			if err := ValidateSSH(s); err != nil {
				t.Fatal(err)
			}
			if label := KeyLabel(s); label != test.label {
				t.Fatalf("label %q", label)
			}
			out, err := remote.Run(context.Background(), s, "true\n")
			if err != nil || marker(out, "MSBOOST_READY") != "1" {
				t.Fatalf("key login failed: %v %s", err, out)
			}
			// A different key is refused by the server.
			_, other, _ := ed25519.GenerateKey(rand.Reader)
			s.PrivateKey, s.Passphrase = privateKeyPEM(t, other, ""), ""
			if _, err := remote.Run(context.Background(), s, "true\n"); err == nil || !strings.Contains(err.Error(), "认证") {
				t.Fatalf("unauthorized key accepted: %v", err)
			}
		})
	}
	address, fingerprint := keyServer(t, nil, "password-login")
	remote := SSHRemote{dialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}}
	out, err := remote.Run(context.Background(), SSH{Host: "8.8.8.8", Port: 22, User: "root", Password: "password-login", Fingerprint: fingerprint}, "true\n")
	if err != nil || marker(out, "MSBOOST_READY") != "1" {
		t.Fatalf("password login failed: %v %s", err, out)
	}
}

func TestSSHKeyValidationRules(t *testing.T) {
	_, edKey, _ := ed25519.GenerateKey(rand.Reader)
	key := privateKeyPEM(t, edKey, "")
	locked := privateKeyPEM(t, edKey, "secret phrase")
	base := SSH{Host: "8.8.8.8", Port: 22, User: "root", AuthMethod: "key", PrivateKey: key, Fingerprint: "SHA256:" + strings.Repeat("A", 43)}
	for name, mutate := range map[string]func(*SSH){
		"password with key": func(s *SSH) { s.Password = "x" },
		"empty key":         func(s *SSH) { s.PrivateKey = "" },
		"oversized key":     func(s *SSH) { s.PrivateKey = strings.Repeat("A", 16*1024+1) },
		"putty file":        func(s *SSH) { s.PrivateKey = "PuTTY-User-Key-File-3: ssh-ed25519\n" },
		"garbage": func(s *SSH) {
			s.PrivateKey = "-----BEGIN OPENSSH PRIVATE KEY-----\nAAAA\n-----END OPENSSH PRIVATE KEY-----\n"
		},
		"missing passphrase":     func(s *SSH) { s.PrivateKey = locked },
		"wrong passphrase":       func(s *SSH) { s.PrivateKey, s.Passphrase = locked, "wrong" },
		"unknown method":         func(s *SSH) { s.AuthMethod = "agent" },
		"key on password method": func(s *SSH) { s.AuthMethod, s.Password = "password", "x" },
	} {
		t.Run(name, func(t *testing.T) {
			s := base
			mutate(&s)
			if err := ValidateSSH(s); err == nil {
				t.Fatalf("accepted %s", name)
			}
		})
	}
	if err := ValidateSSH(base); err != nil {
		t.Fatal(err)
	}
	putty := base
	putty.PrivateKey = "PuTTY-User-Key-File-3: ssh-ed25519\n"
	if err := ValidateSSH(putty); err == nil || !strings.Contains(err.Error(), "PuTTYgen") {
		t.Fatalf("PuTTY guidance missing: %v", err)
	}
	if !strings.Contains(base.WithoutSecrets().PrivateKey+base.WithoutSecrets().Passphrase+base.WithoutSecrets().Password, "") || base.WithoutSecrets().PrivateKey != "" {
		t.Fatal("WithoutSecrets kept the key")
	}
}

func TestDDKeyLoginWritesOnlyThePublicKey(t *testing.T) {
	_, edKey, _ := ed25519.GenerateKey(rand.Reader)
	s := testSSH("8.8.8.8")
	s.AuthMethod, s.Password, s.PrivateKey = "key", "", privateKeyPEM(t, edKey, "")
	public, _ := ssh.NewPublicKey(edKey.Public())
	authorized := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(public)))
	for _, test := range []struct {
		name string
		dd   DDOptions
		ok   bool
	}{
		{"keep key", DDOptions{ConfirmErase: true, PortMode: "keep", PasswordMode: "keep", LoginMode: "key"}, true},
		{"switch to password", DDOptions{ConfirmErase: true, PortMode: "keep", PasswordMode: "new", NewPassword: "New-root-pass-1", LoginMode: "password"}, true},
		{"no login mode", DDOptions{ConfirmErase: true, PortMode: "keep", PasswordMode: "keep"}, false},
		{"key with new password", DDOptions{ConfirmErase: true, PortMode: "keep", PasswordMode: "new", NewPassword: "x", LoginMode: "key"}, false},
		{"password mode without password", DDOptions{ConfirmErase: true, PortMode: "keep", PasswordMode: "keep", LoginMode: "password"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			dd := test.dd
			request := Request{Kind: "dd", SSH: s, DD: &dd}
			if err := ValidateRequest(request); (err == nil) != test.ok {
				t.Fatalf("validation %v", err)
			}
			if !test.ok {
				return
			}
			remote := &fakeRemote{run: func(SSH, string) ([]byte, error) {
				return []byte("MSBOOST_PREPARED=1\n"), errors.New("stop before reboot")
			}}
			(&Engine{Remote: remote}).Execute(context.Background(), Job{ID: "dd-key", Request: request, Script: testAsset()})
			script := remote.scripts[0]
			if strings.Contains(script, "PRIVATE KEY") || strings.Contains(script, base64.StdEncoding.EncodeToString([]byte(s.PrivateKey))) {
				t.Fatal("private key sent to the reinstall script")
			}
			if dd.LoginMode == "key" {
				if !strings.Contains(script, `--ssh-key "$new_ssh_key"`) || strings.Contains(script, "--password") || !strings.Contains(script, encodedAssignment("new_ssh_key", authorized)) {
					t.Fatal("current key not written as the only login")
				}
			} else if !strings.Contains(script, `--password "$new_password"`) || strings.Contains(script, "--ssh-key") || !strings.Contains(script, encodedAssignment("new_password", "New-root-pass-1")) {
				t.Fatal("password login not set")
			}
		})
	}
	password := testSSH("8.8.8.8")
	if err := ValidateRequest(Request{Kind: "dd", SSH: password, DD: &DDOptions{ConfirmErase: true, PortMode: "keep", PasswordMode: "keep", LoginMode: "key"}}); err == nil {
		t.Fatal("password login allowed a key reinstall")
	}
}

func TestRelayRequestValidation(t *testing.T) {
	meta := &RelayMeta{Source: "code", CodeTail: "2F6H", User: "10086123@qq.com", Panel: "https://msboost.de", Remark: "公会群"}
	valid := Request{Kind: "relay-code", SSH: testSSH("8.8.8.8"), ClientConfig: []byte(sampleConfig), RateMbps: 7, Meta: meta}
	if err := ValidateRequest(valid); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Request){
		"rate above 10":             func(r *Request) { r.RateMbps = 11 },
		"code without rate":         func(r *Request) { r.RateMbps = 0 },
		"code without record":       func(r *Request) { r.Meta = nil },
		"bad tail":                  func(r *Request) { m := *meta; m.CodeTail = "2f6h"; r.Meta = &m },
		"manual source on code":     func(r *Request) { m := *meta; m.Source, m.CodeTail = "manual", ""; r.Meta = &m },
		"user with newline":         func(r *Request) { m := *meta; m.User = "a@qq.com\nx"; r.Meta = &m },
		"remark over 60":            func(r *Request) { m := *meta; m.Remark = strings.Repeat("长", 61); r.Meta = &m },
		"management on install":     func(r *Request) { r.Manage = &RelayManage{Action: "delete", Items: []string{"abcr"}} },
		"rate on deploy":            func(r *Request) { r.Kind, r.Mode, r.ClientConfig = "deploy", "fresh", nil },
		"inventory with config":     func(r *Request) { r.Kind, r.RateMbps, r.Meta = "relay-inventory", 0, nil },
		"manage without parameters": func(r *Request) { r.Kind, r.RateMbps, r.Meta, r.ClientConfig = "relay-manage", 0, nil, nil },
	} {
		t.Run(name, func(t *testing.T) {
			r := valid
			mutate(&r)
			if err := ValidateRequest(r); err == nil {
				t.Fatalf("accepted %s", name)
			}
		})
	}
	manage := Request{Kind: "relay-manage", SSH: testSSH("8.8.8.8"), Manage: &RelayManage{Action: "rate", Items: []string{"abcr", "defr"}, RateMbps: 3}}
	if err := ValidateRequest(manage); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []RelayManage{{Action: "rate", Items: []string{"abcr"}}, {Action: "pause", Items: []string{"abcr"}, RateMbps: 3}, {Action: "reboot", Items: []string{"abcr"}}, {Action: "delete", Items: []string{"../etc"}}, {Action: "delete", Items: []string{"abcr", "abcr"}}, {Action: "delete"}} {
		m := bad
		if err := ValidateRequest(Request{Kind: "relay-manage", SSH: testSSH("8.8.8.8"), Manage: &m}); err == nil {
			t.Fatalf("accepted management %+v", bad)
		}
	}
}

func TestRelayCodeInstallCarriesRateAndRecord(t *testing.T) {
	var scripts []string
	remote := &fakeRemote{run: func(s SSH, script string) ([]byte, error) {
		if script == "uname -m\n" {
			return []byte("x86_64\n"), nil
		}
		if strings.Contains(script, "MSBOOST_RELAY_PORT") {
			scripts = append(scripts, script)
			if s.Host == "9.9.9.9" {
				return []byte("MSBOOST_ERROR_CODE=service_failed\nMSBOOST_ERROR_PHASE=service\n"), errors.New("front failed")
			}
			return []byte("MSBOOST_RELAY_PORT=23450\n"), nil
		}
		return []byte("MSBOOST_READY=1\n"), nil
	}}
	front := testSSH("9.9.9.9")
	meta := &RelayMeta{Source: "code", CodeTail: "2F6H", User: "10086123@qq.com", Panel: "https://msboost.de", Remark: "公会群"}
	e := &Engine{Remote: remote, TCPProbe: func(context.Context, string, int) string { return "reachable" }, GostAMD64URL: "https://github.com/go-gost/gost/releases/download/v3.3.0/gost.tar.gz", GostAMD64SHA256: strings.Repeat("a", 64)}
	result := e.Execute(context.Background(), Job{ID: "codejob", Request: Request{Kind: "relay-code", SSH: testSSH("1.1.1.1"), Front: &front, ClientConfig: []byte(sampleConfig), RateMbps: 7, Meta: meta, Remark: "朋友的中转"}})
	if result.State != "failed" || result.FailedRole != "front" {
		t.Fatalf("front failure not attributed: %+v", result)
	}
	if len(scripts) != 2 {
		t.Fatalf("expected relay and front installs, got %d", len(scripts))
	}
	relayScript, frontScript := scripts[0], scripts[1]
	for _, expected := range []string{encodedAssignment("rate_bytes", "875000"), encodedAssignment("meta_rate", "7"), encodedAssignment("meta_source", "code"), encodedAssignment("meta_code_tail", "2F6H"), encodedAssignment("meta_user", "10086123@qq.com"), encodedAssignment("meta_remark", "公会群")} {
		if !strings.Contains(relayScript, expected) {
			t.Fatalf("relay install lacks %s", expected)
		}
	}
	// The owner's private note never reaches the user's own front server.
	if strings.Contains(frontScript, encodedAssignment("meta_remark", "公会群")) || !strings.Contains(frontScript, encodedAssignment("meta_remark", "朋友的中转")) {
		t.Fatal("front record carries the owner's note")
	}
	// The owner-side relay was removed after the front failed.
	cleaned := false
	for _, script := range remote.scripts {
		cleaned = cleaned || (strings.Contains(script, encodedAssignment("task_id", "codejobr")) && strings.Contains(script, `rm -rf -- "$confdir"`) && !strings.Contains(script, "MSBOOST_RELAY_PORT"))
	}
	if !cleaned {
		t.Fatal("owner-side relay not cleaned after the front failed")
	}
	// Manual relays without a choice stay at 5 Mbps; the paid-route front has no limit or record.
	manual := e.Execute(context.Background(), Job{ID: "manualjob", Request: Request{Kind: "relay", SSH: testSSH("1.1.1.1"), ClientConfig: []byte(sampleConfig)}})
	if manual.State != "succeeded" || manual.FailedRole != "" || !strings.Contains(scripts[len(scripts)-1], encodedAssignment("rate_bytes", "625000")) || !strings.Contains(scripts[len(scripts)-1], encodedAssignment("meta_source", "")) {
		t.Fatalf("manual default rate: %+v", manual)
	}
	if strings.Contains(manual.Message, "7 Mbps") || !strings.Contains(manual.Message, "5 Mbps") {
		t.Fatal(manual.Message)
	}
}

func python3Path(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"python3", "python"} {
		if path, err := exec.LookPath(name); err == nil {
			if exec.Command(path, "-c", "import sys; sys.exit(0 if sys.version_info >= (3, 8) else 1)").Run() == nil {
				return path
			}
		}
	}
	t.Skip("Python 3.8+ unavailable")
	return ""
}

func heredocPython(script, after string) string {
	start := strings.Index(script, after)
	if start < 0 {
		return ""
	}
	body := script[start:]
	body = body[strings.Index(body, "<<'PY'\n")+len("<<'PY'\n"):]
	return body[:strings.Index(body, "\nPY\n")+1]
}

func TestRelayMetaWriterRecordsUserWithoutCredentials(t *testing.T) {
	python := python3Path(t)
	code := heredocPython(relayInstallScript, `if [ -n "$meta_source" ]; then`)
	if code == "" || !strings.Contains(code, "O_EXCL") {
		t.Fatal("meta.json writer missing")
	}
	directory := t.TempDir()
	path := filepath.Join(directory, "meta.json")
	cmd := exec.Command(python, "-I", "-c", code, path, "41872", "203.0.113.45", "23456", "8", "code", "K5NA", "55661234@qq.com", "https://msboost.de", "公会群")
	cmd.Env = append(os.Environ(), "PYTHONIOENCODING=utf-8")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record RelayRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	if record.Version != 1 || record.Source != "code" || record.CodeTail != "K5NA" || record.User != "55661234@qq.com" || record.Panel != "https://msboost.de" || record.Target != "203.0.113.45:23456" || record.Port != 41872 || record.RateMbps != 8 || record.Remark != "公会群" {
		t.Fatalf("record: %+v", record)
	}
	if _, err := time.Parse(time.RFC3339, record.CreatedAt); err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	_ = json.Unmarshal(raw, &fields)
	for key := range fields {
		switch key {
		case "version", "source", "codeTail", "user", "panel", "target", "port", "rateMbps", "remark", "createdAt":
		default:
			t.Fatalf("unexpected field %s", key)
		}
	}
	if runtime.GOOS != "windows" {
		if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
			t.Fatalf("mode %o", info.Mode().Perm())
		}
	}
	// A second write never replaces an existing record.
	if err := exec.Command(python, "-I", "-c", code, path, "1", "1.1.1.1", "1", "1", "manual", "", "x@qq.com", "", "").Run(); err == nil {
		t.Fatal("meta.json overwritten")
	}
	manual := filepath.Join(directory, "manual.json")
	if out, err := exec.Command(python, "-I", "-c", code, manual, "30017", "2606:4700::1", "443", "5", "manual", "", "10086123@qq.com", "", "东京中转").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	raw, _ = os.ReadFile(manual)
	if !strings.Contains(string(raw), `"target": "[2606:4700::1]:443"`) || strings.Contains(string(raw), "codeTail") {
		t.Fatalf("manual record: %s", raw)
	}
}

func TestRelayServerScriptsParse(t *testing.T) {
	python := python3Path(t)
	for name, code := range map[string]string{
		"inventory": heredocPython(relayInventoryScript, "python3 - <<'PY'"),
		"manage":    heredocPython(relayManageScript, `python3 - "$manage_action"`),
	} {
		if code == "" {
			t.Fatalf("%s python missing", name)
		}
		cmd := exec.Command(python, "-I", "-c", "import sys; compile(sys.stdin.read(), 'relay', 'exec')")
		cmd.Stdin = strings.NewReader(code)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s python syntax: %v %s", name, err, out)
		}
	}
	bash, err := exec.LookPath("bash")
	if err != nil && runtime.GOOS == "windows" {
		bash = "C:/Program Files/Git/bin/bash.exe"
		_, err = os.Stat(bash)
	}
	if err != nil {
		t.Skip("bash unavailable")
	}
	for name, script := range map[string]string{"check": relayCheckScript, "inventory": relayInventoryScript, "manage": "set -Eeuo pipefail\n" + relayManageScript, "install": relayInstallScript} {
		cmd := exec.Command(bash, "-n")
		cmd.Stdin = strings.NewReader(script)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s shell syntax: %v %s", name, err, out)
		}
	}
	// Every action takes the install lock and is limited to listed relays.
	if !strings.Contains(relayManageScript, "flock -w 30 8") || !strings.Contains(relayManageScript, "for name in items:") || strings.Contains(relayManageScript, "os.listdir(ROOT)") {
		t.Fatal("management must hold the lock and touch only the listed relays")
	}
	if strings.Contains(relayInventoryScript, "systemctl','restart") || strings.Contains(relayInventoryScript, "unlink") {
		t.Fatal("inventory must be read-only")
	}
}

func TestRelayInventoryAndManageResults(t *testing.T) {
	entries := []RelayEntry{
		{ID: "abcr", Role: "relay", Port: 41872, RateMbps: 8, Target: "1.1.1.1:12345", State: "running", Connections: 3, CreatedAt: 1, Record: &RelayRecord{Version: 1, Source: "code", CodeTail: "K5NA", User: "55661234@qq.com", Target: "203.0.113.45:23456", Port: 41872, RateMbps: 8, Remark: "公会群", CreatedAt: "2026-10-16T14:05:00Z"}},
		{ID: "oldr", Role: "relay", Port: 52210, RateMbps: 5, Target: "192.0.2.88:30000", State: "weird", CreatedAt: 2},
		{ID: "../x", Role: "relay", Port: 1, State: "running"},
		{ID: "badr", Role: "relay", Port: 70000, State: "running"},
		{ID: "fakr", Role: "relay", Port: 1000, State: "running", Record: &RelayRecord{Version: 9, Source: "code", User: "x"}},
	}
	raw, _ := json.Marshal(entries)
	remote := &fakeRemote{run: func(SSH, string) ([]byte, error) {
		return []byte("MSBOOST_OS=Debian 12\nMSBOOST_INVENTORY=" + base64.StdEncoding.EncodeToString(raw) + "\n"), nil
	}}
	out := (&Engine{Remote: remote}).Execute(context.Background(), Job{ID: "inv", Request: Request{Kind: "relay-inventory", SSH: testSSH("8.8.8.8")}})
	if out.State != "succeeded" || out.OS != "Debian 12" || len(out.Inventory) != 3 {
		t.Fatalf("inventory: %+v", out)
	}
	if out.Inventory[0].CreatedAt != time.Date(2026, 10, 16, 14, 5, 0, 0, time.UTC).UnixMilli() || out.Inventory[1].State != "stopped" || out.Inventory[1].Record != nil || out.Inventory[2].Record != nil {
		t.Fatalf("inventory not sanitized: %+v", out.Inventory)
	}
	managed, _ := json.Marshal([]RelayManageResult{{ID: "defr", OK: true}, {ID: "abcr", OK: false, Error: "<script>"}})
	remote = &fakeRemote{run: func(_ SSH, script string) ([]byte, error) {
		if !strings.Contains(script, encodedAssignment("manage_items", "abcr defr ghir")) || !strings.Contains(script, encodedAssignment("manage_action", "pause")) {
			t.Fatal("management script lacks its exact items")
		}
		return []byte("MSBOOST_MANAGE=" + base64.StdEncoding.EncodeToString(managed) + "\n"), nil
	}}
	out = (&Engine{Remote: remote}).Execute(context.Background(), Job{ID: "mgr", Request: Request{Kind: "relay-manage", SSH: testSSH("8.8.8.8"), Manage: &RelayManage{Action: "pause", Items: []string{"abcr", "defr", "ghir"}}}})
	if out.State != "succeeded" || len(out.Managed) != 3 || out.Managed[0] != (RelayManageResult{ID: "abcr", Error: "failed"}) || out.Managed[1] != (RelayManageResult{ID: "defr", OK: true}) || out.Managed[2] != (RelayManageResult{ID: "ghir", Error: "failed"}) {
		t.Fatalf("manage results: %+v", out.Managed)
	}
	check := &fakeRemote{run: func(_ SSH, script string) ([]byte, error) {
		if script != relayCheckScript {
			t.Fatal("check must run only the read-only script")
		}
		return []byte("MSBOOST_READY=1\nMSBOOST_OS=Debian 13\n"), nil
	}}
	out = (&Engine{Remote: check}).Execute(context.Background(), Job{ID: "chk", Request: Request{Kind: "relay-check", SSH: testSSH("8.8.8.8")}})
	if out.State != "succeeded" || out.OS != "Debian 13" {
		t.Fatalf("check: %+v", out)
	}
	// A success the panel would reject is reported as a failed check instead.
	versionless := &fakeRemote{run: func(SSH, string) ([]byte, error) { return []byte("MSBOOST_READY=1\n"), nil }}
	out = (&Engine{Remote: versionless}).Execute(context.Background(), Job{ID: "chk2", Request: Request{Kind: "relay-check", SSH: testSSH("8.8.8.8")}})
	if out.State != "failed" || out.ErrorCode != "unsupported_debian" || out.OS != "" {
		t.Fatalf("versionless check: %+v", out)
	}
}

func TestRelayCleanupAcceptsRecordAndPauseMarker(t *testing.T) {
	if !strings.Contains(cleanupPython, "'meta.json'") || !strings.Contains(cleanupPython, "'paused'") {
		t.Fatal("cleanup would refuse relays with a user record or pause marker")
	}
}
