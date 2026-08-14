package upload

// Client-side identity: the fingerprint that decides whether two creates are
// the same upload, and the chimera guard that decides whether a re-selected
// file is really the file whose parts are already in Garage.
//
// The guard exists because a fingerprint is deliberately cheap -- name, size,
// mtime, and the hashes of the first and last MiB -- so a file edited in place
// (a VM image, a database file) can keep all five and still have different
// bytes in the middle. Resuming onto that would splice two files into one
// object that hashes to neither.
//
// The trigger is server-side state, never inference: verify_parts is armed by
// a matched-session create (the only re-selection signal the server gets) and
// by a reconciliation that found drift. While armed, no URL is issued until
// the client shows MD5s for the pinned parts that match the ledger.

import (
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// maxFingerprint bounds what we will store. The client sends a hash, but the
// wire format is the client's business -- the contract does not pin hex over
// base64 -- so this validates shape, not encoding.
const maxFingerprint = 128

// CleanFingerprint validates and returns the fingerprint to store.
func CleanFingerprint(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > maxFingerprint {
		return "", ErrInvalid
	}
	for _, r := range s {
		if r > unicode.MaxASCII || !unicode.IsPrint(r) {
			return "", ErrInvalid
		}
	}
	return s, nil
}

// NormalizeETag strips what S3 and Garage wrap an ETag in -- a weak-validator
// prefix and surrounding quotes -- and lowercases the rest. A part's ETag is
// its MD5, and an unnormalized comparison against a client MD5 never matches,
// which would silently downgrade the integrity check to nothing.
func NormalizeETag(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "W/")
	s = strings.TrimPrefix(s, "w/")
	s = strings.Trim(s, `"`)
	return strings.ToLower(s)
}

// CleanMD5 accepts a 32-character hex digest in any case and returns it
// lowercased.
func CleanMD5(s string) (string, error) {
	s = NormalizeETag(s)
	if len(s) != 32 {
		return "", ErrInvalid
	}
	if _, err := hex.DecodeString(s); err != nil {
		return "", ErrInvalid
	}
	return s, nil
}

// PinnedParts returns the [lowest, highest] confirmed part numbers to pin the
// chimera check to, skipping rows with no client MD5 -- those were adopted
// from ListParts during reconciliation and there is nothing to compare against.
//
// The pair is what the client re-hashes from the re-selected file. It never
// moves once armed, so the bounce and the re-call always talk about the same
// two parts.
func PinnedParts(parts []Part) []int {
	lo, hi := 0, 0
	for _, p := range parts {
		if p.MD5 == nil {
			continue
		}
		if lo == 0 || p.Number < lo {
			lo = p.Number
		}
		if p.Number > hi {
			hi = p.Number
		}
	}
	if lo == 0 {
		return nil
	}
	return []int{lo, hi}
}

// VerifyPins compares the client's MD5s against the ledger for the pinned
// parts.
//
// covered is false when the client did not send an MD5 for every pinned part:
// that is the bounce, and the response carries verify_parts and no URLs. When
// covered, ok reports whether every pinned MD5 matched -- a mismatch is the
// chimera refusal.
//
// Extra entries are ignored, and a pinned pair whose two members are the same
// part is one requirement, not two.
func VerifyPins(pinned []int, ledger []Part, supplied map[string]string) (covered, ok bool) {
	want := make(map[int]string, len(pinned))
	for _, p := range ledger {
		if p.MD5 != nil {
			want[p.Number] = strings.ToLower(*p.MD5)
		}
	}

	for _, n := range uniqueSorted(pinned) {
		got, err := CleanMD5(supplied[strconv.Itoa(n)])
		if err != nil {
			return false, false
		}
		// A pinned part whose ledger MD5 vanished (reconciliation replaced the
		// row) can no longer be verified, so the resume is refused rather than
		// waved through.
		if want[n] == "" || want[n] != got {
			return true, false
		}
	}
	return true, true
}

func uniqueSorted(ns []int) []int {
	seen := make(map[int]bool, len(ns))
	out := make([]int, 0, len(ns))
	for _, n := range ns {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Ints(out)
	return out
}
