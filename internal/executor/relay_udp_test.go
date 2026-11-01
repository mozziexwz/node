package executor

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRelayUDPOnlyForUDPConfigsAboveWellKnownPorts(t *testing.T) {
	for _, c := range []struct {
		udp  bool
		port int
		want bool
	}{{true, 20067, true}, {true, 1024, true}, {true, 1023, false}, {true, 53, false}, {false, 20067, false}} {
		if got := RelayUDP(c.udp, c.port); got != c.want {
			t.Errorf("RelayUDP(%v, %d) = %v", c.udp, c.port, got)
		}
	}
}

func TestRelayInstallWritesTheUDPServiceOnlyWhenAsked(t *testing.T) {
	python := python3Path(t)
	for _, start := range []string{`ports=$(python3 - "$udp"`, `if python3 - "$pid" "$port" "$udp"`} {
		code := heredocPython(relayInstallScript, start)
		cmd := exec.Command(python, "-I", "-c", "import sys; compile(sys.stdin.read(), 'relay', 'exec')")
		cmd.Stdin = strings.NewReader(code)
		if code == "" {
			t.Fatalf("python after %s missing", start)
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("python after %s: %v %s", start, err, out)
		}
	}
	writer := heredocPython(relayInstallScript, `python3 - "$port" "$backend_port"`)
	for _, c := range []struct {
		udp, rate          string
		services, limiters int
	}{{"1", "625000", 2, 2}, {"0", "625000", 1, 1}, {"1", "0", 2, 0}} {
		dir := t.TempDir()
		cmd := exec.Command(python, "-I", "-", "34567", "40001", "203.0.113.7", "20067", filepath.Join(dir, "config.json"), filepath.Join(dir, "guard.json"), c.rate, c.udp)
		cmd.Stdin = strings.NewReader(writer)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("config writer: %v %s", err, out)
		}
		raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
		if err != nil {
			t.Fatal(err)
		}
		var config struct {
			Services []map[string]any `json:"services"`
			Limiters []map[string]any `json:"limiters"`
		}
		if err := json.Unmarshal(raw, &config); err != nil {
			t.Fatal(err)
		}
		if len(config.Services) != c.services || len(config.Limiters) != c.limiters {
			t.Fatalf("udp=%s rate=%s: %s", c.udp, c.rate, raw)
		}
		if c.udp != "1" {
			continue
		}
		udp := config.Services[1]
		listener := udp["listener"].(map[string]any)
		nodes := udp["forwarder"].(map[string]any)["nodes"].([]any)
		if udp["addr"] != ":34567" || udp["handler"].(map[string]any)["type"] != "udp" || listener["type"] != "udp" || listener["metadata"].(map[string]any)["keepalive"] != true || nodes[0].(map[string]any)["addr"] != "203.0.113.7:20067" {
			t.Fatalf("UDP service must listen publicly on the relay port with keepalive: %s", raw)
		}
		if _, admitted := udp["admission"]; admitted {
			t.Fatal("the public UDP entry has no source restriction")
		}
		if c.rate != "0" && (udp["limiter"] != "per-port-udp" || config.Services[0]["limiter"] != "per-port") {
			t.Fatalf("TCP and UDP need separate limiters: %s", raw)
		}
	}
}

func TestRelayCleanupRemovesOwnedUDPRules(t *testing.T) {
	for _, owned := range []string{"ufw-udp-owned", "firewalld-${mode}-udp-owned", `--remove-port="$port/$protocol"`, `ufw --force delete allow "$port/$protocol"`} {
		if !strings.Contains(relayCleanupScript, owned) {
			t.Fatalf("cleanup misses %s", owned)
		}
	}
	for _, owned := range []string{`printf '%s\n' "$port" > "$confdir/ufw-udp-owned"`, `"$confdir/firewalld-runtime-udp-owned"`, `"$confdir/firewalld-permanent-udp-owned"`, `ufw --force delete allow "$port/udp"`} {
		if !strings.Contains(relayInstallScript, owned) {
			t.Fatalf("install misses %s", owned)
		}
	}
}
