package relayruntime

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"time"
)

// Each node receives only its own private key. Peers pin its public certificate
// and verify the generated DNS identity, regardless of the connection IP.
func NewTLSIdentity(name string, expires time.Time) (certificate, privateKey string, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return "", "", err
	}
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: name}, DNSNames: []string{name}, NotBefore: time.Now().Add(-time.Hour), NotAfter: expires, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true, IsCA: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return "", "", err
	}
	raw, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: raw})), nil
}

type gostTLSFiles struct {
	certificate, key string
	cas              []string
}

// Runtime-created paths only: caller-controlled configuration cannot name a
// local certificate file. Cleanup also runs after a failed GOST start.
func prepareGostConfig(rule Rule, observerURL, dir string) ([]byte, func(), error) {
	paths := []string{}
	cleanup := func() {
		for _, path := range paths {
			_ = os.Remove(path)
		}
	}
	write := func(data string) (string, error) {
		f, err := os.CreateTemp(dir, "tls-*")
		if err != nil {
			return "", err
		}
		paths = append(paths, f.Name())
		if err = f.Chmod(0600); err == nil {
			_, err = f.WriteString(data)
		}
		closeErr := f.Close()
		if err == nil {
			err = closeErr
		}
		return f.Name(), err
	}
	files := gostTLSFiles{}
	var err error
	if rule.Protocol == "tls" {
		if _, err = tls.X509KeyPair([]byte(rule.TLSCertificate), []byte(rule.TLSPrivateKey)); err != nil {
			return nil, cleanup, errors.New("invalid TLS node identity")
		}
		files.certificate, err = write(rule.TLSCertificate)
		if err != nil {
			return nil, cleanup, err
		}
		files.key, err = write(rule.TLSPrivateKey)
		if err != nil {
			return nil, cleanup, err
		}
	}
	for _, peer := range rule.TargetTLS {
		pool := x509.NewCertPool()
		if peer.ServerName == "" || !pool.AppendCertsFromPEM([]byte(peer.CA)) {
			return nil, cleanup, errors.New("invalid pinned TLS peer")
		}
		path, err := write(peer.CA)
		if err != nil {
			return nil, cleanup, err
		}
		files.cas = append(files.cas, path)
	}
	raw, err := gostConfig(rule, observerURL, files)
	return raw, cleanup, err
}
