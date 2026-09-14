package control

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/mail"
	"os"
	"runtime"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// ChangeLocalAdminPassword is only called by the installed, root-operated
// recovery binary. Secrets arrive on stdin, never arguments or environment.
// Unlike New/openStore, this path cannot initialize an account, table or DB.
func ChangeLocalAdminPassword(databaseURL string, input io.Reader) error {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		return errors.New("管理员本机改密仅允许 Linux root，通过安装器交互执行")
	}
	if !strings.HasPrefix(databaseURL, "postgres://") && !strings.HasPrefix(databaseURL, "postgresql://") {
		return errors.New("管理员本机改密要求现有 PostgreSQL 数据库")
	}
	email, hash, err := readAdminPasswordInput(input)
	if err != nil {
		return err
	}
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return errors.New("无法打开现有管理员数据库")
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	// Store.Update locks control_state with SELECT FOR UPDATE and re-reads the
	// latest payload in the SAME transaction. The server has no State cache;
	// other writes therefore cannot be overwritten by an earlier snapshot.
	store := &Store{db: db, dialect: "postgres"}
	return changeExistingAdminPassword(store, email, hash)
}

// The fixed, bounded stdin frame has four newline-terminated fields. Keeping
// parsing separate permits offline tests without weakening the root gate.
func readAdminPasswordInput(input io.Reader) (string, []byte, error) {
	raw, err := io.ReadAll(io.LimitReader(input, 1025))
	defer clear(raw)
	if err != nil || len(raw) > 1024 {
		return "", nil, errors.New("改密输入不完整或过长；未修改密码")
	}
	fields := bytes.Split(raw, []byte{'\n'})
	if len(fields) != 5 || len(fields[4]) != 0 || bytes.ContainsAny(raw, "\x00\r") {
		return "", nil, errors.New("改密输入格式无效；未修改密码")
	}
	email := strings.ToLower(strings.TrimSpace(string(fields[0])))
	address, err := mail.ParseAddress(email)
	if err != nil || address.Address != email || address.Name != "" || len(email) > 254 {
		return "", nil, errors.New("管理员邮箱格式无效；未修改密码")
	}
	if len(fields[1]) < 12 || len(fields[1]) > 72 || !bytes.Equal(fields[1], fields[2]) {
		return "", nil, errors.New("密码须为 12–72 字节且两次一致；未修改密码")
	}
	if string(fields[3]) != "RESET "+email {
		return "", nil, errors.New("未明确确认目标管理员；未修改密码")
	}
	hash, err := bcrypt.GenerateFromPassword(fields[1], bcrypt.DefaultCost)
	if err != nil {
		return "", nil, errors.New("密码处理失败；未修改密码")
	}
	return email, hash, nil
}

func changeExistingAdminPassword(store *Store, email string, hash []byte) error {
	err := store.Update(func(s *State) error {
		var target *User
		for id, user := range s.Users {
			if user != nil && strings.EqualFold(strings.TrimSpace(user.Email), email) {
				if target != nil || user.ID == "" || user.ID != id {
					return errors.New("管理员邮箱存在重复记录")
				}
				target = user
			}
		}
		if target == nil || target.Role != "admin" {
			return errors.New("目标不是已存在的管理员")
		}
		// A challenge for an older credential must never reverse this local
		// recovery. Other users and unrelated email purposes remain unchanged.
		for id, raw := range s.Docs["email_challenges"] {
			var challenge emailChallenge
			if json.Unmarshal(raw, &challenge) != nil {
				return errors.New("恢复挑战记录无效")
			}
			if challenge.Purpose == "password_reset" && (challenge.Owner == target.ID || strings.EqualFold(challenge.Email, target.Email)) {
				DeleteDoc(s, "email_challenges", id)
			}
		}
		target.PasswordHash = string(hash)
		revokeSessions(s, target.ID)
		return identityAudit(s, "local-root", "user.admin_password", target.ID, "通过本机受信安装器修改管理员密码")
	})
	if err != nil {
		// Driver/SQL errors may contain connection details. Never echo them (or
		// the incoming frame) to a terminal, container log, or audit record.
		return errors.New("管理员改密未确认成功：目标须为唯一现有管理员，数据库须已初始化且可写；未创建账号或修改会员。请检查数据库状态后重试")
	}
	return nil
}
