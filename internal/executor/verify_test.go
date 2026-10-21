package executor

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeProbe speaks the node's loopback TCP probe protocol on one connection.
type fakeProbe struct {
	// reply, when set, replaces the hello reply line.
	reply func(request map[string]any) string
	// connect answers a connect request: "" means success.
	connect string
}

func (p fakeProbe) serve(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		var request map[string]any
		if json.Unmarshal([]byte(line), &request) != nil {
			return
		}
		if request["operation"] == "hello" && p.reply != nil {
			io.WriteString(conn, p.reply(request))
			continue
		}
		request["node_id"] = strings.Repeat("a", 32)
		request["success"] = true
		if request["operation"] == "connect" && p.connect != "" {
			request["success"] = false
			request["error"] = p.connect
		}
		encoded, _ := json.Marshal(request)
		conn.Write(append(encoded, '\n'))
	}
}

type fakeTunnel struct {
	probe    *fakeProbe
	dialErr  error
	tls      map[string]*tls.Config // host -> server config; absent means the node refuses
	closed   bool
	mu       sync.Mutex
	dialHost []string
}

func (t *fakeTunnel) DialContext(ctx context.Context, address string) (net.Conn, error) {
	t.mu.Lock()
	t.dialHost = append(t.dialHost, address)
	t.mu.Unlock()
	if t.dialErr != nil {
		return nil, t.dialErr
	}
	client, server := net.Pipe()
	switch {
	case address == probeAddress && t.probe != nil:
		go t.probe.serve(server)
	case strings.HasSuffix(address, ":443") && t.tls[strings.TrimSuffix(address, ":443")] != nil:
		config := t.tls[strings.TrimSuffix(address, ":443")]
		go func() {
			conn := tls.Server(server, config)
			defer conn.Close()
			if conn.Handshake() == nil {
				io.Copy(io.Discard, conn)
			}
		}()
	default:
		// mieru answers the SOCKS request first; a refused target then closes.
		server.Close()
	}
	return client, nil
}

func (t *fakeTunnel) Close() error { t.closed = true; return nil }

func testCertificates(t *testing.T, hosts ...string) (*x509.CertPool, map[string]*tls.Config) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test root"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	caCert, _ := x509.ParseCertificate(caDER)
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	configs := map[string]*tls.Config{}
	for i, host := range hosts {
		leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		leaf := &x509.Certificate{SerialNumber: big.NewInt(int64(i + 2)), DNSNames: []string{host}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
		der, err := x509.CreateCertificate(rand.Reader, leaf, caCert, &leafKey.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		configs[host] = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: leafKey}}}
	}
	return pool, configs
}

// noProbeTunnel is a working mieru tunnel to a server without the MSBOOST
// probe: the probe connection closes, the comparison site answers.
func noProbeTunnel(t *testing.T) (*x509.CertPool, *fakeTunnel) {
	t.Helper()
	roots, sites := testCertificates(t, defaultVerifyAllowed)
	return roots, &fakeTunnel{tls: sites}
}

func verifyConfig(t *testing.T, pattern string) []byte {
	t.Helper()
	extra := ""
	if pattern != "" {
		extra = `,"trafficPattern":` + pattern
	}
	return []byte(`{"profiles":[{"profileName":"x","user":{"name":"u","password":"p"},"servers":[{"ipAddress":"8.8.8.8","domainName":"","portBindings":[{"port":31234,"protocol":"TCP"}]}]` + extra + `}],"activeProfile":"x"}`)
}

func reachable(context.Context, string) (net.Conn, error) {
	client, server := net.Pipe()
	server.Close()
	return client, nil
}

func TestVerifyClassifiesEveryOutcome(t *testing.T) {
	roots, sites := testCertificates(t, "www.nexon.com", "www.youtube.com")
	allowedOnly := map[string]*tls.Config{"www.nexon.com": sites["www.nexon.com"]}
	cases := []struct {
		name         string
		tunnel       *fakeTunnel
		openErr      error
		reachErr     error
		code         string
		game, filter string
	}{
		{name: "msboost node", tunnel: &fakeTunnel{probe: &fakeProbe{}, tls: allowedOnly}, code: VerifyOK, game: "ok", filter: "ok"},
		{name: "game refused", tunnel: &fakeTunnel{probe: &fakeProbe{connect: "refused"}, tls: allowedOnly}, code: VerifyOK, game: "refused", filter: "ok"},
		{name: "filter missing", tunnel: &fakeTunnel{probe: &fakeProbe{}, tls: sites}, code: VerifyOK, game: "ok", filter: "missing"},
		{name: "filter inconclusive", tunnel: &fakeTunnel{probe: &fakeProbe{}}, code: VerifyOK, game: "ok", filter: "inconclusive"},
		{name: "entry closed", tunnel: &fakeTunnel{probe: &fakeProbe{}}, reachErr: errors.New("refused"), code: VerifyEntryUnreachable},
		{name: "tunnel fails", tunnel: &fakeTunnel{dialErr: errors.New("socks connect refused")}, code: VerifyTunnelFailed},
		// mihomo accepts every connect; a tunnel that never came up then
		// closes the probe and the comparison site alike.
		{name: "tunnel never comes up", tunnel: &fakeTunnel{}, code: VerifyTunnelFailed},
		{name: "no probe service", tunnel: &fakeTunnel{tls: allowedOnly}, code: VerifyNotMSBOOST},
		{name: "executor cannot start client", openErr: errors.New("noexec"), code: VerifyUnavailable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := &Verifier{Roots: roots, Reach: reachable, Open: func(ctx context.Context, p TunnelProfile) (Tunnel, error) {
				if p.Host != "8.8.8.8" || p.Port != 31234 || p.Username != "u" || p.Password != "p" {
					t.Fatalf("profile not taken from the configuration: %+v", p)
				}
				if c.openErr != nil {
					return nil, c.openErr
				}
				return c.tunnel, nil
			}}
			if c.reachErr != nil {
				v.Reach = func(context.Context, string) (net.Conn, error) { return nil, c.reachErr }
			}
			got := v.Verify(context.Background(), verifyConfig(t, ""), VerifyOptions{Mode: "log"})
			if got.Code != c.code || got.Game != c.game || got.Filter != c.filter {
				t.Fatalf("got %+v, want code=%s game=%s filter=%s", got, c.code, c.game, c.filter)
			}
			if c.tunnel != nil && c.openErr == nil && c.reachErr == nil && !c.tunnel.closed {
				t.Fatal("tunnel left running")
			}
		})
	}
}

func TestVerifyRejectsHelloImitations(t *testing.T) {
	replies := map[string]func(map[string]any) string{
		"wrong nonce": func(r map[string]any) string {
			return `{"version":2,"operation":"hello","nonce":"` + strings.Repeat("b", 32) + `","node_id":"` + strings.Repeat("a", 32) + `","success":true}` + "\n"
		},
		"extra field": func(r map[string]any) string {
			return `{"version":2,"operation":"hello","nonce":"` + r["nonce"].(string) + `","node_id":"` + strings.Repeat("a", 32) + `","success":true,"x":1}` + "\n"
		},
		"missing node id": func(r map[string]any) string {
			return `{"version":2,"operation":"hello","nonce":"` + r["nonce"].(string) + `","success":true}` + "\n"
		},
		"repeated field": func(r map[string]any) string {
			return `{"version":2,"version":2,"operation":"hello","nonce":"` + r["nonce"].(string) + `","node_id":"` + strings.Repeat("a", 32) + `","success":true}` + "\n"
		},
		"node id format": func(r map[string]any) string {
			return `{"version":2,"operation":"hello","nonce":"` + r["nonce"].(string) + `","node_id":"NODE","success":true}` + "\n"
		},
		"not successful": func(r map[string]any) string {
			return `{"version":2,"operation":"hello","nonce":"` + r["nonce"].(string) + `","node_id":"` + strings.Repeat("a", 32) + `","success":false}` + "\n"
		},
		"nested value": func(r map[string]any) string {
			return `{"version":2,"operation":"hello","nonce":{"v":"` + r["nonce"].(string) + `"},"node_id":"` + strings.Repeat("a", 32) + `","success":true}` + "\n"
		},
	}
	for name, reply := range replies {
		t.Run(name, func(t *testing.T) {
			v := &Verifier{Reach: reachable, Open: func(context.Context, TunnelProfile) (Tunnel, error) {
				return &fakeTunnel{probe: &fakeProbe{reply: reply}}, nil
			}}
			if got := v.Verify(context.Background(), verifyConfig(t, ""), VerifyOptions{Mode: "log"}); got.Code != VerifyNotMSBOOST {
				t.Fatalf("an imitation passed as MSBOOST: %+v", got)
			}
		})
	}
}

func TestVerifyCarriesTheTrafficPattern(t *testing.T) {
	var got TunnelProfile
	v := &Verifier{Reach: reachable, Open: func(_ context.Context, p TunnelProfile) (Tunnel, error) {
		got = p
		return &fakeTunnel{}, nil
	}}
	v.Verify(context.Background(), verifyConfig(t, `{"lowEntropy":{"mode":"LOW_ENTROPY_MODE_48"}}`), VerifyOptions{Mode: "log"})
	if got.TrafficPattern != "MgIIAw==" {
		t.Fatalf("pattern %q", got.TrafficPattern)
	}
	// A pattern this executor cannot encode is left out instead of failing.
	v.Verify(context.Background(), verifyConfig(t, `{"futureField":true}`), VerifyOptions{Mode: "log"})
	if got.TrafficPattern != "" {
		t.Fatalf("unknown pattern forwarded: %q", got.TrafficPattern)
	}
}

func TestEncodeTrafficPattern(t *testing.T) {
	// Values the node installer writes for each low-entropy level.
	for level, want := range map[string]string{"32": "MgIIAQ==", "40": "MgIIAg==", "48": "MgIIAw==", "56": "MgIIBA=="} {
		got, err := EncodeTrafficPattern(map[string]any{"lowEntropy": map[string]any{"mode": "LOW_ENTROPY_MODE_" + level}})
		if err != nil || got != want {
			t.Fatalf("level %s: %q %v", level, got, err)
		}
	}
	var pattern any
	json.Unmarshal([]byte(`{"seed":-1,"unlockAll":false,"tcpFragment":{"enable":true,"maxSleepMs":"20"},"nonce":{"type":"NONCE_TYPE_FIXED","minLen":0,"customHexStrings":["00010203","ff"]},"padding":{"maxEndPaddingLen":255},"lowEntropy":{"mode":3,"maskRotation":"LOW_ENTROPY_MASK_ROTATE_LEFT_2"}}`), &pattern)
	got, err := EncodeTrafficPattern(pattern)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := base64.StdEncoding.DecodeString(got)
	want := []byte{
		0x08, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01, // seed -1
		0x10, 0x00, // unlockAll false is still present
		0x1a, 0x04, 0x08, 0x01, 0x10, 0x14, // tcpFragment
		0x22, 0x0c, 0x08, 0x03, 0x18, 0x00, 0x2a, 0x08, '0', '0', '0', '1', '0', '2', '0', '3', 0x2a, 0x02, 'f', 'f',
		0x2a, 0x03, 0x10, 0xff, 0x01, // padding
		0x32, 0x04, 0x08, 0x03, 0x10, 0x20, // lowEntropy
	}
	// The nonce message length covers type, minLen and both strings.
	want[20] = byte(2 + 2 + 10 + 4)
	if string(raw) != string(want) {
		t.Fatalf("encoded %x\nwant    %x", raw, want)
	}
	for _, bad := range []string{`{"lowEntropy":{"mode":"LOW_ENTROPY_MODE_64"}}`, `{"seed":4294967296}`, `{"nonce":{"type":true}}`, `[]`, `{"other":1}`} {
		var value any
		json.Unmarshal([]byte(bad), &value)
		if _, err := EncodeTrafficPattern(value); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
}

func TestSOCKSConnectSendsTheRequestedTarget(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	requests := make(chan []byte, 4)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				greeting := make([]byte, 3)
				io.ReadFull(conn, greeting)
				conn.Write([]byte{5, 0})
				head := make([]byte, 4)
				io.ReadFull(conn, head)
				var rest []byte
				switch head[3] {
				case 1:
					rest = make([]byte, 6)
				case 3:
					n := make([]byte, 1)
					io.ReadFull(conn, n)
					rest = append(n, make([]byte, int(n[0])+2)...)
					io.ReadFull(conn, rest[1:])
					requests <- append(head, rest...)
					conn.Write([]byte{5, 2, 0, 1, 0, 0, 0, 0, 0, 0})
					return
				}
				io.ReadFull(conn, rest)
				requests <- append(head, rest...)
				conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 80})
				conn.Write([]byte("hi"))
			}()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := socksConnect(ctx, listener.Addr().String(), "127.0.0.1:20424")
	if err != nil {
		t.Fatal(err)
	}
	if got := <-requests; string(got) != string([]byte{5, 1, 0, 1, 127, 0, 0, 1, 0x4f, 0xc8}) {
		t.Fatalf("request %v", got)
	}
	data := make([]byte, 2)
	if _, err := io.ReadFull(conn, data); err != nil || string(data) != "hi" {
		t.Fatalf("payload after reply lost: %q %v", data, err)
	}
	conn.Close()
	if _, err := socksConnect(ctx, listener.Addr().String(), "www.youtube.com:443"); err == nil {
		t.Fatal("a refused SOCKS reply was accepted")
	}
	if got := <-requests; string(got[4:5]) != string([]byte{15}) || string(got[5:20]) != "www.youtube.com" {
		t.Fatalf("domain request %v", got)
	}
}

func TestVerifyOptionsValidation(t *testing.T) {
	good := []VerifyOptions{{Mode: "log"}, {Mode: "enforce", GameTarget: "35.155.204.207:8585", FilterBlocked: "www.youtube.com", FilterAllowed: "www.nexon.com"}}
	for _, o := range good {
		if err := ValidateVerifyOptions(&o); err != nil {
			t.Fatalf("%+v: %v", o, err)
		}
	}
	bad := []VerifyOptions{{Mode: ""}, {Mode: "off"}, {Mode: "log", GameTarget: "10.0.0.1:80"}, {Mode: "log", GameTarget: "example.com:80"}, {Mode: "log", GameTarget: "[2001:4860::1]:80"}, {Mode: "log", FilterBlocked: "WWW.YOUTUBE.COM"}, {Mode: "log", FilterAllowed: "nexon"}}
	for _, o := range bad {
		if err := ValidateVerifyOptions(&o); err == nil {
			t.Fatalf("accepted %+v", o)
		}
	}
}
