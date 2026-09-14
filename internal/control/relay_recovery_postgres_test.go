package control

import (
	"bytes"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/mozziexwz/node/internal/relayruntime"
)

func TestRelayRecoveryPostgresConcurrentPrepare(t *testing.T) {
	adminURL := os.Getenv("MSBOOST_TEST_POSTGRES_URL")
	if adminURL == "" {
		t.Skip("isolated PostgreSQL integration URL not set")
	}
	name := "msboost_restore_relay_" + hex.EncodeToString([]byte(ID()[:8]))
	target, err := createRecoveryPostgres(adminURL, name)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		db, e := sql.Open("pgx", adminURL)
		if e == nil {
			defer db.Close()
			_, _ = db.Exec(`DROP DATABASE "` + name + `" WITH (FORCE)`)
		}
	}()
	f, snapshots := recoveryFixture(t)
	var state State
	if err := f.app.Store.View(func(s *State) error { raw, _ := json.Marshal(s); return json.Unmarshal(raw, &state) }); err != nil {
		t.Fatal(err)
	}
	first, err := openStore(Config{DatabaseURL: target})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if err = first.Update(func(s *State) error { *s = state; return nil }); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("pgx", target)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	second := &Store{db: db, dialect: "postgres"}
	apps := []*App{{Store: first, aead: f.app.aead, Config: f.app.Config}, {Store: second, aead: f.app.aead, Config: f.app.Config}}
	plans := make([]relayruntime.V2RecoveryPlan, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range apps {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			plans[i], errs[i] = apps[i].prepareRelayRecovery(recoveryInput(snapshots[0], "adopt"), time.Now().UnixMilli())
		}(i)
	}
	close(start)
	wg.Wait()
	for _, e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	if recoveryDigest(plans[0]) != recoveryDigest(plans[1]) {
		t.Fatal("concurrent original requests rotated token/plan instead of idempotent transaction")
	}
	if err := second.View(func(s *State) error {
		if !restoredRelayRecoveryRequired(s) || !boolSetting(s, "maintenance") {
			t.Fatal("PG prepare failed to preserve recovery/maintenance")
		}
		if len(s.Docs["relay_v2_recovery_prepared"]) != 1 {
			t.Fatal("duplicate prepared plans")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "linux" && os.Geteuid() == 0 {
		key, e := masterKey(f.app.Config)
		if e != nil {
			t.Fatal(e)
		}
		keyText := hex.EncodeToString(key)
		call := func(action string, in any) []byte {
			t.Helper()
			raw, _ := json.Marshal(in)
			var out bytes.Buffer
			if err := RunLocalRelayRecovery(target, keyText, f.app.Config.PublicURL, []string{action}, bytes.NewReader(raw), &out); err != nil {
				t.Fatal(action, err)
			}
			return out.Bytes()
		}
		// Exercise the actual root-only CLI transport, including the directly
		// editable inspect template. No private output goes into test logs.
		raw := call("inspect", snapshots[0])
		var inspected RelayRecoveryRequest
		if json.Unmarshal(raw, &inspected) != nil || inspected.Review == nil || len(inspected.Decisions) != 1 || inspected.Decisions[0].Action != "hold" || inspected.Confirmation != "" {
			t.Fatal("CLI inspection did not produce conservative private request template")
		}
		for i, snapshot := range snapshots {
			raw := call("prepare", recoveryInput(snapshot, "adopt"))
			var plan relayruntime.V2RecoveryPlan
			if json.Unmarshal(raw, &plan) != nil {
				t.Fatal("invalid private plan result")
			}
			in := f.requests[i]
			in.Sequence += 100
			in.ControlEpoch, in.AppliedRevision = plan.ControlEpoch, plan.Revision
			in.Acks = []relayruntime.V2Ack{v2Ready(*plan.Decisions[0].Command)}
			if err := first.Update(func(s *State) error {
				agent, _ := LoadDoc[RelayAgent](s, "relay_agents", plan.AgentID)
				out := relayruntime.V2SyncResponse{PreviousRevision: in.AppliedRevision, Commands: []relayruntime.V2Command{}, TrafficAcks: []relayruntime.V2TrafficAck{}}
				handled, e := relayRecoverySync(s, &agent, in, &out, time.Now().UnixMilli())
				if !handled || out.Status != "ready" || len(out.Commands) != 0 {
					t.Fatal("real PG did not confirm empty recovery response")
				}
				return e
			}); err != nil {
				t.Fatal(err)
			}
		}
		call("status", nil)
		call("finalize", map[string]string{"confirmation": "OLD_CONTROL_ISOLATED_AND_FINANCES_REVIEWED " + plans[0].ControlEpoch})
		if err := first.View(func(s *State) error {
			if restoredRelayRecoveryRequired(s) || !boolSetting(s, "maintenance") {
				t.Fatal("CLI incorrectly released maintenance or retained reviewed recovery")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	} else {
		t.Log("root-only CLI subcase requires Linux root; PG concurrent transaction assertions ran")
	}
}
