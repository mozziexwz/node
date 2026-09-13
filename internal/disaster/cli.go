package disaster

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
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
	case "config-save":
		file := flags.String("file", "", "")
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
			return SaveConfig(*file, config)
		}
	case "config-get":
		file, field := flags.String("file", "", ""), flags.String("field", "", "")
		run = func() error {
			if *file == "" {
				return errArguments
			}
			// Whitelist before reading the credential-bearing file.
			if *field != "localDir" && *field != "retentionDays" && *field != "time" {
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
			}
			_, err = fmt.Fprintln(output, value)
			return err
		}
	case "prepare-dir":
		dir := flags.String("dir", "", "")
		run = func() error { return privateDir(*dir, true) }
	case "pack":
		dir, destination := flags.String("dir", "", ""), flags.String("output", "", "")
		run = func() error { return Pack(*dir, *destination) }
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
			return Upload(ctx, config, *archive)
		}
	default:
		return errArguments
	}
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
		return errArguments
	}
	if err := run(); err != nil {
		// An archive member, filesystem path or SSH server error may contain
		// attacker-controlled text. Keep command-line diagnostics credential-safe.
		return errors.New("整站备份操作失败：请检查参数、私有权限、文件完整性或远程连接；本工具不会输出凭据")
	}
	return nil
}
