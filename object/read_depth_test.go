package object

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func nestedSequenceStream(levels int) []byte {
	le := binary.LittleEndian
	var b bytes.Buffer
	for i := 0; i < levels; i++ {
		b.Write(dicomtest.SequenceHeaderBytes(transfer.ExplicitVRLittleEndian, core.NewTag(0x0040, 0xa730), 0xFFFFFFFF))
		b.Write(dicomtest.SequenceControlBytes(le, core.TagItem, 0xFFFFFFFF))
	}
	for i := 0; i < levels; i++ {
		b.Write(dicomtest.SequenceControlBytes(le, core.TagItemDelimitationItem, 0))
		b.Write(dicomtest.SequenceControlBytes(le, core.TagSequenceDelimitationItem, 0))
	}
	return b.Bytes()
}

func TestReadDataSetSequenceDepthLimit(t *testing.T) {
	tests := []struct {
		name    string
		levels  int
		depth   int
		wantErr bool
	}{
		{name: "zero option applies the default", levels: parser.DefaultMaxSequenceDepth / 2},
		{name: "default refuses deeper nesting", levels: parser.DefaultMaxSequenceDepth/2 + 1, wantErr: true},
		{name: "100k levels refused by default", levels: 100_000, wantErr: true},
		{name: "raised limit", levels: 500, depth: 1000},
		{name: "unlimited", levels: 20_000, depth: parser.UnlimitedSequenceDepth},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			obj, err := ReadDataSetWithOptions(bytes.NewReader(nestedSequenceStream(test.levels)), transfer.ExplicitVRLittleEndian,
				ReadFileOptions{MaxSequenceDepth: test.depth})
			if test.wantErr {
				if !errors.Is(err, parser.ErrMaxDepthExceeded) {
					t.Fatalf("error = %v, want parser.ErrMaxDepthExceeded", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if _, ok := obj.GetSequence(core.NewTag(0x0040, 0xa730)); !ok {
				t.Fatal("outer sequence missing")
			}
		})
	}
}
