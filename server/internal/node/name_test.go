package node

import (
	"errors"
	"strings"
	"testing"
)

func TestCleanAccepts(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "plain", in: "report.pdf", want: "report.pdf"},
		{name: "outer whitespace is trimmed", in: "  report.pdf\t", want: "report.pdf"},
		{name: "inner spaces survive", in: "my holiday photos.zip", want: "my holiday photos.zip"},
		{name: "null bytes are stripped", in: "a\x00b.txt", want: "ab.txt"},
		{name: "CR and LF are stripped", in: "a\r\nb.txt", want: "ab.txt"},
		{name: "every C0 control is stripped", in: "\x01a\x1fb\x08.txt", want: "ab.txt"},
		{name: "DEL is stripped", in: "a\x7fb.txt", want: "ab.txt"},
		{name: "RTL override is stripped", in: "invoice‮gnp.exe", want: "invoicegnp.exe"},
		{name: "LRM and RLM are stripped", in: "a‎b‏c.txt", want: "abc.txt"},
		{name: "bidi isolates are stripped", in: "a⁦b⁩.txt", want: "ab.txt"},
		{name: "emoji survive", in: "vacation 🏖️.jpg", want: "vacation 🏖️.jpg"},
		{name: "RTL text survives", in: "مرحبا.txt", want: "مرحبا.txt"},
		{name: "trailing space is trimmed, not rejected", in: "notes ", want: "notes"},
		{name: "non-breaking space is trimmed", in: "notes ", want: "notes"},
		{name: "NFC normalization", in: "café.txt", want: "café.txt"},
		{name: "a name that is reserved-adjacent is fine", in: "CONS.txt", want: "CONS.txt"},
		{name: "COM10 is not a device", in: "com10.txt", want: "com10.txt"},
		{name: "dotfile", in: ".env", want: ".env"},
		{name: "leading dots are fine", in: "..config", want: "..config"},
		{name: "255 bytes exactly", in: strings.Repeat("a", 255), want: strings.Repeat("a", 255)},
		// 127 two-byte runes plus one ASCII byte is 255 bytes: at the limit in
		// bytes, well under it in runes.
		{name: "255 bytes of multi-byte runes", in: strings.Repeat("é", 127) + "a", want: strings.Repeat("é", 127) + "a"},
		// 255 bytes decomposed, 170 bytes once NFC-composed: the limit applies
		// to what we store.
		{name: "the byte limit is measured after NFC", in: strings.Repeat("é", 85), want: strings.Repeat("é", 85)},
	}

	for _, c := range cases {
		got, err := Clean(c.in)
		if err != nil {
			t.Errorf("%s: Clean(%q) = error %v", c.name, c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: Clean(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
		if len(got) > MaxNameBytes {
			t.Errorf("%s: Clean(%q) is %d bytes", c.name, c.in, len(got))
		}
	}
}

func TestCleanRejects(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{name: "empty", in: ""},
		{name: "whitespace only", in: "   "},
		{name: "controls only", in: "\x01\x02"},
		{name: "forward slash", in: "a/b.txt"},
		{name: "backslash", in: `a\b.txt`},
		{name: "a path", in: "/etc/passwd"},
		{name: "dot", in: "."},
		{name: "dot dot", in: ".."},
		{name: "dot dot after stripping", in: "..\x00"},
		{name: "trailing dot", in: "report."},
		{name: "trailing dots", in: "report..."},
		{name: "CON", in: "CON"},
		{name: "con lowercase", in: "con"},
		{name: "CON with an extension", in: "con.txt"},
		{name: "mixed-case device", in: "PrN.log"},
		{name: "AUX", in: "aux"},
		{name: "NUL", in: "NUL.dat"},
		{name: "COM1", in: "COM1"},
		{name: "COM9 with two extensions", in: "com9.tar.gz"},
		{name: "LPT1", in: "lpt1.txt"},
		{name: "LPT9", in: "LPT9"},
		{name: "256 bytes", in: strings.Repeat("a", 256)},
		{name: "256 bytes of multi-byte runes", in: strings.Repeat("é", 128)},
		{name: "invalid UTF-8", in: "a\xffb.txt"},
	}

	for _, c := range cases {
		got, err := Clean(c.in)
		if !errors.Is(err, ErrInvalidName) {
			t.Errorf("%s: Clean(%q) = (%q, %v), want ErrInvalidName", c.name, c.in, got, err)
		}
	}
}

func TestNextFreeName(t *testing.T) {
	cases := []struct {
		name  string
		want  string
		in    string
		taken []string
	}{
		{name: "nothing taken", in: "a.txt", want: "a.txt"},
		{name: "unrelated siblings", in: "a.txt", taken: []string{"b.txt", "c.txt"}, want: "a.txt"},
		{name: "first collision", in: "a.txt", taken: []string{"a.txt"}, want: "a (1).txt"},
		{
			name:  "walks past existing numbered names",
			in:    "a.txt",
			taken: []string{"a.txt", "a (1).txt", "a (2).txt"},
			want:  "a (3).txt",
		},
		{
			name:  "fills the first gap",
			in:    "a.txt",
			taken: []string{"a.txt", "a (2).txt"},
			want:  "a (1).txt",
		},
		{
			name:  "case-insensitive, like the sibling index",
			in:    "A.TXT",
			taken: []string{"a.txt"},
			want:  "A (1).TXT",
		},
		{
			name:  "case-insensitive against numbered names too",
			in:    "report.pdf",
			taken: []string{"REPORT.PDF", "Report (1).pdf"},
			want:  "report (2).pdf",
		},
		{name: "no extension", in: "report", taken: []string{"report"}, want: "report (1)"},
		{name: "dotfile keeps its leading dot", in: ".env", taken: []string{".env"}, want: ".env (1)"},
		{
			name:  "only the last extension is preserved",
			in:    "a.tar.gz",
			taken: []string{"a.tar.gz"},
			want:  "a.tar (1).gz",
		},
		{
			name:  "the canonical keep-both rename",
			in:    "report.pdf",
			taken: []string{"report.pdf"},
			want:  "report (1).pdf",
		},
	}

	for _, c := range cases {
		if got := NextFreeName(c.in, c.taken); got != c.want {
			t.Errorf("%s: NextFreeName(%q, %v) = %q, want %q", c.name, c.in, c.taken, got, c.want)
		}
	}
}

// A rename must not push the name past the byte limit: the stem gives way, the
// counter and the extension do not.
func TestNextFreeNameStaysWithinTheByteLimit(t *testing.T) {
	long := strings.Repeat("a", MaxNameBytes-len(".txt")) + ".txt"
	got := NextFreeName(long, []string{long})

	if len(got) > MaxNameBytes {
		t.Errorf("NextFreeName produced %d bytes, the limit is %d", len(got), MaxNameBytes)
	}
	if !strings.HasSuffix(got, " (1).txt") {
		t.Errorf("NextFreeName = %q, want it to end with \" (1).txt\"", got)
	}
	if _, err := Clean(got); err != nil {
		t.Errorf("NextFreeName produced a name Clean rejects: %v", err)
	}

	// Multi-byte stems are cut on a rune boundary, not mid-rune.
	wide := strings.Repeat("é", 125) + ".txt"
	got = NextFreeName(wide, []string{wide})
	if len(got) > MaxNameBytes {
		t.Errorf("NextFreeName produced %d bytes for a multi-byte stem", len(got))
	}
	if _, err := Clean(got); err != nil {
		t.Errorf("multi-byte truncation produced an invalid name: %v", err)
	}
}

func TestLikePatternEscapesWildcards(t *testing.T) {
	// A file literally called "100%_of_it.txt" must not turn into a pattern
	// that matches half the folder.
	stem, ext := splitExt("100%_of_it.txt")
	if got, want := likePattern(stem, ext), `100\%\_of\_it (%).txt`; got != want {
		t.Errorf("likePattern = %q, want %q", got, want)
	}
}
