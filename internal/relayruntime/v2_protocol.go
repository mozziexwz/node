package relayruntime

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
)

const (
	ProtocolV2    = 2
	KeepLast      = "keep_last"
	MaxV2Commands = 32
	MaxV2Traffic  = 128
)

// Capabilities are acknowledged by actual v2 exchanges, not a binary version.
// Keep the v2 protocol baseline separate from optional features: older agents
// must continue syncing their existing rules when a new feature is released.
var V2Capabilities = []string{"keep_last", "explicit_stop", "persistent_config", "traffic_ack"}

const SocksGuardCapability = "socks_guard"

// V2 commands are explicit, idempotent intents for globally unique rule IDs.
// Generation is independent of billing or runtime Version. REVOKE is terminal
// for a RuleID; a new rule must receive a different identity.
type V2Command struct {
	CommandID       string `json:"commandId"`
	RuleID          string `json:"ruleId"`
	Generation      int64  `json:"generation"`
	Action          string `json:"action"`
	RuntimeHash     string `json:"runtimeHash,omitempty"`
	Rule            *Rule  `json:"rule,omitempty"`
	BillingPeriodID string `json:"billingPeriodId,omitempty"`
	Reason          string `json:"reason,omitempty"`
}

type V2Ack struct {
	CommandID   string `json:"commandId"`
	RuleID      string `json:"ruleId"`
	Generation  int64  `json:"generation"`
	RuntimeHash string `json:"runtimeHash,omitempty"`
	State       string `json:"state"` // persisted, ready, failed, stopping, stopped
}

// Each epoch belongs to one immutable billing grant. Cross-month or uncertain
// collection intervals are retained for reconciliation, not assigned a fake
// precise month or charged to a replacement entitlement.
type V2Traffic struct {
	Traffic
	BillingPeriodID string `json:"billingPeriodId"`
	CollectedFrom   int64  `json:"collectedFrom"`
	CollectedUntil  int64  `json:"collectedUntil"`
	Uncertain       bool   `json:"uncertain,omitempty"`
}

type V2TrafficAck struct {
	RuleID   string `json:"ruleId"`
	Epoch    string `json:"epoch"`
	Sequence int64  `json:"sequence"`
}

type V2SyncRequest struct {
	ProtocolVersion           int         `json:"protocolVersion"`
	AgentID                   string      `json:"agentId"`
	AgentInstanceID           string      `json:"agentInstanceId"`
	Sequence                  int64       `json:"sequence"`
	RequestID                 string      `json:"requestId"`
	ControlEpoch              string      `json:"controlEpoch"`
	AppliedRevision           int64       `json:"appliedRevision"`
	Capabilities              []string    `json:"capabilities"`
	Acks                      []V2Ack     `json:"acks"`
	Traffic                   []V2Traffic `json:"traffic"`
	AccountingDegraded        bool        `json:"accountingDegraded,omitempty"`
	localCredentialGeneration int64
}

type V2SyncResponse struct {
	ProtocolVersion  int            `json:"protocolVersion"`
	AgentID          string         `json:"agentId"`
	RequestID        string         `json:"requestId"`
	ControlEpoch     string         `json:"controlEpoch"`
	PreviousRevision int64          `json:"previousRevision"`
	Revision         int64          `json:"revision"`
	Status           string         `json:"status"` // ready or recovery_required; never implicit clear
	OfflinePolicy    string         `json:"offlinePolicy"`
	Commands         []V2Command    `json:"commands"`
	TrafficAcks      []V2TrafficAck `json:"trafficAcks"`
}

// Revision is a monotonic catalogue watermark, not an ACK of all commands.
// Commands can be batched; omissions are always no-ops. Only a matching explicit
// stopped ACK permits control-plane resource release.

// RuntimeHash validates and hashes only effective process configuration. No
// clocks, leases, counters, entitlement revisions or command IDs participate.
// Target order is retained because it matters to fifo; source ACL order is not.
func RuntimeHash(rule Rule) (string, error) {
	files := gostTLSFiles{}
	if rule.Protocol == "tls" {
		if _, err := tls.X509KeyPair([]byte(rule.TLSCertificate), []byte(rule.TLSPrivateKey)); err != nil {
			return "", errors.New("invalid TLS node identity")
		}
		files.certificate, files.key = "certificate", "private-key"
	}
	for _, peer := range rule.TargetTLS {
		pool := x509.NewCertPool()
		if peer.ServerName == "" || !pool.AppendCertsFromPEM([]byte(peer.CA)) {
			return "", errors.New("invalid pinned TLS peer")
		}
		files.cas = append(files.cas, "ca")
	}
	if _, err := gostConfig(rule, "http://127.0.0.1/observer", files); err != nil {
		return "", err
	}
	canonical := struct {
		ListenPort                    int
		Targets, AllowedSources       []string
		Strategy, Protocol            string
		RateMbps                      int64
		TLSCertificate, TLSPrivateKey string
		TargetTLS                     []TLSClient
	}{rule.ListenPort, rule.Targets, append([]string(nil), rule.AllowedSources...), rule.Strategy, rule.Protocol, rule.RateMbps, rule.TLSCertificate, rule.TLSPrivateKey, append([]TLSClient(nil), rule.TargetTLS...)}
	if rule.Protocol != "tls" {
		canonical.TLSCertificate, canonical.TLSPrivateKey = "", ""
	}
	sort.Strings(canonical.AllowedSources)
	raw, err := json.Marshal(canonical)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:]), nil
}
