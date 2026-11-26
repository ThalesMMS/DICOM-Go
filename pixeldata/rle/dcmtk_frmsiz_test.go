package rle

// Cross-check against DCMTK's uncompressed frame size vectors (tfrmsiz.cc,
// testdata/upstream/dcmtk/frmsiz_vectors.json, #1053): the native frame size
// of pixeldata.Metadata and the Bits Allocated this RLE decoder accepts.
// Divergences name their normative reason; docs/CONFORMANCE.md lists them.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata"
)

// frameSizeDivergences lists the tfrmsiz.cc rows dicom-go answers
// differently, by line.
var frameSizeDivergences = map[int]string{
	71: "Bits Allocated 12: DCMTK packs two samples in three bytes; PS3.5 8.1.1 allows only 1 or a " +
		"multiple of 8, so Metadata.FrameSize allocates whole bytes, validation reports the value " +
		"and packed data is read only with the opt-in Unpack12BitPacked recovery",
	90: "YBR_FULL_422 with full-size Pixel Data: DCMTK infers the size from the data length; " +
		"PS3.3 C.7.6.3.1.2 defines YBR_FULL_422 as two samples per pixel, so FrameSize follows " +
		"the attributes and a mismatching length is reported, not reinterpreted",
}

func TestFrameSizeMatchesDCMTK(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "upstream", "dcmtk", "frmsiz_vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		FrameSizes []struct {
			Line              int            `json:"line"`
			Encoding          string         `json:"encoding"`
			Attributes        map[string]any `json:"attributes"`
			PixelDataWords    *int           `json:"pixel_data_words"`
			DecoderRegistered *bool          `json:"decoder_registered"`
			FrameSize         *int64         `json:"frame_size"`
		} `json:"frame_sizes"`
		DecodedBitsAllocated []struct {
			Line                 int    `json:"line"`
			Encoding             string `json:"encoding"`
			BitsAllocated        int    `json:"bits_allocated"`
			BitsStored           int    `json:"bits_stored"`
			DecodedBitsAllocated int    `json:"decoded_bits_allocated"`
		} `json:"decoded_bits_allocated"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.FrameSizes) < 8 || len(fixture.DecodedBitsAllocated) < 12 {
		t.Fatalf("%d sizes, %d decoded", len(fixture.FrameSizes), len(fixture.DecodedBitsAllocated))
	}
	seen := map[int]bool{}
	for _, row := range fixture.FrameSizes {
		if row.DecoderRegistered != nil && !*row.DecoderRegistered {
			continue // DCMTK codec registration, not a property of the data
		}
		number := func(name string) uint16 { return uint16(row.Attributes[name].(float64)) }
		metadata := pixeldata.Metadata{
			Rows: number("Rows"), Columns: number("Columns"), SamplesPerPixel: number("SamplesPerPixel"),
			BitsAllocated: number("BitsAllocated"), BitsStored: number("BitsStored"),
			HighBit: number("BitsStored") - 1, NumberOfFrames: 1,
			PhotometricInterpretation: row.Attributes["PhotometricInterpretation"].(string),
		}
		got := strconv.FormatInt(metadata.FrameSize(), 10)
		if row.Encoding == "rle_lossless" && validateDecodeMetadata(metadata) != nil {
			got = "refused"
		}
		want := "refused"
		if row.FrameSize != nil {
			want = strconv.FormatInt(*row.FrameSize, 10)
		}
		reason, diverges := frameSizeDivergences[row.Line]
		seen[row.Line] = true
		switch {
		case got != want && !diverges:
			t.Errorf("tfrmsiz.cc:%d: frame size %s, DCMTK %s", row.Line, got, want)
		case got == want && diverges:
			t.Errorf("tfrmsiz.cc:%d: now agrees with DCMTK; remove the divergence %q", row.Line, reason)
		}
	}
	for line := range frameSizeDivergences {
		if !seen[line] {
			t.Errorf("divergence %d names no vector", line)
		}
	}
	// DCMTK's RLE decoder produces the Bits Allocated it is given when it is
	// a multiple of 8 not below Bits Stored, and 0 (unsupported) otherwise.
	// Its native rows only echo Bits Allocated and are not compared.
	for _, row := range fixture.DecodedBitsAllocated {
		if row.Encoding != "rle_lossless" {
			continue
		}
		got := 0
		if supportedBitsAllocated(row.BitsAllocated, 1) && row.BitsStored <= row.BitsAllocated {
			got = row.BitsAllocated
		}
		if got != row.DecodedBitsAllocated {
			t.Errorf("tfrmsiz.cc:%d: RLE decodes Bits Allocated %d/Bits Stored %d as %d, DCMTK %d",
				row.Line, row.BitsAllocated, row.BitsStored, got, row.DecodedBitsAllocated)
		}
	}
}
