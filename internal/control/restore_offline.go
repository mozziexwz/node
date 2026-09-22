package control

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// OfflineRestoreOptions deliberately has no overwrite/current-database option.
// A corrupt production database is neither opened nor changed. Recovery stages a
// new database; selecting it for serving is a separate administrator action.
type OfflineRestoreOptions struct {
	BackupPath          string
	MasterKey           string
	MasterKeyFile       string
	SQLiteOutputDir     string
	PostgresAdminURL    string
	PostgresNewDatabase string
}
type OfflineRestoreResult struct {
	Database         string `json:"database"`
	Users            int    `json:"users"`
	Collections      int    `json:"collections"`
	Maintenance      bool   `json:"maintenance"`
	RecoveryRequired bool   `json:"recoveryRequired"`
	Message          string `json:"message"`
}

func OfflineRestore(opts OfflineRestoreOptions) (OfflineRestoreResult, error) {
	var result OfflineRestoreResult
	if (opts.SQLiteOutputDir == "") == (opts.PostgresNewDatabase == "") {
		return result, errors.New("必须选择一个全新 SQLite 输出目录或一个全新 PostgreSQL 数据库名")
	}
	if opts.MasterKey != "" && opts.MasterKeyFile != "" {
		return result, errors.New("MASTER_KEY 和 --master-key-file 只能选择一种")
	}
	var key []byte
	var err error
	if opts.MasterKeyFile != "" {
		key, err = readRegularLimited(opts.MasterKeyFile, 32)
		if err != nil || len(key) != 32 {
			return result, errors.New("原 master.key 必须是可读取的 32 字节文件")
		}
	} else {
		if opts.MasterKey == "" {
			return result, errors.New("必须提供创建备份时的 MASTER_KEY，或 --master-key-file；不会自动生成新密钥")
		}
		key, err = masterKey(Config{MasterKey: opts.MasterKey})
		if err != nil {
			return result, err
		}
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return result, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return result, err
	}
	b := NewBackupService(&App{aead: aead})
	raw, err := readRegularLimited(opts.BackupPath, 100<<20)
	if err != nil {
		return result, errors.New("不能读取备份普通文件，或文件超过 100 MB")
	}
	state, err := b.unpack(raw)
	if err != nil {
		return result, err
	}
	if err = validateRestoreReferences(state); err != nil {
		return result, err
	}
	if err = validateOfflineFinancialReferences(state); err != nil {
		return result, err
	}
	if err = isolateRestoredState(state, true); err != nil {
		return result, err
	}
	contentAudit(state, "offline-operator", "backup.disaster_restore", "new-isolated-database")
	config := Config{}
	// No destination is created before every backup/key/relationship check above.
	if opts.SQLiteOutputDir != "" {
		absolute, err := filepath.Abs(opts.SQLiteOutputDir)
		if err != nil {
			return result, err
		}
		if err = os.Mkdir(absolute, 0700); err != nil {
			return result, errors.New("恢复目录必须不存在，且其父目录必须已存在；不会覆盖已有数据目录")
		}
		config.DataDir = absolute
		result.Database = filepath.Join(absolute, "msboost.db")
		if err = writePrivateExclusive(filepath.Join(absolute, "master.key"), key); err != nil {
			return result, err
		}
		// Keep the original encrypted input alongside the staged database. It is
		// not a dump of the possibly corrupt old DB and never blocks on that DB.
		if err = writePrivateExclusive(filepath.Join(absolute, "source-backup.msb"), raw); err != nil {
			return result, err
		}
	} else {
		config.DatabaseURL, err = createRecoveryPostgres(opts.PostgresAdminURL, opts.PostgresNewDatabase)
		if err != nil {
			return result, err
		}
		result.Database = opts.PostgresNewDatabase
	}
	store, err := openStore(config)
	if err != nil {
		return result, errors.New("全新恢复目标已创建但无法打开；请保留该隔离目标检查，生产库未修改")
	}
	defer store.Close()
	err = store.Update(func(current *State) error {
		if len(current.Users) != 0 || len(current.Sessions) != 0 || len(current.Docs) != 0 || len(current.Settings) != 0 {
			return errors.New("恢复目标出现已有内容，拒绝覆盖；请使用另一个全新目标")
		}
		*current = *state
		return nil
	})
	if err != nil {
		return result, fmt.Errorf("新目标导入失败，未切换生产库：%w", err)
	}
	err = store.View(func(current *State) error {
		if !boolSetting(current, "maintenance") || len(current.Users) != len(state.Users) {
			return errors.New("恢复后读回校验失败")
		}
		before, e := json.Marshal(state)
		if e != nil {
			return e
		}
		after, e := json.Marshal(current)
		if e != nil {
			return e
		}
		if string(before) != string(after) {
			return errors.New("恢复后读回数据与隔离快照不一致")
		}
		return nil
	})
	if err != nil {
		return result, err
	}
	result.Users, result.Collections, result.Maintenance = len(state.Users), len(state.Docs), true
	result.RecoveryRequired = restoredRelayRecoveryRequired(state)
	result.Message = "完整快照已写入全新隔离数据库并读回校验。旧数据库未打开、未覆盖、未删除；站点保持维护，支付关闭，所有会话与 Agent 凭据已撤销。转发进入 recovery_required：旧节点可能仍在运行，已保留端口、目标、实例及撤销关联，冻结自动覆盖。停止或可信隔离旧站点后，支持恢复协议的 v2 节点可通过本机 root 中转恢复入口逐规则核对和受信接管；其他节点需独立确认停止。财务与营业开关仍须人工核对，不能直接恢复营业。"
	return result, nil
}

func readRegularLimited(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("invalid regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return nil, errors.New("file too large")
	}
	return raw, nil
}

func writePrivateExclusive(path string, raw []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(raw)
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

func createRecoveryPostgres(adminURL, name string) (string, error) {
	targetURL, err := recoveryPostgresTarget(adminURL, name)
	if err != nil {
		return "", err
	}
	db, err := sql.Open("pgx", adminURL)
	if err != nil {
		return "", errors.New("无法创建 PostgreSQL 管理连接")
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// CREATE DATABASE is atomic and fails if the name already exists. We never
	// issue DROP/TRUNCATE or connect to the old application database.
	if _, err = db.ExecContext(ctx, `CREATE DATABASE "`+name+`" TEMPLATE template0`); err != nil {
		return "", errors.New("无法创建全新恢复数据库（名称已存在、权限不足或数据库不可达）；旧库未修改")
	}
	return targetURL, nil
}

func recoveryPostgresTarget(adminURL, name string) (string, error) {
	if !regexp.MustCompile(`^msboost_restore_[a-z0-9_]{1,40}$`).MatchString(name) {
		return "", errors.New("新数据库名须为 msboost_restore_ 开头，仅小写字母、数字、下划线且后缀不超过 40 字符")
	}
	u, err := url.Parse(adminURL)
	if err != nil || u.Host == "" || u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return "", errors.New("需要有效的 MSBOOST_RESTORE_ADMIN_URL，或 Compose 数据库连接环境")
	}
	if u.Path == "/"+name {
		return "", errors.New("管理员连接不能指向待创建的恢复数据库")
	}
	if u.Path != "/postgres" {
		return "", errors.New("恢复管理连接必须指向 /postgres 维护数据库，不连接生产业务库")
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil || u.Fragment != "" {
		return "", errors.New("恢复数据库连接参数无效")
	}
	// pgx query options can override the path/database, host and service. Decode
	// keys first, reject every routing override, then verify effective pgx config.
	allowed := map[string]bool{"sslmode": true, "sslrootcert": true, "sslcert": true, "sslkey": true, "connect_timeout": true}
	for key, values := range query {
		if !allowed[key] || len(values) != 1 {
			return "", errors.New("恢复连接只允许 TLS 与超时参数，不允许数据库、主机或 service 覆盖参数")
		}
	}
	adminConfig, err := pgx.ParseConfig(u.String())
	if err != nil || adminConfig.Database != "postgres" {
		return "", errors.New("PostgreSQL 实际管理连接目标不是 postgres，拒绝恢复")
	}
	u.Path = "/" + name
	u.RawPath = ""
	targetConfig, err := pgx.ParseConfig(u.String())
	if err != nil || targetConfig.Database != name {
		return "", errors.New("PostgreSQL 实际恢复目标不是指定新库，拒绝恢复")
	}
	return u.String(), nil
}

func validateOfflineFinancialReferences(s *State) error {
	for id, user := range s.Users {
		if user == nil || user.ID != id {
			return errors.New("用户身份关系无效")
		}
	}
	for id, raw := range s.Docs["orders"] {
		var order Order
		// Deleting an account intentionally retains its historical orders and
		// ledgers. A missing historical user is not corruption and must not prevent
		// recovery; never fabricate an account or attach its money to a new user.
		if json.Unmarshal(raw, &order) != nil || order.ID != id || order.UserID == "" {
			return errors.New("兑换记录与用户身份关联无效")
		}
		if order.ChannelID != "" && order.ChannelID != "balance" {
			if _, ok := LoadDoc[PaymentChannel](s, "payment_channels", order.ChannelID); !ok {
				return errors.New("兑换记录支付渠道缺失")
			}
		}
	}
	for _, raw := range s.Docs["payment_trades"] {
		var orderID string
		if json.Unmarshal(raw, &orderID) != nil {
			return errors.New("支付去重记录无效")
		}
		if _, ok := LoadDoc[Order](s, "orders", orderID); !ok {
			return errors.New("支付去重记录关联兑换记录缺失")
		}
	}
	for id, raw := range s.Docs["ledger"] {
		var entry LedgerEntry
		if json.Unmarshal(raw, &entry) != nil || entry.ID != id || entry.UserID == "" || entry.BalanceAfter < 0 {
			return errors.New("钱包账本数据无效")
		}
	}
	for id, raw := range s.Docs["cards"] {
		var card BalanceCard
		if json.Unmarshal(raw, &card) != nil || card.ID != id || card.AmountCents < 0 || card.Status == "used" && card.UsedBy == "" {
			return errors.New("兑换码记录或使用关系无效")
		}
	}
	for collection, referenced := range map[string]string{"order_requests": "orders", "redeem_requests": "cards"} {
		for _, raw := range s.Docs[collection] {
			var request commerceIdempotency
			if json.Unmarshal(raw, &request) != nil || request.ObjectID == "" {
				return errors.New("财务请求去重记录无效")
			}
			if _, ok := s.Docs[referenced][request.ObjectID]; !ok {
				return errors.New("财务请求去重记录引用的兑换记录或兑换码缺失")
			}
		}
	}
	rules := map[string]bool{}
	for _, rule := range ListDocs[UserRule](s, "user_rules") {
		rules[rule.ID] = true
	}
	for id, raw := range s.Docs["relay_rule_archive"] {
		var rule UserRule
		if json.Unmarshal(raw, &rule) != nil || id != rule.ID || rule.UserID == "" || rule.TrafficBytes < 0 || rule.InputBytes < 0 || rule.OutputBytes < 0 || rule.TrafficEntitlementVersion < 0 {
			return errors.New("规则计量归档无效")
		}
		rules[id] = true
	}
	for key, raw := range s.Docs["traffic_cursors"] {
		var cursor TrafficCursor
		parts := strings.SplitN(key, ":", 3)
		if json.Unmarshal(raw, &cursor) != nil || len(parts) != 3 || !rules[parts[1]] || cursor.Sequence < 0 || cursor.InputBytes < 0 || cursor.OutputBytes < 0 || cursor.Remainder < 0 || cursor.Remainder >= 1000 {
			return errors.New("流量去重游标或规则关联无效")
		}
	}
	for collection := range map[string]bool{"traffic_months": true, "entitlement_versions": true} {
		for _, raw := range s.Docs[collection] {
			var amount int64
			if json.Unmarshal(raw, &amount) != nil || amount < 0 {
				return errors.New("月流量或权益版本记录无效")
			}
		}
	}
	return nil
}
