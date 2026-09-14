package control

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"text/template"
)

func TestVerifyLocalRelayEnvironment(t *testing.T) {
	key := strings.Repeat("ab", 32)
	const origin = "https://panel.example.test"
	encode := func(entries ...string) string { b, _ := json.Marshal(entries); return string(b) }
	for _, tc := range []struct {
		name, expectedKey, expectedURL, input string
		valid                                 bool
	}{
		{"match", key, origin, encode("MASTER_KEY="+key, "PUBLIC_URL="+origin), true},
		{"key-case-and-url-slash", key, origin + "/", encode("PUBLIC_URL="+origin+"/", "MASTER_KEY="+strings.ToUpper(key)), true},
		{"wrong-key", key, origin, encode("MASTER_KEY="+strings.Repeat("cd", 32), "PUBLIC_URL="+origin), false},
		{"wrong-url", key, origin, encode("MASTER_KEY="+key, "PUBLIC_URL=https://other.example.test"), false},
		{"invalid-key", "bad", origin, encode("MASTER_KEY="+key, "PUBLIC_URL="+origin), false},
		{"invalid-actual-key", key, origin, encode("MASTER_KEY=bad", "PUBLIC_URL="+origin), false},
		{"insecure-origin", key, "http://panel.example.test", encode("MASTER_KEY="+key, "PUBLIC_URL=http://panel.example.test"), false},
		{"url-query", key, origin, encode("MASTER_KEY="+key, "PUBLIC_URL="+origin+"?secret=abc"), false},
		{"duplicate-key", key, origin, encode("MASTER_KEY="+key, "MASTER_KEY="+key), false},
		{"duplicate-url", key, origin, encode("PUBLIC_URL="+origin, "PUBLIC_URL="+origin), false},
		{"duplicate-among-three", key, origin, encode("MASTER_KEY="+key, "PUBLIC_URL="+origin, "MASTER_KEY="+key), false},
		{"unknown-env", key, origin, encode("MASTER_KEY="+key, "OTHER="+origin), false},
		{"missing", key, origin, encode("MASTER_KEY=" + key), false},
		{"trailing-json", key, origin, encode("MASTER_KEY="+key, "PUBLIC_URL="+origin) + " []", false},
		{"object", key, origin, `{"MASTER_KEY":"` + key + `"}`, false},
		{"malformed", key, origin, key, false},
		{"oversized", key, origin, strings.Repeat(" ", 256*1024+1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			err := VerifyLocalRelayEnvironment(tc.expectedKey, tc.expectedURL, strings.NewReader(tc.input), &out)
			if tc.valid {
				if err != nil || out.String() != "true\n" {
					t.Fatalf("success contract: err=%v output=%q", err, out.String())
				}
			} else if err == nil || out.Len() != 0 {
				t.Fatalf("mismatch leaked output or succeeded: output length %d", out.Len())
			}
			if err != nil && (strings.Contains(err.Error(), key) || strings.Contains(err.Error(), origin) || strings.Contains(err.Error(), "secret=")) {
				t.Fatal("environment value leaked in error")
			}
		})
	}
	if err := VerifyLocalRelayEnvironment(key, origin, relayEnvFailReader{}, io.Discard); err == nil || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatal("input error exposed")
	}
	if err := VerifyLocalRelayEnvironment(key, origin, strings.NewReader(encode("MASTER_KEY="+key, "PUBLIC_URL="+origin)), relayEnvFailWriter{}); err == nil {
		t.Fatal("ignored writer failure")
	}
}

type relayEnvFailReader struct{}

func (relayEnvFailReader) Read([]byte) (int, error) { return 0, errors.New("PRIVATE upstream read") }

type relayEnvFailWriter struct{}

func (relayEnvFailWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestRelayEnvironmentDockerTemplate(t *testing.T) {
	// Exercise the exact fixed template shipped by the shell wrapper, using
	// Docker's documented json helper and Go template builtins. Short/unrelated
	// env fields must never cause an out-of-range slice or enter the secret pipe.
	source, err := os.ReadFile("../../deploy/relay_recovery.sh")
	if err != nil {
		t.Fatal(err)
	}
	const prefix = "--format '{{- $comma := false"
	start := strings.Index(string(source), prefix)
	if start < 0 {
		t.Fatal("fixed selected-environment template missing")
	}
	fragment := string(source)[start+len("--format '"):]
	end := strings.IndexByte(fragment, '\'')
	if end < 0 {
		t.Fatal("template delimiter missing")
	}
	tmpl, err := template.New("inspect").Funcs(template.FuncMap{"json": func(v any) (string, error) { b, e := json.Marshal(v); return string(b), e }}).Parse(fragment[:end])
	if err != nil {
		t.Fatal(err)
	}
	key := strings.Repeat("ab", 32)
	for _, tc := range []struct {
		name string
		env  []string
		want int
	}{
		{"normal", []string{"X", "PATH=/usr/bin", "MASTER_KEY=" + key, "PUBLIC_URL=https://panel.example.test", "POSTGRES_PASSWORD=not-selected"}, 2},
		{"duplicate-retained-for-rejection", []string{"MASTER_KEY=" + key, "MASTER_KEY=" + key, "PUBLIC_URL=https://panel.example.test"}, 3},
		{"absent", []string{"", "MASTER_KEY_SUFFIX=wrong", "PUBLIC_URL_SUFFIX=wrong"}, 0},
		{"quoted-and-newline", []string{"MASTER_KEY=\"bad\"", "PUBLIC_URL=bad\nvalue"}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			data := struct{ Config struct{ Env []string } }{}
			data.Config.Env = tc.env
			if err := tmpl.Execute(&out, data); err != nil {
				t.Fatal(err)
			}
			var selected []string
			if err := json.Unmarshal(out.Bytes(), &selected); err != nil || len(selected) != tc.want {
				t.Fatalf("selected env count/JSON: %v", err)
			}
			for _, value := range selected {
				if !strings.HasPrefix(value, "MASTER_KEY=") && !strings.HasPrefix(value, "PUBLIC_URL=") {
					t.Fatal("unrelated secret selected")
				}
			}
		})
	}
}
