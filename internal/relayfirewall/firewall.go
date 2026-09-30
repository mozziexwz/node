// Package relayfirewall maintains a separately owned UFW child chain. It never
// accepts shell commands, addresses, ports or file paths from the control plane.
package relayfirewall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	Chain      = "MSBOOST-RELAY"
	Marker     = "msboost-relay-firewall-v1"
	StatusPath = "/run/msboost-relay-firewall/status.json"
)

type Status struct {
	PID        int    `json:"pid"`
	Start      string `json:"start"`
	ObservedAt int64  `json:"observedAt"`
	Ports      []int  `json:"ports"`
	Backend    string `json:"backend"`
	Error      string `json:"error,omitempty"`
}

type Runner func(context.Context, string, ...string) (string, error)

func portRule(port int) []string {
	return []string{"-p", "tcp", "-m", "tcp", "--dport", strconv.Itoa(port), "-m", "comment", "--comment", Marker, "-j", "ACCEPT"}
}

func jumpRule() []string { return []string{"-m", "comment", "--comment", Marker, "-j", Chain} }

// The final marker and EVERY rule must have exactly our shape. Never flush or
// adopt an existing same-name chain containing an administrator's rules.
func parseOwned(raw string) (map[int]bool, error) {
	lines := strings.Split(strings.TrimSpace(strings.ReplaceAll(raw, "\"", "")), "\n")
	if len(lines) < 2 || lines[0] != "-N "+Chain || lines[len(lines)-1] != "-A "+Chain+" -m comment --comment "+Marker+" -j RETURN" {
		return nil, errors.New("firewall chain ownership mismatch")
	}
	ports := map[int]bool{}
	for _, line := range lines[1 : len(lines)-1] {
		words := strings.Fields(line)
		if len(words) != 14 {
			return nil, errors.New("unknown firewall rule")
		}
		port, err := strconv.Atoi(words[7])
		if err != nil || port < 1 || port > 65535 || ports[port] || line != "-A "+Chain+" "+strings.Join(portRule(port), " ") {
			return nil, errors.New("unknown firewall rule")
		}
		ports[port] = true
	}
	return ports, nil
}

func sortedPorts(ports map[int]bool) []int {
	out := make([]int, 0, len(ports))
	for port := range ports {
		out = append(out, port)
	}
	sort.Ints(out)
	return out
}

// ReconcileFamily changes only this chain and its tagged jump at the END of
// ufw-user-input. Explicit administrator deny rules therefore retain priority.
// Rechecking every cycle also repairs a UFW reload without restarting Relay.
func ReconcileFamily(ctx context.Context, run Runner, tool string, desired []int, enabled bool) error {
	parent := "ufw-user-input"
	if tool == "ip6tables" {
		parent = "ufw6-user-input"
	}
	wanted := map[int]bool{}
	for _, port := range desired {
		if port < 1 || port > 65535 {
			return errors.New("invalid listen port")
		}
		wanted[port] = true
	}
	ip := func(args ...string) (string, error) { return run(ctx, tool, append([]string{"-w", "5"}, args...)...) }
	raw, err := ip("-S")
	if err != nil {
		return err
	}
	hasChain := strings.Contains("\n"+raw+"\n", "\n-N "+Chain+"\n")
	if !hasChain && (!enabled || len(wanted) == 0) {
		return nil
	}
	if enabled {
		if _, err = ip("-S", parent); err != nil {
			return errors.New("active UFW input chain unavailable")
		}
	}
	if !hasChain {
		// Marker creation is done in the same transaction as chain creation.
		input := "*filter\n:" + Chain + " - [0:0]\n-A " + Chain + " -m comment --comment " + Marker + " -j RETURN\nCOMMIT\n"
		if _, err = run(ctx, tool+"-restore", input); err != nil {
			return err
		}
	}
	raw, err = ip("-S", Chain)
	if err != nil {
		return err
	}
	owned, err := parseOwned(raw)
	if err != nil {
		return err
	}
	if !enabled {
		wanted = map[int]bool{}
	}
	// Remove stale permissions before adding new ones; never change policies,
	// unrelated chains, UFW files, SSH rules, or administrator-owned permits.
	for _, port := range sortedPorts(owned) {
		if !wanted[port] {
			if _, err = ip(append([]string{"-D", Chain}, portRule(port)...)...); err != nil {
				return err
			}
		}
	}
	for _, port := range sortedPorts(wanted) {
		if !owned[port] {
			if _, err = ip(append([]string{"-I", Chain, "1"}, portRule(port)...)...); err != nil {
				return err
			}
		}
	}
	jump := jumpRule()
	_, linked := ip(append([]string{"-C", parent}, jump...)...)
	if len(wanted) > 0 {
		if linked != nil {
			_, err = ip(append([]string{"-A", parent}, jump...)...)
			return err
		}
		return nil
	}
	if linked == nil {
		if _, err = ip(append([]string{"-D", parent}, jump...)...); err != nil {
			return err
		}
	}
	// Leave the empty tagged chain in place. Unknown external references must
	// not turn an otherwise successful close into destructive chain handling.
	return nil
}

// Check is a local firewall readiness observation, NOT public reachability.
// Missing/stale/helper-failed evidence can never be reported as ready.
func Check(path string, pid int, start string, port int, now time.Time) string {
	if path == "" {
		return ""
	} // Isolated callers opt in; the installer always enables it.
	if !trustedStatus(path) {
		return "pending"
	}
	if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() {
		return "pending"
	}
	f, err := os.Open(path)
	if err != nil {
		return "pending"
	}
	defer f.Close()
	var s Status
	if err = json.NewDecoder(newLimitedReader(f)).Decode(&s); err != nil {
		return "error"
	}
	if s.PID != pid || start == "" || s.Start != start || s.ObservedAt > now.UnixMilli()+1000 || s.ObservedAt < now.Add(-15*time.Second).UnixMilli() {
		return "pending"
	}
	if s.Error != "" {
		return "error"
	}
	for _, p := range s.Ports {
		if p == port {
			return "ready"
		}
	}
	return "pending"
}

func errorStatus(err error) string {
	if err != nil {
		return "firewall_maintenance_failed"
	}
	return ""
}
func annotate(err error, operation string) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
}
