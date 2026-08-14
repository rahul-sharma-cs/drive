package testutil

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"testing"
	"time"

	"github.com/google/uuid"
)

// FixturePassword is what every harness-created account signs in with. It is a
// fixture, not a secret: these accounts exist only in the drive-test database.
const FixturePassword = "drive-harness-1"

// Client is an HTTP client for one identity against one server. It keeps the
// session cookie in a jar and adds X-Drive-Client to every mutation, so a test
// that wants the CSRF gate to fire has to ask for it explicitly.
type Client struct {
	// BaseURL is the server this client talks to.
	BaseURL string
	// ID, Email and RootID are zero for an anonymous client.
	ID     uuid.UUID
	Email  string
	RootID uuid.UUID

	t            testing.TB
	http         *http.Client
	clientHeader bool
}

// NewUser signs a fresh account up, marks the address verified, and signs in.
// Two lines from a test to an authenticated request:
//
//	owner := h.NewUser(t)
//	owner.Post(t, "/api/folders", map[string]any{...}).Expect(201)
//
// The verification step is a direct SQL write rather than the mail round trip
// on purpose: at this level the mail loop is not what is under test, and
// Playwright covers signup -> Mailpit -> verify -> login end to end (PLAN
// §Testing 4). See the note in the package's report about the server binary
// currently having no mail sender wired at all.
func (h *Harness) NewUser(t testing.TB) *Client {
	t.Helper()
	c := h.Anonymous(t)
	c.Email = h.uniqueEmail()

	c.Post(t, "/api/auth/signup", map[string]any{
		"email":        c.Email,
		"password":     FixturePassword,
		"display_name": "Harness User",
	}).Expect(http.StatusOK)

	h.MarkVerified(t, c.Email)
	c.Login(t)
	return c
}

// NewUnverifiedUser signs an account up and stops there, leaving the address
// unverified. Login must refuse it.
func (h *Harness) NewUnverifiedUser(t testing.TB) *Client {
	t.Helper()
	c := h.Anonymous(t)
	c.Email = h.uniqueEmail()

	c.Post(t, "/api/auth/signup", map[string]any{
		"email":        c.Email,
		"password":     FixturePassword,
		"display_name": "Unverified User",
	}).Expect(http.StatusOK)
	return c
}

// Anonymous returns a client with a cookie jar and no session. It still sends
// X-Drive-Client, so a rejection it collects is an authentication decision and
// not the CSRF gate firing first -- the two are easy to confuse, because
// RequireClientHeader runs before RequireAuth.
func (h *Harness) Anonymous(t testing.TB) *Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("testutil: cookie jar: %v", err)
	}
	return &Client{
		BaseURL:      h.BaseURL(),
		t:            t,
		http:         &http.Client{Jar: jar, Timeout: 30 * time.Second},
		clientHeader: true,
	}
}

// MarkVerified stamps email_verified_at, which is the only thing standing
// between a fresh signup and a usable login.
func (h *Harness) MarkVerified(t testing.TB, email string) {
	t.Helper()
	const q = `UPDATE users SET email_verified_at = now() WHERE email = $1`
	tag, err := h.Pool.Exec(context.Background(), q, email)
	if err != nil {
		t.Fatalf("testutil: verifying %s: %v", email, err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("testutil: verifying %s: %d rows, want 1", email, tag.RowsAffected())
	}
}

// uniqueEmail returns an address no other account in this database has. Suites
// share one schema for the whole run, so collisions would be silent: signup
// answers 200 for a taken address by design.
func (h *Harness) uniqueEmail() string {
	return fmt.Sprintf("u%d-%d@drive.test", time.Now().UnixNano(), h.users.Add(1))
}

// Login signs in with the client's own address and captures the cookie. It also
// fills ID and RootID from the response, which carries the /me shape.
func (c *Client) Login(t testing.TB) {
	t.Helper()
	var me struct {
		ID     uuid.UUID `json:"id"`
		Email  string    `json:"email"`
		RootID uuid.UUID `json:"root_id"`
	}
	c.Post(t, "/api/auth/login", map[string]any{
		"email":    c.Email,
		"password": FixturePassword,
	}).Expect(http.StatusOK).JSON(&me)

	c.ID, c.RootID = me.ID, me.RootID
	if c.RootID == uuid.Nil {
		t.Fatalf("testutil: login for %s returned no root_id", c.Email)
	}
}

// At returns a copy of this client pointed at another server, sharing the
// cookie jar. Cookies ignore the port, so a session minted against the shared
// server is accepted by a freshly spawned one -- which is what makes the
// kill-and-restart tests short.
func (c *Client) At(baseURL string) *Client {
	clone := *c
	clone.BaseURL = baseURL
	return &clone
}

// WithoutClientHeader returns a copy that omits X-Drive-Client, for the cases
// asserting the CSRF gate.
func (c *Client) WithoutClientHeader() *Client {
	clone := *c
	clone.clientHeader = false
	return &clone
}

// ----------------------------------------------------------------- requests --

// Do sends a request and reads the whole response. body may be nil, a string or
// []byte for a raw body, or any value to be marshalled as JSON.
func (c *Client) Do(t testing.TB, method, path string, body any) *Resp {
	t.Helper()

	var reader io.Reader
	switch v := body.(type) {
	case nil:
	case string:
		reader = bytes.NewReader([]byte(v))
	case []byte:
		reader = bytes.NewReader(v)
	default:
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("testutil: encoding the body for %s %s: %v", method, path, err)
		}
		reader = bytes.NewReader(raw)
	}

	req, err := http.NewRequest(method, c.BaseURL+path, reader)
	if err != nil {
		t.Fatalf("testutil: building %s %s: %v", method, path, err)
	}
	if reader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.clientHeader {
		switch method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
			req.Header.Set("X-Drive-Client", "web")
		}
	}

	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatalf("testutil: %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("testutil: reading the response of %s %s: %v", method, path, err)
	}
	return &Resp{Status: resp.StatusCode, Body: raw, Header: resp.Header, t: t, what: method + " " + path}
}

// Get, Post, Patch and Delete are the four verbs the API uses.
func (c *Client) Get(t testing.TB, path string) *Resp {
	t.Helper()
	return c.Do(t, http.MethodGet, path, nil)
}

func (c *Client) Post(t testing.TB, path string, body any) *Resp {
	t.Helper()
	return c.Do(t, http.MethodPost, path, body)
}

func (c *Client) Patch(t testing.TB, path string, body any) *Resp {
	t.Helper()
	return c.Do(t, http.MethodPatch, path, body)
}

func (c *Client) Delete(t testing.TB, path string) *Resp {
	t.Helper()
	return c.Do(t, http.MethodDelete, path, nil)
}

// ---------------------------------------------------------------- responses --

// Resp is one response, already read.
type Resp struct {
	Status int
	Body   []byte
	Header http.Header

	t    testing.TB
	what string
}

// Expect fails unless the status matches, quoting the body -- an unexpected
// status is nearly always explained by the error envelope underneath it.
func (r *Resp) Expect(status int) *Resp {
	r.t.Helper()
	if r.Status != status {
		r.t.Fatalf("%s: status %d, want %d (body %s)", r.what, r.Status, status, r.Body)
	}
	return r
}

// JSON decodes the body.
func (r *Resp) JSON(dst any) *Resp {
	r.t.Helper()
	if err := json.Unmarshal(r.Body, dst); err != nil {
		r.t.Fatalf("%s: decoding the body: %v (body %s)", r.what, err, r.Body)
	}
	return r
}

// Code returns the error envelope's code, or "" if the body is not one.
func (r *Resp) Code() string {
	var e struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(r.Body, &e)
	return e.Code
}

// Message returns the error envelope's message, or "".
func (r *Resp) Message() string {
	var e struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(r.Body, &e)
	return e.Message
}

// Node decodes the canonical node DTO.
func (r *Resp) Node() NodeDTO {
	r.t.Helper()
	var n NodeDTO
	r.JSON(&n)
	return n
}

// List decodes the list envelope of node DTOs.
func (r *Resp) List() NodeList {
	r.t.Helper()
	var l NodeList
	r.JSON(&l)
	return l
}

// NodeDTO mirrors the API's canonical node shape. The harness decodes into its
// own struct rather than importing the api package's: an accidental change to
// the wire format should break a test here, not be absorbed by a shared type.
type NodeDTO struct {
	ID          uuid.UUID  `json:"id"`
	ParentID    *uuid.UUID `json:"parent_id"`
	Kind        string     `json:"kind"`
	Name        string     `json:"name"`
	Size        *int64     `json:"size"`
	Mime        *string    `json:"mime"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	TrashedRoot bool       `json:"trashed_root"`
	Existing    bool       `json:"existing"`
}

// NodeList is the list envelope.
type NodeList struct {
	Items      []NodeDTO `json:"items"`
	NextCursor *string   `json:"next_cursor"`
}

// Names returns the item names, for assertions that read like the UI.
func (l NodeList) Names() []string {
	out := make([]string, 0, len(l.Items))
	for _, n := range l.Items {
		out = append(out, n.Name)
	}
	return out
}

// CreateFolder is the folder-creation shorthand every suite needs.
func (c *Client) CreateFolder(t testing.TB, parentID uuid.UUID, name string) NodeDTO {
	t.Helper()
	return c.Post(t, "/api/folders", map[string]any{
		"parent_id": parentID,
		"name":      name,
	}).Expect(http.StatusCreated).Node()
}
