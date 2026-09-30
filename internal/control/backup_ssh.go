package control

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/mozziexwz/node/internal/executor"
	"golang.org/x/crypto/ssh"
)

// A server can advertise RSA/ECDSA ahead of the ED25519 key whose fingerprint
// the administrator verified. Try other supported key types only when the
// callback rejected a mismatching key, BEFORE SSH authentication is allowed.
// Never retry an authentication failure or accept an unpinned key.
func (b *BackupService) connectBackupSSH(ctx context.Context, address string, cfg *ssh.ClientConfig, fingerprint string) (*ssh.Client, func(), error) {
	dial := (&net.Dialer{Timeout: 15 * time.Second}).DialContext
	if b.dialContext != nil {
		dial = b.dialContext
	}
	algorithms := ssh.SupportedAlgorithms().HostKeys
	for len(algorithms) > 0 {
		conn, err := dial(ctx, "tcp", address)
		if err != nil {
			return nil, nil, errors.New("SFTP 连接失败")
		}
		stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
		_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
		attempt := *cfg
		attempt.HostKeyAlgorithms = algorithms
		mismatchType := ""
		attempt.HostKeyCallback = func(host string, addr net.Addr, key ssh.PublicKey) error {
			err := executor.PinnedHostKey(fingerprint)(host, addr, key)
			if err != nil {
				mismatchType = key.Type()
			}
			return err
		}
		cc, channels, requests, err := ssh.NewClientConn(conn, address, &attempt)
		if err == nil {
			_ = conn.SetDeadline(time.Now().Add(3 * time.Minute))
			return ssh.NewClient(cc, channels, requests), func() { stop(); _ = conn.Close() }, nil
		}
		stop()
		_ = conn.Close()
		if mismatchType == "" || ctx.Err() != nil {
			return nil, nil, errors.New("SFTP 主机指纹核对或 SSH 认证失败")
		}
		remaining := make([]string, 0, len(algorithms))
		for _, algorithm := range algorithms {
			format := algorithm
			switch algorithm {
			case ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSASHA512:
				format = ssh.KeyAlgoRSA
			case ssh.CertAlgoRSASHA256v01, ssh.CertAlgoRSASHA512v01:
				format = ssh.CertAlgoRSAv01
			}
			if format != mismatchType {
				remaining = append(remaining, algorithm)
			}
		}
		if len(remaining) == len(algorithms) {
			break
		}
		algorithms = remaining
	}
	return nil, nil, errors.New("SFTP 主机未提供已核对指纹对应的受支持密钥")
}
