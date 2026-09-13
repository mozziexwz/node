package executor

import (
	"errors"
	"os"

	"github.com/pkg/sftp"
)

// SFTPFileUID reads server-side Unix ownership, not the local process UID.
// Callers obtain selfUID from an exclusively created, still-empty file handle
// before sending any private backup bytes. Unknown metadata fails closed.
func SFTPFileUID(info os.FileInfo) (uint32, error) {
	if info == nil {
		return 0, errors.New("SFTP 文件所有者信息缺失")
	}
	stat, ok := info.Sys().(*sftp.FileStat)
	if !ok || stat == nil {
		return 0, errors.New("SFTP 服务器未提供 Unix 所有者信息")
	}
	return stat.UID, nil
}

// A non-writable directory owned by a third account is still unsafe: its owner
// can replace it or change its permissions. Parents must belong to root/self;
// the selected leaf (directory or backup file) must belong to self exactly.
func CheckSFTPOwner(info os.FileInfo, selfUID uint32, leaf bool) error {
	owner, err := SFTPFileUID(info)
	if err != nil {
		return err
	}
	if owner != selfUID && (leaf || owner != 0) {
		return errors.New("SFTP 路径由其他账户所有，拒绝上传或清理")
	}
	return nil
}
