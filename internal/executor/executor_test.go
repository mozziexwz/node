package executor

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

const sampleConfig = `{"profiles":[{"profileName":"msboost","user":{"name":"node-auth","password":"node-secret-not-email"},"servers":[{"ipAddress":"8.8.8.8","domainName":"","portBindings":[{"port":23456,"protocol":"TCP"}]}],"mtu":1400}],"activeProfile":"msboost","socks5Port":10086,"rpcPort":8964}`

func testSSH(host string) SSH {
	return SSH{Host: host, Port: 22, User: "root", Password: "ssh-secret", Fingerprint: "SHA256:" + strings.Repeat("A", 43)}
}
func testAsset() Asset {
	data := []byte("#!/bin/bash\nprintf 'fixture only\\n'\n")
	sum := sha256.Sum256(data)
	return Asset{Data: data, SHA256: hex.EncodeToString(sum[:])}
}

type fakeRemote struct {
	mu      sync.Mutex
	scripts []string
	hosts   []string
	run     func(SSH, string) ([]byte, error)
}

func (f *fakeRemote) Run(_ context.Context, s SSH, script string) ([]byte, error) {
	f.mu.Lock()
	f.scripts = append(f.scripts, script)
	f.hosts = append(f.hosts, s.Host)
	f.mu.Unlock()
	return f.run(s, script)
}
func (f *fakeRemote) Probe(context.Context, string, int) (string, string, error) {
	return "", "", errors.New("unused")
}
func TestPublicIPRejectsInternalAndMapped(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "10.0.0.1", "100.100.100.200", "169.254.169.254", "192.168.0.1", "192.0.2.1", "::1", "::ffff:127.0.0.1", "64:ff9b::a00:1", "fe80::1%eth0", "https://8.8.8.8", "8.8.8.8:22"} {
		if PublicIP(host) == nil {
			t.Errorf("accepted internal/invalid %s", host)
		}
	}
	for _, host := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		if err := PublicIP(host); err != nil {
			t.Errorf("rejected %s", host)
		}
	}
}
func TestClientConfigRejectsAmbiguityAndPreservesCredentials(t *testing.T) {
	out, err := RewriteClientConfig([]byte(sampleConfig), "1.1.1.1", 45678)
	if err != nil {
		t.Fatal(err)
	}
	info, err := ParseClientConfig(out)
	if err != nil {
		t.Fatal(err)
	}
	if info.TargetHost != "1.1.1.1" || info.TargetPort != 45678 || info.Username != "node-auth" || info.Password != "node-secret-not-email" {
		t.Fatal("rewriter changed credentials or target")
	}
	for _, bad := range []string{strings.Replace(sampleConfig, `"protocol":"TCP"`, `"protocol":"UDP"`, 1), strings.Replace(sampleConfig, `"domainName":""`, `"domainName":"example.com"`, 1), strings.Replace(sampleConfig, `"portBindings":[`, `"portBindings":[{"port":12345,"protocol":"TCP"},`, 1), strings.Replace(sampleConfig, `"profiles":[`, `"profiles":[{},`, 1)} {
		if _, err := ParseClientConfig([]byte(bad)); err == nil {
			t.Fatal("accepted ambiguous/invalid config")
		}
	}
}
func TestSSHHostKeyPinnedDuringHandshake(t *testing.T) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	serverConfig := &ssh.ServerConfig{NoClientAuth: true}
	serverConfig.AddHostKey(signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for i := 0; i < 2; i++ {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				server, _, _, err := ssh.NewServerConn(conn, serverConfig)
				if err == nil {
					server.Close()
				}
				conn.Close()
			}()
		}
	}()
	for _, test := range []struct {
		fp   string
		good bool
	}{{ssh.FingerprintSHA256(signer.PublicKey()), true}, {"SHA256:" + strings.Repeat("B", 43), false}} {
		conn, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		c, _, _, err := ssh.NewClientConn(conn, listener.Addr().String(), &ssh.ClientConfig{User: "root", HostKeyCallback: PinnedHostKey(test.fp)})
		if test.good && err != nil {
			t.Fatal(err)
		}
		if !test.good && err == nil {
			t.Fatal("changed host key accepted")
		}
		if c != nil {
			c.Close()
		}
		conn.Close()
	}
}
func TestDDRequiresPreparationAndRebootEvidence(t *testing.T) {
	for _, test := range []struct {
		name       string
		prepare    string
		prepareErr error
		commit     string
		commitErr  error
		state      string
	}{{"prepare failure", "", errors.New("auth failed"), "", nil, "failed"}, {"arbitrary disconnect", "MSBOOST_PREPARED=1\n", nil, "", &ssh.ExitMissingError{}, "unknown"}, {"ack without disconnect", "MSBOOST_PREPARED=1\n", nil, "MSBOOST_REBOOT_SUBMITTED=1\n", nil, "unknown"}, {"confirmed submission", "MSBOOST_PREPARED=1\n", nil, "MSBOOST_REBOOT_SUBMITTED=1\n", &ssh.ExitMissingError{}, "executed"}} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			remote := &fakeRemote{run: func(s SSH, script string) ([]byte, error) {
				calls++
				if calls == 1 {
					return []byte(test.prepare), test.prepareErr
				}
				return []byte(test.commit), test.commitErr
			}}
			e := &Engine{Remote: remote}
			request := Request{Kind: "dd", SSH: testSSH("8.8.8.8"), DD: &DDOptions{ConfirmErase: true, PortMode: "keep", PasswordMode: "keep"}}
			out := e.Execute(context.Background(), Job{ID: "test-dd", Request: request, Script: testAsset()})
			if out.State != test.state {
				t.Fatalf("state=%s want=%s", out.State, test.state)
			}
			if test.prepareErr != nil && calls != 1 {
				t.Fatal("reboot issued after failed preparation")
			}
			if strings.Contains(remote.scripts[0], request.SSH.Password) {
				t.Fatal("password included as literal shell text")
			}
			if strings.Contains(out.Message, request.SSH.Password) {
				t.Fatal("secret in ordinary task message")
			}
		})
	}
}
func TestFreshOnlyAndIntegrity(t *testing.T) {
	remote := &fakeRemote{run: func(_ SSH, script string) ([]byte, error) {
		if script == debianPreflightScript {
			return []byte("MSBOOST_READY=1\n"), nil
		}
		return nil, errors.New("stop before real install")
	}}
	e := &Engine{Remote: remote}
	e.Execute(context.Background(), Job{ID: "deploy", Request: Request{Kind: "deploy", SSH: testSSH("8.8.8.8"), Mode: "fresh"}, Script: testAsset()})
	if len(remote.scripts) != 2 || remote.scripts[0] != debianPreflightScript ||
		!strings.Contains(remote.scripts[1], `"$node_user" "$node_pass"`) ||
		!strings.Contains(remote.scripts[1], `MSBOOST_FORCE_FRESH=1`) {
		t.Fatal("fresh mode did not preflight and replace managed credentials/port")
	}
	rejected := &fakeRemote{run: func(SSH, string) ([]byte, error) { t.Fatal("repair reached SSH"); return nil, nil }}
	out := (&Engine{Remote: rejected}).Execute(context.Background(), Job{Request: Request{Kind: "deploy", Mode: "repair", SSH: testSSH("8.8.8.8")}, Script: testAsset()})
	if out.State != "failed" || out.ErrorCode != "invalid_request" {
		t.Fatal("legacy repair mode accepted", out)
	}
	remote = &fakeRemote{run: func(SSH, string) ([]byte, error) { t.Fatal("tampered script executed"); return nil, nil }}
	asset := testAsset()
	asset.Data = append(asset.Data, byte('x'))
	out = (&Engine{Remote: remote}).Execute(context.Background(), Job{Request: Request{Kind: "deploy", Mode: "fresh", SSH: testSSH("8.8.8.8")}, Script: asset})
	if out.State != "failed" || out.Phase != "integrity" {
		t.Fatal(out)
	}
}

func TestManagedFreshInstallerCannotReusePriorAuthOrPort(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "installers", "node", "msboost.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	for _, guard := range []string{
		`if [[ "${MSBOOST_FORCE_FRESH:-0}" != 1 && -z "${USER_NAME}" ]]; then`,
		`if [[ "${MSBOOST_FORCE_FRESH:-0}" != 1 && -z "${USER_PASS}" ]]; then`,
		`if [[ "${MSBOOST_FORCE_FRESH:-0}" != 1 && -z "${PORT}" ]]; then`,
		`"${candidate}" == "${OLD_CONFIG_PORT}"`,
	} {
		if !strings.Contains(script, guard) {
			t.Fatalf("managed fresh guard missing: %s", guard)
		}
	}
}

func TestDebianPreflightRejectsBeforeMutation(t *testing.T) {
	for _, kind := range []string{"deploy", "relay"} {
		t.Run(kind, func(t *testing.T) {
			remote := &fakeRemote{run: func(_ SSH, script string) ([]byte, error) {
				if script != debianPreflightScript {
					t.Fatal("unsupported OS reached mutating script")
				}
				return []byte("MSBOOST_ERROR_CODE=unsupported_debian\nMSBOOST_ERROR_PHASE=preflight\n"), errors.New("unsupported")
			}}
			request := Request{Kind: kind, SSH: testSSH("8.8.8.8"), Mode: "fresh"}
			if kind == "relay" {
				request.Mode = ""
				request.ClientConfig = []byte(sampleConfig)
			}
			result := (&Engine{Remote: remote}).Execute(context.Background(), Job{ID: "reject-os", Request: request, Script: testAsset()})
			if result.State != "failed" || result.ErrorCode != "unsupported_debian" || result.Phase != "preflight" || len(remote.scripts) != 1 {
				t.Fatal("OS not rejected before mutation", result, len(remote.scripts))
			}
			if result.Message != "当前服务器非 Debian 系统，请在 VPS 服务商面板重装 Debian 11 或以上版本系统后再次尝试。" {
				t.Fatal(result.Message)
			}
		})
	}
}
func TestFrontAddsHopWithoutSkippingRelay(t *testing.T) {
	remote := &fakeRemote{run: func(s SSH, script string) ([]byte, error) {
		if script == "uname -m\n" {
			return []byte("x86_64\n"), nil
		}
		if strings.Contains(script, "MSBOOST_RELAY_PORT") {
			if s.Host == "1.1.1.1" {
				return []byte("MSBOOST_RELAY_PORT=23450\n"), nil
			}
			if !strings.Contains(script, encodedAssignment("target_host", "1.1.1.1")) || !strings.Contains(script, encodedAssignment("target_port", "23450")) {
				t.Fatal("front skipped relay")
			}
			return []byte("MSBOOST_RELAY_PORT=45678\n"), nil
		}
		return []byte("MSBOOST_READY=1\n"), nil
	}}
	front := testSSH("9.9.9.9")
	e := &Engine{Remote: remote, TCPProbe: func(context.Context, string, int) string { return "reachable" }, GostAMD64URL: "https://github.com/go-gost/gost/releases/download/v3.2.6/gost.tar.gz", GostAMD64SHA256: strings.Repeat("a", 64)}
	result := e.Execute(context.Background(), Job{ID: "relaytest", Request: Request{Kind: "relay", SSH: testSSH("1.1.1.1"), Front: &front, ClientConfig: []byte(sampleConfig)}})
	if result.State != "succeeded" {
		t.Fatal(result)
	}
	if len(result.Hops) != 2 || result.Hops[1].ToHost != "1.1.1.1" || result.Hops[1].ToPort != 23450 {
		t.Fatal("wrong hop mapping")
	}
	info, err := ParseClientConfig(result.Config)
	if err != nil || info.TargetHost != "9.9.9.9" || info.TargetPort != 45678 || info.Password != "node-secret-not-email" {
		t.Fatal("wrong client entry or credentials")
	}
	if !strings.Contains(relayInstallScript, "'$ 625000B 625000B'") {
		t.Fatal("missing aggregate 5Mbps limiter")
	}
	if !strings.Contains(relayInstallScript, "/proc/net/") {
		t.Fatal("must confirm service-owned bound socket")
	}
}
func TestShellPayloadInjectionIsEncoded(t *testing.T) {
	danger := "'; touch /tmp/unexpected; $(cat /etc/shadow) `id`"
	line := encodedAssignment("password", danger)
	if strings.Contains(line, danger) || strings.Contains(line, "/etc/shadow") || strings.Contains(line, "`id`") {
		t.Fatal("user input entered shell source")
	}
}
