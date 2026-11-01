package relayfirewall

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type fakeFirewall struct {
	chain       bool
	rules       []string
	linked      bool
	legacyJumps int
	mutations   int
	foreign     string
}

func (f *fakeFirewall) run(_ context.Context, tool string, args ...string) (string, error) {
	if strings.HasSuffix(tool, "-restore") {
		f.chain = true
		f.rules = []string{"-m comment --comment " + Marker + " -j RETURN"}
		f.mutations++
		return "", nil
	}
	if len(args) < 3 || args[0] != "-w" || args[1] != "5" {
		return "", errors.New("missing xtables lock")
	}
	args = args[2:]
	parent, legacyParent := "ufw-after-input", "ufw-user-input"
	if tool == "ip6tables" {
		parent, legacyParent = "ufw6-after-input", "ufw6-user-input"
	}
	if args[0] == "-S" {
		if len(args) == 1 {
			if f.chain {
				raw := "-P INPUT DROP\n-N " + Chain + "\n"
				if f.linked {
					raw += "-A " + parent + " " + strings.Join(jumpRule(), " ") + "\n"
				}
				for i := 0; i < f.legacyJumps; i++ {
					raw += "-A " + legacyParent + " " + strings.Join(jumpRule(), " ") + "\n"
				}
				return raw, nil
			}
			return "-P INPUT DROP\n", nil
		}
		if args[1] == parent {
			return "-N " + parent + "\n", nil
		}
		if args[1] == Chain && f.chain {
			raw := "-N " + Chain + "\n"
			for _, r := range f.rules {
				raw += "-A " + Chain + " " + r + "\n"
			}
			return raw + f.foreign, nil
		}
		return "", errors.New("missing chain")
	}
	if args[0] == "-C" {
		if f.linked {
			return "", nil
		}
		return "", errors.New("missing jump")
	}
	f.mutations++
	if args[1] == legacyParent && args[0] == "-D" && f.legacyJumps > 0 {
		f.legacyJumps--
		return "", nil
	}
	if args[1] == parent {
		f.linked = args[0] == "-A"
		return "", nil
	}
	if args[0] == "-I" {
		f.rules = append([]string{strings.Join(args[3:], " ")}, f.rules...)
		return "", nil
	}
	if args[0] == "-D" {
		s := strings.Join(args[2:], " ")
		for i, r := range f.rules {
			if r == s {
				f.rules = append(f.rules[:i], f.rules[i+1:]...)
				return "", nil
			}
		}
	}
	return "", errors.New("unexpected mutation")
}

func TestLegacyJumpsRelocatedOnlyAfterOwnershipCheck(t *testing.T) {
	for _, tool := range []string{"iptables", "ip6tables"} {
		for _, enabled := range []bool{true, false} {
			f := &fakeFirewall{chain: true, legacyJumps: 2, rules: []string{strings.Join(portRule("tcp", 40897), " "), "-m comment --comment " + Marker + " -j RETURN"}}
			if err := ReconcileFamily(context.Background(), f.run, tool, []int{40897}, nil, enabled); err != nil || f.legacyJumps != 0 || f.linked != enabled {
				t.Fatal(tool, enabled, err, f)
			}
			f.legacyJumps = 1
			f.foreign = "-A " + Chain + " -j ACCEPT\n"
			writes := f.mutations
			if err := ReconcileFamily(context.Background(), f.run, tool, nil, nil, false); err == nil || f.legacyJumps != 1 || f.mutations != writes {
				t.Fatal("foreign chain or reference modified", tool, err, f)
			}
		}
	}
}

func TestFirewallLifecycleAndOwnership(t *testing.T) {
	f := &fakeFirewall{}
	ctx := context.Background()
	if err := ReconcileFamily(ctx, f.run, "iptables", []int{40897, 48702}, nil, true); err != nil {
		t.Fatal(err)
	}
	raw, _ := f.run(ctx, "iptables", "-w", "5", "-S", Chain)
	owned, err := parseOwned(raw)
	if err != nil || !reflect.DeepEqual(sortedPorts(owned["tcp"]), []int{40897, 48702}) || len(owned["udp"]) != 0 || !f.linked {
		t.Fatal(raw, err)
	}
	writes := f.mutations
	if err = ReconcileFamily(ctx, f.run, "iptables", []int{48702, 40897}, nil, true); err != nil || f.mutations != writes {
		t.Fatal("not idempotent", err)
	}
	if err = ReconcileFamily(ctx, f.run, "iptables", []int{48702}, nil, true); err != nil {
		t.Fatal(err)
	}
	for _, r := range f.rules {
		if strings.Contains(r, "40897") {
			t.Fatal("stale port left open")
		}
	}
	// UFW reload removed the jump, but not our owned chain.
	f.linked = false
	if err = ReconcileFamily(ctx, f.run, "iptables", []int{48702}, nil, true); err != nil || !f.linked {
		t.Fatal("reload not repaired", err)
	}
	if err = ReconcileFamily(ctx, f.run, "iptables", nil, nil, false); err != nil || f.linked || len(f.rules) != 1 {
		t.Fatal("cleanup failed", err, f)
	}
	// Same-name external chain is never adopted, changed, or flushed.
	f.foreign = "-A " + Chain + " -p tcp --dport 12345 -j ACCEPT\n"
	writes = f.mutations
	if err = ReconcileFamily(ctx, f.run, "iptables", []int{12345}, nil, true); err == nil || writes != f.mutations {
		t.Fatal("modified foreign rule")
	}
}

func TestFirewallPermitsUDPPortsSeparately(t *testing.T) {
	f := &fakeFirewall{}
	ctx := context.Background()
	if err := ReconcileFamily(ctx, f.run, "iptables", []int{40897}, []int{40897, 51000}, true); err != nil {
		t.Fatal(err)
	}
	raw, _ := f.run(ctx, "iptables", "-w", "5", "-S", Chain)
	owned, err := parseOwned(raw)
	if err != nil || !reflect.DeepEqual(sortedPorts(owned["tcp"]), []int{40897}) || !reflect.DeepEqual(sortedPorts(owned["udp"]), []int{40897, 51000}) {
		t.Fatal(raw, err)
	}
	if !strings.Contains(raw, "-p udp -m udp --dport 51000 ") {
		t.Fatal("UDP rule shape", raw)
	}
	// Closing the UDP port of a rule leaves its TCP permit alone.
	if err = ReconcileFamily(ctx, f.run, "iptables", []int{40897}, []int{40897}, true); err != nil {
		t.Fatal(err)
	}
	raw, _ = f.run(ctx, "iptables", "-w", "5", "-S", Chain)
	if owned, err = parseOwned(raw); err != nil || len(owned["tcp"]) != 1 || len(owned["udp"]) != 1 || owned["udp"][51000] {
		t.Fatal("stale UDP port left open", raw, err)
	}
	if err = ReconcileFamily(ctx, f.run, "iptables", nil, []int{51000}, true); err != nil || !f.linked {
		t.Fatal("a relay with only UDP open still needs the jump", err)
	}
	for _, port := range []int{0, 65536} {
		called := false
		run := func(context.Context, string, ...string) (string, error) { called = true; return "", nil }
		if ReconcileFamily(ctx, run, "iptables", nil, []int{port}, true) == nil || called {
			t.Fatal("invalid UDP port", port)
		}
	}
}

func TestFirewallRejectsInvalidPortsBeforeCommands(t *testing.T) {
	for _, p := range []int{-1, 0, 65536} {
		called := false
		run := func(context.Context, string, ...string) (string, error) { called = true; return "", nil }
		if ReconcileFamily(context.Background(), run, "iptables", []int{p}, nil, true) == nil || called {
			t.Fatal(p)
		}
	}
}

func TestFirewallRejectsUnknownChainShapes(t *testing.T) {
	marker := "-A " + Chain + " -m comment --comment " + Marker + " -j RETURN"
	for _, raw := range []string{"-N " + Chain, "-N " + Chain + "\n" + marker + "\n" + marker,
		"-N " + Chain + "\n-A " + Chain + " -j ACCEPT\n" + marker,
		"-N " + Chain + "\n-A " + Chain + " " + strings.Join(portRule("tcp", 0), " ") + "\n" + marker,
		"-N " + Chain + "\n-A " + Chain + " " + strings.Join(portRule("sctp", 40897), " ") + "\n" + marker,
		"-N " + Chain + "\n-A " + Chain + " -p udp -m tcp --dport 40897 -m comment --comment " + Marker + " -j ACCEPT\n" + marker} {
		if _, err := parseOwned(raw); err == nil {
			t.Fatal("accepted unowned chain", raw)
		}
	}
}
