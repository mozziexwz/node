package executor

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Relay target verification proves that a relay leads to a real MSBOOST node.
// Through the real relay entry, a short-lived client opens the user's own mieru
// tunnel and completes a hello with the node's loopback TCP probe
// (127.0.0.1:20424). Only MSBOOST nodes run that service, it is reachable only
// through the tunnel, and the reply echoes a fresh nonce, so a mieru reply that
// merely accepts the request proves nothing on its own.
//
// The mieru client is mihomo, run as a separate program (see verify_mihomo.go);
// no mieru code is linked into the executor.

const (
	RelayVerifyCapability = "relay_verify_v1"
	RelayVerifyHeader     = "X-MSBOOST-Relay-Verify"
)

// Result codes. target_down is decided by the panel: for paid routes from the
// exit's own TCP check, for customer relays by the install step, which tests
// the target from the relay server before anything is created.
const (
	VerifyOK               = "ok"
	VerifyEntryUnreachable = "entry_unreachable"
	VerifyTargetDown       = "target_down"
	VerifyTunnelFailed     = "tunnel_failed"
	VerifyNotMSBOOST       = "not_msboost"
	// The executor could not run a check at all (download, clock, sandbox);
	// never counted against the user.
	VerifyUnavailable = "unavailable"
)

// VerifyBlocks reports whether a code means the target is not a usable MSBOOST
// node, as opposed to a temporary problem on the way there.
func VerifyBlocks(code string) bool { return code == VerifyTunnelFailed || code == VerifyNotMSBOOST }

const (
	defaultVerifyGame    = "35.155.204.207:8585"
	defaultVerifyBlocked = "www.youtube.com"
	defaultVerifyAllowed = "www.nexon.com"
	probeAddress         = "127.0.0.1:20424"
)

// VerifyOptions travel with relay, relay-code and relay-verify jobs. Mode
// "enforce" lets a relay-code job undo its relay when the target fails.
type VerifyOptions struct {
	Mode          string `json:"mode"`
	GameTarget    string `json:"gameTarget,omitempty"`
	FilterBlocked string `json:"filterBlocked,omitempty"`
	FilterAllowed string `json:"filterAllowed,omitempty"`
}

// VerifyReport never carries the node identity or any part of the
// configuration; it is stored with tasks, rules and relay records.
type VerifyReport struct {
	Code string `json:"code"`
	// Game: ok, refused, timeout, unreachable, failed or skipped.
	Game string `json:"game,omitempty"`
	// Filter: ok, missing (a site the node should refuse was reachable),
	// inconclusive (the comparison site failed too) or skipped.
	Filter    string `json:"filter,omitempty"`
	ElapsedMS int64  `json:"elapsedMs"`
}

var verifyHostPattern = regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)

func ValidateVerifyOptions(o *VerifyOptions) error {
	if o == nil {
		return nil
	}
	if o.Mode != "log" && o.Mode != "enforce" {
		return errors.New("校验模式无效")
	}
	if o.GameTarget != "" {
		if _, err := verifyGameTarget(o.GameTarget); err != nil {
			return err
		}
	}
	for _, host := range []string{o.FilterBlocked, o.FilterAllowed} {
		if host != "" && !verifyHostPattern.MatchString(host) {
			return errors.New("过滤检查域名无效")
		}
	}
	return nil
}

func verifyGameTarget(value string) (netip.AddrPort, error) {
	target, err := netip.ParseAddrPort(value)
	if err != nil || !target.Addr().Is4() || target.Port() == 0 || PublicIP(target.Addr().String()) != nil {
		return netip.AddrPort{}, errors.New("游戏检测目标必须是公网 IPv4 地址和端口")
	}
	return target, nil
}

// TunnelProfile is the part of a customer configuration a tunnel needs.
type TunnelProfile struct {
	Host     string
	Port     int
	Username string
	Password string
	// TrafficPattern is mieru's base64 protobuf form, or empty.
	TrafficPattern string
}

// Tunnel opens connections at the far end of one mieru tunnel.
type Tunnel interface {
	DialContext(ctx context.Context, address string) (net.Conn, error)
	Close() error
}

// TunnelOpener starts a tunnel client for one profile.
type TunnelOpener func(ctx context.Context, p TunnelProfile) (Tunnel, error)

// Verifier checks one relay entry. Zero values use the production defaults.
type Verifier struct {
	Open TunnelOpener
	// Reach dials the relay entry directly, only to see that it accepts TCP.
	Reach func(ctx context.Context, address string) (net.Conn, error)
	// Roots verify the comparison sites' certificates; nil uses the system pool.
	Roots *x509.CertPool
	Now   func() time.Time
}

// Verify never returns an error: every outcome is a report code.
func (v *Verifier) Verify(ctx context.Context, config []byte, opts VerifyOptions) VerifyReport {
	now := v.Now
	if now == nil {
		now = time.Now
	}
	started := now()
	done := func(r VerifyReport) VerifyReport {
		r.ElapsedMS = max(0, now().Sub(started).Milliseconds())
		return r
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	info, err := ParseClientConfig(config)
	if err != nil {
		return done(VerifyReport{Code: VerifyUnavailable})
	}
	profile := TunnelProfile{Host: info.TargetHost, Port: info.TargetPort, Username: info.Username, Password: info.Password}
	if p, ok := info.Document["profiles"].([]any)[0].(map[string]any)["trafficPattern"]; ok && p != nil {
		// A pattern this executor cannot encode is left out: receivers accept
		// any pattern, so the check still proves where the tunnel leads.
		if encoded, err := EncodeTrafficPattern(p); err == nil {
			profile.TrafficPattern = encoded
		}
	}
	entry := net.JoinHostPort(profile.Host, strconv.Itoa(profile.Port))
	reach := v.Reach
	if reach == nil {
		reach = func(ctx context.Context, address string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", address)
		}
	}
	reachCtx, cancelReach := context.WithTimeout(ctx, 7*time.Second)
	conn, err := reach(reachCtx, entry)
	cancelReach()
	if err != nil {
		return done(VerifyReport{Code: VerifyEntryUnreachable})
	}
	_ = conn.Close()
	open := v.Open
	if open == nil {
		open = defaultMihomo.Open
	}
	tunnel, err := open(ctx, profile)
	if err != nil {
		return done(VerifyReport{Code: VerifyUnavailable})
	}
	defer tunnel.Close()

	dialCtx, cancelDial := context.WithTimeout(ctx, 15*time.Second)
	probe, err := tunnel.DialContext(dialCtx, probeAddress)
	cancelDial()
	if err != nil {
		return done(VerifyReport{Code: VerifyTunnelFailed})
	}
	defer probe.Close()
	reader := bufio.NewReaderSize(probe, 4100)
	if err := probeHello(probe, reader, now().Add(8*time.Second)); err != nil {
		return done(VerifyReport{Code: VerifyNotMSBOOST})
	}
	report := VerifyReport{Code: VerifyOK, Game: "skipped", Filter: "skipped"}
	game := opts.GameTarget
	if game == "" {
		game = defaultVerifyGame
	}
	if target, err := verifyGameTarget(game); err == nil {
		report.Game = probeConnect(probe, reader, target, now().Add(10*time.Second))
	}
	_ = probe.Close()
	report.Filter = v.filterCheck(ctx, tunnel, opts)
	return done(report)
}

func (v *Verifier) filterCheck(ctx context.Context, tunnel Tunnel, opts VerifyOptions) string {
	blocked, allowed := opts.FilterBlocked, opts.FilterAllowed
	if blocked == "" {
		blocked = defaultVerifyBlocked
	}
	if allowed == "" {
		allowed = defaultVerifyAllowed
	}
	// The allowed site is the control: if it fails too, the network or the
	// site is at fault and nothing is learned about the node's filter.
	if !v.handshake(ctx, tunnel, allowed) {
		return "inconclusive"
	}
	if v.handshake(ctx, tunnel, blocked) {
		return "missing"
	}
	return "ok"
}

func (v *Verifier) handshake(ctx context.Context, tunnel Tunnel, host string) bool {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := tunnel.DialContext(ctx, net.JoinHostPort(host, "443"))
	if err != nil {
		return false
	}
	defer conn.Close()
	client := tls.Client(conn, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12, RootCAs: v.Roots})
	if deadline, ok := ctx.Deadline(); ok {
		_ = client.SetDeadline(deadline)
	}
	return client.HandshakeContext(ctx) == nil
}

var probeNodeID = regexp.MustCompile(`^[0-9a-f]{32}$`)

// probeHello sends a hello and checks the reply field by field, the same way
// the node installer's self-test does: exactly these fields, the same nonce.
func probeHello(conn net.Conn, reader *bufio.Reader, deadline time.Time) error {
	nonce := randomHex(16)
	if err := probeSend(conn, map[string]any{"version": 2, "operation": "hello", "nonce": nonce}, deadline); err != nil {
		return err
	}
	reply, err := probeReply(conn, reader, deadline)
	if err != nil {
		return err
	}
	if len(reply) != 5 || reply["version"] != json.Number("2") || reply["operation"] != "hello" || reply["nonce"] != nonce || reply["success"] != true {
		return errors.New("hello reply fields mismatch")
	}
	if id, ok := reply["node_id"].(string); !ok || !probeNodeID.MatchString(id) {
		return errors.New("hello reply node id invalid")
	}
	return nil
}

// probeConnect asks the node to open a TCP connection to the game target.
func probeConnect(conn net.Conn, reader *bufio.Reader, target netip.AddrPort, deadline time.Time) string {
	nonce := randomHex(16)
	request := map[string]any{"version": 2, "operation": "connect", "nonce": nonce, "target_address": target.Addr().String(), "target_port": int(target.Port())}
	if err := probeSend(conn, request, deadline); err != nil {
		return "failed"
	}
	reply, err := probeReply(conn, reader, deadline)
	if err != nil || reply["version"] != json.Number("2") || reply["operation"] != "connect" || reply["nonce"] != nonce {
		return "failed"
	}
	if reply["success"] == true {
		return "ok"
	}
	switch reply["error"] {
	case "refused", "timeout", "unreachable":
		return reply["error"].(string)
	}
	return "failed"
}

func probeSend(conn net.Conn, request map[string]any, deadline time.Time) error {
	payload, err := json.Marshal(request)
	if err != nil {
		return err
	}
	_ = conn.SetWriteDeadline(deadline)
	_, err = conn.Write(append(payload, '\n'))
	return err
}

// probeReply reads one line of at most 4096 bytes holding one flat JSON
// object with unique keys and scalar values.
func probeReply(conn net.Conn, reader *bufio.Reader, deadline time.Time) (map[string]any, error) {
	_ = conn.SetReadDeadline(deadline)
	line, err := reader.ReadSlice('\n')
	if err != nil {
		return nil, err
	}
	if len(line) > 4097 {
		return nil, errors.New("probe reply too long")
	}
	return parseFlatJSON(bytes.TrimSuffix(line, []byte("\n")))
}

func parseFlatJSON(data []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, errors.New("probe reply is not an object")
	}
	out := map[string]any{}
	for decoder.More() {
		key, err := decoder.Token()
		name, ok := key.(string)
		if err != nil || !ok {
			return nil, errors.New("probe reply key invalid")
		}
		if _, seen := out[name]; seen {
			return nil, errors.New("probe reply repeats a field")
		}
		value, err := decoder.Token()
		if err != nil {
			return nil, errors.New("probe reply value invalid")
		}
		switch value.(type) {
		case string, bool, json.Number:
		default:
			return nil, errors.New("probe reply value is not a scalar")
		}
		out[name] = value
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, errors.New("probe reply is not closed")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errors.New("probe reply has trailing data")
	}
	return out, nil
}

// socksConnect opens address through a local SOCKS5 proxy without
// authentication. Domain names are resolved at the far end of the tunnel.
func socksConnect(ctx context.Context, proxy, address string) (net.Conn, error) {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return nil, errors.New("invalid port")
	}
	request := []byte{5, 1, 0}
	if ip, err := netip.ParseAddr(host); err == nil && ip.Is4() {
		raw := ip.As4()
		request = append(append(request, 1), raw[:]...)
	} else if err == nil {
		raw := ip.As16()
		request = append(append(request, 4), raw[:]...)
	} else if len(host) > 0 && len(host) < 256 {
		request = append(append(request, 3, byte(len(host))), host...)
	} else {
		return nil, errors.New("invalid host")
	}
	request = binary.BigEndian.AppendUint16(request, uint16(port))
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", proxy)
	if err != nil {
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	fail := func(err error) (net.Conn, error) {
		_ = conn.Close()
		return nil, err
	}
	if _, err := conn.Write([]byte{5, 1, 0}); err != nil {
		return fail(err)
	}
	greeting := make([]byte, 2)
	if _, err := io.ReadFull(conn, greeting); err != nil || greeting[0] != 5 || greeting[1] != 0 {
		return fail(errors.New("socks greeting refused"))
	}
	if _, err := conn.Write(request); err != nil {
		return fail(err)
	}
	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		return fail(err)
	}
	if header[0] != 5 || header[1] != 0 {
		return fail(fmt.Errorf("socks connect refused (%d)", header[1]))
	}
	var skip int
	switch header[3] {
	case 1:
		skip = 4
	case 4:
		skip = 16
	case 3:
		length := make([]byte, 1)
		if _, err := io.ReadFull(conn, length); err != nil {
			return fail(err)
		}
		skip = int(length[0])
	default:
		return fail(errors.New("socks bound address invalid"))
	}
	if _, err := io.ReadFull(conn, make([]byte, skip+2)); err != nil {
		return fail(err)
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

// trafficPatternFields maps mieru's TrafficPattern JSON (protojson) onto its
// protobuf field numbers; see mieru's pkg/appctl/proto/base.proto.
type protoField struct {
	number int
	kind   string // int32, bool, enum, message, strings
	enum   map[string]int
	fields map[string]protoField
}

var lowEntropyRotations = func() map[string]int {
	out := map[string]int{"LOW_ENTROPY_MASK_NO_ROTATION": 0}
	for n := 1; n <= 15; n++ {
		out["LOW_ENTROPY_MASK_ROTATE_RIGHT_"+strconv.Itoa(n)] = n
		out["LOW_ENTROPY_MASK_ROTATE_LEFT_"+strconv.Itoa(n)] = n * 16
	}
	return out
}()

var trafficPatternFields = map[string]protoField{
	"seed":      {number: 1, kind: "int32"},
	"unlockAll": {number: 2, kind: "bool"},
	"tcpFragment": {number: 3, kind: "message", fields: map[string]protoField{
		"enable":     {number: 1, kind: "bool"},
		"maxSleepMs": {number: 2, kind: "int32"},
	}},
	"nonce": {number: 4, kind: "message", fields: map[string]protoField{
		"type":                {number: 1, kind: "enum", enum: map[string]int{"NONCE_TYPE_RANDOM": 0, "NONCE_TYPE_PRINTABLE": 1, "NONCE_TYPE_PRINTABLE_SUBSET": 2, "NONCE_TYPE_FIXED": 3}},
		"applyToAllUDPPacket": {number: 2, kind: "bool"},
		"minLen":              {number: 3, kind: "int32"},
		"maxLen":              {number: 4, kind: "int32"},
		"customHexStrings":    {number: 5, kind: "strings"},
	}},
	"padding": {number: 5, kind: "message", fields: map[string]protoField{
		"maxMiddlePaddingLen": {number: 1, kind: "int32"},
		"maxEndPaddingLen":    {number: 2, kind: "int32"},
	}},
	"lowEntropy": {number: 6, kind: "message", fields: map[string]protoField{
		"mode":         {number: 1, kind: "enum", enum: map[string]int{"LOW_ENTROPY_MODE_OFF": 0, "LOW_ENTROPY_MODE_32": 1, "LOW_ENTROPY_MODE_40": 2, "LOW_ENTROPY_MODE_48": 3, "LOW_ENTROPY_MODE_56": 4}},
		"maskRotation": {number: 2, kind: "enum", enum: lowEntropyRotations},
	}},
}

// EncodeTrafficPattern turns a profile's trafficPattern object into the base64
// protobuf string mihomo's traffic-pattern option takes. Every set field is
// encoded, zero values included, because mieru treats presence as meaningful.
func EncodeTrafficPattern(value any) (string, error) {
	data, err := encodeProtoMessage(value, trafficPatternFields)
	if err != nil {
		return "", err
	}
	if len(data) == 0 {
		return "", nil
	}
	return base64Std(data), nil
}

func encodeProtoMessage(value any, fields map[string]protoField) ([]byte, error) {
	object, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("traffic pattern is not an object")
	}
	for name := range object {
		if _, known := fields[name]; !known {
			return nil, fmt.Errorf("traffic pattern field %q is not supported", name)
		}
	}
	var out []byte
	// Canonical order: ascending field numbers.
	for number := 1; number <= len(fields); number++ {
		for name, field := range fields {
			if field.number != number {
				continue
			}
			raw, present := object[name]
			if !present || raw == nil {
				continue
			}
			encoded, err := encodeProtoField(field, raw)
			if err != nil {
				return nil, fmt.Errorf("traffic pattern field %q: %w", name, err)
			}
			out = append(out, encoded...)
		}
	}
	return out, nil
}

func encodeProtoField(field protoField, raw any) ([]byte, error) {
	varint := func(value uint64) []byte {
		out := binary.AppendUvarint(nil, uint64(field.number)<<3)
		return binary.AppendUvarint(out, value)
	}
	bytesField := func(data []byte) []byte {
		out := binary.AppendUvarint(nil, uint64(field.number)<<3|2)
		out = binary.AppendUvarint(out, uint64(len(data)))
		return append(out, data...)
	}
	switch field.kind {
	case "bool":
		b, ok := raw.(bool)
		if !ok {
			return nil, errors.New("not a boolean")
		}
		if b {
			return varint(1), nil
		}
		return varint(0), nil
	case "int32":
		n, err := protoInt32(raw)
		if err != nil {
			return nil, err
		}
		// Negative int32 values are sign-extended to 64 bits on the wire.
		return varint(uint64(int64(n))), nil
	case "enum":
		switch v := raw.(type) {
		case string:
			n, ok := field.enum[v]
			if !ok {
				return nil, fmt.Errorf("unknown value %q", v)
			}
			return varint(uint64(n)), nil
		default:
			n, err := protoInt32(raw)
			if err != nil {
				return nil, err
			}
			for _, known := range field.enum {
				if known == int(n) {
					return varint(uint64(int64(n))), nil
				}
			}
			return nil, fmt.Errorf("unknown value %d", n)
		}
	case "strings":
		list, ok := raw.([]any)
		if !ok {
			return nil, errors.New("not a list")
		}
		var out []byte
		for _, item := range list {
			text, ok := item.(string)
			if !ok {
				return nil, errors.New("not a string")
			}
			out = append(out, bytesField([]byte(text))...)
		}
		return out, nil
	case "message":
		data, err := encodeProtoMessage(raw, field.fields)
		if err != nil {
			return nil, err
		}
		return bytesField(data), nil
	}
	return nil, errors.New("unsupported field kind")
}

// protoInt32 accepts a JSON number or a decimal string, as protojson does.
func protoInt32(raw any) (int32, error) {
	var n int64
	switch v := raw.(type) {
	case float64:
		if v != float64(int64(v)) {
			return 0, errors.New("not an integer")
		}
		n = int64(v)
	case json.Number:
		parsed, err := strconv.ParseInt(string(v), 10, 64)
		if err != nil {
			return 0, errors.New("not an integer")
		}
		n = parsed
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			return 0, errors.New("not an integer")
		}
		n = parsed
	default:
		return 0, errors.New("not a number")
	}
	if n < -1<<31 || n > 1<<31-1 {
		return 0, errors.New("out of int32 range")
	}
	return int32(n), nil
}

var verifyCodes = map[string]bool{VerifyOK: true, VerifyEntryUnreachable: true, VerifyTargetDown: true, VerifyTunnelFailed: true, VerifyNotMSBOOST: true, VerifyUnavailable: true}

// ValidVerifyCode reports whether code belongs to the closed vocabulary.
func ValidVerifyCode(code string) bool { return verifyCodes[code] }

// cleanVerified keeps a relay record's verification only for panels that asked
// for it, and only in its known form.
func cleanVerified(v *RelayVerified, report bool) *RelayVerified {
	if !report || v == nil || !verifyCodes[v.Code] {
		return nil
	}
	at, err := time.Parse(time.RFC3339, v.At)
	if err != nil {
		return nil
	}
	return &RelayVerified{Code: v.Code, At: at.UTC().Format(time.RFC3339)}
}
