package object_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestRawPixelDataEncoding(t *testing.T) {
	for _, tc := range []struct {
		name         string
		data         []byte
		encapsulated bool
	}{
		{"native", []byte{1, 2, 3, 4}, false},
		{"short", []byte{0xfe, 0xff}, false},
		{"little endian items", []byte{0xfe, 0xff, 0x00, 0xe0, 0, 0, 0, 0}, true},
		{"big endian items", []byte{0xff, 0xfe, 0xe0, 0x00, 0, 0, 0, 0}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := object.FromElements([]core.Element{core.NewRawElement(core.TagPixelData, core.VROB, tc.data)}, nil)
			for _, syntax := range []transfer.Syntax{{}, transfer.ExplicitVRLittleEndian, transfer.DeflatedExplicitVRLittleEndian, transfer.JPEGBaseline} {
				err := obj.CheckPixelDataEncoding(syntax)
				if mismatch := tc.encapsulated != syntax.Encapsulated; errors.Is(err, object.ErrPixelDataEncodingMismatch) != mismatch {
					t.Fatalf("syntax %s: %v, want mismatch %v", syntax.UID, err, mismatch)
				}
			}
		})
	}
}

func TestParsedNativePixelDataIsNotClassifiedBySamplePrefix(t *testing.T) {
	syntax := transfer.ExplicitVRLittleEndian
	fixture := object.FromElements(dicomtest.DatasetWithPixelData(), nil)
	var encoded bytes.Buffer
	if err := object.WriteDataSet(&encoded, fixture, syntax); err != nil {
		t.Fatal(err)
	}
	pixels, _ := fixture.GetRaw(core.TagPixelData)
	nativeBytes := encoded.Bytes()
	if len(pixels) < 4 || !bytes.HasSuffix(nativeBytes, pixels) {
		t.Fatal("fixture does not end with native Pixel Data")
	}
	copy(nativeBytes[len(nativeBytes)-len(pixels):], []byte{0xfe, 0xff, 0x00, 0xe0})
	dataset, err := object.ReadDataSet(bytes.NewReader(nativeBytes), syntax)
	if err != nil {
		t.Fatal(err)
	}
	if err := dataset.CheckPixelDataEncoding(syntax); err != nil {
		t.Errorf("parsed native pixels refused: %v", err)
	}
	if err := dataset.CheckPixelDataEncoding(transfer.JPEGBaseline); !errors.Is(err, object.ErrPixelDataEncodingMismatch) {
		t.Errorf("parsed native pixels under JPEG syntax returned %v, want encoding mismatch", err)
	}
	var roundtrip bytes.Buffer
	if err := object.WriteDataSet(&roundtrip, dataset, syntax); err != nil || !bytes.Equal(roundtrip.Bytes(), nativeBytes) {
		t.Errorf("parsed native dataset did not round-trip byte-for-byte: %v", err)
	}

	// Replacing Pixel Data returns it to the authored-value contract.
	dataset.Put(core.NewRawElement(core.TagPixelData, core.VROB, []byte{0xfe, 0xff, 0x00, 0xe0, 0, 0, 0, 0}))
	if err := dataset.CheckPixelDataEncoding(syntax); !errors.Is(err, object.ErrPixelDataEncodingMismatch) {
		t.Errorf("replaced Pixel Data returned %v, want authored Item-tag validation", err)
	}
}
