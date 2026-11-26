package parser

import (
	"bytes"
	"encoding/binary"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/transfer"
	"reflect"
	"testing"
)

func privateBulkData(syntax transfer.Syntax, size int) []byte {
	blob := core.NewRawElement(core.NewTag(0x0011, 0x1010), core.VROB, make([]byte, size))
	// Deliberately unsorted, with public metadata after the private sequence.
	return dicomtest.EncodeElements(syntax,
		dicomtest.NewStringElement(core.NewTag(0x0011, 0x0010), core.VRLO, "BULK OWNER"), blob,
		dicomtest.NewSequenceElement(core.NewTag(0x0011, 0x1020), core.DataSet{Elements: []core.Element{blob,
			dicomtest.NewUIElement(core.NewTag(0x0008, 0x1155), "1.2.3.4")}}),
		dicomtest.NewStringElement(core.NewTag(0x0011, 0x1030), core.VRLO, "RETAIN PRIVATE TEXT"),
		core.NewRawElement(core.NewTag(0x0011, 0x1040), core.VRUS, []byte{2, 0}),
		core.NewRawElement(core.NewTag(0x6000, 0x3000), core.VROB, []byte{1, 2}),
		dicomtest.NewPNElement(core.NewTag(0x0010, 0x0010), "TRAILING^PATIENT"))
}

func TestDataSetProjectionKeepsSelectedValuesAndStructureChecks(t *testing.T) {
	for _, syntax := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ExplicitVRBigEndian} {
		t.Run(syntax.Name, func(t *testing.T) {
			data := privateBulkData(syntax, 1<<20)
			source := &readCountingSeeker{Reader: bytes.NewReader(data)}
			opts := ReaderOptions{Dictionary: std.Dictionary, SkipPixelData: true}
			got, err := NewReader(source, syntax, opts).ReadDataSetProjected([]core.Tag{core.NewTag(0x0011, 0x1030), core.NewTag(0x0011, 0x1040), core.NewTag(0x6000, 0x3000), core.NewTag(0x0010, 0x0010)})
			if err != nil {
				t.Fatal(err)
			}
			if source.read > 4096 {
				t.Fatalf("read %d bytes of unused private bulk", source.read)
			}
			want, err := NewReader(unseekableReader{r: bytes.NewReader(data)}, syntax, opts).ReadDataSetProjected([]core.Tag{core.NewTag(0x0011, 0x1030), core.NewTag(0x0011, 0x1040), core.NewTag(0x6000, 0x3000), core.NewTag(0x0010, 0x0010)})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatal("seek and stream datasets differ")
			}
			if _, ok := got.Elements[1].Value.(core.DiscardedValue); !ok {
				t.Fatal("private binary value lacks discarded marker")
			}
			if _, ok := got.Elements[2].Value.(core.DiscardedValue); !ok {
				t.Fatal("unselected sequence retained")
			}
			for _, i := range []int{0, 3, 4, 5, 6} {
				if _, ok := got.Elements[i].Value.(core.RawValue); !ok {
					t.Fatalf("element %d not retained: %T", i, got.Elements[i].Value)
				}
			}
		})
	}
}

func TestDataSetProjectionPreservesFailures(t *testing.T) {
	data := privateBulkData(transfer.ExplicitVRLittleEndian, 4096)
	odd := append([]byte(nil), data...)
	// The first blob starts after the creator. Make its declared VL odd;
	// ordinary and skipped readers must both reject it before consuming bytes.
	blobAt := bytes.Index(odd, []byte{0x11, 0, 0x10, 0x10, 'O', 'B'})
	binary.LittleEndian.PutUint32(odd[blobAt+8:], 4095)
	container := append([]byte(nil), data...)
	sequenceAt := bytes.Index(container, []byte{0x11, 0, 0x20, 0x10, 'S', 'Q'})
	binary.LittleEndian.PutUint32(container[sequenceAt+8:], 2)
	for _, tc := range []struct {
		name string
		wire []byte
		opts ReaderOptions
	}{
		{"truncated value", data[:2048], ReaderOptions{}},
		{"truncated sequence", data[:6000], ReaderOptions{}},
		{"malformed trailing header", append(append([]byte(nil), data...), 0), ReaderOptions{}},
		{"odd value length", odd, ReaderOptions{}},
		{"defined container overrun", container, ReaderOptions{}},
		{"element limit", data, ReaderOptions{MaxElements: 2}},
		{"element bytes", data, ReaderOptions{MaxElementBytes: 2048}},
		{"total bytes", data, ReaderOptions{MaxTotalBytes: 2048}},
		{"depth", data, ReaderOptions{MaxSequenceDepth: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.opts.Dictionary = std.Dictionary
			_, originalErr := NewReader(unseekableReader{r: bytes.NewReader(tc.wire)}, transfer.ExplicitVRLittleEndian, tc.opts).ReadDataSet()
			tc.opts.SkipPixelData = true
			_, err := NewReader(bytes.NewReader(tc.wire), transfer.ExplicitVRLittleEndian, tc.opts).ReadDataSetProjected(nil)
			if originalErr == nil || err == nil || originalErr.Error() != err.Error() {
				t.Fatalf("ordinary: %v; skipped: %v", originalErr, err)
			}
		})
	}
}

func TestDataSetProjectionBinaryVRsAndImplicitUnknowns(t *testing.T) {
	for _, syntax := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ImplicitVRLittleEndian} {
		for _, vr := range []core.VR{core.VROB, core.VROW, core.VRUN} {
			t.Run(syntax.Name+"/"+vr.String(), func(t *testing.T) {
				wire := dicomtest.EncodeElements(syntax,
					core.NewRawElement(core.NewTag(0x0011, 0x1010), vr, []byte{1, 2, 3, 4}),
					dicomtest.NewUIElement(core.NewTag(0x0008, 0x0018), "1.2.1576"))
				got, err := NewReader(bytes.NewReader(wire), syntax, ReaderOptions{SkipPixelData: true, Dictionary: std.Dictionary}).ReadDataSetProjected([]core.Tag{core.NewTag(0x0008, 0x0018)})
				if err != nil {
					t.Fatal(err)
				}
				if _, ok := got.Elements[0].Value.(core.DiscardedValue); !ok {
					t.Fatalf("private %s retained", vr)
				}
				if _, ok := got.Elements[1].Value.(core.RawValue); !ok {
					t.Fatal("trailing public UID lost")
				}
			})
		}
	}
}

func TestDataSetProjectionRetainsCompleteSelectedSubtree(t *testing.T) {
	selected := core.NewTag(0x0010, 0x0024)
	wire := dicomtest.EncodeElements(transfer.ExplicitVRLittleEndian,
		dicomtest.NewSequenceElement(selected, core.DataSet{Elements: []core.Element{
			core.NewRawElement(core.NewTag(0x0011, 0x1010), core.VROB, []byte{1, 2, 3, 4}),
			dicomtest.NewSequenceElement(core.NewTag(0x0040, 0xA730), core.DataSet{Elements: []core.Element{
				dicomtest.NewStringElement(core.NewTag(0x0040, 0xA160), core.VRUT, "preserve complete subtree"),
			}}),
		}}),
		dicomtest.NewStringElement(core.NewTag(0x0040, 0xA160), core.VRUT, "discard root value"))
	opts := ReaderOptions{Dictionary: std.Dictionary, SkipPixelData: true}
	full, err := NewReader(bytes.NewReader(wire), transfer.ExplicitVRLittleEndian, opts).ReadDataSet()
	if err != nil {
		t.Fatal(err)
	}
	projected, err := NewReader(bytes.NewReader(wire), transfer.ExplicitVRLittleEndian, opts).ReadDataSetProjected([]core.Tag{selected})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(projected.Elements[0], full.Elements[0]) {
		t.Fatal("selected subtree lost values or item provenance")
	}
	if _, ok := projected.Elements[1].Value.(core.DiscardedValue); !ok {
		t.Fatal("unselected public root retained")
	}
	for end := 0; end < len(wire); end++ {
		_, fullErr := NewReader(bytes.NewReader(wire[:end]), transfer.ExplicitVRLittleEndian, opts).ReadDataSet()
		_, projectErr := NewReader(bytes.NewReader(wire[:end]), transfer.ExplicitVRLittleEndian, opts).ReadDataSetProjected(nil)
		if (fullErr == nil) != (projectErr == nil) || fullErr != nil && fullErr.Error() != projectErr.Error() {
			t.Fatalf("truncation %d changed rejection: %v / %v", end, fullErr, projectErr)
		}
	}
}

func TestDataSetProjectionPreservesPrivateCreatorChecksInOmittedItems(t *testing.T) {
	for _, creator := range []string{"ACME", "A\\B"} {
		for _, strict := range []bool{false, true} {
			wire := dicomtest.EncodeElements(transfer.ImplicitVRLittleEndian,
				creatorElement(0x10, "ACME"),
				dicomtest.NewSequenceElement(core.NewTag(0x0011, 0x1002), core.DataSet{Elements: []core.Element{
					creatorElement(0x10, creator), core.NewRawElement(core.NewTag(0x0011, 0x1001), core.VRUS, []byte{7, 0}),
				}}),
				core.NewRawElement(core.NewTag(0x0011, 0x1001), core.VRUS, []byte{8, 0}))
			opts := ReaderOptions{Dictionary: privateTestDictionary(t), SkipPixelData: true, RejectInvalidPrivateCreators: strict}
			full := NewReader(bytes.NewReader(wire), transfer.ImplicitVRLittleEndian, opts)
			want, wantErr := full.ReadDataSet()
			projection := NewReader(bytes.NewReader(wire), transfer.ImplicitVRLittleEndian, opts)
			got, err := projection.ReadDataSetProjected([]core.Tag{core.NewTag(0x0011, 0x1001)})
			if (wantErr == nil) != (err == nil) || err != nil && err.Error() != wantErr.Error() {
				t.Fatalf("private creator changed rejection: %v / %v", wantErr, err)
			}
			gotDiagnostics, gotTruncated := projection.PrivateDiagnostics()
			wantDiagnostics, wantTruncated := full.PrivateDiagnostics()
			if !reflect.DeepEqual(gotDiagnostics, wantDiagnostics) || gotTruncated != wantTruncated {
				t.Fatal("omitted item lost private diagnostics")
			}
			if err == nil && !reflect.DeepEqual(got.Elements[2], want.Elements[2]) {
				t.Fatal("omitted item leaked its creator scope")
			}
		}
	}
}

func BenchmarkProjectedMetadataRead(b *testing.B) {
	data := privateBulkData(transfer.ExplicitVRLittleEndian, 8<<20)
	for _, skip := range []bool{false, true} {
		name := "materialize"
		if skip {
			name = "project"
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			var read int64
			for range b.N {
				source := &readCountingSeeker{Reader: bytes.NewReader(data)}
				reader := NewReader(source, transfer.ExplicitVRLittleEndian, ReaderOptions{Dictionary: std.Dictionary, SkipPixelData: true})
				var err error
				if skip {
					_, err = reader.ReadDataSetProjected([]core.Tag{core.NewTag(0x0010, 0x0010)})
				} else {
					_, err = reader.ReadDataSet()
				}
				if err != nil {
					b.Fatal(err)
				}
				read += source.read
			}
			b.ReportMetric(float64(read)/float64(b.N), "bytes_read/op")
		})
	}
}
