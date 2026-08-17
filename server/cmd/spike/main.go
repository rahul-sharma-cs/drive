// Command spike is the day-0 spike (PLAN §Testing 1).
//
// It answers one question before any protocol code is written: can a browser
// PUT presigned multipart parts straight into the object store, read the ETag
// back, and can the server then reconcile and finalize that upload? A green run
// is the "direct" verdict; a red run after the full triage order is what forces
// relay mode.
//
// It runs against whichever store the -env-file points at — Garage locally,
// Cloudflare R2 with the deployment env file. Where the two answer differently
// (Range-GET disposition overrides, expired-presign surface, upload-id
// stability) the report records both rather than assuming either.
//
// It lives inside the server module on purpose: Go's internal-package rule
// means a top-level e2e harness could never import internal/blob, and the whole
// point is to exercise the REAL presigner configuration rather than a copy.
//
// Run it with `make spike`.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/rahul-sharma-cs/drive/server/internal/blob"
	"github.com/rahul-sharma-cs/drive/server/internal/config"
	"github.com/rahul-sharma-cs/drive/server/internal/upload"
)

const (
	part1Size = 10 << 20 // 10 MiB — a non-final part: >= S3's 5 MiB floor and a multiple of Garage's block size
	part2Size = 1 << 20  // 1 MiB — the final part, deliberately smaller
	dropFiles = 120      // > 100 so the drop probe exercises Chromium's readEntries batching cap

	// The two dispositions must differ: a GET response tells which of stored
	// metadata (g) and the presigned override (c/d) the store honoured.
	storedDisp   = `attachment; filename*=UTF-8''stored.bin`
	overrideDisp = `attachment; filename*=UTF-8''spike.bin`
	overrideType = "application/octet-stream"
)

// check records one spike assertion.
type check struct {
	Name   string `json:"name"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail"`
}

type report struct {
	StartedAt     time.Time       `json:"started_at"`
	S3Endpoint    string          `json:"s3_endpoint"`
	Bucket        string          `json:"bucket"`
	ObjectKey     string          `json:"object_key"`
	Checks        []check         `json:"checks"`
	Browser       json.RawMessage `json:"browser,omitempty"`
	ExpiredGET    map[string]any  `json:"expired_presigned_get"`
	ExpiredPUT    map[string]any  `json:"expired_presigned_put"`
	RangeGET      map[string]any  `json:"range_get"`
	FullGET       map[string]any  `json:"full_get"`
	Downloads     map[string]any  `json:"download_matrix"`
	CompleteTag   map[string]any  `json:"complete_etag"`
	MultipartList map[string]any  `json:"multipart_listing"`
	Verdict       string          `json:"verdict"`
	FailedNames   []string        `json:"failed_checks"`
}

func (r *report) add(name string, pass bool, format string, args ...any) bool {
	detail := fmt.Sprintf(format, args...)
	r.Checks = append(r.Checks, check{Name: name, Pass: pass, Detail: detail})
	status := "FAIL"
	if pass {
		status = "PASS"
	}
	fmt.Printf("%-4s %-46s %s\n", status, name, detail)
	if !pass {
		r.FailedNames = append(r.FailedNames, name)
	}
	return pass
}

type manifestPart struct {
	PartNumber  int32  `json:"part_number"`
	URL         string `json:"url"`
	File        string `json:"file"`
	Size        int    `json:"size"`
	MD5         string `json:"md5"`
	ContentType string `json:"content_type"`
}

type manifest struct {
	S3Endpoint       string         `json:"s3_endpoint"`
	DropDir          string         `json:"drop_dir"`
	DropDirFileCount int            `json:"drop_dir_file_count"`
	Parts            []manifestPart `json:"parts"`
	// Assertion (i): a part URL that is already expired by the time the page
	// loads. The browser's surface for it (readable status vs opaque
	// status-0/onerror) decides which engine path an expired presign takes.
	ExpiredPutURL string `json:"expired_put_url"`
	ExpiredPutKey string `json:"expired_put_key"`
}

func main() {
	var (
		repo     = flag.String("repo", ".", "repo root")
		envFile  = flag.String("env-file", ".env", "env file to load before reading config")
		pagePort = flag.Int("page-port", 5173, "port to serve the spike page on (must match a bucket CORS origin)")
		skipPW   = flag.Bool("skip-browser", false, "skip the Playwright half (server-side assertions only)")
	)
	flag.Parse()

	if err := run(*repo, *envFile, *pagePort, *skipPW); err != nil {
		fmt.Fprintf(os.Stderr, "\nspike failed: %v\n", err)
		os.Exit(1)
	}
}

func run(repo, envFile string, pagePort int, skipPW bool) error {
	ctx := context.Background()

	if err := loadEnvFile(filepath.Join(repo, envFile)); err != nil {
		return fmt.Errorf("load %s: %w", envFile, err)
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	client, presigner, err := blob.New(ctx, cfg)
	if err != nil {
		return err
	}

	publicDir := filepath.Join(repo, "server", "cmd", "spike", "public")
	outDir := filepath.Join(repo, "e2e", "fixtures", "spike")
	for _, d := range []string{publicDir, outDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}

	rep := &report{StartedAt: time.Now(), S3Endpoint: cfg.S3Endpoint, Bucket: cfg.S3Bucket}
	defer func() {
		rep.Verdict = "direct"
		if len(rep.FailedNames) > 0 {
			rep.Verdict = "FAILED — run PLAN's triage order before declaring relay"
		}
		b, _ := json.MarshalIndent(rep, "", "  ")
		_ = os.WriteFile(filepath.Join(outDir, "spike-report.json"), b, 0o644)
	}()

	// --- 1. reachability -----------------------------------------------------
	if _, err := client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &cfg.S3Bucket}); err != nil {
		rep.add("bucket reachable", false, "%v", err)
		return fmt.Errorf("bucket %q unreachable — run `make infra-init` first: %w", cfg.S3Bucket, err)
	}
	rep.add("bucket reachable", true, "%s/%s", cfg.S3Endpoint, cfg.S3Bucket)

	corsOK, corsDetail := checkCORS(ctx, client, cfg.S3Bucket)
	rep.add("bucket CORS: one origin per rule", corsOK, "%s", corsDetail)

	// --- 2. fixtures ---------------------------------------------------------
	parts := []manifestPart{
		{PartNumber: 1, File: "part-1.bin", Size: part1Size, ContentType: ""},
		// The final part carries an explicit Content-Type: that forces a CORS
		// preflight, which passes only if the bucket rule allows that header.
		// Enumerating it ("content-type") is enough — measured against R2 on
		// 2026-08-17; the wildcard this comment used to demand was Garage-era.
		{PartNumber: 2, File: "part-2.bin", Size: part2Size, ContentType: "application/octet-stream"},
	}
	var whole bytes.Buffer
	for i := range parts {
		buf := make([]byte, parts[i].Size)
		if _, err := rand.Read(buf); err != nil {
			return err
		}
		sum := md5.Sum(buf)
		parts[i].MD5 = hex.EncodeToString(sum[:])
		if err := os.WriteFile(filepath.Join(publicDir, parts[i].File), buf, 0o644); err != nil {
			return err
		}
		whole.Write(buf)
	}
	totalSize := int64(part1Size + part2Size)

	dropDir := filepath.Join(outDir, "dropdir")
	dropCount, err := makeDropTree(dropDir)
	if err != nil {
		return err
	}

	// --- 3. create the multipart upload --------------------------------------
	key := "spike/" + randomHex(16)
	rep.ObjectKey = key
	// No ContentType and no ChecksumAlgorithm, ever: objects stored without a
	// Content-Type are what makes the Range-GET posture in PLAN §Sharing hold.
	//
	// Content-Disposition IS stored, deliberately: it is the fallback the
	// download endpoint needs if the store ignores response-content-disposition
	// on presigned GETs (assertion g). It is set to a value that differs from
	// the override tried later, so a response says which of the two won.
	create, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: &cfg.S3Bucket, Key: &key, ContentDisposition: aws.String(storedDisp),
	})
	if err != nil {
		rep.add("CreateMultipartUpload", false, "%v", err)
		return err
	}
	uploadID := aws.ToString(create.UploadId)
	rep.add("CreateMultipartUpload", true, "upload_id=%s key=%s", uploadID, key)
	defer func() {
		_, _ = client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
			Bucket: &cfg.S3Bucket, Key: &key, UploadId: &uploadID,
		})
		_, _ = client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &cfg.S3Bucket, Key: &key})
	}()

	// --- 4. presign the part URLs --------------------------------------------
	for i := range parts {
		n := parts[i].PartNumber
		// UploadPartInput has no ContentType field and PresignUploadPart strips
		// Content-Type from signing — the browser may send any Content-Type.
		req, err := presigner.PresignUploadPart(ctx, &s3.UploadPartInput{
			Bucket: &cfg.S3Bucket, Key: &key, UploadId: &uploadID, PartNumber: &n,
		}, s3.WithPresignExpires(15*time.Minute))
		if err != nil {
			rep.add("PresignUploadPart", false, "part %d: %v", n, err)
			return err
		}
		parts[i].URL = req.URL
	}
	leaked := ""
	for _, p := range parts {
		for _, bad := range []string{"x-amz-checksum", "x-amz-sdk-checksum"} {
			if strings.Contains(strings.ToLower(p.URL), bad) {
				leaked += fmt.Sprintf("part %d has %s; ", p.PartNumber, bad)
			}
		}
	}
	rep.add("presigned URLs carry no checksum params", leaked == "",
		"%s", orElse(leaked, "clean (RequestChecksumCalculation=WhenRequired holds)"))

	// An already-expired part URL, on a throwaway multipart of its own so a
	// surprise success cannot pollute the real upload's part list. One second of
	// TTL is spent long before Playwright finishes booting. Both the browser (i)
	// and the Go client (b) hit this same URL.
	expiredKey := "spike/" + randomHex(16) + "-expired"
	expiredCreate, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: &cfg.S3Bucket, Key: &expiredKey,
	})
	if err != nil {
		return err
	}
	defer func() {
		_, _ = client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
			Bucket: &cfg.S3Bucket, Key: &expiredKey, UploadId: expiredCreate.UploadId,
		})
	}()
	one := int32(1)
	expiredPut, err := presigner.PresignUploadPart(ctx, &s3.UploadPartInput{
		Bucket: &cfg.S3Bucket, Key: &expiredKey, UploadId: expiredCreate.UploadId, PartNumber: &one,
	}, s3.WithPresignExpires(time.Second))
	if err != nil {
		return err
	}

	man := manifest{
		S3Endpoint: cfg.S3Endpoint, DropDir: dropDir, DropDirFileCount: dropCount, Parts: parts,
		ExpiredPutURL: expiredPut.URL, ExpiredPutKey: expiredKey,
	}
	manPath := filepath.Join(publicDir, "manifest.json")
	mb, _ := json.MarshalIndent(man, "", "  ")
	if err := os.WriteFile(manPath, mb, 0o644); err != nil {
		return err
	}

	// --- 5. browser half -----------------------------------------------------
	resultsPath := filepath.Join(outDir, "browser-results.json")
	if skipPW {
		rep.add("browser presigned PUTs", false, "skipped (-skip-browser)")
	} else {
		srv, err := servePublic(publicDir, pagePort)
		if err != nil {
			return err
		}
		defer srv.Close()

		if err := runPlaywright(repo, manPath, resultsPath, pagePort); err != nil {
			rep.add("browser presigned PUTs", false, "%v", err)
			return fmt.Errorf("browser half failed — PLAN triage order: (1) SDK checksum config (2) region+path-style (3) CORS AllowedHeaders (4) clock skew: %w", err)
		}
		raw, err := os.ReadFile(resultsPath)
		if err != nil {
			return err
		}
		rep.Browser = raw
		rep.add("browser presigned PUTs", true, "2 parts PUT from http://localhost:%d, ETag readable, normalized ETag == client MD5", pagePort)

		// (i) The expired-presign surface a browser actually sees. Recorded, not
		// asserted: a readable 403 and an opaque status-0/onerror both route to
		// the engine's network path (parts.ts classifies 0 as `network`); which
		// one the store produces is what this spike exists to write down.
		var br struct {
			Put struct {
				ExpiredPut map[string]any `json:"expired_put"`
			} `json:"put"`
		}
		if err := json.Unmarshal(raw, &br); err != nil {
			return fmt.Errorf("browser results: %w", err)
		}
		if br.Put.ExpiredPut == nil {
			rep.add("browser expired-presign surface recorded", false, "the page reported no expired_put result")
		} else {
			rep.add("browser expired-presign surface recorded", true,
				"status=%v onerror=%v (both route to the engine's network path)",
				br.Put.ExpiredPut["status"], br.Put.ExpiredPut["error"] != nil)
		}
	}

	// --- 5b. expired presigned PUT from the Go client, ± Expect: 100-continue -
	// Phase 0 measured Garage answering an expired presign before draining the
	// body and resetting the socket, which surfaces as a transport error rather
	// than a response; `Expect: 100-continue` is what made it deterministic
	// (uploadclient and testutil both send it now). This records whether the
	// same holds here.
	expiredBody := make([]byte, 1<<20)
	if _, err := rand.Read(expiredBody); err != nil {
		return err
	}
	rep.ExpiredPUT = map[string]any{}
	expiredRefused := true
	expectReadable := false
	for _, v := range []struct {
		label  string
		expect bool
	}{{"without_expect_100_continue", false}, {"with_expect_100_continue", true}} {
		req, err := http.NewRequest(http.MethodPut, expiredPut.URL, bytes.NewReader(expiredBody))
		if err != nil {
			return err
		}
		req.ContentLength = int64(len(expiredBody))
		if v.expect {
			req.Header.Set("Expect", "100-continue")
		}
		obs := map[string]any{}
		resp, err := http.DefaultClient.Do(req)
		switch {
		case err != nil:
			// A reset socket is still a refusal — but an unreadable one: the
			// client sees a transport error and cannot tell expiry from a dead
			// network, which is the bug the Expect header exists to avoid.
			obs["transport_error"] = err.Error()
		default:
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
			resp.Body.Close()
			obs["status"] = resp.StatusCode
			obs["code"] = xmlTag(string(body), "Code")
			obs["body"] = string(body)
			if resp.StatusCode < 400 {
				expiredRefused = false
			}
			if v.expect {
				expectReadable = true
			}
		}
		rep.ExpiredPUT[v.label] = obs
	}
	rep.add("expired presigned PUT never succeeds", expiredRefused,
		"without Expect: %v | with Expect: %v", rep.ExpiredPUT["without_expect_100_continue"], rep.ExpiredPUT["with_expect_100_continue"])
	// uploadclient reads the response code to tell an expired URL from a network
	// failure; without a readable response an expiry burns the integrity budget.
	rep.add("expired presigned PUT is readable with Expect: 100-continue", expectReadable,
		"%v", rep.ExpiredPUT["with_expect_100_continue"])

	// --- 6. server-side reconciliation ---------------------------------------
	listed, err := listAllParts(ctx, client, cfg.S3Bucket, key, uploadID)
	if err != nil {
		rep.add("ListParts", false, "%v", err)
		return err
	}
	if skipPW {
		// Nothing was uploaded; the remaining flow needs real parts.
		return errors.New("-skip-browser leaves no parts to finalize; rerun without it")
	}
	partsOK := len(listed) == len(parts)
	detail := fmt.Sprintf("%d parts", len(listed))
	for _, lp := range listed {
		want := parts[aws.ToInt32(lp.PartNumber)-1].MD5
		if normalizeETag(aws.ToString(lp.ETag)) != want {
			partsOK = false
			detail += fmt.Sprintf("; part %d ETag %q != md5 %s", aws.ToInt32(lp.PartNumber), aws.ToString(lp.ETag), want)
		}
	}
	rep.add("ListParts matches the client ledger", partsOK, "%s", detail)

	// GC's orphan sweep reads this listing and asks "does a live session own
	// this?". What it can key that question on is exactly what this measures:
	// the object key is always safe, the upload id is not.
	found, idMatch, initiated, err := findMultipart(ctx, client, cfg.S3Bucket, key, uploadID)
	if err != nil {
		rep.add("ListMultipartUploads (GC dependency)", false, "%v", err)
		return err
	}
	rep.MultipartList = map[string]any{
		"found_by_key": found, "listed_id_equals_created_id": idMatch, "initiated": initiated,
	}
	idNote := ""
	if !idMatch {
		idNote = " — the store re-mints upload ids per response, so GC must claim by object_key alone; gc.go's multipartClaimed also matches s3_upload_id, which here would abort live uploads"
	}
	rep.add("ListMultipartUploads finds the upload by object key", found,
		"initiated=%s; listed upload_id == created upload_id: %v%s", initiated, idMatch, idNote)

	// --- 7. complete ---------------------------------------------------------
	completed := make([]s3types.CompletedPart, 0, len(listed))
	for _, lp := range listed {
		completed = append(completed, s3types.CompletedPart{ETag: lp.ETag, PartNumber: lp.PartNumber})
	}
	if _, err := client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: &cfg.S3Bucket, Key: &key, UploadId: &uploadID,
		MultipartUpload: &s3types.CompletedMultipartUpload{Parts: completed},
	}); err != nil {
		rep.add("CompleteMultipartUpload", false, "%v", err)
		return err
	}
	rep.add("CompleteMultipartUpload", true, "%d parts", len(completed))

	head, err := client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &cfg.S3Bucket, Key: &key})
	if err != nil {
		rep.add("HeadObject after complete", false, "%v", err)
		return err
	}
	rep.add("HeadObject size == declared", aws.ToInt64(head.ContentLength) == totalSize,
		"got %d want %d, stored Content-Type=%q", aws.ToInt64(head.ContentLength), totalSize, aws.ToString(head.ContentType))

	// (e) An object created with no Content-Type must not come back renderable:
	// that, not the response-content-type override, is what keeps uploaded HTML
	// inert when it is served from the store's own origin (PLAN §Sharing).
	rep.add("stored Content-Type is not renderable", !renderable(aws.ToString(head.ContentType)),
		"stored Content-Type=%q", aws.ToString(head.ContentType))

	// (f) The finalizer stores whatever HeadObject reports, normalized, in
	// blobs.etag — a multipart ETag is "<md5-of-md5s>-<partcount>", not a plain
	// MD5, and nothing downstream may assume 32 hex characters.
	headETag := normalizeETag(aws.ToString(head.ETag))
	rep.CompleteTag = map[string]any{
		"head_etag_raw": aws.ToString(head.ETag), "head_etag_normalized": headETag,
		"parts": len(completed),
	}
	rep.add("multipart ETag survives normalization", multipartETag.MatchString(headETag),
		"normalized=%q (want <32 hex>[-<n>])", headETag)

	// A retried complete must look like NoSuchUpload — the crash-after-complete
	// window the finalizer has to recognise (PLAN §Complete).
	_, reErr := client.ListParts(ctx, &s3.ListPartsInput{Bucket: &cfg.S3Bucket, Key: &key, UploadId: &uploadID})
	rep.add("ListParts after complete => NoSuchUpload", reErr != nil && strings.Contains(reErr.Error(), "NoSuchUpload"),
		"%v", reErr)

	// --- 8. downloads: the attachment matrix (c, d, g) ------------------------
	// Four fetches over two presigned GETs of the SAME object: one carrying the
	// response-content-* overrides, one plain. The object was stored with a
	// different Content-Disposition, so each response says which layer won.
	// Neither layer is documented on R2 either way; this is the empirical answer
	// the download endpoint's design depends on.
	overrideReq, err := presigner.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: &cfg.S3Bucket, Key: &key,
		ResponseContentDisposition: aws.String(overrideDisp),
		ResponseContentType:        aws.String(overrideType),
	}, s3.WithPresignExpires(cfg.PresignTTL))
	if err != nil {
		return err
	}
	plainReq, err := presigner.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: &cfg.S3Bucket, Key: &key,
	}, s3.WithPresignExpires(cfg.PresignTTL))
	if err != nil {
		return err
	}

	type getObs struct {
		status int
		ct     string
		cd     string
		body   []byte
	}
	obsOf := func(label, url string, headers map[string]string) (getObs, error) {
		resp, body, err := fetch(url, headers)
		if err != nil {
			rep.add("presigned GET ("+label+")", false, "%v", err)
			return getObs{}, err
		}
		o := getObs{resp.StatusCode, resp.Header.Get("Content-Type"), resp.Header.Get("Content-Disposition"), body}
		rep.Downloads[label] = map[string]any{
			"status": o.status, "content_type": o.ct, "content_disposition": o.cd,
			"content_range": resp.Header.Get("Content-Range"), "bytes": len(body),
		}
		return o, nil
	}
	rep.Downloads = map[string]any{}
	rangeHdr := map[string]string{"Range": "bytes=0-1023"}
	override200, err := obsOf("override_200", overrideReq.URL, nil)
	if err != nil {
		return err
	}
	override206, err := obsOf("override_206", overrideReq.URL, rangeHdr)
	if err != nil {
		return err
	}
	plain200, err := obsOf("plain_200", plainReq.URL, nil)
	if err != nil {
		return err
	}
	plain206, err := obsOf("plain_206", plainReq.URL, rangeHdr)
	if err != nil {
		return err
	}
	rep.FullGET = rep.Downloads["override_200"].(map[string]any)
	rep.RangeGET = rep.Downloads["override_206"].(map[string]any)

	gotHash := md5.Sum(override200.body)
	wantHash := md5.Sum(whole.Bytes())
	rep.add("presigned GET returns byte-identical object",
		override200.status == 200 && gotHash == wantHash,
		"status=%d bytes=%d md5_match=%v", override200.status, len(override200.body), gotHash == wantHash)
	rep.add("Range GET returns 206", override206.status == 206,
		"status=%d content_range=%v", override206.status, rep.Downloads["override_206"].(map[string]any)["content_range"])

	// The load-bearing posture check from PLAN §Sharing: Garage skips the
	// response-content-* overrides on 206, so safety rests on the object itself
	// never carrying a renderable Content-Type. It must hold on both GETs.
	rep.add("Range GET carries no renderable Content-Type",
		!renderable(override206.ct) && !renderable(plain206.ct),
		"override=%q plain=%q", override206.ct, plain206.ct)

	// The derived verdict the download endpoint is designed against.
	overridesWork := override200.cd == overrideDisp && override200.ct == overrideType
	metadataWorks := plain200.cd == storedDisp && plain206.cd == storedDisp
	strategy := "NONE — stop and decide (never proxy bytes through the server)"
	switch {
	case overridesWork:
		strategy = "overrides — presign with response-content-disposition/-type"
	case metadataWorks:
		strategy = "metadata — store Content-Disposition on the object at upload"
	}
	rep.add("attachment strategy determined", overridesWork || metadataWorks,
		"%s | overrides_on_200=%v overrides_on_206=%v stored_cd_on_200=%v stored_cd_on_206=%v",
		strategy, overridesWork, override206.cd == overrideDisp,
		plain200.cd == storedDisp, plain206.cd == storedDisp)

	// --- 9. expired presigned GET (fixture for the engine's vitest) ----------
	shortReq, err := presigner.PresignGetObject(ctx, &s3.GetObjectInput{Bucket: &cfg.S3Bucket, Key: &key},
		s3.WithPresignExpires(2*time.Second))
	if err != nil {
		return err
	}
	time.Sleep(3 * time.Second)
	exp, expBody, err := fetch(shortReq.URL, nil)
	if err != nil {
		rep.add("expired presigned GET", false, "%v", err)
		return err
	}
	rep.ExpiredGET = map[string]any{
		"status": exp.StatusCode, "content_type": exp.Header.Get("Content-Type"),
		"body": string(expBody), "code": xmlTag(string(expBody), "Code"),
	}
	rep.add("expired presigned GET is refused", exp.StatusCode >= 400,
		"status=%d code=%s (recorded for the engine's error fixtures)", exp.StatusCode, xmlTag(string(expBody), "Code"))

	// --- 10. zero-byte path --------------------------------------------------
	zeroKey := "spike/" + randomHex(16) + "-zero"
	defer func() { _, _ = client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &cfg.S3Bucket, Key: &zeroKey}) }()
	if _, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: &cfg.S3Bucket, Key: &zeroKey, Body: bytes.NewReader(nil),
	}); err != nil {
		rep.add("0-byte PutObject", false, "%v", err)
		return err
	}
	zh, err := client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &cfg.S3Bucket, Key: &zeroKey})
	rep.add("0-byte PutObject + HEAD", err == nil && aws.ToInt64(zh.ContentLength) == 0, "size=%d err=%v", aws.ToInt64(zh.ContentLength), err)

	// Prove the 0-byte special case is a requirement, not a preference: an empty
	// CompleteMultipartUpload part list must be rejected.
	emptyKey := "spike/" + randomHex(16) + "-empty-mpu"
	emptyCreate, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: &cfg.S3Bucket, Key: &emptyKey})
	if err != nil {
		return err
	}
	_, emptyErr := client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: &cfg.S3Bucket, Key: &emptyKey, UploadId: emptyCreate.UploadId,
		MultipartUpload: &s3types.CompletedMultipartUpload{Parts: []s3types.CompletedPart{}},
	})
	rep.add("empty CompleteMultipartUpload is rejected", emptyErr != nil, "%v", emptyErr)
	_, _ = client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket: &cfg.S3Bucket, Key: &emptyKey, UploadId: emptyCreate.UploadId,
	})

	// --- 11. uniform part sizes (h) ------------------------------------------
	// R2 rejects a multipart whose parts are not all the same size except the
	// last. Drive slices at the session's part_size, so the only way to violate
	// that is for ResolvePartSize to hand back a size the ledger cannot honour:
	// below S3's floor, off Garage's block grain, or one that needs more parts
	// than the ceiling allows. This is a pure function — no store involved.
	ok, detail := checkPartSizes(cfg.PartSize)
	rep.add("ResolvePartSize yields uniform, in-range parts", ok, "%s", detail)

	// --- verdict -------------------------------------------------------------
	fmt.Println()
	if len(rep.FailedNames) > 0 {
		return fmt.Errorf("%d checks failed: %s", len(rep.FailedNames), strings.Join(rep.FailedNames, ", "))
	}
	fmt.Printf("SPIKE GREEN — direct browser->store uploads are viable against %s; relay fallback NOT required.\n", cfg.S3Endpoint)
	fmt.Printf("report: %s\n", filepath.Join(outDir, "spike-report.json"))
	return nil
}

// ---------------------------------------------------------------------------

func checkCORS(ctx context.Context, client *s3.Client, bucket string) (bool, string) {
	out, err := client.GetBucketCors(ctx, &s3.GetBucketCorsInput{Bucket: &bucket})
	if err != nil {
		// R2 refuses bucket-level reads to an Object-Read-&-Write token. That is
		// a permission boundary, not a CORS defect: the browser half's CDP
		// assertion on Access-Control-Allow-Origin is the authoritative check
		// either way, so record and continue. Any other error is still a failure.
		if strings.Contains(err.Error(), "AccessDenied") {
			return true, "not readable with this token (AccessDenied) — browser-side Access-Control-Allow-Origin assertion is authoritative"
		}
		return false, fmt.Sprintf("GetBucketCors: %v", err)
	}
	var notes []string
	ok := len(out.CORSRules) > 0
	for i, r := range out.CORSRules {
		if len(r.AllowedOrigins) != 1 {
			ok = false
			notes = append(notes, fmt.Sprintf("rule %d has %d origins (Garage joins them into one header, which browsers reject)", i, len(r.AllowedOrigins)))
			continue
		}
		notes = append(notes, r.AllowedOrigins[0])
		if !contains(r.ExposeHeaders, "ETag") {
			ok = false
			notes = append(notes, fmt.Sprintf("rule %d does not expose ETag", i))
		}
	}
	return ok, strings.Join(notes, ", ")
}

// multipartETag matches what a completed multipart reports: the MD5 of the
// concatenated part MD5s, suffixed with the part count. A single-part or
// PutObject object has no suffix.
var multipartETag = regexp.MustCompile(`^[0-9a-f]{32}(-[0-9]+)?$`)

// checkPartSizes sweeps ResolvePartSize across the boundaries that matter: the
// configured size, both sides of the 10,000-part ceiling, and the largest file
// the grown path can still serve. Every result must be block-aligned, at or
// above S3's 5 MiB floor, within the part ceiling, and leave a final part in
// (0, part_size] — which is what "all parts equal except the last" means.
func checkPartSizes(configured int64) (bool, string) {
	const fiveMiB = 5 << 20
	sizes := []int64{
		1,
		configured - 1,
		configured,
		configured + 1,
		configured * upload.MaxParts,         // exactly the ceiling
		configured*upload.MaxParts + 1,       // one byte past it — the grown path
		configured * upload.MaxParts * 7,     // deep into the grown path
		upload.MaxPartSize * upload.MaxParts, // the largest file that resolves at all
	}
	var notes []string
	ok := true
	for _, size := range sizes {
		partSize, total, err := upload.ResolvePartSize(size, configured)
		if err != nil {
			ok = false
			notes = append(notes, fmt.Sprintf("size=%d: %v", size, err))
			continue
		}
		last := size - int64(total-1)*partSize
		switch {
		case partSize%upload.PartSizeGrain != 0:
			ok = false
			notes = append(notes, fmt.Sprintf("size=%d: part_size %d is not block-aligned", size, partSize))
		case partSize < fiveMiB:
			ok = false
			notes = append(notes, fmt.Sprintf("size=%d: part_size %d is below S3's 5 MiB floor", size, partSize))
		case total > upload.MaxParts:
			ok = false
			notes = append(notes, fmt.Sprintf("size=%d: %d parts exceeds the %d ceiling", size, total, upload.MaxParts))
		case last <= 0 || last > partSize:
			ok = false
			notes = append(notes, fmt.Sprintf("size=%d: final part %d is not in (0, %d]", size, last, partSize))
		}
	}
	if ok {
		return true, fmt.Sprintf("%d file sizes across the 10,000-part boundary: every part == part_size except a final part in (0, part_size]", len(sizes))
	}
	return false, strings.Join(notes, "; ")
}

func listAllParts(ctx context.Context, client *s3.Client, bucket, key, uploadID string) ([]s3types.Part, error) {
	var all []s3types.Part
	var marker *string
	for {
		out, err := client.ListParts(ctx, &s3.ListPartsInput{
			Bucket: &bucket, Key: &key, UploadId: &uploadID, PartNumberMarker: marker,
		})
		if err != nil {
			return nil, err
		}
		all = append(all, out.Parts...)
		if !aws.ToBool(out.IsTruncated) {
			return all, nil
		}
		marker = out.NextPartNumberMarker
	}
}

// findMultipart locates an in-progress multipart in the bucket listing and
// reports both what GC needs (the object key is there, with an Initiated
// timestamp) and whether the listed upload id is the one CreateMultipartUpload
// handed out. R2 mints a fresh opaque upload-id token per response, so the
// second answer decides which column GC's claim query may key on.
func findMultipart(ctx context.Context, client *s3.Client, bucket, key, uploadID string) (found, idMatch bool, initiated string, err error) {
	var keyMarker, idMarker *string
	for {
		out, err := client.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{
			Bucket: &bucket, KeyMarker: keyMarker, UploadIdMarker: idMarker,
		})
		if err != nil {
			return false, false, "", err
		}
		for _, u := range out.Uploads {
			if aws.ToString(u.Key) != key {
				continue
			}
			at := ""
			if u.Initiated != nil {
				at = u.Initiated.Format(time.RFC3339)
			}
			return true, aws.ToString(u.UploadId) == uploadID, at, nil
		}
		if !aws.ToBool(out.IsTruncated) {
			return false, false, "", nil
		}
		keyMarker, idMarker = out.NextKeyMarker, out.NextUploadIdMarker
	}
}

func servePublic(dir string, port int) (io.Closer, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, fmt.Errorf("serve spike page on :%d (is `make dev`'s vite running?): %w", port, err)
	}
	srv := &http.Server{Handler: http.FileServer(http.Dir(dir))}
	go func() { _ = srv.Serve(ln) }()
	return ln, nil
}

func runPlaywright(repo, manifestPath, resultsPath string, port int) error {
	// Seed the results file so the second spec can merge into it even if the
	// first one bails early.
	if err := os.WriteFile(resultsPath, []byte("{}"), 0o644); err != nil {
		return err
	}
	cmd := exec.Command("npx", "playwright", "test", "tests/spike.spec.ts", "--reporter=list")
	cmd.Dir = filepath.Join(repo, "e2e")
	cmd.Env = append(os.Environ(),
		"SPIKE_MANIFEST="+mustAbs(manifestPath),
		"SPIKE_RESULTS="+mustAbs(resultsPath),
		fmt.Sprintf("SPIKE_PAGE_URL=http://localhost:%d/index.html", port),
	)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// makeDropTree builds a real on-disk tree for the CDP folder-drop probe: a
// top-level directory with more than 100 entries (Chromium's readEntries batch
// cap) plus two nested levels.
func makeDropTree(root string) (int, error) {
	if err := os.RemoveAll(root); err != nil {
		return 0, err
	}
	count := 0
	write := func(dir, name string) error {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		count++
		return os.WriteFile(filepath.Join(dir, name), []byte(name+"\n"), 0o644)
	}
	for i := 0; i < dropFiles; i++ {
		if err := write(root, fmt.Sprintf("f%03d.txt", i)); err != nil {
			return 0, err
		}
	}
	for i := 0; i < 3; i++ {
		if err := write(filepath.Join(root, "level1"), fmt.Sprintf("a%d.txt", i)); err != nil {
			return 0, err
		}
		if err := write(filepath.Join(root, "level1", "level2"), fmt.Sprintf("b%d.txt", i)); err != nil {
			return 0, err
		}
	}
	return count, nil
}

func fetch(url string, headers map[string]string) (*http.Response, []byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return resp, body, err
}

// normalizeETag is the production function, not a copy of it: the point of
// these checks is that what the store returns survives the normalizer the
// server actually runs. An unnormalized compare always fails.
func normalizeETag(raw string) string { return upload.NormalizeETag(raw) }

// renderable reports whether a browser would render this Content-Type inline —
// the thing that must never be true for user-supplied bytes on a shared origin.
func renderable(ct string) bool {
	c := strings.ToLower(strings.TrimSpace(strings.Split(ct, ";")[0]))
	switch c {
	case "", "application/octet-stream", "binary/octet-stream":
		return false
	}
	return true
}

func xmlTag(body, tag string) string {
	open, closeTag := "<"+tag+">", "</"+tag+">"
	i := strings.Index(body, open)
	j := strings.Index(body, closeTag)
	if i < 0 || j < i {
		return ""
	}
	return body[i+len(open) : j]
}

// loadEnvFile applies KEY=VALUE lines without overriding variables already set
// in the process environment.
func loadEnvFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%s not found — run `make infra-init` first", path)
		}
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if _, exists := os.LookupEnv(k); !exists {
			if err := os.Setenv(k, v); err != nil {
				return err
			}
		}
	}
	return sc.Err()
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func mustAbs(p string) string {
	a, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return a
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if strings.EqualFold(s, want) {
			return true
		}
	}
	return false
}

func orElse(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
