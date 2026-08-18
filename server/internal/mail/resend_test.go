package mail

// ResendSender against an httptest stand-in for the API.
//
// The far side is a third party, so what is worth pinning here is the request
// Drive builds and what it does with each class of answer -- an unverified
// domain, a spent daily quota and a revoked key all arrive as a non-2xx with a
// sentence, and a log line that swallows the sentence is a deployment nobody
// can debug.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// capture is one recorded request.
type capture struct {
	method string
	path   string
	auth   string
	ctype  string
	body   resendRequest
	hits   int
}

// stubResend answers with the given status and body, recording what it got.
func stubResend(t *testing.T, status int, answer string) (*ResendSender, *capture) {
	t.Helper()
	got := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.hits++
		got.method, got.path = r.Method, r.URL.Path
		got.auth = r.Header.Get("Authorization")
		got.ctype = r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&got.body); err != nil {
			t.Errorf("decoding the request body: %v", err)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(answer))
	}))
	t.Cleanup(srv.Close)

	s := NewResendSender("re_test_key", "Drive <no-reply@drive.test>")
	s.Endpoint = srv.URL + "/emails"
	return s, got
}

func TestResendSendsTheApiTheMessage(t *testing.T) {
	s, got := stubResend(t, http.StatusOK, `{"id":"aaaaaaaa-0000-0000-0000-000000000000"}`)

	if err := s.Send(context.Background(), "Someone <person@example.com>", "Verify your Drive account", "click here"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if got.method != http.MethodPost || got.path != "/emails" {
		t.Errorf("called %s %s, want POST /emails", got.method, got.path)
	}
	if got.auth != "Bearer re_test_key" {
		t.Errorf("Authorization = %q, want the bearer key", got.auth)
	}
	if !strings.HasPrefix(got.ctype, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", got.ctype)
	}
	if got.body.From != `"Drive" <no-reply@drive.test>` {
		t.Errorf("from = %q, want the configured sender", got.body.From)
	}
	// The envelope takes the bare address; the display name the caller supplied
	// is not part of the recipient list.
	if len(got.body.To) != 1 || got.body.To[0] != "person@example.com" {
		t.Errorf("to = %v, want [person@example.com]", got.body.To)
	}
	if got.body.Subject != "Verify your Drive account" || got.body.Text != "click here" {
		t.Errorf("subject/text = %q/%q", got.body.Subject, got.body.Text)
	}
}

// A refusal has to reach the log intact: "resend answered 403" with no reason
// is the difference between a five-minute fix and an afternoon.
func TestResendReportsARefusalWithItsReason(t *testing.T) {
	s, _ := stubResend(t, http.StatusForbidden,
		`{"statusCode":403,"message":"The drive.test domain is not verified"}`)

	err := s.Send(context.Background(), "person@example.com", "hello", "body")
	if err == nil {
		t.Fatal("a 403 answered nil")
	}
	if !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "not verified") {
		t.Errorf("error = %q, want the status and the reason", err)
	}
	// The key travels in a header, and an error string is a log line.
	if strings.Contains(err.Error(), "re_test_key") {
		t.Error("the error carries the API key")
	}
}

// Address validation happens before the request, so a malformed recipient
// cannot spend a send against the account's daily quota.
func TestResendRejectsBadAddressesWithoutCallingTheApi(t *testing.T) {
	for _, c := range []struct {
		what string
		from string
		to   string
	}{
		{"a recipient with a smuggled header", "Drive <ok@drive.test>", "victim@example.com>\r\nBcc: evil@example.com"},
		{"an empty recipient", "Drive <ok@drive.test>", ""},
		{"a sender that is not an address", "not an address", "person@example.com"},
	} {
		t.Run(c.what, func(t *testing.T) {
			s, got := stubResend(t, http.StatusOK, `{"id":"x"}`)
			s.From = c.from

			if err := s.Send(context.Background(), c.to, "hello", "body"); err == nil {
				t.Error("Send accepted it")
			}
			if got.hits != 0 {
				t.Errorf("the API was called %d times for a message that never should have been built", got.hits)
			}
		})
	}
}

// A subject reaches this code from user input -- a display name today, a file
// name if share mails ever ship. JSON escaping protects this request; stripping protects the
// header Resend writes out of the value on the other side.
func TestResendStripsControlCharactersFromTheSubject(t *testing.T) {
	s, got := stubResend(t, http.StatusOK, `{"id":"x"}`)

	if err := s.Send(context.Background(), "person@example.com",
		"Your code for \"report\"\r\nBcc: evil@example.com", "body"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if strings.ContainsAny(got.body.Subject, "\r\n") {
		t.Errorf("subject = %q, still carries CR/LF", got.body.Subject)
	}
	if !strings.Contains(got.body.Subject, "Bcc: evil@example.com") {
		t.Errorf("subject = %q: the text should survive, only the controls go", got.body.Subject)
	}
}

// The request never leaves if the caller is already gone.
func TestResendHonoursACancelledContext(t *testing.T) {
	s, got := stubResend(t, http.StatusOK, `{"id":"x"}`)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := s.Send(ctx, "person@example.com", "hello", "body"); err == nil {
		t.Error("Send on a cancelled context answered nil")
	}
	if got.hits != 0 {
		t.Errorf("the API was called %d times on a cancelled context", got.hits)
	}
}

// An unreachable API is an error, not a silent success -- signup stays a 200
// either way, but the log has to say a message was lost.
func TestResendReportsATransportFailure(t *testing.T) {
	s := NewResendSender("re_test_key", "Drive <no-reply@drive.test>")
	// A port nothing is listening on, on a host that resolves instantly.
	s.Endpoint = "http://127.0.0.1:1/emails"

	if err := s.Send(context.Background(), "person@example.com", "hello", "body"); err == nil {
		t.Fatal("an unreachable API answered nil")
	}
}

// A blank From is a misconfiguration, and falling back to the local default is
// what makes Resend refuse it loudly rather than sending from somewhere unowned.
func TestResendFallsBackToTheDefaultFrom(t *testing.T) {
	s, got := stubResend(t, http.StatusOK, `{"id":"x"}`)
	s.From = ""

	if err := s.Send(context.Background(), "person@example.com", "hello", "body"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !strings.Contains(got.body.From, "no-reply@drive.local") {
		t.Errorf("from = %q, want the DefaultFrom address", got.body.From)
	}
}
