package control

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
)

const commerceDay = int64(24 * time.Hour / time.Millisecond)
const commerceGB = int64(1000000000)

type Plan struct {
	ID                   string `json:"id"`
	Name                 string `json:"name"`
	PriceCents           int64  `json:"priceCents"`
	Days                 int64  `json:"days"`
	TrafficBytes         int64  `json:"trafficBytes"`
	RateMbps             int64  `json:"rateMbps"`
	Enabled              bool   `json:"enabled"`
	Version              int64  `json:"version"`
	CreatedAt            int64  `json:"createdAt"`
	MaxPurchasesPerUser  int64  `json:"maxPurchasesPerUser"`
	RedemptionCountEpoch int64  `json:"redemptionCountEpoch"`
	Level                int    `json:"level"`
	Trial                bool   `json:"trial"`
	CurrentHoldersOnly   bool   `json:"currentHoldersOnly"`
}
type commercePlanView struct {
	Plan
	Eligible bool   `json:"eligible"`
	Reason   string `json:"reason,omitempty"`
}
type LedgerEntry struct {
	ID           string `json:"id"`
	UserID       string `json:"userId"`
	Kind         string `json:"kind"`
	AmountCents  int64  `json:"amountCents"`
	BalanceAfter int64  `json:"balanceAfter"`
	Reference    string `json:"reference"`
	Reason       string `json:"reason"`
	CreatedAt    int64  `json:"createdAt"`
	CardCode     string `json:"cardCode,omitempty"`
}
type BalanceCard struct {
	ID          string `json:"id"`
	CodeHash    string `json:"codeHash,omitempty"`
	SealedCode  string `json:"sealedCode,omitempty"`
	Code        string `json:"code,omitempty"`
	AmountCents int64  `json:"amountCents"`
	// CardVersion distinguishes legacy one-use codes from newly generated
	// codes, where MaxUses == 0 explicitly means unlimited redemptions.
	CardVersion int              `json:"cardVersion,omitempty"`
	MaxUses     int64            `json:"maxUses"`
	UsedCount   int64            `json:"usedCount"`
	Uses        []BalanceCardUse `json:"uses,omitempty"`
	Status      string           `json:"status"`
	Batch       string           `json:"batch"`
	UsedBy      string           `json:"usedBy,omitempty"`
	UsedByEmail string           `json:"usedByEmail,omitempty"`
	CreatedAt   int64            `json:"createdAt"`
	UsedAt      int64            `json:"usedAt,omitempty"`
}
type BalanceCardUse struct {
	UserID    string `json:"userId"`
	UserEmail string `json:"userEmail,omitempty"`
	UsedAt    int64  `json:"usedAt"`
}
type Order struct {
	ID                   string `json:"id"`
	UserID               string `json:"userId"`
	UserEmail            string `json:"userEmail,omitempty"`
	Plan                 Plan   `json:"plan"`
	AmountCents          int64  `json:"amountCents"`
	Currency             string `json:"currency"`
	ChannelID            string `json:"channelId"`
	State                string `json:"state"`
	RequestID            string `json:"requestId"`
	TradeNo              string `json:"tradeNo,omitempty"`
	PaymentURL           string `json:"paymentUrl,omitempty"`
	CreatedAt            int64  `json:"createdAt"`
	ExpiresAt            int64  `json:"expiresAt"`
	PaidAt               int64  `json:"paidAt,omitempty"`
	EntitlementVersion   int64  `json:"entitlementVersion"`
	RedemptionCountEpoch int64  `json:"redemptionCountEpoch"`
	ReviewReason         string `json:"reviewReason,omitempty"`
}
type commerceIdempotency struct {
	ObjectID    string `json:"objectId"`
	Fingerprint string `json:"fingerprint"`
}

func cardUseLimit(c BalanceCard) int64 {
	if c.CardVersion < 2 {
		return 1
	}
	return c.MaxUses
}

func cardUseCount(c BalanceCard) int64 {
	count := c.UsedCount
	if int64(len(c.Uses)) > count {
		count = int64(len(c.Uses))
	}
	if c.UsedBy != "" && count == 0 {
		count = 1
	}
	return count
}

func cardUsedBy(c BalanceCard, userID string) bool {
	if c.UsedBy == userID {
		return true
	}
	for _, use := range c.Uses {
		if use.UserID == userID {
			return true
		}
	}
	return false
}

func cardAdminView(s *State, c BalanceCard) BalanceCard {
	c.MaxUses = cardUseLimit(c)
	c.UsedCount = cardUseCount(c)
	if len(c.Uses) == 0 && c.UsedBy != "" {
		c.Uses = []BalanceCardUse{{UserID: c.UsedBy, UsedAt: c.UsedAt}}
	}
	for i := range c.Uses {
		if user := s.Users[c.Uses[i].UserID]; user != nil {
			c.Uses[i].UserEmail = user.Email
		}
	}
	if user := s.Users[c.UsedBy]; user != nil {
		c.UsedByEmail = user.Email
	}
	return c
}

func commerceID() string {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func commerceHash(v string) string { h := sha256.Sum256([]byte(v)); return hex.EncodeToString(h[:]) }
func commerceAudit(s *State, user, action, object string) error {
	return SaveDoc(s, "commerce_audit", commerceID(), map[string]any{"userId": user, "action": action, "objectId": object, "createdAt": time.Now().UnixMilli()})
}
func commerceError(w http.ResponseWriter, status int, err error) {
	WriteJSON(w, status, map[string]any{"error": err.Error()})
}
func commerceReqID(v string) error {
	if len(v) < 8 || len(v) > 128 {
		return errors.New("requestId 必须为 8–128 个字符")
	}
	return nil
}
func purchaseAllowed(u *User, now int64) bool {
	return u.ExpiresAt-now < 30*commerceDay || u.TrafficTotal-u.TrafficUsed < 10*commerceGB
}
func entitlementLevel(level int) int {
	if level < 1 || level > 3 {
		return 1
	}
	return level
}
func entitlementVersion(s *State, user string) int64 {
	v, _ := LoadDoc[int64](s, "entitlement_versions", user)
	return v
}

func latestSuccessfulPlanID(s *State, userID string) string {
	var latest Order
	found := false
	for _, order := range ListDocs[Order](s, "orders") {
		if order.UserID != userID || order.State != "paid" || order.Plan.ID == "" {
			continue
		}
		if !found || order.PaidAt > latest.PaidAt || order.PaidAt == latest.PaidAt && order.CreatedAt > latest.CreatedAt || order.PaidAt == latest.PaidAt && order.CreatedAt == latest.CreatedAt && order.ID > latest.ID {
			latest, found = order, true
		}
	}
	if !found {
		return ""
	}
	return latest.Plan.ID
}

// inferCurrentPlanID upgrades only an active, still-valid legacy entitlement.
// A pending, expired or paid_review order can never create renewal eligibility.
func inferCurrentPlanID(s *State, user *User, now int64) string {
	if user == nil {
		return ""
	}
	if user.PlanID != "" {
		return user.PlanID
	}
	if user.Status != "active" || user.ExpiresAt <= now {
		return ""
	}
	user.PlanID = latestSuccessfulPlanID(s, user.ID)
	return user.PlanID
}

func effectiveCurrentPlanID(s *State, user *User, now int64) string {
	if user == nil || user.Status != "active" || user.ExpiresAt <= now {
		return ""
	}
	if user.PlanID != "" {
		return user.PlanID
	}
	return latestSuccessfulPlanID(s, user.ID)
}

func planPolicyAllowed(s *State, plan Plan, user *User, now int64) error {
	if user == nil {
		return errors.New("账号已失效")
	}
	// Pending orders carry an immutable snapshot, while a later emergency
	// restriction on the live plan must also take effect. Apply the stricter
	// union so neither changing the plan nor replaying an old order bypasses it.
	if current, ok := LoadDoc[Plan](s, "plans", plan.ID); ok {
		// Unlisting stops new customers but must not cut off renewal for an
		// existing, still-valid holder of this exact entitlement. This also
		// applies when an old pending payment is fulfilled after unlisting.
		if !current.Enabled && effectiveCurrentPlanID(s, user, now) != plan.ID {
			return errors.New("权益已下架，仅当前有效持有者可续兑")
		}
		plan.CurrentHoldersOnly = plan.CurrentHoldersOnly || current.CurrentHoldersOnly
	}
	if plan.CurrentHoldersOnly {
		if effectiveCurrentPlanID(s, user, now) != plan.ID {
			return errors.New("此权益仅允许当前有效持有者续兑")
		}
	}
	return nil
}
func appendLedger(s *State, u *User, amount int64, kind, reference, reason string, now int64) error {
	if u == nil || amount == math.MinInt64 || (amount > 0 && u.BalanceCents > math.MaxInt64-amount) || (amount < 0 && u.BalanceCents < -amount) {
		return errors.New("可用枫叶不足")
	}
	u.BalanceCents += amount
	e := LedgerEntry{ID: commerceID(), UserID: u.ID, Kind: kind, AmountCents: amount, BalanceAfter: u.BalanceCents, Reference: reference, Reason: reason, CreatedAt: now}
	return SaveDoc(s, "ledger", e.ID, e)
}
func fulfillOrder(s *State, o *Order, u *User, now int64) error {
	if err := planPolicyAllowed(s, o.Plan, u, now); err != nil {
		return err
	}
	if err := planPurchaseLimit(s, o.Plan, u.ID); err != nil {
		return err
	}
	// A pending checkout always consumes the current quota epoch at the time
	// of fulfillment, including when an administrator reset it in between.
	if current, ok := LoadDoc[Plan](s, "plans", o.Plan.ID); ok {
		o.RedemptionCountEpoch = current.RedemptionCountEpoch
	}
	u.ExpiresAt = now + o.Plan.Days*commerceDay
	u.TrafficTotal = o.Plan.TrafficBytes
	u.TrafficUsed = 0
	u.Level = entitlementLevel(o.Plan.Level)
	u.PlanID = o.Plan.ID
	if o.Plan.RateMbps > 0 {
		u.RateMbps = o.Plan.RateMbps
	}
	o.State = "paid"
	o.PaidAt = now
	nextEntitlementVersion := entitlementVersion(s, u.ID) + 1
	if err := resetUserRuleTraffic(s, u.ID, nextEntitlementVersion); err != nil {
		return err
	}
	if err := SaveDoc(s, "entitlement_versions", u.ID, nextEntitlementVersion); err != nil {
		return err
	}
	return SaveDoc(s, "orders", o.ID, o)
}

// Count committed fulfillments, never requests, so retries do not consume a
// purchase and every payment path observes the same transaction-locked limit.
func planPurchaseLimit(s *State, plan Plan, userID string) error {
	limit := plan.MaxPurchasesPerUser
	epoch := plan.RedemptionCountEpoch
	if current, ok := LoadDoc[Plan](s, "plans", plan.ID); ok {
		epoch = current.RedemptionCountEpoch
		if current.MaxPurchasesPerUser > 0 && (limit == 0 || current.MaxPurchasesPerUser < limit) {
			limit = current.MaxPurchasesPerUser
		}
	}
	if limit == 0 {
		return nil
	}
	var used int64
	for _, o := range ListDocs[Order](s, "orders") {
		if o.UserID == userID && o.Plan.ID == plan.ID && o.State == "paid" && o.RedemptionCountEpoch == epoch {
			used++
		}
	}
	if used >= limit {
		return errors.New("已达到该权益每位用户兑换次数上限")
	}
	return nil
}

func (a *App) RegisterCommerce(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/plans", a.commercePlans)
	mux.HandleFunc("GET /api/wallet", a.commerceWallet)
	mux.HandleFunc("POST /api/wallet/redeem", a.commerceRedeem)
	mux.HandleFunc("GET /api/orders", a.commerceOrders)
	mux.HandleFunc("POST /api/orders", a.commerceCreateOrder)
	mux.HandleFunc("GET /api/orders/{id}", a.commerceOrder)
	mux.HandleFunc("GET /api/admin/plans", a.commercePlans)
	mux.HandleFunc("POST /api/admin/plans", a.commerceSavePlan)
	mux.HandleFunc("PUT /api/admin/plans/{id}", a.commerceSavePlan)
	mux.HandleFunc("POST /api/admin/plans/{id}/reset-redemption-count", a.commerceResetPlanRedemptionCount)
	mux.HandleFunc("DELETE /api/admin/plans/{id}", a.commerceDeletePlan)
	mux.HandleFunc("GET /api/admin/cards", a.commerceCards)
	mux.HandleFunc("POST /api/admin/cards", a.commerceGenerateCards)
	mux.HandleFunc("POST /api/admin/cards/batch", a.commerceBatchCards)
	mux.HandleFunc("GET /api/admin/orders", a.commerceOrders)
	a.registerPayments(mux)
}
func (a *App) commercePlans(w http.ResponseWriter, r *http.Request) {
	admin := strings.HasPrefix(r.URL.Path, "/api/admin/")
	if admin {
		if _, err := a.Admin(r); err != nil {
			commerceError(w, 403, err)
			return
		}
	}
	out := []commercePlanView{}
	purchasedCounts := map[string]int64{}
	viewer, _ := a.User(r)
	requireVerifiedEmail := false
	err := a.Store.View(func(s *State) error {
		requireVerifiedEmail = boolSetting(s, "purchaseRequireVerifiedEmail")
		plans := ListDocs[Plan](s, "plans")
		planEpochs := make(map[string]int64, len(plans))
		for _, p := range plans {
			planEpochs[p.ID] = p.RedemptionCountEpoch
		}
		if viewer != nil {
			for _, order := range ListDocs[Order](s, "orders") {
				if order.UserID == viewer.ID && order.State == "paid" && order.RedemptionCountEpoch == planEpochs[order.Plan.ID] {
					purchasedCounts[order.Plan.ID]++
				}
			}
		}
		now := time.Now().UnixMilli()
		for _, p := range plans {
			p.Level = entitlementLevel(p.Level)
			unlistedHolder := !p.Enabled && viewer != nil && effectiveCurrentPlanID(s, s.Users[viewer.ID], now) == p.ID
			if !p.Enabled && !admin && !unlistedHolder {
				continue
			}
			view := commercePlanView{Plan: p, Eligible: p.Enabled || unlistedHolder}
			if p.PriceCents < 0 || p.PriceCents%100 != 0 {
				view.Eligible = false
				view.Reason = "权益枫叶数配置无效，请联系管理员"
			}
			if admin {
				out = append(out, view)
				continue
			}
			if viewer == nil {
				if p.CurrentHoldersOnly {
					continue
				}
				view.Eligible = view.Reason == ""
				out = append(out, view)
				continue
			}
			if policyErr := planPolicyAllowed(s, p, s.Users[viewer.ID], now); policyErr != nil {
				if p.CurrentHoldersOnly {
					continue
				}
				view.Eligible, view.Reason = false, policyErr.Error()
			}
			if view.Eligible && p.MaxPurchasesPerUser > 0 && purchasedCounts[p.ID] >= p.MaxPurchasesPerUser {
				view.Eligible, view.Reason = false, "已达到该权益每位用户兑换次数上限"
			}
			out = append(out, view)
		}
		return nil
	})
	if err != nil {
		commerceError(w, 500, err)
		return
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	WriteJSON(w, 200, map[string]any{"plans": out, "purchasedCounts": purchasedCounts, "purchaseRequireVerifiedEmail": requireVerifiedEmail})
}
func (a *App) commerceSavePlan(w http.ResponseWriter, r *http.Request) {
	if _, err := a.Admin(r); err != nil {
		commerceError(w, 403, err)
		return
	}
	var p Plan
	if err := Decode(r, &p); err != nil {
		commerceError(w, 400, err)
		return
	}
	invalidLevel := p.Level < 0 || p.Level > 3
	p.Level = entitlementLevel(p.Level)
	if invalidLevel || strings.TrimSpace(p.Name) == "" || len(p.Name) > 100 || p.Days < 1 || p.Days > 31 || p.PriceCents < 0 || p.PriceCents > 100000000 || p.PriceCents%100 != 0 || p.TrafficBytes < 1 || p.TrafficBytes > 1000000000000000 || p.RateMbps < 1 || p.RateMbps > 100000 || p.MaxPurchasesPerUser < 0 || p.MaxPurchasesPerUser > 1000000 {
		commerceError(w, 400, errors.New("权益名称、枫叶数（必须为整数）、1–31 天、流量、速率、等级或限兑次数无效"))
		return
	}
	p.ID = r.PathValue("id")
	err := a.Store.Update(func(s *State) error {
		if p.ID != "" {
			old, ok := LoadDoc[Plan](s, "plans", p.ID)
			if !ok {
				return errors.New("权益不存在")
			}
			p.RedemptionCountEpoch = old.RedemptionCountEpoch
			p.CreatedAt = old.CreatedAt
			p.Version = old.Version + 1
		} else {
			p.ID = commerceID()
			p.RedemptionCountEpoch = 0
			p.CreatedAt = time.Now().UnixMilli()
			p.Version = 1
		}
		return SaveDoc(s, "plans", p.ID, p)
	})
	if err != nil {
		commerceError(w, 409, err)
		return
	}
	WriteJSON(w, 200, p)
}
func (a *App) commerceResetPlanRedemptionCount(w http.ResponseWriter, r *http.Request) {
	admin, err := a.Admin(r)
	if err != nil {
		commerceError(w, 403, err)
		return
	}
	var p Plan
	err = a.Store.Update(func(s *State) error {
		var ok bool
		p, ok = LoadDoc[Plan](s, "plans", r.PathValue("id"))
		if !ok {
			return errors.New("权益不存在")
		}
		if p.RedemptionCountEpoch == math.MaxInt64 {
			return errors.New("限兑计数无法再次重置")
		}
		p.RedemptionCountEpoch++
		p.Version++
		if err := SaveDoc(s, "plans", p.ID, p); err != nil {
			return err
		}
		return commerceAudit(s, admin.ID, "plans.reset-redemption-count", p.ID)
	})
	if err != nil {
		commerceError(w, 409, err)
		return
	}
	WriteJSON(w, 200, p)
}
func (a *App) commerceDeletePlan(w http.ResponseWriter, r *http.Request) {
	if _, err := a.Admin(r); err != nil {
		commerceError(w, 403, err)
		return
	}
	err := a.Store.Update(func(s *State) error {
		for _, o := range ListDocs[Order](s, "orders") {
			if o.Plan.ID == r.PathValue("id") {
				return errors.New("权益已被兑换记录引用，请下架而非删除")
			}
		}
		DeleteDoc(s, "plans", r.PathValue("id"))
		return nil
	})
	if err != nil {
		commerceError(w, 409, err)
		return
	}
	WriteJSON(w, 200, map[string]bool{"ok": true})
}
func (a *App) commerceWallet(w http.ResponseWriter, r *http.Request) {
	u, err := a.User(r)
	if err != nil {
		commerceError(w, 401, err)
		return
	}
	entries := []LedgerEntry{}
	err = a.Store.View(func(s *State) error {
		for _, e := range ListDocs[LedgerEntry](s, "ledger") {
			if e.UserID == u.ID {
				if e.Kind == "card" {
					if card, ok := LoadDoc[BalanceCard](s, "cards", e.Reference); ok && cardUsedBy(card, u.ID) {
						plain, err := a.Open(card.SealedCode)
						if err != nil {
							return errors.New("兑换码记录解密失败")
						}
						e.CardCode = string(plain)
					}
				}
				entries = append(entries, e)
			}
		}
		return nil
	})
	if err != nil {
		commerceError(w, 500, err)
		return
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].CreatedAt > entries[j].CreatedAt })
	WriteJSON(w, 200, map[string]any{"balanceCents": u.BalanceCents, "ledger": entries})
}
func (a *App) commerceRedeem(w http.ResponseWriter, r *http.Request) {
	u, err := a.User(r)
	if err != nil {
		commerceError(w, 401, err)
		return
	}
	var in struct {
		Code      string `json:"code"`
		RequestID string `json:"requestId"`
	}
	if err = Decode(r, &in); err != nil {
		commerceError(w, 400, err)
		return
	}
	if err = commerceReqID(in.RequestID); err != nil {
		commerceError(w, 400, err)
		return
	}
	hash := commerceHash(strings.ToUpper(strings.TrimSpace(in.Code)))
	var balance int64
	err = a.Store.Update(func(s *State) error {
		key := u.ID + ":" + in.RequestID
		if prev, ok := LoadDoc[commerceIdempotency](s, "redeem_requests", key); ok {
			if prev.Fingerprint != hash {
				return errors.New("幂等键已用于另一兑换码")
			}
			balance = s.Users[u.ID].BalanceCents
			return nil
		}
		if boolSetting(s, "maintenance") {
			return errors.New("站点维护期间暂停兑换码兑换")
		}
		if !boolSetting(s, "cards") {
			return errors.New("兑换码兑换暂时关闭")
		}
		if s.Users[u.ID] == nil || s.Users[u.ID].Status != "active" {
			return errors.New("账号已失效")
		}
		for _, card := range ListDocs[BalanceCard](s, "cards") {
			if card.CodeHash != hash {
				continue
			}
			if cardUsedBy(card, u.ID) {
				return errors.New("兑换码无效或已被使用")
			}
			if card.Status != "active" {
				return errors.New("兑换码无效或已被使用")
			}
			if limit := cardUseLimit(card); limit > 0 && cardUseCount(card) >= limit {
				return errors.New("兑换码已达到使用次数上限")
			}
			if card.AmountCents < 100 || card.AmountCents%100 != 0 {
				return errors.New("兑换码枫叶面额无效，请联系管理员")
			}
			user := s.Users[u.ID]
			now := time.Now().UnixMilli()
			if err := appendLedger(s, user, card.AmountCents, "card", card.ID, "兑换码兑换", now); err != nil {
				return err
			}
			card.Uses = append(card.Uses, BalanceCardUse{UserID: u.ID, UserEmail: user.Email, UsedAt: now})
			card.UsedCount = cardUseCount(card)
			if card.UsedBy == "" {
				card.UsedBy = u.ID
				card.UsedAt = now
			}
			if limit := cardUseLimit(card); limit > 0 && card.UsedCount >= limit {
				card.Status = "used"
			}
			if err := SaveDoc(s, "cards", card.ID, card); err != nil {
				return err
			}
			if err := SaveDoc(s, "redeem_requests", key, commerceIdempotency{card.ID, hash}); err != nil {
				return err
			}
			balance = user.BalanceCents
			return nil
		}
		return errors.New("兑换码无效或已被使用")
	})
	if err != nil {
		commerceError(w, 409, err)
		return
	}
	WriteJSON(w, 200, map[string]any{"balanceCents": balance})
}
func (a *App) commerceCards(w http.ResponseWriter, r *http.Request) {
	admin, err := a.Admin(r)
	if err != nil {
		commerceError(w, 403, err)
		return
	}
	out := []BalanceCard{}
	err = a.Store.Update(func(s *State) error {
		for _, c := range ListDocs[BalanceCard](s, "cards") {
			filter := r.URL.Query().Get("status")
			if filter == "" && c.Status == "archived" || filter != "" && filter != "all" && c.Status != filter {
				continue
			}
			plain, err := a.Open(c.SealedCode)
			if err != nil {
				return errors.New("兑换码解密失败")
			}
			c.Code = string(plain)
			c.SealedCode = ""
			c.CodeHash = ""
			out = append(out, cardAdminView(s, c))
		}
		return commerceAudit(s, admin.ID, "cards.list", "all")
	})
	if err != nil {
		commerceError(w, 500, err)
		return
	}
	WriteJSON(w, 200, map[string]any{"cards": out})
}
func (a *App) commerceGenerateCards(w http.ResponseWriter, r *http.Request) {
	admin, err := a.Admin(r)
	if err != nil {
		commerceError(w, 403, err)
		return
	}
	var in struct {
		Count       int    `json:"count"`
		AmountCents int64  `json:"amountCents"`
		Batch       string `json:"batch"`
		MaxUses     *int64 `json:"maxUses"`
	}
	if err := Decode(r, &in); err != nil {
		commerceError(w, 400, err)
		return
	}
	maxUses := int64(1) // Existing API callers omitted this field: preserve one-use codes.
	if in.MaxUses != nil {
		maxUses = *in.MaxUses
	}
	if in.Count < 1 || in.Count > 1000 || in.AmountCents < 100 || in.AmountCents > 100000000 || in.AmountCents%100 != 0 || len(in.Batch) > 100 || maxUses < 0 || maxUses > 1000000 {
		commerceError(w, 400, errors.New("数量须为1–1000，枫叶面额必须为正整数，使用次数须为0–1000000（0为不限）"))
		return
	}
	out := []BalanceCard{}
	err = a.Store.Update(func(s *State) error {
		for i := 0; i < in.Count; i++ {
			code := "MSB-" + strings.ToUpper(commerceID())
			sealed, err := a.Seal([]byte(code))
			if err != nil {
				return err
			}
			c := BalanceCard{ID: commerceID(), CodeHash: commerceHash(code), SealedCode: sealed, AmountCents: in.AmountCents, CardVersion: 2, MaxUses: maxUses, Status: "active", Batch: in.Batch, CreatedAt: time.Now().UnixMilli()}
			if err := SaveDoc(s, "cards", c.ID, c); err != nil {
				return err
			}
			c.Code = code
			c.CodeHash = ""
			c.SealedCode = ""
			out = append(out, c)
		}
		return commerceAudit(s, admin.ID, "cards.generate", in.Batch)
	})
	if err != nil {
		commerceError(w, 500, err)
		return
	}
	WriteJSON(w, 201, map[string]any{"cards": out})
}
func (a *App) commerceBatchCards(w http.ResponseWriter, r *http.Request) {
	admin, err := a.Admin(r)
	if err != nil {
		commerceError(w, 403, err)
		return
	}
	var in struct {
		IDs    []string `json:"ids"`
		Action string   `json:"action"`
	}
	if err := Decode(r, &in); err != nil {
		commerceError(w, 400, err)
		return
	}
	if len(in.IDs) < 1 || len(in.IDs) > 1000 {
		commerceError(w, 400, errors.New("请选中1–1000个兑换码"))
		return
	}
	out := []BalanceCard{}
	err = a.Store.Update(func(s *State) error {
		for _, id := range in.IDs {
			c, ok := LoadDoc[BalanceCard](s, "cards", id)
			if !ok {
				return errors.New("兑换码不存在")
			}
			switch in.Action {
			case "enable":
				if limit := cardUseLimit(c); limit > 0 && cardUseCount(c) >= limit || c.Status == "archived" {
					return errors.New("已用尽或已归档的兑换码不可重新启用")
				}
				c.Status = "active"
			case "disable":
				if c.Status != "archived" && c.Status != "used" {
					c.Status = "disabled"
				}
			case "delete":
				if cardUseCount(c) > 0 {
					c.Status = "archived"
				} else {
					DeleteDoc(s, "cards", id)
					continue
				}
			case "export":
			default:
				return errors.New("未知批量操作")
			}
			if in.Action != "export" {
				if err := SaveDoc(s, "cards", id, c); err != nil {
					return err
				}
			}
			plain, e := a.Open(c.SealedCode)
			if e != nil {
				return e
			}
			c.Code = string(plain)
			c.SealedCode = ""
			c.CodeHash = ""
			out = append(out, cardAdminView(s, c))
		}
		return commerceAudit(s, admin.ID, "cards."+in.Action, strings.Join(in.IDs, ","))
	})
	if err != nil {
		commerceError(w, 409, err)
		return
	}
	WriteJSON(w, 200, map[string]any{"cards": out, "ok": true})
}
func (a *App) commerceOrders(w http.ResponseWriter, r *http.Request) {
	admin := strings.HasPrefix(r.URL.Path, "/api/admin/")
	u, err := a.User(r)
	if admin {
		u, err = a.Admin(r)
	}
	if err != nil {
		commerceError(w, 401, err)
		return
	}
	out := []Order{}
	err = a.Store.View(func(s *State) error {
		for _, o := range ListDocs[Order](s, "orders") {
			if admin || o.UserID == u.ID {
				if admin {
					if user := s.Users[o.UserID]; user != nil {
						o.UserEmail = user.Email
					}
				}
				out = append(out, o)
			}
		}
		return nil
	})
	if err != nil {
		commerceError(w, 500, err)
		return
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	WriteJSON(w, 200, map[string]any{"orders": out})
}
func (a *App) commerceOrder(w http.ResponseWriter, r *http.Request) {
	u, err := a.User(r)
	if err != nil {
		commerceError(w, 401, err)
		return
	}
	var o Order
	err = a.Store.View(func(s *State) error {
		var ok bool
		o, ok = LoadDoc[Order](s, "orders", r.PathValue("id"))
		if !ok || o.UserID != u.ID {
			return errors.New("兑换记录不存在")
		}
		return nil
	})
	if err != nil {
		commerceError(w, 404, err)
		return
	}
	WriteJSON(w, 200, o)
}
func (a *App) commerceCreateOrder(w http.ResponseWriter, r *http.Request) {
	u, err := a.User(r)
	if err != nil {
		commerceError(w, 401, err)
		return
	}
	var in struct {
		PlanID         string `json:"planId"`
		ChannelID      string `json:"channelId"`
		RequestID      string `json:"requestId"`
		ConfirmReplace bool   `json:"confirmReplace"`
	}
	if err = Decode(r, &in); err != nil {
		commerceError(w, 400, err)
		return
	}
	if err = commerceReqID(in.RequestID); err != nil {
		commerceError(w, 400, err)
		return
	}
	if !in.ConfirmReplace {
		commerceError(w, 400, errors.New("请确认新权益覆盖剩余时间和流量"))
		return
	}
	var out Order
	now := time.Now().UnixMilli()
	fingerprint := in.PlanID + ":" + in.ChannelID
	err = a.Store.Update(func(s *State) error {
		key := u.ID + ":" + in.RequestID
		if prev, ok := LoadDoc[commerceIdempotency](s, "order_requests", key); ok {
			if prev.Fingerprint != fingerprint {
				return errors.New("幂等键已用于其他兑换记录")
			}
			out, _ = LoadDoc[Order](s, "orders", prev.ObjectID)
			return nil
		}
		user := s.Users[u.ID]
		if boolSetting(s, "maintenance") {
			return errors.New("站点维护期间暂停新兑换")
		}
		if !boolSetting(s, "planSale") {
			return errors.New("权益兑换暂时关闭")
		}
		if user == nil || user.Status != "active" {
			return errors.New("账号已失效")
		}
		inferCurrentPlanID(s, user, now)
		if boolSetting(s, "purchaseRequireVerifiedEmail") && user.EmailVerifiedAt == 0 {
			return errors.New("请先验证邮箱再兑换权益")
		}
		if !purchaseAllowed(user, now) {
			return errors.New("剩余时间不少于30天且剩余流量不少于10GB，暂不可再次兑换")
		}
		p, ok := LoadDoc[Plan](s, "plans", in.PlanID)
		if !ok {
			return errors.New("权益不可兑换")
		}
		if p.PriceCents < 0 || p.PriceCents%100 != 0 {
			return errors.New("权益枫叶数配置无效，请联系管理员")
		}
		if err := planPolicyAllowed(s, p, user, now); err != nil {
			return err
		}
		if err := planPurchaseLimit(s, p, u.ID); err != nil {
			return err
		}
		for _, o := range ListDocs[Order](s, "orders") {
			if o.UserID == u.ID && o.State == "pending" && o.ExpiresAt > now {
				return errors.New("已有待处理兑换，请先完成或等待30分钟过期")
			}
		}
		out = Order{ID: commerceID(), UserID: u.ID, Plan: p, AmountCents: p.PriceCents, Currency: "CNY", ChannelID: in.ChannelID, State: "pending", RequestID: in.RequestID, CreatedAt: now, ExpiresAt: now + 30*60*1000, EntitlementVersion: entitlementVersion(s, u.ID)}
		if in.ChannelID == "balance" || p.PriceCents == 0 {
			out.ChannelID = "balance"
			if err := appendLedger(s, user, -p.PriceCents, "purchase", out.ID, "枫叶兑换权益", now); err != nil {
				return err
			}
			if err := fulfillOrder(s, &out, user, now); err != nil {
				return err
			}
		} else {
			ch, ok := LoadDoc[PaymentChannel](s, "payment_channels", in.ChannelID)
			if !ok || !ch.Enabled {
				return errors.New("兑换渠道不可用")
			}
			if ch.Type == "alipay" && !boolSetting(s, "payali") || ch.Type == "wxpay" && !boolSetting(s, "paywx") {
				return errors.New("此兑换方式暂时关闭")
			}
			payURL, err := a.paymentRedirect(ch, out)
			if err != nil {
				return err
			}
			out.PaymentURL = payURL
			if err := SaveDoc(s, "orders", out.ID, out); err != nil {
				return err
			}
		}
		return SaveDoc(s, "order_requests", key, commerceIdempotency{out.ID, fingerprint})
	})
	if err != nil {
		commerceError(w, 409, err)
		return
	}
	WriteJSON(w, 201, out)
}
func (a *App) StartCommerce(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				_ = a.Store.Update(func(s *State) error {
					for _, o := range ListDocs[Order](s, "orders") {
						if o.State == "pending" && o.ExpiresAt <= now.UnixMilli() {
							o.State = "expired"
							if err := SaveDoc(s, "orders", o.ID, o); err != nil {
								return err
							}
						}
					}
					return nil
				})
			}
		}
	}()
}

func centsString(n int64) string { return fmt.Sprintf("%d.%02d", n/100, n%100) }
