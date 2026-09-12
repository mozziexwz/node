// Package executor performs only fixed, authenticated, short-lived customer VPS tasks.
package executor

import (
	"context"
	"encoding/json"
)

type SSH struct {
	Host        string `json:"host"`
	Port        int    `json:"port"`
	User        string `json:"user"`
	Password    string `json:"password"`
	Fingerprint string `json:"fingerprint"`
}
type DDOptions struct {
	ConfirmErase bool   `json:"confirmErase"`
	PortMode     string `json:"portMode"`
	NewPort      int    `json:"newPort,omitempty"`
	PasswordMode string `json:"passwordMode"`
	NewPassword  string `json:"newPassword,omitempty"`
}
type Request struct {
	Kind          string          `json:"kind"`
	SSH           SSH             `json:"ssh"`
	Mode          string          `json:"mode,omitempty"`
	ClientConfig  json.RawMessage `json:"clientConfig,omitempty"`
	Front         *SSH            `json:"front,omitempty"`
	DD            *DDOptions      `json:"dd,omitempty"`
	ForwardTarget *Target         `json:"forwardTarget,omitempty"`
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
	Service       string `json:"service"`
	LocalSelfTest string `json:"localSelfTest"`
	PublicTCP     string `json:"publicTCP"`
	Game          string `json:"game"`
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
}
type Remote interface {
	Run(context.Context, SSH, string) ([]byte, error)
	Probe(context.Context, string, int) (string, string, error)
}
