package upload

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// fakePartLister serves canned ListParts pages and records the markers it was
// asked for.
//
// The pagination loop is unit-tested rather than driven from Garage on purpose:
// making a real upload cross the 1,000-entry page boundary means an ~11 GiB
// file at 10 MiB parts, which is what `make test-big` is for. What can go wrong
// here -- dropping the marker, stopping at the first page, spinning on a
// truncated page with no marker -- is entirely visible from a fake.
type fakePartLister struct {
	pages   []*s3.ListPartsOutput
	markers []string
	err     error
}

func (f *fakePartLister) ListParts(_ context.Context, in *s3.ListPartsInput, _ ...func(*s3.Options)) (*s3.ListPartsOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.markers = append(f.markers, aws.ToString(in.PartNumberMarker))
	if len(f.pages) == 0 {
		return nil, errors.New("fakePartLister: asked for one page too many")
	}
	page := f.pages[0]
	f.pages = f.pages[1:]
	return page, nil
}

// page builds a ListParts page covering part numbers [from, to].
func page(from, to int, truncated bool) *s3.ListPartsOutput {
	out := &s3.ListPartsOutput{IsTruncated: aws.Bool(truncated)}
	for n := from; n <= to; n++ {
		num := int32(n)
		out.Parts = append(out.Parts, types.Part{
			PartNumber: &num,
			Size:       aws.Int64(1 << 20),
			ETag:       aws.String(fmt.Sprintf(`"%032x"`, n)),
		})
	}
	if truncated {
		out.NextPartNumberMarker = aws.String(fmt.Sprint(to))
	}
	return out
}

func TestListAllPartsPaginatesToExhaustion(t *testing.T) {
	f := &fakePartLister{pages: []*s3.ListPartsOutput{
		page(1, 1000, true),
		page(1001, 2000, true),
		page(2001, 2500, false),
	}}

	parts, err := ListAllParts(context.Background(), f, "bucket", "blobs/x", "upload-1")
	if err != nil {
		t.Fatalf("ListAllParts: %v", err)
	}
	if len(parts) != 2500 {
		t.Fatalf("got %d parts, want 2500 -- the loop stopped at a page boundary", len(parts))
	}
	if parts[0].Number != 1 || parts[2499].Number != 2500 {
		t.Fatalf("part numbers run %d..%d, want 1..2500", parts[0].Number, parts[2499].Number)
	}
	// The first call carries no marker; each later one carries the previous
	// page's NextPartNumberMarker.
	want := []string{"", "1000", "2000"}
	for i, m := range want {
		if f.markers[i] != m {
			t.Fatalf("call %d used marker %q, want %q", i+1, f.markers[i], m)
		}
	}
	// ETags arrive normalized, so they can be compared with a client MD5
	// without any further work.
	if parts[0].ETag != fmt.Sprintf("%032x", 1) {
		t.Fatalf("ETag %q is still quoted", parts[0].ETag)
	}
}

func TestListAllPartsHandlesAnEmptyUpload(t *testing.T) {
	f := &fakePartLister{pages: []*s3.ListPartsOutput{page(1, 0, false)}}
	parts, err := ListAllParts(context.Background(), f, "bucket", "blobs/x", "upload-1")
	if err != nil || len(parts) != 0 {
		t.Fatalf("ListAllParts = %v, %v; want no parts and no error", parts, err)
	}
}

// A page that claims truncation but names no next marker would loop forever,
// holding a request open. Treat it as the end.
func TestListAllPartsStopsOnATruncatedPageWithNoMarker(t *testing.T) {
	broken := page(1, 10, true)
	broken.NextPartNumberMarker = nil
	f := &fakePartLister{pages: []*s3.ListPartsOutput{broken}}

	parts, err := ListAllParts(context.Background(), f, "bucket", "blobs/x", "upload-1")
	if err != nil {
		t.Fatalf("ListAllParts: %v", err)
	}
	if len(parts) != 10 {
		t.Fatalf("got %d parts, want 10", len(parts))
	}
}

func TestListAllPartsPropagatesErrors(t *testing.T) {
	f := &fakePartLister{err: &types.NoSuchUpload{}}
	if _, err := ListAllParts(context.Background(), f, "bucket", "blobs/x", "gone"); err == nil {
		t.Fatal("a ListParts failure was swallowed")
	} else {
		var nsu *types.NoSuchUpload
		if !errors.As(err, &nsu) {
			t.Fatalf("the wrapped error lost its type: %v", err)
		}
	}
}
