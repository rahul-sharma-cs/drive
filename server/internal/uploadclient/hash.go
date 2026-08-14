package uploadclient

// Everything that reads bytes: the source abstraction, the fingerprint recipe,
// per-part MD5, whole-file SHA-256, and ETag normalization.
//
// The rule the whole file is written around: never materialize a part. Parts
// are up to 100 MiB by default and the battery uploads multi-GB sparse files,
// so every read here goes through a bounded buffer and the PUT body is a
// SectionReader over the caller's source. Peak memory is (concurrency + 1)
// buffers, independent of file and part size.

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	// EdgeBytes is the fingerprint's head/tail window: 1 MiB, matching the
	// browser engine's EDGE_BYTES.
	EdgeBytes = 1 << 20

	// hashChunk bounds a single read. The browser hashes parts over 8 MiB
	// sub-slices for the same reason -- a whole part in memory is what turns a
	// 50 GB upload into an OOM.
	hashChunk = 8 << 20
)

// Source is the bytes being uploaded: random access plus a known length.
//
// Random access is what lets parts upload concurrently while the SHA-256 pass
// streams the file from byte 0, with nothing buffered: a multi-GB sparse file
// costs a handful of 8 MiB chunks, not its own size.
//
// *bytes.Reader and *io.SectionReader satisfy this directly. A raw *os.File
// does not (it has no Size method), so Upload and Resume accept a plain
// io.ReaderAt and adapt it -- passing an *os.File straight in works.
type Source interface {
	io.ReaderAt
	Size() int64
}

// asSource resolves whatever the caller handed us into something that knows its
// own length. Anything else is a programming error worth naming precisely,
// because the alternative is uploading a file of the wrong declared size.
func asSource(r io.ReaderAt) (Source, error) {
	switch s := r.(type) {
	case nil:
		return nil, fmt.Errorf("uploadclient: no source given")
	case Source:
		return s, nil
	case *os.File:
		return NewFileSource(s)
	}
	return nil, fmt.Errorf(
		"uploadclient: source %T does not report its size; pass an *os.File, a *bytes.Reader, "+
			"or wrap it with NewFileSource", r)
}

// FileSource adapts an *os.File to Source, remembering the size and modification
// time observed at open time. Both feed the fingerprint, and pinning them once
// keeps a resume's fingerprint identical to the original upload's even if the
// file is touched underneath.
type FileSource struct {
	*os.File
	size    int64
	modTime time.Time
}

// NewFileSource wraps an open file. The caller keeps ownership and must close it.
func NewFileSource(f *os.File) (*FileSource, error) {
	if f == nil {
		return nil, fmt.Errorf("uploadclient: nil file")
	}
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("uploadclient: stat %s: %w", f.Name(), err)
	}
	return &FileSource{File: f, size: fi.Size(), modTime: fi.ModTime()}, nil
}

// OpenFile opens a path for upload. The caller must Close the returned source.
func OpenFile(path string) (*FileSource, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("uploadclient: open %s: %w", path, err)
	}
	src, err := NewFileSource(f)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return src, nil
}

// Size is the file's length as of open time.
func (f *FileSource) Size() int64 { return f.size }

// ModTime is the file's modification time as of open time. Upload uses it for
// the fingerprint's lastModified field when the request does not set one.
func (f *FileSource) ModTime() time.Time { return f.modTime }

// modTimer is what Upload looks for on a Source when UploadRequest.ModTime is
// unset. FileSource implements it; so does os.FileInfo-backed anything.
type modTimer interface{ ModTime() time.Time }

// NormalizeETag strips what S3 and Garage wrap an ETag in -- an optional weak
// validator prefix and surrounding quotes -- and lowercases the rest.
//
// A part's ETag is its MD5. Comparing an unnormalized `"ABC"` against a
// lowercase hex digest never matches, which would silently turn the integrity
// check into an unconditional retry loop.
func NormalizeETag(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == 'W' || s[0] == 'w') && s[1] == '/' {
		s = s[2:]
	}
	s = strings.Trim(s, `"`)
	return strings.ToLower(s)
}

// Fingerprint computes the upload fingerprint.
//
// The recipe is a cross-language contract with the browser engine
// (web/src/features/upload/engine/parts.ts) and is pinned byte for byte:
//
//	sha256_hex( utf8(
//	    name ++ dec(size) ++ dec(lastModifiedMs)
//	         ++ hex(sha256(bytes[0 .. min(1MiB, size)]))
//	         ++ hex(sha256(bytes[max(0, size-1MiB) .. size])) ))
//
// No separators of any kind. size and lastModifiedMs are base-10 integers and
// lastModifiedMs is MILLISECONDS since the epoch (a Go ns mtime truncates via
// Time.UnixMilli). Both edge digests and the result are lowercase hex. A file
// below 1 MiB hashes whole for both edges, so head and tail are identical.
//
// A mismatch with the browser is invisible: the server simply fails to match
// the session and the user silently re-uploads from zero. The golden vector in
// hash_test.go is the guard.
func Fingerprint(src io.ReaderAt, name string, size, lastModifiedMillis int64) (string, error) {
	if size < 0 {
		return "", fmt.Errorf("uploadclient: negative size %d", size)
	}
	buf := make([]byte, hashChunk)

	headLen := size
	if headLen > EdgeBytes {
		headLen = EdgeBytes
	}
	head := sha256.New()
	if err := hashRange(head, src, 0, headLen, buf); err != nil {
		return "", fmt.Errorf("uploadclient: hashing the leading %d bytes: %w", headLen, err)
	}

	tailStart := size - EdgeBytes
	if tailStart < 0 {
		tailStart = 0
	}
	tail := sha256.New()
	if err := hashRange(tail, src, tailStart, size-tailStart, buf); err != nil {
		return "", fmt.Errorf("uploadclient: hashing the trailing %d bytes: %w", size-tailStart, err)
	}

	payload := name +
		strconv.FormatInt(size, 10) +
		strconv.FormatInt(lastModifiedMillis, 10) +
		hex.EncodeToString(head.Sum(nil)) +
		hex.EncodeToString(tail.Sum(nil))

	outer := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(outer[:]), nil
}

// hashRange feeds src[off:off+n] into h through buf, never allocating and never
// holding more than one chunk. n == 0 is legal and hashes nothing -- that is
// the 0-byte file's head and tail.
func hashRange(h hash.Hash, src io.ReaderAt, off, n int64, buf []byte) error {
	for n > 0 {
		want := int64(len(buf))
		if want > n {
			want = n
		}
		read, err := src.ReadAt(buf[:want], off)
		if read > 0 {
			h.Write(buf[:read])
			off += int64(read)
			n -= int64(read)
		}
		if err != nil {
			// A full read that happens to end exactly at EOF reports io.EOF
			// alongside the bytes; only a short read is a real problem.
			if err == io.EOF && n == 0 {
				return nil
			}
			return err
		}
	}
	return nil
}

// partMD5 is the hex MD5 of one part, streamed in hashChunk-sized reads.
func partMD5(src io.ReaderAt, off, n int64) (string, error) {
	h := md5.New()
	buf := make([]byte, hashChunk)
	if err := hashRange(h, src, off, n, buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// wholeSHA256 is the hex SHA-256 of the entire source, which is what complete
// carries. It runs alongside the part uploads rather than after them.
func wholeSHA256(src io.ReaderAt, size int64) (string, error) {
	h := sha256.New()
	buf := make([]byte, hashChunk)
	if err := hashRange(h, src, 0, size, buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// partRange returns the byte range of 1-based part n. The final part is short;
// every other part is exactly partSize, which is what the server's confirm
// endpoint enforces.
func partRange(n int, partSize, fileSize int64) (off, length int64) {
	off = int64(n-1) * partSize
	end := off + partSize
	if end > fileSize {
		end = fileSize
	}
	if end < off {
		end = off
	}
	return off, end - off
}
