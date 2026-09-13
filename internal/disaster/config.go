package disaster

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/mozziexwz/node/internal/executor"
)

// This root-only host configuration deliberately has no API response type.
// Like .env it contains a credential, never emitted by config-get or logs.
type Config struct {
	Version       int    `json:"version"`
	LocalDir      string `json:"localDir"`
	RetentionDays int    `json:"retentionDays"`
	Time          string `json:"time"`
	RemoteHost    string `json:"remoteHost,omitempty"`
	RemotePort    int    `json:"remotePort,omitempty"`
	RemoteUser    string `json:"remoteUser,omitempty"`
	RemoteDir     string `json:"remoteDir,omitempty"`
	Fingerprint   string `json:"fingerprint,omitempty"`
	Password      string `json:"password,omitempty"`
	PruneRemote   bool   `json:"pruneRemote,omitempty"`
}

func DefaultConfig() Config {
	return Config{Version: 1, LocalDir: "/root/msboost-backup", RetentionDays: 30, Time: "02:30"}
}
func (c Config) Validate() error {
	if c.Version != 1 || !filepath.IsAbs(c.LocalDir) || filepath.Clean(c.LocalDir) != c.LocalDir || c.LocalDir == string(filepath.Separator) || strings.ContainsAny(c.LocalDir, "\r\n\x00") || c.LocalDir == "/root" || c.LocalDir == "/opt" || c.LocalDir == "/tmp" || c.LocalDir == "/opt/msboost" || strings.HasPrefix(c.LocalDir, "/opt/msboost/") || c.RetentionDays < 1 || c.RetentionDays > 3650 || !regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`).MatchString(c.Time) {
		return errors.New("备份目录、保留天数（1–3650）或时间（HH:MM）无效")
	}
	if c.RemoteHost != "" {
		fingerprint, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(c.Fingerprint, "SHA256:"))
		if executor.PublicIP(c.RemoteHost) != nil || c.RemotePort < 1 || c.RemotePort > 65535 || !regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_.-]{0,63}$`).MatchString(c.RemoteUser) || !path.IsAbs(c.RemoteDir) || path.Clean(c.RemoteDir) != c.RemoteDir || strings.Count(c.RemoteDir, "/") < 2 || strings.ContainsAny(c.RemoteDir, "\\\r\n\x00") || !strings.HasPrefix(c.Fingerprint, "SHA256:") || err != nil || len(fingerprint) != 32 || len(c.Password) < 1 || len(c.Password) > 1024 {
			return errors.New("远程公网 IP、端口、用户、专用目录、密码或 SHA256 指纹无效")
		}
	} else if c.Password != "" || c.PruneRemote {
		return errors.New("未选择远程目标，拒绝保存多余密码或远端清理配置")
	}
	return nil
}

func privateDir(directory string, create bool) error {
	absolute, err := filepath.Abs(directory)
	if err != nil || absolute != filepath.Clean(directory) || filepath.Dir(absolute) == absolute {
		return errors.New("需要明确的绝对专用目录，不能使用根目录")
	}
	// Check every ancestor. Mkdir (not MkdirAll) lets us reject symlinks before
	// descending, and never chmods unrelated existing ancestors.
	var chain []string
	for current := absolute; filepath.Dir(current) != current; current = filepath.Dir(current) {
		chain = append(chain, current)
	}
	for i := len(chain) - 1; i >= 0; i-- {
		name := chain[i]
		info, e := os.Lstat(name)
		if os.IsNotExist(e) && create {
			if e = os.Mkdir(name, 0700); e != nil {
				return e
			}
			info, e = os.Lstat(name)
		}
		if e != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("备份目录不存在、不是目录或包含符号链接")
		}
		if !safeAncestor(info) {
			return errors.New("备份目录的上级目录可被其他用户替换，拒绝使用")
		}
		if name == absolute && runtime.GOOS != "windows" && (info.Mode().Perm()&0077 != 0 || !ownedByCurrentUser(info)) {
			return errors.New("备份专用目录须由当前用户所有且权限 0700；不会更改已有目录权限")
		}
	}
	return nil
}

func ReadConfig(filename string) (Config, error) {
	c := DefaultConfig()
	info, err := os.Lstat(filename)
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > 16384 || runtime.GOOS != "windows" && (info.Mode().Perm()&0077 != 0 || !ownedByCurrentUser(info)) {
		return c, errors.New("配置须是当前用户所有的 0600 普通文件")
	}
	if err = privateDir(filepath.Dir(filename), false); err != nil {
		return c, err
	}
	raw, err := os.ReadFile(filename)
	if err != nil {
		return c, err
	}
	if json.Unmarshal(raw, &c) != nil {
		return c, errors.New("备份配置 JSON 无效")
	}
	return c, c.Validate()
}

// Config-save diagnostics deliberately contain only a fixed stage. Never retain
// a filesystem error here: it may include credentials or a caller-supplied path.
type configSaveError uint8

const (
	errConfigValidate configSaveError = iota + 1
	errConfigParent
	errConfigExisting
	errConfigLocalDir
	errConfigWrite
)

func (stage configSaveError) Error() string {
	switch stage {
	case errConfigValidate:
		return "整站备份配置保存失败 [config-save/validate]：配置校验未通过"
	case errConfigParent:
		return "整站备份配置保存失败 [config-save/config-parent]：配置上级目录私有权限校验未通过"
	case errConfigExisting:
		return "整站备份配置保存失败 [config-save/existing-config]：已有配置检查未通过"
	case errConfigLocalDir:
		return "整站备份配置保存失败 [config-save/local-dir]：本地备份目录创建或私有权限校验未通过"
	case errConfigWrite:
		return "整站备份配置保存失败 [config-save/write-config]：私有配置原子写入未完成"
	default:
		return "整站备份配置保存失败"
	}
}

func SaveConfig(filename string, c Config) error {
	return saveConfig(filename, c, writeConfig)
}

func saveConfig(filename string, c Config, write func(string, []byte) error) error {
	if err := c.Validate(); err != nil {
		return errConfigValidate
	}
	if err := privateDir(filepath.Dir(filename), false); err != nil {
		return errConfigParent
	}
	if _, err := os.Lstat(filename); err == nil {
		if _, err = ReadConfig(filename); err != nil {
			return errConfigExisting
		}
	} else if !os.IsNotExist(err) {
		return errConfigExisting
	}
	if err := privateDir(c.LocalDir, true); err != nil {
		return errConfigLocalDir
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return errConfigWrite
	}
	if err := write(filename, raw); err != nil {
		return errConfigWrite
	}
	return nil
}

func writeConfig(filename string, raw []byte) error {
	f, err := os.CreateTemp(filepath.Dir(filename), ".disaster-config-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err = f.Chmod(0600); err != nil {
		return err
	}
	if _, err = f.Write(raw); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filename)
}
