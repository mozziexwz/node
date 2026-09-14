//go:build linux && msboost_joint_test

package control

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mozziexwz/node/internal/disaster"
	"github.com/mozziexwz/node/internal/relayruntime"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// Opt-in child for deploy/disaster_joint_validation.sh. No production binary
// includes this fixture. Its control protocol is real HTTP/PG; only the echo
// target and transport-only local HTTP proxy are fixtures.
func TestDisasterJointDataPlane(t *testing.T) {
	if os.Getenv("MSBOOST_JOINT_FIXTURE") != "data-plane" {
		t.Skip("isolated full-archive/GOST joint harness only")
	}
	if os.Geteuid() != 0 || os.Getenv("DATA_DIR") != "/joint/runtime" || os.Getenv("DATABASE_HOST") != "database" || os.Getenv("DATABASE_NAME") != "msboost" || os.Getenv("DATABASE_USER") != "msboost" || os.Getenv("DATABASE_SSLMODE") != "disable" {
		t.Fatal("refusing non-isolated data plane")
	}
	owner, err := os.ReadFile("/joint/owner")
	if err != nil || !regexp.MustCompile(`^msboost-joint-[a-z0-9]{8}\n$`).Match(owner) {
		t.Fatal("missing isolated fixture identity")
	}
	gost := "/fixture/gost"
	if err := os.Mkdir("/joint/runtime", 0700); err != nil {
		t.Fatal("runtime must start in a new private directory")
	}
	u := url.URL{Scheme: "postgres", Host: "database:5432", Path: "/msboost", User: url.UserPassword("msboost", os.Getenv("POSTGRES_PASSWORD")), RawQuery: "sslmode=disable&connect_timeout=5&statement_timeout=5000"}
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal("cannot open isolated database")
	}
	defer db.Close()
	store := &Store{db: db, dialect: "postgres"}
	// A checked random project owns this fresh database. Refuse reusing a
	// fixture or any database which already has relay/business inventory.
	if err := store.View(func(s *State) error {
		if len(s.Users) != 1 || len(s.Docs["relay_agents"]) != 0 || len(s.Docs["user_rules"]) != 0 || len(s.Docs["joint_fixture"]) != 0 {
			return errors.New("nonempty fixture database")
		}
		return nil
	}); err != nil {
		t.Fatal("database is not a fresh isolated site")
	}
	echo, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	probe, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	probe.Close()
	f := newRelayV2Fixture(t)
	enrollment := commerceID() + commerceID()
	if err := f.app.Store.Update(func(s *State) error {
		DeleteDoc(s, "relay_agents", f.agents[1].ID)
		a := f.agents[0]
		a.TokenHash, a.EnrollmentHash = "", commerceHash(enrollment)
		a.EnrollmentExpires = time.Now().Add(20 * time.Minute).UnixMilli()
		a.PortRanges = []PortRange{{Start: port, End: port}}
		if err := SaveDoc(s, "relay_agents", a.ID, a); err != nil {
			return err
		}
		if err := SaveDoc(s, "routes", "route", Route{ID: "route", Type: "port_forward", EntryAgentID: a.ID, Enabled: true, RateMbps: 5}); err != nil {
			return err
		}
		rule, _ := LoadDoc[UserRule](s, "user_rules", f.user.ID+":route")
		rule.Segments = rule.Segments[:1]
		rule.Segments[0].Runtime.ListenPort = port
		rule.Segments[0].Runtime.Targets = []string{echo.Addr().String()}
		return SaveDoc(s, "user_rules", f.user.ID+":route", rule)
	}); err != nil {
		t.Fatal(err)
	}
	var seed State
	if err := f.app.Store.View(func(s *State) error {
		raw, e := json.Marshal(s)
		if e != nil {
			return e
		}
		return json.Unmarshal(raw, &seed)
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(func(s *State) error {
		*s = seed
		return SaveDoc(s, "joint_fixture", "owner", strings.TrimSpace(string(owner)))
	}); err != nil {
		t.Fatal("cannot seed isolated relay inventory")
	}
	target, _ := url.Parse("http://caddy:80")
	proxy := httputil.NewSingleHostReverseProxy(target)
	direct := proxy.Director
	proxy.Director = func(r *http.Request) { direct(r); r.Host = "8.8.8.8" }
	proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) { w.WriteHeader(http.StatusBadGateway) }
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: proxy, ReadHeaderTimeout: 5 * time.Second}
	go server.Serve(listener)
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- relayruntime.Run(ctx, relayruntime.Config{ServerURL: "http://" + listener.Addr().String(), EnrollmentToken: enrollment, StateDir: "/joint/runtime", GostBinary: gost, OfflinePolicy: relayruntime.KeepLast})
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("data plane did not reap its child")
		}
	}()
	readCommand := func() (RelayV2Command, bool) {
		var command RelayV2Command
		ready := false
		if err := store.View(func(s *State) error {
			command, _ = LoadDoc[RelayV2Command](s, "relay_v2_commands", f.agents[0].ID+":"+f.ruleID)
			ready = backupPausePreflight(s, time.Now().UnixMilli()).CanPauseControl
			return nil
		}); err != nil {
			t.Fatal("cannot inspect isolated command")
		}
		return command, ready
	}
	var before RelayV2Command
	deadline := time.Now().Add(90 * time.Second)
	for {
		command, ready := readCommand()
		if ready && command.AckState == "ready" && command.Action == "upsert" {
			before = command
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("actual enrollment/ACK did not become backup eligible")
		}
		time.Sleep(200 * time.Millisecond)
	}
	original, err := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
	if err != nil {
		t.Fatal("initial GOST connection failed")
	}
	defer original.Close()
	identity := jointGostIdentity(t, gost)
	phase, acknowledged := "ready", ""
	sequence, phaseStarted := 0, time.Now().UnixMilli()
	deadline = time.Now().Add(25 * time.Minute)
	nextReport := time.Time{}
	for time.Now().Before(deadline) {
		sequence++
		payload := fmt.Sprintf("joint-original-%09d\n", sequence)
		_ = original.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := io.WriteString(original, payload); err != nil {
			t.Fatal("original TCP write failed; no reconnect attempted")
		}
		response := make([]byte, len(payload))
		if _, err := io.ReadFull(original, response); err != nil || string(response) != payload {
			t.Fatal("original TCP echo/sequence failed; no reconnect attempted")
		}
		if time.Now().After(nextReport) {
			if jointGostIdentity(t, gost) != identity {
				t.Fatal("GOST process identity changed")
			}
			if raw, err := os.ReadFile("/joint/phase"); err == nil {
				requested := strings.TrimSpace(string(raw))
				if !regexp.MustCompile(`^(manual|timer|dumpfail|packfail|uploadfail)-(running|complete)$|^finish$`).MatchString(requested) {
					t.Fatal("invalid fixture phase")
				}
				if requested != phase {
					phase = requested
					phaseStarted = time.Now().UnixMilli()
				}
			}
			current, ready := readCommand()
			if !backupPauseSameIntent(before, current) {
				t.Fatal("backup changed original v2 intent")
			}
			canReport := !strings.HasSuffix(phase, "-complete") || ready && current.AppliedAt >= phaseStarted
			if canReport {
				report := map[string]any{"phase": phase, "sequence": sequence, "gostIdentity": identity, "commandID": before.CommandID, "runtimeHash": before.RuntimeHash, "generation": before.Generation, "originalDials": 1, "originalReconnects": 0}
				jointWriteJSON(t, "/joint/report.json", report)
				if phase != acknowledged {
					t.Logf("JOINT_DATA_PHASE=%s samples=%d original_dials=1 original_reconnects=0 gost_unchanged=true intent_unchanged=true", phase, sequence)
					acknowledged = phase
				}
			}
			if phase == "finish" {
				return
			}
			nextReport = time.Now().Add(time.Second)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("joint controller exceeded the bounded lifetime")
}

func jointGostIdentity(t *testing.T, expected string) string {
	t.Helper()
	paths, _ := filepath.Glob("/proc/[0-9]*/exe")
	identity := ""
	for _, path := range paths {
		resolved, err := os.Readlink(path)
		if err != nil || resolved != expected {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(filepath.Dir(path), "stat"))
		fields := strings.Fields(string(raw)[strings.LastIndex(string(raw), ")")+1:])
		if err != nil || len(fields) < 20 || identity != "" {
			t.Fatal("GOST process identity missing or ambiguous")
		}
		identity = filepath.Base(filepath.Dir(path)) + ":" + fields[19]
	}
	if identity == "" {
		t.Fatal("real GOST process not found")
	}
	return identity
}

func jointWriteJSON(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal("cannot encode fixture report")
	}
	if err = os.WriteFile(path+".new", raw, 0600); err != nil {
		t.Fatal("cannot stage fixture report")
	}
	if err = os.Rename(path+".new", path); err != nil {
		t.Fatal("cannot publish fixture report")
	}
}

// Validate the actual encrypted export with its original key, restoring only
// to a new private SQLite directory. Never connect this verification output
// to the live Compose service or replay the diagnostic raw SQL dump.
func TestDisasterJointEncryptedArchive(t *testing.T) {
	if os.Getenv("MSBOOST_JOINT_FIXTURE") != "verify-export" {
		t.Skip("isolated joint encrypted-export verification only")
	}
	work, bundle := os.Getenv("MSBOOST_JOINT_WORK"), os.Getenv("MSBOOST_JOINT_BUNDLE")
	if os.Geteuid() != 0 || !regexp.MustCompile(`^/root/msboost-disaster-joint\.[A-Za-z0-9]{8}$`).MatchString(work) || !strings.HasPrefix(bundle, work+"/verified-") || filepath.Clean(bundle) != bundle {
		t.Fatal("invalid isolated archive path")
	}
	raw, err := os.ReadFile(filepath.Join(bundle, "site.env"))
	if err != nil {
		t.Fatal("cannot read original archive environment")
	}
	key := ""
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "MASTER_KEY=") {
			if key != "" {
				t.Fatal("duplicate original key field")
			}
			key = strings.TrimPrefix(line, "MASTER_KEY=")
		}
	}
	if !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(key) {
		t.Fatal("invalid original key")
	}
	out := filepath.Join(bundle, "isolated-decryption-check")
	wrongKey := strings.Repeat("0", 64)
	if wrongKey == key {
		wrongKey = strings.Repeat("1", 64)
	}
	wrongOut := filepath.Join(bundle, "wrong-key-must-not-exist")
	if _, err := OfflineRestore(OfflineRestoreOptions{BackupPath: filepath.Join(bundle, "state.msb"), MasterKey: wrongKey, SQLiteOutputDir: wrongOut}); err == nil {
		t.Fatal("actual encrypted export accepted a wrong key")
	}
	if _, err := os.Lstat(wrongOut); !os.IsNotExist(err) {
		t.Fatal("wrong-key restore created an output")
	}
	result, err := OfflineRestore(OfflineRestoreOptions{BackupPath: filepath.Join(bundle, "state.msb"), MasterKey: key, SQLiteOutputDir: out})
	if err != nil {
		t.Fatal("actual encrypted export failed isolated restore with original key")
	}
	if !result.Maintenance || !result.RecoveryRequired {
		t.Fatal("isolated decrypted state omitted recovery protections")
	}
	store, err := openStore(Config{DataDir: out})
	if err != nil {
		t.Fatal("cannot inspect isolated decrypted snapshot")
	}
	defer store.Close()
	var original struct {
		CommandID   string `json:"commandID"`
		RuntimeHash string `json:"runtimeHash"`
		Generation  int64  `json:"generation"`
	}
	proof, err := os.ReadFile(filepath.Join(work, "ready.json"))
	if err != nil || json.Unmarshal(proof, &original) != nil || original.CommandID == "" || original.RuntimeHash == "" || original.Generation < 1 {
		t.Fatal("missing original data-plane command proof")
	}
	if err := store.View(func(s *State) error {
		if s.Users["buyer"] == nil || len(s.Docs["joint_fixture"]) != 1 || len(s.Docs["relay_v2_commands"]) != 1 || len(s.Docs["relay_v2_catalogs"]) != 1 || len(s.Docs["backup_pause"]) != 0 {
			return errors.New("isolated snapshot proof missing or runtime gate retained")
		}
		for _, command := range ListDocs[RelayV2Command](s, "relay_v2_commands") {
			if command.CommandID != original.CommandID || command.RuntimeHash != original.RuntimeHash || command.Generation != original.Generation {
				return errors.New("snapshot does not contain the original data-plane command")
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	t.Log("JOINT_ENCRYPTED_EXPORT_VERIFIED original_key=true synthetic_inventory=true isolated_restore_only=true")
}

// The public-looking address exists ONLY on lo in a fresh network namespace,
// with no external interfaces/routes. This satisfies production PublicIP
// validation without contacting any public host or weakening that validation.
func TestDisasterJointReadOnlySFTP(t *testing.T) {
	if os.Getenv("MSBOOST_JOINT_FIXTURE") != "readonly-sftp" {
		t.Skip("isolated joint upload-failure child only")
	}
	interfaces, err := net.Interfaces()
	work := os.Getenv("MSBOOST_JOINT_WORK")
	if os.Geteuid() != 0 || err != nil || len(interfaces) != 1 || interfaces[0].Name != "lo" || !regexp.MustCompile(`^/root/msboost-disaster-joint\.[A-Za-z0-9]{8}$`).MatchString(work) {
		t.Fatal("requires isolated root loopback namespace and owned work")
	}
	if raw, err := os.ReadFile(filepath.Join(work, "channel/owner")); err != nil || !strings.HasPrefix(string(raw), "msboost-joint-") {
		t.Fatal("missing joint owner")
	}
	remote := filepath.Join(work, "remote")
	if err := os.Mkdir(remote, 0700); err != nil {
		t.Fatal("remote must be a new private directory")
	}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	password := commerceID() + commerceID()
	cfg := &ssh.ServerConfig{PasswordCallback: func(meta ssh.ConnMetadata, p []byte) (*ssh.Permissions, error) {
		if meta.User() != "root" || string(p) != password {
			return nil, errors.New("rejected")
		}
		return nil, nil
	}}
	cfg.AddHostKey(signer)
	listener, err := net.Listen("tcp4", "8.8.8.8:0")
	if err != nil {
		t.Fatal("isolated SSH bind failed")
	}
	defer listener.Close()
	config := disaster.DefaultConfig()
	config.LocalDir, config.RemoteDir = filepath.Join(work, "backups"), remote
	config.RemoteHost, config.RemotePort, config.RemoteUser = "8.8.8.8", listener.Addr().(*net.TCPAddr).Port, "root"
	config.Fingerprint, config.Password = ssh.FingerprintSHA256(signer.PublicKey()), password
	if err := disaster.SaveConfig(filepath.Join(work, "site/disaster.json"), config); err != nil {
		t.Fatal("cannot save private pinned remote fixture config")
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
				server, channels, requests, err := ssh.NewServerConn(conn, cfg)
				if err != nil {
					return
				}
				defer server.Close()
				go ssh.DiscardRequests(requests)
				for next := range channels {
					if next.ChannelType() != "session" {
						_ = next.Reject(ssh.UnknownChannelType, "SFTP only")
						continue
					}
					channel, requests, err := next.Accept()
					if err != nil {
						return
					}
					for request := range requests {
						var subsystem struct{ Name string }
						ok := request.Type == "subsystem" && ssh.Unmarshal(request.Payload, &subsystem) == nil && subsystem.Name == "sftp"
						_ = request.Reply(ok, nil)
						if !ok {
							continue
						}
						// Real OS-backed SFTP in read-only mode. It can stat the
						// safe directory but refuses creation of the upload file.
						srv, err := sftp.NewServer(channel, sftp.ReadOnly())
						if err != nil {
							return
						}
						_ = os.WriteFile(filepath.Join(work, "sftp-used"), []byte("authenticated-pinned-sftp\n"), 0600)
						_ = srv.Serve()
						_ = srv.Close()
						break
					}
					_ = channel.Close()
				}
			}()
		}
	}()
	if err := os.WriteFile(filepath.Join(work, "sftp-ready"), []byte("ready\n"), 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(8 * time.Minute)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(work, "sftp-stop")); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("SFTP fixture exceeded bounded lifetime")
}
