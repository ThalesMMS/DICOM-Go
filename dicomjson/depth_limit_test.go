package dicomjson

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/dictionary/std"
)

// nestedSequenceJSON nests levels Content Sequence items below the root.
func nestedSequenceJSON(levels int) []byte {
	var b strings.Builder
	for i := 0; i < levels; i++ {
		b.WriteString(`{"0040A730":{"vr":"SQ","Value":[`)
	}
	b.WriteString(`{}`)
	for i := 0; i < levels; i++ {
		b.WriteString(`]}}`)
	}
	return []byte(b.String())
}

func TestSequenceDepthDefaultAndOverride(t *testing.T) {
	tests := []struct {
		name    string
		levels  int
		limits  Limits
		wantErr bool
	}{
		{name: "zero limit applies the default", levels: DefaultMaxSequenceDepth},
		{name: "default refuses deeper nesting", levels: DefaultMaxSequenceDepth + 1, wantErr: true},
		{name: "raised limit", levels: 200, limits: Limits{MaxSequenceDepth: 200}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			obj, err := UnmarshalContext(context.Background(), nestedSequenceJSON(test.levels), std.Dictionary, UnmarshalOptions{Limits: test.limits})
			if test.wantErr {
				if !errors.Is(err, ErrMaxSequenceDepthExceeded) {
					t.Fatalf("UnmarshalContext() error = %v, want ErrMaxSequenceDepthExceeded", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("UnmarshalContext() error = %v", err)
			}
			// Marshal applies the same default to the decoded object.
			if _, err := MarshalContext(context.Background(), obj, Options{Limits: test.limits}); err != nil {
				t.Fatalf("MarshalContext() error = %v", err)
			}
		})
	}
}
