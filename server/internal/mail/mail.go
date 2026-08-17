// Package mail is Drive's outbound email seam.
//
// The api.Server depends only on the Sender interface below. SMTPSender is the
// Mailpit-backed implementation used everywhere locally (no auth, no TLS,
// exactly what Mailpit expects). Message construction is injection-safe by
// construction: the Subject goes through RFC 2047 Q-encoding, which escapes
// every CR/LF and non-printable byte out of the header value, and From/To are
// validated with net/mail.ParseAddress before they ever reach a header line —
// so a subject or an address that carries a raw "\r\nBcc: ..." attempt cannot
// smuggle an extra header into the message. See mail_test.go for the exact
// injection cases this defends.
package mail

import (
	"context"
	"fmt"
	"mime"
	"net/mail"
	"net/smtp"
	"strings"
)

// Sender delivers one plain-text message.
type Sender interface {
	Send(ctx context.Context, to, subject, body string) error
}

// DefaultFrom is Drive's outbound address when DRIVE_MAIL_FROM is unset. It is
// the local one: Mailpit accepts anything, and a deployment sending through
// Resend must name an address on its own verified domain, which no default can
// guess.
const DefaultFrom = "Drive <no-reply@drive.local>"

// SMTPSender sends mail over plain SMTP with no auth and no TLS — Mailpit's
// listener, in dev and in the test stack alike.
type SMTPSender struct {
	// Addr is the SMTP server's host:port (DRIVE_SMTP_ADDR).
	Addr string
	// From overrides DefaultFrom when set. Tests use this; production code
	// should leave it blank.
	From string
}

// NewSMTPSender builds a sender against addr (host:port, e.g. "localhost:1025").
func NewSMTPSender(addr string) *SMTPSender {
	return &SMTPSender{Addr: addr}
}

// Send delivers one message. ctx bounds only the wait for a result the way
// context normally does at a blocking call — net/smtp has no native
// cancellation, so a request whose context is already done fails fast instead
// of dialing at all.
func (s *SMTPSender) Send(ctx context.Context, to, subject, body string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	from := s.From
	if from == "" {
		from = DefaultFrom
	}

	msg, fromAddr, toAddr, err := buildMessage(from, to, subject, body)
	if err != nil {
		return err
	}

	if err := smtp.SendMail(s.Addr, nil, fromAddr, []string{toAddr}, msg); err != nil {
		return fmt.Errorf("mail: send to %s: %w", toAddr, err)
	}
	return nil
}

// buildMessage renders the raw RFC 5322 bytes handed to the SMTP writer's DATA
// command, and returns the bare addresses SendMail's envelope needs
// separately. It is the one place message-injection safety lives, which is why
// it is tested directly rather than only through a live SMTP round trip:
//
//   - Subject is passed through mime.QEncoding.Encode, which escapes any byte
//     outside printable ASCII (space through '~') — including CR and LF — into
//     an RFC 2047 encoded-word. A literal "\r\nBcc: evil@x" in a subject can
//     therefore never produce a second header line.
//   - From and To are parsed with net/mail.ParseAddress, which rejects control
//     characters in both the address and any display name; a malformed or
//     injection-carrying address is refused before a header is ever written.
func buildMessage(from, to, subject, body string) (msg []byte, fromAddr, toAddr string, err error) {
	fa, err := mail.ParseAddress(from)
	if err != nil {
		return nil, "", "", fmt.Errorf("mail: invalid from address: %w", err)
	}
	ta, err := mail.ParseAddress(to)
	if err != nil {
		return nil, "", "", fmt.Errorf("mail: invalid to address: %w", err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", fa.String())
	fmt.Fprintf(&b, "To: %s\r\n", ta.String())
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", subject))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(body)

	return []byte(b.String()), fa.Address, ta.Address, nil
}
