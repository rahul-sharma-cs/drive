package integration

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Filenames survive the whole round trip -- create, store, read back, rename,
// list -- byte for byte.
//
// The interesting cases are the ones a naive layer mangles: emoji (multi-byte
// and beyond the BMP, so anything counting characters as bytes truncates them),
// and real right-to-left text, which filename hygiene must leave completely
// alone even though it strips the bidi *override* characters that disguise an
// extension.
func TestEmojiAndRTLFilenamesRoundTrip(t *testing.T) {
	owner := H.NewUser(t)
	parent := owner.CreateFolder(t, owner.RootID, "names-"+uuid.NewString()[:8])

	cases := []struct {
		what    string
		created string
		renamed string
	}{
		{"emoji", "📁 Q3 report 🎉.txt", "🚀 launch notes 🇮🇳.txt"},
		{"hebrew", "תיקיית מסמכים", "מסמכים ישנים"},
		{"arabic", "ملف الاختبار.txt", "تقرير نهائي.txt"},
		{"mixed rtl and emoji", "מסמך 📎 2024.pdf", "📎 مستند 2025.pdf"},
	}

	for _, c := range cases {
		t.Run(c.what, func(t *testing.T) {
			created := owner.CreateFolder(t, parent.ID, c.created)
			if created.Name != c.created {
				t.Fatalf("create returned %q, want %q", created.Name, c.created)
			}

			// Read back through the API...
			got := owner.Get(t, "/api/nodes/"+created.ID.String()).Expect(http.StatusOK).Node()
			if got.Name != c.created {
				t.Errorf("GET /nodes returned %q, want %q", got.Name, c.created)
			}

			// ...and straight out of Postgres, so an encoding fault anywhere
			// between the two shows up as a difference here.
			var stored string
			if err := H.Pool.QueryRow(context.Background(),
				`SELECT name FROM nodes WHERE id = $1`, created.ID).Scan(&stored); err != nil {
				t.Fatalf("reading the stored name: %v", err)
			}
			if stored != c.created {
				t.Errorf("stored name %q, want %q", stored, c.created)
			}

			renamed := owner.Patch(t, "/api/nodes/"+created.ID.String(),
				map[string]any{"name": c.renamed}).Expect(http.StatusOK).Node()
			if renamed.Name != c.renamed {
				t.Errorf("rename returned %q, want %q", renamed.Name, c.renamed)
			}

			names := owner.Get(t, "/api/nodes/"+parent.ID.String()+"/children").
				Expect(http.StatusOK).List().Names()
			if !contains(names, c.renamed) {
				t.Errorf("the children listing does not contain %q: %v", c.renamed, names)
			}
			if contains(names, c.created) {
				t.Errorf("the children listing still contains the old name %q: %v", c.created, names)
			}

			// Search finds it too: pg_trgm has to survive the same bytes.
			found := owner.Get(t, "/api/search?q="+url.QueryEscape(c.renamed)).
				Expect(http.StatusOK).List().Names()
			if !contains(found, c.renamed) {
				t.Errorf("search for %q returned %v", c.renamed, found)
			}
		})
	}
}

// Real RTL text survives; the bidi override characters that make "report.txt"
// display as "report.exe" do not. This is filename hygiene (PLAN §Sharing)
// asserted where a client would actually meet it.
func TestBidiOverrideCharactersAreStrippedFromNames(t *testing.T) {
	owner := H.NewUser(t)
	parent := owner.CreateFolder(t, owner.RootID, "bidi-"+uuid.NewString()[:8])

	// U+202E RIGHT-TO-LEFT OVERRIDE between the stem and the extension.
	created := owner.CreateFolder(t, parent.ID, "invoice‮gnp.pdf")
	if strings.ContainsRune(created.Name, '‮') {
		t.Errorf("the stored name still carries U+202E: %q", created.Name)
	}
	if created.Name != "invoicegnp.pdf" {
		t.Errorf("name = %q, want %q", created.Name, "invoicegnp.pdf")
	}
}
