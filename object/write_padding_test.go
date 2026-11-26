package object

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"io"
	"reflect"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestWritePaddingLogicalFileAndDataset(t *testing.T) {
	obj := FromElements(frameTestDataSet(1, 8), nil)
	obj.Put(core.NewRawElement(core.TagPixelData, core.VROB, []byte{1, 2, 3, 0}))
	var original bytes.Buffer
	if err := WriteFile(&original, &File{Dataset: obj, TransferSyntax: transfer.ExplicitVRLittleEndian}); err != nil {
		t.Fatal(err)
	}
	for _, syntax := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ImplicitVRLittleEndian, transfer.ExplicitVRBigEndian, transfer.DeflatedExplicitVRLittleEndian} {
		for _, alignment := range []uint32{2, 16, 64, 512, 4096} {
			var out bytes.Buffer
			if err := WriteFileWithOptions(&out, &File{Dataset: obj, TransferSyntax: syntax}, WriteFileOptions{Padding: PaddingOptions{Policy: PaddingCreate, Alignment: alignment}, GroupLength: GroupLengthCreate}); err != nil {
				t.Fatal(err)
			}
			b := out.Bytes()
			offset := 144 + int(binary.LittleEndian.Uint32(b[140:144]))
			plain := b[offset:]
			if syntax.Deflated {
				r := flate.NewReader(bytes.NewReader(plain))
				var err error
				plain, err = io.ReadAll(r)
				_ = r.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			if (offset+len(plain))%int(alignment) != 0 {
				t.Fatalf("%s logical length %d", syntax.Name, offset+len(plain))
			}
			back, err := ReadFile(bytes.NewReader(b))
			if err != nil {
				t.Fatal(err)
			}
			var pixel bytes.Buffer
			_, err = back.Dataset.CopyValueTo(core.TagPixelData, &pixel)
			if err != nil || !bytes.Equal(pixel.Bytes(), []byte{1, 2, 3, 0}) {
				t.Fatal("pixel changed")
			}
			var dataset bytes.Buffer
			if err := WriteDataSetWithOptions(&dataset, obj, syntax, WriteDataSetOptions{Padding: PaddingOptions{Policy: PaddingCreate, Alignment: alignment}}); err != nil {
				t.Fatal(err)
			}
			ds := dataset.Bytes()
			if syntax.Deflated {
				r := flate.NewReader(bytes.NewReader(ds))
				ds, err = io.ReadAll(r)
				_ = r.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			if len(ds)%int(alignment) != 0 {
				t.Fatal("dataset alignment used file prefix")
			}
		}
	}
	var after bytes.Buffer
	if err := WriteFile(&after, &File{Dataset: obj, TransferSyntax: transfer.ExplicitVRLittleEndian}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original.Bytes(), after.Bytes()) {
		t.Fatal("source changed")
	}
}

func TestWritePaddingDeferredEqualsMaterialized(t *testing.T) {
	source := largeNestedValuePart10(t, 4096, 2048)
	deferred := openNestedTestFile(t, source, ReadFileOptions{InlineValueBytesThreshold: 1024, DeferNestedValues: true})
	materialized, err := ReadFile(bytes.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	for _, length := range []SequenceLengthPolicy{SequenceLengthUndefined, SequenceLengthDefined} {
		for _, group := range []GroupLengthPolicy{GroupLengthPreserve, GroupLengthCreate} {
			opts := WriteFileOptions{SequenceLength: length, GroupLength: group, Padding: PaddingOptions{Policy: PaddingCreate, Alignment: 512, ItemAlignment: 64}}
			var a, b bytes.Buffer
			if err := WriteFileWithOptions(&a, deferred, opts); err != nil {
				t.Fatal(err)
			}
			if err := WriteFileWithOptions(&b, materialized, opts); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(a.Bytes(), b.Bytes()) {
				t.Fatal("deferred output differs")
			}
		}
	}
	var unchanged bytes.Buffer
	if err := WriteFile(&unchanged, deferred); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(source, unchanged.Bytes()) {
		t.Fatal("deferred source changed")
	}
}

func TestWritePaddingInvalidBeforeOutput(t *testing.T) {
	file := &File{Dataset: FromElements(frameTestDataSet(1, 8), nil), TransferSyntax: transfer.ExplicitVRLittleEndian}
	for _, padding := range []PaddingOptions{{Policy: 99}, {Policy: PaddingCreate, Alignment: 3}, {Policy: PaddingRemove, ItemAlignment: 2}, {MaxAddedBytes: -1}} {
		var out bytes.Buffer
		if err := WriteFileWithOptions(&out, file, WriteFileOptions{Padding: padding}); err == nil || out.Len() != 0 {
			t.Fatal("invalid padding wrote output")
		}
	}
	before := append([]core.Element(nil), file.Dataset.Elements()...)
	var out bytes.Buffer
	if err := WriteFileWithOptions(&out, file, WriteFileOptions{Padding: PaddingOptions{Policy: PaddingCreate, Alignment: 4294967294}}); err == nil {
		t.Fatal("unbounded padding accepted")
	}
	if int64(out.Len()) > DefaultMaxPaddingBytes {
		t.Fatal("oversized output")
	}
	if !reflect.DeepEqual(before, file.Dataset.Elements()) {
		t.Fatal("budget error changed source")
	}
}
