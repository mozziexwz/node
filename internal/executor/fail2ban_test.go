package executor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

func TestFail2banOptionsAreValidated(t *testing.T) {
	for _, valid := range []*Fail2banOptions{
		nil,
		{},
		{ClientBits: 16},
		{ClientBits: 24},
		{Allow: []string{"1.1.1.1", "2606:4700:4700::1111"}, ClientBits: 32},
	} {
		if err := ValidateFail2ban(valid); err != nil {
			t.Errorf("rejected %+v: %v", valid, err)
		}
	}
	many := make([]string, MaxFail2banAllow+1)
	for i := range many {
		many[i] = "1.1.1.1"
	}
	for _, invalid := range []*Fail2banOptions{
		{Allow: []string{"10.0.0.1"}},
		{Allow: []string{"127.0.0.1"}},
		{Allow: []string{"1.1.1.0/24"}},
		{Allow: []string{"example.com"}},
		{Allow: []string{"1.1.1.1 2.2.2.2"}},
		{Allow: []string{"1.1.1.1\n[DEFAULT]"}},
		{Allow: many},
		{ClientBits: 8},
		{ClientBits: 31},
		{ClientBits: -16},
	} {
		if ValidateFail2ban(invalid) == nil {
			t.Errorf("accepted %+v", invalid)
		}
	}

	options := &Fail2banOptions{Allow: []string{"1.1.1.1"}, ClientBits: 32}
	for _, request := range []Request{
		{Kind: "deploy", Mode: "fresh", SSH: testSSH("8.8.8.8"), Fail2ban: options},
		{Kind: "upgrade", SSH: testSSH("8.8.8.8"), Fail2ban: options},
	} {
		if err := ValidateRequest(request); err != nil {
			t.Errorf("%s with SSH protection rejected: %v", request.Kind, err)
		}
	}
	for _, request := range []Request{
		{Kind: "fetch-config", SSH: testSSH("8.8.8.8"), Fail2ban: options},
		{Kind: "relay", SSH: testSSH("8.8.8.8"), ClientConfig: []byte(sampleConfig), Fail2ban: options},
		{Kind: "cleanup-preview", SSH: testSSH("8.8.8.8"), Cleanup: &CleanupOptions{Scope: "msboost"}, Fail2ban: options},
		{Kind: "deploy", Mode: "fresh", SSH: testSSH("8.8.8.8"), Fail2ban: &Fail2banOptions{Allow: []string{"192.168.1.1"}}},
	} {
		if ValidateRequest(request) == nil {
			t.Errorf("accepted SSH protection on %s: %+v", request.Kind, request.Fail2ban)
		}
	}
}

// f2bRemote answers the node check, the preflight and the install script.
func f2bRemote(installOutput string) *fakeRemote {
	return &fakeRemote{run: func(_ SSH, script string) ([]byte, error) {
		config := "MSBOOST_CONFIG=" + base64.StdEncoding.EncodeToString([]byte(sampleConfig)) + "\n"
		switch script {
		case nodeConfigScript:
			return []byte("MSBOOST_SERVICE=active\nMSBOOST_PROBE=active\n" + config), nil
		case debianPreflightScript:
			return []byte("MSBOOST_READY=1\n"), nil
		}
		return []byte("MSBOOST_BBR=enabled\n" + config + installOutput), nil
	}}
}

func f2bEngine(remote Remote) *Engine {
	return &Engine{
		Remote:   remote,
		TCPProbe: func(context.Context, string, int) string { return "reachable" },
		UDPProbe: func(context.Context, []byte) string { return UDPOff },
	}
}

func TestFail2banStepIsAddedOnlyWhenAsked(t *testing.T) {
	if fail2banStep(nil) != "" {
		t.Fatal("a request without SSH protection must add nothing to the script")
	}
	options := &Fail2banOptions{Allow: []string{"1.1.1.1", "2606:4700:4700:0::1111"}, ClientBits: 32}
	for _, kind := range []string{"deploy", "upgrade"} {
		request := Request{Kind: kind, SSH: testSSH("8.8.8.8")}
		if kind == "deploy" {
			request.Mode = "fresh"
		}

		// As an older panel sends it: no step, and no field an older panel's
		// strict decoding would refuse.
		plain := f2bRemote("")
		out := f2bEngine(plain).Execute(context.Background(), Job{ID: kind, Request: request, Script: testAsset()})
		install := plain.scripts[len(plain.scripts)-1]
		if out.State != "succeeded" || out.Health.Fail2ban != "" || strings.Contains(install, "fail2ban") || !strings.HasSuffix(install, nodeConfigTail) {
			t.Fatalf("%s without SSH protection: %+v", kind, out)
		}
		if raw, _ := json.Marshal(out); strings.Contains(string(raw), "fail2ban") {
			t.Fatalf("%s reported SSH protection nobody asked for: %s", kind, raw)
		}

		request.Fail2ban = options
		for reported, want := range map[string]string{
			"MSBOOST_FAIL2BAN=enabled\n":         "enabled",
			"MSBOOST_FAIL2BAN=existing\n":        "existing",
			"MSBOOST_FAIL2BAN=review_required\n": "review_required",
			"MSBOOST_FAIL2BAN=unavailable\n":     "unavailable",
			"MSBOOST_FAIL2BAN=rm -rf /\n":        "unavailable",
			"":                                   "unavailable",
		} {
			remote := f2bRemote(reported)
			out = f2bEngine(remote).Execute(context.Background(), Job{ID: kind, Request: request, Script: testAsset()})
			if out.State != "succeeded" || string(out.Config) != sampleConfig || out.Health.Fail2ban != want || out.Health.BBR != "enabled" {
				t.Fatalf("%s reporting %q: %+v %+v", kind, reported, out, out.Health)
			}
			install = remote.scripts[len(remote.scripts)-1]
			step := encodedAssignment("f2b_allow", "1.1.1.1 2606:4700:4700::1111") + encodedAssignment("f2b_client_bits", "32") + fail2banScript
			if !strings.HasSuffix(install, nodeConfigTail+step) {
				t.Fatalf("%s must protect SSH after the node is running and its configuration is read", kind)
			}
		}
	}

	// A failed installation is reported as such; the step's status is not.
	broken := &fakeRemote{run: func(_ SSH, script string) ([]byte, error) {
		if script == debianPreflightScript {
			return []byte("MSBOOST_READY=1\n"), nil
		}
		return []byte("MSBOOST_BBR=enabled\nMSBOOST_ERROR_CODE=execution_failed\nMSBOOST_ERROR_PHASE=install\n"), errors.New("exit status 1")
	}}
	out := f2bEngine(broken).Execute(context.Background(), Job{Request: Request{Kind: "deploy", Mode: "fresh", SSH: testSSH("8.8.8.8"), Fail2ban: options}, Script: testAsset()})
	if out.State != "failed" || out.Phase != "install" || out.Health.Fail2ban != "" {
		t.Fatalf("failed installation: %+v %+v", out, out.Health)
	}
}

func TestFail2banScriptKeepsToItsOwnFiles(t *testing.T) {
	for _, line := range []string{
		"'bantime = 1d'", "'findtime = 15m'", "'maxretry = 5'",
		"'bantime.increment = true'", "'bantime.factor = 1'", "'bantime.maxtime = 1w'",
		"'mode = normal'", "'dbpurgeage = 2592000'", "'enabled = true'",
	} {
		if strings.Count(fail2banScript, line) != 1 {
			t.Errorf("missing or duplicated default %s", line)
		}
	}
	for _, want := range []string{
		"MSBOOST_STEP=" + Fail2banStepNote + `\n`,
		`jail="$dir/jail.d/zz-msboost.conf"`,
		`purge="$dir/fail2ban.d/zz-msboost.conf"`,
		"# Managed by MSBOOST (msboost-fail2ban-v1)",
		"if (msboost_fail2ban) < /dev/null >>",
		"MSBOOST_FAIL2BAN=",
		"nftables-multiport", "iptables-multiport",
		"fail2ban-client -t",
	} {
		if !strings.Contains(fail2banScript, want) {
			t.Errorf("script lacks %q", want)
		}
	}
	// The owner's files are only ever looked at.
	for _, write := range []string{`> "$dir/jail.local"`, `"$dir/jail.local" <`, "fail2ban.local", "jail.conf.", "tee "} {
		if strings.Contains(fail2banScript, write) {
			t.Errorf("script touches a file it does not own: %q", write)
		}
	}
	if regexp.MustCompile(`systemctl[^\n]*nftables`).MatchString(fail2banScript) {
		t.Error("nftables.service must stay as it is: its ruleset flushes other firewalls' rules")
	}
	// A download may be cut short; an installation never is.
	for _, line := range strings.Split(fail2banScript, "\n") {
		if strings.Contains(line, "--no-download") && (strings.Contains(line, "timeout") || strings.Contains(line, "f2b_net")) {
			t.Errorf("installation can be interrupted: %s", line)
		}
		if strings.Contains(line, "timeout ") && !strings.Contains(line, `timeout "$left" apt-get`) {
			t.Errorf("unexpected time limit: %s", line)
		}
	}
	if strings.Index(fail2banScript, `f2b_write "$jail"`) > strings.Index(fail2banScript, "f2b_install fail2ban") {
		t.Error("the allowlist must be written before the package starts its service")
	}
}

// The fixture replaces every system command the step uses. Installing the
// fail2ban package starts its service, as Debian does.
const f2bFixture = `
work="$FIXTURE/work"
F2B="$FIXTURE/etc"
id() { printf '0\n'; }
stat() { case "$2" in %u) echo 0 ;; %a) echo 755 ;; *) return 1 ;; esac; }
chown() { :; }
timeout() { shift; "$@"; }
sleep() { :; }
python3() { [[ $MODE != no_journal ]]; }
ss() { printf 'State Recv-Q Send-Q Local Address:Port Peer Address:Port Process\nLISTEN 0 128 0.0.0.0:2222 0.0.0.0:* users:(("sshd",pid=1,fd=3))\nLISTEN 0 100 127.0.0.1:25 0.0.0.0:* users:(("master",pid=2,fd=13))\n'; }
sshd() { printf 'port 22\naddressfamily any\n'; }
nft() { [[ $MODE != no_nft && $MODE != no_firewall ]]; }
iptables() { [[ $MODE != no_firewall ]]; }
dpkg-query() { if [[ -e "$FIXTURE/pkg-${*: -1}" ]]; then printf 'install ok installed'; else return 1; fi; }
dpkg() { if [[ $MODE == edited ]]; then printf '??5?????? c %s/jail.conf\n' "$F2B"; fi; }
f2b_package() {
  mkdir -p "$F2B/action.d" "$F2B/jail.d" "$F2B/fail2ban.d"
  : > "$F2B/action.d/nftables-multiport.conf"
  : > "$F2B/action.d/iptables-multiport.conf"
  : > "$F2B/jail.d/defaults-debian.conf"
  : > "$FIXTURE/pkg-fail2ban"
  fail2ban-client() {
    case "$1" in
      --version) printf 'Fail2Ban v1.0.2\n' ;;
      -t) [[ $MODE != bad_config ]] ;;
      status) [[ -e "$FIXTURE/active" && $MODE != jail_down ]] ;;
      *) return 1 ;;
    esac
  }
}
apt-get() {
  local argument
  printf 'apt-get %s\n' "$*" >> "$FIXTURE/calls"
  if [[ $MODE == apt_fails ]]; then return 100; fi
  case " $* " in
    *" --no-download "*)
      for argument in "$@"; do
        case "$argument" in
          python3-systemd | nftables) : > "$FIXTURE/pkg-$argument" ;;
          fail2ban)
            cp "$F2B/jail.d/zz-msboost.conf" "$FIXTURE/first-start.conf"
            f2b_package
            : > "$FIXTURE/active"
            : > "$FIXTURE/enabled"
            ;;
        esac
      done
      ;;
  esac
}
systemctl() {
  printf 'systemctl %s\n' "$*" >> "$FIXTURE/calls"
  case "$1" in
    is-active) [[ "${*: -1}" == fail2ban && -e "$FIXTURE/active" ]] ;;
    is-enabled) [[ -e "$FIXTURE/enabled" ]] ;;
    is-failed) return 1 ;;
    enable) : > "$FIXTURE/enabled" ;;
    restart) : > "$FIXTURE/active" ;;
    stop) rm -f "$FIXTURE/active" ;;
    disable)
      rm -f "$FIXTURE/enabled"
      if [[ " $* " == *" --now "* ]]; then rm -f "$FIXTURE/active"; fi
      ;;
  esac
}
`

type f2bServer struct {
	t    *testing.T
	root string
}

func newF2BServer(t *testing.T) f2bServer {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "work"), 0o700); err != nil {
		t.Fatal(err)
	}
	return f2bServer{t: t, root: root}
}

// run executes the step as a deploy does: under the deploy's shell options and
// error trap, from standard input, followed by more of the script.
func (s f2bServer) run(mode, session, setup string, o *Fail2banOptions) string {
	s.t.Helper()
	root := filepath.ToSlash(s.root)
	step := strings.ReplaceAll(fail2banStep(o), "/etc/fail2ban", root+"/etc")
	step = strings.ReplaceAll(step, "/run/systemd/system", root+"/work")
	cmd := exec.Command("bash", "-s")
	cmd.Stdin = strings.NewReader("set -Eeuo pipefail\numask 077\n" + diagnosticPrelude + f2bFixture + setup + step + "\nprintf 'BASE_DEPLOY_CONTINUES\\n'\n")
	cmd.Env = append(os.Environ(), "FIXTURE="+root, "MODE="+mode, "SSH_CONNECTION="+session)
	out, err := cmd.CombinedOutput()
	if err != nil {
		s.t.Fatalf("the step ended the deploy: %v\n%s\n%s", err, out, s.read("work/fail2ban.log"))
	}
	if !strings.Contains(string(out), "MSBOOST_STEP="+Fail2banStepNote+"\n") || !strings.Contains(string(out), "BASE_DEPLOY_CONTINUES") || strings.Contains(string(out), "MSBOOST_ERROR_CODE") {
		s.t.Fatalf("unexpected output:\n%s\n%s", out, s.read("work/fail2ban.log"))
	}
	return marker(out, "MSBOOST_FAIL2BAN")
}

func (s f2bServer) read(name string) string {
	raw, _ := os.ReadFile(filepath.Join(s.root, filepath.FromSlash(name)))
	return string(raw)
}

func (s f2bServer) exists(name string) bool {
	_, err := os.Lstat(filepath.Join(s.root, filepath.FromSlash(name)))
	return err == nil
}

func (s f2bServer) write(name, content string) {
	s.t.Helper()
	path := filepath.Join(s.root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		s.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		s.t.Fatal(err)
	}
}

const (
	f2bJail    = "etc/jail.d/zz-msboost.conf"
	f2bPurge   = "etc/fail2ban.d/zz-msboost.conf"
	f2bSession = "203.0.113.77 51234 10.0.0.5 22022"
)

// Runs the real shell control flow against a simulated server. No package
// manager, service, firewall or /etc file of the host is touched.
func TestFail2banStepOnASimulatedServer(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux shell fixture")
	}
	panel := &Fail2banOptions{Allow: []string{"1.1.1.1", "2606:4700:4700::1111", "1.1.1.1"}, ClientBits: 32}
	local := &Fail2banOptions{ClientBits: 16}

	t.Run("syntax", func(t *testing.T) {
		cmd := exec.Command("bash", "-n")
		cmd.Stdin = strings.NewReader(fail2banStep(panel))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("bash -n: %v %s", err, out)
		}
	})

	t.Run("fresh server from the panel", func(t *testing.T) {
		s := newF2BServer(t)
		if status := s.run("", f2bSession, "", panel); status != "enabled" {
			t.Fatalf("status %q\n%s", status, s.read("work/fail2ban.log"))
		}
		want := "# Managed by MSBOOST (msboost-fail2ban-v1)\n" +
			"# Defaults for a server without its own fail2ban configuration. Anything in\n" +
			"# jail.local or jail.d/*.local takes precedence over this file.\n" +
			"[sshd]\nenabled = true\nmode = normal\nbackend = systemd\nport = 22,2222,22022\n" +
			"ignoreip = 127.0.0.1/8 ::1 1.1.1.1 2606:4700:4700::1111 203.0.113.77\n" +
			"banaction = nftables-multiport\nbantime = 1d\nfindtime = 15m\nmaxretry = 5\n" +
			"bantime.increment = true\nbantime.factor = 1\nbantime.maxtime = 1w\n"
		if got := s.read(f2bJail); got != want {
			t.Fatalf("jail configuration:\n%s", got)
		}
		if got := s.read(f2bPurge); !strings.HasPrefix(got, "# Managed by MSBOOST (msboost-fail2ban-v1)\n") || !strings.HasSuffix(got, "[Definition]\ndbpurgeage = 2592000\n") {
			t.Fatalf("history retention:\n%s", got)
		}
		// The package's own first start already had the allowlist.
		first := s.read("first-start.conf")
		if !strings.Contains(first, "ignoreip = 127.0.0.1/8 ::1 1.1.1.1 2606:4700:4700::1111 203.0.113.77\n") || !strings.Contains(first, "port = 22,2222,22022\n") {
			t.Fatalf("the service first started without the allowlist:\n%s", first)
		}
		calls := s.read("calls")
		for _, call := range []string{"systemctl enable fail2ban\n", "systemctl restart fail2ban\n", "--download-only python3-systemd nftables\n", "--no-download fail2ban\n"} {
			if !strings.Contains(calls, call) {
				t.Errorf("missing %q in:\n%s", call, calls)
			}
		}
		if strings.Contains(calls, "nftables.service") || strings.Contains(calls, "apt-get -o DPkg::Lock::Timeout=60 -o Acquire::Retries=2 update") {
			t.Errorf("unexpected calls:\n%s", calls)
		}
		for _, leftover := range []string{"etc/jail.local", "etc/fail2ban.local"} {
			if s.exists(leftover) {
				t.Errorf("created %s", leftover)
			}
		}
		if entries, _ := filepath.Glob(filepath.Join(s.root, "etc", "*", ".msboost-f2b.*")); len(entries) != 0 {
			t.Errorf("staging files left behind: %v", entries)
		}

		// Running it again changes nothing and restarts nothing.
		before := s.read(f2bJail)
		if err := os.Remove(filepath.Join(s.root, "calls")); err != nil {
			t.Fatal(err)
		}
		if status := s.run("", f2bSession, "f2b_package\n", panel); status != "enabled" {
			t.Fatalf("second run: %q\n%s", status, s.read("work/fail2ban.log"))
		}
		if calls = s.read("calls"); s.read(f2bJail) != before || strings.Contains(calls, "restart") || strings.Contains(calls, "apt-get") {
			t.Fatalf("second run was not idempotent:\n%s", calls)
		}

		// Another executor list is applied with a restart.
		if status := s.run("", f2bSession, "f2b_package\n", &Fail2banOptions{Allow: []string{"9.9.9.9"}, ClientBits: 32}); status != "enabled" {
			t.Fatalf("third run: %q", status)
		}
		if got := s.read(f2bJail); !strings.Contains(got, "ignoreip = 127.0.0.1/8 ::1 9.9.9.9 203.0.113.77\n") || !strings.Contains(s.read("calls"), "systemctl restart fail2ban\n") {
			t.Fatalf("allowlist not refreshed:\n%s", got)
		}
	})

	t.Run("local program allows the /16 it connects from", func(t *testing.T) {
		for bits, want := range map[int]string{16: "203.0.0.0/16", 24: "203.0.113.0/24", 32: "203.0.113.77"} {
			s := newF2BServer(t)
			if status := s.run("", f2bSession, "", &Fail2banOptions{ClientBits: bits}); status != "enabled" {
				t.Fatalf("/%d: status %q\n%s", bits, status, s.read("work/fail2ban.log"))
			}
			if got := s.read(f2bJail); !strings.Contains(got, "ignoreip = 127.0.0.1/8 ::1 "+want+"\n") {
				t.Fatalf("/%d:\n%s", bits, got)
			}
		}
		// OpenSSH may report an IPv4 client on a dual-stack socket as mapped.
		s := newF2BServer(t)
		if status := s.run("", "::ffff:198.51.100.9 40000 ::ffff:10.0.0.5 22", "", local); status != "enabled" || !strings.Contains(s.read(f2bJail), "ignoreip = 127.0.0.1/8 ::1 198.51.0.0/16\n") {
			t.Fatalf("mapped address: %q\n%s", status, s.read(f2bJail))
		}
	})

	t.Run("no session address means no service without the caller allowed", func(t *testing.T) {
		for _, session := range []string{"", "not-an-address 1 2 3", "999.1.1.1 1 10.0.0.5 22"} {
			s := newF2BServer(t)
			if status := s.run("", session, "", local); status != "unavailable" {
				t.Fatalf("%q: status %q", session, status)
			}
			if s.exists("etc") || s.exists("calls") {
				t.Fatalf("%q: the server was changed", session)
			}
		}
	})

	t.Run("an existing configuration is left alone", func(t *testing.T) {
		for name, prepare := range map[string]func(f2bServer) (mode, setup string){
			"jail.local": func(s f2bServer) (string, string) {
				s.write("etc/jail.local", "[sshd]\nenabled = true\n")
				return "", "f2b_package\n"
			},
			"jail.d local": func(s f2bServer) (string, string) {
				s.write("etc/jail.d/custom.local", "[sshd]\nmaxretry = 3\n")
				return "", "f2b_package\n"
			},
			"jail.d conf": func(s f2bServer) (string, string) {
				s.write("etc/jail.d/mine.conf", "[sshd]\nmaxretry = 3\n")
				return "", "f2b_package\n"
			},
			"fail2ban.local": func(s f2bServer) (string, string) {
				s.write("etc/fail2ban.local", "[Definition]\nloglevel = INFO\n")
				return "", ""
			},
			"filter override": func(s f2bServer) (string, string) {
				s.write("etc/filter.d/sshd.local", "[Definition]\n")
				return "", "f2b_package\n"
			},
			"edited jail.conf": func(s f2bServer) (string, string) { return "edited", "f2b_package\n" },
			"unmarked file": func(s f2bServer) (string, string) {
				s.write(f2bJail, "[sshd]\nenabled = false\n")
				return "", "f2b_package\n"
			},
		} {
			s := newF2BServer(t)
			mode, setup := prepare(s)
			unmarked := s.read(f2bJail)
			if status := s.run(mode, f2bSession, setup, panel); status != "existing" {
				t.Fatalf("%s: status %q\n%s", name, status, s.read("work/fail2ban.log"))
			}
			if calls := s.read("calls"); strings.Contains(calls, "apt-get") || strings.Contains(calls, "restart") || strings.Contains(calls, "enable") {
				t.Fatalf("%s: the server was changed:\n%s", name, calls)
			}
			if s.read(f2bJail) != unmarked || s.exists(f2bPurge) {
				t.Fatalf("%s: wrote over the owner's configuration", name)
			}
		}
	})

	t.Run("a failure puts a fresh server back", func(t *testing.T) {
		for _, mode := range []string{"apt_fails", "bad_config", "jail_down", "no_firewall"} {
			s := newF2BServer(t)
			if status := s.run(mode, f2bSession, "", panel); status != "unavailable" {
				t.Fatalf("%s: status %q\n%s", mode, status, s.read("work/fail2ban.log"))
			}
			if s.exists(f2bJail) || s.exists(f2bPurge) || s.exists("active") || s.exists("enabled") {
				t.Fatalf("%s: left a half-configured service behind", mode)
			}
			if mode == "apt_fails" {
				if s.exists("etc") {
					t.Fatal("apt_fails: left its directories behind")
				}
				if calls := s.read("calls"); !strings.Contains(calls, " update -qq\n") {
					t.Fatalf("apt_fails: did not refresh the package lists once:\n%s", calls)
				}
			} else if calls := s.read("calls"); !strings.Contains(calls, "systemctl disable --now fail2ban\n") {
				t.Fatalf("%s: the service it installed keeps running on the distribution's defaults:\n%s", mode, calls)
			}
		}
	})

	t.Run("a failure restores a running service", func(t *testing.T) {
		s := newF2BServer(t)
		if status := s.run("", f2bSession, "", panel); status != "enabled" {
			t.Fatalf("setup: %q", status)
		}
		before := s.read(f2bJail)
		changed := &Fail2banOptions{Allow: []string{"9.9.9.9"}, ClientBits: 32}
		if status := s.run("bad_config", f2bSession, "f2b_package\n", changed); status != "unavailable" {
			t.Fatalf("rejected configuration: %q", status)
		}
		if s.read(f2bJail) != before || !s.exists("active") || !s.exists("enabled") {
			t.Fatal("the previous configuration or service state was not restored")
		}
		// The service does not come back even with the previous file.
		if status := s.run("jail_down", f2bSession, "f2b_package\n", changed); status != "review_required" {
			t.Fatalf("service that stays down: %q", status)
		}
		if s.read(f2bJail) != before {
			t.Fatal("the previous configuration was not restored")
		}
	})

	t.Run("fallbacks", func(t *testing.T) {
		s := newF2BServer(t)
		if status := s.run("no_nft", f2bSession, "", panel); status != "enabled" || !strings.Contains(s.read(f2bJail), "banaction = iptables-multiport\n") {
			t.Fatalf("without nft: %q\n%s", status, s.read(f2bJail))
		}
		s = newF2BServer(t)
		if status := s.run("no_journal", f2bSession, "", panel); status != "enabled" || !strings.Contains(s.read(f2bJail), "backend = auto\n") {
			t.Fatalf("without the journal module: %q\n%s", status, s.read(f2bJail))
		}
	})
}

// TestFail2banStepDump writes the step as the panel sends it, for the check
// against a real fail2ban in deploy/fail2ban_config_test.sh.
func TestFail2banStepDump(t *testing.T) {
	path := os.Getenv("MSBOOST_FAIL2BAN_STEP_OUT")
	if path == "" {
		t.Skip("set MSBOOST_FAIL2BAN_STEP_OUT to write the fail2ban step")
	}
	step := fail2banStep(&Fail2banOptions{Allow: []string{"1.1.1.1", "2606:4700:4700::1111"}, ClientBits: 32})
	if err := os.WriteFile(path, []byte(step), 0o600); err != nil {
		t.Fatal(err)
	}
}
