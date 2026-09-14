package control

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/mozziexwz/node/internal/relayruntime"
)

func relayRecoveryOrigin(value string) bool {
	u, err := url.Parse(value)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.Path == "" && u.RawQuery == "" && u.Fragment == "" && u.Opaque == "" && !u.ForceQuery
}

// The input/output transport is explicit stdin/stdout for root-owned files.
// Use the dedicated private wrapper: plans contain credentials and forwarding
// secrets and must never be sent to container logging drivers or public logs.
// Existing PG only; no New/openStore, bootstrap, migrations or background jobs.
func RunLocalRelayRecovery(databaseURL, masterKey, publicURL string, args []string, input io.Reader, output io.Writer) error {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		return errors.New("受信中转恢复仅允许原/新控制面 Linux root 本机执行")
	}
	if len(args) == 1 && args[0] == "verify-environment" {
		return VerifyLocalRelayEnvironment(masterKey, publicURL, input, output)
	}
	if len(args) != 1 || args[0] != "inspect" && args[0] != "prepare" && args[0] != "status" && args[0] != "finalize" && args[0] != "tls-inspect" && args[0] != "tls-rotate" {
		return errors.New("relay-recovery 仅接受 inspect/prepare/status/finalize/tls-inspect/tls-rotate")
	}
	if !strings.HasPrefix(databaseURL, "postgres://") && !strings.HasPrefix(databaseURL, "postgresql://") {
		return errors.New("必须使用已存在的 PostgreSQL 站点，不初始化数据库")
	}
	aead, err := backupExportCipher(masterKey, "")
	if err != nil {
		return err
	}
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return errors.New("无法连接原站点数据库")
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	app := &App{Store: &Store{db: db, dialect: "postgres"}, aead: aead, Config: Config{PublicURL: strings.TrimRight(publicURL, "/")}}
	var result any
	if args[0] == "status" {
		err = app.Store.View(func(s *State) error {
			control, _ := LoadDoc[RelayV2Control](s, "relay_v2_control", "default")
			rows := []map[string]any{}
			for _, p := range ListDocs[relayRecoveryPrepared](s, "relay_v2_recovery_prepared") {
				rows = append(rows, map[string]any{"agentId": p.AgentID, "planId": p.PlanID, "allDecided": p.AllDecided, "verifiedAt": p.VerifiedAt, "finalizedAt": p.FinalizedAt})
			}
			result = map[string]any{"controlEpoch": control.Epoch, "recoveryRequired": control.RecoveryRequired, "maintenance": boolSetting(s, "maintenance"), "agents": rows}
			return nil
		})
	} else {
		raw, e := io.ReadAll(io.LimitReader(input, 32<<20+1))
		if e != nil || len(raw) > 32<<20 {
			return errRelayRecovery
		}
		defer clear(raw)
		switch args[0] {
		case "inspect", "prepare":
			var in RelayRecoveryRequest
			if !backupPauseDecode(raw, &in) {
				var snapshot relayruntime.V2RecoverySnapshot
				if args[0] != "inspect" || !backupPauseDecode(raw, &snapshot) {
					return errRelayRecovery
				}
				in.Snapshot = snapshot
			}
			if args[0] == "inspect" {
				err = app.Store.View(func(s *State) error {
					r, e := app.relayRecoveryReport(s, in.Snapshot)
					template := RelayRecoveryRequest{Snapshot: in.Snapshot, Fingerprint: r.Fingerprint, Review: &r, Decisions: []RelayRecoverySelection{}}
					for _, row := range r.Differences {
						template.Decisions = append(template.Decisions, RelayRecoverySelection{RuleID: row.RuleID, Action: "hold"})
					}
					result = template
					return e
				})
			} else {
				result, err = app.prepareRelayRecovery(in, time.Now().UnixMilli())
			}
		case "finalize":
			var in struct {
				Confirmation string `json:"confirmation"`
			}
			if !backupPauseDecode(raw, &in) {
				return errRelayRecovery
			}
			err = app.finalizeRelayRecovery(in.Confirmation, time.Now().UnixMilli())
			result = map[string]string{"message": "受信核对已完成；维护、支付与经营开关未自动打开。"}
		case "tls-inspect", "tls-rotate":
			var in RelayTLSMaintenanceRequest
			if !backupPauseDecode(raw, &in) {
				return errRelayRecovery
			}
			if args[0] == "tls-inspect" {
				err = app.Store.View(func(s *State) error { r, e := app.relayTLSMaintenanceReport(s, in.RuleID); result = r; return e })
			} else {
				result, err = app.rotateRelayTLS(in, time.Now().UnixMilli())
			}
		}
	}
	if err != nil {
		return errRelayRecovery
	}
	if json.NewEncoder(output).Encode(result) != nil {
		return errors.New("结果输出失败；提交状态未知，保留原请求文件重新核对，不要生成替代凭据")
	}
	return nil
}
