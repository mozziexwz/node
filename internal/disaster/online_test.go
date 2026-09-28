package disaster

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifiedConfigFailurePreservesPreviousFile(t *testing.T) {
	f := newRemoteFixture(t)
	c := remoteTestConfig(t, f)
	file := filepath.Join(testPrivateDirectory(t), "config.json")
	if err := SaveConfig(file, c); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(file)
	c.Password = "wrong-password"
	if err := saveVerifiedConfig(context.Background(), file, c, f.dial(t)); err != remoteAuthError {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(file)
	if !bytes.Equal(before, after) {
		t.Fatal("replaced verified configuration after failed login")
	}
}

func TestRemoteConfigurationProbe(t *testing.T) {
	for _, kind := range []string{"success", "password", "fingerprint", "unwritable", "corrupt"} {
		t.Run(kind, func(t *testing.T) {
			f := newRemoteFixture(t)
			c := remoteTestConfig(t, f)
			var want error
			switch kind {
			case "password":
				c.Password = "wrong"
				want = remoteAuthError
			case "fingerprint":
				c.Fingerprint = "SHA256:" + strings.Repeat("A", 43)
				want = remoteIdentityError
			case "unwritable":
				f.fs.rejectChmod.Store(true)
				want = remoteDirectoryError
			case "corrupt":
				f.fs.corrupt.Store(true)
				want = remoteVerifyError
			}
			err := checkRemoteWithDial(context.Background(), c, f.dial(t))
			if err != want {
				t.Fatalf("got %v want %v", err, want)
			}
			if kind == "success" {
				sf := f.connect(t)
				rows, e := sf.ReadDir(c.RemoteDir)
				if e != nil || len(rows) != 0 {
					t.Fatal("test file was not cleaned", e, rows)
				}
			}
		})
	}
}

func TestOnlineBundleAndHistory(t *testing.T) {
	dir, outputDir := testPrivateDirectory(t), testPrivateDirectory(t)
	for name, raw := range testFiles(t) {
		if name == "database.dump" || strings.HasPrefix(name, "caddy_") {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	id := "msboost-disaster-20260928T010203Z-0123456789abcdef.tar.gz"
	archive := filepath.Join(outputDir, id)
	if err := PackOnline(dir, archive); err != nil {
		t.Fatal(err)
	}
	m, err := Verify(archive)
	if err != nil || m.Version != 2 || len(m.Files) != 4 {
		t.Fatal(m, err)
	}
	if err := Unpack(archive, filepath.Join(outputDir, "restored")); err != nil {
		t.Fatal(err)
	}
	historyFile := filepath.Join(outputDir, "history.json")
	if err := RecordStage(historyFile, id, "started", "", true); err != nil {
		t.Fatal(err)
	}
	if err := RecordStage(historyFile, id, "remote", "", true); err == nil {
		t.Fatal("remote success without local verification")
	}
	if err := RecordStage(historyFile, id, "local", archive, true); err != nil {
		t.Fatal(err)
	}
	if err := RecordStage(historyFile, id, "remote_failed", "", true); err != nil {
		t.Fatal(err)
	}
	h, err := ReadHistory(historyFile)
	if err != nil || len(h.Runs) != 1 || !h.Runs[0].LocalOK || h.Runs[0].RemoteOK {
		t.Fatal(h, err)
	}
	if err := RecordStage(historyFile, id, "remote", "", true); err != nil {
		t.Fatal(err)
	}
	h, err = ReadHistory(historyFile)
	if err != nil || !h.Runs[0].RemoteOK {
		t.Fatal(h, err)
	}
}
