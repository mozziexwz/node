package control

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

type reconnectTerminal struct {
	file   *os.File
	reader *bufio.Reader
}

func (t *reconnectTerminal) Print(format string, args ...any) { fmt.Fprintf(t.file, format, args...) }
func (t *reconnectTerminal) Read(prompt string, secret bool) (string, error) {
	fmt.Fprint(t.file, prompt)
	if secret {
		original, err := unix.IoctlGetTermios(int(t.file.Fd()), unix.TCGETS)
		if err != nil {
			return "", errors.New("需要真实终端输入密码")
		}
		next := *original
		next.Lflag &^= unix.ECHO
		if unix.IoctlSetTermios(int(t.file.Fd()), unix.TCSETS, &next) != nil {
			return "", errors.New("无法关闭密码回显")
		}
		defer func() { _ = unix.IoctlSetTermios(int(t.file.Fd()), unix.TCSETS, original); fmt.Fprintln(t.file) }()
	}
	line, err := t.reader.ReadString('\n')
	if err != nil {
		return "", errors.New("终端输入已结束")
	}
	line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	if len(line) > 1024 {
		return "", errors.New("输入过长")
	}
	if !secret {
		line = strings.TrimSpace(line)
	}
	return line, nil
}
func RunRelayReconnectWizard(databaseURL, masterKey, publicURL, directory string) error {
	if os.Geteuid() != 0 {
		return errors.New("请在面板服务器以 root 运行恢复向导")
	}
	f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return errors.New("恢复向导需要交互终端")
	}
	defer f.Close()
	if _, err = unix.IoctlGetTermios(int(f.Fd()), unix.TCGETS); err != nil {
		return errors.New("恢复向导需要交互终端")
	}
	return runRelayReconnect(databaseURL, masterKey, publicURL, directory, &reconnectTerminal{file: f, reader: bufio.NewReader(f)})
}
