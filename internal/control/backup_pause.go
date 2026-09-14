package control

import (
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"runtime"
	"strings"
	"time"
)

var (
	ErrBackupPauseActive  = errors.New("整站备份已冻结控制面写入，请等待本机备份流程释放门禁")
	errBackupPauseBlocked = errors.New("停站备份预检未通过，未冻结写入、未允许停止服务")
	errBackupPauseOwner   = errors.New("停站备份门禁属于另一操作；只能使用原私有令牌解除")
	errBackupPauseCorrupt = errors.New("停站备份门禁记录损坏，保持冻结；请核对数据库，不能自动清除")
	errBackupPauseMode    = errors.New("本次门禁模式与原请求不一致；请使用原令牌解除后重新逐次确认")
)

const backupPauseCollection = "backup_pause"
const backupPauseMaintenanceConfirmation = "BACKUP_WITH_RELAY_INTERRUPTION"

// There is deliberately no expiry. A timer cannot prove that a snapshot or a
// stopped control-plane process has finished. Only the matching local owner may
// release this gate. Neither this hash nor the bearer token is returned by APIs.
type backupPauseGate struct {
	Version       int      `json:"version"`
	TokenHash     string   `json:"tokenHash"`
	AcquiredAt    int64    `json:"acquiredAt"`
	CheckedRules  int      `json:"checkedRules"`
	Mode          string   `json:"mode,omitempty"`
	AcceptedRisks []string `json:"acceptedRisks,omitempty"`
}

type BackupPauseResult struct {
	BackupPauseReport
	GateActive    bool     `json:"gateActive"`
	AcquiredAt    int64    `json:"acquiredAt,omitempty"`
	Released      bool     `json:"released,omitempty"`
	Message       string   `json:"message"`
	Mode          string   `json:"mode,omitempty"`
	AcceptedRisks []string `json:"acceptedRisks,omitempty"`
}

func backupPauseGatePresent(s *State) bool { return len(s.Docs[backupPauseCollection]) != 0 }

func backupPauseReadGate(s *State) (backupPauseGate, bool, error) {
	var gate backupPauseGate
	if !backupPauseGatePresent(s) {
		return gate, false, nil
	}
	raw, ok := s.Docs[backupPauseCollection]["default"]
	if !ok || len(s.Docs[backupPauseCollection]) != 1 || len(raw) > 1024 {
		return gate, true, errBackupPauseCorrupt
	}
	if !backupPauseDecode(raw, &gate) || gate.Version != 1 || gate.AcquiredAt <= 0 || gate.CheckedRules < 0 || !backupPauseValidToken(gate.TokenHash) {
		return gate, true, errBackupPauseCorrupt
	}
	if gate.Mode == "" {
		gate.Mode = "strict"
	}
	if gate.Mode != "strict" && gate.Mode != "maintenance" || gate.Mode == "strict" && len(gate.AcceptedRisks) != 0 || len(gate.AcceptedRisks) > 2 {
		return gate, true, errBackupPauseCorrupt
	}
	seen := map[string]bool{}
	for _, risk := range gate.AcceptedRisks {
		if !backupPauseMaintenanceRisk(risk) || seen[risk] {
			return gate, true, errBackupPauseCorrupt
		}
		seen[risk] = true
	}
	return gate, true, nil
}

func backupPauseValidToken(token string) bool {
	decoded, err := hex.DecodeString(token)
	return len(token) == 64 && err == nil && len(decoded) == 32 && strings.ToLower(token) == token
}

func backupPauseReadToken(input io.Reader) (string, error) {
	raw, err := io.ReadAll(io.LimitReader(input, 66))
	defer clear(raw)
	if err != nil || len(raw) != 65 || raw[64] != '\n' || !backupPauseValidToken(string(raw[:64])) {
		return "", errors.New("备份门禁令牌输入无效；必须从原私有文件读取，不接受命令行令牌")
	}
	return string(raw[:64]), nil
}

func backupPauseAcquire(store *Store, token string, now int64) (BackupPauseResult, error) {
	return backupPauseAcquireMode(store, token, now, "strict")
}

func backupPauseReadMaintenanceToken(input io.Reader) (string, error) {
	raw, err := io.ReadAll(io.LimitReader(input, 129))
	defer clear(raw)
	parts := strings.Split(string(raw), "\n")
	if err != nil || len(parts) != 3 || parts[2] != "" || !backupPauseValidToken(parts[0]) || parts[1] != backupPauseMaintenanceConfirmation {
		return "", errors.New("手动维护备份要求原私有令牌和精确逐次中断风险确认；不接受自动确认参数")
	}
	return parts[0], nil
}

func backupPauseAcquireMode(store *Store, token string, now int64, mode string) (BackupPauseResult, error) {
	var result BackupPauseResult
	if !backupPauseValidToken(token) || now <= 0 || mode != "strict" && mode != "maintenance" {
		return result, errors.New("备份门禁输入无效")
	}
	err := store.transactionWithBackupPause(true, true, func(s *State) error {
		gate, active, err := backupPauseReadGate(s)
		if err != nil {
			return err
		}
		if active {
			if subtle.ConstantTimeCompare([]byte(gate.TokenHash), []byte(tokenHash(token))) != 1 {
				return errBackupPauseOwner
			}
			if gate.Mode != mode {
				return errBackupPauseMode
			}
			// An uncertain first reply is safe to retry even after observations
			// become stale: all writers have remained frozen since acquisition.
			result.GateActive, result.AcquiredAt = true, gate.AcquiredAt
			result.CanPauseControl, result.CanPauseMaintenance, result.CheckedRules = gate.Mode == "strict" || len(gate.AcceptedRisks) == 0, true, gate.CheckedRules
			result.Mode, result.AcceptedRisks = gate.Mode, append([]string(nil), gate.AcceptedRisks...)
			return nil
		}
		result.BackupPauseReport = backupPausePreflight(s, now)
		if mode == "strict" && !result.CanPauseControl || mode == "maintenance" && !result.CanPauseMaintenance {
			return errBackupPauseBlocked
		}
		result.Mode = mode
		if mode == "maintenance" {
			seen := map[string]bool{}
			for _, blocker := range result.Blockers {
				if !seen[blocker.Code] {
					result.AcceptedRisks = append(result.AcceptedRisks, blocker.Code)
					seen[blocker.Code] = true
				}
			}
		}
		gate = backupPauseGate{Version: 1, TokenHash: tokenHash(token), AcquiredAt: now, CheckedRules: result.CheckedRules, Mode: mode, AcceptedRisks: append([]string(nil), result.AcceptedRisks...)}
		if err = SaveDoc(s, backupPauseCollection, "default", gate); err != nil {
			return err
		}
		result.GateActive, result.AcquiredAt = true, now
		if mode == "maintenance" {
			contentAudit(s, "local-root", "backup.maintenance.interruption_accepted", strings.Join(result.AcceptedRisks, ","))
		}
		return nil
	})
	if err == nil {
		result.Message = "停站备份门禁已持久化；控制面写入已冻结，导出完成后须用原令牌释放。节点本身未被停止。"
		if mode == "maintenance" {
			result.Message = "手动维护备份门禁已建立；已明确接受旧链路短租约中断风险，不保证现有或新建连接持续可用。所有其他安全门禁保持有效。"
		}
	}
	return result, err
}

func backupPauseRelease(store *Store, token string) (BackupPauseResult, error) {
	var result BackupPauseResult
	if !backupPauseValidToken(token) {
		return result, errors.New("备份门禁输入无效")
	}
	err := store.transactionWithBackupPause(true, true, func(s *State) error {
		gate, active, err := backupPauseReadGate(s)
		if err != nil {
			return err
		}
		if active && subtle.ConstantTimeCompare([]byte(gate.TokenHash), []byte(tokenHash(token))) != 1 {
			return errBackupPauseOwner
		}
		delete(s.Docs, backupPauseCollection)
		if active && gate.Mode == "maintenance" {
			contentAudit(s, "local-root", "backup.maintenance.gate_released", strings.Join(gate.AcceptedRisks, ","))
		}
		result.Mode = gate.Mode
		result.Released = true
		return nil
	})
	if err == nil {
		result.Message = "本次停站备份门禁已解除；此结果不代表服务容器已经恢复健康。"
	}
	return result, err
}

func backupPauseStatus(store *Store, now int64) (BackupPauseResult, error) {
	var result BackupPauseResult
	err := store.View(func(s *State) error {
		result.BackupPauseReport = backupPausePreflight(s, now)
		gate, active, err := backupPauseReadGate(s)
		result.GateActive, result.AcquiredAt = active, gate.AcquiredAt
		result.Mode, result.AcceptedRisks = gate.Mode, append([]string(nil), gate.AcceptedRisks...)
		if active {
			result.CanPauseControl = false
			result.CanPauseMaintenance = false
		}
		return err
	})
	result.Message = "只读状态检查；没有获取停站许可。必须由同一备份流程 begin 成功后才可停站。"
	return result, err
}

// RunLocalBackupPause never calls New/openStore or starts an application worker.
// It only operates on an already provisioned PostgreSQL state row. Database
// credentials retain the existing deployment's environment transport; the
// backup gate's separate random bearer token travels exclusively over stdin.
func RunLocalBackupPause(databaseURL string, args []string, input io.Reader, output io.Writer) error {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		return errors.New("停站备份门禁仅允许目标 Linux 服务器 root 执行")
	}
	if len(args) != 1 || (args[0] != "begin" && args[0] != "begin-maintenance" && args[0] != "end" && args[0] != "check" && args[0] != "status") {
		return errors.New("backup-pause 仅接受 begin、begin-maintenance、end、check 或 status；不接受令牌或自动确认参数")
	}
	if !strings.HasPrefix(databaseURL, "postgres://") && !strings.HasPrefix(databaseURL, "postgresql://") {
		return errors.New("停站备份门禁要求现有 PostgreSQL 数据库")
	}
	var token string
	var err error
	if args[0] == "begin" || args[0] == "end" {
		token, err = backupPauseReadToken(input)
		if err != nil {
			return err
		}
	}
	if args[0] == "begin-maintenance" {
		token, err = backupPauseReadMaintenanceToken(input)
		if err != nil {
			return err
		}
	}
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return errors.New("无法打开现有备份门禁数据库")
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &Store{db: db, dialect: "postgres"}
	var result BackupPauseResult
	switch args[0] {
	case "begin":
		result, err = backupPauseAcquire(store, token, time.Now().UnixMilli())
	case "begin-maintenance":
		result, err = backupPauseAcquireMode(store, token, time.Now().UnixMilli(), "maintenance")
	case "end":
		result, err = backupPauseRelease(store, token)
	default:
		result, err = backupPauseStatus(store, time.Now().UnixMilli())
		if err == nil && args[0] == "check" && !result.CanPauseControl {
			err = errBackupPauseBlocked
		}
	}
	if err == nil || errors.Is(err, errBackupPauseBlocked) {
		if encodeErr := json.NewEncoder(output).Encode(result); encodeErr != nil {
			return errors.New("门禁结果输出失败，提交状态未确认；请使用原令牌安全重试或释放")
		}
	}
	if err == nil || errors.Is(err, errBackupPauseBlocked) || errors.Is(err, errBackupPauseOwner) || errors.Is(err, errBackupPauseCorrupt) || errors.Is(err, errBackupPauseMode) {
		return err
	}
	return errors.New("备份门禁数据库操作未确认完成；未创建或初始化数据库。请保留原私有令牌并核对状态")
}
