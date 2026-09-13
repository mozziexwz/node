package control

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

func backupTestKey(t *testing.T) (string, ssh.Signer) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(key, "isolated backup test")
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(block)), signer
}

func backupStoredTarget(t *testing.T, a *App, id string) BackupTarget {
	t.Helper()
	var target BackupTarget
	if err := a.Store.View(func(s *State) error {
		var exists bool
		target, exists = LoadDoc[BackupTarget](s, "backup_targets", id)
		if !exists {
			return errors.New("target not found")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return target
}

func assertBackupRedacted(t *testing.T, response map[string]any) {
	t.Helper()
	for _, key := range []string{"password", "privateKey", "sealedKey", "sealedPassword"} {
		if _, exists := response[key]; exists {
			t.Fatalf("sensitive field returned: %s", key)
		}
	}
}

func TestBackupTargetCredentialModesSealRedactAndLegacy(t *testing.T) {
	a, identity := identityFixture(t, true)
	cookie, csrf := identityLoginAdmin(t, identity)
	mux := http.NewServeMux()
	b := NewBackupService(a)
	b.Register(mux)
	h := a.Authenticate(mux)
	key, signer := backupTestKey(t)
	input := BackupTarget{Name: "test backup", Host: "8.8.8.8", Port: 22, Fingerprint: ssh.FingerprintSHA256(signer.PublicKey()), AuthMode: "password", Password: "isolated-backup-password", Enabled: true, SealedPassword: "untrusted-client-envelope", SealedKey: "untrusted-client-key"}
	response := identityResponse(t, identityRequest(h, "POST", "/api/admin/backup-targets", input, cookie, csrf), 200)
	assertBackupRedacted(t, response)
	id := response["id"].(string)
	saved := backupStoredTarget(t, a, id)
	if saved.User != "root" || saved.Path != "/root/msboost-backup" || saved.AuthMode != "password" || saved.Password != "" || saved.PrivateKey != "" || saved.SealedKey != "" || saved.SealedPassword == "" || saved.SealedPassword == input.SealedPassword {
		t.Fatal("password target was not normalized and encrypted")
	}
	opened, err := a.Open(saved.SealedPassword)
	if err != nil || string(opened) != input.Password {
		t.Fatal("saved password cannot be decrypted")
	}
	_ = a.Store.View(func(s *State) error {
		if bytes.Contains(s.Docs["backup_targets"][id], []byte(input.Password)) {
			t.Fatal("plaintext password persisted")
		}
		return nil
	})
	update := saved
	update.Password = ""
	update.SealedPassword = "client-attempt-to-replace-sealed-secret"
	update.Name = "renamed"
	response = identityResponse(t, identityRequest(h, "PUT", "/api/admin/backup-targets/"+id, update, cookie, csrf), 200)
	assertBackupRedacted(t, response)
	if backupStoredTarget(t, a, id).SealedPassword != saved.SealedPassword {
		t.Fatal("same-mode blank password did not preserve server-side secret")
	}
	update.Password = "replacement-backup-password"
	identityResponse(t, identityRequest(h, "PUT", "/api/admin/backup-targets/"+id, update, cookie, csrf), 200)
	saved = backupStoredTarget(t, a, id)
	opened, err = a.Open(saved.SealedPassword)
	if err != nil || string(opened) != update.Password {
		t.Fatal("new password did not replace encrypted credential")
	}
	update.Password = ""
	update.AuthMode = "private_key"
	identityResponse(t, identityRequest(h, "PUT", "/api/admin/backup-targets/"+id, update, cookie, csrf), 400)
	if backupStoredTarget(t, a, id).SealedPassword != saved.SealedPassword {
		t.Fatal("failed mode change modified password")
	}
	update.PrivateKey = "not-a-private-key"
	identityResponse(t, identityRequest(h, "PUT", "/api/admin/backup-targets/"+id, update, cookie, csrf), 400)
	update.PrivateKey = key
	response = identityResponse(t, identityRequest(h, "PUT", "/api/admin/backup-targets/"+id, update, cookie, csrf), 200)
	assertBackupRedacted(t, response)
	saved = backupStoredTarget(t, a, id)
	opened, err = a.Open(saved.SealedKey)
	if err != nil || string(opened) != key || saved.SealedPassword != "" || saved.PrivateKey != "" {
		t.Fatal("private-key mode must replace, encrypt and clear obsolete password")
	}
	// A genuine pre-authMode record keeps its private-key credential.
	saved.AuthMode = ""
	if err = a.Store.Update(func(s *State) error { return SaveDoc(s, "backup_targets", id, saved) }); err != nil {
		t.Fatal(err)
	}
	update = saved
	update.SealedKey = "untrusted-envelope"
	response = identityResponse(t, identityRequest(h, "PUT", "/api/admin/backup-targets/"+id, update, cookie, csrf), 200)
	assertBackupRedacted(t, response)
	if response["authMode"] != "private_key" || backupStoredTarget(t, a, id).SealedKey != saved.SealedKey {
		t.Fatal("legacy blank mode did not retain key")
	}
	update.AuthMode = "password"
	identityResponse(t, identityRequest(h, "PUT", "/api/admin/backup-targets/"+id, update, cookie, csrf), 400)
	update.Password = "new-password-after-key-mode"
	response = identityResponse(t, identityRequest(h, "PUT", "/api/admin/backup-targets/"+id, update, cookie, csrf), 200)
	assertBackupRedacted(t, response)
	if current := backupStoredTarget(t, a, id); current.SealedKey != "" || current.SealedPassword == "" {
		t.Fatal("password switch retained obsolete key")
	}
	listed := identityResponse(t, identityRequest(h, "GET", "/api/admin/backup-targets", nil, cookie, ""), 200)
	for _, item := range listed["targets"].([]any) {
		assertBackupRedacted(t, item.(map[string]any))
	}
	identityResponse(t, identityRequest(h, "POST", "/api/admin/backup-targets", input, nil, ""), 403)
}

func TestBackupTargetRejectsInvalidOrInjectedCredentials(t *testing.T) {
	a, identity := identityFixture(t, true)
	cookie, csrf := identityLoginAdmin(t, identity)
	mux := http.NewServeMux()
	NewBackupService(a).Register(mux)
	h := a.Authenticate(mux)
	_, signer := backupTestKey(t)
	base := BackupTarget{Name: "test", Host: "8.8.8.8", Port: 22, User: "root", Path: "/root/msboost-backup", Fingerprint: ssh.FingerprintSHA256(signer.PublicKey()), AuthMode: "password", Password: "isolated-password"}
	for name, change := range map[string]func(*BackupTarget){
		"private IP":                func(v *BackupTarget) { v.Host = "127.0.0.1" },
		"hostname":                  func(v *BackupTarget) { v.Host = "backup.example" },
		"zero port":                 func(v *BackupTarget) { v.Port = 0 },
		"port overflow":             func(v *BackupTarget) { v.Port = 65536 },
		"root directory":            func(v *BackupTarget) { v.Path = "/" },
		"relative directory":        func(v *BackupTarget) { v.Path = "backup" },
		"parent traversal":          func(v *BackupTarget) { v.Path = "/root/../etc" },
		"path NUL":                  func(v *BackupTarget) { v.Path = "/root/backup\x00" },
		"missing fingerprint":       func(v *BackupTarget) { v.Fingerprint = "" },
		"short fingerprint":         func(v *BackupTarget) { v.Fingerprint = "SHA256:abc" },
		"fingerprint newline":       func(v *BackupTarget) { v.Fingerprint += "\n" },
		"padded fingerprint":        func(v *BackupTarget) { v.Fingerprint += "=" },
		"unknown mode":              func(v *BackupTarget) { v.AuthMode = "agent" },
		"missing password":          func(v *BackupTarget) { v.Password = "" },
		"injected envelope":         func(v *BackupTarget) { v.Password = ""; v.SealedPassword = "client-sealed" },
		"mixed credentials":         func(v *BackupTarget) { v.PrivateKey = "private-key-must-not-be-returned" },
		"oversized password":        func(v *BackupTarget) { v.Password = strings.Repeat("x", 513) },
		"password NUL":              func(v *BackupTarget) { v.Password = "invalid\x00password" },
		"password newline":          func(v *BackupTarget) { v.Password = "invalid\npassword" },
		"username newline":          func(v *BackupTarget) { v.User = "root\n" },
		"private key envelope only": func(v *BackupTarget) { v.AuthMode = "private_key"; v.Password = ""; v.SealedKey = "client-sealed" },
	} {
		t.Run(name, func(t *testing.T) {
			input := base
			change(&input)
			w := identityRequest(h, "POST", "/api/admin/backup-targets", input, cookie, csrf)
			identityResponse(t, w, 400)
			if strings.Contains(w.Body.String(), "isolated-password") || strings.Contains(w.Body.String(), "client-sealed") || strings.Contains(w.Body.String(), "private-key-must-not-be-returned") {
				t.Fatal("error echoed credential")
			}
		})
	}
	_ = a.Store.View(func(s *State) error {
		if len(ListDocs[BackupTarget](s, "backup_targets")) != 0 {
			t.Fatal("invalid target persisted")
		}
		return nil
	})
}

// Actual SSH authentication and SFTP wire protocol, backed by an isolated
// in-memory filesystem. Unlike sftp's example handler, this wrapper models
// chmod so permission checks and write-before-chmod failures are observable.
type backupSFTPFiles struct {
	owners       map[string]uint32
	createUID    uint32
	writes       int
	base         sftp.Handlers
	mu           sync.Mutex
	modes        map[string]os.FileMode
	unsafeWrite  bool
	corruptReads atomic.Bool
}

func (f *backupSFTPFiles) Fileread(r *sftp.Request) (io.ReaderAt, error) {
	reader, err := f.base.FileGet.Fileread(r)
	if err != nil {
		return nil, err
	}
	return backupReadAt{reader, f}, nil
}

type backupReadAt struct {
	io.ReaderAt
	fs *backupSFTPFiles
}

func (r backupReadAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := r.ReaderAt.ReadAt(p, off)
	if n > 0 && r.fs.corruptReads.Load() {
		p[0] ^= 1
	}
	return n, err
}
func (f *backupSFTPFiles) Filewrite(r *sftp.Request) (io.WriterAt, error) {
	writer, err := f.base.FilePut.Filewrite(r)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	if _, exists := f.owners[r.Filepath]; !exists {
		f.owners[r.Filepath] = f.createUID
	}
	f.mu.Unlock()
	return backupWriteAt{writer, f, r.Filepath}, nil
}

type backupWriteAt struct {
	io.WriterAt
	fs   *backupSFTPFiles
	path string
}

func (w backupWriteAt) WriteAt(p []byte, off int64) (int, error) {
	w.fs.mu.Lock()
	if w.fs.modes[w.path] != 0600 {
		w.fs.unsafeWrite = true
	}
	w.fs.writes += len(p)
	w.fs.mu.Unlock()
	return w.WriterAt.WriteAt(p, off)
}
func (f *backupSFTPFiles) Filecmd(r *sftp.Request) error {
	if r.Method == "Setstat" && r.AttrFlags().Permissions {
		if _, err := f.base.FileList.(sftp.LstatFileLister).Lstat(sftp.NewRequest("Stat", r.Filepath)); err != nil {
			return err
		}
		f.mu.Lock()
		f.modes[r.Filepath] = r.Attributes().FileMode().Perm()
		f.mu.Unlock()
		return nil
	}
	err := f.base.FileCmd.Filecmd(r)
	if err == nil && r.Method == "Mkdir" {
		f.mu.Lock()
		f.owners[r.Filepath] = f.createUID
		f.mu.Unlock()
	}
	if err == nil && r.Method == "Rename" {
		f.mu.Lock()
		f.modes[r.Target] = f.modes[r.Filepath]
		f.owners[r.Target] = f.owners[r.Filepath]
		delete(f.modes, r.Filepath)
		delete(f.owners, r.Filepath)
		f.mu.Unlock()
	}
	return err
}

type backupFileInfo struct {
	os.FileInfo
	mode os.FileMode
	uid  uint32
}

func (f backupFileInfo) Mode() os.FileMode { return f.FileInfo.Mode()&^os.ModePerm | f.mode }
func (f backupFileInfo) Uid() uint32       { return f.uid }
func (f backupFileInfo) Gid() uint32       { return f.uid }

type backupLister struct {
	sftp.ListerAt
	fs   *backupSFTPFiles
	path string
	list bool
}

func (l backupLister) ListAt(dst []os.FileInfo, off int64) (int, error) {
	n, err := l.ListerAt.ListAt(dst, off)
	l.fs.mu.Lock()
	defer l.fs.mu.Unlock()
	for i := 0; i < n; i++ {
		p := l.path
		if l.list {
			p = path.Join(p, dst[i].Name())
		}
		mode, ok := l.fs.modes[p]
		if !ok {
			mode = dst[i].Mode().Perm()
		}
		dst[i] = backupFileInfo{dst[i], mode, l.fs.owners[p]}
	}
	return n, err
}
func (f *backupSFTPFiles) Filelist(r *sftp.Request) (sftp.ListerAt, error) {
	l, err := f.base.FileList.Filelist(r)
	if err != nil {
		return nil, err
	}
	return backupLister{l, f, r.Filepath, r.Method == "List"}, nil
}
func (f *backupSFTPFiles) Lstat(r *sftp.Request) (sftp.ListerAt, error) {
	l, err := f.base.FileList.(sftp.LstatFileLister).Lstat(r)
	if err != nil {
		return nil, err
	}
	return backupLister{l, f, r.Filepath, false}, nil
}

type backupSSHServer struct {
	allowedUser                      atomic.Value
	address, fingerprint, privateKey string
	passwordCalls, keyCalls          atomic.Int32
	files                            *backupSFTPFiles
}

const backupFixturePassword = "only-for-local-ssh-sftp-test"

func newBackupSSHServer(t *testing.T) *backupSSHServer {
	t.Helper()
	_, hostSigner := backupTestKey(t)
	clientKey, clientSigner := backupTestKey(t)
	f := &backupSSHServer{fingerprint: ssh.FingerprintSHA256(hostSigner.PublicKey()), privateKey: clientKey, files: &backupSFTPFiles{base: sftp.InMemHandler(), modes: map[string]os.FileMode{}, owners: map[string]uint32{}}}
	f.allowedUser.Store("root")
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(meta ssh.ConnMetadata, p []byte) (*ssh.Permissions, error) {
			f.passwordCalls.Add(1)
			if meta.User() != f.allowedUser.Load().(string) || string(p) != backupFixturePassword {
				return nil, errors.New("auth rejected")
			}
			return nil, nil
		},
		PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			f.keyCalls.Add(1)
			if meta.User() != f.allowedUser.Load().(string) || !bytes.Equal(key.Marshal(), clientSigner.PublicKey().Marshal()) {
				return nil, errors.New("key rejected")
			}
			return nil, nil
		},
	}
	cfg.AddHostKey(hostSigner)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.address = listener.Addr().String()
	var wg sync.WaitGroup
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(8 * time.Second))
				server, chans, requests, err := ssh.NewServerConn(conn, cfg)
				if err != nil {
					return
				}
				defer server.Close()
				go ssh.DiscardRequests(requests)
				for next := range chans {
					if next.ChannelType() != "session" {
						_ = next.Reject(ssh.UnknownChannelType, "session only")
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
						sftpServer := sftp.NewRequestServer(channel, sftp.Handlers{FileGet: f.files, FilePut: f.files, FileCmd: f.files, FileList: f.files})
						_ = sftpServer.Serve()
						_ = sftpServer.Close()
						break
					}
					_ = channel.Close()
				}
			}()
		}
	}()
	t.Cleanup(func() { _ = listener.Close(); <-done; wg.Wait() })
	return f
}

func (f *backupSSHServer) connect(t *testing.T) *sftp.Client {
	t.Helper()
	client, err := ssh.Dial("tcp", f.address, &ssh.ClientConfig{User: f.allowedUser.Load().(string), Auth: []ssh.AuthMethod{ssh.Password(backupFixturePassword)}, HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
		if ssh.FingerprintSHA256(key) != f.fingerprint {
			return errors.New("pin mismatch")
		}
		return nil
	}, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	sf, err := sftp.NewClient(client)
	if err != nil {
		client.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sf.Close(); _ = client.Close() })
	return sf
}

func backupUploadFixture(t *testing.T) (*BackupService, *backupSSHServer, BackupTarget) {
	t.Helper()
	a, _ := identityFixture(t, true)
	b := NewBackupService(a)
	server := newBackupSSHServer(t)
	sealed, err := a.Seal([]byte(backupFixturePassword))
	if err != nil {
		t.Fatal(err)
	}
	target := BackupTarget{Host: "8.8.8.8", Port: 22, User: "root", Path: "/root/msboost-backup", Fingerprint: server.fingerprint, AuthMode: "password", SealedPassword: sealed}
	b.dialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if network != "tcp" || address != "8.8.8.8:22" {
			return nil, errors.New("test forbids unexpected dial")
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp4", server.address)
	}
	return b, server, target
}

func TestBackupPasswordActualSSHAndSFTPTransfer(t *testing.T) {
	b, server, target := backupUploadFixture(t)
	raw := []byte("isolated encrypted backup payload")
	if err := b.upload(context.Background(), target, "password-transfer", raw); err != nil {
		t.Fatal(err)
	}
	if server.passwordCalls.Load() != 1 || server.keyCalls.Load() != 0 {
		t.Fatal("password mode did not use only SSH password authentication")
	}
	sf := server.connect(t)
	for _, directory := range []string{"/root", "/root/msboost-backup"} {
		info, err := sf.Lstat(directory)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
			t.Fatal("new backup directory is not private")
		}
	}
	file := path.Join(target.Path, "msboost-password-transfer.msb")
	info, err := sf.Lstat(file)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("published backup is not 0600")
	}
	reader, err := sf.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatal("remote verified content differs")
	}
	if _, err := sf.Lstat(path.Join(target.Path, "msboost-password-transfer.partial")); !os.IsNotExist(err) {
		t.Fatal("published backup retained partial file")
	}
	server.files.mu.Lock()
	unsafe := server.files.unsafeWrite
	server.files.mu.Unlock()
	if unsafe {
		t.Fatal("backup payload written before 0600 permissions")
	}
	if err := b.upload(context.Background(), target, "password-transfer", []byte("do not overwrite")); err == nil {
		t.Fatal("existing backup overwritten")
	}
}

func TestBackupSSHHostPinBeforePasswordAndLegacyKey(t *testing.T) {
	t.Run("pin mismatch blocks credential", func(t *testing.T) {
		b, server, target := backupUploadFixture(t)
		_, other := backupTestKey(t)
		target.Fingerprint = ssh.FingerprintSHA256(other.PublicKey())
		if err := b.upload(context.Background(), target, "pin-mismatch", []byte("test")); err == nil {
			t.Fatal("wrong pin accepted")
		}
		if server.passwordCalls.Load() != 0 || server.keyCalls.Load() != 0 {
			t.Fatal("credential sent before host pin accepted")
		}
	})
	t.Run("wrong password fails safely", func(t *testing.T) {
		b, server, target := backupUploadFixture(t)
		target.SealedPassword, _ = b.app.Seal([]byte("incorrect-test-password"))
		err := b.upload(context.Background(), target, "wrong-password", []byte("test"))
		if err == nil || strings.Contains(err.Error(), "incorrect-test-password") {
			t.Fatal("incorrect password succeeded or leaked")
		}
		if server.passwordCalls.Load() != 1 {
			t.Fatal("password authentication not attempted")
		}
	})
	t.Run("legacy missing mode uses private key", func(t *testing.T) {
		b, server, target := backupUploadFixture(t)
		target.AuthMode = ""
		target.SealedPassword = ""
		target.SealedKey, _ = b.app.Seal([]byte(server.privateKey))
		if err := b.upload(context.Background(), target, "legacy-key", []byte("legacy encrypted payload")); err != nil {
			t.Fatal(err)
		}
		if server.passwordCalls.Load() != 0 || server.keyCalls.Load() == 0 {
			t.Fatal("legacy target did not use private-key authentication")
		}
	})
}

func TestBackupSFTPRejectsSymlinksWritableParentsAndCorruption(t *testing.T) {
	for _, scenario := range []string{"intermediate symlink", "target symlink", "writable parent", "corrupt reread"} {
		t.Run(scenario, func(t *testing.T) {
			b, server, target := backupUploadFixture(t)
			sf := server.connect(t)
			if err := sf.Mkdir("/root"); err != nil {
				t.Fatal(err)
			}
			if err := sf.Chmod("/root", 0700); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "intermediate symlink", "target symlink":
				if err := sf.Mkdir("/outside"); err != nil {
					t.Fatal(err)
				}
				if err := sf.Symlink("/outside", "/root/link"); err != nil {
					t.Fatal(err)
				}
				target.Path = "/root/link"
				if scenario == "intermediate symlink" {
					target.Path += "/nested"
				}
			case "writable parent":
				if err := sf.Chmod("/root", 0777); err != nil {
					t.Fatal(err)
				}
			case "corrupt reread":
				server.files.corruptReads.Store(true)
			}
			if err := b.upload(context.Background(), target, "rejected-upload", []byte("not published")); err == nil {
				t.Fatal("unsafe or corrupt upload succeeded")
			}
			if _, err := sf.Lstat(path.Join(target.Path, "msboost-rejected-upload.msb")); !os.IsNotExist(err) {
				t.Fatal("failed upload published a final file")
			}
			if _, err := sf.Lstat(path.Join(target.Path, "msboost-rejected-upload.partial")); !os.IsNotExist(err) {
				t.Fatal("failed upload left a newly created partial file")
			}
			if strings.Contains(scenario, "symlink") {
				entries, err := sf.ReadDir("/outside")
				if err != nil || len(entries) != 0 {
					t.Fatal("symlink target was modified")
				}
			}
		})
	}
}

func TestBackupUploadValidationBeforeDial(t *testing.T) {
	b, _, target := backupUploadFixture(t)
	dialed := false
	b.dialContext = func(context.Context, string, string) (net.Conn, error) {
		dialed = true
		return nil, errors.New("must not connect")
	}
	target.Host = "127.0.0.1"
	if b.upload(context.Background(), target, "test", nil) == nil || dialed {
		t.Fatal("private target reached TCP dialer")
	}
	target.Host = "8.8.8.8"
	if b.upload(context.Background(), target, "../../outside", nil) == nil || dialed {
		t.Fatal("invalid backup ID reached TCP dialer")
	}
}
