package control

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"time"

	"github.com/mozziexwz/node/internal/relayruntime"
)

// Grants are immutable authorizations to report a historical billing period.
// They outlive rule deletion, renewal and command replacement. No target or key
// is stored here. Historical use is never silently charged to a newer package.
type RelayBillingGrantV2 struct {
	ID                 string `json:"id"`
	AgentID            string `json:"agentId"`
	RuleID             string `json:"ruleId"`
	UserID             string `json:"userId"`
	EntitlementVersion int64  `json:"entitlementVersion"`
	Billing            bool   `json:"billing"`
	TrafficMode        string `json:"trafficMode"`
	Multiplier         int64  `json:"multiplier"`
	FirstVersion       int64  `json:"firstVersion"`
	LastVersion        int64  `json:"lastVersion"`
	CreatedAt          int64  `json:"createdAt"`
}

type RelayBillingPeriodV2 struct {
	UserID             string `json:"userId"`
	EntitlementVersion int64  `json:"entitlementVersion"`
	ConfirmedBytes     int64  `json:"confirmedBytes"`
	ReviewBytes        int64  `json:"reviewBytes"`
}

type relayTrafficCursorV2 struct {
	TrafficCursor
	GrantID        string `json:"grantId"`
	CollectedFrom  int64  `json:"collectedFrom"`
	CollectedUntil int64  `json:"collectedUntil"`
	Uncertain      bool   `json:"uncertain"`
	Review         bool   `json:"review"`
}

func grantRelayBillingV2(s *State, agentID string, rule UserRule, runtime relayruntime.Rule) (string, error) {
	if agentID == "" || rule.ID == "" || rule.UserID == "" || runtime.ID != rule.ID || runtime.Version < 1 || runtime.EntitlementVersion < 0 || runtime.EntitlementVersion != rule.EntitlementVersion {
		return "", errors.New("invalid billing grant identity")
	}
	if _, _, err := relayWeightedTraffic(0, 0, rule.TrafficMode, rule.TrafficMultiplierPermille, 0); err != nil {
		return "", err
	}
	identity, _ := json.Marshal([]any{agentID, rule.ID, rule.UserID, runtime.EntitlementVersion, runtime.Billing, rule.TrafficMode, rule.TrafficMultiplierPermille})
	hash := sha256.Sum256(identity)
	id := hex.EncodeToString(hash[:])
	grant, exists := LoadDoc[RelayBillingGrantV2](s, "relay_v2_billing_grants", id)
	if !exists {
		grant = RelayBillingGrantV2{ID: id, AgentID: agentID, RuleID: rule.ID, UserID: rule.UserID, EntitlementVersion: runtime.EntitlementVersion, Billing: runtime.Billing, TrafficMode: rule.TrafficMode, Multiplier: rule.TrafficMultiplierPermille, FirstVersion: runtime.Version, LastVersion: runtime.Version, CreatedAt: time.Now().UnixMilli()}
	} else if runtime.Version < grant.FirstVersion {
		return "", errors.New("billing grant version rollback")
	}
	grant.LastVersion = max(grant.LastVersion, runtime.Version)
	return id, SaveDoc(s, "relay_v2_billing_grants", id, grant)
}

func addRelayV2Bytes(old, delta int64) (int64, error) {
	if old < 0 || delta < 0 || delta > math.MaxInt64-old {
		return 0, errors.New("traffic ledger overflow")
	}
	return old + delta, nil
}

// Errors leave State untouched, allowing a caller to isolate a malformed sample
// while applying other valid traffic and control messages in the transaction.
// Returned ACKs must not leave the server before Store.Update commits.
func applyRelayTrafficV2(s *State, agent RelayAgent, report relayruntime.V2Traffic, now int64) (relayruntime.V2TrafficAck, error) {
	ack := relayruntime.V2TrafficAck{RuleID: report.ID, Epoch: report.Epoch, Sequence: report.Sequence}
	invalid := func() (relayruntime.V2TrafficAck, error) {
		return relayruntime.V2TrafficAck{}, errors.New("invalid v2 traffic sample")
	}
	if report.Sequence < 1 || report.Version < 1 || len(report.Epoch) < 16 || len(report.Epoch) > 100 || len(report.ID) < 16 || len(report.ID) > 100 || report.InputBytes < 0 || report.OutputBytes < 0 || report.Connections < 0 || report.CollectedFrom < 1 || report.CollectedUntil < report.CollectedFrom {
		return invalid()
	}
	grant, ok := LoadDoc[RelayBillingGrantV2](s, "relay_v2_billing_grants", report.BillingPeriodID)
	if !ok || grant.AgentID != agent.ID || grant.RuleID != report.ID || grant.EntitlementVersion != report.EntitlementVersion || report.Version < grant.FirstVersion || report.Version > grant.LastVersion {
		return invalid()
	}
	key := agent.ID + ":" + report.ID + ":" + report.Epoch
	cursor, exists := LoadDoc[relayTrafficCursorV2](s, "relay_v2_traffic_cursors", key)
	if exists && (cursor.GrantID != grant.ID || cursor.CollectedFrom != report.CollectedFrom || cursor.Uncertain != report.Uncertain) {
		return invalid()
	}
	if report.Sequence <= cursor.Sequence {
		if report.InputBytes > cursor.InputBytes || report.OutputBytes > cursor.OutputBytes || (report.Sequence == cursor.Sequence && (report.InputBytes != cursor.InputBytes || report.OutputBytes != cursor.OutputBytes || report.CollectedUntil != cursor.CollectedUntil)) {
			return invalid()
		}
		return ack, nil
	}
	if report.InputBytes < cursor.InputBytes || report.OutputBytes < cursor.OutputBytes || report.CollectedUntil < cursor.CollectedUntil {
		return invalid()
	}
	delta, remainder, err := relayWeightedTraffic(report.InputBytes-cursor.InputBytes, report.OutputBytes-cursor.OutputBytes, grant.TrafficMode, grant.Multiplier, cursor.Remainder)
	if err != nil {
		return relayruntime.V2TrafficAck{}, err
	}
	nextCursor := relayTrafficCursorV2{TrafficCursor: TrafficCursor{Sequence: report.Sequence, InputBytes: report.InputBytes, OutputBytes: report.OutputBytes, Remainder: remainder}, GrantID: grant.ID, CollectedFrom: report.CollectedFrom, CollectedUntil: report.CollectedUntil, Uncertain: report.Uncertain}
	if !grant.Billing {
		return ack, SaveDoc(s, "relay_v2_traffic_cursors", key, nextCursor)
	}
	fromMonth := time.UnixMilli(report.CollectedFrom).UTC().Format("2006-01")
	untilMonth := time.UnixMilli(report.CollectedUntil).UTC().Format("2006-01")
	uncertain := report.Uncertain || fromMonth != untilMonth || report.CollectedUntil > now+5*60*1000 || report.CollectedFrom < grant.CreatedAt-5*60*1000
	uncertain = uncertain || cursor.Review
	nextCursor.Review = uncertain
	periodKey := grant.UserID + ":" + strconv.FormatInt(grant.EntitlementVersion, 10)
	period, _ := LoadDoc[RelayBillingPeriodV2](s, "relay_v2_billing_periods", periodKey)
	period.UserID, period.EntitlementVersion = grant.UserID, grant.EntitlementVersion
	if uncertain {
		period.ReviewBytes, err = addRelayV2Bytes(period.ReviewBytes, delta)
	} else {
		period.ConfirmedBytes, err = addRelayV2Bytes(period.ConfirmedBytes, delta)
	}
	if err != nil {
		return relayruntime.V2TrafficAck{}, err
	}
	var userCopy *User
	if user := s.Users[grant.UserID]; user != nil && grant.EntitlementVersion == entitlementVersion(s, grant.UserID) && !uncertain {
		copy := *user
		copy.TrafficUsed, err = addRelayV2Bytes(copy.TrafficUsed, delta)
		if err != nil {
			return relayruntime.V2TrafficAck{}, err
		}
		userCopy = &copy
	}
	monthKey := grant.UserID + ":" + fromMonth
	month, _ := LoadDoc[int64](s, "traffic_months", monthKey)
	if !uncertain {
		month, err = addRelayV2Bytes(month, delta)
		if err != nil {
			return relayruntime.V2TrafficAck{}, err
		}
	}
	var associated UserRule
	collection, associatedKey := "", ""
	for id := range s.Docs["user_rules"] {
		rule, valid := LoadDoc[UserRule](s, "user_rules", id)
		if valid && rule.ID == report.ID {
			associated, collection, associatedKey = rule, "user_rules", id
			break
		}
	}
	if collection == "" {
		if rule, valid := LoadDoc[UserRule](s, "relay_rule_archive", report.ID); valid {
			associated, collection, associatedKey = rule, "relay_rule_archive", report.ID
		}
	}
	if collection != "" {
		associated.TrafficBytes, err = addRelayV2Bytes(associated.TrafficBytes, delta)
		if err != nil {
			return relayruntime.V2TrafficAck{}, err
		}
	}
	// All validation and overflow checks precede any mutation.
	if err = SaveDoc(s, "relay_v2_traffic_cursors", key, nextCursor); err != nil {
		return relayruntime.V2TrafficAck{}, err
	}
	if err = SaveDoc(s, "relay_v2_billing_periods", periodKey, period); err != nil {
		return relayruntime.V2TrafficAck{}, err
	}
	if uncertain {
		// Bounded to one latest cumulative record per epoch, with provenance.
		if err = SaveDoc(s, "relay_v2_traffic_review", key, report); err != nil {
			return relayruntime.V2TrafficAck{}, err
		}
	} else if err = SaveDoc(s, "traffic_months", monthKey, month); err != nil {
		return relayruntime.V2TrafficAck{}, err
	}
	if collection != "" {
		if err = SaveDoc(s, collection, associatedKey, associated); err != nil {
			return relayruntime.V2TrafficAck{}, err
		}
	}
	if userCopy != nil {
		s.Users[grant.UserID] = userCopy
	}
	return ack, nil
}
