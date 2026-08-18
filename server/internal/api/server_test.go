package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rahul-sharma-cs/drive/server/internal/config"
)

// newTestServer builds a Server with no database. Everything exercised here
// runs before the session loader touches one.
//
// It goes through New rather than a struct literal so the limiters New builds
// are present: a Server assembled by hand has none, and the handlers that use
// them would be exercised in a shape production never has.
func newTestServer(t *testing.T) http.Handler {
	t.Helper()
	return New(&config.Config{}, nil, nil, nil, nil, nil).Routes()
}

func decodeErr(t *testing.T, body string) ErrorBody {
	t.Helper()
	var e ErrorBody
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatalf("response is not an error envelope: %v (body %q)", err, body)
	}
	return e
}

// Every /api mutation needs X-Drive-Client, including the public share POSTs.
// GETs are exempt.
func TestRequireClientHeader(t *testing.T) {
	h := newTestServer(t)

	cases := []struct {
		name       string
		method     string
		path       string
		header     bool
		wantStatus int
		wantCode   string
	}{
		{name: "POST without header", method: http.MethodPost, path: "/api/folders", wantStatus: http.StatusForbidden, wantCode: CodeInvalid},
		{name: "PATCH without header", method: http.MethodPatch, path: "/api/nodes/x", wantStatus: http.StatusForbidden, wantCode: CodeInvalid},
		{name: "DELETE without header", method: http.MethodDelete, path: "/api/nodes/x", wantStatus: http.StatusForbidden, wantCode: CodeInvalid},
		{name: "PUT without header", method: http.MethodPut, path: "/api/uploads/x/parts/1/blob", wantStatus: http.StatusForbidden, wantCode: CodeInvalid},
		{name: "public share POST without header", method: http.MethodPost, path: "/api/s/tok/verify-otp", wantStatus: http.StatusForbidden, wantCode: CodeInvalid},
		// With the header the request passes the CSRF gate and falls through
		// to the API's own 404 envelope -- proving the gate, not the route.
		// These must target an UNROUTED path: a real route sits behind
		// RequireAuth, which answers 401 long before the 404 fall-through.
		{name: "POST with header", method: http.MethodPost, path: "/api/nope", header: true, wantStatus: http.StatusNotFound, wantCode: CodeNotFound},
		{name: "GET is exempt", method: http.MethodGet, path: "/api/nope", wantStatus: http.StatusNotFound, wantCode: CodeNotFound},
	}

	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, nil)
		if c.header {
			req.Header.Set(ClientHeader, "web")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != c.wantStatus {
			t.Errorf("%s: status = %d, want %d (body %q)", c.name, rec.Code, c.wantStatus, rec.Body.String())
		}
		if got := decodeErr(t, rec.Body.String()).Code; got != c.wantCode {
			t.Errorf("%s: code = %q, want %q", c.name, got, c.wantCode)
		}
	}
}

func TestErrorEnvelopeShape(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/nodes/x", nil)
	WriteErr(rec, req, http.StatusNotFound, CodeNotFound, "no such node")

	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("body: %v", err)
	}
	if len(raw) != 2 {
		t.Errorf("envelope without details has %d keys (%v), want 2", len(raw), raw)
	}
	if raw["code"] != CodeNotFound || raw["message"] != "no such node" {
		t.Errorf("envelope = %v", raw)
	}

	rec = httptest.NewRecorder()
	WriteErrDetails(rec, req, http.StatusUnprocessableEntity, CodeInvalid, "bad field", map[string]string{"field": "name"})
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("body: %v", err)
	}
	details, ok := raw["details"].(map[string]any)
	if !ok || details["field"] != "name" {
		t.Errorf("details = %v, want {field: name}", raw["details"])
	}
}

func TestListEnvelope(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteJSON(rec, http.StatusOK, NewList[NodeDTO](nil, ""))
	if got := strings.TrimSpace(rec.Body.String()); got != `{"items":[],"next_cursor":null}` {
		t.Errorf("empty list = %s", got)
	}

	rec = httptest.NewRecorder()
	WriteJSON(rec, http.StatusOK, NewList([]string{"a"}, "cur"))
	if got := strings.TrimSpace(rec.Body.String()); got != `{"items":["a"],"next_cursor":"cur"}` {
		t.Errorf("list with cursor = %s", got)
	}
}

// A folder's size must serialize as null, not be omitted.
func TestNodeDTOFolderSizeIsNull(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteJSON(rec, http.StatusOK, NodeDTO{Kind: "folder", Name: "My Drive"})
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("body: %v", err)
	}
	for _, key := range []string{"id", "parent_id", "kind", "name", "size", "mime", "created_at", "updated_at"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("node DTO is missing %q: %v", key, raw)
		}
	}
	if raw["size"] != nil {
		t.Errorf("folder size = %v, want null", raw["size"])
	}
	if _, ok := raw["trashed_root"]; ok {
		t.Errorf("trashed_root must be omitted when false: %v", raw)
	}
}

type testCursor struct {
	UpdatedAt string `json:"updated_at"`
	ID        string `json:"id"`
}

func TestCursorRoundTrip(t *testing.T) {
	want := testCursor{UpdatedAt: "2026-08-14T12:00:00Z", ID: "5e0f2a1c-0000-4000-8000-000000000001"}

	var got testCursor
	if err := DecodeCursor(EncodeCursor(want), &got); err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if got != want {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
}

func TestDecodeCursorRejectsGarbage(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{name: "not base64", in: "!!!!"},
		{name: "base64 of non-JSON", in: base64.RawURLEncoding.EncodeToString([]byte("not json"))},
		{name: "JSON of the wrong shape", in: EncodeCursor(map[string]int{"page": 3})},
		{name: "empty", in: ""},
		{name: "truncated cursor", in: EncodeCursor(testCursor{ID: "x"})[:4]},
	}
	for _, c := range cases {
		var dst testCursor
		err := DecodeCursor(c.in, &dst)
		if !errors.Is(err, ErrBadCursor) {
			t.Errorf("%s: DecodeCursor = %v, want ErrBadCursor", c.name, err)
		}
	}
}

func TestPage(t *testing.T) {
	cases := []struct {
		query      string
		wantCursor string
		wantLimit  int
		wantErr    bool
	}{
		{query: "", wantLimit: DefaultLimit},
		{query: "?limit=10", wantLimit: 10},
		{query: "?limit=1000", wantLimit: MaxLimit},
		{query: "?cursor=abc&limit=5", wantCursor: "abc", wantLimit: 5},
		{query: "?limit=0", wantErr: true},
		{query: "?limit=-1", wantErr: true},
		{query: "?limit=many", wantErr: true},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodGet, "/api/nodes/x/children"+c.query, nil)
		cursor, limit, err := Page(req)
		if c.wantErr {
			if err == nil {
				t.Errorf("Page(%q): want error, got limit %d", c.query, limit)
			}
			continue
		}
		if err != nil {
			t.Errorf("Page(%q): %v", c.query, err)
			continue
		}
		if cursor != c.wantCursor || limit != c.wantLimit {
			t.Errorf("Page(%q) = (%q, %d), want (%q, %d)", c.query, cursor, limit, c.wantCursor, c.wantLimit)
		}
	}
}

func TestReadJSON(t *testing.T) {
	type body struct {
		Name string `json:"name"`
	}

	req := httptest.NewRequest(http.MethodPost, "/api/folders", strings.NewReader(`{"name":"docs"}`))
	var ok body
	if err := ReadJSON(req, &ok); err != nil {
		t.Fatalf("valid body: %v", err)
	}
	if ok.Name != "docs" {
		t.Errorf("name = %q", ok.Name)
	}

	bad := []string{`{`, `{"name":"docs"} {"name":"more"}`, `{"nope":1}`, ``}
	for _, in := range bad {
		req := httptest.NewRequest(http.MethodPost, "/api/folders", strings.NewReader(in))
		var dst body
		if err := ReadJSON(req, &dst); !errors.Is(err, ErrBadJSON) {
			t.Errorf("ReadJSON(%q) = %v, want ErrBadJSON", in, err)
		}
	}

	// Over the 1 MiB cap.
	req = httptest.NewRequest(http.MethodPost, "/api/folders",
		strings.NewReader(`{"name":"`+strings.Repeat("a", 2<<20)+`"}`))
	var big body
	if err := ReadJSON(req, &big); !errors.Is(err, ErrBadJSON) {
		t.Errorf("oversize body = %v, want ErrBadJSON", err)
	}
}

func TestPanicBecomesJSON500(t *testing.T) {
	s := &Server{}
	r := s.Routes()
	// Reach the recoverer through a handler that panics: MustUser without a
	// session is exactly that programming error.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("healthz without a database = %d, want 503", rec.Code)
	}

	rec = httptest.NewRecorder()
	handler := s.recoverer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/nodes/x", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("panic status = %d, want 500", rec.Code)
	}
	if got := decodeErr(t, rec.Body.String()).Code; got != CodeInternal {
		t.Errorf("panic code = %q, want %q", got, CodeInternal)
	}
}

func TestMustUser(t *testing.T) {
	u := &User{Email: "rahul@drive.local"}
	ctx := WithUser(t.Context(), u)
	if got, ok := UserFromCtx(ctx); !ok || got != u {
		t.Fatalf("UserFromCtx = (%v, %v)", got, ok)
	}
	if MustUser(ctx) != u {
		t.Error("MustUser returned a different user")
	}

	defer func() {
		if recover() == nil {
			t.Error("MustUser without a user must panic")
		}
	}()
	MustUser(t.Context())
}

// /livez answers without touching the database, and /healthz does not.
//
// They are separate because a platform health check that fails restarts the
// container: wiring the platform's probe to a database ping turns a brief blip
// into a restart loop, exactly when a running process would have ridden it out.
// The readiness question -- which is what `make e2e` and the harness wait on --
// is a different one, and keeps its ping.
func TestLivezIsAliveWithoutADatabase(t *testing.T) {
	h := newTestServer(t)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/livez", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("/livez with no database: status %d, want 200", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("/healthz with no database: status %d, want 503 -- readiness must still mean ready", rec.Code)
	}
}

// The entry document must never be cached: it names the hashed asset files, so
// a stale copy points at assets a new release no longer has. The assets
// themselves carry a content hash and are immutable by construction.
func TestSPACacheHeaders(t *testing.T) {
	h := newTestServer(t)

	cases := []struct {
		name string
		path string
		want string
	}{
		{"index", "/", "no-cache"},
		{"client-side route falls back to index", "/verify", "no-cache"},
		// A hashed asset the current build no longer has falls back to the
		// entry document, and must be labelled as the document it actually is
		// -- labelling that HTML immutable is how a browser gets stuck on a
		// release that no longer exists.
		{"stale hashed asset falls back to index", "/assets/index-OLD.js", "no-cache"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if got := rec.Header().Get("Cache-Control"); got != tc.want {
				t.Fatalf("Cache-Control for %s = %q, want %q", tc.path, got, tc.want)
			}
		})
	}

	// The hashed assets are asserted on the rule rather than through a request:
	// which files exist under assets/ depends on whether the SPA has been built,
	// and a fresh clone embeds only the placeholder index.html.
	if got := spaCacheControl("assets/index-0123456789.js"); got != "public, max-age=31536000, immutable" {
		t.Fatalf("Cache-Control for a hashed asset = %q", got)
	}
	if got := spaCacheControl("index.html"); got != "no-cache" {
		t.Fatalf("Cache-Control for the entry document = %q", got)
	}
}
