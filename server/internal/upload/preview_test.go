package upload

// The allowlist is the whole security control for previews, so these cases are
// less about the types that work than about the ones that must not: an input
// that is not in the map has to come back refused, and an input that is in it
// has to come back as the map's own constant rather than as itself. Echoing the
// caller's string -- even after normalizing it -- would hand text/html straight
// through, which is the break these cases are written against.

import (
	"strings"
	"testing"
)

func TestPreviewContentTypeAllowsOnlyTheMapAndReturnsItsOwnConstant(t *testing.T) {
	cases := []struct {
		what  string
		raw   string
		want  string
		allow bool
	}{
		// Rendered as themselves.
		{"png", "image/png", "image/png", true},
		{"jpeg", "image/jpeg", "image/jpeg", true},
		{"gif", "image/gif", "image/gif", true},
		{"webp", "image/webp", "image/webp", true},
		{"avif", "image/avif", "image/avif", true},
		{"mp4 video", "video/mp4", "video/mp4", true},
		{"webm", "video/webm", "video/webm", true},
		{"mp3", "audio/mpeg", "audio/mpeg", true},
		{"ogg audio", "audio/ogg", "audio/ogg", true},
		{"wav", "audio/wav", "audio/wav", true},
		{"m4a", "audio/mp4", "audio/mp4", true},
		{"pdf", "application/pdf", "application/pdf", true},
		{"plain text", "text/plain", "text/plain", true},

		// Inert, but browsers download them under their own type, so they are
		// served as text/plain instead.
		{"markdown becomes plain text", "text/markdown", "text/plain", true},
		{"csv becomes plain text", "text/csv", "text/plain", true},
		{"json becomes plain text", "application/json", "text/plain", true},
		{"javascript becomes plain text", "application/javascript", "text/plain", true},
		{"a source type becomes plain text", "text/x-go", "text/plain", true},
		{"any text/x- subtype becomes plain text", "text/x-python", "text/plain", true},

		// Normalization: the answer is the constant, never the input.
		{"case is ignored", "IMAGE/PNG", "image/png", true},
		{"parameters are ignored", "text/plain; charset=utf-8", "text/plain", true},
		{"surrounding space is ignored", "  application/pdf  ", "application/pdf", true},
		{"a parameterized alias still normalizes", "Application/JSON;charset=UTF-8", "text/plain", true},

		// Refused. text/html and SVG are the two that would be script on the
		// store origin; the rest simply have no preview.
		{"html is refused", "text/html", "", false},
		{"svg is refused", "image/svg+xml", "", false},
		{"svg with parameters is still refused", "image/svg+xml; charset=utf-8", "", false},
		{"xhtml is refused", "application/xhtml+xml", "", false},
		{"an empty mime is refused", "", "", false},
		{"whitespace is refused", "   ", "", false},
		{"the upload default is refused", "application/octet-stream", "", false},
		{"a type that merely starts like an allowed one is refused", "image/png-evil", "", false},
		{"a bare parameter is refused", "; charset=utf-8", "", false},
		{"an unknown video is refused", "video/quicktime", "", false},
		{"a zip is refused", "application/zip", "", false},
	}

	for _, c := range cases {
		t.Run(c.what, func(t *testing.T) {
			got, ok := PreviewContentType(c.raw)
			if ok != c.allow {
				t.Fatalf("PreviewContentType(%q) allowed = %v, want %v", c.raw, ok, c.allow)
			}
			if got != c.want {
				t.Errorf("PreviewContentType(%q) = %q, want %q", c.raw, got, c.want)
			}
		})
	}
}

// A refused type must not leak its own string into the first return value
// either: the caller signs whatever it is handed, and "" is the only value that
// cannot be signed by accident.
func TestPreviewContentTypeReturnsNothingWhenItRefuses(t *testing.T) {
	for _, raw := range []string{"text/html", "image/svg+xml", "application/x-httpd-php", ""} {
		if got, _ := PreviewContentType(raw); got != "" {
			t.Errorf("PreviewContentType(%q) returned %q with its refusal", raw, got)
		}
	}
}

// Inline and attachment must name the file through the same escaping path. A
// preview whose disposition were built separately would be the one place where
// a quote or a control character in a name could end the quoted string early --
// and it would be the place where it matters most, because that response is the
// one a browser is asked to render.
func TestInlineDispositionEscapesExactlyLikeAttachment(t *testing.T) {
	names := []string{
		`a"b\c.txt`,
		"my report.pdf",
		"🚀.bin",
		"مرحبا.txt",
		"100%*'.txt",
		"",
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			inline, attachment := InlineDisposition(name), AttachmentDisposition(name)

			inlineParams, ok := strings.CutPrefix(inline, "inline; ")
			if !ok {
				t.Fatalf("InlineDisposition(%q) = %q, which is not an inline disposition", name, inline)
			}
			attachmentParams, ok := strings.CutPrefix(attachment, "attachment; ")
			if !ok {
				t.Fatalf("AttachmentDisposition(%q) = %q, which is not an attachment disposition", name, attachment)
			}
			if inlineParams != attachmentParams {
				t.Errorf("the two dispositions name %q differently:\n inline     %q\n attachment %q",
					name, inlineParams, attachmentParams)
			}

			// The escaping itself, asserted on the inline value rather than
			// relative to the other one, so a break in the shared helper cannot
			// hide by changing both.
			fallback, rest, found := strings.Cut(strings.TrimPrefix(inlineParams, `filename="`), `"; filename*=UTF-8''`)
			if !found {
				t.Fatalf("inline disposition %q is missing one of the two filename forms", inline)
			}
			if strings.ContainsAny(fallback, "\"\\") {
				t.Errorf("the quoted fallback %q can end the quoted string early", fallback)
			}
			for i := 0; i < len(fallback); i++ {
				if fallback[i] < 0x20 || fallback[i] >= 0x7F {
					t.Errorf("the quoted fallback %q carries a byte no client can read: %#x", fallback, fallback[i])
					break
				}
			}
			for i := 0; i < len(rest); i++ {
				if rest[i] >= 0x80 {
					t.Errorf("the extended form %q carries a raw non-ASCII byte instead of %%XX", rest)
					break
				}
			}
		})
	}
}
