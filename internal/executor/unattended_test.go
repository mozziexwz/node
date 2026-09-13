package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// This fixture models only the pinned upstream's username prompt. It cannot
// inspect disks, download anything, change boot configuration, or reboot.
const unattendedReinstallFixture = `#!/usr/bin/env bash
set -e
username=''
while (( $# )); do
  case "$1" in
    --username) username=$2; shift 2 ;;
    --password|--ssh-port) shift 2 ;;
    debian|12) shift ;;
    *) exit 70 ;;
  esac
done
if [ -z "$username" ]; then
  IFS= read -r -p 'Username: ' username
fi
[ "$username" = root ]
# Unexpected input must not contain the remaining outer execution script.
if IFS= read -r unexpected; then exit 71; fi
`

func TestDDUnattendedUsernameAndDetachedStdin(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil && runtime.GOOS == "windows" {
		bash = "C:/Program Files/Git/bin/bash.exe"
		if _, err = os.Stat(bash); err != nil {
			t.Skip("Bash unavailable")
		}
	}
	if bash == "" {
		t.Fatal("Bash required")
	}
	// Verify the relevant upstream behavior remains present without running it.
	upstream, err := os.ReadFile(filepath.Join("..", "..", "installers", "reinstall", "reinstall.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(upstream), `if ! is_netboot_xyz && [ -z "$username" ]; then`) || !strings.Contains(string(upstream), `IFS= read -r -p "Username: " username`) {
		t.Fatal("pinned upstream prompt changed: review the unattended contract")
	}
	root := filepath.ToSlash(t.TempDir())
	remote := &fakeRemote{run: func(_ SSH, script string) ([]byte, error) {
		if strings.Contains(script, "systemctl reboot") {
			// Never execute the reboot command: return synthetic evidence only.
			return []byte("MSBOOST_REBOOT_SUBMITTED=1\n"), &ssh.ExitMissingError{}
		}
		if !strings.Contains(script, "--username root") || !strings.Contains(script, "</dev/null") {
			t.Fatal("DD still depends on interactive stdin")
		}
		script = strings.Replace(script, `[ "$(id -u)" = 0 ]`, ":", 1)
		script = strings.Replace(script, "/run/msboost-task.XXXXXX", root+"/dd-fixture.XXXXXX", 1)
		cmd := exec.Command(bash, "-s")
		cmd.Stdin = strings.NewReader(script)
		return cmd.CombinedOutput()
	}}
	data := []byte(unattendedReinstallFixture)
	sum := sha256.Sum256(data)
	job := Job{ID: "unattended-dd-fixture", Script: Asset{Data: data, SHA256: hex.EncodeToString(sum[:])}, Request: Request{Kind: "dd", SSH: testSSH("8.8.8.8"), DD: &DDOptions{ConfirmErase: true, PortMode: "keep", PasswordMode: "keep"}}}
	result := (&Engine{Remote: remote}).Execute(context.Background(), job)
	if result.State != "executed" || len(remote.scripts) != 2 {
		t.Fatalf("unattended preparation failed: %+v", result)
	}
	// Reproduce the previous wrapper against the same harmless fixture: omitting
	// --username lets read consume the remaining outer script (or EOF), so no
	// verified preparation marker is produced. This is not a real DD execution.
	oldScript := strings.Replace(remote.scripts[0], " --username root", "", 1)
	oldScript = strings.Replace(oldScript, " </dev/null", "", 1)
	oldScript = strings.Replace(oldScript, `[ "$(id -u)" = 0 ]`, ":", 1)
	oldScript = strings.Replace(oldScript, "/run/msboost-task.XXXXXX", root+"/dd-old-fixture.XXXXXX", 1)
	oldCommand := exec.Command(bash, "-s")
	oldCommand.Stdin = strings.NewReader(oldScript)
	oldOutput, oldErr := oldCommand.CombinedOutput()
	if oldErr == nil || marker(oldOutput, "MSBOOST_PREPARED") == "1" {
		t.Fatal("old missing-username regression was not reproduced")
	}
}
