package upload

// The preview allowlist: the one thing standing between an uploaded file and a
// browser that will happily execute it.
//
// A download forces application/octet-stream on every object, so nothing served
// that way can run. A preview cannot -- rendering in place is the whole point --
// so the type has to come from somewhere, and the one place it must never come
// from is the client. nodes.mime is whatever the uploader's browser guessed (or
// whatever an attacker typed into the create call); here it is a lookup key and
// nothing else. What goes on the wire is always the map's own constant, so a
// value that is not in the map cannot reach the store's response headers even
// as a substring.
//
// Two consequences worth stating, because both look like omissions:
//
//   - SVG is absent on purpose. An SVG is script, and a presigned URL is
//     navigable -- serving one image/svg+xml would hand an attacker script
//     execution on the store origin, which is shared by every user of this
//     Drive. SVG previews are refused (415) and the UI offers a download.
//   - Every text-ish type collapses to text/plain. Markdown, CSV and JSON are
//     inert but browsers download them instead of showing them; source types
//     (text/x-*, application/javascript) are inert as text and dangerous as
//     anything else. text/plain renders, and no browser sniffs it up to HTML.
//
// XML is refused rather than collapsed, and that is the one refusal worth
// spelling out: application/xhtml+xml is markup a browser renders as a document,
// and text/xml with a stylesheet processing instruction is close enough. Since
// nothing here wants an XML preview, no key gets near the question.

import "strings"

// previewTextPlain is what every text-ish and source type is served as. It is a
// constant rather than a repeated literal because the prefix branch below
// returns it too: reaching into the map for previewTypes["text/plain"] would
// sign an empty content type the day somebody renames that key.
const previewTextPlain = "text/plain"

// previewTypes is the allowlist. Keys are normalized client-declared types;
// values are what the presign emits -- which is not always the key.
var previewTypes = map[string]string{
	"image/png":  "image/png",
	"image/jpeg": "image/jpeg",
	"image/gif":  "image/gif",
	"image/webp": "image/webp",
	"image/avif": "image/avif",

	"video/mp4":  "video/mp4",
	"video/webm": "video/webm",

	"audio/mpeg": "audio/mpeg",
	"audio/ogg":  "audio/ogg",
	"audio/wav":  "audio/wav",
	"audio/mp4":  "audio/mp4",

	"application/pdf": "application/pdf",

	"text/plain":    previewTextPlain,
	"text/markdown": previewTextPlain,
	"text/csv":      previewTextPlain,

	// Source and config types. text/javascript is the canonical one RFC 9239
	// settled on and the one a browser attaches to a .js upload, so refusing it
	// while previewing text/x-javascript was backwards. None of this widens the
	// posture: whatever the key, the value is the same inert constant.
	"application/json":       previewTextPlain,
	"application/javascript": previewTextPlain,
	"text/javascript":        previewTextPlain,
	"application/x-yaml":     previewTextPlain,
	"text/yaml":              previewTextPlain,
	"application/toml":       previewTextPlain,
}

// textSourcePrefix covers the text/x-* family -- text/x-go, text/x-python and
// the rest of the source types browsers have no opinion about. They render as
// text/plain like every other source file.
const textSourcePrefix = "text/x-"

// PreviewContentType maps a stored (client-declared) MIME to the type an inline
// preview may be served as, or reports that there is none.
//
// The input is normalized the way RFC 9110 says to read a media type -- case
// insensitive, parameters ignored -- and then looked up. It is never returned:
// the second result being true means the first is one of the constants above.
func PreviewContentType(raw string) (string, bool) {
	essence := strings.ToLower(strings.TrimSpace(raw))
	if i := strings.IndexByte(essence, ';'); i >= 0 {
		essence = strings.TrimSpace(essence[:i])
	}
	if ct, ok := previewTypes[essence]; ok {
		return ct, true
	}
	if strings.HasPrefix(essence, textSourcePrefix) {
		return previewTextPlain, true
	}
	return "", false
}
