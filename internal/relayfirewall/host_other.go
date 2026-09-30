//go:build !linux

package relayfirewall

import (
	"context"
	"errors"
	"io"
)

func newLimitedReader(r io.Reader) io.Reader { return io.LimitReader(r, 256<<10) }
func trustedStatus(string) bool              { return false }
func ProcessStart(int) string                { return "" }
func Run(context.Context, bool) error        { return errors.New("firewall maintenance requires Linux") }
