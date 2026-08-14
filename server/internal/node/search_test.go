package node

import (
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The search corpus is seeded with exact timestamps and exact sizes so the
// boundary cases below assert inclusivity at the boundary itself, not near it.

// touch pins a node's updated_at, the column search sorts and filters on.
func (f *fixture) touch(id uuid.UUID, at time.Time) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx,
		`UPDATE nodes SET updated_at = $2 WHERE id = $1`, id, at); err != nil {
		f.t.Fatalf("setting updated_at: %v", err)
	}
}

// names runs a search and returns the matched names, in result order.
func (f *fixture) names(q SearchQuery) []string {
	f.t.Helper()
	items, _, err := f.store.Search(f.ctx, f.owner, q, nil, 50)
	if err != nil {
		f.t.Fatalf("Search(%+v): %v", q, err)
	}
	out := make([]string, 0, len(items))
	for _, n := range items {
		out = append(out, n.Name)
	}
	return out
}

func hasName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// searchCorpus seeds one user with:
//
//	report.pdf   1000 bytes, updated at mid
//	Report.txt   2000 bytes, updated at mid+1h
//	reports/     a folder,   updated at mid
//	notes.md     3000 bytes, updated at mid-1h
//	deleted.pdf  1000 bytes, trashed
//	100%.txt     1000 bytes  (the LIKE-escaping case)
type corpus struct {
	*fixture
	mid time.Time
}

func newCorpus(t *testing.T) *corpus {
	t.Helper()
	f := newFixture(t)
	mid := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	c := &corpus{fixture: f, mid: mid}

	c.touch(c.file(f.root, "report.pdf", 1000), mid)
	c.touch(c.file(f.root, "Report.txt", 2000), mid.Add(time.Hour))
	c.touch(c.folder(f.root, "reports"), mid)
	c.touch(c.file(f.root, "notes.md", 3000), mid.Add(-time.Hour))
	c.touch(c.file(f.root, "100%.txt", 1000), mid)

	gone := c.file(f.root, "deleted.pdf", 1000)
	if err := c.store.Trash(c.ctx, c.owner, gone); err != nil {
		t.Fatalf("trashing the corpus's deleted file: %v", err)
	}
	return c
}

// Name matching is a case-insensitive substring, and the trash is invisible.
func TestSearchMatchesNamesCaseInsensitively(t *testing.T) {
	c := newCorpus(t)

	got := c.names(SearchQuery{Q: "report"})
	for _, want := range []string{"report.pdf", "Report.txt", "reports"} {
		if !hasName(got, want) {
			t.Errorf("searching %q did not return %q (got %v)", "report", want, got)
		}
	}
	if hasName(got, "notes.md") {
		t.Errorf("searching %q returned an unrelated name (got %v)", "report", got)
	}
	if hasName(got, "deleted.pdf") {
		t.Error("a trashed node came back from search")
	}
}

// A trashed node is never a result, whatever the filters say.
func TestSearchNeverReturnsTrashedNodes(t *testing.T) {
	c := newCorpus(t)
	if got := c.names(SearchQuery{Q: "deleted"}); len(got) != 0 {
		t.Errorf("search returned trashed nodes: %v", got)
	}
	if got := c.names(SearchQuery{}); hasName(got, "deleted.pdf") {
		t.Errorf("an unfiltered search returned a trashed node: %v", got)
	}
}

// Results are the caller's own nodes, full stop.
func TestSearchNeverReturnsAnotherUsersNodes(t *testing.T) {
	c := newCorpus(t)
	other := newFixture(t)
	other.file(other.root, "report.pdf", 1000)

	if got := other.names(SearchQuery{Q: "notes"}); len(got) != 0 {
		t.Errorf("the other user saw our nodes: %v", got)
	}
	got := c.names(SearchQuery{Q: "report.pdf"})
	if len(got) != 1 {
		t.Errorf("search returned %v, want only our own report.pdf", got)
	}
}

// LIKE's wildcards in the query text are literal characters.
func TestSearchEscapesLikeWildcards(t *testing.T) {
	c := newCorpus(t)
	got := c.names(SearchQuery{Q: "100%"})
	if len(got) != 1 || got[0] != "100%.txt" {
		t.Errorf("searching %q = %v, want just 100%%.txt", "100%", got)
	}
	if got := c.names(SearchQuery{Q: "_eport.pdf"}); len(got) != 0 {
		t.Errorf("underscore matched as a wildcard: %v", got)
	}
}

// after/before bound updated_at inclusively; min_size/max_size bound size
// inclusively, in bytes; a size filter never returns folders.
func TestSearchFilterBoundaries(t *testing.T) {
	c := newCorpus(t)
	at := func(d time.Duration) *time.Time { t := c.mid.Add(d); return &t }
	sz := func(n int64) *int64 { return &n }

	cases := []struct {
		name  string
		query SearchQuery
		want  []string
	}{
		{
			name:  "after is inclusive at the exact timestamp",
			query: SearchQuery{Q: "report", After: at(0)},
			want:  []string{"Report.txt", "report.pdf", "reports"},
		},
		{
			name:  "a microsecond past the timestamp excludes it",
			query: SearchQuery{Q: "report", After: at(time.Microsecond)},
			want:  []string{"Report.txt"},
		},
		{
			name:  "before is inclusive at the exact timestamp",
			query: SearchQuery{Q: "report", Before: at(0)},
			want:  []string{"report.pdf", "reports"},
		},
		{
			name:  "a microsecond before the timestamp excludes it",
			query: SearchQuery{Q: "report", Before: at(-time.Microsecond)},
			want:  nil,
		},
		{
			name:  "after and before AND together into a window",
			query: SearchQuery{After: at(0), Before: at(0)},
			want:  []string{"100%.txt", "report.pdf", "reports"},
		},
		{
			name:  "min_size is inclusive at the exact byte",
			query: SearchQuery{Q: "report", MinSize: sz(1000)},
			want:  []string{"Report.txt", "report.pdf"},
		},
		{
			name:  "one byte past min_size excludes the file",
			query: SearchQuery{Q: "report", MinSize: sz(1001)},
			want:  []string{"Report.txt"},
		},
		{
			name:  "max_size is inclusive at the exact byte",
			query: SearchQuery{Q: "report", MaxSize: sz(1000)},
			want:  []string{"report.pdf"},
		},
		{
			name:  "one byte below max_size excludes the file",
			query: SearchQuery{Q: "report", MaxSize: sz(999)},
			want:  nil,
		},
		{
			name:  "a size range AND-combines both ends",
			query: SearchQuery{MinSize: sz(1000), MaxSize: sz(2000)},
			want:  []string{"Report.txt", "100%.txt", "report.pdf"},
		},
		{
			name:  "a size filter never returns folders",
			query: SearchQuery{Q: "reports", MinSize: sz(0)},
			want:  nil,
		},
		{
			name:  "type=folder returns only folders",
			query: SearchQuery{Q: "report", Kind: KindFolder},
			want:  []string{"reports"},
		},
		{
			name:  "type=file returns only files",
			query: SearchQuery{Q: "report", Kind: KindFile},
			want:  []string{"Report.txt", "report.pdf"},
		},
		{
			name:  "every filter at once",
			query: SearchQuery{Q: "report", Kind: KindFile, After: at(0), Before: at(time.Hour), MinSize: sz(1000), MaxSize: sz(1000)},
			want:  []string{"report.pdf"},
		},
	}

	// Membership, not order: rows sharing an updated_at break the tie on a
	// random uuid. Ordering is asserted by TestSearchPaginates.
	for _, tc := range cases {
		got := c.names(tc.query)
		slices.Sort(got)
		want := slices.Clone(tc.want)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, want)
		}
	}
}

// Results are newest-updated first and page on (updated_at, id).
func TestSearchPaginates(t *testing.T) {
	c := newCorpus(t)

	var seen []string
	var cur *SearchCursor
	for range 10 {
		items, next, err := c.store.Search(c.ctx, c.owner, SearchQuery{Q: "report"}, cur, 1)
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		for _, n := range items {
			seen = append(seen, n.Name)
		}
		if next == nil {
			break
		}
		cur = next
	}

	want := []string{"Report.txt", "report.pdf", "reports"}
	if len(seen) != len(want) {
		t.Fatalf("paged through %v, want %v", seen, want)
	}
	// The two mid-stamped rows tie on updated_at and break on id, so only the
	// newest position is asserted; the set has to be complete either way.
	if seen[0] != want[0] {
		t.Errorf("first page = %q, want the most recently updated %q", seen[0], want[0])
	}
	for _, w := range want {
		if !hasName(seen, w) {
			t.Errorf("paging lost %q (saw %v)", w, seen)
		}
	}
}
