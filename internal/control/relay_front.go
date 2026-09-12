package control

import (
	"context"
	"errors"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/mozziexwz/node/internal/executor"
)

// Resolve once at provisioning and pin validated public addresses in Agent
// rules. GOST never resolves a user-controlled name into an internal address.
func resolveRelayTarget(ctx context.Context, host string, port int) ([]string, error) {
	if net.ParseIP(host) != nil {
		if err := executor.PublicIP(host); err != nil {
			return nil, err
		}
		return []string{net.JoinHostPort(net.ParseIP(host).String(), strconv.Itoa(port))}, nil
	}
	lookup, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIPAddr(lookup, host)
	if err != nil || len(ips) == 0 {
		return nil, errors.New("目标域名DNS解析失败")
	}
	out := []string{}
	for _, ip := range ips {
		if err := executor.PublicIP(ip.IP.String()); err != nil {
			return nil, errors.New("目标域名解析到了非公网地址")
		}
		if len(out) < 8 {
			out = append(out, net.JoinHostPort(ip.IP.String(), strconv.Itoa(port)))
		}
	}
	return out, nil
}

type frontProvisioner func(context.Context, string, executor.SSH, string, int) (executor.Hop, error)

var relayFrontProvisioners sync.Map

func (a *App) SetFrontProvisioner(fn func(context.Context, string, executor.SSH, string, int) (executor.Hop, error)) {
	relayFrontProvisioners.Store(a, frontProvisioner(fn))
}
func (a *App) relayFrontReady(ctx context.Context, key string) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	wait, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	for {
		ready := false
		err := a.Store.View(func(s *State) error {
			rule, ok := LoadDoc[UserRule](s, "user_rules", key)
			if !ok || rule.State == "failed" || rule.State == "revoking" {
				return errors.New("本站线路准备失败")
			}
			ready = len(rule.Segments) > 0
			for _, seg := range rule.Segments {
				ready = ready && seg.AckState == "ready" && seg.LastLease > time.Now().UnixMilli()
			}
			return nil
		})
		if err != nil {
			return err
		}
		if ready {
			return nil
		}
		select {
		case <-wait.Done():
			return errors.New("等待本站线路绑定超时，已安排撤销")
		case <-ticker.C:
		}
	}
}
