package executor

import (
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

type SSHRemote struct {
	// Private test seam; production always uses a bounded TCP dialer.
	dialContext func(context.Context, string, string) (net.Conn, error)
}

func (remote SSHRemote) dial(ctx context.Context, address string, timeout time.Duration) (net.Conn, error) {
	if remote.dialContext != nil {
		return remote.dialContext(ctx, "tcp", address)
	}
	return (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", address)
}

var errFingerprintCaptured = errors.New("host key captured without authentication")

func (remote SSHRemote) Probe(ctx context.Context, host string, port int) (string, string, error) {
	if err := PublicIP(host); err != nil {
		return "", "", err
	}
	if port < 1 || port > 65535 {
		return "", "", errors.New("SSH 端口无效")
	}
	address := net.JoinHostPort(host, strconv.Itoa(port))
	conn, err := remote.dial(ctx, address, 12*time.Second)
	if err != nil {
		return "", "", connectDiagnostic(err)
	}
	defer conn.Close()
	stopClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopClose()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	var fp, algorithm string
	_, _, _, _ = ssh.NewClientConn(conn, address, &ssh.ClientConfig{User: "root", HostKeyCallback: func(_ string, _ net.Addr, k ssh.PublicKey) error {
		fp = ssh.FingerprintSHA256(k)
		algorithm = k.Type()
		return errFingerprintCaptured
	}})
	if fp == "" {
		if ctx.Err() != nil {
			return "", "", ctx.Err()
		}
		return "", "", diagnosticError("ssh_handshake")
	}
	return fp, algorithm, nil
}

func PinnedHostKey(fingerprint string) ssh.HostKeyCallback {
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		if subtle.ConstantTimeCompare([]byte(fingerprint), []byte(ssh.FingerprintSHA256(key))) != 1 {
			return diagnosticError("ssh_host_changed")
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
func (remote SSHRemote) Run(ctx context.Context, s SSH, script string) ([]byte, error) {
	if err := ValidateSSH(s); err != nil {
		return nil, err
	}
	address := net.JoinHostPort(s.Host, strconv.Itoa(s.Port))
	conn, err := remote.dial(ctx, address, 15*time.Second)
	if err != nil {
		return nil, connectDiagnostic(err)
	}
	defer conn.Close()
	// Cancellation must cover handshake AND synchronous channel-open, not only
	// session.Run below. A peer can authenticate yet never answer NewSession.
	stopClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopClose()
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	hostChanged := false
	pin := PinnedHostKey(s.Fingerprint)
	clientConn, chans, reqs, err := ssh.NewClientConn(conn, address, &ssh.ClientConfig{User: s.User, Auth: []ssh.AuthMethod{ssh.Password(s.Password)}, HostKeyCallback: func(host string, addr net.Addr, key ssh.PublicKey) error {
		err := pin(host, addr, key)
		hostChanged = err != nil
		return err
	}, Timeout: 15 * time.Second})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if hostChanged {
			return nil, diagnosticError("ssh_host_changed")
		}
		if strings.Contains(err.Error(), "unable to authenticate") {
			return nil, diagnosticError("ssh_auth")
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return nil, diagnosticError("ssh_timeout")
		}
		return nil, diagnosticError("ssh_handshake")
	}
	_ = conn.SetDeadline(time.Time{})
	client := ssh.NewClient(clientConn, chans, reqs)
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, diagnosticError("ssh_session")
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
		if ctx.Err() != nil {
			return output.Bytes(), ctx.Err()
		}
		return output.Bytes(), err
	case <-ctx.Done():
		_ = client.Close()
		<-done
		return output.Bytes(), ctx.Err()
	}
}
