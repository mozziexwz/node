package control

import (
	"math"
	"net/http"
	"sort"

	"github.com/mozziexwz/node/internal/relayruntime"
)

func (a *App) relayAccountingV2(w http.ResponseWriter, r *http.Request) {
	if _, err := a.Admin(r); err != nil {
		commerceError(w, http.StatusForbidden, err)
		return
	}
	periods := []RelayBillingPeriodV2{}
	var reviewBytes int64
	var reviewCount, degradedAgents int
	err := a.Store.View(func(s *State) error {
		periods = ListDocs[RelayBillingPeriodV2](s, "relay_v2_billing_periods")
		for _, period := range periods {
			if period.ReviewBytes > math.MaxInt64-reviewBytes {
				reviewBytes = math.MaxInt64
			} else {
				reviewBytes += max(0, period.ReviewBytes)
			}
		}
		reviewCount = len(ListDocs[relayruntime.V2Traffic](s, "relay_v2_traffic_review"))
		for _, agent := range ListDocs[RelayAgent](s, "relay_agents") {
			if agent.AccountingDegraded {
				degradedAgents++
			}
		}
		return nil
	})
	if err != nil {
		commerceError(w, http.StatusServiceUnavailable, err)
		return
	}
	sort.Slice(periods, func(i, j int) bool {
		if periods[i].UserID != periods[j].UserID {
			return periods[i].UserID < periods[j].UserID
		}
		return periods[i].EntitlementVersion < periods[j].EntitlementVersion
	})
	WriteJSON(w, http.StatusOK, map[string]any{"periods": periods, "reviewBytes": reviewBytes, "reviewSamples": reviewCount, "degradedAgents": degradedAgents, "message": "历史周期按原权益归属；待核对流量未混扣新套餐。跨月或时钟不确定区间须人工核对，当前未提供自动调账操作。"})
}
