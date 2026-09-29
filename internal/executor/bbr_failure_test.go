package executor

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Execute the actual shell control flow with an isolated simulated kernel.
// No host sysctl, package manager, module or /etc file is touched.
func TestBBRFailureRollbackAndIndependentStatus(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux shell fixture")
	}
	for _, mode := range []string{"enabled", "unsupported", "partial", "rollback_failed"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			for _, sub := range []string{"conf", "module", "work"} {
				if err := os.Mkdir(filepath.Join(dir, sub), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "unsupported" {
				_ = os.Remove(filepath.Join(dir, "module"))
			}
			prefix := `set -Eeuo pipefail
work="$FIXTURE/work"
id() { printf '0\n'; }
stat() { case "$2" in %u) echo 0;; %a) echo 700;; '%u:%h') echo 0:1;; *) return 1;; esac; }
modprobe() { [[ $MODE != unsupported ]]; }
apt-get() { echo 'unexpected package mutation' >&2; return 1; }
install() { cp -- "${@: -2:1}" "${@: -1}"; }
sysctl() {
  case "$1" in
    -n) if [[ -f "$work/applied" ]]; then sed -n "s/^$2=//p" "$work/bbr-supported"; else echo old; fi;;
    -p) touch "$work/applied"; [[ $MODE == enabled ]];;
    -w) printf '%s\n' "$2" >> "$work/rollback"; [[ $MODE != rollback_failed ]];;
    *) return 1;;
  esac
}
`
			script := strings.ReplaceAll(bbrTuneScript, "/etc/sysctl.d", dir+"/conf")
			script = strings.ReplaceAll(script, "/sys/module/tcp_bbr", dir+"/module")
			cmd := exec.Command("bash", "-c", prefix+script+"\nprintf 'BASE_DEPLOY_CONTINUES\\n'\n")
			cmd.Env = append(os.Environ(), "FIXTURE="+dir, "MODE="+mode)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			status := "unavailable"
			if mode == "enabled" {
				status = "enabled"
			}
			if mode == "rollback_failed" {
				status = "review_required"
			}
			if !strings.Contains(string(out), "MSBOOST_BBR="+status) || !strings.Contains(string(out), "BASE_DEPLOY_CONTINUES") {
				t.Fatalf("%s", out)
			}
			_, err = os.Stat(filepath.Join(dir, "conf", "zz-msboost-bbr.conf"))
			if (err == nil) != (mode == "enabled") {
				t.Fatal("failed optimization published configuration")
			}
			if mode == "partial" || mode == "rollback_failed" {
				raw, err := os.ReadFile(filepath.Join(dir, "work", "rollback"))
				if err != nil || len(strings.Split(strings.TrimSpace(string(raw)), "\n")) != 13 {
					t.Fatal("partial kernel writes were not all compensated")
				}
			}
		})
	}
}
