package disaster

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDisasterCLIConfigStdinAndPublicGetWhitelist(t *testing.T) {
	fixture := newRemoteFixture(t)
	parent, local := testPrivateDirectory(t), filepath.Join(testPrivateDirectory(t), "backups")
	file := filepath.Join(parent, "disaster.json")
	args := []string{"config-save", "--file", file, "--local-dir", local, "--retention-days", "45", "--time", "03:45", "--remote-host", "8.8.8.8", "--remote-port", "22", "--remote-user", "root", "--remote-dir", "/remote/backups", "--fingerprint", fixture.fingerprint}
	var output bytes.Buffer
	if err := Run(args, strings.NewReader(remoteFixturePassword), &output); err != nil {
		t.Fatal(err)
	}
	if output.Len() != 0 {
		t.Fatal("saving config printed contents")
	}
	config, err := ReadConfig(file)
	if err != nil || config.Password != remoteFixturePassword || config.LocalDir != local || config.RetentionDays != 45 {
		t.Fatal("config did not round trip")
	}
	if config.PruneRemote {
		t.Fatal("remote pruning enabled by default")
	}
	if err := Run(append(args, "--prune-remote"), strings.NewReader(remoteFixturePassword), &output); err != nil {
		t.Fatal(err)
	}
	config, err = ReadConfig(file)
	if err != nil || !config.PruneRemote {
		t.Fatal("explicit remote pruning was not saved")
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(file)
		if info.Mode().Perm() != 0600 {
			t.Fatal("config not private")
		}
		info, _ = os.Stat(local)
		if info.Mode().Perm() != 0700 {
			t.Fatal("local backup directory not private")
		}
	}
	for field, want := range map[string]string{"localDir": local + "\n", "time": "03:45\n", "retentionDays": "45\n"} {
		output.Reset()
		if err := Run([]string{"config-get", "--file", file, "--field", field}, nil, &output); err != nil || output.String() != want {
			t.Fatal("public config field unavailable")
		}
	}
	for _, field := range []string{"password", "Password", "fingerprint", "remoteHost", "", "*"} {
		output.Reset()
		err := Run([]string{"config-get", "--file", file, "--field", field}, nil, &output)
		if err == nil || output.Len() != 0 || strings.Contains(err.Error(), remoteFixturePassword) {
			t.Fatal("non-whitelisted field revealed")
		}
	}
	// Reconfiguring local-only deliberately removes the previous credential.
	output.Reset()
	if err := Run([]string{"config-save", "--file", file, "--local-dir", local}, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	config, err = ReadConfig(file)
	if err != nil || config.Password != "" || config.RemoteHost != "" || config.PruneRemote {
		t.Fatal("old credentials retained unexpectedly")
	}
}

func TestDisasterCLIRejectsSecretArgumentsAndOversizedStdin(t *testing.T) {
	file := filepath.Join(testPrivateDirectory(t), "disaster.json")
	local := filepath.Join(testPrivateDirectory(t), "backups")
	for _, args := range [][]string{{"config-save", "--password", remoteFixturePassword}, {"config-save", "--retention-days", remoteFixturePassword}, {"config-get", "--field", remoteFixturePassword}, {"unknown", remoteFixturePassword}, {"prepare-dir", "--dir", local, remoteFixturePassword}} {
		var out bytes.Buffer
		err := Run(args, strings.NewReader(remoteFixturePassword), &out)
		if err == nil || out.Len() != 0 || strings.Contains(err.Error(), remoteFixturePassword) {
			t.Fatal("argument error leaked secret")
		}
	}
	var out bytes.Buffer
	if err := Run([]string{"config-save", "--file", file, "--local-dir", local}, strings.NewReader(strings.Repeat("x", 1025)), &out); err == nil {
		t.Fatal("oversized password accepted")
	}
	if _, err := os.Lstat(file); !os.IsNotExist(err) {
		t.Fatal("bad configuration written")
	}
	if _, err := os.Lstat(local); !os.IsNotExist(err) {
		t.Fatal("bad configuration created directory")
	}
}

func TestDisasterCLIArchiveDispatchAndNoOverwrite(t *testing.T) {
	archive := remoteBundleFixture(t)
	var output bytes.Buffer
	if err := Run([]string{"verify", "--archive", archive}, nil, &output); err != nil {
		t.Fatal(err)
	}
	var manifest Manifest
	if json.Unmarshal(output.Bytes(), &manifest) != nil || len(manifest.Files) != len(members) {
		t.Fatal("verify result invalid")
	}
	if bytes.Contains(output.Bytes(), []byte("private isolated fixture")) {
		t.Fatal("verify leaked archive contents")
	}
	destination := filepath.Join(testPrivateDirectory(t), "recovered")
	output.Reset()
	if err := Run([]string{"unpack", "--archive", archive, "--dir", destination}, nil, &output); err != nil {
		t.Fatal(err)
	}
	if err := Run([]string{"unpack", "--archive", archive, "--dir", destination}, nil, &output); err == nil {
		t.Fatal("unpack overwrote existing data")
	}
	if err := Run([]string{"validate-volume", "--archive", filepath.Join(destination, "app_data.tar")}, nil, &output); err != nil {
		t.Fatal(err)
	}
	if err := Run([]string{"prepare-dir", "--dir", filepath.Join(testPrivateDirectory(t), "new")}, nil, &output); err != nil {
		t.Fatal(err)
	}
	if err := Run([]string{"prepare-dir", "--dir", "."}, nil, &output); err == nil {
		t.Fatal("relative directory accepted")
	}
}
