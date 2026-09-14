package control

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/smtp"
	"net/textproto"
	"strings"
	"testing"
	"time"
)

type emailDeliveryStub struct {
	stage      string
	quitErr    error
	quitCalled bool
	closed     bool
	data       bytes.Buffer
}

func (c *emailDeliveryStub) fail(stage string) error {
	if c.stage == stage {
		return errors.New("simulated " + stage + " failure")
	}
	return nil
}
func (c *emailDeliveryStub) Mail(string) error             { return c.fail("mail") }
func (c *emailDeliveryStub) Rcpt(string) error             { return c.fail("rcpt") }
func (c *emailDeliveryStub) Data() (io.WriteCloser, error) { return c, c.fail("data") }
func (c *emailDeliveryStub) Write(p []byte) (int, error) {
	if err := c.fail("write"); err != nil {
		return 0, err
	}
	return c.data.Write(p)
}
func (c *emailDeliveryStub) Close() error { c.closed = true; return c.fail("accept") }
func (c *emailDeliveryStub) Quit() error  { c.quitCalled = true; return c.quitErr }

func TestEmailSMTPSubmissionAcceptanceBoundary(t *testing.T) {
	for _, stage := range []string{"mail", "rcpt", "data", "write", "accept"} {
		t.Run(stage, func(t *testing.T) {
			client := &emailDeliveryStub{stage: stage}
			if err := submitSMTP(client, "s@example.com", "r@example.com", []byte("message")); err == nil {
				t.Fatal("unaccepted message reported success")
			}
			if client.quitCalled {
				t.Fatal("QUIT used after failed submission")
			}
			if stage == "write" && client.closed {
				t.Fatal("partial DATA submitted after write failure")
			}
		})
	}
	client := &emailDeliveryStub{quitErr: errors.New("connection lost after accepted DATA")}
	if err := submitSMTP(client, "s@example.com", "r@example.com", []byte("message")); err != nil {
		t.Fatal("accepted DATA invalidated by QUIT failure")
	}
	if !client.closed || !client.quitCalled || client.data.String() != "message" {
		t.Fatal("missing DATA acceptance or QUIT attempt")
	}
}

// Exercise Go's real SMTP DATA close/QUIT parsing without any network listener,
// credentials, TLS bypass, or external SMTP service. TLS/auth precede this layer.
func TestEmailSMTPAcceptedDATAThenQUITFailureProtocol(t *testing.T) {
	for _, rejectData := range []bool{false, true} {
		t.Run(fmt.Sprintf("rejectData=%v", rejectData), func(t *testing.T) {
			clientConn, serverConn := net.Pipe()
			defer clientConn.Close()
			defer serverConn.Close()
			_ = clientConn.SetDeadline(time.Now().Add(3 * time.Second))
			_ = serverConn.SetDeadline(time.Now().Add(3 * time.Second))
			done := make(chan error, 1)
			go func() {
				wire := textproto.NewConn(serverConn)
				defer wire.Close()
				if err := wire.PrintfLine("220 pipe.example.com ESMTP"); err != nil {
					done <- err
					return
				}
				for _, step := range []struct{ prefix, reply string }{
					{"EHLO ", "250 pipe.example.com"},
					{"MAIL FROM:<s@example.com>", "250 sender accepted"},
					{"RCPT TO:<r@example.com>", "250 recipient accepted"},
					{"DATA", "354 send message"},
				} {
					line, err := wire.ReadLine()
					if err != nil {
						done <- err
						return
					}
					if !strings.HasPrefix(line, step.prefix) {
						done <- errors.New("unexpected SMTP command")
						return
					}
					if err = wire.PrintfLine("%s", step.reply); err != nil {
						done <- err
						return
					}
				}
				body, err := wire.ReadDotBytes()
				if err != nil {
					done <- err
					return
				}
				if string(body) != "test message\n" {
					done <- errors.New("wrong DATA body")
					return
				}
				if rejectData {
					done <- wire.PrintfLine("451 message not accepted")
					return
				}
				if err = wire.PrintfLine("250 queued for delivery"); err != nil {
					done <- err
					return
				}
				line, err := wire.ReadLine()
				if err != nil {
					done <- err
					return
				}
				if line != "QUIT" {
					done <- errors.New("missing QUIT")
					return
				}
				done <- wire.PrintfLine("500 QUIT failed after accepted DATA")
			}()
			client, err := smtp.NewClient(clientConn, "pipe.example.com")
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			err = submitSMTP(client, "s@example.com", "r@example.com", []byte("test message\r\n"))
			if rejectData && err == nil {
				t.Fatal("DATA rejection was not surfaced")
			}
			if !rejectData && err != nil {
				t.Fatal("QUIT rejection invalidated accepted DATA")
			}
			if err = <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestEmailQUITFailureDoesNotInvalidateDeliveredChallenge(t *testing.T) {
	a, h := identityFixture(t, false)
	cookie, csrf, _ := identityRegister(t, h, "12345678@qq.com")
	identityMail(t, a)
	client := &emailDeliveryStub{quitErr: errors.New("quit failed after SMTP acceptance")}
	a.mailSender = func(_ context.Context, c SMTPConfig, _, recipient string, m EmailMessage) error {
		if m.Subject != "[MSBOOST] 邮箱验证" {
			t.Fatal("wrong verification template")
		}
		raw, err := BuildMIME(c.Name, c.Sender, recipient, m, time.Now())
		if err != nil {
			return err
		}
		return submitSMTP(client, c.Sender, recipient, raw)
	}
	identityResponse(t, identityRequest(h, "POST", "/api/auth/email/send", map[string]any{"purpose": "verify"}, cookie, csrf), 200)
	if err := a.Store.View(func(s *State) error {
		challenge, ok := LoadDoc[emailChallenge](s, "email_challenges", challengeID("verify", "12345678@qq.com"))
		if !ok || !challenge.Ready {
			t.Fatal("accepted challenge removed/not marked ready")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestEmailSMTPRejectsUnsafeInputBeforeDial(t *testing.T) {
	m, err := RenderEmail("register", emailTestData())
	if err != nil {
		t.Fatal(err)
	}
	if err = sendSMTP(context.Background(), SMTPConfig{Encryption: "none"}, "secret", "12345678@qq.com", m); err == nil {
		t.Fatal("plaintext SMTP accepted")
	}
	if err = sendSMTP(context.Background(), SMTPConfig{Encryption: "tls", Sender: "s@example.com\r\nBcc: bad@example.com"}, "secret", "12345678@qq.com", m); err == nil {
		t.Fatal("unsafe sender accepted")
	}
}
