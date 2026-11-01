package relayruntime

import (
	"context"
	"errors"
	"log"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

const (
	udpGuardSessions = 4096
	udpGuardIdle     = 90 * time.Second
	// Mieru keeps its datagrams under the 1400-byte MTU of the configuration.
	udpGuardDatagram = 8192
)

var udpGuardRetry = 5 * time.Second

// udpGuard owns a rule's public UDP port the way socksGuard owns its TCP port:
// GOST receives datagrams only on a private loopback port, from this guard.
// Each client address gets its own loopback socket, so GOST keeps one
// forwarding channel per client. It never screens content and never fails the
// rule: while the port cannot be bound it keeps retrying, and TCP forwarding
// is unaffected.
type udpGuard struct {
	port     int
	backend  *net.UDPAddr
	allowed  []net.IP
	mu       sync.Mutex
	conn     *net.UDPConn
	sessions map[netip.AddrPort]*udpSession
	closed   chan struct{}
	done     chan struct{}
	once     sync.Once
}

type udpSession struct {
	conn *net.UDPConn
	seen atomic.Int64 // UnixNano of the last datagram either way
}

func reserveUDPBackend(publicPort int) (*net.UDPConn, error) {
	if publicPort < 1 || publicPort > 65535 {
		return nil, errors.New("invalid relay port")
	}
	for i := 0; i < 16; i++ {
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			return nil, err
		}
		if conn.LocalAddr().(*net.UDPAddr).Port != publicPort {
			return conn, nil
		}
		_ = conn.Close()
	}
	return nil, errors.New("cannot reserve a private relay UDP port")
}

func newUDPGuard(rule Rule, backend string) (*udpGuard, error) {
	allowed, err := parseGuardSources(rule.AllowedSources)
	if err != nil {
		return nil, err
	}
	addr, err := net.ResolveUDPAddr("udp4", backend)
	if err != nil || addr.IP == nil || !addr.IP.IsLoopback() {
		return nil, errors.New("invalid private relay UDP port")
	}
	return &udpGuard{port: rule.ListenPort, backend: addr, allowed: allowed, sessions: map[netip.AddrPort]*udpSession{}, closed: make(chan struct{}), done: make(chan struct{})}, nil
}

func (g *udpGuard) close() {
	g.once.Do(func() {
		close(g.closed)
		g.mu.Lock()
		if g.conn != nil {
			_ = g.conn.Close()
		}
		for client, session := range g.sessions {
			_ = session.conn.Close()
			delete(g.sessions, client)
		}
		g.mu.Unlock()
	})
}

func (g *udpGuard) wait() { <-g.done }

func (g *udpGuard) admitted(addr netip.Addr) bool {
	if len(g.allowed) == 0 {
		return true
	}
	ip := net.IP(addr.Unmap().AsSlice())
	for _, allowed := range g.allowed {
		if allowed.Equal(ip) {
			return true
		}
	}
	return false
}

func (g *udpGuard) serve(ctx context.Context) {
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
		defer workers.Wait()
		conn := g.bind()
		if conn == nil {
			return
		}
		go g.expire()
		buffer := make([]byte, udpGuardDatagram)
		for {
			n, client, err := conn.ReadFromUDPAddrPort(buffer)
			if err != nil {
				select {
				case <-g.closed:
					return
				case <-time.After(10 * time.Millisecond):
				}
				continue
			}
			if !g.admitted(client.Addr()) {
				continue
			}
			if session := g.session(conn, client, &workers); session != nil {
				session.seen.Store(time.Now().UnixNano())
				_, _ = session.conn.Write(buffer[:n])
			}
		}
	}()
}

// bind retries while another socket holds the port, e.g. an outgoing UDP
// socket that was given it as an ephemeral port.
func (g *udpGuard) bind() *net.UDPConn {
	for logged := false; ; {
		conn, err := net.ListenUDP("udp", &net.UDPAddr{Port: g.port})
		if err == nil {
			g.mu.Lock()
			defer g.mu.Unlock()
			select {
			case <-g.closed:
				_ = conn.Close()
				return nil
			default:
			}
			g.conn = conn
			return conn
		}
		if !logged {
			log.Printf("relay UDP port %d unavailable, retrying: %v", g.port, err)
			logged = true
		}
		select {
		case <-g.closed:
			return nil
		case <-time.After(udpGuardRetry):
		}
	}
}

func (g *udpGuard) session(public *net.UDPConn, client netip.AddrPort, workers *sync.WaitGroup) *udpSession {
	g.mu.Lock()
	defer g.mu.Unlock()
	if session := g.sessions[client]; session != nil {
		return session
	}
	select {
	case <-g.closed:
		return nil
	default:
	}
	if len(g.sessions) >= udpGuardSessions {
		return nil
	}
	conn, err := net.DialUDP("udp4", nil, g.backend)
	if err != nil {
		return nil
	}
	session := &udpSession{conn: conn}
	session.seen.Store(time.Now().UnixNano())
	g.sessions[client] = session
	workers.Add(1)
	go func() {
		defer workers.Done()
		defer g.drop(client, session)
		buffer := make([]byte, udpGuardDatagram)
		for {
			// A refused datagram (GOST restarting) also ends the session; the
			// client's next datagram opens a new one.
			n, err := conn.Read(buffer)
			if err != nil {
				return
			}
			session.seen.Store(time.Now().UnixNano())
			_, _ = public.WriteToUDPAddrPort(buffer[:n], client)
		}
	}()
	return session
}

func (g *udpGuard) drop(client netip.AddrPort, session *udpSession) {
	_ = session.conn.Close()
	g.mu.Lock()
	if g.sessions[client] == session {
		delete(g.sessions, client)
	}
	g.mu.Unlock()
}

func (g *udpGuard) expire() {
	ticker := time.NewTicker(udpGuardIdle / 6)
	defer ticker.Stop()
	for {
		select {
		case <-g.closed:
			return
		case now := <-ticker.C:
			g.sweep(now)
		}
	}
}

// sweep closes the sessions idle since before now-udpGuardIdle; their reply
// goroutines then end.
func (g *udpGuard) sweep(now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for client, session := range g.sessions {
		if now.UnixNano()-session.seen.Load() > int64(udpGuardIdle) {
			_ = session.conn.Close()
			delete(g.sessions, client)
		}
	}
}
