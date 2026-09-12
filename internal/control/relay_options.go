package control

import (
	"errors"
	"github.com/mozziexwz/node/internal/executor"
	"net"
	"strings"
)

func relayNodeAddresses(node RelayAgent) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, address := range append([]string{node.Address}, node.Addresses...) {
		if address != "" && !seen[address] {
			out = append(out, address)
			seen[address] = true
		}
	}
	return out
}
func relayNodeHasAddress(node RelayAgent, address string) bool {
	for _, known := range relayNodeAddresses(node) {
		if known == address {
			return true
		}
	}
	return false
}
func normalizeRelayAddresses(node *RelayAgent) error {
	if len(node.Addresses) > 16 {
		return errors.New("节点最多登记16个公网地址")
	}
	addresses := relayNodeAddresses(*node)
	if len(addresses) > 16 {
		return errors.New("节点最多登记16个公网地址（含主地址）")
	}
	for i, address := range addresses {
		if net.ParseIP(address) == nil {
			return errors.New("节点连接地址必须是公网IP")
		}
		if err := executor.PublicIP(address); err != nil {
			return err
		}
		addresses[i] = net.ParseIP(address).String()
	}
	if len(addresses) == 0 {
		return errors.New("节点缺少公网地址")
	}
	node.Address, node.Addresses = addresses[0], addresses
	return nil
}
func relayConnectionAddress(node RelayAgent, preference, explicit string) (string, error) {
	if explicit != "" {
		if !relayNodeHasAddress(node, explicit) {
			return "", errors.New("连接IP未登记到此节点")
		}
		return explicit, nil
	}
	for _, address := range relayNodeAddresses(node) {
		ip := net.ParseIP(address)
		if ip == nil {
			continue
		}
		if preference == "" || preference == "auto" || preference == "ipv4" && ip.To4() != nil || preference == "ipv6" && ip.To4() == nil {
			return address, nil
		}
	}
	return "", errors.New("节点没有符合连接地址偏好的已登记IP")
}
func normalizeRelayRoute(s *State, route *Route) error {
	if route.Type == "" {
		route.Type = "tunnel"
		if len(route.Hops) == 0 && len(route.Exit.AgentIDs) == 1 && route.Exit.AgentIDs[0] == route.EntryAgentID {
			route.Type = "port_forward"
		}
	}
	if route.Type != "port_forward" && route.Type != "tunnel" {
		return errors.New("隧道类型无效")
	}
	if route.Type == "port_forward" {
		if len(route.Hops) > 0 {
			return errors.New("端口转发不包含中间跳")
		}
		route.Exit = RouteStage{}
	}
	if route.TrafficMode == "" {
		route.TrafficMode = "both"
	}
	if route.TrafficMode != "both" && route.TrafficMode != "upload" && route.TrafficMode != "download" {
		return errors.New("流量计算须为双向、仅上传或仅下载")
	}
	if route.TrafficMultiplierPermille == 0 {
		route.TrafficMultiplierPermille = 1000
	}
	if route.TrafficMultiplierPermille < 1 || route.TrafficMultiplierPermille > 100000 {
		return errors.New("流量倍率须为0.001至100，最多三位小数")
	}
	if route.AddressPreference == "" {
		route.AddressPreference = "auto"
	}
	if route.AddressPreference != "auto" && route.AddressPreference != "ipv4" && route.AddressPreference != "ipv6" {
		return errors.New("地址偏好无效")
	}
	node, ok := LoadDoc[RelayAgent](s, "relay_agents", route.EntryAgentID)
	if !ok || node.Capability != "relay" {
		return errors.New("入口节点不存在或能力不符")
	}
	if len(route.EntryAddresses) > 16 {
		return errors.New("最多配置16个入口地址")
	}
	if route.EntryAuto || len(route.EntryAddresses) == 0 && route.EntryAddress == "" {
		route.EntryAuto = true
		route.EntryAddresses = relayNodeAddresses(node)
	}
	if len(route.EntryAddresses) == 0 && route.EntryAddress != "" {
		route.EntryAddresses = []string{route.EntryAddress}
	}
	for i, address := range route.EntryAddresses {
		route.EntryAddresses[i] = strings.TrimSpace(address)
		if err := relayAddress(route.EntryAddresses[i]); err != nil {
			return err
		}
	}
	if len(route.EntryAddresses) == 0 {
		route.EntryAddresses = relayNodeAddresses(node)
	}
	route.EntryAddress = route.EntryAddresses[0]
	// A client config contains one server. Prefer the requested family among
	// configured entrance IPs; a deliberately provided domain stays a domain.
	for _, address := range route.EntryAddresses {
		ip := net.ParseIP(address)
		if ip == nil || route.AddressPreference == "auto" || route.AddressPreference == "ipv4" && ip.To4() != nil || route.AddressPreference == "ipv6" && ip.To4() == nil {
			route.EntryAddress = address
			return nil
		}
	}
	return errors.New("入口没有符合地址偏好的IP，请登记相应地址或选择自动")
}
