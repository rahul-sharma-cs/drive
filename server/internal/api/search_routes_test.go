package api

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The node package's suite owns the filter semantics; these cover the query
// string: what parses, what is refused, and that a rejected parameter is a 422
// with the canonical code rather than a silently ignored filter.

// searchSeed puts a small corpus under one user and pins the two updated_at
// values the boundary assertions use.
func searchSeed(t *testing.T) (*trashClient, time.Time) {
	t.Helper()
	c := newTrashClient(t)
	mid := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	touch := func(id uuid.UUID, at time.Time) {
		if _, err := c.pool.Exec(c.ctx, `UPDATE nodes SET updated_at = $2 WHERE id = $1`, id, at); err != nil {
			t.Fatalf("setting updated_at: %v", err)
		}
	}
	touch(c.file(c.root, "report.pdf", 1000), mid)
	touch(c.file(c.root, "Report.txt", 2000), mid.Add(time.Hour))
	touch(c.folder(c.root, "reports"), mid)
	return c, mid
}

// searchNames runs a search over the wire and returns the names it matched.
func searchNames(c *trashClient, query url.Values) []string {
	c.t.Helper()
	rec := c.do(http.MethodGet, "/api/search?"+query.Encode(), true)
	if rec.Code != http.StatusOK {
		c.t.Fatalf("GET /api/search?%s = %d, want 200 (body %s)", query.Encode(), rec.Code, rec.Body.String())
	}
	var names []string
	for _, n := range c.decodeList(rec).Items {
		names = append(names, n.Name)
	}
	return names
}

// The filters arrive intact: inclusive timestamps, inclusive byte counts, and
// a size filter that leaves folders out.
func TestSearchQueryParameters(t *testing.T) {
	c, mid := searchSeed(t)
	rfc := func(d time.Duration) string { return mid.Add(d).Format(time.RFC3339Nano) }

	cases := []struct {
		name  string
		query url.Values
		want  int
	}{
		{"q alone", url.Values{"q": {"report"}}, 3},
		{"q matches nothing", url.Values{"q": {"invoice"}}, 0},
		{"type=file", url.Values{"q": {"report"}, "type": {"file"}}, 2},
		{"type=folder", url.Values{"q": {"report"}, "type": {"folder"}}, 1},
		{"after at the exact timestamp is inclusive", url.Values{"q": {"report"}, "after": {rfc(0)}}, 3},
		{"after past it excludes", url.Values{"q": {"report"}, "after": {rfc(time.Microsecond)}}, 1},
		{"before at the exact timestamp is inclusive", url.Values{"q": {"report"}, "before": {rfc(0)}}, 2},
		{"min_size at the exact byte is inclusive", url.Values{"q": {"report"}, "min_size": {"1000"}}, 2},
		{"min_size one byte past excludes", url.Values{"q": {"report"}, "min_size": {"1001"}}, 1},
		{"max_size at the exact byte is inclusive", url.Values{"q": {"report"}, "max_size": {"1000"}}, 1},
		{"a size filter never returns folders", url.Values{"q": {"reports"}, "min_size": {"0"}}, 0},
		{"filters AND together", url.Values{"q": {"report"}, "after": {rfc(0)}, "before": {rfc(0)}, "min_size": {"1000"}, "max_size": {"1000"}}, 1},
	}

	for _, tc := range cases {
		if got := searchNames(c, tc.query); len(got) != tc.want {
			t.Errorf("%s: got %v (%d), want %d results", tc.name, got, len(got), tc.want)
		}
	}
}

// A parameter the API cannot honour is a 422, not a filter quietly dropped:
// silently ignoring "min_size=huge" would show a user files they filtered out.
func TestSearchRejectsUnusableParameters(t *testing.T) {
	c, _ := searchSeed(t)

	for _, query := range []string{
		"type=archive",
		"after=yesterday",
		"before=2026-03-01", // a date, not an RFC3339 timestamp
		"after=1772000000",  // a unix stamp
		"min_size=-1",
		"max_size=lots",
		"min_size=1.5",
		"limit=0",
		"limit=abc",
		"cursor=not-a-cursor",
	} {
		rec := c.do(http.MethodGet, "/api/search?"+query, true)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("?%s: status = %d, want 422 (body %s)", query, rec.Code, rec.Body.String())
			continue
		}
		if got := decodeErr(t, rec.Body.String()).Code; got != CodeInvalid {
			t.Errorf("?%s: code = %q, want %q", query, got, CodeInvalid)
		}
	}
}

// Search is a signed-in operation, and it only ever sees the caller's nodes.
func TestSearchIsScopedToTheCaller(t *testing.T) {
	c, _ := searchSeed(t)

	if rec := c.do(http.MethodGet, "/api/search?q=report", false); rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous search = %d, want 401 (body %s)", rec.Code, rec.Body.String())
	}

	other := newTrashClient(t)
	other.file(other.root, "secret.pdf", 10)
	if got := searchNames(c, url.Values{"q": {"secret"}}); len(got) != 0 {
		t.Errorf("search returned another user's nodes: %v", got)
	}

	// An empty q is the "filters only" form, not an error.
	if got := searchNames(c, url.Values{"type": {"folder"}}); len(got) == 0 {
		t.Error("an empty q with a type filter returned nothing; it should list the caller's folders")
	}
}

// The listing pages with the opaque cursor.
func TestSearchPagination(t *testing.T) {
	c, _ := searchSeed(t)

	rec := c.do(http.MethodGet, "/api/search?q=report&limit=2", true)
	first := c.decodeList(rec)
	if len(first.Items) != 2 || first.NextCursor == nil {
		t.Fatalf("first page = %d items, cursor %v; want 2 and a cursor", len(first.Items), first.NextCursor)
	}

	rec = c.do(http.MethodGet, "/api/search?q=report&limit=2&cursor="+*first.NextCursor, true)
	second := c.decodeList(rec)
	if len(second.Items) != 1 || second.NextCursor != nil {
		t.Fatalf("second page = %d items, cursor %v; want 1 and null", len(second.Items), second.NextCursor)
	}
	if second.Items[0].ID == first.Items[0].ID {
		t.Error("the second page repeated a row from the first")
	}
}
