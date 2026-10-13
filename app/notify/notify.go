// SPDX-License-Identifier: Apache-2.0

// Package notify sends pgtijori's emails: the daily summary, failure alerts and
// notices about new or deleted databases. They are plain text, written to be
// understood without knowing pgBackRest.
package notify

import (
	"context"
	"fmt"
	"time"

	mail "github.com/wneessen/go-mail"

	"github.com/logisticinfotech/pgtijori/config"
)

// Mailer sends one email to the configured recipients.
type Mailer interface {
	Send(ctx context.Context, subject, body string) error
}

// SMTP sends email through the configured SMTP account. TLS is always required.
type SMTP struct {
	Cfg config.Email
}

func (s SMTP) Send(ctx context.Context, subject, body string) error {
	m := mail.NewMsg()
	if err := m.From(s.Cfg.From); err != nil {
		return fmt.Errorf("email from: %w", err)
	}
	if err := m.To(s.Cfg.To...); err != nil {
		return fmt.Errorf("email to: %w", err)
	}
	m.Subject(subject)
	m.SetDate()
	m.SetMessageID()
	m.SetBodyString(mail.TypeTextPlain, body)

	opts := []mail.Option{
		mail.WithPort(s.Cfg.SMTPPort),
		mail.WithUsername(s.Cfg.Username),
		mail.WithPassword(s.Cfg.Password),
		mail.WithOpportunisticSMTPAuth(mail.SMTPAuthPlain, mail.SMTPAuthLogin),
		mail.WithTimeout(30 * time.Second),
	}
	if s.Cfg.TLS == "tls" {
		opts = append(opts, mail.WithSSL())
	} else {
		opts = append(opts, mail.WithTLSPolicy(mail.TLSMandatory))
	}
	client, err := mail.NewClient(s.Cfg.SMTPHost, opts...)
	if err != nil {
		return fmt.Errorf("email: %w", err)
	}
	if err := client.DialAndSendWithContext(ctx, m); err != nil {
		return fmt.Errorf("send email via %s:%d: %w", s.Cfg.SMTPHost, s.Cfg.SMTPPort, err)
	}
	return nil
}

// Discard is a Mailer that sends nothing, for tests and dry runs.
type Discard struct{}

func (Discard) Send(context.Context, string, string) error { return nil }

// Recorder is a Mailer that keeps what it was asked to send, for tests.
type Recorder struct {
	Sent []Email
}

// Email is a sent email.
type Email struct{ Subject, Body string }

func (r *Recorder) Send(_ context.Context, subject, body string) error {
	r.Sent = append(r.Sent, Email{subject, body})
	return nil
}
