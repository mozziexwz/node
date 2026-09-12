// msboost-restore stages a complete encrypted snapshot in a NEW database. It
// never replaces the running database, changes Compose, or restarts services.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"

	"github.com/mozziexwz/node/internal/control"
)

func main() {
	var options control.OfflineRestoreOptions
	var confirmed bool
	flag.StringVar(&options.BackupPath, "backup", "", "完整加密 .msb 备份文件")
	flag.StringVar(&options.MasterKeyFile, "master-key-file", "", "原始 32 字节 master.key 文件；不用时读取 MASTER_KEY 环境变量")
	flag.StringVar(&options.SQLiteOutputDir, "sqlite-output-dir", "", "全新且不存在的 SQLite 输出目录")
	flag.StringVar(&options.PostgresNewDatabase, "postgres-new-database", "", "全新 PostgreSQL 数据库名，须为 msboost_restore_ 开头")
	flag.BoolVar(&confirmed, "confirm-disaster-restore", false, "确认快照之后的数据不会进入恢复库，生产旧库将保留，恢复后需人工对账")
	flag.Parse()
	if !confirmed || options.BackupPath == "" || flag.NArg() != 0 {
		flag.Usage()
		fmt.Fprintln(os.Stderr, "必须提供 --backup 和 --confirm-disaster-restore；完整灾难恢复不等于恢复营业。")
		os.Exit(2)
	}
	if options.MasterKeyFile == "" {
		options.MasterKey = os.Getenv("MASTER_KEY")
	}
	options.PostgresAdminURL = os.Getenv("MSBOOST_RESTORE_ADMIN_URL")
	if options.PostgresAdminURL == "" && os.Getenv("DATABASE_HOST") != "" {
		port, user, ssl := os.Getenv("DATABASE_PORT"), os.Getenv("DATABASE_USER"), os.Getenv("DATABASE_SSLMODE")
		if port == "" {
			port = "5432"
		}
		if user == "" {
			user = "msboost"
		}
		if ssl == "" {
			ssl = "require"
		}
		u := url.URL{Scheme: "postgres", User: url.UserPassword(user, os.Getenv("POSTGRES_PASSWORD")), Host: net.JoinHostPort(os.Getenv("DATABASE_HOST"), port), Path: "/postgres", RawQuery: "sslmode=" + url.QueryEscape(ssl)}
		options.PostgresAdminURL = u.String()
	}
	result, err := control.OfflineRestore(options)
	if err != nil {
		fmt.Fprintln(os.Stderr, "恢复失败："+err.Error())
		os.Exit(1)
	}
	_ = json.NewEncoder(os.Stdout).Encode(result)
}
