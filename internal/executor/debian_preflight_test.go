package executor

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDebianPreflightShellMatrix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Linux shell contract; live Debian 12 SSH preflight is checked separately")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash unavailable")
	}
	for _, test := range []struct {
		name, id, version string
		allowed           bool
	}{
		{"debian-10", "debian", "10", false},
		{"debian-11", "debian", "11", true},
		{"debian-12", "debian", "12", true},
		{"debian-13", "debian", "13", true},
		{"ubuntu-24", "ubuntu", "24", false},
		{"missing-version", "debian", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			release := filepath.Join(root, "os-release")
			if err := os.WriteFile(release, []byte("ID="+test.id+"\nVERSION_ID="+test.version+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			script := strings.ReplaceAll(debianPreflightScript, "/etc/os-release", release)
			script = strings.ReplaceAll(script, `"$(id -u)"`, `0`)
			script = strings.ReplaceAll(script, "/run/systemd/system", root)
			script = strings.ReplaceAll(script, "command -v systemctl >/dev/null", "true")
			cmd := exec.Command("bash", "-s")
			cmd.Stdin = strings.NewReader(script)
			out, err := cmd.CombinedOutput()
			if test.allowed {
				if err != nil || marker(out, "MSBOOST_READY") != "1" {
					t.Fatalf("supported Debian rejected: %v %s", err, out)
				}
			} else if err == nil || marker(out, "MSBOOST_ERROR_CODE") != "unsupported_debian" {
				t.Fatalf("unsupported OS accepted or misclassified: %v %s", err, out)
			}
		})
	}
}
