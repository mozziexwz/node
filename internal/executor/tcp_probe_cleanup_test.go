package executor

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestTCPProbeCleanupReportInventory(t *testing.T) {
	report := &CleanupReport{Scope: "msboost", Digest: strings.Repeat("a", 64)}
	for _, path := range []string{"/etc/msboost/relay_tcp_probe.py", "/etc/msboost/tcp-probe.json", "/etc/systemd/system/msboost-tcp-probe.service"} {
		report.Items = []CleanupItem{{Kind: "file", Path: path}}
		if !ValidCleanupReport(report, "msboost", false) {
			t.Fatalf("managed TCP probe resource missing from preview whitelist: %s", path)
		}
		if ValidCleanupReport(report, "relay", false) {
			t.Fatal("TCP probe crossed cleanup scope")
		}
	}
	for _, path := range []string{"/etc/msboost/relay_tcp_probe.py.bak", "/etc/msboost/__pycache__", "/etc/systemd/system/msboost-tcp-probe.service.d", "/etc/systemd/system/another-probe.service"} {
		report.Items = []CleanupItem{{Kind: "file", Path: path}}
		if ValidCleanupReport(report, "msboost", false) {
			t.Fatalf("unknown probe resource accepted: %s", path)
		}
	}
}

// Execute the actual embedded cleanup code with real fixture file reads, hashes,
// inventory and removals. Only Linux ownership metadata, locks and service calls
// are modeled, so this test also runs on Windows without invoking host services.
func TestTCPProbeCleanupPythonFixture(t *testing.T) {
	pythonName := "python3"
	if runtime.GOOS == "windows" {
		pythonName = "python"
	}
	python, err := exec.LookPath(pythonName)
	if err != nil {
		t.Fatal(err)
	}
	installer, err := os.ReadFile(filepath.Join("..", "..", "installers", "node", "msboost.sh"))
	if err != nil {
		t.Fatal(err)
	}
	source := strings.ReplaceAll(string(installer), "\r\n", "\n")
	probeSource := tcpProbeHeredoc(t, source, "relay_tcp_probe.py")
	probeConfig := tcpProbeHeredoc(t, source, "tcp-probe.json")
	probeUnit := tcpProbeHeredoc(t, source, "msboost-tcp-probe.service")
	sourceHash := sha256.Sum256([]byte(probeSource))
	if !strings.Contains(cleanupPython, "probe_source_sha256='"+hex.EncodeToString(sourceHash[:])+"'") {
		t.Fatal("cleanup identity does not match the installer TCP probe source")
	}
	for _, scenario := range []string{"owned", "legacy", "changed-config", "source-changed", "unknown-file", "missing-source", "missing-unit", "config-duplicate", "config-bool", "config-float", "config-listener", "config-unknown", "source-owner", "source-group", "source-mode", "source-hardlink", "config-owner", "unit-owner", "unit-group", "unit-mode", "source-symlink", "unit-symlink", "parent-symlink", "dropin", "active-dropin", "fragment", "unit-stop-hook", "unit-also", "unit-capability", "unit-extra-section", "main-other-dependency", "enable-link", "persistent-and-runtime", "changed-enable-link", "changed-runtime-link", "enable-parent-symlink", "unknown-enable-link", "alias-other-name", "runtime-mask", "root-account", "root-group"} {
		t.Run(scenario, func(t *testing.T) {
			root := filepath.ToSlash(t.TempDir())
			write := func(path, data string) {
				p := root + "/" + path
				if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			mainUnit := cleanupUnitFixture(t, root, "msboost")
			unit := strings.NewReplacer("${APP_USER}", "msboost", "${APP_GROUP}", "msboost", "${APP_DIR}", "/etc/msboost", "${PROBE_SOURCE}", "/etc/msboost/relay_tcp_probe.py", "${PROBE_CONFIG}", "/etc/msboost/tcp-probe.json").Replace(probeUnit)
			for _, prefix := range []string{"/etc/", "/usr/", "/var/"} {
				unit = strings.ReplaceAll(unit, prefix, root+prefix)
			}
			write("etc/msboost/install.env", "MANAGED_BY=msboost-installer-v1\nUSER_NAME=node\nUSER_PASS=private-password\nFIREWALL_PORT=30001\nFIREWALL_UFW_OWNED=1\nFIREWALLD_RUNTIME_OWNED=0\nFIREWALLD_PERMANENT_OWNED=0\n")
			write("etc/msboost/config.yaml", "private config")
			write("etc/msboost/relay_tcp_probe.py", probeSource)
			write("etc/msboost/tcp-probe.json", probeConfig)
			write("etc/systemd/system/msboost.service", mainUnit)
			write("etc/systemd/system/msboost-tcp-probe.service", unit)
			write("var/lib/msboost/cache.db", "owned cache")
			write("usr/local/bin/msboost", "owned binary")
			write("var/backups/msboost/retained", "retain backup")
			write("usr/local/bin/unrelated", "retain third-party")
			write("run/placeholder", "")
			write("fixture-metadata.json", "{}")
			script := cleanupPython
			for _, prefix := range []string{"/etc/", "/usr/", "/run/", "/root/", "/var/"} {
				script = strings.ReplaceAll(script, prefix, root+prefix)
			}
			// fcntl is unavailable on Windows; root, passwd and stat metadata are
			// explicit fixture inputs while production checks remain unchanged.
			preamble := "import sys,types\nsys.modules['fcntl']=types.SimpleNamespace(LOCK_EX=1,LOCK_NB=2,flock=lambda *args:None)\nsys.modules['pwd']=types.SimpleNamespace()\n"
			mock := `import types
fixture_root=` + strconvQuote(root) + `
os.geteuid=lambda:0
os.O_NOFOLLOW=getattr(os,'O_NOFOLLOW',0)
pwd.getpwnam=lambda name:types.SimpleNamespace(pw_uid=0 if metadata().get('root-account') else 2000,pw_gid=0 if metadata().get('root-group') else 2050)
real_lstat=os.lstat
real_islink=os.path.islink
real_readlink=os.readlink
real_abspath=os.path.abspath
real_realpath=os.path.realpath
real_walk=os.walk
os.path.abspath=lambda path:real_abspath(path).replace('\\','/')
def fake_walk(*args,**kwargs):
    for directory,dirs,files in real_walk(*args,**kwargs):yield directory.replace('\\','/'),dirs,files
os.walk=fake_walk
def metadata():
    with open(fixture_root+'/fixture-metadata.json') as f:return json.load(f)
def fake_lstat(path,*args,**kwargs):
    info=real_lstat(path,*args,**kwargs)
    path=os.fspath(path).replace('\\','/')
    relative=path.removeprefix(fixture_root+'/')
    mode=0o700 if stat.S_ISDIR(info.st_mode) else 0o600
    gid=0
    if relative in ['etc/msboost/relay_tcp_probe.py','etc/msboost/tcp-probe.json']:mode=0o640;gid=2050
    if relative=='etc/systemd/system/msboost-tcp-probe.service':mode=0o644
    override=metadata().get(relative,{})
    return types.SimpleNamespace(st_mode=stat.S_IFMT(info.st_mode)|override.get('mode',mode),st_uid=override.get('uid',0),st_gid=override.get('gid',gid),st_size=info.st_size,st_nlink=override.get('nlink',1),st_ino=info.st_ino,st_dev=info.st_dev,st_file_attributes=getattr(info,'st_file_attributes',0),st_reparse_tag=getattr(info,'st_reparse_tag',0))
os.lstat=fake_lstat
def fake_islink(path):
    relative=os.fspath(path).replace('\\','/').removeprefix(fixture_root+'/')
    return metadata().get(relative,{}).get('symlink',False) or real_islink(path)
os.path.islink=fake_islink
def fake_readlink(path,*args,**kwargs):
    relative=os.fspath(path).replace('\\','/').removeprefix(fixture_root+'/')
    if 'readlink' in metadata().get(relative,{}):return metadata()[relative]['readlink']
    return real_readlink(path,*args,**kwargs)
os.readlink=fake_readlink
def fake_realpath(path,*args,**kwargs):
    normalized=os.path.abspath(path)
    relative=normalized.removeprefix(fixture_root+'/')
    if 'readlink' in metadata().get(relative,{}):
        return fake_realpath(os.path.join(os.path.dirname(normalized),metadata()[relative]['readlink']))
    return real_realpath(path,*args,**kwargs).replace('\\','/')
os.path.realpath=fake_realpath
def fake_check_output(args,**kwargs):
    if 'DropInPaths' in args:return 'unknown.conf' if metadata().get('active-dropin') and args[2]=='msboost-tcp-probe.service' else ''
    if metadata().get('fragment') and args[2]=='msboost-tcp-probe.service':return '/unrelated.service\n'
    return fixture_root+'/etc/systemd/system/'+args[2]+'\n'
subprocess.check_output=fake_check_output
def fake_run(args,**kwargs):
    if args[:3]==['systemctl','disable','--now']:
        # Neither service may be stopped after its source has been removed.
        assert os.path.isfile(fixture_root+'/etc/msboost/relay_tcp_probe.py') or metadata().get('legacy')
    if args[:2]==['systemctl','disable']:
        target='multi-user.target' if args[-1]=='msboost.service' else 'msboost.service'
        prefix='/run' if '--runtime' in args else '/etc'
        link=fixture_root+prefix+'/systemd/system/'+target+'.wants/'+args[-1]
        if os.path.exists(link):os.unlink(link)
    with open(fixture_root+'/calls','a') as f:f.write(json.dumps(args)+'\n')
subprocess.run=fake_run
`
			script = preamble + strings.Replace(script, "scope,expected,action=sys.argv[1:4]", mock+"\nscope,expected,action=sys.argv[1:4]", 1)
			script = strings.ReplaceAll(script, "except Rejected:\n", "except Rejected:\n    import traceback;traceback.print_exc()\n")
			script = strings.ReplaceAll(script, "except Exception:\n", "except Exception:\n    import traceback;traceback.print_exc()\n")
			script = strings.ReplaceAll(script, "if key in parsed[section] or value!=expected[section][key]: reject()", "if key in parsed[section] or value!=expected[section][key]: raise Rejected((key,value,expected[section][key]))")
			run := func(action, digest string) ([]byte, error) {
				cmd := exec.Command(python, "-", "msboost", digest, action)
				cmd.Stdin = strings.NewReader(script)
				return cmd.CombinedOutput()
			}
			out, err := run("cleanup-preview", "")
			if err != nil {
				t.Fatalf("healthy preview: %s %v", out, err)
			}
			var preview CleanupReport
			raw, _ := base64.StdEncoding.DecodeString(marker(out, "MSBOOST_CLEANUP"))
			if json.Unmarshal(raw, &preview) != nil || preview.Removed || len(preview.Items) != 8 {
				t.Fatalf("invalid preview: %s", out)
			}
			for _, path := range []string{"etc/msboost/relay_tcp_probe.py", "etc/msboost/tcp-probe.json", "etc/systemd/system/msboost-tcp-probe.service", "etc/systemd/system/msboost.service"} {
				found := false
				for _, item := range preview.Items {
					found = found || item.Path == root+"/"+path && item.Kind == "file"
				}
				if !found {
					t.Fatalf("preview omitted lifecycle/resource %s", path)
				}
			}
			if _, err := os.Stat(root + "/calls"); !os.IsNotExist(err) {
				t.Fatal("preview mutated services")
			}
			meta := map[string]any{}
			switch scenario {
			case "legacy":
				for _, path := range []string{"etc/msboost/relay_tcp_probe.py", "etc/msboost/tcp-probe.json", "etc/systemd/system/msboost-tcp-probe.service"} {
					if err := os.Remove(root + "/" + path); err != nil {
						t.Fatal(err)
					}
				}
				write("etc/systemd/system/msboost.service", strings.ReplaceAll(mainUnit, "Wants=network-online.target msboost-tcp-probe.service", "Wants=network-online.target"))
				meta["legacy"] = true
			case "changed-config":
				write("etc/msboost/tcp-probe.json", " \n"+probeConfig)
			case "source-changed":
				write("etc/msboost/relay_tcp_probe.py", probeSource+"# unrelated source alteration\n")
			case "unknown-file":
				write("etc/msboost/third-party.conf", "retain third-party")
			case "missing-source":
				_ = os.Remove(root + "/etc/msboost/relay_tcp_probe.py")
			case "missing-unit":
				_ = os.Remove(root + "/etc/systemd/system/msboost-tcp-probe.service")
			case "config-duplicate":
				write("etc/msboost/tcp-probe.json", strings.Replace(probeConfig, "{", `{"listen_port":20424,`, 1))
			case "config-bool", "config-float", "config-listener", "config-unknown":
				var document map[string]any
				if err := json.Unmarshal([]byte(probeConfig), &document); err != nil {
					t.Fatal(err)
				}
				switch scenario {
				case "config-bool":
					document["connect_timeout"] = true
				case "config-float":
					document["connect_timeout"] = 2.5
				case "config-listener":
					document["listen_address"] = "0.0.0.0"
				case "config-unknown":
					document["exec"] = "third-party"
				}
				data, _ := json.Marshal(document)
				write("etc/msboost/tcp-probe.json", string(data))
			case "source-owner", "source-group", "source-mode", "source-hardlink", "config-owner", "unit-owner", "unit-group", "unit-mode":
				path := "etc/msboost/relay_tcp_probe.py"
				if strings.HasPrefix(scenario, "config-") {
					path = "etc/msboost/tcp-probe.json"
				} else if strings.HasPrefix(scenario, "unit-") {
					path = "etc/systemd/system/msboost-tcp-probe.service"
				}
				value := map[string]any{}
				switch {
				case strings.HasSuffix(scenario, "owner"):
					value["uid"] = 2000
				case strings.HasSuffix(scenario, "group"):
					value["gid"] = 9999
				case strings.HasSuffix(scenario, "mode"):
					value["mode"] = 0660
				case strings.HasSuffix(scenario, "hardlink"):
					value["nlink"] = 2
				}
				meta[path] = value
			case "source-symlink":
				meta["etc/msboost/relay_tcp_probe.py"] = map[string]any{"symlink": true}
			case "unit-symlink":
				meta["etc/systemd/system/msboost-tcp-probe.service"] = map[string]any{"symlink": true}
			case "parent-symlink":
				meta["etc/msboost"] = map[string]any{"symlink": true}
			case "dropin":
				write("etc/systemd/system/msboost-tcp-probe.service.d/override.conf", "[Service]\nExecStop=/unsafe\n")
			case "active-dropin", "fragment":
				meta[scenario] = true
			case "unit-stop-hook":
				write("etc/systemd/system/msboost-tcp-probe.service", strings.Replace(unit, "[Service]\n", "[Service]\nExecStop=/unsafe\n", 1))
			case "unit-also":
				write("etc/systemd/system/msboost-tcp-probe.service", strings.Replace(unit, "[Install]\n", "[Install]\nAlso=ssh.service\n", 1))
			case "unit-capability":
				write("etc/systemd/system/msboost-tcp-probe.service", strings.Replace(unit, "CapabilityBoundingSet=\n", "CapabilityBoundingSet=CAP_SYS_ADMIN\n", 1))
			case "unit-extra-section":
				write("etc/systemd/system/msboost-tcp-probe.service", unit+"[Socket]\nListenStream=22\n")
			case "main-other-dependency":
				write("etc/systemd/system/msboost.service", strings.Replace(mainUnit, "Wants=network-online.target", "Wants=network-online.target ssh.service", 1))
			case "enable-link", "changed-enable-link":
				path := "etc/systemd/system/msboost.service.wants/msboost-tcp-probe.service"
				write(path, "fixture link")
				meta[path] = map[string]any{"symlink": true, "readlink": "../msboost-tcp-probe.service"}
			case "persistent-and-runtime", "changed-runtime-link":
				roots := []string{"run"}
				if scenario == "persistent-and-runtime" {
					roots = append(roots, "etc")
				}
				for _, prefix := range roots {
					for _, service := range []string{"msboost.service", "msboost-tcp-probe.service"} {
						target := "multi-user.target"
						if service == "msboost-tcp-probe.service" {
							target = "msboost.service"
						}
						path := prefix + "/systemd/system/" + target + ".wants/" + service
						write(path, "fixture link")
						destination := "../" + service
						if prefix == "run" {
							destination = root + "/etc/systemd/system/" + service
						}
						meta[path] = map[string]any{"symlink": true, "readlink": destination}
					}
				}
			case "enable-parent-symlink":
				write("etc/systemd/system/msboost.service.wants/keep", "third-party directory")
				meta["etc/systemd/system/msboost.service.wants"] = map[string]any{"symlink": true}
			case "unknown-enable-link":
				path := "etc/systemd/system/ssh.service.wants/msboost-tcp-probe.service"
				write(path, "fixture link")
				meta[path] = map[string]any{"symlink": true, "readlink": "../msboost-tcp-probe.service"}
			case "alias-other-name":
				path := "etc/systemd/system/third-party-alias.service"
				write(path, "fixture alias")
				meta[path] = map[string]any{"symlink": true, "readlink": "msboost-tcp-probe.service"}
			case "runtime-mask":
				path := "run/systemd/system/msboost-tcp-probe.service"
				write(path, "fixture mask")
				meta[path] = map[string]any{"symlink": true, "readlink": "/dev/null"}
			case "root-account", "root-group":
				meta[scenario] = true
			}
			data, _ := json.Marshal(meta)
			write("fixture-metadata.json", string(data))
			if scenario == "legacy" || scenario == "enable-link" || scenario == "persistent-and-runtime" {
				out, err = run("cleanup-preview", "")
				if err != nil {
					t.Fatalf("legacy preview: %s %v", out, err)
				}
				raw, _ = base64.StdEncoding.DecodeString(marker(out, "MSBOOST_CLEANUP"))
				_ = json.Unmarshal(raw, &preview)
			}
			out, err = run("cleanup", preview.Digest)
			if scenario == "owned" || scenario == "legacy" || scenario == "enable-link" || scenario == "persistent-and-runtime" {
				if err != nil {
					t.Fatalf("cleanup: %s %v", out, err)
				}
				calls, _ := os.ReadFile(root + "/calls")
				lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
				firstUnit := "msboost-tcp-probe.service"
				if scenario == "legacy" {
					firstUnit = "msboost.service"
				}
				var first []string
				_ = json.Unmarshal([]byte(lines[0]), &first)
				if strings.Join(first, " ") != "systemctl disable --now "+firstUnit {
					t.Fatalf("companion lifecycle order incorrect: %s", calls)
				}
				if strings.Count(string(calls), `"ufw"`) != 1 || strings.Contains(string(calls), "20424") {
					t.Fatalf("probe changed firewall cleanup ownership: %s", calls)
				}
				expectedRuntimeCalls := 0
				if scenario == "persistent-and-runtime" {
					expectedRuntimeCalls = 2
				}
				if strings.Count(string(calls), `"--runtime"`) != expectedRuntimeCalls {
					t.Fatalf("runtime enable cleanup did not match owned links: %s", calls)
				}
				for _, prefix := range []string{"etc", "run"} {
					for _, path := range []string{"multi-user.target.wants/msboost.service", "msboost.service.wants/msboost-tcp-probe.service"} {
						if _, err := os.Stat(root + "/" + prefix + "/systemd/system/" + path); !os.IsNotExist(err) {
							t.Fatalf("managed enable link remains: %s/%s", prefix, path)
						}
					}
				}
				for _, path := range []string{"etc/msboost", "var/lib/msboost", "usr/local/bin/msboost", "etc/systemd/system/msboost.service", "etc/systemd/system/msboost-tcp-probe.service"} {
					if _, err := os.Stat(root + "/" + path); !os.IsNotExist(err) {
						t.Fatalf("owned resource remains: %s", path)
					}
				}
			} else {
				if err == nil {
					t.Fatalf("unsafe fixture %s accepted: %s", scenario, out)
				}
				code := "ownership_failed"
				if scenario == "changed-config" || scenario == "changed-enable-link" || scenario == "changed-runtime-link" {
					code = "cleanup_changed"
				}
				if marker(out, "MSBOOST_ERROR_CODE") != code {
					t.Fatalf("unexpected rejection code: %s", out)
				}
				if _, err := os.Stat(root + "/calls"); !os.IsNotExist(err) {
					t.Fatal("rejected cleanup mutated services")
				}
				if _, err := os.Stat(root + "/etc/systemd/system/msboost.service"); err != nil {
					t.Fatal("rejected cleanup removed main unit")
				}
			}
			for _, path := range []string{"var/backups/msboost/retained", "usr/local/bin/unrelated"} {
				if _, err := os.Stat(root + "/" + path); err != nil {
					t.Fatalf("unrelated resource removed: %s", path)
				}
			}
		})
	}
}

func tcpProbeHeredoc(t *testing.T, installer, filename string) string {
	t.Helper()
	lines := strings.Split(installer, "\n")
	for i, line := range lines {
		if !strings.Contains(line, "cat >") || !strings.Contains(line, filename) && !(filename == "msboost-tcp-probe.service" && strings.Contains(line, "<<'MSBOOST_TCP_PROBE_UNIT'")) {
			continue
		}
		_, marker, ok := strings.Cut(line, "<<")
		if !ok {
			continue
		}
		marker = strings.Trim(strings.TrimSpace(marker), "'\"")
		for end := i + 1; end < len(lines); end++ {
			if lines[end] == marker {
				return strings.Join(lines[i+1:end], "\n") + "\n"
			}
		}
	}
	t.Fatalf("installer heredoc missing: %s", filename)
	return ""
}
