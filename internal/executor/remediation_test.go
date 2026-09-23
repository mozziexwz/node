package executor

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
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

func TestDiagnosticsClosedVocabularyAndNoRemoteSecrets(t *testing.T) {
	for _, tc := range []struct {
		err          error
		output, want string
	}{
		{diagnosticError("ssh_auth"), "password=not-for-users", "ssh_auth"},
		{diagnosticError("ssh_host_changed"), "private-key", "ssh_host_changed"},
		{errors.New("ssh-secret=private"), "MSBOOST_ERROR_CODE=download_failed\nMSBOOST_ERROR_PHASE=download\nprivate-key", "download_failed"},
		{errors.New("secret"), "MSBOOST_ERROR_CODE=secret\nMSBOOST_ERROR_PHASE=secret", "execution_failed"},
		{context.DeadlineExceeded, "", "task_timeout"},
	} {
		result := failRemote(Result{}, "prepare", []byte(tc.output), tc.err)
		if result.ErrorCode != tc.want || result.NextStep == "" {
			t.Fatalf("incorrect diagnostic: %+v", result)
		}
		data, _ := json.Marshal(result)
		if strings.Contains(string(data), "secret") || strings.Contains(string(data), "private-key") || strings.Contains(string(data), "not-for-users") {
			t.Fatal("remote diagnostic leaked secret")
		}
	}
	if strings.Contains(PublicDiagnostic("ssh_auth", "").Message, "密码错误") {
		t.Fatal("auth failure cannot prove wrong password")
	}
}

func TestEmbeddedTaskShellSyntax(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil && runtime.GOOS == "windows" {
		bash = "C:/Program Files/Git/bin/bash.exe"
		if _, err = os.Stat(bash); err != nil {
			t.Skip("Bash not installed")
		}
	}
	if bash == "" {
		t.Fatal("Bash required for installer shell syntax checks")
	}
	for name, script := range map[string]string{"diagnostics": "set -Eeuo pipefail\n" + diagnosticPrelude, "relay": "set -Eeuo pipefail\n" + diagnosticPrelude + relayInstallScript, "cleanup": "set -Eeuo pipefail\n" + diagnosticPrelude + "python3 - <<'MSBOOST_CLEANUP_PY'\n" + cleanupPython + "\nMSBOOST_CLEANUP_PY\n"} {
		cmd := exec.Command(bash, "-n")
		cmd.Stdin = strings.NewReader(script)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s syntax: %s %v", name, out, err)
		}
	}
}

func TestCleanupPythonSyntax(t *testing.T) {
	name := "python3"
	if runtime.GOOS == "windows" {
		name = "python"
	}
	python, err := exec.LookPath(name)
	if err != nil {
		if runtime.GOOS == "windows" {
			t.Skip("Python not installed")
		}
		t.Fatal(err)
	}
	cmd := exec.Command(python, "-c", "import ast,sys;ast.parse(sys.stdin.read())")
	cmd.Stdin = strings.NewReader(cleanupPython)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cleanup Python syntax: %s %v", out, err)
	}
}

func TestRelayRemarkPreservesCredentials(t *testing.T) {
	remote := &fakeRemote{run: func(_ SSH, script string) ([]byte, error) {
		if script == "uname -m\n" {
			return []byte("x86_64"), nil
		}
		if strings.Contains(script, "MSBOOST_RELAY_PORT") {
			return []byte("MSBOOST_RELAY_PORT=30001\n"), nil
		}
		return []byte("MSBOOST_READY=1\n"), nil
	}}
	e := &Engine{Remote: remote, TCPProbe: func(context.Context, string, int) string { return "reachable" }, GostAMD64URL: "https://github.com/go-gost/gost/releases/download/v3.3.0/gost.tar.gz", GostAMD64SHA256: strings.Repeat("a", 64)}
	r := e.Execute(context.Background(), Job{ID: "remark", Request: Request{Kind: "relay", SSH: testSSH("1.1.1.1"), ClientConfig: []byte(sampleConfig), Remark: "东京 / 一线"}})
	info, err := ParseClientConfig(r.Config)
	if err != nil || info.Username != "node-auth" || info.Password != "node-secret-not-email" || info.Document["activeProfile"] != "东京 / 一线" {
		t.Fatalf("remark changed credentials or not applied: %+v %v", r, err)
	}
}

func TestCleanupResultRejectsUnboundedPathsAndKinds(t *testing.T) {
	r := &CleanupReport{Scope: "relay", Digest: strings.Repeat("a", 64), Items: []CleanupItem{{Path: "/etc/msboost-free/task1", Kind: "directory"}}}
	if !ValidCleanupReport(r, "relay", false) {
		t.Fatal("valid scoped report refused")
	}
	for _, p := range []string{"/", "/etc", "/etc/msboost-free/../../shadow", "/usr/local/bin/gost", "password=my-secret"} {
		r.Items[0].Path = p
		if ValidCleanupReport(r, "relay", false) {
			t.Fatalf("accepted %s", p)
		}
	}
}

// Execute the real embedded Python in a disposable fixture tree. Every absolute
// production path is rewritten, and subprocess is replaced so no host service or
// firewall command can execute. Linux CI exercises symlink and filesystem rules.
func TestCleanupPythonIsolatedInventoryAndMutation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Linux filesystem/UID/symlink semantics exercised in CI")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"preview", "cleanup", "old-v030-cleanup", "legacy-cleanup", "credential-mismatch-load", "credential-mismatch-exec", "credential-other-name", "changed", "unknown", "symlink", "dropin", "stop-hook", "continued-hook", "propagated-stop", "also-unit", "failure-trigger", "new-section"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			write := func(path, data string) {
				p := filepath.Join(root, path)
				if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			conf := "etc/msboost-free/task1"
			unit := "etc/systemd/system/msboost-free-task1.service"
			write(conf+"/config.json", `{"services":[{"name":"msboost-free","addr":":30001"}]}`)
			write(conf+"/ufw-owned", "30001\n")
			unitText := cleanupUnitFixture(t, root, "relay")
			if scenario == "old-v030-cleanup" {
				unitText = strings.Replace(unitText, "-C ${CREDENTIALS_DIRECTORY}/config.json", "-C %d/config.json", 1)
			}
			if scenario == "legacy-cleanup" {
				unitText = strings.Replace(unitText, "LoadCredential=config.json:", "LoadCredential=config:", 1)
				unitText = strings.Replace(unitText, "-C ${CREDENTIALS_DIRECTORY}/config.json", "-C %d/config", 1)
			}
			write(unit, unitText)
			write("usr/local/libexec/msboost-free/gost-"+strings.Repeat("a", 64), "fixture binary")
			write("usr/local/bin/gost", "unrelated third party")
			write("run/placeholder", "")
			script := cleanupPython
			for _, prefix := range []string{"/etc/", "/usr/", "/run/", "/root/", "/var/"} {
				script = strings.ReplaceAll(script, prefix, root+prefix)
			}
			// The fixture uses the CI user's UID; production code still requires root.
			script = strings.ReplaceAll(script, "os.geteuid()!=0", "False")
			script = strings.ReplaceAll(script, "{0}", "{os.getuid()}")
			mock := `original_check_output=subprocess.check_output
def fake_check_output(args,**kwargs):
    if 'DropInPaths' in args: return ''
    return ` + strconvQuote(root+"/etc/systemd/system/msboost-free-task1.service") + `+'\n'
subprocess.check_output=fake_check_output
def fake_run(args,**kwargs):
    with open(` + strconvQuote(root+"/calls") + `, 'a') as f: f.write(json.dumps(args)+'\n')
subprocess.run=fake_run
`
			script = strings.Replace(script, "scope,expected,action=sys.argv[1:]", mock+"\nscope,expected,action=sys.argv[1:]", 1)
			run := func(action, digest string) ([]byte, error) {
				cmd := exec.Command(python, "-", "relay", digest, action)
				cmd.Stdin = strings.NewReader(script)
				return cmd.CombinedOutput()
			}
			out, err := run("cleanup-preview", "")
			if err != nil {
				t.Fatalf("preview: %s %v", out, err)
			}
			raw, _ := base64.StdEncoding.DecodeString(marker(out, "MSBOOST_CLEANUP"))
			var report CleanupReport
			if json.Unmarshal(raw, &report) != nil || len(report.Items) != 3 || report.Removed {
				t.Fatalf("invalid preview %s", out)
			}
			if _, err := os.Stat(filepath.Join(root, "calls")); !os.IsNotExist(err) {
				t.Fatal("preview modified services")
			}
			if scenario == "preview" {
				return
			}
			switch scenario {
			case "credential-mismatch-load", "credential-mismatch-exec", "credential-other-name":
				text := unitText
				switch scenario {
				case "credential-mismatch-load":
					text = strings.Replace(text, "LoadCredential=config.json:", "LoadCredential=config:", 1)
				case "credential-mismatch-exec":
					text = strings.Replace(text, "-C ${CREDENTIALS_DIRECTORY}/config.json", "-C ${CREDENTIALS_DIRECTORY}/config", 1)
				case "credential-other-name":
					text = strings.Replace(text, "LoadCredential=config.json:", "LoadCredential=other.json:", 1)
					text = strings.Replace(text, "-C ${CREDENTIALS_DIRECTORY}/config.json", "-C ${CREDENTIALS_DIRECTORY}/other.json", 1)
				}
				write(unit, text)
			case "changed":
				write(conf+"/config.json", `{"services":[{"name":"msboost-free","addr":":30002"}]}`)
				write(conf+"/ufw-owned", "30002\n")
			case "unknown":
				write(conf+"/other-app-secret", "do not delete")
			case "symlink":
				if err := os.Symlink(filepath.Join(root, "usr/local/bin/gost"), filepath.Join(root, conf, "managed-by")); err != nil {
					t.Fatal(err)
				}
			case "dropin":
				write(unit+".d/override.conf", "third party override")
			case "stop-hook":
				data, _ := os.ReadFile(filepath.Join(root, unit))
				write(unit, string(data)+"  ExecStop = /bin/unsafe\n")
			case "continued-hook":
				data, _ := os.ReadFile(filepath.Join(root, unit))
				write(unit, string(data)+"ExecStop=\\\n/bin/unsafe\n")
			case "propagated-stop", "also-unit", "failure-trigger", "new-section":
				data, _ := os.ReadFile(filepath.Join(root, unit))
				text := string(data)
				switch scenario {
				case "propagated-stop":
					text = strings.Replace(text, "[Unit]\n", "[Unit]\nPropagatesStopTo=ssh.service\n", 1)
				case "also-unit":
					text = strings.Replace(text, "[Install]\n", "[Install]\nAlso=ssh.service\n", 1)
				case "failure-trigger":
					text = strings.Replace(text, "[Unit]\n", "[Unit]\nOnFailure=ssh.service\n", 1)
				case "new-section":
					text += "\n[Socket]\nListenStream=22\n"
				}
				write(unit, text)
			}
			out, err = run("cleanup", report.Digest)
			if scenario == "cleanup" || scenario == "old-v030-cleanup" || scenario == "legacy-cleanup" {
				if err != nil {
					t.Fatalf("cleanup: %s %v", out, err)
				}
				if _, err := os.Stat(filepath.Join(root, conf)); !os.IsNotExist(err) {
					t.Fatal("owned directory remains")
				}
				if _, err := os.Stat(filepath.Join(root, unit)); !os.IsNotExist(err) {
					t.Fatal("owned unit remains")
				}
			} else {
				if err == nil {
					t.Fatalf("unsafe %s accepted: %s", scenario, out)
				}
				if strings.HasPrefix(scenario, "credential-") && marker(out, "MSBOOST_ERROR_CODE") != "ownership_failed" {
					t.Fatalf("mismatched credential pair must fail full ownership check: %s", out)
				}
				if _, err := os.Stat(filepath.Join(root, unit)); err != nil {
					t.Fatal("rejected cleanup mutated files")
				}
				if _, err := os.Stat(filepath.Join(root, "calls")); !os.IsNotExist(err) {
					t.Fatal("rejected cleanup mutated services")
				}
			}
			data, err := os.ReadFile(filepath.Join(root, "usr/local/bin/gost"))
			if err != nil || string(data) != "unrelated third party" {
				t.Fatal("third party binary affected")
			}
		})
	}
}
func strconvQuote(s string) string { b, _ := json.Marshal(s); return string(b) }

func TestCleanupPythonMSBOOSTOwnershipAndRetainedData(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Linux filesystem and ownership semantics exercised in CI")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"owned", "no-firewall", "unknown", "symlink", "unowned", "changed-cache", "also-unit", "propagated-stop"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			write := func(path, data string) {
				p := filepath.Join(root, path)
				if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			owned := "1"
			if scenario == "no-firewall" {
				owned = "0"
			}
			write("etc/msboost/install.env", "MANAGED_BY=msboost-installer-v1\nUSER_NAME=node\nUSER_PASS=private-password\nPORT=30001\nFIREWALL_PORT=30001\nFIREWALL_UFW_OWNED="+owned+"\nFIREWALLD_RUNTIME_OWNED=0\nFIREWALLD_PERMANENT_OWNED=0\n")
			write("etc/msboost/config.yaml", "private configuration")
			write("etc/msboost/ruleset/msboost-direct.yaml", "owned rules")
			write("var/lib/msboost/ruleset/msboost-filter.mrs", "owned data")
			write("var/lib/msboost/cache.db", "cache snapshot")
			_ = os.Chmod(filepath.Join(root, "var/lib/msboost/ruleset"), 0770)
			write("usr/local/bin/msboost", "managed binary")
			write("etc/systemd/system/msboost.service", cleanupUnitFixture(t, root, "msboost"))
			write("root/直连.json", `{"profiles":[{"user":{"name":"node","password":"private-password"}}]}`)
			write("var/backups/msboost/backup", "keep backup")
			write("usr/local/bin/gost", "keep third-party binary")
			write("run/placeholder", "")
			script := cleanupPython
			for _, prefix := range []string{"/etc/", "/usr/", "/run/", "/root/", "/var/"} {
				script = strings.ReplaceAll(script, prefix, root+prefix)
			}
			script = strings.ReplaceAll(script, "os.geteuid()!=0", "False")
			script = strings.ReplaceAll(script, "{0}", "{os.getuid()}")
			script = strings.ReplaceAll(script, "{0,uid}", "{os.getuid(),uid}")
			script = strings.ReplaceAll(script, "{0,pwd.getpwnam('msboost').pw_uid}", "{os.getuid(),pwd.getpwnam('msboost').pw_uid}")
			mock := `import types
pwd.getpwnam=lambda name:types.SimpleNamespace(pw_uid=os.getuid(),pw_gid=os.getgid())
def fake_check_output(args,**kwargs):
    return '' if 'DropInPaths' in args else ` + strconvQuote(root+"/etc/systemd/system/msboost.service") + `+'\n'
subprocess.check_output=fake_check_output
def fake_run(args,**kwargs):
    with open(` + strconvQuote(root+"/calls") + `,'a') as f:f.write(json.dumps(args)+'\n')
subprocess.run=fake_run
`
			script = strings.Replace(script, "scope,expected,action=sys.argv[1:]", mock+"\nscope,expected,action=sys.argv[1:]", 1)
			run := func(action, digest string) ([]byte, error) {
				cmd := exec.Command(python, "-", "msboost", digest, action)
				cmd.Stdin = strings.NewReader(script)
				return cmd.CombinedOutput()
			}
			out, err := run("cleanup-preview", "")
			if err != nil {
				t.Fatalf("preview %s %v", out, err)
			}
			raw, _ := base64.StdEncoding.DecodeString(marker(out, "MSBOOST_CLEANUP"))
			var report CleanupReport
			if json.Unmarshal(raw, &report) != nil || len(report.Items) < 5 {
				t.Fatalf("bad preview %s", out)
			}
			if strings.Contains(string(out), "private-password") {
				t.Fatal("preview leaked credentials")
			}
			switch scenario {
			case "unknown":
				write("etc/msboost/third-party.conf", "keep unknown file")
			case "symlink":
				if err := os.Symlink(filepath.Join(root, "var/backups/msboost/backup"), filepath.Join(root, "etc/msboost/ruleset/extra")); err != nil {
					t.Fatal(err)
				}
			case "unowned":
				write("etc/msboost/install.env", "MANAGED_BY=third-party\n")
			case "changed-cache":
				write("var/lib/msboost/cache.db", "cache changed after preview")
			case "also-unit", "propagated-stop":
				unit := cleanupUnitFixture(t, root, "msboost")
				if scenario == "also-unit" {
					unit = strings.Replace(unit, "[Install]\n", "[Install]\nAlso=ssh.service\n", 1)
				} else {
					unit = strings.Replace(unit, "[Unit]\n", "[Unit]\nPropagatesStopTo=ssh.service\n", 1)
				}
				write("etc/systemd/system/msboost.service", unit)
			}
			out, err = run("cleanup", report.Digest)
			if scenario == "owned" || scenario == "no-firewall" {
				if err != nil {
					t.Fatalf("cleanup %s %v", out, err)
				}
				for _, path := range []string{"etc/msboost", "var/lib/msboost", "usr/local/bin/msboost", "etc/systemd/system/msboost.service", "root/直连.json"} {
					if _, err := os.Stat(filepath.Join(root, path)); !os.IsNotExist(err) {
						t.Fatalf("owned %s remains", path)
					}
				}
				calls, _ := os.ReadFile(filepath.Join(root, "calls"))
				if strings.Contains(string(calls), "userdel") || strings.Contains(string(calls), "groupdel") || strings.Contains(string(calls), "flush") {
					t.Fatal("broad cleanup command")
				}
				if strings.Contains(string(calls), "ufw") != (scenario == "owned") {
					t.Fatal("firewall ownership not respected")
				}
			} else {
				if err == nil {
					t.Fatalf("unsafe %s accepted", scenario)
				}
				if _, err := os.Stat(filepath.Join(root, "calls")); !os.IsNotExist(err) {
					t.Fatal("rejected cleanup mutated services")
				}
			}
			for _, path := range []string{"var/backups/msboost/backup", "usr/local/bin/gost"} {
				if _, err := os.Stat(filepath.Join(root, path)); err != nil {
					t.Fatalf("retained %s removed", path)
				}
			}
		})
	}
}

// Derive fixtures from the actual shipped installer templates so cleanup cannot
// accidentally accept only a simplified test unit but reject real installations.
func cleanupUnitFixture(t *testing.T, root, scope string) string {
	t.Helper()
	source := relayInstallScript
	begin := "cat > \"/etc/systemd/system/$unit\" <<EOF\n"
	if scope == "msboost" {
		data, err := os.ReadFile(filepath.Join("..", "..", "installers", "node", "msboost.sh"))
		if err != nil {
			t.Fatal(err)
		}
		source = strings.ReplaceAll(string(data), "\r\n", "\n")
		begin = "cat > \"${STAGE_DIR}/msboost.service\" <<EOF\n"
	}
	_, template, ok := strings.Cut(source, begin)
	if !ok {
		t.Fatal("unit template missing")
	}
	template, _, ok = strings.Cut(template, "\nEOF")
	if !ok {
		t.Fatal("unit template terminator missing")
	}
	template = strings.NewReplacer("${APP_USER}", "msboost", "${APP_GROUP}", "msboost", "${STATE_DIR}", "/var/lib/msboost", "${APP_DIR}", "/etc/msboost", "${BIN}", "/usr/local/bin/msboost", "${confdir}", "/etc/msboost-free/task1", "${gost_sha}", strings.Repeat("a", 64)).Replace(template)
	template = strings.ReplaceAll(template, "\\${CREDENTIALS_DIRECTORY}", "${CREDENTIALS_DIRECTORY}")
	for _, prefix := range []string{"/etc/", "/usr/", "/var/"} {
		template = strings.ReplaceAll(template, prefix, root+prefix)
	}
	return template + "\n"
}

func TestSSHSessionOpenHonorsContextCancellation(t *testing.T) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
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
	stopServer := make(chan struct{})
	defer close(stopServer)
	channelOpened := make(chan struct{})
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		server, channels, requests, err := ssh.NewServerConn(conn, serverConfig)
		if err != nil {
			return
		}
		defer server.Close()
		go ssh.DiscardRequests(requests)
		select {
		case _, ok := <-channels:
			if !ok {
				return
			}
			close(channelOpened)
		case <-stopServer:
			return
		}
		// Intentionally neither Accept nor Reject the authenticated channel-open.
		<-stopServer
	}()
	remote := SSHRemote{dialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
	}}
	connection := testSSH("8.8.8.8")
	connection.Fingerprint = ssh.FingerprintSHA256(signer.PublicKey())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() { _, err := remote.Run(ctx, connection, "true\n"); finished <- err }()
	select {
	case <-channelOpened:
	case <-time.After(3 * time.Second):
		t.Fatal("SSH never reached blocked channel-open")
	}
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation lost: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("NewSession ignored context cancellation")
	}
}
