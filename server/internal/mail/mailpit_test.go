package mail

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// These run against a real Mailpit -- the drive-test stack's, by default. `make
// test` sources .env.test into the shell before `go test`, which is where
// DRIVE_SMTP_ADDR/DRIVE_MAILPIT_API normally come from; the fallbacks below are
// the same values so `go test ./server/internal/mail/...` works standalone too.
func smtpAddr() string {
	if v := os.Getenv("DRIVE_SMTP_ADDR"); v != "" {
		return v
	}
	return "localhost:1026"
}

func mailpitAPI() string {
	if v := os.Getenv("DRIVE_MAILPIT_API"); v != "" {
		return v
	}
	return "http://localhost:8026"
}

// TestSMTPAndMailpit_RoundTrip sends a real message through SMTPSender and
// reads it back via the REST client -- the closed loop the OTP and
// verify-email tests depend on.
func TestSMTPAndMailpit_RoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	mp := NewClient(mailpitAPI())
	if err := mp.DeleteAll(ctx); err != nil {
		t.Fatalf("purge inbox: %v", err)
	}

	to := fmt.Sprintf("seed-test-%d@drive.local", time.Now().UnixNano())
	const subject = "Verify your Drive account"
	const body = "hello from the mail package integration test"

	sender := NewSMTPSender(smtpAddr())
	since := time.Now().Add(-2 * time.Second)
	if err := sender.Send(ctx, to, subject, body); err != nil {
		t.Fatalf("Send: %v", err)
	}

	msg, err := mp.WaitForLatestTo(ctx, to, since)
	if err != nil {
		t.Fatalf("WaitForLatestTo: %v", err)
	}

	if msg.Subject != subject {
		t.Errorf("subject = %q, want %q", msg.Subject, subject)
	}
	if got := strings.TrimSpace(msg.Text); got != body {
		t.Errorf("body = %q, want %q", got, body)
	}
	if len(msg.To) != 1 || !strings.EqualFold(msg.To[0].Address, to) {
		t.Errorf("To = %v, want exactly [%s] -- an extra recipient would mean a header injection landed", msg.To, to)
	}
}

// TestSMTPAndMailpit_SubjectInjectionDoesNotAddARecipient is the mandatory
// header-injection case, exercised end to end: a subject containing a raw
// CRLF + "Bcc:" line must not cause Mailpit -- a real SMTP receiver -- to
// observe more than the one intended recipient, and the encoded-word must
// round-trip back to the exact original string (proof it travelled as opaque
// Subject content, never as a second raw header line on the wire; that raw-byte
// property is what TestBuildMessage_SubjectInjection asserts directly).
func TestSMTPAndMailpit_SubjectInjectionDoesNotAddARecipient(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	mp := NewClient(mailpitAPI())
	if err := mp.DeleteAll(ctx); err != nil {
		t.Fatalf("purge inbox: %v", err)
	}

	to := fmt.Sprintf("injection-test-%d@drive.local", time.Now().UnixNano())
	subject := "Verify your Drive account\r\nBcc: attacker@evil.example"
	const body = "hello"

	sender := NewSMTPSender(smtpAddr())
	since := time.Now().Add(-2 * time.Second)
	if err := sender.Send(ctx, to, subject, body); err != nil {
		t.Fatalf("Send: %v", err)
	}

	msg, err := mp.WaitForLatestTo(ctx, to, since)
	if err != nil {
		t.Fatalf("WaitForLatestTo: %v", err)
	}

	// Mailpit fully decodes the RFC 2047 encoded-word, so the recovered
	// Subject legitimately contains the original CRLF as content -- that is
	// correct round-tripping, not a leak. What must never happen is a second
	// recipient materializing from it.
	if msg.Subject != subject {
		t.Errorf("decoded subject = %q, want the exact original %q (round-trip should be lossless)", msg.Subject, subject)
	}
	if len(msg.To) != 1 || !strings.EqualFold(msg.To[0].Address, to) {
		t.Fatalf("To = %v, want exactly [%s] -- the injected Bcc line must never become a real recipient", msg.To, to)
	}

	list, err := mp.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if n := countMessagesTo(list, "attacker@evil.example"); n != 0 {
		t.Errorf("%d message(s) reached attacker@evil.example -- the subject injection was not contained", n)
	}
}

func countMessagesTo(list []Summary, addr string) int {
	n := 0
	for i := range list {
		if addressedTo(&list[i], addr) {
			n++
		}
	}
	return n
}

// TestClient_DeleteAll is a narrower check that DeleteAll actually empties the
// inbox, independent of the round-trip test's own send.
func TestClient_DeleteAll(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	mp := NewClient(mailpitAPI())
	to := fmt.Sprintf("delete-all-test-%d@drive.local", time.Now().UnixNano())
	since := time.Now().Add(-2 * time.Second)

	sender := NewSMTPSender(smtpAddr())
	if err := sender.Send(ctx, to, "throwaway", "throwaway"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, err := mp.WaitForLatestTo(ctx, to, since); err != nil {
		t.Fatalf("WaitForLatestTo before delete: %v", err)
	}

	if err := mp.DeleteAll(ctx); err != nil {
		t.Fatalf("DeleteAll: %v", err)
	}

	list, err := mp.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("inbox has %d messages after DeleteAll, want 0", len(list))
	}
}
