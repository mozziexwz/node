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
	ID                  string `json:"id"`
	Name                string `json:"name"`
	PriceCents          int64  `json:"priceCents"`
	Days                int64  `json:"days"`
	TrafficBytes        int64  `json:"trafficBytes"`
	RateMbps            int64  `json:"rateMbps"`
	Enabled             bool   `json:"enabled"`
	Version             int64  `json:"version"`
	CreatedAt           int64  `json:"createdAt"`
	MaxPurchasesPerUser int64  `json:"maxPurchasesPerUser"`
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
	Status      string `json:"status"`
	Batch       string `json:"batch"`
	UsedBy      string `json:"usedBy,omitempty"`
	UsedByEmail string `json:"usedByEmail,omitempty"`
	CreatedAt   int64  `json:"createdAt"`
	UsedAt      int64  `json:"usedAt,omitempty"`
}
type Order struct {
	ID                 string `json:"id"`
	UserID             string `json:"userId"`
	UserEmail          string `json:"userEmail,omitempty"`
	Plan               Plan   `json:"plan"`
	AmountCents        int64  `json:"amountCents"`
	Currency           string `json:"currency"`
	ChannelID          string `json:"channelId"`
	State              string `json:"state"`
	RequestID          string `json:"requestId"`
	TradeNo            string `json:"tradeNo,omitempty"`
	PaymentURL         string `json:"paymentUrl,omitempty"`
	CreatedAt          int64  `json:"createdAt"`
	ExpiresAt          int64  `json:"expiresAt"`
	PaidAt             int64  `json:"paidAt,omitempty"`
	EntitlementVersion int64  `json:"entitlementVersion"`
	ReviewReason       string `json:"reviewReason,omitempty"`
}
type commerceIdempotency struct {
	ObjectID    string `json:"objectId"`
	Fingerprint string `json:"fingerprint"`
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
func entitlementVersion(s *State, user string) int64 {
	v, _ := LoadDoc[int64](s, "entitlement_versions", user)
	return v
}
func appendLedger(s *State, u *User, amount int64, kind, reference, reason string, now int64) error {
	if u == nil || amount == math.MinInt64 || (amount > 0 && u.BalanceCents > math.MaxInt64-amount) || (amount < 0 && u.BalanceCents < -amount) {
		return errors.New("余额不足或金额溢出")
	}
	u.BalanceCents += amount
	e := LedgerEntry{ID: commerceID(), UserID: u.ID, Kind: kind, AmountCents: amount, BalanceAfter: u.BalanceCents, Reference: reference, Reason: reason, CreatedAt: now}
	return SaveDoc(s, "ledger", e.ID, e)
}
func fulfillOrder(s *State, o *Order, u *User, now int64) error {
	if err := planPurchaseLimit(s, o.Plan, u.ID); err != nil {
		return err
	}
	u.ExpiresAt = now + o.Plan.Days*commerceDay
	u.TrafficTotal = o.Plan.TrafficBytes
	u.TrafficUsed = 0
	if o.Plan.RateMbps > 0 {
		u.RateMbps = o.Plan.RateMbps
	}
	o.State = "paid"
	o.PaidAt = now
	if err := SaveDoc(s, "entitlement_versions", u.ID, entitlementVersion(s, u.ID)+1); err != nil {
		return err
	}
	return SaveDoc(s, "orders", o.ID, o)
}

// Count committed fulfillments, never requests, so retries do not consume a
// purchase and every payment path observes the same transaction-locked limit.
func planPurchaseLimit(s *State, plan Plan, userID string) error {
	limit := plan.MaxPurchasesPerUser
	if current, ok := LoadDoc[Plan](s, "plans", plan.ID); ok && current.MaxPurchasesPerUser > 0 && (limit == 0 || current.MaxPurchasesPerUser < limit) {
		limit = current.MaxPurchasesPerUser
	}
	if limit == 0 {
		return nil
	}
	var used int64
	for _, o := range ListDocs[Order](s, "orders") {
		if o.UserID == userID && o.Plan.ID == plan.ID && o.State == "paid" {
			used++
		}
	}
	if used >= limit {
		return errors.New("已达到该套餐每用户购买次数上限")
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
	out := []Plan{}
	purchasedCounts := map[string]int64{}
	viewer, _ := a.User(r)
	requireVerifiedEmail := false
	err := a.Store.View(func(s *State) error {
		requireVerifiedEmail = boolSetting(s, "purchaseRequireVerifiedEmail")
		if viewer != nil {
			for _, order := range ListDocs[Order](s, "orders") {
				if order.UserID == viewer.ID && order.State == "paid" {
					purchasedCounts[order.Plan.ID]++
				}
			}
		}
		for _, p := range ListDocs[Plan](s, "plans") {
			if p.Enabled || admin {
				out = append(out, p)
			}
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
	if strings.TrimSpace(p.Name) == "" || len(p.Name) > 100 || p.Days < 1 || p.Days > 31 || p.PriceCents < 0 || p.PriceCents > 100000000 || p.TrafficBytes < 1 || p.TrafficBytes > 1000000000000000 || p.RateMbps < 1 || p.RateMbps > 100000 || p.MaxPurchasesPerUser < 0 || p.MaxPurchasesPerUser > 1000000 {
		commerceError(w, 400, errors.New("套餐名称、金额、1–31 天、流量和速率无效"))
		return
	}
	p.ID = r.PathValue("id")
	err := a.Store.Update(func(s *State) error {
		if p.ID != "" {
			old, ok := LoadDoc[Plan](s, "plans", p.ID)
			if !ok {
				return errors.New("套餐不存在")
			}
			p.CreatedAt = old.CreatedAt
			p.Version = old.Version + 1
		} else {
			p.ID = commerceID()
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
func (a *App) commerceDeletePlan(w http.ResponseWriter, r *http.Request) {
	if _, err := a.Admin(r); err != nil {
		commerceError(w, 403, err)
		return
	}
	err := a.Store.Update(func(s *State) error {
		for _, o := range ListDocs[Order](s, "orders") {
			if o.Plan.ID == r.PathValue("id") {
				return errors.New("套餐已被订单引用，请下架而非删除")
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
					if card, ok := LoadDoc[BalanceCard](s, "cards", e.Reference); ok && card.UsedBy == u.ID {
						plain, err := a.Open(card.SealedCode)
						if err != nil {
							return errors.New("卡密记录解密失败")
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
				return errors.New("幂等键已用于另一卡密")
			}
			balance = s.Users[u.ID].BalanceCents
			return nil
		}
		if boolSetting(s, "maintenance") {
			return errors.New("站点维护期间暂停新卡密兑换")
		}
		if !boolSetting(s, "cards") {
			return errors.New("卡密兑换暂时关闭")
		}
		if s.Users[u.ID] == nil || s.Users[u.ID].Status != "active" {
			return errors.New("账号已失效")
		}
		for _, card := range ListDocs[BalanceCard](s, "cards") {
			if card.CodeHash != hash {
				continue
			}
			if card.UsedBy != "" {
				return errors.New("该卡密已被使用")
			}
			if card.Status != "active" {
				return errors.New("卡密不可用")
			}
			user := s.Users[u.ID]
			if err := appendLedger(s, user, card.AmountCents, "card", card.ID, "余额卡密兑换", time.Now().UnixMilli()); err != nil {
				return err
			}
			card.Status = "used"
			card.UsedBy = u.ID
			card.UsedAt = time.Now().UnixMilli()
			if err := SaveDoc(s, "cards", card.ID, card); err != nil {
				return err
			}
			if err := SaveDoc(s, "redeem_requests", key, commerceIdempotency{card.ID, hash}); err != nil {
				return err
			}
			balance = user.BalanceCents
			return nil
		}
		return errors.New("卡密不存在")
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
				return errors.New("卡密解密失败")
			}
			c.Code = string(plain)
			c.SealedCode = ""
			c.CodeHash = ""
			if user := s.Users[c.UsedBy]; user != nil {
				c.UsedByEmail = user.Email
			}
			out = append(out, c)
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
	}
	if err := Decode(r, &in); err != nil {
		commerceError(w, 400, err)
		return
	}
	if in.Count < 1 || in.Count > 1000 || in.AmountCents < 1 || in.AmountCents > 100000000 || len(in.Batch) > 100 {
		commerceError(w, 400, errors.New("数量须为1–1000且面额有效"))
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
			c := BalanceCard{ID: commerceID(), CodeHash: commerceHash(code), SealedCode: sealed, AmountCents: in.AmountCents, Status: "active", Batch: in.Batch, CreatedAt: time.Now().UnixMilli()}
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
		commerceError(w, 400, errors.New("请选中1–1000张卡密"))
		return
	}
	out := []BalanceCard{}
	err = a.Store.Update(func(s *State) error {
		for _, id := range in.IDs {
			c, ok := LoadDoc[BalanceCard](s, "cards", id)
			if !ok {
				return errors.New("卡密不存在")
			}
			switch in.Action {
			case "enable":
				if c.UsedBy != "" {
					return errors.New("已使用的卡密不可重新启用")
				}
				c.Status = "active"
			case "disable":
				if c.UsedBy == "" {
					c.Status = "disabled"
				}
			case "delete":
				if c.UsedBy != "" {
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
			out = append(out, c)
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
			return errors.New("订单不存在")
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
		commerceError(w, 400, errors.New("请确认新套餐覆盖剩余时间和流量"))
		return
	}
	var out Order
	now := time.Now().UnixMilli()
	fingerprint := in.PlanID + ":" + in.ChannelID
	err = a.Store.Update(func(s *State) error {
		key := u.ID + ":" + in.RequestID
		if prev, ok := LoadDoc[commerceIdempotency](s, "order_requests", key); ok {
			if prev.Fingerprint != fingerprint {
				return errors.New("幂等键已用于其他订单")
			}
			out, _ = LoadDoc[Order](s, "orders", prev.ObjectID)
			return nil
		}
		user := s.Users[u.ID]
		if boolSetting(s, "maintenance") {
			return errors.New("站点维护期间暂停新订单")
		}
		if !boolSetting(s, "planSale") {
			return errors.New("套餐销售暂时关闭")
		}
		if user == nil || user.Status != "active" {
			return errors.New("账号已失效")
		}
		if boolSetting(s, "purchaseRequireVerifiedEmail") && user.EmailVerifiedAt == 0 {
			return errors.New("请先验证邮箱再购买套餐")
		}
		if !purchaseAllowed(user, now) {
			return errors.New("剩余时间不少于30天且剩余流量不少于10GB，暂不可再次购买")
		}
		p, ok := LoadDoc[Plan](s, "plans", in.PlanID)
		if !ok || !p.Enabled {
			return errors.New("套餐不可购买")
		}
		if err := planPurchaseLimit(s, p, u.ID); err != nil {
			return err
		}
		for _, o := range ListDocs[Order](s, "orders") {
			if o.UserID == u.ID && o.State == "pending" && o.ExpiresAt > now {
				return errors.New("已有待支付订单，请先完成或等待30分钟过期")
			}
		}
		out = Order{ID: commerceID(), UserID: u.ID, Plan: p, AmountCents: p.PriceCents, Currency: "CNY", ChannelID: in.ChannelID, State: "pending", RequestID: in.RequestID, CreatedAt: now, ExpiresAt: now + 30*60*1000, EntitlementVersion: entitlementVersion(s, u.ID)}
		if in.ChannelID == "balance" || p.PriceCents == 0 {
			out.ChannelID = "balance"
			if err := appendLedger(s, user, -p.PriceCents, "purchase", out.ID, "余额购买套餐", now); err != nil {
				return err
			}
			if err := fulfillOrder(s, &out, user, now); err != nil {
				return err
			}
		} else {
			ch, ok := LoadDoc[PaymentChannel](s, "payment_channels", in.ChannelID)
			if !ok || !ch.Enabled {
				return errors.New("支付渠道不可用")
			}
			if ch.Type == "alipay" && !boolSetting(s, "payali") || ch.Type == "wxpay" && !boolSetting(s, "paywx") {
				return errors.New("此支付方式暂时关闭")
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
