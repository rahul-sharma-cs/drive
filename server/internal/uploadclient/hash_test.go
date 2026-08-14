package uploadclient

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// The golden vector.
//
// This is the single highest-risk value in the package: the fingerprint decides
// whether the server matches an existing session, and a Go/browser mismatch
// produces NO error anywhere -- the user just silently re-uploads a 50 GB file
// from byte zero. The browser engine has the identical vector.
//
// Input:  2048 bytes, every one of them 0x00 (the browser's fixture is
//
//	`new File([new Uint8Array(2048)], 'report.pdf', {lastModified: 1_700_000_000_000})`)
//
//	name          = "report.pdf"
//	size          = 2048
//	lastModified  = 1700000000000  (milliseconds)
//
// Because 2048 < 1 MiB, head and tail both hash the whole file and are equal:
//
//	sha256(2048 zero bytes) = e5a00aa9991ac8a5ee3109844d84a55583bd20572ad3ffcd42792f3c36b183ad
//
// Payload (no separators, UTF-8):
//
//	report.pdf20481700000000000<head><tail>
const (
	goldenName    = "report.pdf"
	goldenSize    = 2048
	goldenModMs   = int64(1700000000000)
	goldenEdge    = "e5a00aa9991ac8a5ee3109844d84a55583bd20572ad3ffcd42792f3c36b183ad"
	goldenPayload = "report.pdf20481700000000000" + goldenEdge + goldenEdge
	goldenDigest  = "8d64d4f47dc60e17724c5541a57afb5014f972cec889a11d5d00f4f9548d7ca5"
)

func TestFingerprintGoldenVector(t *testing.T) {
	src := bytes.NewReader(make([]byte, goldenSize))

	got, err := Fingerprint(src, goldenName, goldenSize, goldenModMs)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	if got != goldenDigest {
		t.Fatalf("fingerprint mismatch\n got: %s\nwant: %s", got, goldenDigest)
	}

	// Pin the intermediates too, so a future mismatch says which half moved
	// instead of just showing two different digests.
	edge := sha256.Sum256(make([]byte, goldenSize))
	if hex.EncodeToString(edge[:]) != goldenEdge {
		t.Fatalf("edge digest moved: %s", hex.EncodeToString(edge[:]))
	}
	outer := sha256.Sum256([]byte(goldenPayload))
	if hex.EncodeToString(outer[:]) != goldenDigest {
		t.Fatalf("payload no longer hashes to the golden digest")
	}
	t.Logf("GOLDEN FINGERPRINT VECTOR\n"+
		"  content      = %d bytes of 0x00\n"+
		"  name         = %q\n"+
		"  size         = %d\n"+
		"  lastModified = %d (ms)\n"+
		"  head == tail = %s\n"+
		"  payload      = %s\n"+
		"  DIGEST       = %s",
		goldenSize, goldenName, goldenSize, goldenModMs, goldenEdge, goldenPayload, got)
}

// A file below 1 MiB hashes whole for both edges, so the payload carries the
// same digest twice. The browser pins the same rule.
func TestFingerprintSmallFileHeadEqualsTail(t *testing.T) {
	data := []byte("hello drive")
	sum := sha256.Sum256(data)
	edge := hex.EncodeToString(sum[:])

	want := sha256.Sum256([]byte("tiny.txt" + strconv.Itoa(len(data)) + "42" + edge + edge))
	got, err := Fingerprint(bytes.NewReader(data), "tiny.txt", int64(len(data)), 42)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	if got != hex.EncodeToString(want[:]) {
		t.Fatalf("small-file fingerprint = %s, want %s", got, hex.EncodeToString(want[:]))
	}
}

// Above 1 MiB the two edges are distinct windows, and the chunked reader has to
// stitch multiple 8 MiB reads together correctly.
func TestFingerprintLargeFileEdges(t *testing.T) {
	const size = 20 << 20 // 20 MiB: more than two hash chunks per edge pass
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i * 7 % 251)
	}
	head := sha256.Sum256(data[:EdgeBytes])
	tail := sha256.Sum256(data[size-EdgeBytes:])
	want := sha256.Sum256([]byte("big.bin" + strconv.Itoa(size) + "1700000000000" +
		hex.EncodeToString(head[:]) + hex.EncodeToString(tail[:])))

	got, err := Fingerprint(bytes.NewReader(data), "big.bin", size, goldenModMs)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	if got != hex.EncodeToString(want[:]) {
		t.Fatalf("large-file fingerprint = %s, want %s", got, hex.EncodeToString(want[:]))
	}
}

func TestFingerprintZeroByteFile(t *testing.T) {
	empty := sha256.Sum256(nil)
	e := hex.EncodeToString(empty[:])
	want := sha256.Sum256([]byte("empty.txt" + "0" + "0" + e + e))

	got, err := Fingerprint(bytes.NewReader(nil), "empty.txt", 0, 0)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	if got != hex.EncodeToString(want[:]) {
		t.Fatalf("0-byte fingerprint = %s, want %s", got, hex.EncodeToString(want[:]))
	}
}

// An *os.File is the battery's source: a multi-GB sparse file that must never
// be buffered. Its mtime feeds the fingerprint in milliseconds.
func TestFileSourceFingerprintUsesModTimeMillis(t *testing.T) {
	path := filepath.Join(t.TempDir(), goldenName)
	if err := os.WriteFile(path, make([]byte, goldenSize), 0o600); err != nil {
		t.Fatal(err)
	}
	mod := time.UnixMilli(goldenModMs)
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}

	src, err := OpenFile(path)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer func() { _ = src.Close() }()

	if src.Size() != goldenSize {
		t.Fatalf("Size = %d, want %d", src.Size(), goldenSize)
	}
	if src.ModTime().UnixMilli() != goldenModMs {
		t.Fatalf("ModTime = %d ms, want %d", src.ModTime().UnixMilli(), goldenModMs)
	}

	got, err := Fingerprint(src, goldenName, src.Size(), src.ModTime().UnixMilli())
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	if got != goldenDigest {
		t.Fatalf("*os.File fingerprint = %s, want the golden %s", got, goldenDigest)
	}
}

func TestNormalizeETag(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`"ABC"`, "abc"},
		{`W/"abc"`, "abc"},
		{`abc`, "abc"},
		{`"abc"`, "abc"},
		{`w/"ABC"`, "abc"},
		{`  "AbC"  `, "abc"},
		{`W/abc`, "abc"},
		{``, ""},
	} {
		if got := NormalizeETag(tc.in); got != tc.want {
			t.Errorf("NormalizeETag(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPartRange(t *testing.T) {
	// 2.5 parts: the last one is short, every other one is exact.
	const partSize, fileSize = 100, 250
	for _, tc := range []struct {
		n                int
		wantOff, wantLen int64
	}{
		{1, 0, 100},
		{2, 100, 100},
		{3, 200, 50},
	} {
		off, length := partRange(tc.n, partSize, fileSize)
		if off != tc.wantOff || length != tc.wantLen {
			t.Errorf("partRange(%d) = (%d,%d), want (%d,%d)", tc.n, off, length, tc.wantOff, tc.wantLen)
		}
	}
}

func TestPartMD5StreamsChunks(t *testing.T) {
	// Larger than one hash chunk, so the range loop has to iterate.
	data := make([]byte, hashChunk+4096)
	for i := range data {
		data[i] = byte(i % 253)
	}
	got, err := partMD5(bytes.NewReader(data), 0, int64(len(data)))
	if err != nil {
		t.Fatalf("partMD5: %v", err)
	}
	if want := md5Hex(data); got != want {
		t.Fatalf("partMD5 = %s, want %s", got, want)
	}
}

func TestWholeSHA256MatchesCrypto(t *testing.T) {
	data := []byte("the quick brown fox")
	got, err := wholeSHA256(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("wholeSHA256: %v", err)
	}
	sum := sha256.Sum256(data)
	if got != hex.EncodeToString(sum[:]) {
		t.Fatalf("wholeSHA256 = %s, want %s", got, hex.EncodeToString(sum[:]))
	}
}
