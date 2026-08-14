package upload

import (
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeETag(t *testing.T) {
	cases := map[string]string{
		`"D41D8CD98F00B204E9800998ECF8427E"`: "d41d8cd98f00b204e9800998ecf8427e",
		`W/"abc123"`:                         "abc123",
		` "abc123" `:                         "abc123",
		`abc123`:                             "abc123",
		``:                                   "",
	}
	for in, want := range cases {
		if got := NormalizeETag(in); got != want {
			t.Errorf("NormalizeETag(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCleanMD5(t *testing.T) {
	good := "D41D8CD98F00B204E9800998ECF8427E"
	got, err := CleanMD5(`"` + good + `"`)
	if err != nil || got != strings.ToLower(good) {
		t.Fatalf("CleanMD5(%q) = %q, %v", good, got, err)
	}
	for _, bad := range []string{"", "abc", strings.Repeat("z", 32), good + "00"} {
		if _, err := CleanMD5(bad); err == nil {
			t.Errorf("CleanMD5(%q) was accepted", bad)
		}
	}
}

func TestCleanFingerprint(t *testing.T) {
	if _, err := CleanFingerprint("  abc123  "); err != nil {
		t.Fatalf("a plain fingerprint was rejected: %v", err)
	}
	// The contract does not pin the encoding, so base64 has to pass too.
	if _, err := CleanFingerprint("3q2+7w==/base64+like"); err != nil {
		t.Fatalf("a base64-shaped fingerprint was rejected: %v", err)
	}
	for _, bad := range []string{"", "   ", strings.Repeat("a", maxFingerprint+1), "line\nbreak", "emoji \U0001F600"} {
		if _, err := CleanFingerprint(bad); err == nil {
			t.Errorf("CleanFingerprint(%q) was accepted", bad)
		}
	}
}

func md5p(s string) *string { return &s }

func TestPinnedPartsSkipsReconciledRows(t *testing.T) {
	cases := []struct {
		name  string
		parts []Part
		want  []int
	}{
		{name: "no parts at all", parts: nil, want: nil},
		{
			// Rows adopted from ListParts have no client MD5, so there is
			// nothing to compare and they cannot be pinned.
			name:  "every part came from reconciliation",
			parts: []Part{{Number: 1}, {Number: 4}},
			want:  nil,
		},
		{
			name:  "the pins skip the reconciled rows at both ends",
			parts: []Part{{Number: 1}, {Number: 2, MD5: md5p("aa")}, {Number: 5, MD5: md5p("bb")}, {Number: 9}},
			want:  []int{2, 5},
		},
		{
			name:  "a single confirmed part pins to itself",
			parts: []Part{{Number: 3, MD5: md5p("aa")}},
			want:  []int{3, 3},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := PinnedParts(c.parts); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("PinnedParts = %v, want %v", got, c.want)
			}
		})
	}
}

func TestVerifyPins(t *testing.T) {
	ledger := []Part{
		{Number: 2, MD5: md5p("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")},
		{Number: 5, MD5: md5p("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")},
		{Number: 7}, // adopted from ListParts: no MD5
	}

	cases := []struct {
		name        string
		pinned      []int
		supplied    map[string]string
		wantCovered bool
		wantOK      bool
	}{
		{
			name:   "nothing supplied is the bounce",
			pinned: []int{2, 5},
		},
		{
			name:     "only half the pins is still the bounce",
			pinned:   []int{2, 5},
			supplied: map[string]string{"2": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		},
		{
			name:   "both pins match",
			pinned: []int{2, 5},
			supplied: map[string]string{
				"2": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
				"5": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			},
			wantCovered: true, wantOK: true,
		},
		{
			name:   "a chimera: same edges, different middle",
			pinned: []int{2, 5},
			supplied: map[string]string{
				"2": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				"5": "cccccccccccccccccccccccccccccccc",
			},
			wantCovered: true,
		},
		{
			name:        "a pair pinned to one part needs one answer",
			pinned:      []int{2, 2},
			supplied:    map[string]string{"2": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
			wantCovered: true, wantOK: true,
		},
		{
			// The row lost its MD5 to reconciliation, so the client's answer
			// can no longer be checked -- refuse rather than wave it through.
			name:        "a pin whose ledger MD5 is gone cannot pass",
			pinned:      []int{7},
			supplied:    map[string]string{"7": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
			wantCovered: true,
		},
		{
			name:   "extra entries are ignored",
			pinned: []int{2},
			supplied: map[string]string{
				"2": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				"9": "dddddddddddddddddddddddddddddddd",
			},
			wantCovered: true, wantOK: true,
		},
		{
			name:        "a malformed digest is not coverage",
			pinned:      []int{2},
			supplied:    map[string]string{"2": "nope"},
			wantCovered: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			covered, ok := VerifyPins(c.pinned, ledger, c.supplied)
			if covered != c.wantCovered || ok != c.wantOK {
				t.Fatalf("VerifyPins = (covered %v, ok %v), want (%v, %v)",
					covered, ok, c.wantCovered, c.wantOK)
			}
		})
	}
}
