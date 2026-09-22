package executor

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestBBRTuningIsLimitedToCustomerVPSTasks(t *testing.T) {
	if strings.Contains(bbrTuneScript, "/etc/sysctl.conf") {
		t.Fatal("BBR tuning must not replace the operator sysctl.conf")
	}
	for _, line := range []string{
		"net.core.default_qdisc=fq",
		"net.ipv4.tcp_congestion_control=bbr",
		"net.ipv4.tcp_fastopen=3",
		"net.ipv4.tcp_timestamps=1",
		"net.ipv4.tcp_sack=1",
		"net.ipv4.tcp_window_scaling=1",
		"net.ipv4.tcp_mtu_probing=1",
		"net.ipv4.tcp_adv_win_scale=1",
		"net.core.somaxconn=4096",
		"net.ipv4.tcp_max_syn_backlog=4096",
		"net.core.netdev_max_backlog=10000",
		"net.ipv4.conf.all.rp_filter=2",
		"net.ipv4.conf.default.rp_filter=2",
	} {
		if strings.Count(bbrTuneScript, line) != 1 {
			t.Fatalf("missing or duplicated BBR setting %q", line)
		}
	}
	for _, want := range []string{"/etc/sysctl.d/99-msboost-bbr.conf", "cmp -s", "sysctl -p", "sysctl --system", "modprobe tcp_bbr"} {
		if !strings.Contains(bbrTuneScript, want) {
			t.Fatalf("BBR script missing %q", want)
		}
	}

	remote := &fakeRemote{run: func(SSH, string) ([]byte, error) {
		return nil, errors.New("stop before mutation")
	}}
	(&Engine{Remote: remote}).Execute(context.Background(), Job{
		ID:      "deploy-bbr",
		Request: Request{Kind: "deploy", Mode: "fresh", SSH: testSSH("8.8.8.8")},
		Script:  testAsset(),
	})
	if len(remote.scripts) != 1 || !strings.Contains(remote.scripts[0], bbrTuneScript) {
		t.Fatal("MSBOOST deployment did not configure BBR on the customer VPS")
	}
	if strings.Index(remote.scripts[0], bbrTuneScript) > strings.Index(remote.scripts[0], `bash "$work/installer.sh"`) {
		t.Fatal("BBR setup must finish before the MSBOOST installer begins")
	}
	if !strings.Contains(relayInstallScript, bbrTuneScript) {
		t.Fatal("customer relay/front installation did not configure BBR")
	}
}
