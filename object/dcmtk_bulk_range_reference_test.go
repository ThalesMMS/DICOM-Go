package object_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/transfer"
)

type dcmtkByteRange struct{ offset, length int }

// DCMTK dcmdata/tests/tpread.cc (dcmdata_partialElementAccess) exercises
// consecutive, overlapping and random reads across sample widths. The Go
// range API returns the original file bytes, so the expected byte order comes
// from each source syntax. dcmdata/tests/tfhstrm.cc also exercises independent
// loading of items from one file. Each parallel Go worker opens its own handle.
func TestDCMTKDeferredBulkRangePatterns(t *testing.T) {
	const valueBytes = 4096
	sequenceTag := core.NewTag(0x0008, 0x1140)
	values := []struct {
		tag   core.Tag
		vr    core.VR
		width int
	}{
		{core.NewTag(0x0042, 0x0011), core.VROB, 1},
		{core.NewTag(0x0028, 0x6040), core.VRUS, 2},
		{core.NewTag(0x0018, 0x6058), core.VRUL, 4},
		{core.NewTag(0x0018, 0x6054), core.VRFD, 8},
	}
	for _, syntax := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ExplicitVRBigEndian} {
		t.Run(syntax.Name, func(t *testing.T) {
			items := make([]core.DataSet, 2)
			for item := range items {
				for _, tc := range values {
					value := make([]byte, valueBytes)
					for i := 0; i < len(value); i += tc.width {
						// Keep each adjacent byte and each item distinguishable,
						// including inside a numerical sample.
						bits := uint64(i+7)*0x001020304050607 + uint64(item)*0x5040201
						switch tc.width {
						case 1:
							value[i] = byte(bits)
						case 2:
							syntax.ByteOrder.PutUint16(value[i:], uint16(bits))
						case 4:
							syntax.ByteOrder.PutUint32(value[i:], uint32(bits))
						case 8:
							syntax.ByteOrder.PutUint64(value[i:], bits)
						}
					}
					items[item].Elements = append(items[item].Elements, core.NewRawElement(tc.tag, tc.vr, value))
				}
			}
			elements := append(dicomtest.MinimalDataset(), dicomtest.NewSequenceElement(sequenceTag, items...))
			wire, err := dicomtest.Part10File(syntax, elements...)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "ranges.dcm")
			if err := os.WriteFile(path, wire, 0600); err != nil {
				t.Fatal(err)
			}
			for item, ds := range items {
				for _, elem := range ds.Elements {
					name := fmt.Sprintf("item=%d/%s", item, elem.VR())
					t.Run(name, func(t *testing.T) {
						t.Parallel()
						leaf := []parser.BulkDataPathStep{
							{Tag: sequenceTag, ItemIndex: item},
							{Tag: elem.Tag(), ItemIndex: -1},
						}
						want, _ := elem.RawBytes()
						checkDCMTKDeferredRanges(t, path, leaf, want)
					})
				}
			}
		})
	}
}

func checkDCMTKDeferredRanges(t *testing.T, path string, leaf []parser.BulkDataPathStep, want []byte) {
	t.Helper()
	source, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	opts := object.ReadFileOptions{BulkDataDescriptor: &parser.BulkDataDescriptor{
		Rules: []parser.BulkDataRule{{Path: leaf, Threshold: 32}},
	}}
	f, err := object.ReadFileWithOptions(source, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	ref, ok, err := f.Dataset.DeferredBulkDataReference(leaf)
	if !ok || err != nil || ref.Length != int64(len(want)) {
		t.Fatalf("deferred value reference = %v, %t, %v", ref, ok, err)
	}
	before, err := source.Seek(0, io.SeekCurrent)
	if err != nil {
		t.Fatal(err)
	}
	for _, trace := range []struct {
		name   string
		ranges []dcmtkByteRange
	}{
		{"consecutive", dcmtkWalkRanges(len(want), 113, 113)},
		{"overlapping", dcmtkWalkRanges(len(want), 113, 107)},
		{"random", dcmtkRandomRanges(len(want))},
		{"exact-boundaries", []dcmtkByteRange{{0, 0}, {len(want) - 1, 1}, {len(want), 0}, {0, len(want)}}},
	} {
		t.Run(trace.name, func(t *testing.T) {
			var got bytes.Buffer
			for _, r := range trace.ranges {
				got.Reset()
				n, err := f.Dataset.CopyBulkDataRangeTo(context.Background(), leaf, int64(r.offset), int64(r.length), &got)
				if err != nil || n != int64(r.length) || !bytes.Equal(got.Bytes(), want[r.offset:r.offset+r.length]) {
					t.Fatalf("range [%d:%d] = %d bytes, %v", r.offset, r.offset+r.length, n, err)
				}
				after, err := source.Seek(0, io.SeekCurrent)
				if err != nil || before != after {
					t.Fatalf("range replay moved source cursor %d -> %d: %v", before, after, err)
				}
			}
		})
	}
}

func dcmtkWalkRanges(size, width, stride int) []dcmtkByteRange {
	var ranges []dcmtkByteRange
	for offset := 0; offset < size; offset += stride {
		ranges = append(ranges, dcmtkByteRange{offset, min(width, size-offset)})
	}
	return ranges
}

func dcmtkRandomRanges(size int) []dcmtkByteRange {
	rng := rand.New(rand.NewSource(904))
	ranges := make([]dcmtkByteRange, 64)
	for i := range ranges {
		offset := rng.Intn(size)
		ranges[i] = dcmtkByteRange{offset, min(1+rng.Intn(137), size-offset)}
	}
	return ranges
}
