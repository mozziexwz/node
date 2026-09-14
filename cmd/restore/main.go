// msboost-restore provides isolated recovery/export and a narrowly scoped local
// administrator password command. No command changes Compose or restarts it.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"

	"github.com/mozziexwz/node/internal/control"
	"github.com/mozziexwz/node/internal/disaster"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "relay-recovery" {
		if err := control.RunLocalRelayRecovery(connectionURL(false), os.Getenv("MASTER_KEY"), os.Getenv("PUBLIC_URL"), os.Args[2:], os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "受信中转恢复："+err.Error())
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "backup-activity" {
		if err := control.RunLocalBackupActivity(connectionURL(false), os.Getenv("DATA_DIR"), os.Args[2:], os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "异常网页备份："+err.Error())
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "backup-pause" {
		if err := control.RunLocalBackupPause(connectionURL(false), os.Args[2:], os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "停站备份门禁："+err.Error())
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "admin-password" {
		if len(os.Args) != 2 {
			fmt.Fprintln(os.Stderr, "admin-password 不接受参数；请从本机安装器的无回显终端交互执行。")
			os.Exit(2)
		}
		if err := control.ChangeLocalAdminPassword(connectionURL(false), os.Stdin); err != nil {
			fmt.Fprintln(os.Stderr, err.Error())
			os.Exit(1)
		}
		fmt.Fprintln(os.Stdout, "管理员密码已更新，原登录会话与未完成的密码恢复请求已失效。")
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "disaster" {
		if err := disaster.Run(os.Args[2:], os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "整站备份工具："+err.Error())
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "export" {
		exportFlags := flag.NewFlagSet("export", flag.ExitOnError)
		keyFile := exportFlags.String("master-key-file", "", "原始 32 字节 master.key（未设置 MASTER_KEY 时使用）")
		_ = exportFlags.Parse(os.Args[2:])
		if exportFlags.NArg() != 0 {
			os.Exit(2)
		}
		if err := control.ExportPostgresBackup(connectionURL(false), os.Getenv("MASTER_KEY"), *keyFile, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "导出失败："+err.Error())
			os.Exit(1)
		}
		return
	}
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
	options.PostgresAdminURL = connectionURL(true)
	result, err := control.OfflineRestore(options)
	if err != nil {
		fmt.Fprintln(os.Stderr, "恢复失败："+err.Error())
		os.Exit(1)
	}
	_ = json.NewEncoder(os.Stdout).Encode(result)
}

func connectionURL(admin bool) string {
	key := "DATABASE_URL"
	if admin {
		key = "MSBOOST_RESTORE_ADMIN_URL"
	}
	if existing := os.Getenv(key); existing != "" {
		return existing
	}
	if os.Getenv("DATABASE_HOST") != "" {
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
		database := os.Getenv("DATABASE_NAME")
		if database == "" {
			database = "msboost"
		}
		if admin {
			database = "postgres"
		}
		u := url.URL{Scheme: "postgres", User: url.UserPassword(user, os.Getenv("POSTGRES_PASSWORD")), Host: net.JoinHostPort(os.Getenv("DATABASE_HOST"), port), Path: "/" + database, RawQuery: "sslmode=" + url.QueryEscape(ssl)}
		return u.String()
	}
	return ""
}
