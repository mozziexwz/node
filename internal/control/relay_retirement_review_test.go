package control

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mozziexwz/node/internal/relayruntime"
)

func TestRelayRetirementReinsertedIdentityCannotAuthenticateOrEnroll(t *testing.T) {
	for _, operation := range []string{"bearer", "register", "renew"} {
		t.Run(operation, func(t *testing.T) {
			f, _ := retirementFixture(t)
			retireFixtureAgents(t, f)
			enrollment := commerceID() + commerceID()
			// Simulate a damaged restore reinserting a live-looking old row. The
			// retirement guard must deny it independently of ordinary credentials.
			if err := f.app.Store.Update(func(s *State) error {
				agent := f.agents[0]
				agent.Enabled = true
				agent.TokenHash = commerceHash(f.tokens[0])
				agent.EnrollmentHash, agent.EnrollmentExpires = commerceHash(enrollment), time.Now().Add(time.Hour).UnixMilli()
				return SaveDoc(s, "relay_agents", agent.ID, agent)
			}); err != nil {
				t.Fatal(err)
			}
			before := recoveryState(t, f.app)
			switch operation {
			case "bearer":
				f.send(t, 0, f.requests[0], 401)
			case "register":
				w := commerceTestRequest(f.mux, nil, http.MethodPost, "/api/relay-agent/register", map[string]string{"enrollmentToken": enrollment})
				if w.Code != 401 {
					t.Fatalf("retired registration HTTP %d, want 401", w.Code)
				}
			case "renew":
				admin := &User{ID: "review-admin", Role: "admin", Status: "active"}
				w := commerceTestRequest(f.mux, admin, http.MethodPost, "/api/admin/relay-agents/"+f.agents[0].ID+"/enrollment", nil)
				if w.Code != 409 {
					t.Fatalf("retired credential renewal HTTP %d, want 409", w.Code)
				}
			}
			if after := recoveryState(t, f.app); after != before {
				t.Fatal("denied retired identity operation changed persistent state")
			}
		})
	}
}

func TestRelayRecoveryFinalizeRejectsAmbiguousDefaultControl(t *testing.T) {
	for _, kind := range []string{"duplicate epoch", "case-folded recovery", "unknown field", "second default", "invalid epoch"} {
		t.Run(kind, func(t *testing.T) {
			f, snapshots := recoveryFixture(t)
			epoch := ""
			for i, snapshot := range snapshots {
				plan, err := f.app.prepareRelayRecovery(recoveryInput(snapshot, "adopt"), time.Now().UnixMilli())
				if err != nil {
					t.Fatal(err)
				}
				recoveryObserve(t, f, i, plan)
				epoch = plan.ControlEpoch
			}
			if err := f.app.Store.Update(func(s *State) error {
				raw := strings.TrimSuffix(string(s.Docs["relay_v2_control"]["default"]), "}")
				switch kind {
				case "duplicate epoch":
					raw += `,"epoch":"` + epoch + `"}`
				case "case-folded recovery":
					raw += `,"RecoveryRequired":true}`
				case "unknown field":
					raw += `,"unrecognized":true}`
				case "second default":
					s.Docs["relay_v2_control"]["other"] = append(json.RawMessage(nil), s.Docs["relay_v2_control"]["default"]...)
					return nil
				case "invalid epoch":
					return SaveDoc(s, "relay_v2_control", "default", RelayV2Control{Epoch: "bad epoch", RecoveryRequired: true})
				}
				s.Docs["relay_v2_control"]["default"] = json.RawMessage(raw)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			before := recoveryState(t, f.app)
			if err := f.app.finalizeRelayRecovery("OLD_CONTROL_ISOLATED_AND_FINANCES_REVIEWED "+epoch, time.Now().UnixMilli()); err == nil {
				t.Fatal("ambiguous default control released isolation")
			}
			if recoveryState(t, f.app) != before {
				t.Fatal("failed strict control check partially committed")
			}
		})
	}
}

func TestRelayRecoveryFinalizeRejectsDuplicateSealedPlanDecision(t *testing.T) {
	f, snapshots := recoveryFixture(t)
	extra := snapshots[0].Records[0]
	extra.RuleID, extra.Command.CommandID = commerceID(), commerceID()
	extra.Command.RuleID = extra.RuleID
	extraRule := *extra.Command.Rule
	extraRule.ID = extra.RuleID
	extraRule.ListenPort += 100
	extra.Command.Rule = &extraRule
	extra.Command.RuntimeHash, _ = relayruntime.RuntimeHash(extraRule)
	extra.RuntimeHash = extra.Command.RuntimeHash
	last := extra.Command
	extra.LastApplied = &last
	snapshots[0].Records = append(snapshots[0].Records, extra)
	epoch := ""
	for i, snapshot := range snapshots {
		in := recoveryInput(snapshot, "adopt")
		if i == 0 {
			in.Decisions[1].Action = "stop"
		}
		plan, err := f.app.prepareRelayRecovery(in, time.Now().UnixMilli())
		if err != nil {
			t.Fatal(err)
		}
		if out := recoveryObserve(t, f, i, plan); out.Status != "ready" {
			t.Fatal("fixture did not verify whole two-record plan")
		}
		epoch = plan.ControlEpoch
	}
	if err := f.app.Store.View(f.app.validateRelayRecoveryFinalization); err != nil {
		t.Fatalf("valid two-record plan rejected before mutation: %v", err)
	}
	if err := f.app.Store.Update(func(s *State) error {
		id := f.agents[0].ID
		prepared, _ := LoadDoc[relayRecoveryPrepared](s, "relay_v2_recovery_prepared", id)
		raw, err := f.app.Open(prepared.SealedPlan)
		var plan relayruntime.V2RecoveryPlan
		if err != nil || json.Unmarshal(raw, &plan) != nil || len(plan.Decisions) != 2 {
			t.Fatal("invalid fixture plan")
		}
		plan.Decisions[1] = plan.Decisions[0]
		raw, _ = json.Marshal(plan)
		prepared.SealedPlan, err = f.app.Seal(raw)
		if err != nil {
			return err
		}
		return SaveDoc(s, "relay_v2_recovery_prepared", id, prepared)
	}); err != nil {
		t.Fatal(err)
	}
	before := recoveryState(t, f.app)
	if err := f.app.finalizeRelayRecovery("OLD_CONTROL_ISOLATED_AND_FINANCES_REVIEWED "+epoch, time.Now().UnixMilli()); err == nil {
		t.Fatal("duplicate plan decision hid a missing reviewed rule")
	}
	if recoveryState(t, f.app) != before {
		t.Fatal("failed duplicate-decision check changed persistent state")
	}
}
