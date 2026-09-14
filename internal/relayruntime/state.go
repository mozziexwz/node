package relayruntime

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
)

const v2StateFile = "relay-v2-state.json"
const maxV2Records = 4096
const maxV2StateBytes = 32 << 20

type v2Record struct {
	Command     V2Command  `json:"command"`
	State       string     `json:"state"`
	LastApplied *V2Command `json:"lastApplied,omitempty"`
}

type v2DiskState struct {
	Schema           int                   `json:"schema"`
	OfflinePolicy    string                `json:"offlinePolicy"`
	ServerURL        string                `json:"serverUrl"`
	AgentID          string                `json:"agentId"`
	ControlEpoch     string                `json:"controlEpoch"`
	Revision         int64                 `json:"revision"`
	RecoveryRequired bool                  `json:"recoveryRequired,omitempty"`
	ManagementToken  string                `json:"managementToken,omitempty"`
	Recovery         *v2RecoveryCheckpoint `json:"recovery,omitempty"`
	Records          map[string]v2Record   `json:"records"`
}

func ensureV2StateDir(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return errors.New("invalid private relay state directory")
	}
	// Reject links in every existing component before MkdirAll follows anything.
	for at := abs; ; at = filepath.Dir(at) {
		info, e := os.Lstat(at)
		if e == nil && (info.Mode()&os.ModeSymlink != 0 || !info.IsDir()) {
			return errors.New("relay state path must not contain links")
		}
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			return errors.New("cannot inspect relay state directory")
		}
		if at == filepath.Dir(at) {
			break
		}
	}
	if err = os.MkdirAll(abs, 0700); err != nil {
		return errors.New("cannot create private relay state directory")
	}
	return checkV2PrivatePath(abs, true)
}

func checkV2PrivatePath(path string, directory bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || (directory && !info.IsDir()) || (!directory && !info.Mode().IsRegular()) {
		return errors.New("relay state must be a private regular file or directory")
	}
	if runtime.GOOS != "windows" {
		if info.Mode().Perm()&0077 != 0 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
			return errors.New("relay state permissions must be private")
		}
		// Linux production ownership without importing a platform-only Stat_t
		// shape into this file; Windows is used only for local helper tests.
		stat := reflect.ValueOf(info.Sys())
		if stat.Kind() == reflect.Pointer {
			stat = stat.Elem()
		}
		if stat.Kind() != reflect.Struct {
			return errors.New("cannot establish relay state ownership")
		}
		uid := stat.FieldByName("Uid")
		if !uid.IsValid() || uid.Uint() != uint64(os.Geteuid()) {
			return errors.New("relay state owner mismatch")
		}
		if !directory {
			links := stat.FieldByName("Nlink")
			if !links.IsValid() || links.Uint() != 1 {
				return errors.New("relay state must not be hard linked")
			}
		}
	}
	return nil
}

func readV2PrivateJSON(path string, out any) error {
	if err := checkV2PrivatePath(path, false); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxV2StateBytes+1))
	if err != nil || len(raw) > maxV2StateBytes {
		return errors.New("relay state exceeds safe bounds")
	}
	return strictV2JSON(raw, out)
}

func strictV2JSON(raw []byte, out any) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return errors.New("missing v2 object")
	}
	// encoding/json otherwise accepts duplicate fields by keeping the last
	// value, which is not a well-defined authenticated command envelope.
	keys := json.NewDecoder(bytes.NewReader(raw))
	keys.UseNumber()
	if err := uniqueV2JSON(keys, 0); err != nil {
		return errors.New("ambiguous v2 JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return errors.New("invalid v2 JSON")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("extra v2 JSON data")
	}
	return nil
}

func uniqueV2JSON(decoder *json.Decoder, depth int) error {
	if depth > 64 {
		return errors.New("JSON nesting exceeds bound")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, nested := token.(json.Delim)
	if !nested {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			// Struct field matching in encoding/json is case-insensitive. Reject
			// case variants as duplicates too instead of silently taking the last.
			folded := strings.ToLower(name)
			if !ok || seen[folded] {
				return errors.New("duplicate JSON field")
			}
			seen[folded] = true
			if err := uniqueV2JSON(decoder, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := uniqueV2JSON(decoder, depth+1); err != nil {
				return err
			}
		}
	default:
		return errors.New("invalid JSON delimiter")
	}
	_, err = decoder.Token()
	return err
}

// writeV2PrivateJSON syncs both the file and parent directory on Linux. Failure
// is reported without granting permission to alter running processes.
func writeV2PrivateJSON(path string, value any) error {
	if err := checkV2PrivatePath(filepath.Dir(path), true); err != nil {
		return err
	}
	if err := checkV2PrivatePath(path, false); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	raw, err := json.Marshal(value)
	if err != nil || len(raw) > maxV2StateBytes {
		return errors.New("invalid or oversized relay state")
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".relay-v2-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(raw)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		dir, err := os.Open(filepath.Dir(path))
		if err != nil {
			return err
		}
		defer dir.Close()
		if err = dir.Sync(); err != nil {
			return err
		}
	}
	return nil
}

func loadV2State(cfg Config, agentID string) (v2DiskState, error) {
	state := v2DiskState{Schema: ProtocolV2, OfflinePolicy: KeepLast, ServerURL: strings.TrimRight(cfg.ServerURL, "/"), AgentID: agentID, Records: map[string]v2Record{}}
	var saved v2DiskState
	err := readV2PrivateJSON(filepath.Join(cfg.StateDir, v2StateFile), &saved)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, errors.New("cannot read trusted v2 relay state; manual recovery required")
	}
	state = saved
	if state.Schema != ProtocolV2 || state.OfflinePolicy != KeepLast || state.ServerURL != strings.TrimRight(cfg.ServerURL, "/") || state.AgentID != agentID || state.Revision < 0 || state.Records == nil || len(state.Records) > maxV2Records || (!validV2ID(state.ControlEpoch) && !(state.ControlEpoch == "" && len(state.Records) == 0)) {
		return state, errors.New("v2 relay state identity or schema mismatch")
	}
	if err := validateV2RecoveryCheckpoint(state); err != nil {
		return state, err
	}
	seenCommands := map[string]bool{}
	for id, record := range state.Records {
		if id != record.Command.RuleID || validateV2Command(record.Command) != nil {
			return state, errors.New("invalid cached v2 command")
		}
		if seenCommands[record.Command.CommandID] {
			return state, errors.New("duplicate cached v2 command identity")
		}
		seenCommands[record.Command.CommandID] = true
		if record.State != "persisted" && record.State != "ready" && record.State != "failed" && record.State != "stopping" && record.State != "stopped" {
			return state, errors.New("invalid cached command status")
		}
		if record.LastApplied != nil && (validateV2Command(*record.LastApplied) != nil || record.LastApplied.RuleID != id || !v2RunningAction(record.LastApplied.Action)) {
			return state, errors.New("invalid last applied configuration")
		}
		if record.LastApplied != nil && record.LastApplied.Generation > record.Command.Generation {
			return state, errors.New("cached configuration generation rollback")
		}
		if record.LastApplied != nil && record.LastApplied.Generation == record.Command.Generation && !sameV2Command(record.Command, *record.LastApplied) {
			return state, errors.New("conflicting cached configuration generation")
		}
		if record.State == "ready" && (record.LastApplied == nil || !sameV2Command(record.Command, *record.LastApplied)) {
			return state, errors.New("cached ready status does not match applied configuration")
		}
		if record.State == "stopped" && v2RunningAction(record.Command.Action) {
			return state, errors.New("cached stop status has no stop command")
		}
	}
	return state, nil
}
