package relayruntime

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// The public relay port is owned by the Agent, not by GOST. GOST listens only
// on a private loopback port behind this guard. This is deliberately mandatory
// for every rule; no control-plane or member option can disable it.
type socksGuard struct {
	listener net.Listener
	backend  string
	allowed  []net.IP
	permits  chan struct{}
	mu       sync.Mutex
	active   map[net.Conn]struct{}
	closed   chan struct{}
	done     chan struct{}
	once     sync.Once
}

func reserveGuardBackend(publicPort int) (net.Listener, error) {
	if publicPort < 1 || publicPort > 65535 {
		return nil, errors.New("invalid relay port")
	}
	for i := 0; i < 16; i++ {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		if listener.Addr().(*net.TCPAddr).Port != publicPort {
			return listener, nil
		}
		_ = listener.Close()
	}
	return nil, errors.New("cannot reserve a private relay port")
}

func parseGuardSources(sources []string) ([]net.IP, error) {
	allowed := make([]net.IP, 0, len(sources))
	for _, source := range sources {
		ip := net.ParseIP(source)
		if ip == nil {
			return nil, errors.New("invalid upstream source IP")
		}
		allowed = append(allowed, ip)
	}
	return allowed, nil
}

func newSocksGuard(rule Rule, backend string) (*socksGuard, error) {
	allowed, err := parseGuardSources(rule.AllowedSources)
	if err != nil {
		return nil, err
	}
	var tlsConfig *tls.Config
	if rule.Protocol == "tls" {
		identity, err := tls.X509KeyPair([]byte(rule.TLSCertificate), []byte(rule.TLSPrivateKey))
		if err != nil {
			return nil, errors.New("invalid TLS node identity")
		}
		tlsConfig = &tls.Config{Certificates: []tls.Certificate{identity}, MinVersion: tls.VersionTLS12}
	}
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", rule.ListenPort))
	if err != nil {
		return nil, err
	}
	if tlsConfig != nil {
		listener = tls.NewListener(listener, tlsConfig)
	}
	return &socksGuard{listener: listener, backend: backend, allowed: allowed, permits: make(chan struct{}, 4096), active: make(map[net.Conn]struct{}), closed: make(chan struct{}), done: make(chan struct{})}, nil
}

func (g *socksGuard) close() {
	g.once.Do(func() {
		close(g.closed)
		_ = g.listener.Close()
		g.mu.Lock()
		for conn := range g.active {
			_ = conn.Close()
		}
		g.mu.Unlock()
	})
}

func (g *socksGuard) wait() { <-g.done }

func (g *socksGuard) track(conn net.Conn) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	select {
	case <-g.closed:
		_ = conn.Close()
		return false
	default:
	}
	g.active[conn] = struct{}{}
	return true
}

func (g *socksGuard) untrack(conn net.Conn) {
	g.mu.Lock()
	delete(g.active, conn)
	g.mu.Unlock()
	_ = conn.Close()
}

func (g *socksGuard) admitted(conn net.Conn) bool {
	if len(g.allowed) == 0 {
		return true
	}
	addr, ok := conn.RemoteAddr().(*net.TCPAddr)
	if !ok {
		return false
	}
	for _, ip := range g.allowed {
		if ip.Equal(addr.IP) {
			return true
		}
	}
	return false
}

func (g *socksGuard) serve(ctx context.Context) {
	go func() {
		select {
		case <-ctx.Done():
			g.close()
		case <-g.closed:
		}
	}()
	go func() {
		defer close(g.done)
		var workers sync.WaitGroup
		for {
			client, err := g.listener.Accept()
			if err != nil {
				break
			}
			if !g.admitted(client) {
				_ = client.Close()
				continue
			}
			select {
			case g.permits <- struct{}{}:
			default:
				_ = client.Close()
				continue
			}
			if !g.track(client) {
				<-g.permits
				continue
			}
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer func() { <-g.permits }()
				defer g.untrack(client)
				g.forward(client)
			}()
		}
		g.close()
		workers.Wait()
	}()
}

func (g *socksGuard) dialBackend() (net.Conn, error) {
	deadline := time.Now().Add(3 * time.Second)
	for {
		select {
		case <-g.closed:
			return nil, net.ErrClosed
		default:
		}
		backend, err := net.DialTimeout("tcp4", g.backend, 200*time.Millisecond)
		if err == nil {
			return backend, nil
		}
		if time.Now().After(deadline) {
			return nil, err
		}
		time.Sleep(30 * time.Millisecond)
	}
}

func (g *socksGuard) forward(client net.Conn) {
	prefix, blocked, err := screenSOCKS(client)
	if err != nil || blocked {
		return
	}
	backend, err := g.dialBackend()
	if err != nil || !g.track(backend) {
		return
	}
	defer g.untrack(backend)
	if _, err := backend.Write(prefix); err != nil {
		return
	}
	var copyDone sync.WaitGroup
	copyDone.Add(1)
	go func() {
		defer copyDone.Done()
		_, _ = io.Copy(backend, client)
		if tcp, ok := backend.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
	}()
	_, _ = io.Copy(client, backend)
	// Once the backend is done sending, an idle or malicious client must not
	// keep the upload goroutine (and its connection permit) alive forever.
	_ = client.Close()
	copyDone.Wait()
}

// screenSOCKS buffers only the opening bytes, so fragmented SOCKS4/5
// handshakes cannot slip through a first-Read boundary as they can in a
// packet-based filter. Encrypted/obfuscated SOCKS is intentionally outside
// this plaintext signature screen; the fixed target and account policy remain
// separate controls.
func screenSOCKS(conn net.Conn) ([]byte, bool, error) {
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	defer conn.SetReadDeadline(time.Time{})
	prefix := make([]byte, 0, 66)
	read := func(n int) error {
		start := len(prefix)
		prefix = append(prefix, make([]byte, n)...)
		_, err := io.ReadFull(conn, prefix[start:])
		return err
	}
	if err := read(1); err != nil {
		return nil, false, err
	}
	switch prefix[0] {
	case 0x04:
		if err := read(1); err != nil {
			return nil, false, err
		}
		if prefix[1] != 0x01 && prefix[1] != 0x02 {
			return prefix, false, nil
		}
		if err := read(6); err != nil {
			return nil, false, err
		}
		return prefix, prefix[2] != 0 || prefix[3] != 0, nil
	case 0x05:
		if err := read(1); err != nil {
			return nil, false, err
		}
		n := int(prefix[1])
		if n == 0 || n > 16 {
			// A long method list is indistinguishable from a random Mieru
			// opening segment. Preserve game reliability rather than reject
			// a substantial fraction of genuine encrypted connections.
			return prefix, false, nil
		}
		if err := read(n); err != nil {
			return nil, false, err
		}
		for _, method := range prefix[2:] {
			if method == 0x00 || method == 0x01 || method == 0x02 {
				return prefix, true, nil
			}
		}
	}
	return prefix, false, nil
}
