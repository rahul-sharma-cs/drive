package upload

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The part-size arithmetic is the one piece of the create path that cannot be
// exercised with real bytes: proving the auto-grow needs a file bigger than
// 10,000 parts, which at any realistic part size is terabytes.
func TestResolvePartSize(t *testing.T) {
	const (
		miB  int64 = 1 << 20
		giB  int64 = 1 << 30
		base       = 100 * miB
	)

	cases := []struct {
		name       string
		fileSize   int64
		configured int64
		wantSize   int64
		wantTotal  int
		wantErr    error
	}{
		{name: "empty file has no parts", fileSize: 0, configured: base, wantSize: base, wantTotal: 0},
		{name: "one byte is one part", fileSize: 1, configured: base, wantSize: base, wantTotal: 1},
		{name: "exact multiple", fileSize: 3 * base, configured: base, wantSize: base, wantTotal: 3},
		{name: "remainder gets its own part", fileSize: 3*base + 1, configured: base, wantSize: base, wantTotal: 4},
		// 50 GB at 100 MiB: the number PLAN quotes, derived rather than typed.
		{name: "50 GB at 100 MiB", fileSize: 50_000_000_000, configured: base, wantSize: base, wantTotal: 477},
		{name: "50 GiB at 100 MiB", fileSize: 50 * giB, configured: base, wantSize: base, wantTotal: 512},
		// At 10 MiB parts a 200 GiB file would need 20,480 parts, so the part
		// size grows to the smallest 10 MiB multiple that fits in 10,000.
		{name: "auto-grow keeps parts under the ceiling", fileSize: 200 * giB, configured: 10 * miB,
			wantSize: 30 * miB, wantTotal: 6827},
		// Exactly at the ceiling: no growth.
		{name: "exactly 10,000 parts does not grow", fileSize: 10000 * 10 * miB, configured: 10 * miB,
			wantSize: 10 * miB, wantTotal: 10000},
		// One byte past it: the smallest possible growth, one grain -- and the
		// stray byte still costs a whole part of its own.
		{name: "one byte past the ceiling grows by one grain", fileSize: 10000*10*miB + 1, configured: 10 * miB,
			wantSize: 20 * miB, wantTotal: 5001},
		{name: "too large even at the maximum part size",
			fileSize: MaxParts*MaxPartSize + 1, configured: 10 * miB, wantErr: ErrTooLarge},
		{name: "negative size", fileSize: -1, configured: base, wantErr: ErrInvalid},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			size, total, err := ResolvePartSize(c.fileSize, c.configured)
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("err = %v, want %v", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if size != c.wantSize || total != c.wantTotal {
				t.Fatalf("part_size=%d parts_total=%d, want %d and %d", size, total, c.wantSize, c.wantTotal)
			}
			if total > MaxParts {
				t.Fatalf("parts_total %d exceeds the %d ceiling", total, MaxParts)
			}
			if c.fileSize > 0 && size%PartSizeGrain != 0 && c.configured%PartSizeGrain == 0 {
				t.Fatalf("part_size %d is not a multiple of the %d block size", size, PartSizeGrain)
			}
		})
	}
}

func TestCheckPart(t *testing.T) {
	// Three parts of 10 bytes plus a 4-byte tail.
	sess := &Session{FileSize: 34, PartSize: 10, PartsTotal: 4}

	cases := []struct {
		name    string
		number  int
		size    int64
		wantErr bool
	}{
		{name: "non-final part at exactly part_size", number: 1, size: 10},
		{name: "non-final part short", number: 2, size: 9, wantErr: true},
		{name: "non-final part long", number: 2, size: 11, wantErr: true},
		{name: "final part at the remainder", number: 4, size: 4},
		{name: "final part shorter than the remainder", number: 4, size: 1},
		{name: "final part past the remainder", number: 4, size: 5, wantErr: true},
		{name: "final part empty", number: 4, size: 0, wantErr: true},
		{name: "part zero", number: 0, size: 10, wantErr: true},
		{name: "part past the total", number: 5, size: 10, wantErr: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := sess.CheckPart(c.number, c.size)
			if (err != nil) != c.wantErr {
				t.Fatalf("CheckPart(%d, %d) = %v, wantErr %v", c.number, c.size, err, c.wantErr)
			}
		})
	}
}

func TestMissingParts(t *testing.T) {
	confirmed := []Part{{Number: 2}, {Number: 5}}
	if got, want := MissingParts(5, confirmed), []int{1, 3, 4}; !reflect.DeepEqual(got, want) {
		t.Fatalf("MissingParts = %v, want %v", got, want)
	}
	if got := MissingParts(0, nil); got != nil {
		t.Fatalf("a 0-byte session has no missing parts, got %v", got)
	}
	if got := MissingParts(2, []Part{{Number: 1}, {Number: 2}}); got != nil {
		t.Fatalf("a complete ledger has no missing parts, got %v", got)
	}
}

func TestExpiredCoversBothWaysOfBeingGone(t *testing.T) {
	future := time.Now().Add(time.Hour)
	past := time.Now().Add(-time.Hour)

	cases := []struct {
		name    string
		sess    Session
		expired bool
		live    bool
	}{
		{name: "active and in date", sess: Session{Status: StatusActive, ExpiresAt: future}, live: true},
		{name: "active but past its deadline", sess: Session{Status: StatusActive, ExpiresAt: past}, expired: true},
		{name: "aborted", sess: Session{Status: StatusAborted, ExpiresAt: future}, expired: true},
		{name: "completing", sess: Session{Status: StatusCompleting, ExpiresAt: past}},
		{name: "done", sess: Session{Status: StatusDone, ExpiresAt: past}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.sess.Expired(); got != c.expired {
				t.Errorf("Expired() = %v, want %v", got, c.expired)
			}
			if got := c.sess.Live(); got != c.live {
				t.Errorf("Live() = %v, want %v", got, c.live)
			}
		})
	}
}

// Every status transition in the codebase serializes on this key, so it has to
// be a pure function of the id and nothing else.
func TestLockKeyIsStable(t *testing.T) {
	id := uuid.MustParse("11223344-5566-7788-99aa-bbccddeeff00")
	if got, want := LockKey(id), int64(0x1122334455667788); got != want {
		t.Fatalf("LockKey = %#x, want %#x", got, want)
	}
	if LockKey(id) != LockKey(uuid.MustParse(id.String())) {
		t.Fatal("LockKey is not stable across parses of the same id")
	}
	if LockKey(uuid.New()) == LockKey(uuid.New()) {
		t.Fatal("two random ids collided, which should not happen twice in a lifetime")
	}
}
