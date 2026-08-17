package mail

import (
	"strings"
	"testing"
)

// TestBuildMessage_SubjectInjection is the mandatory header-injection case: a
// subject carrying a raw CRLF must not produce a second header in the
// message. Asserted directly on the bytes buildMessage hands to the SMTP
// writer, not on a live send.
func TestBuildMessage_SubjectInjection(t *testing.T) {
	msg, _, _, err := buildMessage(DefaultFrom, "victim@example.com",
		"Report\r\nBcc: attacker@evil.com", "body")
	if err != nil {
		t.Fatalf("buildMessage: %v", err)
	}
	text := string(msg)
	if strings.Contains(text, "\r\nBcc:") {
		t.Errorf("raw header injection survived into the message:\n%s", text)
	}
	if n := strings.Count(text, "\r\nSubject:"); n != 1 {
		t.Errorf("expected exactly one Subject header, found %d:\n%s", n, text)
	}
}

// TestBuildMessage_ToAddressInjectionRejected covers an attempted injection
// riding in the address itself rather than the subject.
func TestBuildMessage_ToAddressInjectionRejected(t *testing.T) {
	_, _, _, err := buildMessage(DefaultFrom,
		"victim@example.com>\r\nBcc: attacker@evil.com<x@x.com", "subject", "body")
	if err == nil {
		t.Fatal("buildMessage: want error for a control-character address, got nil")
	}
}

// TestBuildMessage_DisplayNameInjectionRejected covers the "display name"
// half of the mandatory case: a quoted display-name containing CRLF.
func TestBuildMessage_DisplayNameInjectionRejected(t *testing.T) {
	_, _, _, err := buildMessage(DefaultFrom,
		"\"Evil\r\nBcc: attacker@evil.com\" <victim@example.com>", "subject", "body")
	if err == nil {
		t.Fatal("buildMessage: want error for a CRLF-carrying display name, got nil")
	}
}

func TestBuildMessage_Shape(t *testing.T) {
	msg, from, to, err := buildMessage(DefaultFrom, "victim@example.com", "hello", "body text")
	if err != nil {
		t.Fatalf("buildMessage: %v", err)
	}
	if from != "no-reply@drive.local" {
		t.Errorf("from = %q, want no-reply@drive.local", from)
	}
	if to != "victim@example.com" {
		t.Errorf("to = %q, want victim@example.com", to)
	}
	text := string(msg)
	if !strings.HasSuffix(text, "body text") {
		t.Errorf("message does not end with the body:\n%s", text)
	}
	if !strings.Contains(text, "Subject: hello\r\n") {
		t.Errorf("plain-ASCII subject should not be encoded:\n%s", text)
	}
	if !strings.Contains(text, "\r\n\r\nbody text") {
		t.Errorf("missing blank line separating headers from body:\n%s", text)
	}
}

func TestBuildMessage_NonASCIISubjectEncoded(t *testing.T) {
	msg, _, _, err := buildMessage(DefaultFrom, "victim@example.com", "café", "body")
	if err != nil {
		t.Fatalf("buildMessage: %v", err)
	}
	text := string(msg)
	if strings.Contains(text, "café") {
		t.Errorf("non-ASCII subject was not RFC 2047 encoded:\n%s", text)
	}
	if !strings.Contains(text, "=?utf-8?") {
		t.Errorf("expected an RFC 2047 encoded-word, got:\n%s", text)
	}
}
