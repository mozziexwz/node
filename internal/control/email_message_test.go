package control

import (
	"bytes"
	"context"
	"html"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"
	"testing"
	"time"
)

func emailTestData() EmailData {
	return EmailData{
		Code: "028461", RecipientMask: "12***@qq.com", TTLMinutes: 10,
		ExpiresAtLabel: "2026-09-15 09:10:00 UTC+8", EventAtLabel: "2026-09-15 09:00:00 UTC+8",
		SiteURL: "https://msboost.de", Year: 2026,
	}
}

func TestEmailTemplatesKindsAndSafeContent(t *testing.T) {
	for kind, subject := range map[string]string{
		"register": "[MSBOOST] 注册验证码", "verify": "[MSBOOST] 邮箱验证",
		"password_reset": "[MSBOOST] 找回密码验证码", "password_changed": "[MSBOOST] 密码重置成功通知",
		"smtp_test": "[MSBOOST] 邮件服务测试",
	} {
		t.Run(kind, func(t *testing.T) {
			data := emailTestData()
			usesCode := kind != "password_changed" && kind != "smtp_test"
			if !usesCode {
				data.Code = ""
			}
			m, err := RenderEmail(kind, data)
			if err != nil {
				t.Fatal(err)
			}
			if m.Subject != subject || strings.Contains(m.Subject, "028461") {
				t.Fatal("wrong or sensitive subject")
			}
			if !strings.Contains(m.HTMLBody, `color:#000000;">MSBOOST.</td>`) || strings.Contains(m.HTMLBody, ">BOOST</span>") {
				t.Fatal("entire MSBOOST. brand must be black")
			}
			if !strings.Contains(m.HTMLBody, "<!--[if mso]>") || !strings.Contains(m.HTMLBody, "600") {
				t.Fatal("Outlook layout guards missing")
			}
			for _, unsafe := range []string{"<script", "<iframe", "<form", "<img", "@import", "url(", "028461@"} {
				if strings.Contains(strings.ToLower(m.HTMLBody), unsafe) {
					t.Fatalf("unexpected active/external content %q", unsafe)
				}
			}
			if !strings.Contains(m.HTMLBody, `href="https://msboost.de"`) || !strings.Contains(m.TextBody, "访问 MSBOOST：https://msboost.de") {
				t.Fatal("trusted website missing")
			}
			if strings.Contains(m.HTMLBody[:strings.Index(m.HTMLBody, "<table")], "028461") {
				t.Fatal("code in preheader")
			}
			if usesCode {
				if strings.Count(m.HTMLBody, "028461") != 1 || strings.Count(m.TextBody, "028461") != 1 ||
					!strings.Contains(html.UnescapeString(m.HTMLBody), data.ExpiresAtLabel) || !strings.Contains(m.TextBody, data.ExpiresAtLabel) {
					t.Fatal("code/actual expiry differ between representations")
				}
			} else if strings.Contains(m.HTMLBody+m.TextBody, "028461") || !strings.Contains(m.TextBody, data.EventAtLabel) {
				t.Fatal("event email included a code or lost its actual time")
			}
		})
	}
}

func TestEmailRendererRejectsInvalidInputsAndEscapesHTML(t *testing.T) {
	mutations := map[string]func(*EmailData){
		"short code":      func(d *EmailData) { d.Code = "28461" },
		"HTML code":       func(d *EmailData) { d.Code = "<b>028461</b>" },
		"missing expiry":  func(d *EmailData) { d.ExpiresAtLabel = "" },
		"zero TTL":        func(d *EmailData) { d.TTLMinutes = 0 },
		"unbounded TTL":   func(d *EmailData) { d.TTLMinutes = 1000 },
		"invalid year":    func(d *EmailData) { d.Year = 0 },
		"HTTP":            func(d *EmailData) { d.SiteURL = "http://msboost.de" },
		"script URL":      func(d *EmailData) { d.SiteURL = "javascript:alert(1)" },
		"URL credentials": func(d *EmailData) { d.SiteURL = "https://user:pass@msboost.de" },
		"URL code query":  func(d *EmailData) { d.SiteURL = "https://msboost.de?code=028461" },
		"empty query":     func(d *EmailData) { d.SiteURL = "https://msboost.de?" },
		"fragment":        func(d *EmailData) { d.SiteURL = "https://msboost.de#code" },
		"empty fragment":  func(d *EmailData) { d.SiteURL = "https://msboost.de#" },
		"invented path":   func(d *EmailData) { d.SiteURL = "https://msboost.de/reset" },
		"CRLF":            func(d *EmailData) { d.SiteURL = "https://msboost.de\r\nBcc: bad@example.com" },
	}
	for name, change := range mutations {
		t.Run(name, func(t *testing.T) {
			d := emailTestData()
			change(&d)
			if _, err := RenderEmail("register", d); err == nil {
				t.Fatal("invalid data accepted")
			}
		})
	}
	if _, err := RenderEmail("../../secrets", emailTestData()); err == nil {
		t.Fatal("unknown template accepted")
	}
	if _, err := RenderEmail("password_changed", emailTestData()); err == nil {
		t.Fatal("event email accepted an extraneous secret code")
	}
	d := emailTestData()
	d.SiteURL = ""
	d.RecipientMask = `<script>alert("mail")</script>`
	m, err := RenderEmail("register", d)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(m.HTMLBody, "<script>") || !strings.Contains(m.HTMLBody, "&lt;script&gt;") || strings.Contains(m.HTMLBody, "href=") {
		t.Fatal("unescaped dynamic text or an invented website")
	}
	if !strings.Contains(m.TextBody, d.RecipientMask) {
		t.Fatal("plain representation lost the original text")
	}
}

func TestEmailMIMERoundTripsEveryKind(t *testing.T) {
	at := time.Date(2026, 9, 15, 1, 0, 0, 0, time.UTC)
	for _, kind := range []string{"register", "verify", "password_reset", "password_changed", "smtp_test"} {
		t.Run(kind, func(t *testing.T) {
			d := emailTestData()
			if kind == "password_changed" || kind == "smtp_test" {
				d.Code = ""
			}
			m, err := RenderEmail(kind, d)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := BuildMIME("MSBOOST 账号服务", "sender@example.com", "12345678@qq.com", m, at)
			if err != nil {
				t.Fatal(err)
			}
			wire := string(raw)
			if strings.Contains(strings.ReplaceAll(wire, "\r\n", ""), "\n") {
				t.Fatal("bare LF in MIME")
			}
			for _, line := range strings.Split(wire, "\r\n") {
				if len(line) > 998 {
					t.Fatal("RFC line limit exceeded")
				}
			}
			parsed, err := mail.ReadMessage(bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			subject, err := new(mime.WordDecoder).DecodeHeader(parsed.Header.Get("Subject"))
			if err != nil || subject != m.Subject {
				t.Fatal("Chinese subject round trip failed")
			}
			from, err := parsed.Header.AddressList("From")
			if err != nil || len(from) != 1 || from[0].Name != "MSBOOST 账号服务" {
				t.Fatal("sender name encoding failed")
			}
			if got, err := parsed.Header.Date(); err != nil || !got.Equal(at) {
				t.Fatal("missing/incorrect Date")
			}
			if !strings.HasSuffix(parsed.Header.Get("Message-ID"), "@example.com>") {
				t.Fatal("invalid Message-ID")
			}
			media, params, err := mime.ParseMediaType(parsed.Header.Get("Content-Type"))
			if err != nil || media != "multipart/alternative" {
				t.Fatal("not a multipart alternative")
			}
			reader := multipart.NewReader(parsed.Body, params["boundary"])
			for i, expected := range []string{m.TextBody, m.HTMLBody} {
				part, err := reader.NextPart()
				if err != nil {
					t.Fatal(err)
				}
				media, params, err := mime.ParseMediaType(part.Header.Get("Content-Type"))
				if err != nil || params["charset"] != "UTF-8" || media != []string{"text/plain", "text/html"}[i] {
					t.Fatal("wrong body order/type")
				}
				decoded, err := io.ReadAll(part)
				if err != nil || strings.ReplaceAll(string(decoded), "\r\n", "\n") != expected {
					t.Fatal("body encoding did not round trip")
				}
			}
			if _, err := reader.NextPart(); err != io.EOF {
				t.Fatal("unexpected MIME part")
			}
			raw2, err := BuildMIME("MSBOOST 账号服务", "sender@example.com", "12345678@qq.com", m, at)
			if err != nil {
				t.Fatal(err)
			}
			parsed2, err := mail.ReadMessage(bytes.NewReader(raw2))
			if err != nil || parsed2.Header.Get("Message-ID") == parsed.Header.Get("Message-ID") {
				t.Fatal("Message-ID not unique")
			}
		})
	}
}

func TestEmailMIMERejectsHeaderInjectionAndInvalidEnvelope(t *testing.T) {
	m, err := RenderEmail("register", emailTestData())
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, from, to, subject string }{
		{"bad\r\nBcc: a@example.com", "s@example.com", "r@example.com", "test"},
		{"name", "s@example.com\nBcc: x@example.com", "r@example.com", "test"},
		{"name", "s@example.com", "r@example.com\r\nBcc: x@example.com", "test"},
		{"name", "s@example.com", "r@example.com", "test\r\nBcc: x@example.com"},
		{"name", "display <s@example.com>", "r@example.com", "test"},
		{"name", "s@example.com", "r@example.com,second@example.com", "test"},
		{"name", "s@example.com", "邮箱@example.com", "test"},
	} {
		m.Subject = test.subject
		if _, err := BuildMIME(test.name, test.from, test.to, m, time.Now()); err == nil {
			t.Fatal("unsafe header accepted")
		}
	}
}

func TestAccountEmailUsesTrustedOriginAndPersistedExpiry(t *testing.T) {
	a, _ := identityFixture(t, false)
	a.Config.PublicURL = "https://trusted.example.com"
	secret, err := a.Seal([]byte("mail-secret"))
	if err != nil {
		t.Fatal(err)
	}
	d := emailTestData()
	d.SiteURL = "https://untrusted.example.com?code=028461"
	d.RecipientMask = "injected@example.com"
	var got EmailMessage
	a.mailSender = func(_ context.Context, _ SMTPConfig, clearSecret, recipient string, m EmailMessage) error {
		if clearSecret != "mail-secret" || recipient != "12345678@qq.com" {
			t.Fatal("incorrect delivery arguments")
		}
		got = m
		return nil
	}
	if err = a.sendAccountEmail(context.Background(), SMTPConfig{Secret: secret}, "12345678@qq.com", "register", d); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.HTMLBody, `href="https://trusted.example.com"`) || strings.Contains(got.HTMLBody+got.TextBody, "untrusted.example.com") ||
		strings.Contains(got.TextBody, "injected@example.com") || !strings.Contains(got.TextBody, "12***@qq.com") || !strings.Contains(got.TextBody, d.ExpiresAtLabel) {
		t.Fatal("caller-supplied link/mask used or actual expiry changed")
	}
	a.Config.PublicURL = "http://127.0.0.1:8080"
	if err = a.sendAccountEmail(context.Background(), SMTPConfig{Secret: secret}, "12345678@qq.com", "register", d); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got.HTMLBody, "href=") {
		t.Fatal("local HTTP link leaked into account mail")
	}
	if emailTimeLabel(time.Date(2026, 9, 15, 1, 10, 0, 0, time.UTC).UnixMilli()) != d.ExpiresAtLabel {
		t.Fatal("wrong display timezone")
	}
}
