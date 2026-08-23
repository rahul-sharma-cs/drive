package upload

// The allowlist is the whole security control for previews, so these cases are
// less about the types that work than about the ones that must not: an input
// that is not in the map has to come back refused, and an input that is in it
// has to come back as the map's own constant rather than as itself. Echoing the
// caller's string -- even after normalizing it -- would hand text/html straight
// through, which is the break these cases are written against.

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/rahul-sharma-cs/drive/server/internal/blob"
	"github.com/rahul-sharma-cs/drive/server/internal/config"
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
		{"the canonical javascript type becomes plain text", "text/javascript", "text/plain", true},
		{"yaml becomes plain text", "application/x-yaml", "text/plain", true},
		{"the other yaml spelling becomes plain text", "text/yaml", "text/plain", true},
		{"toml becomes plain text", "application/toml", "text/plain", true},
		{"a source type becomes plain text", "text/x-go", "text/plain", true},
		{"any text/x- subtype becomes plain text", "text/x-python", "text/plain", true},

		// The text/x- branch returns a constant, not a map lookup: even the two
		// subtypes named after the formats that are refused above come back as
		// inert plain text rather than as an empty content type.
		{"text/x-html is plain text, not markup", "text/x-html", "text/plain", true},
		{"text/x-svg is plain text, not an image", "text/x-svg", "text/plain", true},

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

		// XML is refused rather than collapsed to text/plain. xhtml renders as a
		// document, and an xml document can carry a stylesheet instruction --
		// neither is something to hand a browser off the store's origin, and
		// "text/x" is not a prefix of "text/xml", which is the thing to keep true.
		{"xhtml is refused", "application/xhtml+xml", "", false},
		{"application/xml is refused", "application/xml", "", false},
		{"text/xml is refused", "text/xml", "", false},
		{"text/xml with parameters is still refused", "text/xml; charset=utf-8", "", false},
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

// ------------------------------------------------ the signature's coverage --

// A fabricated store. Presigning is a local HMAC, so nothing here reaches the
// network and none of these values needs to be real: what the cases below read
// is the shape of the signed query.
const (
	previewSignEndpoint = "https://s3.example.test"
	previewSignBucket   = "preview-signing"
	previewSignKey      = "blobs/0123456789abcdef0123456789abcdef"
	previewSignName     = `مرحبا "quarterly" 🚀.pdf`
)

func previewSignPresigner(t *testing.T) *Presigner {
	t.Helper()
	cfg := &config.Config{
		S3Endpoint:  previewSignEndpoint,
		S3Bucket:    previewSignBucket,
		S3AccessKey: "0123456789ABCDEF0123",
		S3SecretKey: "0123456789abcdef0123456789abcdef01234567",
		S3Region:    config.DefaultS3Region,
		PresignTTL:  time.Hour,
	}
	s3c, presign, err := blob.New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("building the presigner: %v", err)
	}
	return &Presigner{S3: s3c, Presign: presign, Bucket: cfg.S3Bucket, TTL: cfg.PresignTTL}
}

// signedPreview presigns through the production path and hands back the parsed
// query.
func signedPreview(t *testing.T, p *Presigner, contentType string) url.Values {
	t.Helper()
	signed, err := p.PreviewURL(context.Background(), previewSignKey, previewSignName, contentType)
	if err != nil {
		t.Fatalf("presigning a %s preview: %v", contentType, err)
	}
	u, err := url.Parse(signed.URL)
	if err != nil {
		t.Fatalf("the %s preview URL does not parse: %v", contentType, err)
	}
	return u.Query()
}

// signedRaw presigns the same object with one parameter changed, through the
// same presigner. It exists because no production call signs an inline
// disposition and an attachment disposition for the same content type, and the
// disposition has to be varied on its own to be shown to matter on its own.
func signedRaw(t *testing.T, p *Presigner, contentType, disposition string) url.Values {
	t.Helper()
	req, err := p.Presign.PresignGetObject(context.Background(), &s3.GetObjectInput{
		Bucket:                     aws.String(p.Bucket),
		Key:                        aws.String(previewSignKey),
		ResponseContentDisposition: aws.String(disposition),
		ResponseContentType:        aws.String(contentType),
	}, s3.WithPresignExpires(p.TTL))
	if err != nil {
		t.Fatalf("presigning %s as %q: %v", contentType, disposition, err)
	}
	u, err := url.Parse(req.URL)
	if err != nil {
		t.Fatalf("the %s URL does not parse: %v", contentType, err)
	}
	return u.Query()
}

// The response-content-* overrides have to be signed inputs, not decoration.
//
// Every other assertion about them -- here, in the api package, in the store
// round trip -- checks that the parameter is *present* in the query. A URL
// assembled by appending the two parameters onto an already-signed string would
// satisfy every one of those and still be broken in the only way that matters:
// whoever holds the link could rewrite response-content-type to text/html and
// have the store render markup on its own origin. The property that rules that
// out is that changing an override changes the signature, so it is asserted
// directly.
//
// The clock is the only other input that moves. SigV4 stamps it to the second,
// so the whole set is signed again if it straddles a boundary, and one pair is
// signed identically on purpose: if two identical presigns in the same second
// did not agree, a difference between two different ones would prove nothing.
func TestPreviewOverridesAreInsideTheSignature(t *testing.T) {
	p := previewSignPresigner(t)
	inline, attachment := InlineDisposition(previewSignName), AttachmentDisposition(previewSignName)

	var pdf, text, download, pdfAgain url.Values
	for attempt := 0; ; attempt++ {
		if attempt == 20 {
			t.Fatal("could not sign four URLs inside one second")
		}
		pdf = signedPreview(t, p, "application/pdf")
		text = signedPreview(t, p, "text/plain")
		download = signedRaw(t, p, "application/pdf", attachment)
		pdfAgain = signedPreview(t, p, "application/pdf")

		stamp := pdf.Get("X-Amz-Date")
		if stamp != "" && stamp == text.Get("X-Amz-Date") &&
			stamp == download.Get("X-Amz-Date") && stamp == pdfAgain.Get("X-Amz-Date") {
			break
		}
	}

	// Nothing but Host is a signed header, which is what leaves the overrides
	// nowhere to ride except the signed query. A SignedHeaders list that grew
	// would mean the signature covers something a browser does not send.
	for what, q := range map[string]url.Values{
		"the pdf preview": pdf, "the text preview": text, "the download": download,
	} {
		if got := q.Get("X-Amz-SignedHeaders"); got != "host" {
			t.Errorf("%s signs headers %q, want just \"host\"", what, got)
		}
	}

	// The three URLs are what they claim to be before their signatures are
	// compared: a missing override would also produce a different signature.
	for _, c := range []struct {
		what        string
		q           url.Values
		contentType string
		disposition string
	}{
		{"the pdf preview", pdf, "application/pdf", inline},
		{"the text preview", text, "text/plain", inline},
		{"the download", download, "application/pdf", attachment},
	} {
		if got := c.q.Get("response-content-type"); got != c.contentType {
			t.Fatalf("%s carries response-content-type %q, want %q", c.what, got, c.contentType)
		}
		if got := c.q.Get("response-content-disposition"); got != c.disposition {
			t.Fatalf("%s carries response-content-disposition %q, want %q", c.what, got, c.disposition)
		}
	}

	// The control itself. Two presigns differing only in the content type, and
	// two differing only in the disposition, must not share a signature.
	sig := func(q url.Values) string { return q.Get("X-Amz-Signature") }
	if sig(pdf) == "" {
		t.Fatal("the preview URL is not signed at all")
	}
	if sig(pdf) != sig(pdfAgain) {
		t.Fatalf("two identical presigns in the same second disagree (%s vs %s), so nothing below means anything",
			sig(pdf), sig(pdfAgain))
	}
	if sig(pdf) == sig(text) {
		t.Errorf("application/pdf and text/plain sign identically (%s): the content type is outside the signature, "+
			"so a URL holder can rewrite it to text/html", sig(pdf))
	}
	if sig(pdf) == sig(download) {
		t.Errorf("inline and attachment sign identically (%s): the disposition is outside the signature", sig(pdf))
	}
	if sig(text) == sig(download) {
		t.Errorf("two URLs differing in both overrides sign identically (%s)", sig(text))
	}
}
