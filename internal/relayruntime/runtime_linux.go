//go:build linux

package relayruntime

import (
	"os/exec"
	"syscall"
)

// A crashed/killed Agent cannot leave an unleased GOST child forwarding.
func protectChild(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL} }
