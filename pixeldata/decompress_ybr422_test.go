package pixeldata_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// A JPEG 2000 codec that upsamples 4:2:2 chroma reports RGB and a lossy
// result (#2028); decompression writes both, even for a reversible syntax.
func TestDecompressJPEG2000YBRFull422CodecReportsLossyRGB(t *testing.T) {
	rgb := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9}
	dataset := func() *object.Object {
		elements := append(decompressColorMetadataElements(1, 3, "YBR_FULL_422"),
			dicomtest.NewFragmentSequenceElement(core.TagPixelData, nil, []byte{0xff, 0x4f, 0xff, 0xd9}))
		return object.FromElements(elements, std.Dictionary)
	}
	registry := pixeldata.NewMemoryRegistry()
	if err := registry.RegisterCodec(transfer.JPEG2000LosslessOnly.UID, fakeDecompressCodec{
		frames: pixeldata.Frames{Rows: 1, Columns: 3, Data: [][]byte{rgb}, PhotometricInterpretation: "RGB", Lossy: true},
	}); err != nil {
		t.Fatal(err)
	}
	got, _, err := pixeldata.DecompressDataSet(dataset(), transfer.JPEG2000LosslessOnly, pixeldata.DecompressOptions{Registry: registry})
	if err != nil {
		t.Fatal(err)
	}
	if value, _ := got.GetString(decompressTagPhotometricInterpretation); value != "RGB" {
		t.Fatalf("Photometric Interpretation = %q, want RGB", value)
	}
	if value, _ := got.GetString(lossyTagCompression); value != "01" {
		t.Fatalf("Lossy Image Compression = %q, want 01", value)
	}
	// The codestream itself is reversible: no compression method or ratio
	// is claimed for the conversion.
	if got.Has(lossyTagMethod) || got.Has(lossyTagRatio) {
		t.Fatal("a conversion-only lossy result gained a compression method or ratio")
	}
	pixel, err := pixeldata.Extract(got)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pixel.Raw, rgb) {
		t.Fatalf("Pixel Data = %v, want %v", pixel.Raw, rgb)
	}

	// Admission charges the expanded RGB frame, not the two samples per pixel
	// of native 4:2:2: 9 bytes, over a limit of 8.
	_, _, err = pixeldata.DecompressDataSet(dataset(), transfer.JPEG2000LosslessOnly, pixeldata.DecompressOptions{
		Registry: registry,
		Limits:   pixeldata.DecompressLimits{MaxNativeBytes: 8},
	})
	var limit *pixeldata.DecompressResourceLimitError
	if !errors.As(err, &limit) || limit.Limit != "MaxNativeBytes" || limit.Actual != 9 {
		t.Fatalf("DecompressDataSet() error = %v, want MaxNativeBytes 9 over 8", err)
	}
}
