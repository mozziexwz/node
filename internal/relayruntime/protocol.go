// Package relayruntime manages only GOST forwarding. It deliberately contains
// no SSH, installer, DD, or general command endpoint.
package relayruntime

type Config struct{ ServerURL, EnrollmentToken, StateDir, GostBinary, OfflinePolicy string }
type Rule struct {
	ID                 string      `json:"id"`
	Version            int64       `json:"version"`
	ListenPort         int         `json:"listenPort"`
	Targets            []string    `json:"targets"`
	AllowedSources     []string    `json:"allowedSources,omitempty"`
	Strategy           string      `json:"strategy"`
	Protocol           string      `json:"protocol"`
	RateMbps           int64       `json:"rateMbps"`
	Billing            bool        `json:"billing"`
	EntitlementVersion int64       `json:"entitlementVersion"`
	LeaseUntil         int64       `json:"leaseUntil"`
	TLSCertificate     string      `json:"tlsCertificate,omitempty"`
	TLSPrivateKey      string      `json:"tlsPrivateKey,omitempty"`
	TargetTLS          []TLSClient `json:"targetTls,omitempty"`
}
type TLSClient struct {
	CA         string `json:"ca"`
	ServerName string `json:"serverName"`
}
type Ack struct {
	ID      string `json:"id"`
	Version int64  `json:"version"`
	State   string `json:"state"`
	Message string `json:"message,omitempty"`
}
type Traffic struct {
	ID                 string `json:"id"`
	Version            int64  `json:"version"`
	Epoch              string `json:"epoch"`
	Sequence           int64  `json:"sequence"`
	InputBytes         int64  `json:"inputBytes"`
	OutputBytes        int64  `json:"outputBytes"`
	Connections        int64  `json:"connections"`
	EntitlementVersion int64  `json:"entitlementVersion"`
}
type SyncRequest struct {
	BootID       string    `json:"bootId"`
	Sequence     int64     `json:"sequence"`
	Version      string    `json:"version"`
	Capabilities []string  `json:"capabilities,omitempty"`
	Acks         []Ack     `json:"acks"`
	Traffic      []Traffic `json:"traffic"`
}
type SyncResponse struct {
	ServerTime   int64  `json:"serverTime"`
	LeaseSeconds int64  `json:"leaseSeconds"`
	Rules        []Rule `json:"rules"`
}
