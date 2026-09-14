//go:build !linux

package relayruntime

import "os/exec"

func protectChild(cmd *exec.Cmd) {}

// Run rejects non-Linux production runtimes before this helper is reachable.
func lockV2StateDir(path string) (func(), error) { return func() {}, nil }
