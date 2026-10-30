// Package executor performs only fixed, authenticated, short-lived customer VPS tasks.
package executor

import (
	"context"
	"encoding/json"
)

type SSH struct {
	Host string `json:"host"`
	Port int    `json:"port"`
	User string `json:"user"`
	// AuthMethod is "" or "password" for password login and "key" for an
	// OpenSSH private key. Exactly one secret kind is accepted per connection.
	AuthMethod         string `json:"authMethod,omitempty"`
	Password           string `json:"password"`
	PrivateKey         string `json:"privateKey,omitempty"`
	Passphrase         string `json:"passphrase,omitempty"`
	Fingerprint        string `json:"fingerprint"`
	TrustMode          string `json:"trustMode,omitempty"`
	ReplaceFingerprint string `json:"replaceFingerprint,omitempty"`
}

func (s SSH) UsesKey() bool { return s.AuthMethod == "key" }

// WithoutSecrets keeps the connection identity for records and busy checks.
func (s SSH) WithoutSecrets() SSH {
	s.Password, s.PrivateKey, s.Passphrase = "", "", ""
	return s
}

type DDOptions struct {
	ConfirmErase bool   `json:"confirmErase"`
	PortMode     string `json:"portMode"`
	NewPort      int    `json:"newPort,omitempty"`
	PasswordMode string `json:"passwordMode"`
	NewPassword  string `json:"newPassword,omitempty"`
	// LoginMode applies only to key login: "key" writes the current key's
	// public half into the new system, "password" sets NewPassword instead.
	LoginMode string `json:"loginMode,omitempty"`
}
type Request struct {
	Kind          string          `json:"kind"`
	SSH           SSH             `json:"ssh"`
	Mode          string          `json:"mode,omitempty"`
	ClientConfig  json.RawMessage `json:"clientConfig,omitempty"`
	Front         *SSH            `json:"front,omitempty"`
	DD            *DDOptions      `json:"dd,omitempty"`
	ForwardTarget *Target         `json:"forwardTarget,omitempty"`
	Remark        string          `json:"remark,omitempty"`
	Cleanup       *CleanupOptions `json:"cleanup,omitempty"`
	// Customer relay bandwidth for each direction; zero means the 5 Mbps default.
	RateMbps int          `json:"rateMbps,omitempty"`
	Meta     *RelayMeta   `json:"meta,omitempty"`
	Manage   *RelayManage `json:"manage,omitempty"`
	// Verify asks for relay target verification (relay, relay-code and
	// relay-verify only); results then carry Result.Verify.
	Verify *VerifyOptions `json:"verify,omitempty"`
	// ReportVerified lets relay-inventory return each record's verification;
	// only panels that set it understand the field.
	ReportVerified bool `json:"reportVerified,omitempty"`
}

// RelayMeta is the panel-supplied part of meta.json. The executor adds the
// target, port, rate and installation time; no credential is ever included.
type RelayMeta struct {
	Source   string `json:"source"`
	CodeTail string `json:"codeTail,omitempty"`
	User     string `json:"user"`
	Panel    string `json:"panel,omitempty"`
	Remark   string `json:"remark,omitempty"`
}

// RelayManage changes only the listed managed relays on one server.
type RelayManage struct {
	Action   string   `json:"action"`
	Items    []string `json:"items"`
	RateMbps int      `json:"rateMbps,omitempty"`
}
type CleanupOptions struct {
	ManagedTaskID string `json:"managedTaskId,omitempty"`
	Scope         string `json:"scope"`
	PreviewID     string `json:"previewId,omitempty"`
	Digest        string `json:"digest,omitempty"`
	Confirm       bool   `json:"confirm"`
}
type CleanupItem struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
}
type CleanupReport struct {
	ManagedTaskID string        `json:"managedTaskId,omitempty"`
	Scope         string        `json:"scope"`
	Digest        string        `json:"digest"`
	Items         []CleanupItem `json:"items"`
	Removed       bool          `json:"removed"`
}
type Target struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}
type Asset struct {
	Data   []byte `json:"data"`
	SHA256 string `json:"sha256"`
}
type Job struct {
	ID       string  `json:"id"`
	Lease    string  `json:"lease"`
	Request  Request `json:"request"`
	Script   Asset   `json:"script,omitempty"`
	Deadline int64   `json:"deadline"`
}
type Hop struct {
	FromHost string `json:"fromHost"`
	FromPort int    `json:"fromPort"`
	ToHost   string `json:"toHost"`
	ToPort   int    `json:"toPort"`
}
type Health struct {
	BBR           string `json:"bbr,omitempty"`
	Service       string `json:"service"`
	LocalSelfTest string `json:"localSelfTest"`
	PublicTCP     string `json:"publicTCP"`
	// PublicUDP: reachable, unreachable, off or not_tested; empty in
	// results from before nodes served UDP.
	PublicUDP string `json:"publicUDP,omitempty"`
	Game      string `json:"game"`
}

// RelayRecord is meta.json as written on the relay server.
type RelayRecord struct {
	Version   int    `json:"version"`
	Source    string `json:"source"`
	CodeTail  string `json:"codeTail,omitempty"`
	User      string `json:"user"`
	Panel     string `json:"panel,omitempty"`
	Target    string `json:"target"`
	Port      int    `json:"port"`
	RateMbps  int    `json:"rateMbps"`
	Remark    string `json:"remark,omitempty"`
	CreatedAt string `json:"createdAt"`
	// Verified is the last target verification of this relay, if any.
	Verified *RelayVerified `json:"verified,omitempty"`
}

// RelayVerified records when and with which result code a relay's target was
// last verified. It never includes node credentials or the node identity.
type RelayVerified struct {
	Code string `json:"code"`
	At   string `json:"at"`
}

// RelayEntry is one managed relay read back from a server. Role follows the
// managed identity suffix: relay (r), front (f) or route (p, paid-route front).
type RelayEntry struct {
	ID          string       `json:"id"`
	Role        string       `json:"role"`
	Port        int          `json:"port"`
	RateMbps    int          `json:"rateMbps"`
	Target      string       `json:"target"`
	State       string       `json:"state"`
	Connections int          `json:"connections"`
	CreatedAt   int64        `json:"createdAt"`
	Record      *RelayRecord `json:"record,omitempty"`
}
type RelayManageResult struct {
	ID    string `json:"id"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}
type Result struct {
	ID          string          `json:"id"`
	Lease       string          `json:"lease"`
	State       string          `json:"state"`
	Phase       string          `json:"phase"`
	Message     string          `json:"message"`
	Fingerprint string          `json:"fingerprint,omitempty"`
	Algorithm   string          `json:"algorithm,omitempty"`
	Config      json.RawMessage `json:"config,omitempty"`
	Health      *Health         `json:"health,omitempty"`
	Hops        []Hop           `json:"hops,omitempty"`
	ErrorCode   string          `json:"errorCode,omitempty"`
	NextStep    string          `json:"nextStep,omitempty"`
	Cleanup     *CleanupReport  `json:"cleanup,omitempty"`
	// Set only for relay-check, relay-inventory and relay-manage, which older
	// panels never request, so their strict result decoding is unaffected.
	OS        string              `json:"os,omitempty"`
	Inventory []RelayEntry        `json:"inventory,omitempty"`
	Managed   []RelayManageResult `json:"managed,omitempty"`
	// FailedRole tells a relay-code user whether the owner's relay server or
	// their own front failed, since only the owner can act on the former.
	FailedRole string `json:"failedRole,omitempty"`
	// Verify is reported only when the request asked for it.
	Verify *VerifyReport `json:"verify,omitempty"`
}
type Remote interface {
	Run(context.Context, SSH, string) ([]byte, error)
	Probe(context.Context, string, int) (string, string, error)
}
