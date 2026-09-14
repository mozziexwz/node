package relayruntime

import "testing"

func TestV2RuntimeHashOnlyIncludesEffectiveConfiguration(t *testing.T) {
	rule := Rule{ID: "immutable-rule-identity", Version: 1, ListenPort: 21000, Targets: []string{"1.1.1.1:8000", "8.8.8.8:8000"}, AllowedSources: []string{"9.9.9.9", "8.8.4.4"}, Protocol: "tcp", Strategy: "fifo", RateMbps: 5, Billing: true, EntitlementVersion: 1}
	hash, err := RuntimeHash(rule)
	if err != nil {
		t.Fatal(err)
	}
	metadata := rule
	metadata.Version++
	metadata.EntitlementVersion++
	metadata.LeaseUntil = 999999
	metadata.Billing = false
	metadata.AllowedSources = []string{"8.8.4.4", "9.9.9.9"}
	metadata.TargetTLS = []TLSClient{}
	if got, err := RuntimeHash(metadata); err != nil || got != hash {
		t.Fatal("metadata-only update would restart process")
	}
	for _, change := range []func(*Rule){func(r *Rule) { r.ListenPort++ }, func(r *Rule) { r.RateMbps++ }, func(r *Rule) { r.Targets = []string{"8.8.8.8:8000", "1.1.1.1:8000"} }, func(r *Rule) { r.AllowedSources = []string{"9.9.9.9"} }, func(r *Rule) { r.Strategy = "round" }} {
		candidate := rule
		change(&candidate)
		if got, err := RuntimeHash(candidate); err != nil || got == hash {
			t.Fatal("effective configuration change not detected")
		}
	}
	bad := rule
	bad.Protocol = "udp"
	if _, err := RuntimeHash(bad); err == nil {
		t.Fatal("invalid protocol accepted")
	}
	bad = rule
	bad.Protocol = "tls"
	if _, err := RuntimeHash(bad); err == nil {
		t.Fatal("TLS identity validation disabled")
	}
}
