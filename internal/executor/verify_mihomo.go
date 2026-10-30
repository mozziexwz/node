package executor

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"time"
)

func base64Std(data []byte) string { return base64.StdEncoding.EncodeToString(data) }

// mihomo is the same release the node installer pins (v1.19.33), checked
// against the reviewed archive digests before it is ever run. It is GPL-3.0
// software, downloaded and run as its own process.
type mihomoRelease struct{ URL, SHA256 string }

var mihomoReleases = map[string]mihomoRelease{
	"amd64": {"https://github.com/MetaCubeX/mihomo/releases/download/v1.19.33/mihomo-linux-amd64-compatible-v1.19.33.gz", "386508bfb1c72c43b6a0eda73708ecdce82546581b0ebcb16366c53757c622b6"},
	"arm64": {"https://github.com/MetaCubeX/mihomo/releases/download/v1.19.33/mihomo-linux-arm64-v1.19.33.gz", "ba408b317080ee265c46cb923a50018c1b82cbd708551cdd46a1e3e307396187"},
}

type mihomoRunner struct {
	mu sync.Mutex
	// binary is a verified copy in the service's private temporary directory;
	// sha is the digest of that copy, checked again before every start.
	binary, sha string
	// fixed, when set, is an operator- or test-supplied binary used as is.
	fixed string
}

var defaultMihomo = &mihomoRunner{fixed: os.Getenv("MSBOOST_MIHOMO_BINARY")}

func (m *mihomoRunner) program(ctx context.Context) (string, error) {
	if m.fixed != "" {
		return m.fixed, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.binary != "" {
		if digest, err := fileSHA256(m.binary); err == nil && digest == m.sha {
			return m.binary, nil
		}
		m.binary, m.sha = "", ""
	}
	release, ok := mihomoReleases[runtime.GOARCH]
	if runtime.GOOS != "linux" || !ok {
		return "", errors.New("unsupported platform for relay verification")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, release.URL, nil)
	if err != nil {
		return "", err
	}
	response, err := (&http.Client{Timeout: 3 * time.Minute}).Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", errors.New("mihomo download failed")
	}
	archive, err := io.ReadAll(io.LimitReader(response.Body, 64<<20))
	if err != nil {
		return "", err
	}
	if sum := sha256.Sum256(archive); hex.EncodeToString(sum[:]) != release.SHA256 {
		return "", errors.New("mihomo archive digest mismatch")
	}
	reader, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return "", err
	}
	program, err := io.ReadAll(io.LimitReader(reader, 256<<20))
	if err != nil || len(program) < 4 || !bytes.Equal(program[:4], []byte{0x7f, 'E', 'L', 'F'}) {
		return "", errors.New("mihomo archive content invalid")
	}
	dir := filepath.Join(os.TempDir(), "msboost-verify")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	staged, err := os.CreateTemp(dir, "mihomo-*")
	if err != nil {
		return "", err
	}
	if _, err := staged.Write(program); err != nil {
		staged.Close()
		os.Remove(staged.Name())
		return "", err
	}
	if err := staged.Chmod(0o700); err != nil || staged.Close() != nil {
		os.Remove(staged.Name())
		return "", errors.New("mihomo staging failed")
	}
	sum := sha256.Sum256(program)
	m.binary, m.sha = staged.Name(), hex.EncodeToString(sum[:])
	return m.binary, nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// Open starts mihomo with one SOCKS listener on loopback and one mieru proxy.
// The configuration reaches the process only through its environment, which
// other users cannot read, and the working directory is removed on Close.
func (m *mihomoRunner) Open(ctx context.Context, p TunnelProfile) (Tunnel, error) {
	binary, err := m.program(ctx)
	if err != nil {
		return nil, err
	}
	home, err := os.MkdirTemp("", "msboost-verify-run-*")
	if err != nil {
		return nil, err
	}
	port, err := freeLoopbackPort()
	if err != nil {
		os.RemoveAll(home)
		return nil, err
	}
	proxy := map[string]any{
		"name": "verify", "type": "mieru", "server": p.Host, "port": p.Port, "transport": "TCP",
		"username": p.Username, "password": p.Password,
		// With the standard handshake mihomo waits for the node's answer and
		// closes the connection when the node refuses. Its SOCKS listener
		// still accepts every connect at once (see Verifier.Verify).
		"multiplexing": "MULTIPLEXING_LOW", "handshake-mode": "HANDSHAKE_STANDARD",
	}
	if p.TrafficPattern != "" {
		proxy["traffic-pattern"] = p.TrafficPattern
	}
	// JSON is valid YAML, so the configuration needs no hand-written quoting.
	config, err := json.Marshal(map[string]any{
		"mode": "rule", "log-level": "silent", "ipv6": false, "allow-lan": false,
		"find-process-mode": "off", "geo-auto-update": false,
		"profile":   map[string]any{"store-selected": false, "store-fake-ip": false},
		"listeners": []any{map[string]any{"name": "verify-in", "type": "socks", "listen": "127.0.0.1", "port": port, "udp": false}},
		"proxies":   []any{proxy},
		"rules":     []string{"MATCH,verify"},
	})
	if err != nil {
		os.RemoveAll(home)
		return nil, err
	}
	cmd := exec.Command(binary, "-d", home)
	cmd.Dir = home
	cmd.Env = []string{"CLASH_CONFIG_STRING=" + base64Std(config), "CLASH_HOME_DIR=" + home, "HOME=" + home, "PATH=/usr/sbin:/usr/bin:/sbin:/bin"}
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		os.RemoveAll(home)
		return nil, err
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	t := &mihomoTunnel{cmd: cmd, exited: exited, home: home, proxy: net.JoinHostPort("127.0.0.1", strconv.Itoa(port))}
	ready, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for {
		conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(ready, "tcp", t.proxy)
		if err == nil {
			_ = conn.Close()
			return t, nil
		}
		select {
		case <-exited:
			t.Close()
			return nil, errors.New("mihomo exited before listening")
		case <-ready.Done():
			t.Close()
			return nil, errors.New("mihomo did not start listening")
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func freeLoopbackPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

type mihomoTunnel struct {
	cmd    *exec.Cmd
	exited chan struct{}
	home   string
	proxy  string
	once   sync.Once
}

func (t *mihomoTunnel) DialContext(ctx context.Context, address string) (net.Conn, error) {
	return socksConnect(ctx, t.proxy, address)
}

func (t *mihomoTunnel) Close() error {
	t.once.Do(func() {
		_ = t.cmd.Process.Kill()
		select {
		case <-t.exited:
		case <-time.After(5 * time.Second):
		}
		_ = os.RemoveAll(t.home)
	})
	return nil
}
