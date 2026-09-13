package executor

import (
	"github.com/pkg/sftp"
	"os"
	"testing"
)

type sftpOwnerInfo struct {
	os.FileInfo
	stat any
}

func (i sftpOwnerInfo) Sys() any { return i.stat }

func TestSFTPOwnershipRequiresRootOrSelfAncestorsAndSelfLeaf(t *testing.T) {
	for _, test := range []struct {
		owner, self uint32
		leaf, ok    bool
	}{
		{0, 0, false, true}, {1002, 0, false, false}, {0, 0, true, true}, {1002, 0, true, false},
		{0, 1001, false, true}, {1001, 1001, false, true}, {1002, 1001, false, false},
		{1001, 1001, true, true}, {0, 1001, true, false}, {1002, 1001, true, false},
	} {
		info := sftpOwnerInfo{stat: &sftp.FileStat{UID: test.owner}}
		if (CheckSFTPOwner(info, test.self, test.leaf) == nil) != test.ok {
			t.Errorf("owner %d self %d leaf %v", test.owner, test.self, test.leaf)
		}
	}
	if _, err := SFTPFileUID(nil); err == nil {
		t.Fatal("nil info trusted")
	}
	if _, err := SFTPFileUID(sftpOwnerInfo{stat: nil}); err == nil {
		t.Fatal("missing metadata trusted")
	}
}
