package pixeldata_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

var (
	lossyTagCompression = core.NewTag(0x0028, 0x2110)
	lossyTagRatio       = core.NewTag(0x0028, 0x2112)
	lossyTagMethod      = core.NewTag(0x0028, 0x2114)
)

func codecFixtureBytes(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("codecfixture", "testdata", "codecfull", path))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func lossyTestDataSet(fragment []byte, extra ...core.Element) *object.Object {
	elements := append(decompressMetadataElements(2, 2), extra...)
	elements = append(elements, dicomtest.NewFragmentSequenceElement(core.TagPixelData, nil, fragment))
	return object.FromElements(elements, std.Dictionary)
}

func TestDetectDataSetLossyStateFromCodestream(t *testing.T) {
	tests := []struct {
		name     string
		fixture  string
		syntax   transfer.Syntax
		want     pixeldata.LossyState
		attrib   string
		fragment []byte
	}{
		{name: "JPEG baseline", fixture: "jpeg-assembly/jpeg-baseline-small.jpg", syntax: transfer.JPEGBaseline,
			want: pixeldata.LossyState{Lossy: true, Evidence: pixeldata.LossyEvidenceTransferSyntax, Method: "ISO_10918_1"}},
		{name: "JPEG extended", fixture: "jpeg-assembly/jpeg-extended-small.jpg", syntax: transfer.JPEGExtended,
			want: pixeldata.LossyState{Lossy: true, Evidence: pixeldata.LossyEvidenceTransferSyntax, Method: "ISO_10918_1"}},
		{name: "JPEG 2000 irreversible ICT", fixture: "jpeg2000/core/jpeg2000-core-rgb8-ict.j2k", syntax: transfer.JPEG2000,
			want: pixeldata.LossyState{Lossy: true, Evidence: pixeldata.LossyEvidenceCodestream, Method: "ISO_15444_1"}},
		{name: "JPEG 2000 Part 2 lossy", fixture: "jpeg2000/part2-lossy.j2k", syntax: transfer.JPEG2000Part2,
			want: pixeldata.LossyState{Lossy: true, Evidence: pixeldata.LossyEvidenceCodestream, Method: "ISO_15444_1"}},
		{name: "JPEG 2000 reversible RCT", fixture: "jpeg2000/core/jpeg2000-core-rgb8-rct.j2k", syntax: transfer.JPEG2000},
		{name: "JPEG 2000 Part 2 lossless", fixture: "jpeg2000/part2-lossless.j2k", syntax: transfer.JPEG2000Part2},
		{name: "HTJ2K lossless", fixture: "jpeg2000/htj2k-rpcl-lossless.j2c", syntax: transfer.HTJ2K},
		{name: "JPEG-LS NEAR=0 three scans", fixture: "jpegls-near/near-0-3c-8-129x33-ilv0.jls", syntax: transfer.JPEGLSNearLossless},
		{name: "JPEG-LS NEAR=1", fixture: "jpegls-near/near-1-1c-12-129x33-ilv0.jls", syntax: transfer.JPEGLSNearLossless,
			want: pixeldata.LossyState{Lossy: true, Evidence: pixeldata.LossyEvidenceCodestream, Method: "ISO_14495_1"}},
		{name: "JPEG-LS lossless", fixture: "jpegls/rgb10-129x33-ilv2.jls", syntax: transfer.JPEGLSLossless},
		{name: "attribute on native data", syntax: transfer.ExplicitVRLittleEndian, attrib: "01", fragment: []byte{0},
			want: pixeldata.LossyState{Lossy: true, Evidence: pixeldata.LossyEvidenceAttribute}},
		{name: "attribute 00 does not hide an irreversible codestream", fixture: "jpeg2000/core/jpeg2000-core-rgb8-ict.j2k", syntax: transfer.JPEG2000, attrib: "00",
			want: pixeldata.LossyState{Lossy: true, Evidence: pixeldata.LossyEvidenceCodestream, Method: "ISO_15444_1"}},
		{name: "video", syntax: transfer.MPEG2MPML, fragment: []byte{0, 0, 1, 0xb3},
			want: pixeldata.LossyState{Lossy: true, Evidence: pixeldata.LossyEvidenceTransferSyntax, Method: "ISO_13818_2"}},
		{name: "truncated JPEG 2000", syntax: transfer.JPEG2000, fragment: []byte{0xff, 0x4f, 0xff, 0x51}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fragment := tt.fragment
			if tt.fixture != "" {
				fragment = codecFixtureBytes(t, tt.fixture)
			}
			var extra []core.Element
			if tt.attrib != "" {
				extra = append(extra, dicomtest.NewStringElement(lossyTagCompression, core.VRCS, tt.attrib))
			}
			if got := pixeldata.DetectDataSetLossyState(lossyTestDataSet(fragment, extra...), tt.syntax); got != tt.want {
				t.Fatalf("DetectDataSetLossyState() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func decompressLossyFixture(t *testing.T, fragment []byte, syntax transfer.Syntax, extra ...core.Element) *object.Object {
	t.Helper()
	registry := pixeldata.NewMemoryRegistry()
	if err := registry.RegisterCodec(syntax.UID, fakeDecompressCodec{
		frames: pixeldata.Frames{Rows: 2, Columns: 2, Data: [][]byte{{1, 2, 3, 4}}},
	}); err != nil {
		t.Fatal(err)
	}
	got, _, err := pixeldata.DecompressDataSet(lossyTestDataSet(fragment, extra...), syntax, pixeldata.DecompressOptions{Registry: registry})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestDecompressRecordsLossyStateOfTheSource(t *testing.T) {
	for _, tt := range []struct {
		name    string
		fixture string
		syntax  transfer.Syntax
		method  string
	}{
		{"JPEG baseline", "jpeg-assembly/jpeg-baseline-small.jpg", transfer.JPEGBaseline, "ISO_10918_1"},
		{"JPEG 2000 irreversible", "jpeg2000/core/jpeg2000-core-rgb8-ict.j2k", transfer.JPEG2000, "ISO_15444_1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fragment := codecFixtureBytes(t, tt.fixture)
			got := decompressLossyFixture(t, fragment, tt.syntax)
			if value, _ := got.GetString(lossyTagCompression); value != "01" {
				t.Fatalf("Lossy Image Compression = %q, want 01", value)
			}
			if value, _ := got.GetString(lossyTagMethod); value != tt.method {
				t.Fatalf("Lossy Image Compression Method = %q, want %s", value, tt.method)
			}
			ratios, _ := got.GetStrings(lossyTagRatio)
			if len(ratios) != 1 || ratios[0] == "" {
				t.Fatalf("Lossy Image Compression Ratio = %q, want one ratio", ratios)
			}
			if state := pixeldata.DetectDataSetLossyState(got, transfer.ExplicitVRLittleEndian); !state.Lossy {
				t.Fatal("decompressed output no longer reads as lossy")
			}
		})
	}

	for _, tt := range []struct {
		name    string
		fixture string
		syntax  transfer.Syntax
	}{
		{"JPEG 2000 reversible", "jpeg2000/core/jpeg2000-core-rgb8-rct.j2k", transfer.JPEG2000},
		{"JPEG-LS NEAR=0", "jpegls-near/near-0-1c-8-129x33-ilv0.jls", transfer.JPEGLSNearLossless},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := decompressLossyFixture(t, codecFixtureBytes(t, tt.fixture), tt.syntax)
			for _, tag := range []core.Tag{lossyTagCompression, lossyTagMethod, lossyTagRatio} {
				if got.Has(tag) {
					t.Fatalf("lossless source gained %s", tag)
				}
			}
		})
	}

	t.Run("existing history is kept", func(t *testing.T) {
		got := decompressLossyFixture(t, codecFixtureBytes(t, "jpeg-assembly/jpeg-baseline-small.jpg"), transfer.JPEGBaseline,
			dicomtest.NewStringElement(lossyTagRatio, core.VRDS, "8"),
			dicomtest.NewStringElement(lossyTagMethod, core.VRCS, "ISO_10918_1"),
		)
		if value, _ := got.GetString(lossyTagCompression); value != "01" {
			t.Fatalf("Lossy Image Compression = %q, want 01", value)
		}
		if ratios, _ := got.GetStrings(lossyTagRatio); len(ratios) != 1 || ratios[0] != "8" {
			t.Fatalf("ratio history = %q, want [8]", ratios)
		}
	})
}
