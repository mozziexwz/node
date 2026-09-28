package control

import (
	"archive/tar"
	"errors"
	"os"
	"path/filepath"
)

// ExportOnlineBackup does not acquire the stop-the-site admission gate. The
// single control_state row contains business records AND attachment bytes;
// ExportPostgresBackup reads it with a repeatable-read transaction. Tasks and
// nodes can continue while this immutable snapshot is encrypted and packed.
func ExportOnlineBackup(databaseURL, key, keyFile, directory string) error {
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return errors.New("在线备份需要已创建的私有暂存目录")
	}
	f, err := os.OpenFile(filepath.Join(directory, "state.msb"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return errors.New("无法创建业务快照文件")
	}
	defer f.Close()
	if err = ExportPostgresBackup(databaseURL, key, keyFile, f); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return errors.New("业务快照写入磁盘失败")
	}
	if err = f.Close(); err != nil {
		return err
	}
	var raw []byte
	if keyFile != "" {
		raw, err = readRegularLimited(keyFile, 32)
	} else {
		raw, err = masterKey(Config{MasterKey: key})
	}
	if err != nil || len(raw) != 32 {
		return errors.New("无法保存原站点密钥")
	}
	defer clear(raw)
	return writeOnlineRecoveryKey(filepath.Join(directory, "app_data.tar"), raw)
}

func writeOnlineRecoveryKey(filename string, key []byte) error {
	f, err := os.OpenFile(filename, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	w := tar.NewWriter(f)
	// Preserve the service user's ownership even on a newly created Docker
	// volume. History files and runtime locks must not be copied into a new site.
	for _, h := range []*tar.Header{
		{Name: ".", Typeflag: tar.TypeDir, Mode: 0700, Uid: 10001, Gid: 10001},
		{Name: "master.key", Typeflag: tar.TypeReg, Mode: 0600, Uid: 10001, Gid: 10001, Size: int64(len(key))},
	} {
		if err = w.WriteHeader(h); err != nil {
			return err
		}
	}
	if _, err = w.Write(key); err != nil {
		return err
	}
	if err = w.Close(); err != nil {
		return err
	}
	return f.Sync()
}
