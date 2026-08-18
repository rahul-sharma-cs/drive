package blob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/rahul-sharma-cs/drive/server/internal/config"
)

// TestNormalizeETag is a pure table test; TestPutFixture below is the real
// round trip against Garage.
func TestNormalizeETag(t *testing.T) {
	cases := map[string]string{
		`"abc123"`:    "abc123",
		`W/"abc123"`:  "abc123",
		`"ABC123"`:    "abc123",
		`abc123`:      "abc123",
		`W/"ABCdef1"`: "abcdef1",
	}
	for in, want := range cases {
		if got := normalizeETag(in); got != want {
			t.Errorf("normalizeETag(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestPutFixture writes a real object to the drive-test stack's Garage and
// reads it back: size, sha256 and body bytes must all match what PutFixture
// reported.
func TestPutFixture(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cfg := testConfig()
	client, _, err := New(ctx, cfg)
	if err != nil {
		t.Fatalf("blob.New: %v", err)
	}

	data := []byte(strings.Repeat("drive-fixture-test ", 500)) // ~9.5 KB
	want := sha256.Sum256(data)

	fx, err := PutFixture(ctx, client, cfg.S3Bucket, data)
	if err != nil {
		t.Fatalf("PutFixture: %v", err)
	}

	if fx.Size != int64(len(data)) {
		t.Errorf("Size = %d, want %d", fx.Size, len(data))
	}
	if !bytes.Equal(fx.SHA256, want[:]) {
		t.Errorf("SHA256 = %x, want %x", fx.SHA256, want)
	}
	if !strings.HasPrefix(fx.ObjectKey, "blobs/") {
		t.Errorf("ObjectKey = %q, want a blobs/ prefix", fx.ObjectKey)
	}
	if fx.ETag == "" || strings.ContainsAny(fx.ETag, `"`) {
		t.Errorf("ETag = %q, want a normalized (unquoted) value", fx.ETag)
	}

	out, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(cfg.S3Bucket),
		Key:    aws.String(fx.ObjectKey),
	})
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	defer out.Body.Close()
	got, err := io.ReadAll(out.Body)
	if err != nil {
		t.Fatalf("read object body: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("object body (%d bytes) does not match the fixture data (%d bytes)", len(got), len(data))
	}
	// The posture only requires the type not be renderable by a
	// browser -- Garage defaults an unset Content-Type to
	// "application/octet-stream" on a full GET (confirmed here) and to empty
	// on a Range/206 (confirmed in the day-0 spike report); either is fine,
	// text/html or image/* would not be.
	if ct := aws.ToString(out.ContentType); ct != "" && ct != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want empty or application/octet-stream -- a renderable stored type would let a Range GET serve uploaded HTML", ct)
	}
}

// testConfig points at the drive-test stack by default -- the same fallback
// convention as internal/mail's integration tests -- so this runs whether or
// not the caller sourced .env.test first.
func testConfig() *config.Config {
	return &config.Config{
		S3Endpoint:  envOr("DRIVE_S3_ENDPOINT", "http://localhost:3910"),
		S3Bucket:    envOr("DRIVE_S3_BUCKET", "drive-blobs"),
		S3AccessKey: envOr("DRIVE_S3_ACCESS_KEY", "drivetestkey0001"),
		S3SecretKey: envOr("DRIVE_S3_SECRET_KEY", "drivetestsecretkey0001"),
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
