//go:build linux

package relayruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

const v2RecoverySocketName = "recovery.sock"

func recoveryPeerUID(conn *net.UnixConn) (uint32, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var credentials *unix.Ucred
	var socketErr error
	if err = raw.Control(func(fd uintptr) {
		credentials, socketErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if socketErr != nil || credentials == nil {
		return 0, errors.New("cannot establish local recovery peer credentials")
	}
	return credentials.Uid, nil
}

type rootRecoveryListener struct{ *net.UnixListener }

func (l rootRecoveryListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.AcceptUnix()
		if err != nil {
			return nil, err
		}
		uid, err := recoveryPeerUID(conn)
		if err == nil && uid == 0 {
			return conn, nil
		}
		_ = conn.Close()
	}
}

func recoverySocketInfo(path string, owner uint32) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	var metadata unix.Stat_t
	if err := unix.Lstat(path, &metadata); err != nil {
		return nil, err
	}
	if metadata.Mode&unix.S_IFMT != unix.S_IFSOCK || metadata.Mode&07777 != 0600 || metadata.Uid != owner || metadata.Nlink != 1 || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("local recovery endpoint must be an owned mode-0600 Unix socket")
	}
	return info, nil
}

func serveV2Recovery(ctx context.Context, s *runtimeState) (func(), error) {
	if err := checkV2PrivatePath(s.cfg.StateDir, true); err != nil {
		return nil, err
	}
	path := filepath.Join(s.cfg.StateDir, v2RecoverySocketName)
	if previous, err := recoverySocketInfo(path, uint32(os.Geteuid())); err == nil {
		conn, dialErr := net.DialTimeout("unix", path, 200*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			return nil, errors.New("local recovery socket already has a live listener")
		}
		if !errors.Is(dialErr, unix.ECONNREFUSED) {
			return nil, errors.New("cannot prove local recovery socket is stale")
		}
		current, err := os.Lstat(path)
		if err != nil || !os.SameFile(previous, current) {
			return nil, errors.New("local recovery socket changed during stale check")
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	listener.SetUnlinkOnClose(false)
	if err := os.Chmod(path, 0600); err != nil {
		_ = listener.Close()
		return nil, err
	}
	created, err := recoverySocketInfo(path, uint32(os.Geteuid()))
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/snapshot", func(w http.ResponseWriter, r *http.Request) {
		var empty struct{}
		if !decodeLocalRecovery(w, r, &empty) {
			return
		}
		out, err := s.recoverySnapshot()
		writeLocalRecovery(w, out, err)
	})
	mux.HandleFunc("/adopt", func(w http.ResponseWriter, r *http.Request) {
		var plan V2RecoveryPlan
		if !decodeLocalRecovery(w, r, &plan) {
			return
		}
		out, err := s.applyRecoveryPlan(ctx, plan)
		writeLocalRecovery(w, out, err)
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 8 << 10}
	go server.Serve(rootRecoveryListener{listener})
	return func() {
		_ = server.Close()
		if current, err := os.Lstat(path); err == nil && os.SameFile(created, current) {
			_ = os.Remove(path)
		}
	}, nil
}

func decodeLocalRecovery(w http.ResponseWriter, r *http.Request, out any) bool {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return false
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxV2StateBytes+1))
	if err != nil || len(raw) > maxV2StateBytes || strictV2JSON(raw, out) != nil {
		http.Error(w, "invalid bounded local recovery request", http.StatusBadRequest)
		return false
	}
	return true
}

func writeLocalRecovery(w http.ResponseWriter, value any, err error) {
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(value)
}

// systemd DynamicUser may expose /var/lib/name as a root-owned symlink to
// /var/lib/private/name on the host. Permit only that exact systemd layout;
// arbitrary user-controlled symlinks are not a credential transport target.
func resolveRecoveryClientDirectory(dataDir string) (string, uint32, error) {
	path, err := filepath.Abs(dataDir)
	if err != nil || dataDir == "" || path == "/" {
		return "", 0, errors.New("explicit local Agent state directory required")
	}
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		var stat unix.Stat_t
		target, readErr := os.Readlink(path)
		if unix.Lstat(path, &stat) != nil || stat.Uid != 0 || filepath.Dir(path) != "/var/lib" || readErr != nil {
			return "", 0, errors.New("untrusted local Agent state symlink")
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		if filepath.Clean(target) != filepath.Join("/var/lib/private", filepath.Base(path)) {
			return "", 0, errors.New("state link is not the protected systemd DynamicUser layout")
		}
		path = filepath.Clean(target)
	}
	var owner uint32
	for current := path; ; current = filepath.Dir(current) {
		var stat unix.Stat_t
		if err := unix.Lstat(current, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFDIR {
			return "", 0, errors.New("local Agent state path must contain only existing directories")
		}
		if current == path {
			if stat.Mode&07777 != 0700 {
				return "", 0, errors.New("local Agent state directory must be mode 0700")
			}
			owner = stat.Uid
		} else if stat.Uid != 0 || stat.Mode&0022 != 0 && stat.Mode&unix.S_ISVTX == 0 {
			return "", 0, errors.New("local Agent state ancestors must be protected root-owned directories")
		}
		if current == "/" {
			break
		}
	}
	return path, owner, nil
}

func callRecoverySocket(ctx context.Context, dataDir, action string, input any, output any) error {
	path, owner, err := resolveRecoveryClientDirectory(dataDir)
	if err != nil {
		return err
	}
	socketPath := filepath.Join(path, v2RecoverySocketName)
	before, err := recoverySocketInfo(socketPath, owner)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return err
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		dialer := net.Dialer{Timeout: 3 * time.Second}
		conn, err := dialer.DialContext(ctx, "unix", socketPath)
		if err != nil {
			return nil, err
		}
		local, ok := conn.(*net.UnixConn)
		if !ok {
			_ = conn.Close()
			return nil, errors.New("recovery transport is not a Unix socket")
		}
		uid, err := recoveryPeerUID(local)
		after, checkErr := recoverySocketInfo(socketPath, owner)
		if err != nil || uid != owner || checkErr != nil || !os.SameFile(before, after) {
			_ = conn.Close()
			return nil, errors.New("local recovery Agent peer or socket identity changed")
		}
		return conn, nil
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://local.invalid/"+action, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := client.Do(req)
	if err != nil {
		return errors.New("protected local Agent recovery transport unavailable")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxV2StateBytes+1))
	if err != nil || len(data) > maxV2StateBytes {
		return errors.New("invalid bounded local recovery response")
	}
	if response.StatusCode != http.StatusOK {
		var result struct {
			Error string `json:"error"`
		}
		if strictV2JSON(data, &result) == nil && len(result.Error) <= 512 {
			return errors.New(result.Error)
		}
		return errors.New("protected local Agent rejected recovery operation")
	}
	return strictV2JSON(data, output)
}

// RunV2RecoveryClient is intentionally root-only even when the Agent itself
// runs as systemd DynamicUser. No token travels in argv, environment or stdout.
func RunV2RecoveryClient(ctx context.Context, dataDir, action, filePath string) error {
	if os.Geteuid() != 0 {
		return errors.New("local relay recovery requires Linux root")
	}
	if filePath == "" || (action != "snapshot" && action != "adopt") {
		return errors.New("recovery needs snapshot|adopt and a private --recovery-file")
	}
	if err := checkV2PrivatePath(filepath.Dir(filePath), true); err != nil {
		return err
	}
	if action == "snapshot" {
		// Validate the destination before freezing, and never print its contents.
		if err := checkV2PrivatePath(filepath.Dir(filePath), true); err != nil {
			return err
		}
		if err := checkV2PrivatePath(filePath, false); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		var snapshot V2RecoverySnapshot
		if err := callRecoverySocket(ctx, dataDir, action, struct{}{}, &snapshot); err != nil {
			return err
		}
		if err := writeV2PrivateJSON(filePath, snapshot); err != nil {
			return errors.New("Agent is frozen but snapshot output failed; retry the same snapshot destination")
		}
		_, err := fmt.Fprintln(os.Stdout, "受保护恢复快照已保存；原有转发保留，尚未信任新控制面。")
		return err
	}
	var plan V2RecoveryPlan
	if err := readV2PrivateJSON(filePath, &plan); err != nil {
		return errors.New("recovery plan must be an existing root-owned private regular JSON file")
	}
	var result V2RecoveryResult
	if err := callRecoverySocket(ctx, dataDir, action, plan, &result); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}
