package derivation

import (
	"errors"
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/internal/derivedio"
)

func TestSourceWithFrameIndices(t *testing.T) {
	base := Source{SOPClassUID: "1.2.3", SOPInstanceUID: "1.2.4", Purpose: &SourceImageForProcessing}
	for _, tc := range []struct {
		name    string
		count   int
		indices []int
		want    []string
		invalid bool
	}{
		{name: "classic single frame", count: 1, indices: []int{0}},
		{name: "first frame subset", count: 6, indices: []int{0}, want: []string{"1"}},
		{name: "multiple contributors", count: 6, indices: []int{4, 0, 4, 2}, want: []string{"5", "1", "3"}},
		{name: "missing count", count: 0, indices: []int{0}, invalid: true},
		{name: "negative count", count: -1, indices: []int{0}, invalid: true},
		{name: "negative index", count: 6, indices: []int{0, -1}, invalid: true},
		{name: "index past end", count: 6, indices: []int{0, 6}, invalid: true},
		{name: "no frame selection", count: 6, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := base.WithFrameIndices(tc.count, tc.indices...)
			if tc.invalid {
				if !errors.Is(err, ErrInvalid) {
					t.Fatalf("WithFrameIndices error = %v, want ErrInvalid", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.SOPClassUID != base.SOPClassUID || got.SOPInstanceUID != base.SOPInstanceUID || got.Purpose != base.Purpose {
				t.Fatalf("reference identity changed: %+v", got)
			}
			item, err := got.Item()
			if err != nil {
				t.Fatal(err)
			}
			data := reread(t, derivedio.Object(item.Elements...))
			values, _ := data.GetStrings(tagReferencedFrameNumber)
			if data.Has(tagReferencedFrameNumber) != (len(tc.want) > 0) || !slices.Equal(values, tc.want) {
				t.Fatalf("Referenced Frame Number = %v, want %v", values, tc.want)
			}
		})
	}
	whole, err := base.Item()
	if err != nil || derivedio.Object(whole.Elements...).Has(tagReferencedFrameNumber) {
		t.Fatalf("whole-instance reference changed: %+v, %v", whole, err)
	}
}
