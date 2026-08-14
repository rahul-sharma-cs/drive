package node

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// MaxNameBytes is the hard limit on a node name, measured in UTF-8 bytes (not
// runes): a 255-rune emoji name is far past it, a 255-byte multi-byte name is
// exactly at it.
const MaxNameBytes = 255

// reservedNames are the Windows device names. A file called CON, con.txt or
// COM1.tar.gz is unopenable on Windows and unsafe to hand a sync client, so we
// refuse them at the API boundary regardless of the caller's platform.
var reservedNames = func() map[string]bool {
	m := map[string]bool{"con": true, "prn": true, "aux": true, "nul": true}
	for i := 1; i <= 9; i++ {
		m[fmt.Sprintf("com%d", i)] = true
		m[fmt.Sprintf("lpt%d", i)] = true
	}
	return m
}()

// Clean normalizes and validates a user-supplied name, returning the exact
// string to store. It is the single gate in front of every create and rename.
//
// In order: reject invalid UTF-8; strip the characters that must never reach a
// filesystem, an HTTP header or a terminal (NUL, every C0 control including
// CR/LF, DEL, and the bidi override/isolate marks); NFC-normalize; trim outer
// whitespace; then reject what is left if it is unusable.
//
// Emoji and real RTL *text* survive untouched -- only the override control
// characters are stripped, because those are what let a name display with its
// extension reversed on screen.
func Clean(raw string) (string, error) {
	if !utf8.ValidString(raw) {
		return "", fmt.Errorf("%w: not valid UTF-8", ErrInvalidName)
	}

	name := strings.TrimFunc(norm.NFC.String(stripUnsafe(raw)), unicode.IsSpace)

	switch {
	case name == "":
		return "", fmt.Errorf("%w: empty", ErrInvalidName)
	case strings.ContainsAny(name, `/\`):
		return "", fmt.Errorf(`%w: must not contain / or \`, ErrInvalidName)
	case name == "." || name == "..":
		return "", fmt.Errorf("%w: %q is reserved", ErrInvalidName, name)
	case strings.HasSuffix(name, ".") || strings.HasSuffix(name, " "):
		return "", fmt.Errorf("%w: must not end with a dot or a space", ErrInvalidName)
	case reservedNames[strings.ToLower(baseOf(name))]:
		return "", fmt.Errorf("%w: %q is a reserved device name", ErrInvalidName, baseOf(name))
	case len(name) > MaxNameBytes:
		return "", fmt.Errorf("%w: %d bytes, the limit is %d", ErrInvalidName, len(name), MaxNameBytes)
	}
	return name, nil
}

// stripUnsafe drops the characters no name may carry: C0 controls (CR and LF
// among them, so no name can inject a header), DEL, and the bidi
// override/isolate/mark characters used to disguise extensions.
func stripUnsafe(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r < 0x20, r == 0x7f:
		case r == '\u200e', r == '\u200f': // LRM, RLM
		case r >= '\u202a' && r <= '\u202e': // LRE, RLE, PDF, LRO, RLO
		case r >= '\u2066' && r <= '\u2069': // LRI, RLI, FSI, PDI
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// baseOf returns the part before the first dot -- what Windows treats as the
// device name, so that "con.txt" is caught along with "CON".
func baseOf(name string) string {
	if i := strings.IndexByte(name, '.'); i >= 0 {
		return name[:i]
	}
	return name
}

// NextFreeName returns the first name not already taken, walking the
// "report (1).pdf", "report (2).pdf" sequence. Comparison is case-insensitive
// because the sibling uniqueness index is on lower(name).
//
// It is deterministic: the same desired name against the same set of siblings
// always yields the same answer, which is what makes an unattended upload's
// auto-rename reproducible.
func NextFreeName(name string, taken []string) string {
	if len(taken) == 0 {
		return name
	}
	set := make(map[string]bool, len(taken))
	for _, t := range taken {
		set[strings.ToLower(t)] = true
	}
	if !set[strings.ToLower(name)] {
		return name
	}

	stem, ext := splitExt(name)
	for n := 1; ; n++ {
		candidate := numberedName(stem, ext, n)
		if !set[strings.ToLower(candidate)] {
			return candidate
		}
	}
}

// splitExt splits at the last dot, so "a.tar.gz" keeps ".gz" and a dotfile
// like ".env" is all stem (its leading dot is not an extension).
func splitExt(name string) (stem, ext string) {
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		return name[:i], name[i:]
	}
	return name, ""
}

// numberedName builds "stem (n)ext", shortening the stem if the result would
// break the byte limit. The counter and the extension are never sacrificed.
func numberedName(stem, ext string, n int) string {
	suffix := fmt.Sprintf(" (%d)", n)
	return truncateBytes(stem, MaxNameBytes-len(suffix)-len(ext)) + suffix + ext
}

// truncateBytes cuts s to at most max bytes on a rune boundary.
func truncateBytes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	for len(s) > max {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return s
}

// likePattern builds the LIKE pattern that matches a name and its numbered
// variants, with the wildcards in the caller's text escaped (escapeLike lives
// in search.go, same package).
func likePattern(stem, ext string) string {
	return escapeLike(strings.ToLower(stem)) + ` (%)` + escapeLike(strings.ToLower(ext))
}
