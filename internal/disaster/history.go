package disaster

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// This file contains public operational metadata only and can be mounted
// read-only in the web container. Passwords and configuration never enter it.
type BackupRun struct {
	ID               string `json:"id"`
	StartedAt        int64  `json:"startedAt"`
	UpdatedAt        int64  `json:"updatedAt"`
	Archive          string `json:"archive,omitempty"`
	Size             int64  `json:"size,omitempty"`
	Stage            string `json:"stage"`
	LocalOK          bool   `json:"localOK"`
	RemoteOK         bool   `json:"remoteOK"`
	LocalVerifiedAt  int64  `json:"localVerifiedAt"`
	RemoteVerifiedAt int64  `json:"remoteVerifiedAt"`
	RemoteConfigured bool   `json:"remoteConfigured"`
	Message          string `json:"message"`
	Detail           string `json:"detail,omitempty"`
}
type BackupSchedule struct {
	Enabled          bool   `json:"enabled"`
	Time             string `json:"time"`
	Zone             string `json:"zone"`
	Offset           int    `json:"offset"`
	NextRunAt        int64  `json:"nextRunAt"`
	RemoteConfigured bool   `json:"remoteConfigured"`
	VerifiedAt       int64  `json:"verifiedAt"`
}
type BackupHistory struct {
	Version  int            `json:"version"`
	Schedule BackupSchedule `json:"schedule"`
	Runs     []BackupRun    `json:"runs"`
}

func ReadHistory(file string) (BackupHistory, error) {
	h := BackupHistory{Version: 1, Runs: []BackupRun{}}
	info, err := os.Lstat(file)
	if os.IsNotExist(err) {
		return h, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return h, errors.New("备份记录不可读取")
	}
	raw, err := os.ReadFile(file)
	if err != nil || json.Unmarshal(raw, &h) != nil || h.Version != 1 || len(h.Runs) > 50 {
		return h, errors.New("备份记录格式无效")
	}
	if h.Schedule.Enabled {
		zone := time.FixedZone(h.Schedule.Zone, h.Schedule.Offset)
		if loaded, e := time.LoadLocation(h.Schedule.Zone); e == nil && h.Schedule.Zone != "Local" {
			zone = loaded
		}
		now := time.Now().In(zone)
		var hour, minute int
		if _, e := fmt.Sscanf(h.Schedule.Time, "%d:%d", &hour, &minute); e == nil {
			next := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, zone)
			if !next.After(now) {
				next = next.AddDate(0, 0, 1)
			}
			h.Schedule.NextRunAt = next.UnixMilli()
		}
	}
	return h, nil
}

func saveHistory(file string, h BackupHistory) error {
	// Caller is the root-owned deployment manager, serialized by its flock.
	info, err := os.Lstat(filepath.Dir(file))
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || runtime.GOOS != "windows" && info.Mode().Perm()&0022 != 0 {
		return errors.New("备份状态目录不可用")
	}
	if info, err := os.Lstat(file); err == nil && !info.Mode().IsRegular() || err != nil && !os.IsNotExist(err) {
		return errors.New("备份状态文件不可替换")
	}
	raw, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(file), ".history-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err = f.Chmod(0644); err != nil {
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
	return os.Rename(f.Name(), file)
}

var stageMessages = map[string]string{
	"started":         "正在准备在线备份",
	"export":          "正在导出一致的业务快照",
	"pack":            "正在打包并校验",
	"local":           "本地备份已完成并通过校验",
	"upload":          "本地备份成功，正在上传异地并回读校验",
	"remote":          "本地及异地备份均已通过校验",
	"complete":        "备份完成",
	"failed":          "本次备份未完成，请查看本次日志和处理建议",
	"remote_failed":   "本地备份成功，异地上传失败；可重新测试连接并重传",
	"cleanup_warning": "备份已保存，旧备份清理未完成",
}

func RecordStage(file, id, stage, archive string, remote bool) error {
	message, ok := stageMessages[stage]
	if !ok || !bundleName.MatchString(id) {
		return errors.New("备份阶段或编号无效")
	}
	h, err := ReadHistory(file)
	if err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	index := -1
	for i := range h.Runs {
		if h.Runs[i].ID == id {
			index = i
			break
		}
	}
	if index < 0 {
		if stage != "started" {
			return errors.New("备份运行记录不存在")
		}
		h.Runs = append([]BackupRun{{ID: id, StartedAt: now, RemoteConfigured: remote}}, h.Runs...)
		if len(h.Runs) > 50 {
			h.Runs = h.Runs[:50]
		}
		index = 0
	}
	r := &h.Runs[index]
	r.Stage, r.UpdatedAt, r.Message = stage, now, message
	if stage == "started" {
		r.Detail = ""
		r.RemoteConfigured = remote
		// A retry is a new attempt. A past remote result does not prove the
		// current destination still holds a valid copy.
		r.RemoteOK = false
	}
	if stage == "local" {
		if filepath.Base(archive) != id {
			return errors.New("备份文件与运行记录不匹配")
		}
		if _, err := Verify(archive); err != nil {
			return err
		}
		info, err := regular(archive)
		if err != nil {
			return err
		}
		r.LocalOK, r.Archive, r.Size = true, archive, info.Size()
		r.LocalVerifiedAt = now
	}
	if stage == "remote" {
		if !r.LocalOK {
			return errors.New("尚未校验本地备份")
		}
		r.RemoteOK = true
		r.RemoteVerifiedAt = now
	}
	return saveHistory(file, h)
}

func recordRemoteDiagnostic(file, id string, err error) error {
	if file == "" {
		return nil
	}
	message := "异地传输未完成，请测试连接并检查远程磁盘空间"
	if known, ok := err.(remoteError); ok {
		message = known.Error()
	}
	h, e := ReadHistory(file)
	if e != nil {
		return e
	}
	for i := range h.Runs {
		if h.Runs[i].ID == id {
			h.Runs[i].Detail = message
			return saveHistory(file, h)
		}
	}
	return errors.New("备份记录不存在")
}

func RecordSchedule(file string, c Config, enabled, verified bool) error {
	h, err := ReadHistory(file)
	if err != nil {
		return err
	}
	now := time.Now()
	candidates := []string{now.Location().String()}
	if target, e := os.Readlink("/etc/localtime"); e == nil {
		if _, name, found := strings.Cut(filepath.ToSlash(target), "zoneinfo/"); found {
			candidates = append(candidates, name)
		}
	}
	if raw, e := os.ReadFile("/etc/timezone"); e == nil {
		candidates = append(candidates, strings.TrimSpace(string(raw)))
	}
	zone, offset := scheduleTimeZone(now, candidates)
	h.Schedule = BackupSchedule{Enabled: enabled, Time: c.Time, Zone: zone, Offset: offset, RemoteConfigured: c.RemoteHost != ""}
	if verified {
		h.Schedule.VerifiedAt = time.Now().UnixMilli()
	}
	return saveHistory(file, h)
}

// /etc/timezone can be stale after timedatectl or /etc/localtime changes.
// Use an IANA name only when it agrees with the process's effective zone;
// otherwise use an explicit offset, never reinterpret a CST/UTC abbreviation.
func scheduleTimeZone(now time.Time, candidates []string) (string, int) {
	abbreviation, offset := now.Zone()
	for _, name := range candidates {
		if name == "" || name == "Local" {
			continue
		}
		if location, err := time.LoadLocation(name); err == nil {
			candidateAbbreviation, candidateOffset := now.In(location).Zone()
			if candidateOffset == offset && candidateAbbreviation == abbreviation {
				return name, offset
			}
		}
	}
	sign, seconds := "+", offset
	if seconds < 0 {
		sign, seconds = "-", -seconds
	}
	return fmt.Sprintf("UTC%s%02d:%02d", sign, seconds/3600, seconds%3600/60), offset
}
