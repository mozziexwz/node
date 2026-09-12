//go:build !linux

package relayruntime

import "os/exec"

func protectChild(cmd *exec.Cmd) {}
