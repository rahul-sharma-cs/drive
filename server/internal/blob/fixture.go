package blob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
)

// Fixture is what a blobs row needs after PutFixture writes one real object.
type Fixture struct {
	ObjectKey string
	Size      int64
	SHA256    []byte
	ETag      string
}

// PutFixture writes data as one object via a direct S3 PutObject and returns
// what a blobs row needs. It is the one shared path seed and every test
// fixture use to get real bytes into Garage: Phases 1 and 3 need objects to
// exist before Phase 2's upload session protocol does, and every later
// fixture goes through this same function rather than re-implementing the
// write.
//
// The key format ("blobs/{uuid}") matches the real upload path's object keys.
// No Content-Type is set, matching the upload path for the same reason: Garage
// only applies response-content-* overrides to full 200 GETs, never to Range
// (206) responses, so an object stored with a renderable Content-Type could
// serve as one on a seek. Uploaded HTML/SVG staying inert rests entirely on
// that, so fixtures must not create an exception to it.
func PutFixture(ctx context.Context, c *s3.Client, bucket string, data []byte) (Fixture, error) {
	key := "blobs/" + uuid.NewString()
	sum := sha256.Sum256(data)

	out, err := c.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(data),
	})
	if err != nil {
		return Fixture{}, fmt.Errorf("blob: put fixture %s: %w", key, err)
	}

	return Fixture{
		ObjectKey: key,
		Size:      int64(len(data)),
		SHA256:    sum[:],
		ETag:      normalizeETag(aws.ToString(out.ETag)),
	}, nil
}

// normalizeETag strips the surrounding quotes and any weak-validator "W/"
// prefix S3-compatible stores wrap an ETag in, and lowercases it -- the same
// normalization every ETag comparison in the upload protocol applies, so a
// blobs.etag column is always stored in comparable form. An unnormalized
// compare always fails and would falsely downgrade client integrity.
func normalizeETag(etag string) string {
	e := strings.TrimPrefix(etag, "W/")
	e = strings.Trim(e, `"`)
	return strings.ToLower(e)
}
