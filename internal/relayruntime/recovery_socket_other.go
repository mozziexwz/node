//go:build !linux

package relayruntime

import (
	"context"
	"errors"
)

// Non-Linux unit tests exercise pure recovery decisions and child lifetimes,
// never expose a substitute local credential endpoint without SO_PEERCRED.
func serveV2Recovery(context.Context, *runtimeState) (func(), error) { return func() {}, nil }

func RunV2RecoveryClient(context.Context, string, string, string) error {
	return errors.New("protected local relay recovery requires Linux root and Unix peer credentials")
}
