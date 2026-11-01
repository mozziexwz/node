package control

import (
	"github.com/mozziexwz/node/internal/executor"
	"github.com/mozziexwz/node/internal/relayruntime"
)

// UDP on a paid route. A UDP relay answers datagrams from any source address,
// so to a target that answers with more data it would be a reflector; TCP
// cannot be used that way. A new rule therefore runs TCP only and opens UDP
// on every hop once a check has shown its target is an MSBOOST node, whose
// UDP port speaks only Mieru. On a site without checks it stays TCP only.

// relayUDPEligible decides at creation whether a rule may open UDP later.
func relayUDPEligible(s *State, route Route, info executor.ConfigInfo) bool {
	return executor.RelayUDP(info.UDP, info.TargetPort) && relayVerifySettings(s).Donation != "off" && relayAgentsCapable(s, relayRouteAgents(route), relayruntime.UDPForwardCapability)
}

// relayEnableUDP opens UDP on every hop of a rule waiting for it, after a
// passing check. Each hop restarts its GOST once, with its next sync, so a TCP
// connection made before the check is cut once; with enforcement nobody has
// the configuration yet. The downloaded configuration gains its UDP binding.
// A failure leaves the rule waiting for the next passing check.
func (a *App) relayEnableUDP(s *State, rule *UserRule) error {
	if rule.UDP != "pending" || len(rule.Segments) == 0 || rule.SealedConfig == "" || rule.HasFront && !rule.FrontUDP {
		return nil
	}
	agents := make([]string, len(rule.Segments))
	for i, seg := range rule.Segments {
		agents[i] = seg.AgentID
	}
	if !relayAgentsCapable(s, agents, relayruntime.UDPForwardCapability) {
		return nil
	}
	raw, err := a.Open(rule.SealedConfig)
	if err != nil {
		return err
	}
	config, err := executor.WithUDPBinding(raw)
	clear(raw)
	if err != nil {
		return err
	}
	sealed, err := a.Seal(config)
	clear(config)
	if err != nil {
		return err
	}
	rule.SealedConfig, rule.UDP = sealed, "on"
	for i := range rule.Segments {
		rule.Segments[i].Runtime.UDP = true
	}
	if rule.State == "active" {
		rule.State = "pending"
	}
	return nil
}
