package executor

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// TestVerifyField runs the real check, with the downloaded mihomo client (or
// MSBOOST_MIHOMO_BINARY), against live relays and nodes. It is skipped unless
// MSBOOST_VERIFY_FIELD names a directory of client configurations; each file's
// report is logged. Use it for acceptance on a Linux machine:
//
//	go test -c -o verify-field.test ./internal/executor
//	MSBOOST_VERIFY_FIELD=/root/configs ./verify-field.test -test.run TestVerifyField -test.v
func TestVerifyField(t *testing.T) {
	dir := os.Getenv("MSBOOST_VERIFY_FIELD")
	if dir == "" {
		t.Skip("set MSBOOST_VERIFY_FIELD to a directory of client configurations")
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no configurations in %s", dir)
	}
	sort.Strings(files)
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		report := (&Verifier{}).Verify(context.Background(), raw, VerifyOptions{Mode: "log"})
		t.Logf("%s: code=%s game=%s filter=%s elapsed=%dms", filepath.Base(file), report.Code, report.Game, report.Filter, report.ElapsedMS)
	}
}
