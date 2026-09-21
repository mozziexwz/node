//go:build linux

package relayruntime

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// Bounded certificate-time proof: actual two-hop GOST with production TLS
// configuration/child lifecycle, not full Run, control/PG, or TLS rotation.
// Only certificate validity times vary; a valid control must actually echo.
func TestRealGostTLSCertificateTimeBoundaries(t *testing.T) {
	if os.Getenv("MSBOOST_REAL_GOST_TLS_TIME") != "1" {
		t.Skip("opt-in real GOST expired/not-yet-valid certificate handshake test")
	}
	interfaces, err := net.Interfaces()
	if os.Geteuid() != 0 || os.Getenv("MSBOOST_TEST_LOOPBACK_NETNS") != "1" || err != nil || len(interfaces) != 1 || interfaces[0].Name != "lo" {
		t.Fatal("requires root in a new loopback-only network namespace")
	}
	binary := os.Getenv("GOST_TEST_BINARY")
	info, err := os.Lstat(binary)
	if !filepath.IsAbs(binary) || err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 {
		t.Fatal("trusted absolute non-writable GOST executable required")
	}
	raw, err := os.ReadFile(binary)
	if err != nil || fmt.Sprintf("%x", sha256.Sum256(raw)) != "1d8f971e9447cf4114fb1376b85c8c14e840db50ae8dbe895398a34e97c13e08" {
		t.Fatal("requires pinned Linux amd64 GOST 3.3.0")
	}
	now := time.Now().Truncate(time.Second)
	for _, tc := range []struct {
		name                string
		notBefore, notAfter time.Time
		valid               bool
	}{
		{"valid", now.Add(-time.Hour), now.Add(time.Hour), true},
		{"expired", now.Add(-2 * time.Hour), now.Add(-time.Hour), false},
		{"not-yet-valid", now.Add(time.Hour), now.Add(2 * time.Hour), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ca, certificate, key := tlsTimeFixtureIdentity(t, now, tc.notBefore, tc.notAfter, tc.valid)
			testRealGostTLSCertificateTime(t, binary, ca, certificate, key, tc.valid)
		})
	}
}

// Verify the generated chain is legal at its own midpoint and that failures
// at the real observation time are specifically certificate-time failures.
func tlsTimeFixtureIdentity(t *testing.T, now, notBefore, notAfter time.Time, valid bool) (string, string, string) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "isolated TLS time fixture CA"}, NotBefore: now.Add(-24 * time.Hour), NotAfter: now.Add(24 * time.Hour), BasicConstraintsValid: true, IsCA: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err = x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "relay.time.invalid"}, DNSNames: []string{"relay.time.invalid"}, NotBefore: notBefore, NotAfter: notAfter, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err = x509.ParseCertificate(leafDER)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	options := x509.VerifyOptions{Roots: roots, DNSName: "relay.time.invalid", CurrentTime: now}
	_, err = leaf.Verify(options)
	if valid {
		if err != nil {
			t.Fatalf("valid fixture certificate rejected: %v", err)
		}
	} else {
		var invalid x509.CertificateInvalidError
		if !errors.As(err, &invalid) || invalid.Reason != x509.Expired {
			t.Fatalf("bad time fixture is not an exact certificate-time failure: %v", err)
		}
	}
	options.CurrentTime = notBefore.Add(notAfter.Sub(notBefore) / 2)
	if _, err := leaf.Verify(options); err != nil {
		t.Fatalf("time fixture has an unrelated trust/name/key usage defect: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(leafKey)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})), string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})), string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
}

func testRealGostTLSCertificateTime(t *testing.T, binary, ca, certificate, key string, valid bool) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	echo, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = echo.Close() })
	var delivered atomic.Int64
	go func() {
		for {
			conn, err := echo.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
				buf := make([]byte, 4096)
				for {
					n, err := conn.Read(buf)
					if n > 0 {
						delivered.Add(int64(n))
						if _, writeErr := conn.Write(buf[:n]); writeErr != nil {
							return
						}
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	s := &runtimeState{cfg: Config{StateDir: dir, GostBinary: binary}, processes: map[string]*process{}, pending: map[string]Traffic{}, journal: filepath.Join(dir, "journal.json"), observerToken: "isolated-time-fixture"}
	observer := httptest.NewServer(httpHandler(s))
	t.Cleanup(observer.Close)
	s.observerURL = observer.URL + "/observer?token=" + s.observerToken
	// Wait for actual reaping before observer and private directory cleanup.
	t.Cleanup(func() {
		cancel()
		s.mu.Lock()
		processes := make([]*process, 0, len(s.processes))
		for id, p := range s.processes {
			s.stopLocked(id)
			processes = append(processes, p)
		}
		s.mu.Unlock()
		for _, p := range processes {
			select {
			case <-p.done:
			case <-time.After(5 * time.Second):
				t.Error("TLS time fixture failed to reap GOST")
			}
		}
	})
	// Keep both reservations open together so the OS cannot return one port twice.
	probes := make([]net.Listener, 0, 2)
	for i := 0; i < 2; i++ {
		probe, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			for _, p := range probes {
				_ = p.Close()
			}
			t.Fatal(err)
		}
		probes = append(probes, probe)
	}
	exitPort, entryPort := probes[0].Addr().(*net.TCPAddr).Port, probes[1].Addr().(*net.TCPAddr).Port
	for _, p := range probes {
		_ = p.Close()
	}
	rules := []Rule{
		{ID: "tls-time-exit", Version: 1, ListenPort: exitPort, Protocol: "tls", RateMbps: 100, Targets: []string{echo.Addr().String()}, AllowedSources: []string{"127.0.0.1"}, Strategy: "round", TLSCertificate: certificate, TLSPrivateKey: key},
		{ID: "tls-time-entry", Version: 1, ListenPort: entryPort, Protocol: "tcp", RateMbps: 100, Targets: []string{fmt.Sprintf("127.0.0.1:%d", exitPort)}, Strategy: "round", TargetTLS: []TLSClient{{CA: ca, ServerName: "relay.time.invalid"}}},
	}
	for _, rule := range rules {
		s.mu.Lock()
		err = s.startLocked(ctx, rule)
		s.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(12 * time.Second)
	for {
		s.mu.Lock()
		ready, failed := len(s.processes) == 2, false
		for _, p := range s.processes {
			ready = ready && p.ack.State == "ready" && !processDone(p)
			failed = failed || p.ack.State == "failed" || processDone(p)
		}
		s.mu.Unlock()
		if failed {
			t.Fatal("GOST startup failed instead of exercising certificate handshake")
		}
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("both real GOST listeners did not acknowledge readiness")
		}
		time.Sleep(30 * time.Millisecond)
	}
	conn, err := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", entryPort), time.Second)
	if err != nil {
		t.Fatal("GOST entry listener was not reachable")
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	payload := "exact-certificate-time-fixture"
	if _, err := io.WriteString(conn, payload); err != nil {
		t.Fatalf("could not submit a handshake-triggering TCP payload: %v", err)
	}
	reply := make([]byte, len(payload))
	n, readErr := io.ReadFull(conn, reply)
	if valid {
		if readErr != nil || string(reply) != payload || delivered.Load() < int64(len(payload)) {
			t.Fatalf("valid certificate control did not complete real two-hop echo: %v", readErr)
		}
	} else if n != 0 || (!errors.Is(readErr, io.EOF) && !errors.Is(readErr, syscall.ECONNRESET)) || delivered.Load() != 0 {
		t.Fatalf("bad certificate time must close without business bytes, not timeout: read=%d delivered=%d error=%v", n, delivered.Load(), readErr)
	}
	s.mu.Lock()
	for _, p := range s.processes {
		if p.stopping || processDone(p) || p.ack.State != "ready" {
			s.mu.Unlock()
			t.Fatal("GOST process failure incorrectly masqueraded as certificate rejection")
		}
	}
	s.mu.Unlock()
	t.Logf("REAL_GOST_TLS_TIME_PASS valid_control=%t two_actual_gost_listeners=true secure_ca_and_name=true no_timeout_success=true delivered_bytes=%d", valid, delivered.Load())
}
