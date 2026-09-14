package control

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

// sendSMTP returns success once the server has accepted DATA. This is SMTP
// acceptance, not a guarantee that the message reaches the recipient's inbox.
func sendSMTP(ctx context.Context, config SMTPConfig, secret, recipient string, message EmailMessage) error {
	if config.Encryption != "tls" && config.Encryption != "starttls" {
		return errors.New("SMTP requires TLS or STARTTLS")
	}
	raw, err := BuildMIME(config.Name, config.Sender, recipient, message, time.Now())
	if err != nil {
		return err
	}
	address := net.JoinHostPort(config.Host, strconv.Itoa(config.Port))
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	tlsConfig := &tls.Config{ServerName: config.Host, MinVersion: tls.VersionTLS12}
	var conn net.Conn
	if config.Encryption == "tls" {
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: tlsConfig}).DialContext(ctx, "tcp", address)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", address)
	}
	if err != nil {
		return err
	}
	defer conn.Close()
	stopCancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopCancel()
	deadline := time.Now().Add(25 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err = conn.SetDeadline(deadline); err != nil {
		return err
	}
	client, err := smtp.NewClient(conn, config.Host)
	if err != nil {
		return err
	}
	defer client.Close()
	if config.Encryption == "starttls" {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return errors.New("SMTP server does not support required STARTTLS")
		}
		if err = client.StartTLS(tlsConfig); err != nil {
			return err
		}
	}
	if err = client.Auth(smtp.PlainAuth("", config.Sender, secret, config.Host)); err != nil {
		return err
	}
	return submitSMTP(client, config.Sender, recipient, raw)
}

type smtpSubmissionClient interface {
	Mail(string) error
	Rcpt(string) error
	Data() (io.WriteCloser, error)
	Quit() error
}

func submitSMTP(client smtpSubmissionClient, from, recipient string, raw []byte) error {
	if err := client.Mail(from); err != nil {
		return err
	}
	if err := client.Rcpt(recipient); err != nil {
		return err
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	n, err := w.Write(raw)
	if err != nil {
		// Do not Close the DATA writer after a failed write: that could submit
		// a truncated message. The connection owner closes the SMTP client.
		return err
	}
	if n != len(raw) {
		return io.ErrShortWrite
	}
	if err = w.Close(); err != nil {
		return err
	}
	// DATA's final positive reply is the acceptance boundary. A failed QUIT
	// must not invalidate an already emailed challenge or trigger a duplicate.
	_ = client.Quit()
	return nil
}

// BuildMIME constructs UTF-8 multipart/alternative bytes. It does not transmit mail.
// Envelope MAIL FROM / RCPT TO must still use parsed bare addresses in the SMTP layer.
// Sign with DKIM at the mail service; MIME alone does not provide authentication.
func BuildMIME(fromName, fromAddress, toAddress string, m EmailMessage, at time.Time) ([]byte, error) {
	for _, s := range []string{fromName, fromAddress, toAddress, m.Subject} {
		if strings.ContainsAny(s, "\r\n") {
			return nil, fmt.Errorf("CR/LF in a header")
		}
	}
	parse := func(s string) (string, error) {
		a, err := mail.ParseAddress(s)
		if err != nil || a.Address != s {
			return "", fmt.Errorf("invalid bare address")
		}
		// This reference deliberately avoids requiring SMTPUTF8 for envelope addresses.
		for _, r := range s {
			if r > 127 {
				return "", fmt.Errorf("non-ASCII envelope address unsupported")
			}
		}
		return a.Address, nil
	}
	from, err := parse(fromAddress)
	if err != nil {
		return nil, err
	}
	to, err := parse(toAddress)
	if err != nil {
		return nil, err
	}
	if m.Subject == "" || m.HTMLBody == "" || m.TextBody == "" || at.IsZero() {
		return nil, fmt.Errorf("both bodies, subject, and date are required")
	}
	if len(fromName) > 150 || len(m.Subject) > 200 || len(from) > 254 || len(to) > 254 {
		return nil, fmt.Errorf("header too long")
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for _, p := range []struct{ kind, body string }{{"text/plain", m.TextBody}, {"text/html", m.HTMLBody}} {
		hdr := textproto.MIMEHeader{}
		hdr.Set("Content-Type", p.kind+`; charset="UTF-8"`)
		hdr.Set("Content-Transfer-Encoding", "quoted-printable")
		part, err := mw.CreatePart(hdr)
		if err != nil {
			return nil, err
		}
		qp := quotedprintable.NewWriter(part)
		if _, err = io.WriteString(qp, p.body); err != nil {
			return nil, err
		}
		if err = qp.Close(); err != nil {
			return nil, err
		}
	}
	if err = mw.Close(); err != nil {
		return nil, err
	}
	var id [16]byte
	if _, err = rand.Read(id[:]); err != nil {
		return nil, err
	}
	domain := from[strings.LastIndex(from, "@")+1:]
	fromHeader := (&mail.Address{Name: fromName, Address: from}).String()
	toHeader := (&mail.Address{Address: to}).String()
	subject := mime.BEncoding.Encode("UTF-8", m.Subject)
	// Break between encoded words; never split a multibyte character or encoded word.
	subject = strings.ReplaceAll(subject, "?= =?", "?=\r\n =?")
	var message bytes.Buffer
	fmt.Fprintf(&message, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMessage-ID: <%s@%s>\r\nMIME-Version: 1.0\r\nContent-Type: multipart/alternative; boundary=\"%s\"\r\n\r\n",
		fromHeader, toHeader, subject, at.Format(time.RFC1123Z), hex.EncodeToString(id[:]), domain, mw.Boundary())
	message.Write(body.Bytes())
	message.WriteString("\r\n")
	return message.Bytes(), nil
}
