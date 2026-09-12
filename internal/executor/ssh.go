package executor

import (
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

type SSHRemote struct{}

var errFingerprintCaptured = errors.New("host key captured without authentication")

func (SSHRemote) Probe(ctx context.Context, host string, port int) (string, string, error) {
	if err := PublicIP(host); err != nil {
		return "", "", err
	}
	if port < 1 || port > 65535 {
		return "", "", errors.New("SSH 端口无效")
	}
	address := net.JoinHostPort(host, strconv.Itoa(port))
	conn, err := (&net.Dialer{Timeout: 12 * time.Second}).DialContext(ctx, "tcp", address)
	if err != nil {
		return "", "", errors.New("无法连接 SSH 端口")
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	var fp, algorithm string
	_, _, _, _ = ssh.NewClientConn(conn, address, &ssh.ClientConfig{User: "root", HostKeyCallback: func(_ string, _ net.Addr, k ssh.PublicKey) error {
		fp = ssh.FingerprintSHA256(k)
		algorithm = k.Type()
		return errFingerprintCaptured
	}})
	if fp == "" {
		return "", "", errors.New("SSH 握手失败，未获得真实主机指纹")
	}
	return fp, algorithm, nil
}

func PinnedHostKey(fingerprint string) ssh.HostKeyCallback {
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		if subtle.ConstantTimeCompare([]byte(fingerprint), []byte(ssh.FingerprintSHA256(key))) != 1 {
			return errors.New("SSH 主机指纹已变化，执行已中止")
		}
		return nil
	}
}

type limitedOutput struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *limitedOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if b.buffer.Len()+n > 256*1024 {
		return n, errors.New("远端输出超出限制")
	}
	_, _ = b.buffer.Write(p)
	return n, nil
}
func (b *limitedOutput) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buffer.Bytes()...)
}
func (SSHRemote) Run(ctx context.Context, s SSH, script string) ([]byte, error) {
	if err := ValidateSSH(s); err != nil {
		return nil, err
	}
	address := net.JoinHostPort(s.Host, strconv.Itoa(s.Port))
	conn, err := (&net.Dialer{Timeout: 15 * time.Second}).DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, errors.New("SSH 连接失败")
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	clientConn, chans, reqs, err := ssh.NewClientConn(conn, address, &ssh.ClientConfig{User: s.User, Auth: []ssh.AuthMethod{ssh.Password(s.Password)}, HostKeyCallback: PinnedHostKey(s.Fingerprint), Timeout: 15 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("SSH 握手/认证失败（请检查凭据与指纹）")
	}
	_ = conn.SetDeadline(time.Time{})
	client := ssh.NewClient(clientConn, chans, reqs)
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		return nil, errors.New("SSH 会话创建失败")
	}
	defer session.Close()
	var output limitedOutput
	session.Stdout = &output
	session.Stderr = &output
	session.Stdin = bytes.NewBufferString(script)
	done := make(chan error, 1)
	go func() { done <- session.Run("bash -s") }()
	select {
	case err = <-done:
		return output.Bytes(), err
	case <-ctx.Done():
		_ = client.Close()
		<-done
		return output.Bytes(), ctx.Err()
	}
}
