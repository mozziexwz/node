package disaster

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

type archiveEntry struct {
	header tar.Header
	data   []byte
}

func testTar(t *testing.T, entries []archiveEntry) []byte {
	t.Helper()
	var out bytes.Buffer
	w := tar.NewWriter(&out)
	for _, entry := range entries {
		h := entry.header
		h.Size = int64(len(entry.data))
		if err := w.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(entry.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func testGzip(t *testing.T, raw []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	w := gzip.NewWriter(&out)
	if _, err := w.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func testFiles(t *testing.T) map[string][]byte {
	t.Helper()
	volume := testTar(t, []archiveEntry{{tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0700}, nil}, {tar.Header{Name: "./private.txt", Typeflag: tar.TypeReg, Mode: 0600}, []byte("synthetic fixture only")}})
	result := map[string][]byte{}
	for _, name := range members {
		if strings.HasSuffix(name, ".tar") {
			result[name] = bytes.Clone(volume)
		} else {
			result[name] = []byte("synthetic " + name)
		}
	}
	return result
}

func testBundle(t *testing.T, files map[string][]byte, created int64, mutate func([]archiveEntry) []archiveEntry) []byte {
	t.Helper()
	m := Manifest{Version: 1, CreatedAt: created, Files: map[string]digest{}}
	for name, data := range files {
		hash := sha256.Sum256(data)
		m.Files[name] = digest{SHA256: hex.EncodeToString(hash[:]), Size: int64(len(data))}
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	entries := []archiveEntry{{tar.Header{Name: "manifest.json", Typeflag: tar.TypeReg, Mode: 0600}, raw}}
	for _, name := range members {
		if data, ok := files[name]; ok {
			entries = append(entries, archiveEntry{tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0600}, data})
		}
	}
	if mutate != nil {
		entries = mutate(entries)
	}
	return testTar(t, entries)
}

func testWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func testPrivateDirectory(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestArchivePackVerifyUnpackRoundTrip(t *testing.T) {
	dir := testPrivateDirectory(t)
	input := filepath.Join(dir, "input")
	if err := os.Mkdir(input, 0700); err != nil {
		t.Fatal(err)
	}
	files := testFiles(t)
	for name, data := range files {
		testWrite(t, filepath.Join(input, name), data)
	}
	// Unexpected staging files never become part of the bundle.
	testWrite(t, filepath.Join(input, "unrelated.log"), []byte("retain"))
	output := filepath.Join(dir, "msboost-disaster-20260913T010203Z-0123456789abcdef.tar.gz")
	if err := Pack(input, output); err != nil {
		t.Fatal(err)
	}
	m, err := Verify(output)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Files) != 7 || m.Version != 1 {
		t.Fatal("fixed member contract changed")
	}
	for name, data := range files {
		hash := sha256.Sum256(data)
		if m.Files[name].SHA256 != hex.EncodeToString(hash[:]) || m.Files[name].Size != int64(len(data)) {
			t.Fatal("wrong member digest")
		}
	}
	if _, err := os.Lstat(output + ".partial"); !os.IsNotExist(err) {
		t.Fatal("partial remained after successful publication")
	}
	destination := filepath.Join(dir, "recovered")
	if err := Unpack(output, destination); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(destination)
	if err != nil || len(entries) != 7 {
		t.Fatal("wrong restored member count", err)
	}
	for name, data := range files {
		got, err := os.ReadFile(filepath.Join(destination, name))
		if err != nil || !bytes.Equal(got, data) {
			t.Fatal("roundtrip mismatch", name, err)
		}
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(output)
		if info.Mode().Perm() != 0600 || !ownedByCurrentUser(info) {
			t.Fatal("bundle not private")
		}
		info, _ = os.Stat(destination)
		if info.Mode().Perm() != 0700 || !ownedByCurrentUser(info) {
			t.Fatal("restored directory not private")
		}
		for _, entry := range entries {
			info, _ := entry.Info()
			if info.Mode().Perm() != 0600 || !ownedByCurrentUser(info) {
				t.Fatal("restored member not private")
			}
		}
	}
	before, _ := os.ReadFile(output)
	if Pack(input, output) == nil || Unpack(output, destination) == nil {
		t.Fatal("existing output overwritten")
	}
	after, _ := os.ReadFile(output)
	if !bytes.Equal(before, after) {
		t.Fatal("existing bundle changed")
	}
	if Pack(input, filepath.Join(dir, "unmanaged.tar.gz")) == nil {
		t.Fatal("unmanaged output name accepted")
	}
}

func TestArchiveAtomicPublicationNeverReplaces(t *testing.T) {
	dir := testPrivateDirectory(t)
	partial := filepath.Join(dir, "partial")
	target := filepath.Join(dir, "target")
	testWrite(t, partial, []byte("new"))
	testWrite(t, target, []byte("existing"))
	if publishBundle(partial, target) == nil {
		t.Fatal("atomic publish replaced existing target")
	}
	got, _ := os.ReadFile(target)
	if string(got) != "existing" {
		t.Fatal("target changed")
	}
	if _, err := os.Stat(partial); err != nil {
		t.Fatal("failed publish removed recoverable partial")
	}
	newTarget := filepath.Join(dir, "new-target")
	if err := publishBundle(partial, newTarget); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(partial); !os.IsNotExist(err) {
		t.Fatal("successful publication left partial")
	}
}

func TestArchiveCaddyStickyDirectoriesRoundTrip(t *testing.T) {
	dir := testPrivateDirectory(t)
	input := filepath.Join(dir, "input")
	if err := os.Mkdir(input, 0700); err != nil {
		t.Fatal(err)
	}
	// Official Caddy images create /data/caddy and /config/caddy as 01777.
	// Preserve that directory deletion protection; never chmod a live volume.
	caddy := testTar(t, []archiveEntry{
		{tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0755}, nil},
		{tar.Header{Name: "./caddy/", Typeflag: tar.TypeDir, Mode: 01777}, nil},
		{tar.Header{Name: "./caddy/private.json", Typeflag: tar.TypeReg, Mode: 0600}, []byte("synthetic Caddy data")},
	})
	files := testFiles(t)
	files["caddy_data.tar"], files["caddy_config.tar"] = caddy, caddy
	for name, data := range files {
		testWrite(t, filepath.Join(input, name), data)
	}
	if err := ValidateVolume(filepath.Join(input, "caddy_data.tar")); err != nil {
		t.Fatal("standard Caddy directory rejected", err)
	}
	output := filepath.Join(dir, "msboost-disaster-20260914T010203Z-0123456789abcdef.tar.gz")
	if err := Pack(input, output); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(output); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(dir, "recovered")
	if err := Unpack(output, destination); err != nil {
		t.Fatal(err)
	}
	for name, expected := range files {
		actual, err := os.ReadFile(filepath.Join(destination, name))
		if err != nil || !bytes.Equal(actual, expected) {
			t.Fatal("roundtrip changed member or permissions", name, err)
		}
	}
	restored, err := os.ReadFile(filepath.Join(destination, "caddy_data.tar"))
	if err != nil {
		t.Fatal(err)
	}
	r := tar.NewReader(bytes.NewReader(restored))
	for _, mode := range []int64{0755, 01777, 0600} {
		h, err := r.Next()
		if err != nil || h.Mode != mode {
			t.Fatal("nested archive mode not preserved", err)
		}
	}
}

func TestArchiveRejectsCorruptionExtraDataAndManifestMismatch(t *testing.T) {
	files := testFiles(t)
	now := time.Now().Unix()
	raw := testBundle(t, files, now, nil)
	valid := testGzip(t, raw)
	cases := map[string][]byte{
		"gzip-footer-checksum":     bytes.Clone(valid),
		"gzip-footer-missing":      valid[:len(valid)-8],
		"outer-trailing-garbage":   append(bytes.Clone(valid), []byte("hidden")...),
		"concatenated-gzip":        append(bytes.Clone(valid), testGzip(t, []byte{0})...),
		"inner-trailing-garbage":   testGzip(t, append(bytes.Clone(raw), []byte("hidden")...)),
		"hidden-archive-after-eof": testGzip(t, append(bytes.Clone(raw), raw...)),
	}
	cases["gzip-footer-checksum"][len(valid)-8] ^= 0xff
	mutations := map[string]func([]archiveEntry) []archiveEntry{
		"extra-entry": func(e []archiveEntry) []archiveEntry {
			return append(e, archiveEntry{tar.Header{Name: "extra", Typeflag: tar.TypeReg, Mode: 0600}, []byte("extra")})
		},
		"duplicate-member": func(e []archiveEntry) []archiveEntry { e[2] = e[1]; return e },
		"member-path":      func(e []archiveEntry) []archiveEntry { e[1].header.Name = "../site.env"; return e },
		"member-mode":      func(e []archiveEntry) []archiveEntry { e[1].header.Mode = 04700; return e },
		"member-link": func(e []archiveEntry) []archiveEntry {
			e[1].header.Typeflag = tar.TypeSymlink
			e[1].header.Linkname = "/etc/shadow"
			e[1].data = nil
			return e
		},
		"member-hash":      func(e []archiveEntry) []archiveEntry { e[1].data = bytes.Clone(e[1].data); e[1].data[0] ^= 1; return e },
		"member-size":      func(e []archiveEntry) []archiveEntry { e[1].data = append(bytes.Clone(e[1].data), 'x'); return e },
		"manifest-mode":    func(e []archiveEntry) []archiveEntry { e[0].header.Mode = 0644; return e },
		"manifest-invalid": func(e []archiveEntry) []archiveEntry { e[0].data = []byte(`{}`); return e },
		"manifest-oversize": func(e []archiveEntry) []archiveEntry {
			var m Manifest
			_ = json.Unmarshal(e[0].data, &m)
			d := m.Files["site.env"]
			d.Size = maxBundleSize + 1
			m.Files["site.env"] = d
			e[0].data, _ = json.Marshal(m)
			return e
		},
		"manifest-future": func(e []archiveEntry) []archiveEntry {
			var m Manifest
			_ = json.Unmarshal(e[0].data, &m)
			m.CreatedAt = time.Now().Add(time.Hour).Unix()
			e[0].data, _ = json.Marshal(m)
			return e
		},
	}
	for name, mutate := range mutations {
		cases[name] = testGzip(t, testBundle(t, files, now, mutate))
	}
	missing := testFiles(t)
	delete(missing, "database.dump")
	cases["missing-required-member"] = testGzip(t, testBundle(t, missing, now, nil))
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			dir := testPrivateDirectory(t)
			file := filepath.Join(dir, "input.tar.gz")
			testWrite(t, file, data)
			if _, err := Verify(file); err == nil {
				t.Fatal("malformed archive accepted")
			}
			dest := filepath.Join(dir, "restore")
			if Unpack(file, dest) == nil {
				t.Fatal("malformed archive extracted")
			}
			if _, err := os.Lstat(dest); !os.IsNotExist(err) {
				t.Fatal("verification failure created output")
			}
		})
	}
	// GNU zero-record padding is compatible, not treated as a hidden member.
	file := filepath.Join(testPrivateDirectory(t), "padded.tar.gz")
	testWrite(t, file, testGzip(t, append(bytes.Clone(raw), make([]byte, 4096)...)))
	if _, err := Verify(file); err != nil {
		t.Fatal("normal zero padding rejected", err)
	}
}

func TestArchiveNestedTarSafetyAndVerifyIntegration(t *testing.T) {
	for _, name := range []string{"../outside", "/absolute", "inside/../../outside", "inside/../alias", "C:/drive", "a\\b", "line\nfeed", "."} {
		t.Run("path-"+strings.ReplaceAll(name, "/", "_"), func(t *testing.T) {
			testInvalidNested(t, testTar(t, []archiveEntry{{tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0600}, []byte("x")}}))
		})
	}
	for _, kind := range []byte{tar.TypeSymlink, tar.TypeLink, tar.TypeChar, tar.TypeBlock, tar.TypeFifo} {
		t.Run(fmt.Sprintf("type-%d", kind), func(t *testing.T) {
			testInvalidNested(t, testTar(t, []archiveEntry{{tar.Header{Name: "bad", Typeflag: kind, Linkname: "/outside", Mode: 0600}, nil}}))
		})
	}
	for _, mode := range []int64{04000, 02000, 01000, 01777, 07777, 010000, -1} {
		t.Run(fmt.Sprintf("mode-%d", mode), func(t *testing.T) {
			testInvalidNested(t, testTar(t, []archiveEntry{{tar.Header{Name: "bad", Typeflag: tar.TypeReg, Mode: mode}, nil}}))
		})
	}
	for _, mode := range []int64{02777, 04777, 06777, 07777, 010000, -1} {
		t.Run(fmt.Sprintf("directory-mode-%o", mode), func(t *testing.T) {
			testInvalidNested(t, testTar(t, []archiveEntry{{tar.Header{Name: "bad/", Typeflag: tar.TypeDir, Mode: mode}, nil}}))
		})
	}
	entry := archiveEntry{tar.Header{Name: "same", Typeflag: tar.TypeReg, Mode: 0600}, []byte("x")}
	t.Run("duplicate", func(t *testing.T) {
		other := entry
		other.header.Name = "./same"
		testInvalidNested(t, testTar(t, []archiveEntry{entry, other}))
	})
	t.Run("sparse-extension", func(t *testing.T) {
		sparse := entry
		sparse.header.PAXRecords = map[string]string{"SCHILY.sparse.foo": "true"}
		testInvalidNested(t, testTar(t, []archiveEntry{sparse}))
	})
	t.Run("trailing-archive", func(t *testing.T) { a := testTar(t, []archiveEntry{entry}); testInvalidNested(t, append(a, a...)) })
	t.Run("size-limit-header-only", func(t *testing.T) {
		var buf bytes.Buffer
		w := tar.NewWriter(&buf)
		if err := w.WriteHeader(&tar.Header{Name: "huge", Typeflag: tar.TypeReg, Mode: 0600, Size: maxBundleSize + 1}); err != nil {
			t.Fatal(err)
		}
		testInvalidNested(t, buf.Bytes())
	})
}

func testInvalidNested(t *testing.T, nested []byte) {
	t.Helper()
	dir := testPrivateDirectory(t)
	file := filepath.Join(dir, "volume.tar")
	testWrite(t, file, nested)
	if ValidateVolume(file) == nil {
		t.Fatal("unsafe nested archive accepted")
	}
	for _, name := range []string{"deployment.tar", "app_data.tar", "caddy_data.tar", "caddy_config.tar"} {
		files := testFiles(t)
		files[name] = nested
		bundle := filepath.Join(dir, name+".gz")
		testWrite(t, bundle, testGzip(t, testBundle(t, files, time.Now().Unix(), nil)))
		if _, err := Verify(bundle); err == nil {
			t.Fatalf("hash-valid unsafe %s accepted", name)
		}
	}
}

func TestArchiveRetentionCountsOnlyValidManagedCopies(t *testing.T) {
	dir := testPrivateDirectory(t)
	config := DefaultConfig()
	config.LocalDir = dir
	config.RetentionDays = 2
	managed := func(id int) string {
		return filepath.Join(dir, fmt.Sprintf("msboost-disaster-20260913T010203Z-%016x.tar.gz", id))
	}
	now := time.Now()
	files := testFiles(t)
	for i := 1; i <= 3; i++ {
		testWrite(t, managed(i), testGzip(t, testBundle(t, files, now.AddDate(0, 0, -(10+i)).Unix(), nil)))
	}
	unsafe := testFiles(t)
	unsafe["deployment.tar"] = testTar(t, []archiveEntry{{tar.Header{Name: "bad", Typeflag: tar.TypeSymlink, Linkname: "/outside", Mode: 0600}, nil}})
	testWrite(t, managed(4), testGzip(t, testBundle(t, unsafe, now.Unix(), nil)))
	testWrite(t, managed(5), []byte("broken"))
	unmanaged := filepath.Join(dir, "operator-backup.tar.gz")
	testWrite(t, unmanaged, testGzip(t, testBundle(t, files, now.AddDate(0, 0, -30).Unix(), nil)))
	if err := os.Mkdir(managed(6), 0700); err != nil {
		t.Fatal(err)
	}
	if err := Retain(config); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{managed(1), managed(2), managed(4), managed(5), managed(6), unmanaged} {
		if _, err := os.Lstat(file); err != nil {
			t.Fatal("retention removed protected/unmanaged/invalid item", file, err)
		}
	}
	if _, err := os.Lstat(managed(3)); !os.IsNotExist(err) {
		t.Fatal("old third valid copy not removed")
	}
	if err := Retain(config); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{managed(1), managed(2)} {
		if _, err := Verify(file); err != nil {
			t.Fatal("fewer than two valid copies retained", err)
		}
	}
	config.RetentionDays = 0
	if Retain(config) == nil {
		t.Fatal("invalid retention accepted")
	}
}

func TestArchiveSymlinksAndExclusivePartial(t *testing.T) {
	dir := testPrivateDirectory(t)
	input := filepath.Join(dir, "input")
	if err := os.Mkdir(input, 0700); err != nil {
		t.Fatal(err)
	}
	for name, data := range testFiles(t) {
		testWrite(t, filepath.Join(input, name), data)
	}
	output := filepath.Join(dir, "msboost-disaster-20260913T010203Z-0123456789abcdef.tar.gz")
	testWrite(t, output+".partial", []byte("existing partial"))
	if Pack(input, output) == nil {
		t.Fatal("partial overwritten")
	}
	got, _ := os.ReadFile(output + ".partial")
	if string(got) != "existing partial" {
		t.Fatal("partial data changed")
	}
	target := filepath.Join(dir, "target")
	testWrite(t, target, []byte("synthetic"))
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("symlink fixtures unavailable on this host: " + err.Error())
	}
	if _, err := Verify(link); err == nil {
		t.Fatal("linked bundle accepted")
	}
	if ValidateVolume(link) == nil {
		t.Fatal("linked nested archive accepted")
	}
	if err := os.Remove(filepath.Join(input, "site.env")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(input, "site.env")); err != nil {
		t.Fatal(err)
	}
	if Pack(input, filepath.Join(dir, "msboost-disaster-20260913T010203Z-1111111111111111.tar.gz")) == nil {
		t.Fatal("linked input accepted")
	}
}

func TestConfigRoundTripPermissionsAndAncestorSafety(t *testing.T) {
	dir := testPrivateDirectory(t)
	file := filepath.Join(dir, "config.json")
	config := DefaultConfig()
	config.LocalDir = filepath.Join(dir, "copies")
	if err := SaveConfig(file, config); err != nil {
		t.Fatal(err)
	}
	got, err := ReadConfig(file)
	if err != nil || !reflect.DeepEqual(got, config) {
		t.Fatal("config roundtrip", err)
	}
	config.RetentionDays = 14
	if err := SaveConfig(file, config); err != nil {
		t.Fatal(err)
	}
	got, err = ReadConfig(file)
	if err != nil || got.RetentionDays != 14 {
		t.Fatal("atomic config replacement failed", err)
	}
	if runtime.GOOS != "windows" {
		for _, target := range []string{file, config.LocalDir} {
			info, _ := os.Stat(target)
			want := os.FileMode(0600)
			if target == config.LocalDir {
				want = 0700
			}
			if info.Mode().Perm() != want || !ownedByCurrentUser(info) {
				t.Fatal("config storage not private")
			}
		}
		if err := os.Chmod(file, 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadConfig(file); err == nil {
			t.Fatal("world-readable credential config accepted")
		}
		if err := os.Chmod(file, 0600); err != nil {
			t.Fatal(err)
		}
		ancestor := filepath.Join(dir, "replaceable")
		if err := os.Mkdir(ancestor, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(ancestor, 0777); err != nil {
			t.Fatal(err)
		}
		child := filepath.Join(ancestor, "private")
		if err := os.Mkdir(child, 0700); err != nil {
			t.Fatal(err)
		}
		if privateDir(child, false) == nil {
			t.Fatal("replaceable non-sticky ancestor accepted")
		}
		if err := os.Chmod(ancestor, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(child, 0755); err != nil {
			t.Fatal(err)
		}
		if privateDir(child, false) == nil {
			t.Fatal("nonprivate target directory accepted")
		}
	}
	link := filepath.Join(dir, "linked")
	if err := os.Symlink(config.LocalDir, link); err != nil {
		t.Skip("symlink fixtures unavailable on this host: " + err.Error())
	}
	if privateDir(filepath.Join(link, "child"), true) == nil {
		t.Fatal("symlink ancestor accepted")
	}
	config.LocalDir = filepath.Join(link, "child")
	if SaveConfig(file, config) == nil {
		t.Fatal("config accepted symlink directory")
	}
}

// Reflect avoids importing Unix-only syscall.Stat_t into Windows fixture
// builds, while exercising the real ownership predicates on Linux CI.
type ownershipInfo struct {
	os.FileInfo
	stat any
}

func (i ownershipInfo) Sys() any { return i.stat }

func TestConfigLinuxOwnershipPredicates(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("real Unix ownership checks run on Linux")
	}
	info, err := os.Stat(testPrivateDirectory(t))
	if err != nil {
		t.Fatal(err)
	}
	if !ownedByCurrentUser(info) || !safeAncestor(info) {
		t.Fatal("real owned private directory rejected")
	}
	stat := reflect.ValueOf(info.Sys())
	copy := reflect.New(stat.Elem().Type())
	copy.Elem().Set(stat.Elem())
	uid := copy.Elem().FieldByName("Uid")
	uid.SetUint(uid.Uint() + 10001)
	foreign := ownershipInfo{info, copy.Interface()}
	if ownedByCurrentUser(foreign) || safeAncestor(foreign) {
		t.Fatal("foreign-owned ancestor accepted")
	}
	root := copy.Elem().FieldByName("Uid")
	root.SetUint(0)
	if !safeAncestor(ownershipInfo{info, copy.Interface()}) {
		t.Fatal("protected root-owned ancestor rejected")
	}
}

func TestConfigRejectsInvalidSettingsWithoutWriting(t *testing.T) {
	dir := testPrivateDirectory(t)
	base := DefaultConfig()
	base.LocalDir = filepath.Join(dir, "copies")
	cases := []Config{}
	for _, days := range []int{0, -1, 3651} {
		c := base
		c.RetentionDays = days
		cases = append(cases, c)
	}
	for _, clock := range []string{"2:30", "24:00", "12:60", "02:30\n"} {
		c := base
		c.Time = clock
		cases = append(cases, c)
	}
	for _, name := range []string{"relative", "/", "/root", "/opt", "/tmp", "/opt/msboost", "/opt/msboost/backups"} {
		c := base
		c.LocalDir = name
		cases = append(cases, c)
	}
	c := base
	c.Password = "extra"
	cases = append(cases, c)
	c = base
	c.RemoteHost = "127.0.0.1"
	c.RemotePort = 22
	c.RemoteUser = "root"
	c.RemoteDir = "/root/backups"
	c.Password = "synthetic"
	c.Fingerprint = "SHA256:" + strings.Repeat("A", 43)
	cases = append(cases, c)
	for i, c := range cases {
		file := filepath.Join(dir, fmt.Sprintf("config-%d.json", i))
		if SaveConfig(file, c) == nil {
			t.Fatalf("invalid config %d accepted", i)
		}
		if _, err := os.Lstat(file); !os.IsNotExist(err) {
			t.Fatal("invalid config wrote credentials")
		}
	}
	bad := filepath.Join(dir, "invalid.json")
	testWrite(t, bad, []byte(`{"version":99}`))
	if _, err := ReadConfig(bad); err == nil {
		t.Fatal("invalid saved config accepted")
	}
}

// Do not allocate or write a 64 GiB fixture. The manifest and nested declared
// size tests above exercise their early bounds before any content is copied.
var _ io.Reader = (*bytes.Reader)(nil)
