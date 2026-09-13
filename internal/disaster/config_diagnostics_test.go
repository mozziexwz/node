package disaster

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigSaveDiagnosticStages(t *testing.T) {
	for _, stage := range []configSaveError{errConfigValidate, errConfigParent, errConfigExisting, errConfigLocalDir} {
		t.Run(stage.Error(), func(t *testing.T) {
			parent := testPrivateDirectory(t)
			filename := filepath.Join(parent, "secret-path-disaster.json")
			config := DefaultConfig()
			config.LocalDir = filepath.Join(parent, "local-backups")
			switch stage {
			case errConfigValidate:
				config.RetentionDays = 0
			case errConfigParent:
				filename = filepath.Join(parent, "missing-secret-parent", "disaster.json")
			case errConfigExisting:
				if err := os.WriteFile(filename, []byte("secret-invalid-config"), 0600); err != nil {
					t.Fatal(err)
				}
			case errConfigLocalDir:
				if err := os.WriteFile(config.LocalDir, []byte("secret-file-not-directory"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := SaveConfig(filename, config); err != stage {
				t.Fatalf("wrong diagnostic stage: got %v, want %v", err, stage)
			}
			var output bytes.Buffer
			err := Run([]string{"config-save", "--file", filename, "--local-dir", config.LocalDir, "--retention-days", fmt.Sprint(config.RetentionDays)}, strings.NewReader(""), &output)
			if err == nil || err.Error() != stage.Error() || output.Len() != 0 {
				t.Fatal("CLI did not return only the fixed configuration stage")
			}
			if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), parent) {
				t.Fatal("diagnostic exposed path or configuration contents")
			}
		})
	}
}

func TestConfigSaveWriteDiagnosticDiscardsUnderlyingError(t *testing.T) {
	parent := testPrivateDirectory(t)
	filename := filepath.Join(parent, "disaster.json")
	config := DefaultConfig()
	config.LocalDir = filepath.Join(parent, "local-backups")
	if err := SaveConfig(filename, config); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	config.RetentionDays++
	called := false
	err = saveConfig(filename, config, func(string, []byte) error {
		called = true
		return &os.PathError{Op: "secret-operation", Path: "secret-password-and-path", Err: errors.New("secret-SSH-server-response")}
	})
	if !called || err != errConfigWrite || safeCommandError("config-save", err).Error() != errConfigWrite.Error() {
		t.Fatal("write failure did not become the fixed write-config stage")
	}
	after, err := os.ReadFile(filename)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("failed write changed the existing private configuration")
	}
}

func TestConfigSaveDiagnosticWhitelistRejectsForgedErrors(t *testing.T) {
	for _, err := range []error{
		errors.New("[config-save/validate] secret-password"),
		fmt.Errorf("secret-password: %w", errConfigParent),
		&os.PathError{Op: "secret-operation", Path: "secret-path", Err: errConfigWrite},
		configSaveError(255),
	} {
		for _, command := range []string{"config-save", "upload", "verify", "config-get"} {
			got := safeCommandError(command, err).Error()
			if strings.Contains(got, "secret") || strings.Contains(got, "[config-save/") {
				t.Fatal("untrusted error bypassed the fixed diagnostic whitelist")
			}
		}
	}
	for _, stage := range []configSaveError{errConfigValidate, errConfigParent, errConfigExisting, errConfigLocalDir, errConfigWrite} {
		if got := safeCommandError("config-save", stage).Error(); got != stage.Error() {
			t.Fatal("known stage not preserved")
		}
		if got := safeCommandError("upload", stage).Error(); strings.Contains(got, "[config-save/") {
			t.Fatal("another command gained configuration-stage diagnostics")
		}
	}
}
