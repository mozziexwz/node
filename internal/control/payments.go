package control

import (
	"crypto"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

type PaymentChannel struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	Version           string `json:"version"`
	Type              string `json:"type"`
	Gateway           string `json:"gateway"`
	MerchantID        string `json:"merchantId"`
	Enabled           bool   `json:"enabled"`
	MerchantKey       string `json:"merchantKey,omitempty"`
	PrivateKey        string `json:"privateKey,omitempty"`
	PlatformPublicKey string `json:"platformPublicKey,omitempty"`
	SealedSecrets     string `json:"sealedSecrets,omitempty"`
	Configured        bool   `json:"configured"`
	NotifyURL         string `json:"notifyUrl,omitempty"`
	ReturnURLTemplate string `json:"returnUrlTemplate,omitempty"`
}
type paymentSecrets struct {
	MerchantKey       string `json:"merchantKey"`
	PrivateKey        string `json:"privateKey"`
	PlatformPublicKey string `json:"platformPublicKey"`
}

func paymentCanonical(values url.Values) (string, error) {
	keys := []string{}
	for k, v := range values {
		if len(v) != 1 {
			return "", errors.New("重复支付参数")
		}
		if k != "sign" && k != "sign_type" && v[0] != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+values.Get(k))
	}
	return strings.Join(out, "&"), nil
}
func paymentMD5(values url.Values, key string) (string, error) {
	plain, err := paymentCanonical(values)
	if err != nil {
		return "", err
	}
	sum := md5.Sum([]byte(plain + key))
	return hex.EncodeToString(sum[:]), nil
}
func paymentDER(value string) ([]byte, error) {
	if block, _ := pem.Decode([]byte(value)); block != nil {
		return block.Bytes, nil
	}
	return base64.StdEncoding.DecodeString(strings.Join(strings.Fields(value), ""))
}
func paymentPrivate(value string) (*rsa.PrivateKey, error) {
	der, err := paymentDER(value)
	if err != nil {
		return nil, errors.New("商户私钥格式无效")
	}
	if key, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		if key.N.BitLen() < 2048 {
			return nil, errors.New("RSA密钥至少2048位")
		}
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		return nil, errors.New("商户私钥须为RSA PEM/PKCS8")
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok || key.N.BitLen() < 2048 {
		return nil, errors.New("商户私钥须为至少2048位RSA")
	}
	return key, nil
}
func paymentPublic(value string) (*rsa.PublicKey, error) {
	der, err := paymentDER(value)
	if err != nil {
		return nil, errors.New("平台公钥格式无效")
	}
	if key, err := x509.ParsePKCS1PublicKey(der); err == nil {
		if key.N.BitLen() < 2048 {
			return nil, errors.New("RSA密钥至少2048位")
		}
		return key, nil
	}
	parsed, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, errors.New("平台公钥须为RSA PEM/PKIX")
	}
	key, ok := parsed.(*rsa.PublicKey)
	if !ok || key.N.BitLen() < 2048 {
		return nil, errors.New("平台公钥须为至少2048位RSA")
	}
	return key, nil
}
func paymentSign(values url.Values, version string, secrets paymentSecrets) (string, error) {
	if version == "v1" {
		return paymentMD5(values, secrets.MerchantKey)
	}
	canonical, err := paymentCanonical(values)
	if err != nil {
		return "", err
	}
	key, err := paymentPrivate(secrets.PrivateKey)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(canonical))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	return base64.StdEncoding.EncodeToString(sig), err
}
func paymentVerify(values url.Values, version string, secrets paymentSecrets) error {
	if version == "v1" {
		if values.Get("sign_type") != "" && values.Get("sign_type") != "MD5" {
			return errors.New("签名类型错误")
		}
		expected, err := paymentMD5(values, secrets.MerchantKey)
		if err != nil {
			return err
		}
		if subtle.ConstantTimeCompare([]byte(expected), []byte(strings.ToLower(values.Get("sign")))) != 1 {
			return errors.New("支付签名验证失败")
		}
		return nil
	}
	if version != "v2" || values.Get("sign_type") != "" && values.Get("sign_type") != "RSA" {
		return errors.New("签名类型错误")
	}
	canonical, err := paymentCanonical(values)
	if err != nil {
		return err
	}
	key, err := paymentPublic(secrets.PlatformPublicKey)
	if err != nil {
		return err
	}
	sig, err := base64.StdEncoding.DecodeString(values.Get("sign"))
	if err != nil {
		return errors.New("支付签名格式无效")
	}
	sum := sha256.Sum256([]byte(canonical))
	if rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], sig) != nil {
		return errors.New("支付签名验证失败")
	}
	return nil
}
func paymentCents(value string) (int64, error) {
	parts := strings.Split(value, ".")
	if len(parts) > 2 || len(parts[0]) == 0 || len(parts[0]) > 9 {
		return 0, errors.New("支付金额格式无效")
	}
	for _, part := range parts {
		for _, c := range part {
			if c < '0' || c > '9' {
				return 0, errors.New("支付金额格式无效")
			}
		}
	}
	n, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, err
	}
	fraction := "00"
	if len(parts) == 2 {
		if len(parts[1]) < 1 || len(parts[1]) > 2 {
			return 0, errors.New("金额精度不得超过分")
		}
		fraction = (parts[1] + "0")[:2]
	}
	f, _ := strconv.ParseInt(fraction, 10, 64)
	return n*100 + f, nil
}
func (a *App) paymentSecrets(ch PaymentChannel) (paymentSecrets, error) {
	var secrets paymentSecrets
	raw, err := a.Open(ch.SealedSecrets)
	if err != nil {
		return secrets, errors.New("支付渠道密钥解密失败")
	}
	err = json.Unmarshal(raw, &secrets)
	return secrets, err
}
func (a *App) paymentRedirect(ch PaymentChannel, o Order) (string, error) {
	if !strings.HasPrefix(a.Config.PublicURL, "https://") {
		return "", errors.New("在线支付须先配置本站 HTTPS PUBLIC_URL")
	}
	secret, err := a.paymentSecrets(ch)
	if err != nil {
		return "", err
	}
	values := url.Values{"pid": {ch.MerchantID}, "type": {ch.Type}, "out_trade_no": {o.ID}, "notify_url": {a.Config.PublicURL + "/api/payments/epay/" + ch.ID + "/notify"}, "return_url": {a.Config.PublicURL + "/?order=" + o.ID}, "name": {o.Plan.Name}, "money": {centsString(o.AmountCents)}}
	path := "/submit.php"
	signType := "MD5"
	if ch.Version == "v2" {
		values.Set("timestamp", strconv.FormatInt(time.Now().Unix(), 10))
		path = "/api/pay/submit"
		signType = "RSA"
	}
	signature, err := paymentSign(values, ch.Version, secret)
	if err != nil {
		return "", err
	}
	values.Set("sign", signature)
	values.Set("sign_type", signType)
	return strings.TrimRight(ch.Gateway, "/") + path + "?" + values.Encode(), nil
}
func (a *App) registerPayments(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/payment-channels", a.paymentChannels)
	mux.HandleFunc("GET /api/admin/payment-channels", a.paymentChannels)
	mux.HandleFunc("POST /api/admin/payment-channels", a.paymentSaveChannel)
	mux.HandleFunc("PUT /api/admin/payment-channels/{id}", a.paymentSaveChannel)
	mux.HandleFunc("DELETE /api/admin/payment-channels/{id}", a.paymentDeleteChannel)
	mux.HandleFunc("GET /api/payments/epay/{channel}/notify", a.paymentNotify)
	mux.HandleFunc("POST /api/payments/epay/{channel}/notify", a.paymentNotify)
}
func (a *App) paymentChannels(w http.ResponseWriter, r *http.Request) {
	admin := strings.Contains(r.URL.Path, "/admin/")
	if admin {
		if _, err := a.Admin(r); err != nil {
			commerceError(w, 403, err)
			return
		}
	}
	out := []PaymentChannel{}
	err := a.Store.View(func(s *State) error {
		for _, ch := range ListDocs[PaymentChannel](s, "payment_channels") {
			if !admin && !ch.Enabled {
				continue
			}
			if !admin && (ch.Type == "alipay" && !boolSetting(s, "payali") || ch.Type == "wxpay" && !boolSetting(s, "paywx")) {
				continue
			}
			ch.Configured = ch.SealedSecrets != ""
			ch.SealedSecrets = ""
			ch.NotifyURL = a.Config.PublicURL + "/api/payments/epay/" + ch.ID + "/notify"
			ch.ReturnURLTemplate = a.Config.PublicURL + "/?order={orderId}"
			if !admin {
				ch.MerchantID = ""
				ch.Gateway = ""
				ch.NotifyURL = ""
				ch.ReturnURLTemplate = ""
			}
			out = append(out, ch)
		}
		return nil
	})
	if err != nil {
		commerceError(w, 500, err)
		return
	}
	WriteJSON(w, 200, map[string]any{"channels": out})
}
func (a *App) paymentSaveChannel(w http.ResponseWriter, r *http.Request) {
	if _, err := a.Admin(r); err != nil {
		commerceError(w, 403, err)
		return
	}
	var ch PaymentChannel
	if err := Decode(r, &ch); err != nil {
		commerceError(w, 400, err)
		return
	}
	gateway, err := url.Parse(ch.Gateway)
	if err != nil || gateway.Scheme != "https" || gateway.Hostname() == "" || gateway.User != nil || gateway.RawQuery != "" || gateway.Fragment != "" || len(ch.Name) < 1 || len(ch.Name) > 100 || ch.MerchantID == "" || ch.Type != "alipay" && ch.Type != "wxpay" || ch.Version != "v1" && ch.Version != "v2" {
		commerceError(w, 400, errors.New("请填写HTTPS网关、商户号、v1/v2、支付宝或微信渠道"))
		return
	}
	ch.ID = r.PathValue("id")
	err = a.Store.Update(func(s *State) error {
		var old PaymentChannel
		if ch.ID != "" {
			var ok bool
			old, ok = LoadDoc[PaymentChannel](s, "payment_channels", ch.ID)
			if !ok {
				return errors.New("支付渠道不存在")
			}
			for _, o := range ListDocs[Order](s, "orders") {
				if o.ChannelID == ch.ID && (ch.Version != old.Version || ch.MerchantID != old.MerchantID || ch.Type != old.Type || ch.Gateway != old.Gateway || ch.MerchantKey != "" || ch.PrivateKey != "" || ch.PlatformPublicKey != "") {
					return errors.New("此渠道已被订单引用，商户与密钥不可覆盖；轮换请新建渠道，可修改名称或启停")
				}
			}
		} else {
			ch.ID = commerceID()
		}
		secrets := paymentSecrets{}
		if old.Version == ch.Version && old.SealedSecrets != "" {
			var err error
			secrets, err = a.paymentSecrets(old)
			if err != nil {
				return err
			}
		}
		if ch.MerchantKey != "" {
			secrets.MerchantKey = ch.MerchantKey
		}
		if ch.PrivateKey != "" {
			secrets.PrivateKey = ch.PrivateKey
		}
		if ch.PlatformPublicKey != "" {
			secrets.PlatformPublicKey = ch.PlatformPublicKey
		}
		if ch.Version == "v1" {
			if len(secrets.MerchantKey) < 8 {
				return errors.New("v1必须填写有效商户密钥")
			}
			secrets.PrivateKey = ""
			secrets.PlatformPublicKey = ""
		} else {
			if _, err := paymentPrivate(secrets.PrivateKey); err != nil {
				return err
			}
			if _, err := paymentPublic(secrets.PlatformPublicKey); err != nil {
				return err
			}
			secrets.MerchantKey = ""
		}
		raw, err := json.Marshal(secrets)
		if err != nil {
			return err
		}
		ch.SealedSecrets, err = a.Seal(raw)
		if err != nil {
			return err
		}
		ch.MerchantKey = ""
		ch.PrivateKey = ""
		ch.PlatformPublicKey = ""
		ch.Configured = true
		return SaveDoc(s, "payment_channels", ch.ID, ch)
	})
	if err != nil {
		commerceError(w, 409, err)
		return
	}
	ch.SealedSecrets = ""
	WriteJSON(w, 200, ch)
}
func (a *App) paymentDeleteChannel(w http.ResponseWriter, r *http.Request) {
	if _, err := a.Admin(r); err != nil {
		commerceError(w, 403, err)
		return
	}
	err := a.Store.Update(func(s *State) error {
		for _, o := range ListDocs[Order](s, "orders") {
			if o.ChannelID == r.PathValue("id") {
				return errors.New("支付渠道已被订单引用，请停用而非删除")
			}
		}
		DeleteDoc(s, "payment_channels", r.PathValue("id"))
		return nil
	})
	if err != nil {
		commerceError(w, 409, err)
		return
	}
	WriteJSON(w, 200, map[string]bool{"ok": true})
}
func (a *App) paymentNotify(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "fail", 400)
		return
	}
	values := r.Form
	var ch PaymentChannel
	err := a.Store.View(func(s *State) error {
		var ok bool
		ch, ok = LoadDoc[PaymentChannel](s, "payment_channels", r.PathValue("channel"))
		if !ok {
			return errors.New("unknown channel")
		}
		return nil
	})
	if err != nil {
		http.Error(w, "fail", 400)
		return
	}
	secrets, err := a.paymentSecrets(ch)
	if err == nil {
		err = paymentVerify(values, ch.Version, secrets)
	}
	amount, amountErr := paymentCents(values.Get("money"))
	if err != nil || amountErr != nil || values.Get("pid") != ch.MerchantID || values.Get("trade_status") != "TRADE_SUCCESS" || values.Get("type") != ch.Type || values.Get("trade_no") == "" || (values.Get("currency") != "" && strings.ToUpper(values.Get("currency")) != "CNY") {
		http.Error(w, "fail", 400)
		return
	}
	now := time.Now().UnixMilli()
	err = a.Store.Update(func(s *State) error {
		o, ok := LoadDoc[Order](s, "orders", values.Get("out_trade_no"))
		if !ok || o.ChannelID != ch.ID || o.AmountCents != amount || o.Currency != "CNY" {
			return errors.New("order mismatch")
		}
		tradeKey := ch.ID + ":" + values.Get("trade_no")
		if existing, ok := LoadDoc[string](s, "payment_trades", tradeKey); ok && existing != o.ID {
			return errors.New("trade reused")
		}
		if o.State == "paid" || o.State == "paid_review" {
			if o.TradeNo != values.Get("trade_no") {
				return errors.New("trade mismatch")
			}
			return nil
		}
		if o.State != "pending" && o.State != "expired" {
			return errors.New("invalid order state")
		}
		o.TradeNo = values.Get("trade_no")
		o.PaidAt = now
		u := s.Users[o.UserID]
		switch {
		case boolSetting(s, "maintenance"):
			o.ReviewReason = "站点维护期间收到付款，请核对后人工处理"
		case u == nil || u.Status != "active":
			o.ReviewReason = "用户不存在或已停用"
		case boolSetting(s, "purchaseRequireVerifiedEmail") && u.EmailVerifiedAt == 0:
			o.ReviewReason = "当前购买要求验证邮箱，请核对后处理"
		case planPurchaseLimit(s, o.Plan, o.UserID) != nil:
			o.ReviewReason = "已达到该套餐每用户购买次数上限"
		case o.ExpiresAt <= now:
			o.ReviewReason = "付款通知超过订单有效期"
		case entitlementVersion(s, o.UserID) != o.EntitlementVersion:
			o.ReviewReason = "订单创建后用户权益已变更"
		case !purchaseAllowed(u, now):
			o.ReviewReason = "用户已不满足再次购买门槛"
		}
		if o.ReviewReason != "" {
			o.State = "paid_review"
			if err := SaveDoc(s, "orders", o.ID, o); err != nil {
				return err
			}
		} else {
			if err := fulfillOrder(s, &o, u, now); err != nil {
				return err
			}
		}
		return SaveDoc(s, "payment_trades", tradeKey, o.ID)
	})
	if err != nil {
		http.Error(w, "fail", 400)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprint(w, "success")
}
