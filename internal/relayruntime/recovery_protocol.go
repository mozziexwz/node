package relayruntime

// Recovery envelopes are exchanged only through the protected local root
// channel. They are never accepted from the ordinary control-plane sync API.
// Snapshots deliberately omit TLS private keys and management credentials.
type V2RecoverySnapshot struct {
	ProtocolVersion    int                `json:"protocolVersion"`
	RecoveryID         string             `json:"recoveryId"`
	AgentID            string             `json:"agentId"`
	ServerURL          string             `json:"serverUrl"`
	ControlEpoch       string             `json:"controlEpoch"`
	Revision           int64              `json:"revision"`
	PlanSequence       int64              `json:"planSequence"`
	Records            []V2RecoveryRecord `json:"records"`
	Traffic            []V2Traffic        `json:"traffic"`
	AccountingDegraded bool               `json:"accountingDegraded"`
	HistoryIncomplete  bool               `json:"historyIncomplete"`
	CapturedAt         int64              `json:"capturedAt"`
}

type V2RecoveryRecord struct {
	RuleID       string     `json:"ruleId"`
	Command      V2Command  `json:"command"`
	LastApplied  *V2Command `json:"lastApplied,omitempty"`
	State        string     `json:"state"`
	Running      bool       `json:"running"`
	RuntimeHash  string     `json:"runtimeHash,omitempty"`
	ProcessEpoch string     `json:"processEpoch,omitempty"`
}

type V2RecoveryCommandRef struct {
	CommandID       string `json:"commandId"`
	Generation      int64  `json:"generation"`
	Action          string `json:"action"`
	RuntimeHash     string `json:"runtimeHash,omitempty"`
	BillingPeriodID string `json:"billingPeriodId,omitempty"`
}

type V2RecoveryDecision struct {
	RuleID              string                `json:"ruleId"`
	Action              string                `json:"action"` // adopt, stop, hold
	ExpectedCommand     V2RecoveryCommandRef  `json:"expectedCommand"`
	ExpectedLastApplied *V2RecoveryCommandRef `json:"expectedLastApplied,omitempty"`
	ExpectedRuntimeHash string                `json:"expectedRuntimeHash,omitempty"`
	Command             *V2Command            `json:"command,omitempty"`
}

type V2RecoveryPlan struct {
	ProtocolVersion      int                  `json:"protocolVersion"`
	RecoveryID           string               `json:"recoveryId"`
	PlanID               string               `json:"planId"`
	PlanSequence         int64                `json:"planSequence"`
	AgentID              string               `json:"agentId"`
	ExpectedServerURL    string               `json:"expectedServerUrl"`
	ExpectedControlEpoch string               `json:"expectedControlEpoch"`
	ServerURL            string               `json:"serverUrl"`
	ControlEpoch         string               `json:"controlEpoch"`
	Token                string               `json:"token"`
	Revision             int64                `json:"revision"`
	Decisions            []V2RecoveryDecision `json:"decisions"`
}

type V2RecoveryResult struct {
	RecoveryID       string `json:"recoveryId"`
	PlanID           string `json:"planId"`
	PlanSequence     int64  `json:"planSequence"`
	RecoveryRequired bool   `json:"recoveryRequired"`
	Message          string `json:"message"`
}

func RecoveryCommandRef(command V2Command) V2RecoveryCommandRef {
	return V2RecoveryCommandRef{CommandID: command.CommandID, Generation: command.Generation, Action: command.Action, RuntimeHash: command.RuntimeHash, BillingPeriodID: command.BillingPeriodID}
}
