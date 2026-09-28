//go:build !linux

package control

import "errors"

func RunRelayReconnectWizard(databaseURL, masterKey, publicURL, directory string) error {
	return errors.New("请在 Debian 面板服务器运行恢复向导")
}
