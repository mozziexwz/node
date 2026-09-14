package control

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/mozziexwz/node/internal/relayruntime"
)

func billingV2Fixture(t *testing.T) (*State, RelayAgent, UserRule, relayruntime.V2Traffic, int64) {
	t.Helper()
	s := newState()
	agent := RelayAgent{ID: "billing-entry"}
	s.Users["member"] = &User{ID: "member", Status: "active", TrafficTotal: 10000000}
	if err := SaveDoc(s, "entitlement_versions", "member", int64(1)); err != nil {
		t.Fatal(err)
	}
	rule := UserRule{ID: "rule-instance-unique-123", UserID: "member", RouteID: "route", Version: 1, EntitlementVersion: 1, TrafficMode: "both", TrafficMultiplierPermille: 1000}
	runtime := relayruntime.Rule{ID: rule.ID, Version: 1, EntitlementVersion: 1, Billing: true}
	grant, err := grantRelayBillingV2(s, agent.ID, rule, runtime)
	if err != nil {
		t.Fatal(err)
	}
	if err = SaveDoc(s, "user_rules", "member:route", rule); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	sample := relayruntime.V2Traffic{Traffic: relayruntime.Traffic{ID: rule.ID, Version: 1, Epoch: "billing-stream-unique-epoch", Sequence: 1, InputBytes: 100, OutputBytes: 200, EntitlementVersion: 1}, BillingPeriodID: grant, CollectedFrom: now, CollectedUntil: now + 1000}
	return s, agent, rule, sample, now + 2000
}

func TestRelayV2TrafficHistoricalPeriodDedupAndExplicitAck(t *testing.T) {
	s, agent, rule, sample, now := billingV2Fixture(t)
	for i := 0; i < 3; i++ {
		ack, err := applyRelayTrafficV2(s, agent, sample, now)
		if err != nil || ack.Epoch != sample.Epoch || ack.Sequence != 1 {
			t.Fatal("valid sample not explicitly acknowledged", err)
		}
	}
	if s.Users["member"].TrafficUsed != 300 {
		t.Fatal("retransmission charged twice")
	}
	period, _ := LoadDoc[RelayBillingPeriodV2](s, "relay_v2_billing_periods", "member:1")
	if period.ConfirmedBytes != 300 {
		t.Fatal("historical ledger missing")
	}
	if err := SaveDoc(s, "entitlement_versions", "member", int64(2)); err != nil {
		t.Fatal(err)
	}
	s.Users["member"].TrafficUsed = 0
	sample.Sequence++
	sample.InputBytes = 150
	sample.OutputBytes = 250
	sample.CollectedUntil++
	if _, err := applyRelayTrafficV2(s, agent, sample, now); err != nil {
		t.Fatal(err)
	}
	period, _ = LoadDoc[RelayBillingPeriodV2](s, "relay_v2_billing_periods", "member:1")
	if period.ConfirmedBytes != 400 || s.Users["member"].TrafficUsed != 0 {
		t.Fatal("old period use lost or charged to replacement package")
	}
	archived, _ := LoadDoc[UserRule](s, "user_rules", "member:route")
	DeleteDoc(s, "user_rules", "member:route")
	if err := SaveDoc(s, "relay_rule_archive", rule.ID, archived); err != nil {
		t.Fatal(err)
	}
	sample.Sequence++
	sample.InputBytes = 200
	sample.CollectedUntil++
	if _, err := applyRelayTrafficV2(s, agent, sample, now); err != nil {
		t.Fatal(err)
	}
	archived, _ = LoadDoc[UserRule](s, "relay_rule_archive", rule.ID)
	if archived.TrafficBytes != 450 || len(s.Docs["user_rules"]) != 0 {
		t.Fatal("late report lost or resurrected archived rule")
	}
}

func TestRelayV2TrafficCollectionMonthAndUncertainIntervals(t *testing.T) {
	s, agent, _, sample, _ := billingV2Fixture(t)
	start := time.Date(2026, 1, 31, 23, 59, 0, 0, time.UTC).UnixMilli()
	grant, _ := LoadDoc[RelayBillingGrantV2](s, "relay_v2_billing_grants", sample.BillingPeriodID)
	grant.CreatedAt = start
	if err := SaveDoc(s, "relay_v2_billing_grants", grant.ID, grant); err != nil {
		t.Fatal(err)
	}
	sample.CollectedFrom = start
	sample.CollectedUntil = start + 1000
	received := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	if _, err := applyRelayTrafficV2(s, agent, sample, received); err != nil {
		t.Fatal(err)
	}
	january, _ := LoadDoc[int64](s, "traffic_months", "member:2026-01")
	march, _ := LoadDoc[int64](s, "traffic_months", "member:2026-03")
	if january != 300 || march != 0 {
		t.Fatal("late traffic assigned to receive month")
	}
	sample.Epoch = "boundary-stream-unique-epoch"
	sample.CollectedUntil = start + 120000
	sample.Uncertain = true
	if _, err := applyRelayTrafficV2(s, agent, sample, received); err != nil {
		t.Fatal(err)
	}
	period, _ := LoadDoc[RelayBillingPeriodV2](s, "relay_v2_billing_periods", "member:1")
	if period.ConfirmedBytes != 300 || period.ReviewBytes != 300 || s.Users["member"].TrafficUsed != 300 || len(s.Docs["relay_v2_traffic_review"]) != 1 {
		t.Fatal("uncertain interval lost or silently charged as precise")
	}
}

func TestRelayV2TrafficInvalidSampleIsolatedBeforeMutation(t *testing.T) {
	for _, kind := range []string{"foreign", "grant", "negative", "future-version", "overflow", "mutated-epoch"} {
		t.Run(kind, func(t *testing.T) {
			s, agent, _, sample, now := billingV2Fixture(t)
			switch kind {
			case "foreign":
				agent.ID = "other-agent"
			case "grant":
				sample.BillingPeriodID = "unknown"
			case "negative":
				sample.InputBytes = -1
			case "future-version":
				sample.Version = 100
			case "overflow":
				s.Users["member"].TrafficUsed = math.MaxInt64
			case "mutated-epoch":
				if _, err := applyRelayTrafficV2(s, agent, sample, now); err != nil {
					t.Fatal(err)
				}
				sample.CollectedFrom++
				sample.Sequence++
			}
			before, _ := json.Marshal(s)
			ack, err := applyRelayTrafficV2(s, agent, sample, now)
			after, _ := json.Marshal(s)
			if err == nil || ack.Epoch != "" || string(before) != string(after) {
				t.Fatal("invalid sample acknowledged or partially mutated state")
			}
		})
	}
}

func TestRelayV2TrafficOnlyGrantedIngressBills(t *testing.T) {
	s, agent, rule, sample, now := billingV2Fixture(t)
	grant, err := grantRelayBillingV2(s, agent.ID, rule, relayruntime.Rule{ID: rule.ID, Version: 1, EntitlementVersion: 1, Billing: false})
	if err != nil {
		t.Fatal(err)
	}
	sample.BillingPeriodID = grant
	if _, err = applyRelayTrafficV2(s, agent, sample, now); err != nil {
		t.Fatal(err)
	}
	if s.Users["member"].TrafficUsed != 0 || len(s.Docs["relay_v2_billing_periods"]) != 0 {
		t.Fatal("downstream traffic double billed")
	}
}

func TestRelayV2BillingGrantRejectsMismatchedEntitlement(t *testing.T) {
	s, agent, rule, _, _ := billingV2Fixture(t)
	before, _ := json.Marshal(s)
	if _, err := grantRelayBillingV2(s, agent.ID, rule, relayruntime.Rule{ID: rule.ID, Version: 1, EntitlementVersion: rule.EntitlementVersion + 1, Billing: true}); err == nil {
		t.Fatal("grant silently changed billing period")
	}
	after, _ := json.Marshal(s)
	if string(before) != string(after) {
		t.Fatal("invalid grant mutated state")
	}
}
