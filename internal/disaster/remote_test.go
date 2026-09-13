package disaster

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

const remoteFixturePassword = "isolated-secret-never-log"

// This filesystem is memory-only. SFTP packets (including chmod and rename)
// still pass through a real localhost SSH password-authenticated connection.
type remoteFixtureFS struct {
	owners                           map[string]uint32
	createUID                        uint32
	writes                           int
	base                             sftp.Handlers
	mu                               sync.Mutex
	modes                            map[string]os.FileMode
	unsafeWrite                      bool
	removes                          int
	changeName                       string
	changeAt, changeCalls            int
	corruptName                      string
	corruptOnRead, reads             int
	readFailureName                  string
	corrupt, rejectChmod, raceRename atomic.Bool
}

type remoteFixtureRead struct {
	io.ReaderAt
	fs *remoteFixtureFS
}

func (r remoteFixtureRead) ReadAt(data []byte, offset int64) (int, error) {
	n, err := r.ReaderAt.ReadAt(data, offset)
	if n > 0 && r.fs.corrupt.Load() {
		data[0] ^= 1
	}
	return n, err
}
func (f *remoteFixtureFS) Fileread(r *sftp.Request) (io.ReaderAt, error) {
	f.mu.Lock()
	if r.Filepath == f.readFailureName {
		f.mu.Unlock()
		return nil, errors.New("fixture read interrupted")
	}
	if r.Filepath == f.corruptName {
		f.reads++
		if f.reads >= f.corruptOnRead {
			f.corrupt.Store(true)
		}
	}
	f.mu.Unlock()
	reader, err := f.base.FileGet.Fileread(r)
	if err != nil {
		return nil, err
	}
	return remoteFixtureRead{reader, f}, nil
}

type remoteFixtureWrite struct {
	io.WriterAt
	fs   *remoteFixtureFS
	name string
}

func (w remoteFixtureWrite) WriteAt(data []byte, offset int64) (int, error) {
	w.fs.mu.Lock()
	if w.fs.modes[w.name] != 0600 {
		w.fs.unsafeWrite = true
	}
	w.fs.writes += len(data)
	w.fs.mu.Unlock()
	return w.WriterAt.WriteAt(data, offset)
}
func (f *remoteFixtureFS) Filewrite(r *sftp.Request) (io.WriterAt, error) {
	writer, err := f.base.FilePut.Filewrite(r)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	if _, exists := f.owners[r.Filepath]; !exists {
		f.owners[r.Filepath] = f.createUID
	}
	f.mu.Unlock()
	return remoteFixtureWrite{writer, f, r.Filepath}, nil
}
func (f *remoteFixtureFS) Filecmd(r *sftp.Request) error {
	if r.Method == "Setstat" && r.AttrFlags().Permissions {
		if f.rejectChmod.Load() {
			return errors.New("fixture chmod failed")
		}
		if _, err := f.base.FileList.(sftp.LstatFileLister).Lstat(sftp.NewRequest("Stat", r.Filepath)); err != nil {
			return err
		}
		f.mu.Lock()
		f.modes[r.Filepath] = r.Attributes().FileMode().Perm()
		f.mu.Unlock()
		return nil
	}
	if r.Method == "Remove" {
		f.mu.Lock()
		f.removes++
		f.mu.Unlock()
	}
	if r.Method == "Rename" {
		if f.raceRename.Load() {
			return os.ErrExist
		}
		if _, err := f.base.FileList.(sftp.LstatFileLister).Lstat(sftp.NewRequest("Stat", r.Target)); !os.IsNotExist(err) {
			return os.ErrExist
		}
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

type remoteFixtureInfo struct {
	os.FileInfo
	mode os.FileMode
	uid  uint32
}
type remoteFixtureChangedInfo struct{ os.FileInfo }

func (i remoteFixtureChangedInfo) Size() int64 { return i.FileInfo.Size() + 1 }

func (i remoteFixtureInfo) Mode() os.FileMode { return i.FileInfo.Mode()&^os.ModePerm | i.mode }
func (i remoteFixtureInfo) Uid() uint32       { return i.uid }
func (i remoteFixtureInfo) Gid() uint32       { return i.uid }

type remoteFixtureLister struct {
	sftp.ListerAt
	fs      *remoteFixtureFS
	name    string
	listing bool
}

func (l remoteFixtureLister) ListAt(dst []os.FileInfo, off int64) (int, error) {
	n, err := l.ListerAt.ListAt(dst, off)
	l.fs.mu.Lock()
	defer l.fs.mu.Unlock()
	for i := 0; i < n; i++ {
		name := l.name
		if l.listing {
			name = path.Join(name, dst[i].Name())
		}
		mode, ok := l.fs.modes[name]
		if !ok {
			mode = dst[i].Mode().Perm()
		}
		dst[i] = remoteFixtureInfo{dst[i], mode, l.fs.owners[name]}
		if name == l.fs.changeName && !l.listing {
			l.fs.changeCalls++
			if l.fs.changeCalls >= l.fs.changeAt {
				dst[i] = remoteFixtureChangedInfo{dst[i]}
			}
		}
	}
	return n, err
}
func (f *remoteFixtureFS) Filelist(r *sftp.Request) (sftp.ListerAt, error) {
	l, err := f.base.FileList.Filelist(r)
	if err != nil {
		return nil, err
	}
	return remoteFixtureLister{l, f, r.Filepath, r.Method == "List"}, nil
}
func (f *remoteFixtureFS) Lstat(r *sftp.Request) (sftp.ListerAt, error) {
	l, err := f.base.FileList.(sftp.LstatFileLister).Lstat(r)
	if err != nil {
		return nil, err
	}
	return remoteFixtureLister{l, f, r.Filepath, false}, nil
}

type remoteFixture struct {
	allowedUser          atomic.Value
	address, fingerprint string
	fs                   *remoteFixtureFS
	authCalls            atomic.Int32
}

func newRemoteFixture(t *testing.T) *remoteFixture {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	f := &remoteFixture{fingerprint: ssh.FingerprintSHA256(signer.PublicKey()), fs: &remoteFixtureFS{base: sftp.InMemHandler(), modes: map[string]os.FileMode{}, owners: map[string]uint32{}}}
	f.allowedUser.Store("root")
	cfg := &ssh.ServerConfig{PasswordCallback: func(meta ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
		f.authCalls.Add(1)
		if meta.User() != f.allowedUser.Load().(string) || string(password) != remoteFixturePassword {
			return nil, errors.New("authentication rejected")
		}
		return nil, nil
	}}
	cfg.AddHostKey(signer)
	// Debian commonly offers both. The CLI must use the Ed25519 key named by
	// its verification instructions, not the default preferred ECDSA key.
	ecdsaKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecdsaSigner, err := ssh.NewSignerFromKey(ecdsaKey)
	if err != nil {
		t.Fatal(err)
	}
	cfg.AddHostKey(ecdsaSigner)
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
				server, channels, requests, err := ssh.NewServerConn(conn, cfg)
				if err != nil {
					return
				}
				defer server.Close()
				go ssh.DiscardRequests(requests)
				for next := range channels {
					if next.ChannelType() != "session" {
						_ = next.Reject(ssh.UnknownChannelType, "only sftp")
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
						srv := sftp.NewRequestServer(channel, sftp.Handlers{FileGet: f.fs, FilePut: f.fs, FileCmd: f.fs, FileList: f.fs})
						_ = srv.Serve()
						_ = srv.Close()
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
func (f *remoteFixture) dial(t *testing.T) remoteDial {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		if network != "tcp" || address != "8.8.8.8:22" {
			t.Errorf("unexpected target")
			return nil, errors.New("unexpected target")
		}
		return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp", f.address)
	}
}
func (f *remoteFixture) connect(t *testing.T) *sftp.Client {
	t.Helper()
	client, err := ssh.Dial("tcp", f.address, &ssh.ClientConfig{User: f.allowedUser.Load().(string), HostKeyAlgorithms: []string{ssh.KeyAlgoED25519}, Auth: []ssh.AuthMethod{ssh.Password(remoteFixturePassword)}, HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
		if ssh.FingerprintSHA256(key) != f.fingerprint {
			return errors.New("bad pin")
		}
		return nil
	}, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	sf, err := sftp.NewClient(client)
	if err != nil {
		_ = client.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sf.Close(); _ = client.Close() })
	return sf
}
func remoteBundleFixture(t *testing.T) string {
	t.Helper()
	source, dir := testPrivateDirectory(t), testPrivateDirectory(t)
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	if err := tw.WriteHeader(&tar.Header{Name: "fixture.txt", Mode: 0600, Size: 7, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write([]byte("fixture"))
	_ = tw.Close()
	for _, name := range members {
		content := []byte("private isolated fixture")
		if strings.HasSuffix(name, ".tar") {
			content = raw.Bytes()
		}
		if err := os.WriteFile(filepath.Join(source, name), content, 0600); err != nil {
			t.Fatal(err)
		}
	}
	archive := filepath.Join(dir, "msboost-disaster-20260913T010203Z-0123456789abcdef.tar.gz")
	if err := Pack(source, archive); err != nil {
		t.Fatal(err)
	}
	return archive
}
func remoteTestConfig(t *testing.T, f *remoteFixture) Config {
	t.Helper()
	c := DefaultConfig()
	c.LocalDir = testPrivateDirectory(t)
	c.RemoteHost = "8.8.8.8"
	c.RemotePort = 22
	c.RemoteUser = "root"
	c.RemoteDir = "/remote/backups"
	c.Fingerprint = f.fingerprint
	c.Password = remoteFixturePassword
	return c
}

func TestDisasterRemoteRealSSHUploadAndNoOverwrite(t *testing.T) {
	f := newRemoteFixture(t)
	c := remoteTestConfig(t, f)
	archive := remoteBundleFixture(t)
	if err := uploadWithDial(context.Background(), c, archive, f.dial(t)); err != nil {
		t.Fatal(err)
	}
	sf := f.connect(t)
	name := path.Join(c.RemoteDir, filepath.Base(archive))
	file, err := sf.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(file)
	_ = file.Close()
	if err != nil {
		t.Fatal(err)
	}
	local, _ := os.ReadFile(archive)
	if !bytes.Equal(raw, local) {
		t.Fatal("remote bytes differ")
	}
	info, err := sf.Lstat(name)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("remote file not private")
	}
	dir, err := sf.Lstat(c.RemoteDir)
	if err != nil || dir.Mode().Perm() != 0700 {
		t.Fatal("remote directory not private")
	}
	if err := uploadWithDial(context.Background(), c, archive, f.dial(t)); err == nil {
		t.Fatal("existing backup overwritten")
	}
	file, err = sf.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := io.ReadAll(file)
	_ = file.Close()
	if !bytes.Equal(raw, after) {
		t.Fatal("existing backup changed")
	}
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	if f.fs.unsafeWrite || f.fs.removes != 0 {
		t.Fatal("nonprivate write or remote deletion")
	}
}

func TestDisasterRemoteRejectsPinBeforePasswordAndBadCredentials(t *testing.T) {
	archive := remoteBundleFixture(t)
	for _, kind := range []string{"pin", "password"} {
		t.Run(kind, func(t *testing.T) {
			f := newRemoteFixture(t)
			c := remoteTestConfig(t, f)
			if kind == "pin" {
				_, key, _ := ed25519.GenerateKey(rand.Reader)
				signer, _ := ssh.NewSignerFromKey(key)
				c.Fingerprint = ssh.FingerprintSHA256(signer.PublicKey())
			} else {
				c.Password = "wrong-fixture-secret"
			}
			err := uploadWithDial(context.Background(), c, archive, f.dial(t))
			if err == nil {
				t.Fatal("invalid credentials accepted")
			}
			if strings.Contains(err.Error(), c.Password) {
				t.Fatal("credential in error")
			}
			if kind == "pin" && f.authCalls.Load() != 0 {
				t.Fatal("password sent before host verification")
			}
		})
	}
}

func TestDisasterRemoteFailuresNeverPublishOrDelete(t *testing.T) {
	archive := remoteBundleFixture(t)
	for _, kind := range []string{"corrupt", "chmod", "race", "symlink", "shared-parent", "shared-destination"} {
		t.Run(kind, func(t *testing.T) {
			f := newRemoteFixture(t)
			c := remoteTestConfig(t, f)
			sf := f.connect(t)
			switch kind {
			case "corrupt":
				f.fs.corrupt.Store(true)
			case "chmod":
				f.fs.rejectChmod.Store(true)
			case "race":
				f.fs.raceRename.Store(true)
			case "symlink":
				if err := sf.Symlink("/", "/remote"); err != nil {
					t.Fatal(err)
				}
			case "shared-parent":
				if err := sf.Mkdir("/remote"); err != nil {
					t.Fatal(err)
				}
				if err := sf.Chmod("/remote", 0777); err != nil {
					t.Fatal(err)
				}
			case "shared-destination":
				if err := sf.Mkdir("/remote"); err != nil {
					t.Fatal(err)
				}
				if err := sf.Chmod("/remote", 0700); err != nil {
					t.Fatal(err)
				}
				if err := sf.Mkdir(c.RemoteDir); err != nil {
					t.Fatal(err)
				}
				if err := sf.Chmod(c.RemoteDir, 0755); err != nil {
					t.Fatal(err)
				}
			}
			if err := uploadWithDial(context.Background(), c, archive, f.dial(t)); err == nil {
				t.Fatalf("accepted %s", kind)
			}
			if _, err := sf.Lstat(path.Join(c.RemoteDir, filepath.Base(archive))); !os.IsNotExist(err) {
				t.Fatal("failure published final name")
			}
			if _, err := Verify(archive); err != nil {
				t.Fatal("local copy changed")
			}
			f.fs.mu.Lock()
			defer f.fs.mu.Unlock()
			if f.fs.unsafeWrite || f.fs.removes != 0 {
				t.Fatal("nonprivate upload or deletion")
			}
		})
	}
}

func TestDisasterRemoteRejectsInvalidInputBeforeDial(t *testing.T) {
	f := newRemoteFixture(t)
	c := remoteTestConfig(t, f)
	archive := remoteBundleFixture(t)
	dial := func(context.Context, string, string) (net.Conn, error) {
		t.Error("unexpected connection")
		return nil, errors.New("blocked")
	}
	for _, host := range []string{"127.0.0.1", "10.1.1.1", "example.com", "::ffff:127.0.0.1"} {
		bad := c
		bad.RemoteHost = host
		if uploadWithDial(context.Background(), bad, archive, dial) == nil {
			t.Fatal("non-public literal accepted")
		}
	}
	if err := os.WriteFile(archive, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if uploadWithDial(context.Background(), c, archive, dial) == nil {
		t.Fatal("corrupt local bundle uploaded")
	}
}

func remoteAgedBundle(t *testing.T, index, days int) string {
	t.Helper()
	original := remoteBundleFixture(t)
	file, err := os.Open(original)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var raw bytes.Buffer
	writer := gzip.NewWriter(&raw)
	tw := tar.NewWriter(writer)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		content, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		if header.Name == "manifest.json" {
			var m Manifest
			if json.Unmarshal(content, &m) != nil {
				t.Fatal("manifest fixture")
			}
			m.CreatedAt = time.Now().AddDate(0, 0, -days).Unix()
			content, _ = json.Marshal(m)
			header.Size = int64(len(content))
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if tw.Close() != nil || writer.Close() != nil {
		t.Fatal("aged bundle fixture close")
	}
	name := filepath.Join(testPrivateDirectory(t), fmt.Sprintf("msboost-disaster-20260913T010203Z-%016x.tar.gz", index))
	if err := os.WriteFile(name, raw.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(name); err != nil {
		t.Fatal(err)
	}
	return name
}

func seedRemoteBundle(t *testing.T, sf *sftp.Client, directory, source string) string {
	t.Helper()
	raw, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	name := path.Join(directory, filepath.Base(source))
	file, err := sf.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY)
	if err != nil {
		t.Fatal(err)
	}
	if err = file.Chmod(0600); err != nil {
		t.Fatal(err)
	}
	if _, err = file.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	return name
}

func TestDisasterRemoteRetentionExplicitOnlyExpiredAndTwoMinimum(t *testing.T) {
	for _, kind := range []string{"off", "expired", "minimum"} {
		t.Run(kind, func(t *testing.T) {
			f := newRemoteFixture(t)
			c := remoteTestConfig(t, f)
			c.PruneRemote = kind != "off"
			sf := f.connect(t)
			uid := uint32(0)
			if err := privateRemoteDirectory(sf, c.RemoteDir, &uid); err != nil {
				t.Fatal(err)
			}
			ages := []int{1, 2, 3, 50}
			wantRemoved := 1
			if kind == "minimum" {
				ages = []int{50, 51, 52, 53}
				wantRemoved = 2
			}
			if kind == "off" {
				wantRemoved = 0
			}
			var names []string
			for i, age := range ages {
				names = append(names, seedRemoteBundle(t, sf, c.RemoteDir, remoteAgedBundle(t, i+1, age)))
			}
			// An unmanaged name and a link never count toward the two-copy minimum.
			if err := sf.Symlink(names[0], path.Join(c.RemoteDir, "msboost-disaster-20260913T010203Z-9999999999999999.tar.gz")); err != nil {
				t.Fatal(err)
			}
			if err := pruneRemote(sf, c, uid); err != nil {
				t.Fatal(err)
			}
			for i, name := range names {
				_, err := sf.Lstat(name)
				shouldRemain := i < len(names)-wantRemoved
				if shouldRemain && err != nil {
					t.Fatal("retained backup missing")
				}
				if !shouldRemain && !os.IsNotExist(err) {
					t.Fatal("expired candidate not removed")
				}
			}
			f.fs.mu.Lock()
			removed := f.fs.removes
			f.fs.mu.Unlock()
			if removed != wantRemoved {
				t.Fatalf("removed %d, wanted %d", removed, wantRemoved)
			}
			entries, err := os.ReadDir(c.LocalDir)
			if err != nil || len(entries) != 0 {
				t.Fatal("task private temporary downloads not cleaned")
			}
		})
	}
}

func TestDisasterRemoteRetentionAnyBadOrChangedCandidateStops(t *testing.T) {
	for _, kind := range []string{"bad-bundle", "read-failure", "changed-metadata", "changed-content"} {
		t.Run(kind, func(t *testing.T) {
			f := newRemoteFixture(t)
			c := remoteTestConfig(t, f)
			c.PruneRemote = true
			sf := f.connect(t)
			uid := uint32(0)
			if err := privateRemoteDirectory(sf, c.RemoteDir, &uid); err != nil {
				t.Fatal(err)
			}
			var names []string
			for i, age := range []int{1, 2, 60} {
				names = append(names, seedRemoteBundle(t, sf, c.RemoteDir, remoteAgedBundle(t, i+1, age)))
			}
			switch kind {
			case "read-failure":
				f.fs.mu.Lock()
				f.fs.readFailureName = names[1]
				f.fs.mu.Unlock()
			case "bad-bundle":
				file, err := sf.OpenFile(names[1], os.O_WRONLY|os.O_TRUNC)
				if err != nil {
					t.Fatal(err)
				}
				_, err = file.Write([]byte("invalid backup"))
				_ = file.Close()
				if err != nil {
					t.Fatal(err)
				}
			case "changed-metadata":
				f.fs.mu.Lock()
				f.fs.changeName = names[2]
				f.fs.changeAt = 4
				f.fs.mu.Unlock()
			case "changed-content":
				f.fs.mu.Lock()
				f.fs.corruptName = names[2]
				f.fs.corruptOnRead = 2
				f.fs.mu.Unlock()
			}
			if err := pruneRemote(sf, c, uid); err == nil {
				t.Fatalf("accepted %s", kind)
			}
			f.fs.mu.Lock()
			removed := f.fs.removes
			f.fs.mu.Unlock()
			if removed != 0 {
				t.Fatal("failure deleted a backup")
			}
			for _, name := range names {
				if _, err := sf.Lstat(name); err != nil {
					t.Fatal("backup removed on failed validation")
				}
			}
			entries, err := os.ReadDir(c.LocalDir)
			if err != nil || len(entries) != 0 {
				t.Fatal("private task download cleanup incomplete")
			}
		})
	}
}
