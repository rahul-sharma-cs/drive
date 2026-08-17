package upload

// The presigned-download half of GET /files/{id}/download, against a real
// object store.
//
// This file is written to run unchanged against Garage (the drive-test stack,
// every `make test`) and against Cloudflare R2 (`.env.r2` sourced over
// `.env.test`, once per stage gate) -- which is the only way the disposition
// question gets a real answer, since the response-content-* overrides are
// documented on neither store. It needs no database: presigning is a local HMAC
// and the assertions are about what the store does with the signed query.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/rahul-sharma-cs/drive/server/internal/blob"
	"github.com/rahul-sharma-cs/drive/server/internal/config"
)

// dlName is deliberately hostile: an emoji outside the BMP, Arabic (RTL), a
// quote and a backslash that would end the quoted fallback early, a space, and
// an .html extension whose bytes must never be served as markup.
const dlName = `ملف "spicy"\🚀 report.html`

var (
	dlOnce      sync.Once
	dlPresigner *Presigner
	dlS3        *s3.Client
	dlInitErr   error
)

func dlEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// dlSetup builds a presigner from the environment. Unlike the other suites in
// this package it reads DRIVE_S3_REGION too: Garage signs with "garage" and R2
// with "auto", and a wrong region is a SignatureDoesNotMatch on every request.
func dlSetup(t *testing.T) (*Presigner, *s3.Client) {
	t.Helper()
	dlOnce.Do(func() {
		cfg := &config.Config{
			S3Endpoint:  dlEnv("DRIVE_S3_ENDPOINT", "http://localhost:3910"),
			S3Bucket:    dlEnv("DRIVE_S3_BUCKET", "drive-blobs"),
			S3AccessKey: dlEnv("DRIVE_S3_ACCESS_KEY", "drivetestkey0001"),
			S3SecretKey: dlEnv("DRIVE_S3_SECRET_KEY", "drivetestsecretkey0001"),
			S3Region:    dlEnv("DRIVE_S3_REGION", config.DefaultS3Region),
			PresignTTL:  time.Hour,
		}
		if strings.Contains(cfg.S3Endpoint, ":3900") {
			dlInitErr = fmt.Errorf("DRIVE_S3_ENDPOINT points at the dev stack (%s); tests run against drive-test on :3910", cfg.S3Endpoint)
			return
		}
		s3c, presign, err := blob.New(context.Background(), cfg)
		if err != nil {
			dlInitErr = err
			return
		}
		dlS3 = s3c
		dlPresigner = &Presigner{S3: s3c, Presign: presign, Bucket: cfg.S3Bucket, TTL: cfg.PresignTTL}
	})
	if dlInitErr != nil {
		t.Fatalf("object store: %v (is `docker compose -p drive-test --env-file .env.test up -d` running?)", dlInitErr)
	}
	return dlPresigner, dlS3
}

// putTestObject stores bytes the way the upload path does -- with no
// Content-Type at all -- and removes them when the test ends.
func putTestObject(t *testing.T, body []byte) string {
	t.Helper()
	p, s3c := dlSetup(t)
	key := NewObjectKey()

	if _, err := s3c.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(p.Bucket), Key: aws.String(key), Body: bytes.NewReader(body),
	}); err != nil {
		t.Fatalf("storing the test object: %v", err)
	}
	t.Cleanup(func() {
		_, _ = s3c.DeleteObject(context.Background(), &s3.DeleteObjectInput{
			Bucket: aws.String(p.Bucket), Key: aws.String(key),
		})
	})
	return key
}

func fetchDownload(t *testing.T, url string, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("building the download request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("fetching the presigned URL: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the download body: %v", err)
	}
	return resp, body
}

// The full 200 GET: the bytes come back intact, and the store applies both
// response-content-* overrides -- which is what makes an uploaded .html file a
// download rather than a page on the store's origin.
func TestPresignedDownloadForcesAttachmentAndOctetStream(t *testing.T) {
	p, _ := dlSetup(t)
	content := []byte(`<html><body><script>alert(document.domain)</script></body></html>`)
	key := putTestObject(t, content)

	signed, err := p.GetURL(context.Background(), key, dlName)
	if err != nil {
		t.Fatalf("presigning the download: %v", err)
	}
	if signed.ExpiresAt.Before(time.Now().Add(50 * time.Minute)) {
		t.Errorf("the URL expires at %s, sooner than the one-hour TTL a download manager needs", signed.ExpiresAt)
	}

	resp, body := fetchDownload(t, signed.URL, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200 (body %q)", resp.StatusCode, truncate(body))
	}
	if !bytes.Equal(body, content) {
		t.Error("the downloaded bytes are not the stored bytes")
	}
	if got, want := resp.Header.Get("Content-Disposition"), AttachmentDisposition(dlName); got != want {
		t.Errorf("Content-Disposition = %q, want %q", got, want)
	}
	if got := resp.Header.Get("Content-Type"); got != DownloadContentType {
		t.Errorf("Content-Type = %q, want %q -- uploaded markup must never come back renderable", got, DownloadContentType)
	}
}

// The 206 Range response, which is what a download manager's resume and a
// browser's media scrub actually issue.
//
// R2 applies the overrides here too; Garage skips them on 206 by design. The
// assertion is therefore about the property that must hold on both stores: a
// partial response can never arrive with a renderable content type or an inline
// disposition. It holds on Garage only because objects are stored with no
// Content-Type for it to fall back to -- so this case also guards that rule.
func TestPresignedDownloadRangeIsNeverRenderable(t *testing.T) {
	p, _ := dlSetup(t)
	content := []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)
	key := putTestObject(t, content)

	signed, err := p.GetURL(context.Background(), key, "drawing.svg")
	if err != nil {
		t.Fatalf("presigning the download: %v", err)
	}

	resp, body := fetchDownload(t, signed.URL, map[string]string{"Range": "bytes=0-15"})
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status %d, want 206 (body %q)", resp.StatusCode, truncate(body))
	}
	if !bytes.Equal(body, content[:16]) {
		t.Errorf("the range response returned %q, want the first 16 bytes", truncate(body))
	}

	ct := resp.Header.Get("Content-Type")
	if ct != "" && ct != DownloadContentType {
		t.Errorf("Content-Type on the 206 is %q: a partial response must be empty or %q, never renderable",
			ct, DownloadContentType)
	}
	if cd := resp.Header.Get("Content-Disposition"); strings.Contains(strings.ToLower(cd), "inline") {
		t.Errorf("Content-Disposition on the 206 is %q, which would render in the tab", cd)
	}
	t.Logf("206 overrides: content_type=%q content_disposition=%q", ct, resp.Header.Get("Content-Disposition"))
}

// ------------------------------------------------------------------- header --

// The header value itself, which no store round trip can check: RFC 6266 wants
// both parameter forms, and the extended one is the only reason a name outside
// ASCII survives at all.
func TestAttachmentDispositionCarriesBothFilenameForms(t *testing.T) {
	cases := []struct {
		name string
		file string
		want string
	}{
		{
			"plain ascii is unchanged in both forms",
			"report.pdf",
			`attachment; filename="report.pdf"; filename*=UTF-8''report.pdf`,
		},
		{
			"a space is literal in the fallback and percent-encoded in the extended form",
			"my report.pdf",
			`attachment; filename="my report.pdf"; filename*=UTF-8''my%20report.pdf`,
		},
		{
			"a quote and a backslash cannot end the quoted fallback early",
			`a"b\c.txt`,
			`attachment; filename="a_b_c.txt"; filename*=UTF-8''a%22b%5Cc.txt`,
		},
		{
			"an emoji outside the BMP survives as its four UTF-8 bytes",
			"🚀.bin",
			`attachment; filename="_.bin"; filename*=UTF-8''%F0%9F%9A%80.bin`,
		},
		{
			"an RTL name survives",
			"مرحبا.txt",
			`attachment; filename="_____.txt"; filename*=UTF-8''%D9%85%D8%B1%D8%AD%D8%A8%D8%A7.txt`,
		},
		{
			"a name with nothing ASCII in it still has a usable fallback",
			"日本語",
			`attachment; filename="___"; filename*=UTF-8''%E6%97%A5%E6%9C%AC%E8%AA%9E`,
		},
		{
			"the reserved percent, quote and star are always escaped in the extended form",
			"100%*'.txt",
			`attachment; filename="100%*'.txt"; filename*=UTF-8''100%25%2A%27.txt`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := AttachmentDisposition(c.file); got != c.want {
				t.Errorf("AttachmentDisposition(%q)\n = %q\nwant %q", c.file, got, c.want)
			}
		})
	}
}

// An empty name would otherwise produce filename="" , which some clients treat
// as "no name" and others as a parse error.
func TestAttachmentDispositionNamesAnEmptyFile(t *testing.T) {
	if got, want := AttachmentDisposition(""), `attachment; filename="download"; filename*=UTF-8''`; got != want {
		t.Errorf("AttachmentDisposition(\"\") = %q, want %q", got, want)
	}
}

func truncate(b []byte) string {
	if len(b) > 200 {
		return string(b[:200]) + "..."
	}
	return string(b)
}
