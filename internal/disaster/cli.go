package disaster

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"time"
)

var errArguments = errors.New("整站备份子命令或参数无效；密码仅允许从标准输入传入")

// Run is shared by the standalone restore binary and isolated tests. Flags and
// lower-level errors are never printed: flag parsing can otherwise echo a
// credential accidentally supplied as an unknown argument.
func Run(args []string, input io.Reader, output io.Writer) error {
	if len(args) == 0 {
		return errArguments
	}
	flags := flag.NewFlagSet("disaster "+args[0], flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var run func() error
	switch args[0] {
	case "record":
		file := flags.String("file", "", "")
		id, stage := flags.String("id", "", ""), flags.String("stage", "", "")
		archive := flags.String("archive", "", "")
		remote := flags.Bool("remote", false, "")
		run = func() error { return RecordStage(*file, *id, *stage, *archive, *remote) }
	case "schedule-record":
		file, configFile := flags.String("file", "", ""), flags.String("config", "", "")
		enabled, verified := flags.Bool("enabled", false, ""), flags.Bool("verified", false, "")
		run = func() error {
			c, err := ReadConfig(*configFile)
			if err != nil {
				return err
			}
			return RecordSchedule(*file, c, *enabled, *verified)
		}
	case "status":
		file := flags.String("file", "", "")
		run = func() error {
			h, err := ReadHistory(*file)
			if err != nil {
				return err
			}
			state := "未启用"
			if h.Schedule.Enabled {
				state = "已启用"
			}
			fmt.Fprintf(output, "每日计划：%s；时间 %s（%s）\n", state, h.Schedule.Time, h.Schedule.Zone)
			if h.Schedule.NextRunAt > 0 {
				fmt.Fprintf(output, "下次计划：%s（可能有 60 秒随机延迟）\n", time.UnixMilli(h.Schedule.NextRunAt).Format(time.RFC3339))
			}
			if len(h.Runs) == 0 {
				fmt.Fprintln(output, "尚无本版本备份记录；启用计划不代表已有成功备份")
			}
			for i, r := range h.Runs {
				if i == 10 {
					break
				}
				fmt.Fprintf(output, "%s  %s\n  本地校验成功：%t；异地校验成功：%t\n", time.UnixMilli(r.UpdatedAt).Format(time.RFC3339), r.Message, r.LocalOK, r.RemoteOK)
				if r.Archive != "" {
					fmt.Fprintf(output, "  文件：%s（%d 字节）\n", r.Archive, r.Size)
				}
				if r.Detail != "" {
					fmt.Fprintln(output, "  处理建议："+r.Detail)
				}
			}
			return nil
		}
	case "config-save":
		file := flags.String("file", "", "")
		verify := flags.Bool("verify-remote", false, "")
		config := DefaultConfig()
		flags.StringVar(&config.LocalDir, "local-dir", config.LocalDir, "")
		flags.IntVar(&config.RetentionDays, "retention-days", config.RetentionDays, "")
		flags.StringVar(&config.Time, "time", config.Time, "")
		flags.StringVar(&config.RemoteHost, "remote-host", "", "")
		flags.IntVar(&config.RemotePort, "remote-port", 22, "")
		flags.StringVar(&config.RemoteUser, "remote-user", "root", "")
		flags.StringVar(&config.RemoteDir, "remote-dir", "/root/msboost-backup", "")
		flags.StringVar(&config.Fingerprint, "fingerprint", "", "")
		flags.BoolVar(&config.PruneRemote, "prune-remote", false, "")
		run = func() error {
			if *file == "" || input == nil {
				return errArguments
			}
			password, err := io.ReadAll(io.LimitReader(input, 1025))
			if err != nil || len(password) > 1024 {
				clear(password)
				return errors.New("密码输入无效或超过 1024 字节")
			}
			config.Password = string(password)
			clear(password)
			defer func() { config.Password = "" }()
			if config.RemoteHost == "" {
				config.RemotePort, config.RemoteUser, config.RemoteDir, config.Fingerprint = 0, "", "", ""
			}
			if *verify {
				ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
				defer cancel()
				return saveVerifiedConfig(ctx, *file, config, (&net.Dialer{Timeout: 15 * time.Second}).DialContext)
			}
			return SaveConfig(*file, config)
		}
	case "test-remote":
		file := flags.String("config", "", "")
		run = func() error {
			config, err := ReadConfig(*file)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			if err = CheckRemote(ctx, config); err != nil {
				return err
			}
			if config.RemoteHost == "" {
				_, err = fmt.Fprintln(output, "仅本机备份，未配置异地目标")
			} else {
				_, err = fmt.Fprintln(output, "异地连接、登录、目录写入与回读校验通过")
			}
			return err
		}
	case "config-get":
		file, field := flags.String("file", "", ""), flags.String("field", "", "")
		run = func() error {
			if *file == "" {
				return errArguments
			}
			// Whitelist before reading the credential-bearing file.
			if *field != "localDir" && *field != "retentionDays" && *field != "time" && *field != "hasRemote" {
				return errArguments
			}
			config, err := ReadConfig(*file)
			if err != nil {
				return err
			}
			var value any
			switch *field {
			case "localDir":
				value = config.LocalDir
			case "retentionDays":
				value = config.RetentionDays
			case "time":
				value = config.Time
			case "hasRemote":
				value = config.RemoteHost != ""
			}
			_, err = fmt.Fprintln(output, value)
			return err
		}
	case "prepare-dir":
		dir := flags.String("dir", "", "")
		run = func() error { return privateDir(*dir, true) }
	case "pack", "pack-online":
		dir, destination := flags.String("dir", "", ""), flags.String("output", "", "")
		run = func() error {
			if args[0] == "pack-online" {
				return PackOnline(*dir, *destination)
			}
			return Pack(*dir, *destination)
		}
	case "format":
		archive := flags.String("archive", "", "")
		run = func() error {
			m, err := Verify(*archive)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(output, m.Version)
			return err
		}
	case "verify":
		archive := flags.String("archive", "", "")
		run = func() error {
			manifest, err := Verify(*archive)
			if err != nil {
				return err
			}
			return json.NewEncoder(output).Encode(manifest)
		}
	case "unpack":
		archive, dir := flags.String("archive", "", ""), flags.String("dir", "", "")
		run = func() error { return Unpack(*archive, *dir) }
	case "validate-volume":
		archive := flags.String("archive", "", "")
		run = func() error { return ValidateVolume(*archive) }
	case "retain", "upload":
		file := flags.String("config", "", "")
		history, id := flags.String("history", "", ""), flags.String("id", "", "")
		var archive *string
		if args[0] == "upload" {
			archive = flags.String("archive", "", "")
		}
		run = func() error {
			if *file == "" {
				return errArguments
			}
			config, err := ReadConfig(*file)
			if err != nil {
				return err
			}
			if archive == nil {
				return Retain(config)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()
			err = Upload(ctx, config, *archive)
			if err != nil && *history != "" {
				if e := recordRemoteDiagnostic(*history, *id, err); e != nil {
					return errors.New("异地上传失败且状态记录未保存，请查看本次终端日志")
				}
			}
			return err
		}
	default:
		return errArguments
	}
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
		return errArguments
	}
	if err := run(); err != nil {
		return safeCommandError(args[0], err)
	}
	return nil
}

func safeCommandError(command string, err error) error {
	if stage, ok := err.(remoteError); ok {
		switch stage {
		case remoteConnectError, remoteIdentityError, remoteAuthError, remoteSFTPError, remoteDirectoryError, remoteWriteError, remoteVerifyError, remoteCleanupError, remoteProbeCleanupError:
			return stage
		}
	}
	// Only exact, locally generated config-save stages may cross the CLI
	// boundary. Do not unwrap errors or trust text resembling a diagnostic code.
	if command == "config-save" {
		if stage, ok := err.(configSaveError); ok {
			switch stage {
			case errConfigValidate, errConfigParent, errConfigExisting, errConfigLocalDir, errConfigWrite:
				return errors.New(stage.Error())
			}
		}
	}
	// Select complete literal codes, never interpolate a caller-supplied command.
	// Archive members, paths, arguments and SSH errors remain discarded.
	var code string
	switch command {
	case "config-save":
		code = " [disaster/config-save]"
	case "config-get":
		code = " [disaster/config-get]"
	case "prepare-dir":
		code = " [disaster/prepare-dir]"
	case "pack":
		code = " [disaster/pack]"
	case "verify":
		code = " [disaster/verify]"
	case "unpack":
		code = " [disaster/unpack]"
	case "validate-volume":
		code = " [disaster/validate-volume]"
	case "retain":
		code = " [disaster/retain]"
	case "upload":
		code = " [disaster/upload]"
	}
	return errors.New("整站备份操作失败" + code + "：请检查参数、私有权限、文件完整性或远程连接；本工具不会输出凭据")
}
