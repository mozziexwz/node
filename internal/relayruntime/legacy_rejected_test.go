package relayruntime

import (
	"context"
	"strings"
	"testing"
)

func TestLegacyLeaseRuntimeRejected(t *testing.T) {
	err := Run(context.Background(), Config{OfflinePolicy: "lease"})
	if err == nil || !strings.Contains(err.Error(), "keep_last") {
		t.Fatal("legacy runtime must not start", err)
	}
}
