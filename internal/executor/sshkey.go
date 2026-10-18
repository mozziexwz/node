package executor

import (
	"crypto/x509"
	"errors"
	"strings"

	"golang.org/x/crypto/ssh"
)

const maxPrivateKeyBytes = 16 * 1024

// ParsePrivateKey accepts ed25519, ECDSA and RSA private keys in the formats
// OpenSSH reads (openssh-key-v1 and the classic PEM encodings), with an
// optional passphrase. A passphrase given for an unencrypted key is ignored.
func ParsePrivateKey(key, passphrase string) (ssh.Signer, error) {
	if len(key) == 0 || len(key) > maxPrivateKeyBytes {
		return nil, errors.New("SSH 私钥为空或超过 16 KB")
	}
	if len(passphrase) > 1024 || strings.ContainsAny(passphrase, "\x00\r\n") {
		return nil, errors.New("私钥口令无效")
	}
	if strings.HasPrefix(strings.TrimSpace(key), "PuTTY-User-Key-File-") {
		return nil, errors.New("PuTTY 的 .ppk 请先用 PuTTYgen 导出为 OpenSSH 格式")
	}
	signer, err := ssh.ParsePrivateKey([]byte(key))
	var missing *ssh.PassphraseMissingError
	if errors.As(err, &missing) {
		if passphrase == "" {
			return nil, errors.New("私钥已加密，请填写私钥口令")
		}
		signer, err = ssh.ParsePrivateKeyWithPassphrase([]byte(key), []byte(passphrase))
		if errors.Is(err, x509.IncorrectPasswordError) {
			return nil, errors.New("私钥口令不正确")
		}
	}
	if err != nil {
		return nil, errors.New("无法识别 SSH 私钥，请使用 OpenSSH 格式的 ed25519、ECDSA 或 RSA 私钥")
	}
	switch signer.PublicKey().Type() {
	case ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521, ssh.KeyAlgoRSA:
		return signer, nil
	}
	return nil, errors.New("仅支持 ed25519、ECDSA、RSA 私钥")
}

// AuthorizedKey is the public half of a validated private key in
// authorized_keys form, without a comment or trailing newline.
func AuthorizedKey(s SSH) (string, error) {
	signer, err := ParsePrivateKey(s.PrivateKey, s.Passphrase)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))), nil
}

// KeyLabel describes a key for display, e.g. "ED25519" or "RSA".
func KeyLabel(s SSH) string {
	if !s.UsesKey() {
		return ""
	}
	signer, err := ParsePrivateKey(s.PrivateKey, s.Passphrase)
	if err != nil {
		return ""
	}
	switch signer.PublicKey().Type() {
	case ssh.KeyAlgoED25519:
		return "ED25519"
	case ssh.KeyAlgoRSA:
		return "RSA"
	default:
		return "ECDSA"
	}
}

func sshAuthMethods(s SSH) ([]ssh.AuthMethod, error) {
	if s.UsesKey() {
		signer, err := ParsePrivateKey(s.PrivateKey, s.Passphrase)
		if err != nil {
			return nil, err
		}
		return []ssh.AuthMethod{ssh.PublicKeys(signer)}, nil
	}
	return []ssh.AuthMethod{ssh.Password(s.Password)}, nil
}
