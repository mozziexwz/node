package executor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestRelayNoexecStagingAndDiagnosticBoundaries(t *testing.T) {
	if strings.Contains(relayInstallScript, "mktemp -d /run/") || strings.Contains(relayInstallScript, `"$work/gost" -V`) {
		t.Fatal("relay executes an ELF from noexec /run staging")
	}
	if !strings.Contains(relayStageScript, `work=$(mktemp -d "$managed/.stage.XXXXXXXX")`) || !strings.Contains(relayBinaryScript, `"$binary" -V`) || !strings.Contains(relayBinaryScript, `ln -T -- "$work/gost-ready" "$binary"`) {
		t.Fatal("missing protected disk staging or no-clobber atomic publication")
	}
	for _, tc := range []struct{ phase, code string }{{"integrity", "integrity_failed"}, {"extract", "archive_failed"}, {"binary", "binary_unusable"}} {
		d := remoteDiagnostic(errors.New("private diagnostic"), []byte("MSBOOST_ERROR_CODE=execution_failed\nMSBOOST_ERROR_PHASE="+tc.phase+"\n"), "relay")
		if d.Code != tc.code || d.Phase != tc.phase || d.NextStep == "" {
			t.Fatalf("wrong stage diagnostic %+v", d)
		}
	}
}

// A real native ELF (/bin/true) is packaged in a disposable archive. No network,
// service, firewall, or privileged mounts are used. The production script must
// stage and execute only on the protected disk path, never /run.
func TestRelayCacheAtomicPublicationOnDisk(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("native Linux ELF and permission checks exercised in CI")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	binaryData, err := os.ReadFile("/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	archive := func(payload []byte) []byte {
		var buffer bytes.Buffer
		gz := gzip.NewWriter(&buffer)
		tw := tar.NewWriter(gz)
		if err := tw.WriteHeader(&tar.Header{Name: "gost", Mode: 0755, Size: int64(len(payload))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(payload); err != nil {
			t.Fatal(err)
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
		return buffer.Bytes()
	}
	for _, scenario := range []string{"new", "reuse", "changed-cache", "symlink-cache", "symlink-managed", "bad-digest", "bad-archive", "wrong-binary"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			managed := filepath.Join(root, "usr", "local", "libexec", "msboost-free")
			if err := os.MkdirAll(managed, 0755); err != nil {
				t.Fatal(err)
			}
			archiveBytes := archive(binaryData)
			if scenario == "wrong-binary" {
				archiveBytes = archive([]byte("not an ELF"))
			}
			if scenario == "bad-archive" {
				archiveBytes = []byte("not a tar archive")
			}
			sum := sha256.Sum256(archiveBytes)
			digest := hex.EncodeToString(sum[:])
			if scenario == "bad-digest" {
				digest = strings.Repeat("0", 64)
			}
			input := filepath.Join(root, "fixture.tar.gz")
			if err := os.WriteFile(input, archiveBytes, 0600); err != nil {
				t.Fatal(err)
			}
			binary := filepath.Join(managed, "gost-"+digest)
			var original os.FileInfo
			switch scenario {
			case "reuse":
				if err := os.WriteFile(binary, binaryData, 0755); err != nil {
					t.Fatal(err)
				}
				original, _ = os.Stat(binary)
			case "changed-cache":
				if err := os.WriteFile(binary, []byte("third-party-content"), 0755); err != nil {
					t.Fatal(err)
				}
			case "symlink-cache":
				if err := os.Symlink(input, binary); err != nil {
					t.Fatal(err)
				}
			case "symlink-managed":
				if err := os.Remove(managed); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(root, managed); err != nil {
					t.Fatal(err)
				}
			}
			script := "set -Eeuo pipefail\numask 077\n" + diagnosticPrelude + relayStageScript + "\ntrap 'rm -rf -- \"$work\"' EXIT\n" + encodedAssignment("fixture_archive", input) + encodedAssignment("gost_sha", digest) + "cp -- \"$fixture_archive\" \"$work/gost.tar.gz\"\n" + relayBinaryScript + "printf 'CACHE_READY=1\\n'\n"
			// Rewrite production paths and root UID for this isolated CI-user tree.
			script = strings.ReplaceAll(script, "/usr", root+"/usr")
			script = strings.ReplaceAll(script, `" = 0 ]`, `" = `+strconv.Itoa(os.Getuid())+` ]`)
			cmd := exec.Command(bash, "-s")
			cmd.Stdin = strings.NewReader(script)
			out, err := cmd.CombinedOutput()
			want := ""
			switch scenario {
			case "changed-cache", "symlink-cache", "symlink-managed":
				want = "ownership_failed"
			case "bad-digest":
				want = "integrity_failed"
			case "bad-archive":
				want = "archive_failed"
			case "wrong-binary":
				want = "binary_unusable"
			}
			if want != "" {
				if err == nil {
					t.Fatalf("unsafe %s accepted", scenario)
				}
				d := remoteDiagnostic(err, out, "relay")
				if d.Code != want {
					t.Fatalf("want %s got %+v output=%s", want, d, out)
				}
				if scenario == "changed-cache" {
					data, _ := os.ReadFile(binary)
					if string(data) != "third-party-content" {
						t.Fatal("overwrote changed existing shared binary")
					}
				}
				if scenario == "symlink-cache" {
					if _, err := os.Readlink(binary); err != nil {
						t.Fatal("replaced existing cache symlink")
					}
				}
				return
			}
			if err != nil || marker(out, "CACHE_READY") != "1" {
				t.Fatalf("cache prepare failed: %s %v", out, err)
			}
			data, err := os.ReadFile(binary)
			if err != nil || !bytes.Equal(data, binaryData) {
				t.Fatal("verified binary not published")
			}
			if scenario == "reuse" {
				current, _ := os.Stat(binary)
				if !os.SameFile(original, current) {
					t.Fatal("running shared binary was replaced")
				}
			}
			stages, _ := filepath.Glob(filepath.Join(managed, ".stage.*"))
			if len(stages) != 0 {
				t.Fatal("private stage was not removed")
			}
		})
	}
}
