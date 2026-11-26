package parser

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"reflect"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// definedSequenceBytes encodes an Explicit VR Little Endian sequence whose
// sequence and items all have defined lengths.
func definedSequenceBytes(tag core.Tag, items int) []byte {
	var body bytes.Buffer
	for i := range items {
		item := dicomtest.EncodeElements(transfer.ExplicitVRLittleEndian,
			dicomtest.NewUIElement(core.NewTag(0x0008, 0x1150), "1.2.840.10008.5.1.4.1.1.2"),
			dicomtest.NewUIElement(core.NewTag(0x0008, 0x1155), fmt.Sprintf("1.2.1619.%d", i+1)))
		body.Write(dicomtest.SequenceControlBytes(binary.LittleEndian, core.TagItem, uint32(len(item))))
		body.Write(item)
	}
	out := dicomtest.ExplicitLongHeaderBytes(binary.LittleEndian, tag, core.VRSQ, uint32(body.Len()))
	return append(out, body.Bytes()...)
}

func seekProjectionData(items int) []byte {
	var data bytes.Buffer
	data.Write(dicomtest.EncodeElements(transfer.ExplicitVRLittleEndian,
		dicomtest.NewUIElement(core.NewTag(0x0008, 0x0018), "1.2.1619")))
	data.Write(definedSequenceBytes(core.NewTag(0x0008, 0x1140), items))
	data.Write(dicomtest.EncodeElements(transfer.ExplicitVRLittleEndian,
		dicomtest.NewStringElement(core.NewTag(0x0008, 0x0060), core.VRCS, "CT"),
		dicomtest.NewPNElement(core.NewTag(0x0010, 0x0010), "TRAILING^PATIENT")))
	return data.Bytes()
}

var seekProjectionTags = []core.Tag{core.NewTag(0x0008, 0x0018), core.NewTag(0x0010, 0x0010)}

func TestDataSetProjectionSeeksUnselectedSequences(t *testing.T) {
	data := seekProjectionData(512)
	opts := ReaderOptions{Dictionary: std.Dictionary, SkipPixelData: true}
	want, err := NewReader(bytes.NewReader(data), transfer.ExplicitVRLittleEndian, opts).ReadDataSetProjected(seekProjectionTags)
	if err != nil {
		t.Fatal(err)
	}
	opts.SeekUnselectedSequences = true
	source := &readCountingSeeker{Reader: bytes.NewReader(data)}
	got, err := NewReader(source, transfer.ExplicitVRLittleEndian, opts).ReadDataSetProjected(seekProjectionTags)
	if err != nil {
		t.Fatal(err)
	}
	if source.read > 4096 {
		t.Fatalf("read %d bytes of an unselected %d-byte sequence", source.read, len(data))
	}
	if len(got.Elements) != len(want.Elements) {
		t.Fatalf("elements = %d, want %d", len(got.Elements), len(want.Elements))
	}
	for i := range want.Elements {
		if got.Elements[i].Tag() != want.Elements[i].Tag() || !reflect.DeepEqual(got.Elements[i].Value, want.Elements[i].Value) {
			t.Fatalf("element %d = %+v, want %+v", i, got.Elements[i], want.Elements[i])
		}
	}
	if _, ok := got.Elements[1].Value.(core.DiscardedValue); !ok {
		t.Fatalf("unselected sequence = %T, want a discarded marker", got.Elements[1].Value)
	}

	// A non-seekable source parses the sequence exactly as without the option.
	streamed, err := NewReader(unseekableReader{r: bytes.NewReader(data)}, transfer.ExplicitVRLittleEndian, opts).ReadDataSetProjected(seekProjectionTags)
	if err != nil || !reflect.DeepEqual(streamed, want) {
		t.Fatalf("streamed projection differs: %v", err)
	}
	// Selected sequences keep their complete subtree.
	selected, err := NewReader(bytes.NewReader(data), transfer.ExplicitVRLittleEndian, opts).ReadDataSetProjected(append(seekProjectionTags, core.NewTag(0x0008, 0x1140)))
	if err != nil {
		t.Fatal(err)
	}
	if sequence, ok := selected.Elements[1].Value.(core.SequenceValue); !ok || len(sequence.Items) != 512 {
		t.Fatalf("selected sequence = %T", selected.Elements[1].Value)
	}
	// Other reads ignore the option.
	plain, err := NewReader(bytes.NewReader(data), transfer.ExplicitVRLittleEndian, opts).ReadDataSet()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := plain.Elements[1].Value.(core.SequenceValue); !ok {
		t.Fatalf("ordinary read sequence = %T", plain.Elements[1].Value)
	}
}

// A sequence the seek cannot pass indistinguishably (its length overruns the
// source or the byte ceiling, or the source is truncated inside it) is
// parsed, so the failure is the one the option-free projection reports.
func TestDataSetProjectionSeekKeepsLengthFailures(t *testing.T) {
	data := seekProjectionData(64)
	sequenceAt := bytes.Index(data, []byte{0x08, 0, 0x40, 0x11, 'S', 'Q'})
	overrun := append([]byte(nil), data...)
	binary.LittleEndian.PutUint32(overrun[sequenceAt+8:], uint32(len(data)))
	for _, tc := range []struct {
		name string
		wire []byte
		opts ReaderOptions
	}{
		{"truncated sequence", data[:sequenceAt+600], ReaderOptions{}},
		{"length past the source end", overrun, ReaderOptions{}},
		{"total bytes", data, ReaderOptions{MaxTotalBytes: int64(sequenceAt + 600)}},
		{"depth", data, ReaderOptions{MaxSequenceDepth: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.opts.Dictionary, tc.opts.SkipPixelData = std.Dictionary, true
			_, wantErr := NewReader(bytes.NewReader(tc.wire), transfer.ExplicitVRLittleEndian, tc.opts).ReadDataSetProjected(seekProjectionTags)
			tc.opts.SeekUnselectedSequences = true
			_, err := NewReader(bytes.NewReader(tc.wire), transfer.ExplicitVRLittleEndian, tc.opts).ReadDataSetProjected(seekProjectionTags)
			if tc.name == "depth" {
				// Depth is a property of the sequence content, which the
				// seek does not open.
				if wantErr == nil || err != nil {
					t.Fatalf("projection: %v; seek: %v", wantErr, err)
				}
				return
			}
			if wantErr == nil || err == nil || wantErr.Error() != err.Error() {
				t.Fatalf("projection: %v; seek: %v", wantErr, err)
			}
		})
	}
}

// BenchmarkDataSetProjectionUnselectedSequence compares, in one run, the
// projection of a data set with a 2,048-item unselected sequence parsed
// structurally and passed by seek (#1619).
func BenchmarkDataSetProjectionUnselectedSequence(b *testing.B) {
	data := seekProjectionData(2048)
	for _, seek := range []bool{false, true} {
		b.Run(fmt.Sprintf("seek_%v", seek), func(b *testing.B) {
			b.ReportAllocs()
			opts := ReaderOptions{Dictionary: std.Dictionary, SkipPixelData: true, SeekUnselectedSequences: seek}
			for range b.N {
				if _, err := NewReader(bytes.NewReader(data), transfer.ExplicitVRLittleEndian, opts).ReadDataSetProjected(seekProjectionTags); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
