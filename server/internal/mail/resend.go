package mail

// The production sender.
//
// It exists because Railway's Hobby plan blocks outbound SMTP entirely ("SMTP
// is only available on the Pro plan and above"), so the deployed server cannot
// reach any mail server on 25/465/587 no matter how it is configured. Resend's
// HTTP API is therefore not a fallback but the only path off the box, and
// SMTPSender stays for local Mailpit, where being able to read the inbox over
// REST is what makes the verification and OTP loops closed-loop testable.
//
// Injection safety is the same contract SMTPSender keeps, reached differently:
// the message is a JSON document rather than raw RFC 5322 bytes, so there are
// no header lines to smuggle a Bcc into -- but the addresses still go through
// net/mail.ParseAddress, and the subject still has its control characters
// removed, because a display name or a file name reaches this code from user
// input and the far side does build headers out of it.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"strings"
	"time"
	"unicode"
)

// resendEndpoint is Resend's send-one-email call.
const resendEndpoint = "https://api.resend.com/emails"

// resendTimeout bounds one send. Delivery is fire-and-forget from a detached
// goroutine, so nothing waits on it -- but nothing should hold a goroutine open
// on a hung connection either.
const resendTimeout = 15 * time.Second

// ResendSender delivers mail over Resend's HTTP API.
type ResendSender struct {
	// Key is the API key (DRIVE_RESEND_KEY). It is sent as a bearer token and
	// must never be logged.
	Key string
	// From is the envelope and header From (DRIVE_MAIL_FROM). Resend refuses any
	// address outside a verified domain.
	From string
	// Endpoint overrides the API URL. Tests point it at an httptest server;
	// production leaves it blank.
	Endpoint string
	// HTTP overrides the client. Production leaves it blank.
	HTTP *http.Client
}

// NewResendSender builds a sender. from may be blank, in which case DefaultFrom
// is used -- which Resend will reject, loudly, which is the right failure for a
// misconfigured deployment.
func NewResendSender(key, from string) *ResendSender {
	if from == "" {
		from = DefaultFrom
	}
	return &ResendSender{Key: key, From: from}
}

// resendRequest is the API's send payload. Only the fields Drive uses are here;
// everything Drive sends is plain text.
type resendRequest struct {
	From    string   `json:"from"`
	To      []string `json:"to"`
	Subject string   `json:"subject"`
	Text    string   `json:"text"`
}

// Send delivers one message.
//
// A non-2xx answer is an error carrying the status and the body, because
// Resend's refusals are specific and worth reading in a log line: an
// unverified domain, a spent daily quota and a bad key all look different.
func (s *ResendSender) Send(ctx context.Context, to, subject, body string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	from := s.From
	if from == "" {
		from = DefaultFrom
	}
	fa, err := mail.ParseAddress(from)
	if err != nil {
		return fmt.Errorf("mail: invalid from address: %w", err)
	}
	ta, err := mail.ParseAddress(to)
	if err != nil {
		return fmt.Errorf("mail: invalid to address: %w", err)
	}

	payload, err := json.Marshal(resendRequest{
		From:    fa.String(),
		To:      []string{ta.Address},
		Subject: stripControls(subject),
		Text:    body,
	})
	if err != nil {
		return fmt.Errorf("mail: encoding the message: %w", err)
	}

	sendCtx, cancel := context.WithTimeout(ctx, resendTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(sendCtx, http.MethodPost, s.endpoint(), bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("mail: building the request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.Key)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client().Do(req)
	if err != nil {
		return fmt.Errorf("mail: send to %s: %w", ta.Address, err)
	}
	defer resp.Body.Close()

	// Bounded: an error body is a sentence, and nothing downstream reads more.
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("mail: send to %s: resend answered %d: %s",
			ta.Address, resp.StatusCode, strings.TrimSpace(string(answer)))
	}
	return nil
}

func (s *ResendSender) endpoint() string {
	if s.Endpoint != "" {
		return s.Endpoint
	}
	return resendEndpoint
}

func (s *ResendSender) client() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return &http.Client{Timeout: resendTimeout}
}

// stripControls removes the C0 controls and DEL from a subject. JSON encoding
// already escapes them, so this is not about this request -- it is about the
// header Resend writes on the other side out of a value that reached us from a
// display name or a file name.
func stripControls(s string) string {
	return strings.Map(func(r rune) rune {
		if r == 0x7F || unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}
