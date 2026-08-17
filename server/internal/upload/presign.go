package upload

// The presigner: the only place that talks to Garage's multipart API.
//
// Presigning is a local HMAC -- no round trip, no rate limit -- which is why
// the resume handshake hands out a fresh URL for every missing part rather
// than rationing them. 500 URLs cost nothing, and a client that is never short
// of URLs never stalls mid-transfer.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
)

// listPartsPage is Garage's (and S3's) maximum page size for ListParts. A
// multi-GB file at 10 MiB parts has more parts than one page holds, so every
// caller must paginate to exhaustion.
const listPartsPage = 1000

// PresignedPart is one part URL on the wire.
type PresignedPart struct {
	PartNumber int       `json:"part_number"`
	URL        string    `json:"url"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// Presigner wraps the S3 client and its presigner with the bucket and TTL.
type Presigner struct {
	S3      *s3.Client
	Presign *s3.PresignClient
	Bucket  string
	TTL     time.Duration
}

// NewObjectKey is the key layout for uploaded blobs.
func NewObjectKey() string { return "blobs/" + uuid.NewString() }

// CreateMultipart opens a multipart upload.
//
// No ContentType and no ChecksumAlgorithm, ever: objects stored without a
// Content-Type are what keeps uploaded HTML from being renderable when it is
// served back, and an SDK checksum parameter leaking into a presigned query
// breaks the PUT outright.
func (p *Presigner) CreateMultipart(ctx context.Context, key string) (string, error) {
	out, err := p.S3.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(p.Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return "", fmt.Errorf("creating multipart upload for %s: %w", key, err)
	}
	return aws.ToString(out.UploadId), nil
}

// AbortMultipart discards a multipart upload and everything uploaded to it.
func (p *Presigner) AbortMultipart(ctx context.Context, key, uploadID string) error {
	_, err := p.S3.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(p.Bucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadID),
	})
	if err != nil {
		return fmt.Errorf("aborting multipart upload %s: %w", uploadID, err)
	}
	return nil
}

// PartURL signs a PUT for one part.
func (p *Presigner) PartURL(ctx context.Context, key, uploadID string, n int) (PresignedPart, error) {
	num := int32(n)
	req, err := p.Presign.PresignUploadPart(ctx, &s3.UploadPartInput{
		Bucket:     aws.String(p.Bucket),
		Key:        aws.String(key),
		UploadId:   aws.String(uploadID),
		PartNumber: &num,
	}, s3.WithPresignExpires(p.TTL))
	if err != nil {
		return PresignedPart{}, fmt.Errorf("presigning part %d: %w", n, err)
	}
	return PresignedPart{PartNumber: n, URL: req.URL, ExpiresAt: time.Now().Add(p.TTL)}, nil
}

// PartURLs signs a PUT for each of the given part numbers.
func (p *Presigner) PartURLs(ctx context.Context, key, uploadID string, numbers []int) ([]PresignedPart, error) {
	out := make([]PresignedPart, 0, len(numbers))
	for _, n := range numbers {
		part, err := p.PartURL(ctx, key, uploadID, n)
		if err != nil {
			return nil, err
		}
		out = append(out, part)
	}
	return out, nil
}

// ------------------------------------------------------------------ download --

// DownloadContentType is what every download claims to be, whatever was
// uploaded. Objects are stored with no Content-Type at all, and the presigned
// GET overrides it to this unconditionally: uploaded HTML or SVG served back
// under its own MIME type from a store origin would be a stored-XSS channel,
// and "never echo the client's MIME" is the rule that closes it.
const DownloadContentType = "application/octet-stream"

// PresignedGet is one download URL and the moment it stops working.
type PresignedGet struct {
	URL       string
	ExpiresAt time.Time
}

// GetURL signs a GET for an object, forcing the response's disposition and
// content type through the response-content-* query overrides.
//
// The overrides are honoured by R2 on both the 200 and the 206 Range response,
// and by Garage on the 200 only (both measured 2026-08-17). Garage's 206 is
// still safe -- the object carries no stored Content-Type for it to fall back
// to -- so the posture holds on both stores.
//
// The TTL is the presigner's (DRIVE_PRESIGN_TTL, an hour). Anything much
// shorter breaks download managers: a resumed transfer re-requests ranges
// against this URL directly, long after the redirect that produced it.
func (p *Presigner) GetURL(ctx context.Context, key, fileName string) (PresignedGet, error) {
	req, err := p.Presign.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket:                     aws.String(p.Bucket),
		Key:                        aws.String(key),
		ResponseContentDisposition: aws.String(AttachmentDisposition(fileName)),
		ResponseContentType:        aws.String(DownloadContentType),
	}, s3.WithPresignExpires(p.TTL))
	if err != nil {
		return PresignedGet{}, fmt.Errorf("presigning a download of %s: %w", key, err)
	}
	return PresignedGet{URL: req.URL, ExpiresAt: time.Now().Add(p.TTL)}, nil
}

// AttachmentDisposition builds the Content-Disposition value a download is
// served with: always an attachment, never inline.
//
// Both parameter forms are emitted, as RFC 6266 recommends. filename= carries
// an ASCII-only fallback for anything that cannot read the extended form, and
// filename*= carries the real name RFC 5987 style -- UTF-8, percent-encoded --
// so emoji, CJK and RTL names arrive intact. The fallback also drops quotes and
// backslashes, which are the only two characters that could otherwise end the
// quoted string early; names never contain CR/LF, because node.Clean strips
// every C0 control before a name is ever stored.
func AttachmentDisposition(fileName string) string {
	return `attachment; filename="` + asciiFallbackName(fileName) +
		`"; filename*=UTF-8''` + percentEncodeName(fileName)
}

// asciiFallbackName reduces a name to printable ASCII for the plain filename=
// parameter. Everything else becomes an underscore rather than disappearing, so
// the fallback keeps the name's shape and its extension.
func asciiFallbackName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('_')
		case r >= 0x20 && r < 0x7F:
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "download"
	}
	return b.String()
}

// percentEncodeName encodes a name into RFC 5987's value-chars: attr-char stays
// literal, every other byte of the UTF-8 encoding becomes %XX. Note that '%',
// '\'' and '*' are deliberately not attr-char and so are always escaped.
func percentEncodeName(name string) string {
	const attrChar = "!#$&+-.^_`|~"
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			strings.IndexByte(attrChar, c) >= 0:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// PartLister is the slice of the S3 client ListAllParts needs. It is an
// interface so the pagination loop -- the part of reconciliation that only
// misbehaves past 1,000 parts, where a real fixture would be an 11 GiB
// upload -- is unit-testable against a fake.
type PartLister interface {
	ListParts(context.Context, *s3.ListPartsInput, ...func(*s3.Options)) (*s3.ListPartsOutput, error)
}

// ListAllParts returns every part Garage holds for a multipart upload,
// paginating to exhaustion. ETags come back normalized, ready to compare
// against the ledger.
func ListAllParts(ctx context.Context, l PartLister, bucket, key, uploadID string) ([]Part, error) {
	var (
		parts  []Part
		marker *string
	)
	for {
		out, err := l.ListParts(ctx, &s3.ListPartsInput{
			Bucket:           aws.String(bucket),
			Key:              aws.String(key),
			UploadId:         aws.String(uploadID),
			MaxParts:         aws.Int32(listPartsPage),
			PartNumberMarker: marker,
		})
		if err != nil {
			return nil, fmt.Errorf("listing parts of %s: %w", uploadID, err)
		}
		for _, p := range out.Parts {
			parts = append(parts, Part{
				Number: int(aws.ToInt32(p.PartNumber)),
				Size:   aws.ToInt64(p.Size),
				ETag:   NormalizeETag(aws.ToString(p.ETag)),
			})
		}
		if !aws.ToBool(out.IsTruncated) {
			return parts, nil
		}
		// A truncated page with no next marker would spin forever; treat it as
		// the end rather than hang a request holding a database transaction.
		if aws.ToString(out.NextPartNumberMarker) == "" {
			return parts, nil
		}
		marker = out.NextPartNumberMarker
	}
}
