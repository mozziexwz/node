package disaster

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/mozziexwz/node/internal/executor"
	"github.com/pkg/sftp"
)

type remoteDial func(context.Context, string, string) (net.Conn, error)

// Upload sends only the explicitly selected, verified bundle. Remote retention
// defaults off. With explicit PruneRemote consent, only verified, expired
// managed bundles may be removed. Failed private .partial files remain for
// inspection and are never counted or pruned as valid backups.
func Upload(ctx context.Context, config Config, archive string) error {
	return uploadWithDial(ctx, config, archive, (&net.Dialer{Timeout: 15 * time.Second}).DialContext)
}

func uploadWithDial(ctx context.Context, config Config, archive string, dial remoteDial) error {
	if err := config.Validate(); err != nil {
		return err
	}
	if !bundleName.MatchString(filepath.Base(archive)) {
		return errors.New("需要受管命名的整站备份文件")
	}
	if err := privateDir(filepath.Dir(archive), false); err != nil {
		return err
	}
	info, err := regular(archive)
	if err != nil || runtime.GOOS != "windows" && (info.Mode().Perm()&0077 != 0 || !ownedByCurrentUser(info)) {
		return errors.New("整站备份文件必须是当前用户所有的私有普通文件")
	}
	if _, err = Verify(archive); err != nil {
		return errors.New("本机整站备份校验失败")
	}
	if config.RemoteHost == "" {
		return nil
	}
	file, err := os.Open(archive)
	if err != nil {
		return errors.New("无法读取本机备份")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || !sameRemoteInfo(info, opened) {
		return errors.New("本机备份在校验后发生变化")
	}
	// Use a hash of this open descriptor, not a remotely reported checksum.
	hash := sha256.New()
	if _, err = io.Copy(hash, file); err != nil {
		return errors.New("本机备份摘要计算失败")
	}
	want := hex.EncodeToString(hash.Sum(nil))
	if current, err := file.Stat(); err != nil || !sameRemoteInfo(info, current) {
		return errors.New("本机备份在摘要计算时发生变化")
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return errors.New("本机备份无法重新读取")
	}
	sf, closeRemote, err := connectRemote(ctx, config, dial)
	if err != nil {
		return err
	}
	defer closeRemote()
	var selfUID *uint32
	if config.RemoteUser == "root" {
		uid := uint32(0)
		selfUID = &uid
	}
	if err = privateRemoteDirectory(sf, config.RemoteDir, selfUID); err != nil {
		return remoteDirectoryError
	}
	final := path.Join(config.RemoteDir, filepath.Base(archive))
	if existing, statErr := sf.Lstat(final); statErr == nil {
		uid, ownerErr := executor.SFTPFileUID(existing)
		if ownerErr != nil || selfUID != nil && *selfUID != uid || privateRemoteDirectory(sf, config.RemoteDir, &uid) != nil {
			return remoteDirectoryError
		}
		if verifyRemoteFile(sf, final, info.Size(), want, uid) != nil {
			return remoteVerifyError
		}
		if config.PruneRemote {
			if err := pruneRemote(sf, config, uid); err != nil {
				return remoteCleanupError
			}
		}
		return nil // Lost responses can be retried without overwriting any file.
	} else if !os.IsNotExist(statErr) {
		return remoteDirectoryError
	}
	var nonce [8]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return errors.New("无法生成上传标识")
	}
	temporary := path.Join(config.RemoteDir, "."+filepath.Base(archive)+"."+hex.EncodeToString(nonce[:])+".partial")
	remote, err := sf.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY)
	if err != nil {
		return remoteDirectoryError
	}
	defer remote.Close()
	if err = remote.Chmod(0600); err != nil {
		return errors.New("远程文件不能设置私有权限，未上传内容")
	}
	state, err := remote.Stat()
	if err != nil || !state.Mode().IsRegular() || state.Mode().Perm() != 0600 || state.Size() != 0 {
		return errors.New("远程文件权限不安全，未上传内容")
	}
	uid, err := executor.SFTPFileUID(state)
	if err != nil || selfUID != nil && *selfUID != uid {
		return errors.New("远程上传账户 UID 无法安全确定，未上传内容")
	}
	selfUID = &uid
	// The empty private upload itself is the ownership probe for non-root
	// users. Only after checking the entire tree may the first byte be sent.
	if err = privateRemoteDirectory(sf, config.RemoteDir, selfUID); err != nil {
		return err
	}
	sentHash := sha256.New()
	n, err := io.Copy(remote, io.TeeReader(io.LimitReader(file, info.Size()+1), sentHash))
	if err != nil || n != info.Size() || hex.EncodeToString(sentHash.Sum(nil)) != want {
		return remoteWriteError
	}
	if err = remote.Close(); err != nil {
		return remoteWriteError
	}
	if err = verifyRemoteFile(sf, temporary, info.Size(), want, uid); err != nil {
		return remoteVerifyError
	}
	if err = privateRemoteDirectory(sf, config.RemoteDir, selfUID); err != nil {
		return err
	}
	if _, err = sf.Lstat(final); !os.IsNotExist(err) {
		return errors.New("远程目标已存在或不可检查，拒绝覆盖")
	}
	// SFTP v3 SSH_FXP_RENAME must fail if newpath exists (filexfer-02 §6.5).
	// Do not use PosixRename: that extension explicitly permits overwriting.
	if err = sf.Rename(temporary, final); err != nil {
		return errors.New("远程原子发布失败；未覆盖已有备份")
	}
	if err = verifyRemoteFile(sf, final, info.Size(), want, uid); err != nil {
		return remoteVerifyError
	}
	if config.PruneRemote {
		if err := pruneRemote(sf, config, uid); err != nil {
			return remoteCleanupError
		}
	}
	return nil
}

func sameRemoteInfo(before, after os.FileInfo) bool {
	beforeStat, beforeSFTP := before.Sys().(*sftp.FileStat)
	afterStat, afterSFTP := after.Sys().(*sftp.FileStat)
	if beforeSFTP || afterSFTP {
		if !beforeSFTP || !afterSFTP || beforeStat.UID != afterStat.UID || beforeStat.GID != afterStat.GID {
			return false
		}
	}
	return before.Mode() == after.Mode() && before.Size() == after.Size() && before.ModTime().Equal(after.ModTime())
}

// Pruning is deliberately two-phase: download and validate ALL managed normal
// files before considering any removal. A bad bundle or interrupted read aborts
// the entire scan, not merely the affected candidate. No symlink/partial file
// participates. The caller has already explicitly enabled remote retention.
func pruneRemote(sf *sftp.Client, config Config, selfUID uint32) error {
	if !config.PruneRemote {
		return nil
	}
	if err := privateRemoteDirectory(sf, config.RemoteDir, &selfUID); err != nil {
		return err
	}
	if err := privateDir(config.LocalDir, false); err != nil {
		return err
	}
	entries, err := sf.ReadDir(config.RemoteDir)
	if err != nil {
		return errors.New("无法读取远程备份清单；未开始清理")
	}
	type verifiedCopy struct {
		name     string
		created  int64
		info     os.FileInfo
		manifest Manifest
	}
	var copies []verifiedCopy
	staging, err := os.MkdirTemp(config.LocalDir, ".remote-retention-")
	if err != nil {
		return errors.New("无法创建私有远端校验暂存目录；未开始清理")
	}
	defer os.Remove(staging) // Empty task-created directory only; never recursive.
	for _, entry := range entries {
		if !bundleName.MatchString(entry.Name()) || !entry.Mode().IsRegular() {
			continue
		}
		if len(copies) >= 500 {
			return errors.New("远端备份过多，需人工分批核对；未开始清理")
		}
		name := path.Join(config.RemoteDir, entry.Name())
		info, err := sf.Lstat(name)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() <= 0 || info.Size() > maxBundleSize {
			return errors.New("远端备份类型、权限或大小异常；未开始清理")
		}
		if err := executor.CheckSFTPOwner(info, selfUID, true); err != nil {
			return err
		}
		manifest, err := downloadForRetention(sf, name, info, staging)
		if err != nil {
			return err
		}
		copies = append(copies, verifiedCopy{name, manifest.CreatedAt, info, manifest})
	}
	sort.Slice(copies, func(i, j int) bool {
		if copies[i].created != copies[j].created {
			return copies[i].created > copies[j].created
		}
		return copies[i].name > copies[j].name
	})
	// Confirm that the validated survivor set still exists before any deletion.
	for _, copy := range copies {
		current, err := sf.Lstat(copy.name)
		if err != nil || !sameRemoteInfo(copy.info, current) {
			return errors.New("远端备份在验证后发生变化；未开始清理")
		}
	}
	cutoff := time.Now().AddDate(0, 0, -config.RetentionDays).Unix()
	for i, copy := range copies {
		if i < 2 || copy.created >= cutoff {
			continue
		}
		if err := privateRemoteDirectory(sf, config.RemoteDir, &selfUID); err != nil {
			return err
		}
		current, err := sf.Lstat(copy.name)
		if err != nil || !sameRemoteInfo(copy.info, current) {
			return errors.New("远端候选备份已变化，立即停止清理")
		}
		// SFTP timestamps may have only second precision: metadata alone cannot
		// detect a same-size rewrite. Revalidate this exact candidate's full
		// contents before deletion and compare the original integrity manifest.
		manifest, err := downloadForRetention(sf, copy.name, current, staging)
		if err != nil || !reflect.DeepEqual(copy.manifest, manifest) {
			return errors.New("远端候选备份内容已变化或无法再次校验，立即停止清理")
		}
		// Exactly this verified managed regular file. Never RemoveAll, globs,
		// unknown files, temporary uploads or remote directory removal.
		if err := sf.Remove(copy.name); err != nil {
			return errors.New("远端备份删除失败，立即停止后续清理")
		}
	}
	return nil
}

func downloadForRetention(sf *sftp.Client, name string, info os.FileInfo, staging string) (Manifest, error) {
	var manifest Manifest
	local, err := os.CreateTemp(staging, "bundle-*.tar.gz")
	if err != nil {
		return manifest, errors.New("私有校验暂存文件创建失败；未开始清理")
	}
	defer os.Remove(local.Name())
	defer local.Close()
	if local.Chmod(0600) != nil {
		return manifest, errors.New("暂存文件权限设置失败；未开始清理")
	}
	remote, err := sf.Open(name)
	if err != nil {
		return manifest, errors.New("远端备份下载失败；未开始清理")
	}
	n, err := io.Copy(local, io.LimitReader(remote, info.Size()+1))
	closeErr := remote.Close()
	if err != nil || closeErr != nil || n != info.Size() {
		return manifest, errors.New("远端备份未完整下载；未开始清理")
	}
	if local.Close() != nil {
		return manifest, errors.New("远端校验暂存文件关闭失败；未开始清理")
	}
	current, err := sf.Lstat(name)
	if err != nil || !sameRemoteInfo(info, current) {
		return manifest, errors.New("远端备份下载时已变化；未开始清理")
	}
	manifest, err = Verify(local.Name())
	if err != nil {
		return manifest, errors.New("至少一份远端备份校验失败；未开始清理")
	}
	return manifest, nil
}

func verifyRemoteFile(sf *sftp.Client, name string, size int64, checksum string, selfUID uint32) error {
	info, err := sf.Lstat(name)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() != size {
		return errors.New("远程备份类型、大小或私有权限不符")
	}
	if err := executor.CheckSFTPOwner(info, selfUID, true); err != nil {
		return err
	}
	file, err := sf.Open(name)
	if err != nil {
		return errors.New("无法回读远程备份")
	}
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(file, size+1))
	closeErr := file.Close()
	if err != nil || closeErr != nil || n != size || hex.EncodeToString(hash.Sum(nil)) != checksum {
		return errors.New("远程备份回读 SHA256 校验失败")
	}
	return nil
}

func privateRemoteDirectory(sf *sftp.Client, directory string, selfUID *uint32) error {
	current := "/"
	if selfUID != nil {
		root, err := sf.Lstat(current)
		if err != nil {
			return errors.New("远程根目录不可检查")
		}
		if err := executor.CheckSFTPOwner(root, *selfUID, false); err != nil {
			return err
		}
	}
	for _, component := range strings.Split(directory, "/") {
		if component == "" {
			continue
		}
		current = path.Join(current, component)
		info, err := sf.Lstat(current)
		if os.IsNotExist(err) {
			if sf.Mkdir(current) != nil || sf.Chmod(current, 0700) != nil {
				return errors.New("无法创建远程私有备份目录")
			}
			info, err = sf.Lstat(current)
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 {
			return errors.New("远程路径含链接、非目录或可被其他用户改写的父目录")
		}
		if current == directory && info.Mode().Perm() != 0700 {
			return errors.New("远程备份专用目录必须为 0700；不会修改已有目录权限")
		}
		if selfUID != nil {
			if err := executor.CheckSFTPOwner(info, *selfUID, current == directory); err != nil {
				return err
			}
		}
	}
	return nil
}
