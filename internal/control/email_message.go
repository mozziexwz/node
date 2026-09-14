package control

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	htmltemplate "html/template"
	"net/url"
	"regexp"
	"strings"
	texttemplate "text/template"
	"time"
)

// EmailMessage holds both representations of one transactional message.
type EmailMessage struct{ Subject, TextBody, HTMLBody string }

// EmailData is supplied by the business flow. Expiry labels must come from the
// persisted challenge, never a fresh expiry calculated while rendering/retrying.
type EmailData struct {
	Code, RecipientMask          string
	TTLMinutes                   int
	ExpiresAtLabel, EventAtLabel string
	SiteURL                      string
	Year                         int
}

type emailMetadata struct {
	Subject, Preheader, Category, Title, Intro, Instruction string
	NoticeTitle, Notice, EventLabel, StatusText             string
	UsesCode                                                bool
}

//go:embed email_templates/*
var emailTemplates embed.FS

var emailCodeRE = regexp.MustCompile(`^[0-9]{6}$`)
var emailDisplayZone = time.FixedZone("UTC+8", 8*60*60)

func emailTimeLabel(atMillis int64) string {
	return time.UnixMilli(atMillis).In(emailDisplayZone).Format("2006-01-02 15:04:05") + " UTC+8"
}

func emailSiteURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Opaque != "" ||
		(u.Path != "" && u.Path != "/") || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.ContainsAny(raw, "\r\n#") {
		return ""
	}
	return strings.TrimRight(u.String(), "/")
}

// RenderEmail accepts only the five embedded account templates. It performs no
// delivery, business-state changes, logging, or user-provided HTML execution.
func RenderEmail(kind string, data EmailData) (EmailMessage, error) {
	var out EmailMessage
	raw, err := emailTemplates.ReadFile("email_templates/messages.zh-CN.json")
	if err != nil {
		return out, err
	}
	var catalog map[string]emailMetadata
	if err = json.Unmarshal(raw, &catalog); err != nil {
		return out, err
	}
	meta, ok := catalog[kind]
	if !ok {
		return out, errors.New("unknown account email template")
	}
	if data.Year < 2000 || data.Year > 9999 || len(data.RecipientMask) > 320 || len(data.ExpiresAtLabel) > 120 || len(data.EventAtLabel) > 120 {
		return out, errors.New("invalid email data")
	}
	if meta.UsesCode {
		if !emailCodeRE.MatchString(data.Code) || data.TTLMinutes < 1 || data.TTLMinutes > 120 || strings.TrimSpace(data.ExpiresAtLabel) == "" {
			return out, errors.New("real six-digit challenge and expiry are required")
		}
	} else if data.Code != "" || strings.TrimSpace(data.EventAtLabel) == "" {
		return out, errors.New("event email requires an event time and no code")
	}
	if data.SiteURL != "" {
		if data.SiteURL = emailSiteURL(data.SiteURL); data.SiteURL == "" {
			return out, errors.New("email site URL must be a trusted HTTPS origin")
		}
	}
	html, err := htmltemplate.New("message.html.tmpl").Funcs(htmltemplate.FuncMap{
		// Only fixed Outlook guards bypass escaping, never variable content.
		"msoStart": func() htmltemplate.HTML {
			return htmltemplate.HTML(`<!--[if mso]><table role="presentation" width="600" cellpadding="0" cellspacing="0" border="0"><tr><td><![endif]-->`)
		},
		"msoEnd": func() htmltemplate.HTML { return htmltemplate.HTML(`<!--[if mso]></td></tr></table><![endif]-->`) },
	}).Option("missingkey=error").ParseFS(emailTemplates, "email_templates/message.html.tmpl")
	if err != nil {
		return out, err
	}
	plain, err := texttemplate.New("message.txt.tmpl").Option("missingkey=error").ParseFS(emailTemplates, "email_templates/message.txt.tmpl")
	if err != nil {
		return out, err
	}
	view := struct {
		Meta emailMetadata
		Data EmailData
	}{meta, data}
	var h, t bytes.Buffer
	if err = html.Execute(&h, view); err != nil {
		return out, err
	}
	if err = plain.Execute(&t, view); err != nil {
		return out, err
	}
	return EmailMessage{Subject: meta.Subject, TextBody: t.String(), HTMLBody: h.String()}, nil
}

func maskedEmail(recipient string) string {
	parts := strings.SplitN(recipient, "@", 2)
	if len(parts) != 2 || parts[0] == "" {
		return ""
	}
	local := []rune(parts[0])
	if len(local) <= 2 {
		return "***@" + parts[1]
	}
	return string(local[:2]) + "***@" + parts[1]
}

func (a *App) sendAccountEmail(ctx context.Context, config SMTPConfig, recipient, kind string, data EmailData) error {
	// The caller cannot override the trusted deployment origin or recipient mask.
	// Local HTTP deployments intentionally omit links instead of weakening TLS.
	data.SiteURL = emailSiteURL(a.Config.PublicURL)
	data.RecipientMask = maskedEmail(recipient)
	if data.Year == 0 {
		data.Year = time.Now().In(emailDisplayZone).Year()
	}
	message, err := RenderEmail(kind, data)
	if err != nil {
		return err
	}
	secret, err := a.Open(config.Secret)
	if err != nil {
		return err
	}
	defer clear(secret)
	return a.mailSender(ctx, config, string(secret), recipient, message)
}
