package executor

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
)

func relayRemote() *fakeRemote {
	return &fakeRemote{run: func(s SSH, script string) ([]byte, error) {
		switch {
		case script == "uname -m\n":
			return []byte("x86_64\n"), nil
		case strings.Contains(script, "MSBOOST_RELAY_PORT"):
			return []byte("MSBOOST_RELAY_PORT=23450\n"), nil
		}
		return []byte("MSBOOST_READY=1\n"), nil
	}}
}

func verifyingEngine(remote *fakeRemote, open TunnelOpener) *Engine {
	return &Engine{Remote: remote, TCPProbe: func(context.Context, string, int) string { return "reachable" },
		GostAMD64URL: "https://github.com/go-gost/gost/releases/download/v3.3.0/gost.tar.gz", GostAMD64SHA256: strings.Repeat("a", 64),
		Verifier: &Verifier{Reach: reachable, Open: open}}
}

func scriptsFor(remote *fakeRemote, host string, parts ...string) int {
	remote.mu.Lock()
	defer remote.mu.Unlock()
	n := 0
	for i, script := range remote.scripts {
		if remote.hosts[i] != host {
			continue
		}
		all := true
		for _, part := range parts {
			all = all && strings.Contains(script, part)
		}
		if all {
			n++
		}
	}
	return n
}

func TestEnforcedVerificationUndoesARefusedRelay(t *testing.T) {
	remote := relayRemote()
	front := testSSH("9.9.9.9")
	var checked TunnelProfile
	e := verifyingEngine(remote, func(_ context.Context, p TunnelProfile) (Tunnel, error) {
		checked = p
		return &fakeTunnel{}, nil // a mieru server without the MSBOOST probe
	})
	meta := &RelayMeta{Source: "code", CodeTail: "2F6H", User: "10086123@qq.com"}
	out := e.Execute(context.Background(), Job{ID: "codejob", Request: Request{Kind: "relay-code", SSH: testSSH("1.1.1.1"), Front: &front, ClientConfig: []byte(sampleConfig), RateMbps: 5, Meta: meta, Verify: &VerifyOptions{Mode: "enforce"}}})
	// The check goes through the entry the user would actually connect to.
	if checked.Host != "9.9.9.9" || checked.Port != 23450 {
		t.Fatalf("verified %s:%d instead of the front entry", checked.Host, checked.Port)
	}
	if out.State != "failed" || out.ErrorCode != "verify_not_msboost" || out.Phase != "verify" || out.FailedRole != "target" || out.Config != nil || out.Hops != nil {
		t.Fatalf("refused relay reported as %+v", out)
	}
	if out.Verify == nil || out.Verify.Code != VerifyNotMSBOOST {
		t.Fatalf("report missing: %+v", out.Verify)
	}
	cleanup := func(id string) string { return "set -e\n" + encodedAssignment("task_id", id) + relayCleanupScript }
	if scriptsFor(remote, "9.9.9.9", cleanup("codejobf")) != 1 || scriptsFor(remote, "1.1.1.1", cleanup("codejobr")) != 1 {
		t.Fatal("refused relay left on a server")
	}
	if scriptsFor(remote, "1.1.1.1", "verify_code") != 0 {
		t.Fatal("a removed relay was marked verified")
	}
}

func TestLoggedVerificationKeepsTheRelayAndNotesTheResult(t *testing.T) {
	for _, c := range []struct {
		tunnel *fakeTunnel
		code   string
	}{{&fakeTunnel{probe: &fakeProbe{}}, VerifyOK}, {&fakeTunnel{}, VerifyNotMSBOOST}} {
		remote := relayRemote()
		e := verifyingEngine(remote, func(context.Context, TunnelProfile) (Tunnel, error) { return c.tunnel, nil })
		meta := &RelayMeta{Source: "code", CodeTail: "2F6H", User: "10086123@qq.com"}
		out := e.Execute(context.Background(), Job{ID: "logjob", Request: Request{Kind: "relay-code", SSH: testSSH("1.1.1.1"), ClientConfig: []byte(sampleConfig), RateMbps: 5, Meta: meta, Verify: &VerifyOptions{Mode: "log"}}})
		if out.State != "succeeded" || len(out.Config) == 0 || out.Verify == nil || out.Verify.Code != c.code {
			t.Fatalf("%s: %+v", c.code, out)
		}
		if scriptsFor(remote, "1.1.1.1", encodedAssignment("task_id", "logjobr"), encodedAssignment("verify_code", c.code), "os.replace") != 1 {
			t.Fatalf("%s: result not noted in meta.json", c.code)
		}
	}
	// Unavailable checks never undo a relay, even when enforcing.
	remote := relayRemote()
	e := verifyingEngine(remote, func(context.Context, TunnelProfile) (Tunnel, error) { return nil, errors.New("noexec /tmp") })
	out := e.Execute(context.Background(), Job{ID: "freejob", Request: Request{Kind: "relay-code", SSH: testSSH("1.1.1.1"), ClientConfig: []byte(sampleConfig), RateMbps: 5, Meta: &RelayMeta{Source: "code", CodeTail: "2F6H", User: "10086123@qq.com"}, Verify: &VerifyOptions{Mode: "enforce"}}})
	if out.State != "succeeded" || out.Verify.Code != VerifyUnavailable {
		t.Fatalf("executor fault blocked the relay: %+v", out)
	}
	// Without a request for it, no report is added: older panels decode strictly.
	plain := verifyingEngine(relayRemote(), func(context.Context, TunnelProfile) (Tunnel, error) {
		t.Fatal("verified without a request")
		return nil, nil
	}).Execute(context.Background(), Job{ID: "plainjob", Request: Request{Kind: "relay", SSH: testSSH("1.1.1.1"), ClientConfig: []byte(sampleConfig)}})
	if plain.State != "succeeded" || plain.Verify != nil {
		t.Fatalf("unrequested report: %+v", plain)
	}
}

func TestRelayVerifyJobNeedsOnlyTheRouteConfiguration(t *testing.T) {
	var checked TunnelProfile
	e := verifyingEngine(&fakeRemote{run: func(SSH, string) ([]byte, error) {
		t.Fatal("a route check must not open SSH")
		return nil, nil
	}}, func(_ context.Context, p TunnelProfile) (Tunnel, error) {
		checked = p
		return &fakeTunnel{probe: &fakeProbe{}}, nil
	})
	request := Request{Kind: "relay-verify", ClientConfig: []byte(sampleConfig), Verify: &VerifyOptions{Mode: "log"}}
	out := e.Execute(context.Background(), Job{ID: "verifyjob", Request: request})
	if out.State != "succeeded" || out.Verify == nil || out.Verify.Code != VerifyOK || out.Config != nil {
		t.Fatalf("route check: %+v", out)
	}
	info, _ := ParseClientConfig([]byte(sampleConfig))
	if checked.Host != info.TargetHost || checked.Port != info.TargetPort {
		t.Fatalf("checked %s:%d", checked.Host, checked.Port)
	}
	for name, change := range map[string]func(*Request){
		"ssh":       func(r *Request) { r.SSH = testSSH("1.1.1.1") },
		"no verify": func(r *Request) { r.Verify = nil },
		"remark":    func(r *Request) { r.Remark = "x" },
		"front":     func(r *Request) { front := testSSH("9.9.9.9"); r.Front = &front },
		"bad mode":  func(r *Request) { r.Verify = &VerifyOptions{Mode: "off"} },
		"no config": func(r *Request) { r.ClientConfig = nil },
	} {
		bad := request
		change(&bad)
		if ValidateRequest(bad) == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	deploy := Request{Kind: "deploy", Mode: "fresh", SSH: testSSH("8.8.8.8"), Verify: &VerifyOptions{Mode: "log"}}
	if ValidateRequest(deploy) == nil {
		t.Fatal("verify options accepted on a deployment")
	}
	relay := Request{Kind: "relay", SSH: testSSH("1.1.1.1"), ClientConfig: []byte(sampleConfig), ReportVerified: true}
	if ValidateRequest(relay) == nil {
		t.Fatal("reportVerified accepted outside relay-inventory")
	}
}

func TestInventoryReportsVerificationOnlyWhenAsked(t *testing.T) {
	entries := []RelayEntry{
		{ID: "ar", Port: 23450, Record: &RelayRecord{Version: 1, Source: "code", User: "a@qq.com", Verified: &RelayVerified{Code: "ok", At: "2026-10-21T03:00:00Z"}}},
		{ID: "br", Port: 23451, Record: &RelayRecord{Version: 1, Source: "code", User: "b@qq.com", Verified: &RelayVerified{Code: "trusted", At: "2026-10-21T03:00:00Z"}}},
		{ID: "cr", Port: 23452, Record: &RelayRecord{Version: 1, Source: "code", User: "c@qq.com", Verified: &RelayVerified{Code: "ok", At: "yesterday"}}},
	}
	for _, entry := range cleanInventory(entries, false) {
		if entry.Record.Verified != nil {
			t.Fatal("verification sent to a panel that did not ask")
		}
	}
	cleaned := cleanInventory(entries, true)
	if cleaned[0].Record.Verified == nil || cleaned[0].Record.Verified.Code != "ok" || cleaned[1].Record.Verified != nil || cleaned[2].Record.Verified != nil {
		t.Fatalf("verification cleaning: %+v %+v %+v", cleaned[0].Record.Verified, cleaned[1].Record.Verified, cleaned[2].Record.Verified)
	}
}

var _ net.Conn = (*net.TCPConn)(nil)
