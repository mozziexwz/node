package control

import (
	"github.com/mozziexwz/node/internal/relayruntime"
	"time"
)

func (a *App) configureRelayTLS(rule *UserRule, stages []RouteStage, _ int64) error {
	certificates := map[string]relayruntime.TLSClient{}
	expires := time.Now().Add(365 * 24 * time.Hour)
	rule.TLSExpiresAt = 0
	for i := range rule.Segments {
		seg := &rule.Segments[i]
		seg.Runtime.TargetTLS = nil
		if seg.Runtime.Protocol != "tls" {
			continue
		}
		name := seg.AgentID + "." + rule.ID + ".msboost.invalid"
		cert, key, err := relayruntime.NewTLSIdentity(name, expires)
		if err != nil {
			return err
		}
		sealed, err := a.Seal([]byte(key))
		if err != nil {
			return err
		}
		seg.Runtime.TLSCertificate, seg.SealedTLSKey = cert, sealed
		seg.Runtime.TLSPrivateKey = ""
		rule.TLSExpiresAt = expires.UnixMilli()
		certificates[seg.AgentID] = relayruntime.TLSClient{CA: cert, ServerName: name}
	}
	for i, stage := range stages {
		if i+1 == len(stages) || stages[i+1].Protocol != "tls" {
			continue
		}
		for _, id := range stage.AgentIDs {
			for j := range rule.Segments {
				seg := &rule.Segments[j]
				if seg.AgentID != id {
					continue
				}
				for _, nextID := range stages[i+1].AgentIDs {
					seg.Runtime.TargetTLS = append(seg.Runtime.TargetTLS, certificates[nextID])
				}
			}
		}
	}
	return nil
}
