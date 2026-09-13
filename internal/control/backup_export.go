package control

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
)

// ExportPostgresBackup is the read-only half of offline disaster recovery. It
// deliberately does not call New/openStore: exporting must not bootstrap users,
// migrate a database, start workers or create a missing control_state table.
func ExportPostgresBackup(databaseURL, key, keyFile string, output io.Writer) error {
	if !strings.HasPrefix(databaseURL, "postgres://") && !strings.HasPrefix(databaseURL, "postgresql://") {
		return errors.New("整站导出要求现有 PostgreSQL 数据库连接")
	}
	aead, err := backupExportCipher(key, keyFile)
	if err != nil {
		return err
	}
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return errors.New("无法打开备份数据库连接")
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return errors.New("无法开始只读备份事务")
	}
	defer tx.Rollback()
	var raw string
	// Refuse oversized rows at the database before allocating the snapshot.
	err = tx.QueryRowContext(ctx, "SELECT payload FROM control_state WHERE id=1 AND octet_length(payload)<=104857600").Scan(&raw)
	if err != nil {
		return errors.New("无法读取现有站点快照，或快照超过 100 MB；没有创建或修改业务库")
	}
	if err = tx.Commit(); err != nil {
		return errors.New("只读备份事务失败")
	}
	return exportStateBackup([]byte(raw), aead, output)
}

func backupExportCipher(key, keyFile string) (cipher.AEAD, error) {
	if (key == "") == (keyFile == "") {
		return nil, errors.New("必须且只能提供原 MASTER_KEY 或原始 master.key 文件，不会生成替代密钥")
	}
	var raw []byte
	var err error
	if keyFile != "" {
		raw, err = readRegularLimited(keyFile, 32)
		if err != nil || len(raw) != 32 {
			return nil, errors.New("原 master.key 必须是 32 字节普通文件")
		}
	} else {
		raw, err = masterKey(Config{MasterKey: key})
		if err != nil {
			return nil, err
		}
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func exportStateBackup(raw []byte, aead cipher.AEAD, output io.Writer) error {
	var state State
	if len(raw) > 100<<20 || json.Unmarshal(raw, &state) != nil || state.Users == nil || state.Sessions == nil || state.Settings == nil || state.Docs == nil {
		return errors.New("数据库快照结构无效")
	}
	if err := validateRestoreReferences(&state); err != nil {
		return err
	}
	if err := validateOfflineFinancialReferences(&state); err != nil {
		return err
	}
	b := NewBackupService(&App{aead: aead})
	packed, err := b.pack(&state)
	if err != nil {
		return err
	}
	if _, err = b.unpack(packed); err != nil {
		return err
	}
	_, err = output.Write(packed)
	return err
}
