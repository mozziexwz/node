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

// A browser disconnect must not cancel an already accepted deployment. The
// persisted rule is its operation ID; credentials stay only in this bounded
// worker's memory and are never replayed after a panel restart.
func (a *App) startRelayFront(rule UserRule, ssh executor.SSH, config []byte) {
	a.relayWorkMu.Lock()
	if a.relayWorkClosed {
		a.relayWorkMu.Unlock()
		clear(config)
		return
	}
	a.relayWorkWG.Add(1)
	a.relayWorkMu.Unlock()
	go func() {
		defer a.relayWorkWG.Done()
		defer clear(config)
		ctx, cancel := context.WithTimeout(a.relayWorkCtx, 5*time.Minute)
		defer cancel()
		key := rule.UserID + ":" + rule.RouteID
		err := a.relayFrontReady(ctx, key)
		attempted := false
		var hop executor.Hop
		if err == nil {
			if value, ok := relayFrontProvisioners.Load(a); ok {
				attempted = true
				hop, err = value.(frontProvisioner)(ctx, rule.UserID, ssh, rule.EntryAddress, rule.EntryPort)
			} else {
				err = errors.New("front executor unavailable")
			}
		}
		ssh.Password = ""
		if err == nil && (hop.FromHost != ssh.Host || hop.FromPort < 1 || hop.FromPort > 65535 || hop.ToHost != rule.EntryAddress || hop.ToPort != rule.EntryPort) {
			err = errors.New("front result mismatch")
		}
		if err == nil {
			var raw []byte
			raw, err = executor.RewriteClientConfig(config, hop.FromHost, hop.FromPort)
			if err == nil {
				var sealed string
				sealed, err = a.Seal(raw)
				clear(raw)
				if err == nil {
					err = a.Store.Update(func(s *State) error {
						current, ok := LoadDoc[UserRule](s, "user_rules", key)
						route, routeOK := LoadDoc[Route](s, "routes", current.RouteID)
						if !ok || current.ID != rule.ID || current.State != "awaiting_front" || !routeOK || !route.Enabled || relayRecoveryRequired(s, current) || !relayEntitled(s.Users[rule.UserID], time.Now().UnixMilli()) || !userCanUseRoute(s.Users[rule.UserID], route) {
							return errors.New("front authorization changed")
						}
						current.SealedConfig, current.HasFront = sealed, true
						current.EntryAddress, current.EntryPort = hop.FromHost, hop.FromPort
						current.State, current.FrontStatus = "pending", "ready"
						return SaveDoc(s, "user_rules", key, current)
					})
				}
			}
		}
		if err != nil {
			_ = a.Store.Update(func(s *State) error {
				current, ok := LoadDoc[UserRule](s, "user_rules", key)
				if !ok || current.ID != rule.ID || current.FrontStatus == "cleaned" {
					return nil
				}
				relayRevoke(&current, time.Now().UnixMilli(), true)
				current.FrontStatus = "not_deployed"
				if attempted && current.FrontTaskID != "" {
					current.FrontStatus = "check_customer_vps"
					if task, found := LoadDoc[Task](s, "tasks", current.FrontTaskID); found && task.Kind == "front" && task.State == "failed" {
						switch task.Phase {
						case "validate", "preflight", "target", "ssh_connect", "ssh_host_key", "ssh_auth", "ssh_handshake", "ssh_session":
							current.FrontStatus = "not_deployed"
						}
					}
				}
				return SaveDoc(s, "user_rules", key, current)
			})
		}
	}()
}

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
				ready = ready && seg.ProtocolVersion == 2 && relaySegmentReady(s, seg, time.Now().UnixMilli())
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
