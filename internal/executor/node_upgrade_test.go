package executor

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

// nodeOutput is what nodeConfigScript prints for a node holding config.
func nodeOutput(config string) []byte {
	return []byte("MSBOOST_SERVICE=active\nMSBOOST_PROBE=active\nMSBOOST_CONFIG=" + base64.StdEncoding.EncodeToString([]byte(config)) + "\n")
}

var notInstalled = []byte("MSBOOST_ERROR_CODE=not_installed\nMSBOOST_ERROR_PHASE=node_check\n")

func tcpReachable(context.Context, string, int) string { return "reachable" }

func TestFetchConfigOnlyReadsTheNode(t *testing.T) {
	remote := &fakeRemote{run: func(_ SSH, script string) ([]byte, error) {
		if script != nodeConfigScript {
			t.Fatal("fetching ran another script")
		}
		return nodeOutput(sampleConfig), nil
	}}
	out := (&Engine{Remote: remote, TCPProbe: tcpReachable}).Execute(context.Background(), Job{ID: "fetch", Request: Request{Kind: "fetch-config", SSH: testSSH("8.8.8.8")}})
	if out.State != "succeeded" || string(out.Config) != sampleConfig || len(remote.scripts) != 1 {
		t.Fatalf("configuration not fetched: %+v", out)
	}
	if out.Health == nil || out.Health.Service != "running" || out.Health.PublicTCP != "reachable" || out.Health.LocalSelfTest != "not_tested" || out.Health.BBR != "" {
		t.Fatalf("fetch reported checks it did not run: %+v", out.Health)
	}
	for _, write := range []string{"installer.sh", "systemctl restart", "systemctl start", "systemctl stop", "rm ", "mv ", "cp ", "tee "} {
		if strings.Contains(nodeConfigScript, write) {
			t.Fatalf("read-only script contains %q", write)
		}
	}
	stopped := &fakeRemote{run: func(SSH, string) ([]byte, error) {
		return []byte(strings.Replace(string(nodeOutput(sampleConfig)), "MSBOOST_PROBE=active", "MSBOOST_PROBE=failed", 1)), nil
	}}
	out = (&Engine{Remote: stopped, TCPProbe: tcpReachable}).Execute(context.Background(), Job{Request: Request{Kind: "fetch-config", SSH: testSSH("8.8.8.8")}})
	if out.State != "succeeded" || out.Health.Service != "stopped" {
		t.Fatalf("a stopped service was reported as running: %+v", out)
	}
}

func TestFetchConfigRefusesWhatIsNotThisNode(t *testing.T) {
	missing := &fakeRemote{run: func(SSH, string) ([]byte, error) { return notInstalled, errors.New("exit status 1") }}
	out := (&Engine{Remote: missing, TCPProbe: tcpReachable}).Execute(context.Background(), Job{Request: Request{Kind: "fetch-config", SSH: testSSH("8.8.8.8")}})
	if out.State != "failed" || out.ErrorCode != "not_installed" || out.Phase != "node_check" || len(out.Config) != 0 {
		t.Fatalf("server without a node: %+v", out)
	}
	other := &fakeRemote{run: func(SSH, string) ([]byte, error) {
		return nodeOutput(strings.Replace(sampleConfig, "8.8.8.8", "1.1.1.1", 1)), nil
	}}
	out = (&Engine{Remote: other, TCPProbe: tcpReachable}).Execute(context.Background(), Job{Request: Request{Kind: "fetch-config", SSH: testSSH("8.8.8.8")}})
	if out.State != "failed" || out.ErrorCode != "config_host_mismatch" || len(out.Config) != 0 {
		t.Fatalf("configuration for another entry was handed out: %+v", out)
	}
}

func TestUpgradeKeepsTheRecordedCredentials(t *testing.T) {
	remote := &fakeRemote{run: func(_ SSH, script string) ([]byte, error) {
		switch {
		case script == nodeConfigScript:
			return nodeOutput(sampleConfig), nil
		case script == debianPreflightScript:
			return []byte("MSBOOST_READY=1\n"), nil
		}
		return []byte("MSBOOST_BBR=enabled\nMSBOOST_CONFIG=" + base64.StdEncoding.EncodeToString([]byte(sampleConfig)) + "\n"), nil
	}}
	out := (&Engine{Remote: remote, TCPProbe: tcpReachable}).Execute(context.Background(), Job{ID: "upgrade", Request: Request{Kind: "upgrade", SSH: testSSH("8.8.8.8")}, Script: testAsset()})
	if out.State != "succeeded" || string(out.Config) != sampleConfig || out.Health.LocalSelfTest != "passed" || out.Health.BBR != "enabled" {
		t.Fatalf("upgrade did not finish: %+v", out)
	}
	if len(remote.scripts) != 3 || remote.scripts[0] != nodeConfigScript || remote.scripts[1] != debianPreflightScript {
		t.Fatalf("upgrade did not read the node and check the system first: %d scripts", len(remote.scripts))
	}
	install := remote.scripts[2]
	for _, fresh := range []string{"MSBOOST_FORCE_FRESH", "node_user", "node_pass"} {
		if strings.Contains(install, fresh) {
			t.Fatalf("upgrade replaces the node's credentials: %s", fresh)
		}
	}
	if !strings.Contains(install, "bash \"$work/installer.sh\" >\"$work/private.log\" 2>&1\n") || !strings.Contains(install, "[ -f /etc/msboost/install.env ]") || !strings.Contains(install, encodedAssignment("MSBOOST_SERVER_IP", "8.8.8.8")) || !strings.HasSuffix(install, nodeConfigTail) {
		t.Fatal("upgrade did not rerun the installer for the recorded node")
	}
}

func TestUpgradeNeverInstallsOnAServerWithoutANode(t *testing.T) {
	remote := &fakeRemote{run: func(_ SSH, script string) ([]byte, error) {
		if script != nodeConfigScript {
			t.Fatal("upgrade went past the node check")
		}
		return notInstalled, errors.New("exit status 1")
	}}
	out := (&Engine{Remote: remote, TCPProbe: tcpReachable}).Execute(context.Background(), Job{Request: Request{Kind: "upgrade", SSH: testSSH("8.8.8.8")}, Script: testAsset()})
	if out.State != "failed" || out.ErrorCode != "not_installed" || !strings.Contains(out.Message, "没有做任何改动") {
		t.Fatalf("upgrade of a server without a node: %+v", out)
	}
	tampered := testAsset()
	tampered.Data = append(tampered.Data, 'x')
	untouched := &fakeRemote{run: func(SSH, string) ([]byte, error) { t.Fatal("tampered installer reached the server"); return nil, nil }}
	out = (&Engine{Remote: untouched}).Execute(context.Background(), Job{Request: Request{Kind: "upgrade", SSH: testSSH("8.8.8.8")}, Script: tampered})
	if out.State != "failed" || out.Phase != "integrity" {
		t.Fatalf("tampered installer: %+v", out)
	}
}

func TestUpgradeReportsAChangedConfiguration(t *testing.T) {
	changed := strings.Replace(sampleConfig, "node-secret-not-email", "another-node-secret", 1)
	remote := &fakeRemote{run: func(_ SSH, script string) ([]byte, error) {
		switch {
		case script == nodeConfigScript:
			return nodeOutput(sampleConfig), nil
		case script == debianPreflightScript:
			return []byte("MSBOOST_READY=1\n"), nil
		}
		return []byte("MSBOOST_CONFIG=" + base64.StdEncoding.EncodeToString([]byte(changed)) + "\n"), nil
	}}
	out := (&Engine{Remote: remote, TCPProbe: tcpReachable}).Execute(context.Background(), Job{Request: Request{Kind: "upgrade", SSH: testSSH("8.8.8.8")}, Script: testAsset()})
	if out.State != "failed" || out.ErrorCode != "config_changed" || len(out.Config) != 0 || !strings.Contains(out.NextStep, "取回配置文件") {
		t.Fatalf("changed configuration: %+v", out)
	}
}

func TestUpgradeAndFetchTakeOnlySSH(t *testing.T) {
	for _, request := range []Request{
		{Kind: "upgrade", SSH: testSSH("8.8.8.8"), Mode: "fresh"},
		{Kind: "fetch-config", SSH: testSSH("8.8.8.8"), Remark: "x"},
		{Kind: "fetch-config", SSH: testSSH("8.8.8.8"), ClientConfig: []byte(sampleConfig)},
		{Kind: "upgrade", SSH: testSSH("2606:4700:4700::1111")},
	} {
		if ValidateRequest(request) == nil {
			t.Fatalf("accepted %+v", request)
		}
	}
	for _, kind := range []string{"upgrade", "fetch-config"} {
		if err := ValidateRequest(Request{Kind: kind, SSH: testSSH("8.8.8.8")}); err != nil {
			t.Fatalf("%s rejected: %v", kind, err)
		}
	}
}
