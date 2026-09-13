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

func TestDisasterCommandDiagnosticWhitelist(t *testing.T) {
	commands := []string{"config-save", "config-get", "prepare-dir", "pack", "verify", "unpack", "validate-volume", "retain", "upload"}
	for _, command := range commands {
		for _, injected := range []error{
			errors.New("secret-password [disaster/forged]"),
			&os.PathError{Op: "secret-operation", Path: "secret-path", Err: errors.New("secret-SSH-message")},
			fmt.Errorf("secret-password: %w", errConfigParent),
		} {
			got := safeCommandError(command, injected).Error()
			if !strings.Contains(got, "[disaster/"+command+"]") || strings.Contains(got, "secret") || strings.Contains(got, "forged") {
				t.Fatal("known command diagnostic missing or injected data exposed")
			}
		}
	}
	for _, command := range []string{"", "unknown", "pack-secret-password", "pack\nsecret-password", "[disaster/pack] secret-password", "PACK", " pack", "pack ", "upload\x00secret-password"} {
		got := safeCommandError(command, errors.New("secret-password")).Error()
		if strings.Contains(got, "[disaster/") || strings.Contains(got, "secret") || strings.Contains(got, "unknown") {
			t.Fatal("unknown or forged command was included in diagnostic")
		}
		var output bytes.Buffer
		err := Run([]string{command}, strings.NewReader("secret-password"), &output)
		if err != errArguments || output.Len() != 0 || strings.Contains(err.Error(), "secret") {
			t.Fatal("unknown command dispatch exposed input")
		}
	}
}

func TestDisasterCommandFailuresReturnOnlyKnownStage(t *testing.T) {
	parent := testPrivateDirectory(t)
	badConfig := filepath.Join(parent, "secret-private-config.json")
	if err := os.WriteFile(badConfig, []byte("secret-invalid-config-contents"), 0600); err != nil {
		t.Fatal(err)
	}
	missingArchive := filepath.Join(parent, "secret-missing-archive")
	cases := [][]string{
		{"config-save"},
		{"config-get"},
		{"prepare-dir", "--dir", "secret-relative-directory"},
		{"pack", "--dir", "secret-relative-directory", "--output", missingArchive},
		{"verify", "--archive", missingArchive},
		{"unpack", "--archive", missingArchive, "--dir", filepath.Join(parent, "new")},
		{"validate-volume", "--archive", missingArchive},
		{"retain", "--config", badConfig},
		{"upload", "--config", badConfig, "--archive", missingArchive},
	}
	for _, args := range cases {
		var output bytes.Buffer
		err := Run(args, strings.NewReader("secret-password"), &output)
		if err == nil || !strings.Contains(err.Error(), "[disaster/"+args[0]+"]") || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), parent) || output.Len() != 0 {
			t.Fatalf("unsafe or missing fixed diagnostic for %s", args[0])
		}
	}
}
