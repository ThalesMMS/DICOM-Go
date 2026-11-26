package dimse

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// A path source re-encodes an object whose Pixel Data is longer than 64 MiB:
// the element is bounded by the source, not by a fixed ceiling.
func TestPathStoreSourceReencodesPixelDataOver64MiB(t *testing.T) {
	if testing.Short() {
		t.Skip("writes an object of 64 MiB")
	}
	const length = 64<<20 + 16
	path := filepath.Join(t.TempDir(), "large.dcm")
	output, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	elements := append(dicomtest.MinimalDataset(), core.NewRawElement(core.TagPixelData, core.VROW, make([]byte, length)))
	writeErr := object.WriteFile(output, &object.File{Dataset: object.FromElements(elements, nil), TransferSyntax: transfer.ImplicitVRLittleEndian})
	if closeErr := output.Close(); writeErr != nil || closeErr != nil {
		t.Fatalf("write %v close %v", writeErr, closeErr)
	}
	source := NewPathStoreSource(path, StorePathSourceOptions{NativePreference: transfer.NativeStoreExplicitLittleEndianFirst})
	opened, err := source.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	var counted countingDiscard
	if err := opened.WriteDataSet(context.Background(), &counted, transfer.ExplicitVRLittleEndian); err != nil {
		t.Fatalf("re-encode in Explicit VR Little Endian: %v", err)
	}
	if counted.n < length {
		t.Fatalf("re-encoded data set of %d bytes", counted.n)
	}
	if !bytes.Equal(counted.head[4:6], []byte("UI")) {
		t.Fatalf("the data set does not start with an Explicit VR element: % X", counted.head)
	}
}

// A path source re-encodes, for C-STORE, an object holding a top-level value
// over the 1 MiB inline threshold of a VR the parser does not defer (OL, OV,
// SV, UV, UC) from Implicit and Explicit VR Little Endian instead of failing
// the parse (#1980).
func TestPathStoreSourceReencodesLargeNonDeferrableValues(t *testing.T) {
	const length = 1<<20 + 64
	for _, tc := range []struct {
		vr  core.VR
		tag core.Tag
	}{
		{core.VROL, core.NewTag(0x0066, 0x0040)}, // Long Primitive Point Index List
		{core.VROV, core.NewTag(0x0072, 0x0081)}, // Selector OV Value
		{core.VRSV, core.NewTag(0x0072, 0x0082)}, // Selector SV Value
		{core.VRUV, core.NewTag(0x0072, 0x0083)}, // Selector UV Value
		{core.VRUC, core.NewTag(0x0072, 0x006F)}, // Selector UC Value
	} {
		littleEndian := make([]byte, length)
		for i := range littleEndian {
			littleEndian[i] = byte(i*37) + 5
		}
		if tc.vr == core.VRUC {
			littleEndian = bytes.Repeat([]byte("UC"), length/2)
		}
		for _, c := range []struct{ source, target transfer.Syntax }{
			{transfer.ImplicitVRLittleEndian, transfer.ExplicitVRLittleEndian},
			{transfer.ExplicitVRLittleEndian, transfer.ImplicitVRLittleEndian},
		} {
			t.Run(string(tc.vr)+"/"+c.source.UID, func(t *testing.T) {
				data, err := dicomtest.Part10File(c.source, append(dicomtest.MinimalDataset(), core.NewRawElement(tc.tag, tc.vr, littleEndian))...)
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(t.TempDir(), "source.dcm")
				if err := os.WriteFile(path, data, 0o600); err != nil {
					t.Fatal(err)
				}
				opened, err := NewPathStoreSource(path).Open(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				defer opened.Close()
				var encoded bytes.Buffer
				if err := opened.WriteDataSet(context.Background(), &encoded, c.target); err != nil {
					t.Fatalf("re-encode in %s: %v", c.target.UID, err)
				}
				parsed, err := object.ReadDataSet(bytes.NewReader(encoded.Bytes()), c.target)
				if err != nil {
					t.Fatal(err)
				}
				element, ok := parsed.Get(tc.tag)
				got, isRaw := element.RawBytes()
				if !ok || !isRaw || !bytes.Equal(got, littleEndian) {
					t.Fatalf("re-encoded %s present=%t raw=%t length %d, want the %d source bytes", tc.vr, ok, isRaw, len(got), length)
				}
			})
		}
	}
}

// countingDiscard counts the bytes written and keeps the first eight.
type countingDiscard struct {
	n    int64
	head []byte
}

func (c *countingDiscard) Write(p []byte) (int, error) {
	if missing := 8 - len(c.head); missing > 0 {
		c.head = append(c.head, p[:min(missing, len(p))]...)
	}
	c.n += int64(len(p))
	return len(p), nil
}
