package disaster

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path"
	"time"

	"github.com/mozziexwz/node/internal/executor"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// Only these fixed, credential-free diagnostics cross the CLI boundary.
type remoteError string

func (e remoteError) Error() string { return string(e) }

const (
	remoteConnectError      remoteError = "连接远程服务器失败，请检查 IP、端口和网络"
	remoteIdentityError     remoteError = "远程服务器指纹不匹配，请核对服务器身份后更新指纹"
	remoteAuthError         remoteError = "SSH 登录失败，请检查账号、密码及服务器是否允许密码登录"
	remoteSFTPError         remoteError = "远程服务器未提供可用的 SFTP 文件传输服务"
	remoteDirectoryError    remoteError = "远程备份目录不可写或权限不符合要求，请使用该账号所有的 0700 专用目录"
	remoteWriteError        remoteError = "远程文件写入失败，请检查磁盘空间和连接"
	remoteVerifyError       remoteError = "远程文件回读校验失败，尚未确认异地备份成功"
	remoteCleanupError      remoteError = "异地备份已校验成功，但旧备份清理失败；新备份已保留"
	remoteProbeCleanupError remoteError = "连接与读写测试通过，但测试文件无法清理；请检查远程目录删除权限"
)

func connectRemote(ctx context.Context, config Config, dial remoteDial) (*sftp.Client, func(), error) {
	address := net.JoinHostPort(config.RemoteHost, fmt.Sprint(config.RemotePort))
	conn, err := dial(ctx, "tcp", address)
	if err != nil {
		return nil, nil, remoteConnectError
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	cleanup := func() { stop(); _ = conn.Close() }
	deadline := time.Now().Add(30 * time.Minute)
	if limit, ok := ctx.Deadline(); ok && limit.Before(deadline) {
		deadline = limit
	}
	handshakeDeadline := time.Now().Add(15 * time.Second)
	if deadline.Before(handshakeDeadline) {
		handshakeDeadline = deadline
	}
	if conn.SetDeadline(handshakeDeadline) != nil {
		cleanup()
		return nil, nil, remoteConnectError
	}
	mismatch := false
	cc, channels, requests, err := ssh.NewClientConn(conn, address, &ssh.ClientConfig{
		User: config.RemoteUser, Auth: []ssh.AuthMethod{ssh.Password(config.Password)},
		HostKeyAlgorithms: []string{ssh.KeyAlgoED25519}, Timeout: 15 * time.Second,
		HostKeyCallback: func(host string, address net.Addr, key ssh.PublicKey) error {
			err := executor.PinnedHostKey(config.Fingerprint)(host, address, key)
			mismatch = err != nil
			return err
		},
	})
	if err != nil {
		cleanup()
		if mismatch {
			return nil, nil, remoteIdentityError
		}
		return nil, nil, remoteAuthError
	}
	client := ssh.NewClient(cc, channels, requests)
	if conn.SetDeadline(deadline) != nil {
		_ = client.Close()
		cleanup()
		return nil, nil, remoteConnectError
	}
	sf, err := sftp.NewClient(client)
	if err != nil {
		_ = client.Close()
		cleanup()
		return nil, nil, remoteSFTPError
	}
	return sf, func() { _ = sf.Close(); _ = client.Close(); cleanup() }, nil
}

func CheckRemote(ctx context.Context, config Config) error {
	return checkRemoteWithDial(ctx, config, (&net.Dialer{Timeout: 15 * time.Second}).DialContext)
}

func saveVerifiedConfig(ctx context.Context, file string, config Config, dial remoteDial) error {
	if err := checkRemoteWithDial(ctx, config, dial); err != nil {
		return err
	}
	return SaveConfig(file, config)
}

func checkRemoteWithDial(ctx context.Context, config Config, dial remoteDial) error {
	if err := config.Validate(); err != nil {
		return errConfigValidate
	}
	if config.RemoteHost == "" {
		return nil
	}
	sf, closeRemote, err := connectRemote(ctx, config, dial)
	if err != nil {
		return err
	}
	defer closeRemote()
	var owner *uint32
	if config.RemoteUser == "root" {
		uid := uint32(0)
		owner = &uid
	}
	if privateRemoteDirectory(sf, config.RemoteDir, owner) != nil {
		return remoteDirectoryError
	}
	var data [128]byte
	if _, err = rand.Read(data[:]); err != nil {
		return remoteWriteError
	}
	name := path.Join(config.RemoteDir, ".msboost-connection-test-"+hex.EncodeToString(data[:16]))
	f, err := sf.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY)
	if err != nil {
		return remoteDirectoryError
	}
	defer f.Close()
	// Remove only the exact random test file created by this invocation.
	removed := false
	defer func() {
		if !removed {
			_ = sf.Remove(name)
		}
	}()
	if f.Chmod(0600) != nil {
		return remoteDirectoryError
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return remoteDirectoryError
	}
	uid, err := executor.SFTPFileUID(info)
	if err != nil || owner != nil && *owner != uid || privateRemoteDirectory(sf, config.RemoteDir, &uid) != nil {
		return remoteDirectoryError
	}
	if n, err := f.Write(data[:]); err != nil || n != len(data) {
		return remoteWriteError
	}
	if f.Close() != nil {
		return remoteWriteError
	}
	sum := sha256.Sum256(data[:])
	if verifyRemoteFile(sf, name, int64(len(data)), hex.EncodeToString(sum[:]), uid) != nil {
		return remoteVerifyError
	}
	if sf.Remove(name) != nil {
		return remoteProbeCleanupError
	}
	removed = true
	return nil
}

func ExitCode(err error) int {
	if err == remoteCleanupError {
		return 3
	}
	return 1
}
