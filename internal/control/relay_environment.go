package control

import (
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// VerifyLocalRelayEnvironment compares selected Docker Config.Env entries with
// the original root-only env-file supplied to the isolated helper. The caller
// must stream inspect output straight to stdin, never a terminal or log. No DB,
// AEAD, bootstrap, secret-bearing error or expected secret argument is needed.
// The local restore entrypoint enforces Linux root before dispatching here.
func VerifyLocalRelayEnvironment(masterKey, publicURL string, input io.Reader, output io.Writer) error {
	invalid := errors.New("原 server 与受管配置身份不一致或无法安全核对")
	expectedKey, err := hex.DecodeString(masterKey)
	expectedURL := strings.TrimRight(publicURL, "/")
	if err != nil || len(expectedKey) != 32 || !relayRecoveryOrigin(expectedURL) {
		return invalid
	}
	const limit = 256 * 1024
	data, err := io.ReadAll(io.LimitReader(input, limit+1))
	if err != nil || len(data) > limit {
		return invalid
	}
	var entries []string
	if json.Unmarshal(data, &entries) != nil || len(entries) != 2 {
		return invalid
	}
	var actualKey, actualURL string
	seen := map[string]bool{}
	for _, entry := range entries {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || seen[name] {
			return invalid
		}
		seen[name] = true
		switch name {
		case "MASTER_KEY":
			actualKey = value
		case "PUBLIC_URL":
			actualURL = strings.TrimRight(value, "/")
		default:
			return invalid
		}
	}
	decoded, err := hex.DecodeString(actualKey)
	if err != nil || len(decoded) != 32 || subtle.ConstantTimeCompare(expectedKey, decoded) != 1 || !relayRecoveryOrigin(actualURL) || actualURL != expectedURL {
		return invalid
	}
	if _, err = io.WriteString(output, "true\n"); err != nil {
		return errors.New("无法返回受管配置核对结果")
	}
	return nil
}
