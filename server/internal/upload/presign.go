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
