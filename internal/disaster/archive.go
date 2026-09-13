// Package disaster handles private, explicitly selected whole-site backups.
// Checksums detect corruption; they are not signatures or a trust boundary for
// archives received from an unknown person. No archived code is ever executed.
package disaster

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const maxBundleSize int64 = 64 << 30

var bundleName = regexp.MustCompile(`^msboost-disaster-[0-9]{8}T[0-9]{6}Z-[a-f0-9]{16}\.tar\.gz$`)
var members = []string{"site.env", "deployment.tar", "state.msb", "database.dump", "app_data.tar", "caddy_data.tar", "caddy_config.tar"}

type digest struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}
type Manifest struct {
	Version   int               `json:"version"`
	CreatedAt int64             `json:"createdAt"`
	Files     map[string]digest `json:"files"`
}

func regular(name string) (os.FileInfo, error) {
	i, err := os.Lstat(name)
	if err != nil || !i.Mode().IsRegular() {
		return nil, errors.New("需要普通文件，拒绝符号链接或特殊文件")
	}
	return i, nil
}

// ValidateVolume permits only directories and normal files with contained
// paths. Reject links, devices, FIFOs, sparse tricks, duplicates and bombs
// before GNU tar receives the archive, even on an otherwise trusted backup.
func ValidateVolume(name string) error {
	if _, err := regular(name); err != nil {
		return err
	}
	f, err := os.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	return validateVolumeReader(f)
}

func validateVolumeReader(input io.Reader) error {
	limited := &io.LimitedReader{R: input, N: maxBundleSize + 1}
	r := tar.NewReader(limited)
	seen := map[string]bool{}
	var total int64
	for count := 0; ; count++ {
		h, err := r.Next()
		if err == io.EOF {
			if err := zeroTarPadding(limited); err != nil {
				return err
			}
			if limited.N == 0 {
				return errors.New("数据卷归档超过 64 GiB 上限")
			}
			return nil
		}
		if err != nil {
			return err
		}
		clean := path.Clean(h.Name)
		if count >= 200000 || strings.ContainsAny(h.Name, "\\\x00\r\n:") || path.IsAbs(h.Name) || strings.Contains("/"+h.Name+"/", "/../") || clean == ".." || strings.HasPrefix(clean, "../") || seen[clean] || h.Size < 0 || h.Size > maxBundleSize-total {
			return errors.New("数据卷归档包含危险路径、重复文件或超出大小限制")
		}
		seen[clean] = true
		total += h.Size
		allowedMode := int64(0777)
		if h.Typeflag == tar.TypeDir {
			// Caddy's official image uses 01777 directories. The sticky bit
			// restricts deletion there; unlike setuid/setgid it is not a
			// privilege elevation bit. Preserve it only on directories.
			allowedMode |= 01000
		}
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeDir || clean == "." && h.Typeflag != tar.TypeDir || h.Mode < 0 || h.Mode & ^allowedMode != 0 || h.Typeflag == tar.TypeDir && h.Size != 0 {
			return errors.New("数据卷归档不允许链接、设备或特权权限")
		}
		for key := range h.PAXRecords {
			if strings.Contains(strings.ToLower(key), "sparse") {
				return errors.New("不支持稀疏归档扩展")
			}
		}
		if _, err := io.Copy(io.Discard, r); err != nil {
			return err
		}
	}
}

// GNU tar pads its final 10 KiB record with zeroes. Only this zero padding,
// never hidden entries or arbitrary bytes after the end marker, is accepted.
func zeroTarPadding(input io.Reader) error {
	raw, err := io.ReadAll(io.LimitReader(input, 10*1024+1))
	if err != nil || len(raw) > 10*1024 {
		return errors.New("归档尾部或压缩校验无效")
	}
	for _, b := range raw {
		if b != 0 {
			return errors.New("归档结束标记后存在额外内容")
		}
	}
	return nil
}

func publishBundle(partial, output string) error {
	// Link is an atomic no-replace operation on both Unix and NTFS. Rename
	// would overwrite a destination created after the initial existence check.
	if err := os.Link(partial, output); err != nil {
		return err
	}
	return os.Remove(partial)
}

func Pack(directory, output string) error {
	if !bundleName.MatchString(filepath.Base(output)) {
		return errors.New("整站备份文件名格式无效")
	}
	if err := privateDir(directory, false); err != nil {
		return err
	}
	if err := privateDir(filepath.Dir(output), false); err != nil {
		return err
	}
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		return errors.New("输出文件已存在或不可检查，拒绝覆盖")
	}
	manifest := Manifest{Version: 1, CreatedAt: time.Now().UTC().Unix(), Files: map[string]digest{}}
	var total int64
	for _, name := range members {
		filename := filepath.Join(directory, name)
		info, err := regular(filename)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if info.Size() <= 0 || info.Size() > maxBundleSize-total {
			return errors.New("整站备份缺少内容或超过 64 GiB 上限")
		}
		total += info.Size()
		if strings.HasSuffix(name, ".tar") {
			if err := ValidateVolume(filename); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
		f, err := os.Open(filename)
		if err != nil {
			return err
		}
		hash := sha256.New()
		_, err = io.Copy(hash, f)
		_ = f.Close()
		if err != nil {
			return err
		}
		manifest.Files[name] = digest{hex.EncodeToString(hash.Sum(nil)), info.Size()}
	}
	f, err := os.OpenFile(output+".partial", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	raw, _ := json.Marshal(manifest)
	if err = tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0600, Size: int64(len(raw)), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	if _, err = tw.Write(raw); err != nil {
		return err
	}
	for _, name := range members {
		if err = tw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: manifest.Files[name].Size, Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		in, e := os.Open(filepath.Join(directory, name))
		if e != nil {
			return e
		}
		_, err = io.Copy(tw, in)
		_ = in.Close()
		if err != nil {
			return err
		}
	}
	if err = tw.Close(); err != nil {
		return err
	}
	if err = gz.Close(); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if _, err = Verify(output + ".partial"); err != nil {
		return err
	}
	return publishBundle(output+".partial", output)
}

func Verify(archive string) (Manifest, error) { return readBundle(archive, "") }
func readBundle(archive, destination string) (Manifest, error) {
	var m Manifest
	info, err := regular(archive)
	if err != nil {
		return m, err
	}
	if info.Size() > maxBundleSize {
		return m, errors.New("归档超过 64 GiB 上限")
	}
	f, err := os.Open(archive)
	if err != nil {
		return m, err
	}
	defer f.Close()
	compressed := bufio.NewReader(f)
	gz, err := gzip.NewReader(compressed)
	if err != nil {
		return m, err
	}
	defer gz.Close()
	gz.Multistream(false)
	limited := &io.LimitedReader{R: gz, N: maxBundleSize + 1}
	tr := tar.NewReader(limited)
	header, err := tr.Next()
	if err != nil || header.Name != "manifest.json" || header.Typeflag != tar.TypeReg || header.Mode != 0600 || header.Size > 16384 || header.Size < 1 {
		return m, errors.New("整站归档缺少有效清单")
	}
	raw, err := io.ReadAll(tr)
	if err != nil || json.Unmarshal(raw, &m) != nil || m.Version != 1 || m.CreatedAt <= 0 || m.CreatedAt > time.Now().Add(5*time.Minute).Unix() || len(m.Files) != len(members) {
		return m, errors.New("整站备份清单无效")
	}
	allowed := map[string]bool{}
	var total int64
	for _, name := range members {
		d, ok := m.Files[name]
		if !ok || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(d.SHA256) || d.Size <= 0 || d.Size > maxBundleSize-total {
			return m, errors.New("整站备份清单文件或大小无效")
		}
		total += d.Size
		allowed[name] = true
	}
	for range members {
		h, err := tr.Next()
		if err != nil {
			return m, err
		}
		if !allowed[h.Name] || h.Typeflag != tar.TypeReg || h.Mode != 0600 || h.Size != m.Files[h.Name].Size {
			return m, errors.New("整站归档文件不符合清单")
		}
		delete(allowed, h.Name)
		hash := sha256.New()
		var output *os.File
		var writer io.Writer = hash
		if destination != "" {
			output, err = os.OpenFile(filepath.Join(destination, h.Name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				return m, err
			}
			writer = io.MultiWriter(hash, output)
		}
		if strings.HasSuffix(h.Name, ".tar") {
			err = validateVolumeReader(io.TeeReader(tr, writer))
		} else {
			_, err = io.Copy(writer, tr)
		}
		if output != nil {
			if err == nil {
				err = output.Sync()
			}
			closeErr := output.Close()
			if err == nil {
				err = closeErr
			}
		}
		if err != nil {
			return m, err
		}
		if hex.EncodeToString(hash.Sum(nil)) != m.Files[h.Name].SHA256 {
			return m, errors.New("整站归档 SHA256 不符")
		}
	}
	if _, err = tr.Next(); err != io.EOF {
		return m, errors.New("整站归档包含额外文件")
	}
	// Consume gzip footer to validate its checksum; reject trailing data inside
	// the compressed stream, which tar would otherwise silently ignore.
	if err := zeroTarPadding(limited); err != nil || limited.N == 0 {
		return m, errors.New("整站归档结尾或压缩校验无效")
	}
	if _, err := compressed.ReadByte(); err != io.EOF {
		return m, errors.New("整站归档包含额外压缩成员或尾随数据")
	}
	return m, nil
}

func Unpack(archive, destination string) error {
	if _, err := Verify(archive); err != nil {
		return err
	}
	if err := privateDir(filepath.Dir(destination), false); err != nil {
		return err
	}
	if err := os.Mkdir(destination, 0700); err != nil {
		return errors.New("恢复暂存目录必须不存在，拒绝覆盖")
	}
	_, err := readBundle(archive, destination)
	return err
}

func Retain(config Config) error {
	if err := config.Validate(); err != nil {
		return err
	}
	if err := privateDir(config.LocalDir, false); err != nil {
		return err
	}
	entries, err := os.ReadDir(config.LocalDir)
	if err != nil {
		return err
	}
	type copyInfo struct {
		name    string
		created int64
	}
	var valid []copyInfo
	for _, entry := range entries {
		if !bundleName.MatchString(entry.Name()) || !entry.Type().IsRegular() {
			continue
		}
		m, err := Verify(filepath.Join(config.LocalDir, entry.Name()))
		if err != nil {
			continue
		}
		valid = append(valid, copyInfo{entry.Name(), m.CreatedAt})
	}
	sort.Slice(valid, func(i, j int) bool {
		if valid[i].created == valid[j].created {
			return valid[i].name > valid[j].name
		}
		return valid[i].created > valid[j].created
	})
	cutoff := time.Now().AddDate(0, 0, -config.RetentionDays).Unix()
	for i, copy := range valid {
		if i < 2 || copy.created >= cutoff {
			continue
		}
		name := filepath.Join(config.LocalDir, copy.name)
		// Only checked, managed-named regular files, never a directory or glob.
		if _, err := regular(name); err != nil {
			return err
		}
		if err := os.Remove(name); err != nil {
			return err
		}
	}
	return nil
}
