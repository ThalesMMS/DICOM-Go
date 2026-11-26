package encapimage

import (
	"bytes"
	"encoding/binary"
	"errors"
	"github.com/ThalesMMS/dicom-go/internal/commontest"
	"image/jpeg"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/builtin"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// The fixtures are 32x24 synthetic gradients (see testdata/README.md).
const fixtureColumns, fixtureRows = 32, 24

func readFixture(t testing.TB, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func getUS(t *testing.T, ds *object.Object, tag core.Tag) int {
	t.Helper()
	raw, ok := ds.GetRaw(tag)
	if !ok || len(raw) != 2 {
		t.Fatalf("%s missing or not US: %v", tag, raw)
	}
	return int(binary.LittleEndian.Uint16(raw))
}

func getString(ds *object.Object, tag core.Tag) string {
	value, _ := ds.GetString(tag)
	return value
}

func fragment(t *testing.T, file *object.File) []byte {
	t.Helper()
	pixel, err := pixeldata.ExtractView(file.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	if !pixel.Encapsulated || len(pixel.Sequence.Fragments) != 1 || len(pixel.Sequence.OffsetTable) != 0 {
		t.Fatalf("pixel data is not one fragment with an empty offset table: %+v", pixel.Sequence)
	}
	return pixel.Sequence.Fragments[0]
}

func TestFromJPEGReportsOnlyGeneratedStudyUID(t *testing.T) {
	for _, tc := range []struct {
		name, sourceUID, explicitUID string
		generated                    bool
	}{
		{"inherited", "1.2.3", "", false},
		{"generated", "", "", true},
		{"explicit", "1.2.3", "1.2.4", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := commontest.Source()
			source.Remove(tagStudyInstanceUID)
			if tc.sourceUID != "" {
				source.Put(stringElement(tagStudyInstanceUID, core.VRUI, tc.sourceUID))
			}
			file, report, err := FromJPEG(readFixture(t, "baseline_gray.jpg"), JPEGOptions{
				StudyInstanceUID: tc.explicitUID, Common: object.DerivedCommon{Source: source},
			})
			if err != nil {
				t.Fatal(err)
			}
			uid := getString(file.Dataset, tagStudyInstanceUID)
			want := tc.explicitUID
			if want == "" {
				want = tc.sourceUID
			}
			if uid == "" || (want != "" && uid != want) || slices.Contains(report.GeneratedUIDs, "StudyInstanceUID") != tc.generated {
				t.Fatalf("Study Instance UID %q, generated %v", uid, report.GeneratedUIDs)
			}
		})
	}
}

func TestFromJPEGMapsProcessAndPixelModule(t *testing.T) {
	cases := []struct {
		file        string
		syntax      string
		process     string
		photometric string
		subsampling string
		samples     int
		allocated   int
		stored      int
		lossy       string
	}{
		{"baseline_gray.jpg", transfer.JPEGBaseline.UID, "baseline", "MONOCHROME2", "", 1, 8, 8, "01"},
		{"baseline_444.jpg", transfer.JPEGBaseline.UID, "baseline", "YBR_FULL_422", "4:4:4", 3, 8, 8, "01"},
		{"baseline_422.jpg", transfer.JPEGBaseline.UID, "baseline", "YBR_FULL_422", "4:2:2", 3, 8, 8, "01"},
		// cjpeg -rgb writes an Adobe APP14 with transform 0.
		{"baseline_rgb.jpg", transfer.JPEGBaseline.UID, "baseline", "RGB", "4:4:4", 3, 8, 8, "01"},
		{"extended12_gray.jpg", transfer.JPEGExtended.UID, "extended", "MONOCHROME2", "", 1, 16, 12, "01"},
		{"lossless_sv1_gray16.jpg", transfer.JPEGLosslessSV1.UID, "lossless", "MONOCHROME2", "", 1, 16, 16, "00"},
		{"lossless_p4_rgb.jpg", transfer.JPEGLosslessNonHierarchical.UID, "lossless", "RGB", "4:4:4", 3, 8, 8, "00"},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			data := readFixture(t, tc.file)
			if common, _, err := FromJPEG(data, JPEGOptions{Common: object.DerivedCommon{Source: commontest.Source()}}); err != nil {
				t.Fatalf("FromJPEG with common modules: %v", err)
			} else {
				commontest.Check(t, common.Dataset, false)
			}
			file, report, err := FromJPEG(data, JPEGOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if file.TransferSyntax.UID != tc.syntax || report.TransferSyntaxUID != tc.syntax || report.Process != tc.process {
				t.Fatalf("syntax %s, report %+v", file.TransferSyntax.UID, report)
			}
			ds := file.Dataset
			if got := getString(ds, tagPhotometric); got != tc.photometric || report.PhotometricInterpretation != got || report.Subsampling != tc.subsampling {
				t.Fatalf("photometric %s, report %+v", got, report)
			}
			if getUS(t, ds, tagRows) != fixtureRows || getUS(t, ds, tagColumns) != fixtureColumns ||
				getUS(t, ds, tagSamplesPerPixel) != tc.samples || getUS(t, ds, tagBitsAllocated) != tc.allocated ||
				getUS(t, ds, tagBitsStored) != tc.stored || getUS(t, ds, tagHighBit) != tc.stored-1 ||
				getUS(t, ds, tagPixelRepresentation) != 0 {
				t.Fatal("image pixel module disagrees with the frame header")
			}
			if _, ok := ds.GetRaw(tagPlanarConfiguration); ok != (tc.samples == 3) {
				t.Fatalf("planar configuration present=%v for %d samples", ok, tc.samples)
			}
			if got := getString(ds, tagLossyImageCompression); got != tc.lossy {
				t.Fatalf("lossy image compression %q", got)
			}
			if _, ok := ds.Get(tagLossyCompressionRatio); ok != (tc.lossy == "01") {
				t.Fatalf("lossy ratio present=%v", ok)
			}
			// Only the JFIF and Adobe headers are present: nothing is removed
			// and the bitstream is copied byte for byte.
			got := fragment(t, file)
			if report.Padded {
				got = got[:len(got)-1]
			}
			if !bytes.Equal(got, data) || len(report.RemovedSegments) != 0 || report.TrailingBytesRemoved != 0 {
				t.Fatalf("bitstream changed: report %+v", report)
			}
		})
	}
}

// TestFromJPEGDecodes decodes every encapsulated fixture with the built-in
// codecs: lossless ones must return the source samples exactly, the others
// what image/jpeg (8-bit) decodes from the input file.
func TestFromJPEGDecodes(t *testing.T) {
	registry, err := builtin.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"baseline_gray.jpg", "baseline_444.jpg", "baseline_422.jpg", "baseline_rgb.jpg", "extended12_gray.jpg", "lossless_sv1_gray16.jpg", "lossless_p4_rgb.jpg"} {
		t.Run(name, func(t *testing.T) {
			data := readFixture(t, name)
			file, _, err := FromJPEG(data, JPEGOptions{})
			if err != nil {
				t.Fatal(err)
			}
			native, err := pixeldata.DecompressFile(roundTrip(t, file), pixeldata.DecompressOptions{Registry: registry})
			if err != nil {
				t.Fatal(err)
			}
			samples, _ := native.Dataset.GetRaw(tagPixelData)
			switch name {
			case "lossless_sv1_gray16.jpg":
				for i := range fixtureRows * fixtureColumns {
					if got, want := binary.LittleEndian.Uint16(samples[2*i:]), grayGradient(i, 65535); int(got) != want {
						t.Fatalf("sample %d = %d, want %d", i, got, want)
					}
				}
			case "lossless_p4_rgb.jpg":
				for i := range fixtureRows * fixtureColumns {
					if want := colorGradient(i); !bytes.Equal(samples[3*i:3*i+3], want[:]) {
						t.Fatalf("pixel %d = %v, want %v", i, samples[3*i:3*i+3], want)
					}
				}
			case "baseline_gray.jpg":
				img, err := jpeg.Decode(bytes.NewReader(data))
				if err != nil {
					t.Fatal(err)
				}
				for y := range fixtureRows {
					for x := range fixtureColumns {
						r, _, _, _ := img.At(x, y).RGBA()
						if got := samples[y*fixtureColumns+x]; got != byte(r>>8) {
							t.Fatalf("(%d,%d) = %d, image/jpeg %d", x, y, got, r>>8)
						}
					}
				}
			default:
				bits := getUS(t, file.Dataset, tagBitsAllocated)
				want := fixtureRows * fixtureColumns * getUS(t, file.Dataset, tagSamplesPerPixel) * bits / 8
				if len(samples) != want {
					t.Fatalf("%d native bytes, want %d", len(samples), want)
				}
			}
		})
	}
}

// grayGradient and colorGradient reproduce the fixture sources.
func grayGradient(i, maxValue int) int {
	x, y := i%fixtureColumns, i/fixtureColumns
	return (x*37 + y*101) * maxValue / (fixtureColumns*37 + fixtureRows*101) % (maxValue + 1)
}

func colorGradient(i int) [3]byte {
	x, y := i%fixtureColumns, i/fixtureColumns
	return [3]byte{byte(x * 8), byte(y * 10), byte(x * y)}
}

func roundTrip(t *testing.T, file *object.File) *object.File {
	t.Helper()
	var buf bytes.Buffer
	if err := object.WriteFile(&buf, file); err != nil {
		t.Fatal(err)
	}
	out, err := object.ReadFile(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func segment(marker byte, payload []byte) []byte {
	out := []byte{0xff, marker, 0, 0}
	binary.BigEndian.PutUint16(out[2:], uint16(len(payload)+2))
	return append(out, payload...)
}

// withMetadata inserts personal-looking APPn and COM segments after SOI and
// between the tables, and appends bytes after EOI.
func withMetadata(data []byte, iccChunks ...[]byte) []byte {
	const canary = "PATIENT^CANARY"
	var inserted []byte
	inserted = append(inserted, segment(0xe1, []byte("Exif\x00\x00"+canary+" GPS 12.34"))...)
	inserted = append(inserted, segment(0xe1, []byte("http://ns.adobe.com/xap/1.0/\x00<x:xmpmeta>"+canary+"</x:xmpmeta>"))...)
	for i, chunk := range iccChunks {
		inserted = append(inserted, segment(0xe2, append([]byte{'I', 'C', 'C', '_', 'P', 'R', 'O', 'F', 'I', 'L', 'E', 0, byte(i + 1), byte(len(iccChunks))}, chunk...))...)
	}
	inserted = append(inserted, segment(0xed, []byte("Photoshop 3.0\x00"+canary))...)
	inserted = append(inserted, segment(0xe5, []byte(canary+" vendor"))...)
	inserted = append(inserted, segment(0xfe, []byte("Comment "+canary))...)
	out := append([]byte{}, data[:2]...)
	out = append(out, inserted...)
	out = append(out, data[2:]...)
	return append(out, []byte("\xff\xd8 trailer "+canary)...)
}

func TestFromJPEGStripsPersonalSegments(t *testing.T) {
	data := readFixture(t, "baseline_444.jpg")
	iccA, iccB := bytes.Repeat([]byte{0x11}, 7), bytes.Repeat([]byte{0x22}, 8)
	input := withMetadata(data, iccA, iccB)
	file, report, err := FromJPEG(input, JPEGOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := fragment(t, file)
	if report.Padded {
		got = got[:len(got)-1]
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("stripped stream differs from the original (%d vs %d bytes)", len(got), len(data))
	}
	want := []RemovedSegment{
		{Marker: "APP1", Identifier: "Exif", Count: 1},
		{Marker: "APP1", Identifier: "XMP", Count: 1},
		{Marker: "APP2", Identifier: "ICC_PROFILE", Count: 2},
		{Marker: "APP13", Identifier: "Photoshop", Count: 1},
		{Marker: "APP5", Identifier: "", Count: 1},
		{Marker: "COM", Identifier: "", Count: 1},
	}
	if len(report.RemovedSegments) != len(want) {
		t.Fatalf("removed %+v", report.RemovedSegments)
	}
	for i, w := range want {
		r := report.RemovedSegments[i]
		if r.Marker != w.Marker || r.Identifier != w.Identifier || r.Count != w.Count || r.Bytes <= 4 {
			t.Fatalf("removed[%d] = %+v, want %+v", i, r, w)
		}
	}
	if report.TrailingBytesRemoved != len("\xff\xd8 trailer PATIENT^CANARY") {
		t.Fatalf("trailing %d", report.TrailingBytesRemoved)
	}
	if icc, _ := file.Dataset.GetRaw(tagICCProfile); !report.ICCProfileCopied || !bytes.Equal(icc, append(append(append([]byte{}, iccA...), iccB...), 0)) {
		t.Fatalf("ICC profile %v copied=%v", icc, report.ICCProfileCopied)
	}
	var buf bytes.Buffer
	if err := object.WriteFile(&buf, file); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(buf.Bytes(), []byte("CANARY")) {
		t.Fatal("personal segment content reached the DICOM file")
	}

	file, report, err = FromJPEG(input, JPEGOptions{DiscardICCProfile: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := file.Dataset.GetRaw(tagICCProfile); ok || report.ICCProfileCopied {
		t.Fatal("ICC profile copied although discarded")
	}
}

func TestFromJPEGKeepsSegmentsOnRequest(t *testing.T) {
	data := readFixture(t, "baseline_gray.jpg")
	input := withMetadata(data)
	eoi := bytes.LastIndex(input, []byte{0xff, 0xd9}) + 2
	file, report, err := FromJPEG(input, JPEGOptions{KeepAPPn: true, KeepComments: true})
	if err != nil {
		t.Fatal(err)
	}
	got := fragment(t, file)
	if report.Padded {
		got = got[:len(got)-1]
	}
	if !bytes.Equal(got, input[:eoi]) || len(report.RemovedSegments) != 0 {
		t.Fatalf("kept stream differs; report %+v", report)
	}
	// Comments alone.
	_, report, err = FromJPEG(input, JPEGOptions{KeepComments: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range report.RemovedSegments {
		if r.Marker == "COM" {
			t.Fatal("COM removed although kept")
		}
	}
}

func TestFromJPEGIODAttributes(t *testing.T) {
	data := readFixture(t, "baseline_422.jpg")
	sc, report, err := FromJPEG(data, JPEGOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.GeneratedUIDs) != 3 {
		t.Fatalf("generated %v", report.GeneratedUIDs)
	}
	ds := sc.Dataset
	for _, tag := range []core.Tag{tagSOPInstanceUID, tagStudyInstanceUID, tagSeriesInstanceUID} {
		if uid := getString(ds, tag); !strings.HasPrefix(uid, "2.25.") || len(uid) > 64 {
			t.Fatalf("%s = %q", tag, uid)
		}
	}
	for tag, want := range map[core.Tag]string{
		tagSOPClassUID: SecondaryCaptureImageStorage, tagModality: "OT", tagConversionType: "WSD",
		tagSeriesNumber: "1", tagInstanceNumber: "1", tagLossyCompressionMethod: "ISO_10918_1",
	} {
		if got := getString(ds, tag); got != want {
			t.Fatalf("%s = %q, want %q", tag, got, want)
		}
	}
	// Type 2 attributes are present even without a value.
	for _, tag := range []core.Tag{tagPatientName, tagPatientID, tagPatientBirthDate, tagPatientSex, tagStudyDate,
		tagStudyTime, tagReferringPhysicianName, tagStudyID, tagAccessionNumber, tagPatientOrientation} {
		if _, ok := ds.Get(tag); !ok {
			t.Fatalf("type 2 %s missing", tag)
		}
	}
	if _, ok := ds.Get(tagImageType); ok {
		t.Fatal("secondary capture writes Image Type only on request")
	}

	vlp, report, err := FromJPEG(data, JPEGOptions{
		SOPClassUID:        VLPhotographicImageStorage,
		SOPInstanceUID:     "1.2.3.4",
		StudyInstanceUID:   "1.2.3",
		SeriesInstanceUID:  "1.2.3.5",
		PatientName:        "Müller^José",
		PatientID:          "ID985",
		ContentDate:        "20260926",
		ContentTime:        "101500",
		BurnedInAnnotation: "no",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.GeneratedUIDs) != 0 {
		t.Fatalf("generated %v with every UID given", report.GeneratedUIDs)
	}
	ds = roundTrip(t, vlp).Dataset
	for tag, want := range map[core.Tag]string{
		tagSOPClassUID: VLPhotographicImageStorage, tagModality: "XC", tagSOPInstanceUID: "1.2.3.4",
		tagSpecificCharacterSet: "ISO_IR 192", tagPatientName: "Müller^José", tagContentDate: "20260926",
		tagContentTime: "101500", tagBurnedInAnnotation: "NO",
	} {
		if got := getString(ds, tag); got != want {
			t.Fatalf("%s = %q, want %q", tag, got, want)
		}
	}
	if types, _ := ds.GetStrings(tagImageType); strings.Join(types, `\`) != `DERIVED\SECONDARY` {
		t.Fatalf("image type %v", types)
	}
	if items, ok := ds.GetSequence(tagAcquisitionContextSeq); !ok || len(items) != 0 {
		t.Fatalf("acquisition context sequence ok=%v items=%d", ok, len(items))
	}
	if _, ok := ds.Get(tagManufacturer); !ok {
		t.Fatal("VL Photographic needs the type 2 Manufacturer")
	}
	if _, ok := ds.Get(tagConversionType); ok {
		t.Fatal("Conversion Type belongs to Secondary Capture")
	}
}

// TestFromJPEGWritesLaterality checks the Type 2C Laterality (0020,0060) of
// the General Series Module: without a Body Part Examined showing it is not
// needed, it is written, empty (unknown) unless the caller knows it.
func TestFromJPEGWritesLaterality(t *testing.T) {
	data := readFixture(t, "baseline_gray.jpg")
	cases := []struct {
		name       string
		sopClass   string
		laterality string
		want       string
	}{
		{"secondary capture unknown", "", "", ""},
		{"secondary capture right", "", "R", "R"},
		{"VL photographic unknown", VLPhotographicImageStorage, "", ""},
		{"VL photographic left", VLPhotographicImageStorage, " l ", "L"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			file, _, err := FromJPEG(data, JPEGOptions{SOPClassUID: tc.sopClass, Laterality: tc.laterality})
			if err != nil {
				t.Fatal(err)
			}
			ds := roundTrip(t, file).Dataset
			if _, ok := ds.Get(tagLaterality); !ok {
				t.Fatal("Laterality missing")
			}
			if got := getString(ds, tagLaterality); got != tc.want {
				t.Fatalf("Laterality = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestFromJPEGVLPhotographicPixels checks the VL Image Module (PS3.3
// C.8.12.1.1.1-2) against every fixture: 8-bit samples only, and lossy color
// only as YBR_FULL_422. Secondary Capture takes every frame VL Photographic
// refuses.
func TestFromJPEGVLPhotographicPixels(t *testing.T) {
	// A lossless frame header declaring 6-bit samples.
	sixBit := readFixture(t, "lossless_p4_rgb.jpg")
	sixBit[bytes.Index(sixBit, []byte{0xff, 0xc3})+4] = 6
	cases := []struct {
		name        string
		data        []byte
		opts        JPEGOptions
		photometric string // VL Photographic result; empty when refused
	}{
		{"baseline gray", readFixture(t, "baseline_gray.jpg"), JPEGOptions{}, "MONOCHROME2"},
		{"baseline 4:4:4", readFixture(t, "baseline_444.jpg"), JPEGOptions{}, "YBR_FULL_422"},
		{"baseline 4:2:2", readFixture(t, "baseline_422.jpg"), JPEGOptions{}, "YBR_FULL_422"},
		{"progressive", readFixture(t, "progressive.jpg"), JPEGOptions{AllowRetiredProgressive: true}, "YBR_FULL_422"},
		{"lossless RGB", readFixture(t, "lossless_p4_rgb.jpg"), JPEGOptions{}, "RGB"},
		{"baseline RGB (Adobe transform 0)", readFixture(t, "baseline_rgb.jpg"), JPEGOptions{}, ""},
		{"extended 12-bit gray", readFixture(t, "extended12_gray.jpg"), JPEGOptions{}, ""},
		{"lossless 16-bit gray", readFixture(t, "lossless_sv1_gray16.jpg"), JPEGOptions{}, ""},
		{"lossless 6-bit RGB", sixBit, JPEGOptions{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := FromJPEG(tc.data, tc.opts); err != nil {
				t.Fatalf("Secondary Capture: %v", err)
			}
			opts := tc.opts
			opts.SOPClassUID = VLPhotographicImageStorage
			file, _, err := FromJPEG(tc.data, opts)
			if tc.photometric == "" {
				if !errors.Is(err, ErrUnsupportedComponents) {
					t.Fatalf("VL Photographic err = %v, want ErrUnsupportedComponents", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("VL Photographic: %v", err)
			}
			ds := file.Dataset
			if got := getString(ds, tagPhotometric); got != tc.photometric {
				t.Fatalf("Photometric Interpretation = %q, want %q", got, tc.photometric)
			}
			for tag, want := range map[core.Tag]int{tagBitsAllocated: 8, tagBitsStored: 8, tagHighBit: 7, tagPixelRepresentation: 0} {
				if got := getUS(t, ds, tag); got != want {
					t.Fatalf("%s = %d, want %d", tag, got, want)
				}
			}
		})
	}
}

func TestFromJPEGRejects(t *testing.T) {
	baseline := readFixture(t, "baseline_gray.jpg")
	eoi := bytes.LastIndex(baseline, []byte{0xff, 0xd9})
	sof0 := bytes.Index(baseline, []byte{0xff, 0xc0})
	twelveBitBaseline := append([]byte{}, baseline...)
	twelveBitBaseline[sof0+4] = 12
	// An Adobe transform flag of 1 declares YCbCr, which lossless JPEG in
	// DICOM does not use.
	losslessYCbCr := readFixture(t, "lossless_p4_rgb.jpg")
	losslessYCbCr[bytes.Index(losslessYCbCr, []byte("Adobe"))+11] = 1
	// A fourth component (CMYK) appended to the frame header.
	color := readFixture(t, "baseline_444.jpg")
	at := bytes.Index(color, []byte{0xff, 0xc0})
	length := int(binary.BigEndian.Uint16(color[at+2:]))
	sof := append([]byte{}, color[at+4:at+2+length]...)
	sof[5] = 4
	sof = append(sof, 4, 0x11, 0)
	cmyk := append(append(append([]byte{}, color[:at]...), segment(0xc0, sof)...), color[at+2+length:]...)
	cases := []struct {
		name string
		data []byte
		opts JPEGOptions
		want error
	}{
		{"empty", nil, JPEGOptions{}, ErrNotJPEG},
		{"png", []byte("\x89PNG\r\n\x1a\n"), JPEGOptions{}, ErrNotJPEG},
		{"no EOI", baseline[:eoi], JPEGOptions{}, ErrTruncatedJPEG},
		{"cut header", baseline[:20], JPEGOptions{}, ErrTruncatedJPEG},
		{"progressive", readFixture(t, "progressive.jpg"), JPEGOptions{}, ErrProgressiveJPEG},
		{"arithmetic", readFixture(t, "arithmetic.jpg"), JPEGOptions{}, ErrUnsupportedJPEGProcess},
		{"lossless YCbCr", losslessYCbCr, JPEGOptions{}, ErrUnsupportedComponents},
		{"12-bit baseline", twelveBitBaseline, JPEGOptions{}, ErrMalformedJPEG},
		{"four components", cmyk, JPEGOptions{}, ErrUnsupportedComponents},
		{"SOP class", baseline, JPEGOptions{SOPClassUID: "1.2.840.10008.5.1.4.1.1.2"}, ErrUnsupportedSOPClass},
		{"burned in", baseline, JPEGOptions{BurnedInAnnotation: "maybe"}, ErrInvalidOption},
		{"laterality", baseline, JPEGOptions{Laterality: "B"}, ErrInvalidOption},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := FromJPEG(tc.data, tc.opts); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestFromJPEGRetiredProgressiveOnRequest(t *testing.T) {
	data := readFixture(t, "progressive.jpg")
	file, report, err := FromJPEG(data, JPEGOptions{AllowRetiredProgressive: true})
	if err != nil {
		t.Fatal(err)
	}
	if file.TransferSyntax.UID != JPEGProgressiveRetired || report.Process != "progressive" || getString(file.Dataset, tagPhotometric) != "YBR_FULL_422" {
		t.Fatalf("syntax %s report %+v", file.TransferSyntax.UID, report)
	}
	out := roundTrip(t, file)
	if out.TransferSyntax.UID != JPEGProgressiveRetired {
		t.Fatalf("read back as %s", out.TransferSyntax.UID)
	}
	got := fragment(t, out)
	if report.Padded {
		got = got[:len(got)-1]
	}
	if !bytes.Equal(got, data) {
		t.Fatal("progressive bitstream changed")
	}
}

func TestFromJPEGPadsOddStreams(t *testing.T) {
	data := readFixture(t, "baseline_gray.jpg")
	odd := append(append([]byte{}, data[:2]...), segment(0xfe, []byte("x"))...)
	odd = append(odd, data[2:]...)
	if len(odd)%2 == 0 {
		odd = append(append(append([]byte{}, data[:2]...), segment(0xfe, []byte("xy"))...), data[2:]...)
	}
	file, report, err := FromJPEG(odd, JPEGOptions{KeepComments: true})
	if err != nil {
		t.Fatal(err)
	}
	got := fragment(t, file)
	if !report.Padded || len(got)%2 != 0 || got[len(got)-1] != 0 || !bytes.Equal(got[:len(got)-1], odd) {
		t.Fatalf("padding: report %+v, %d bytes", report, len(got))
	}
}

func FuzzFromJPEG(f *testing.F) {
	for _, name := range []string{"baseline_gray.jpg", "lossless_p4_rgb.jpg", "progressive.jpg"} {
		f.Add(readFixture(f, name))
	}
	f.Add(withMetadata(readFixture(f, "baseline_gray.jpg"), []byte{1}, []byte{2}))
	f.Fuzz(func(t *testing.T, data []byte) {
		file, report, err := FromJPEG(data, JPEGOptions{AllowRetiredProgressive: true})
		if err != nil {
			return
		}
		if report.PixelDataBytes%2 != 0 || report.PixelDataBytes > len(data)+1 {
			t.Fatalf("fragment of %d bytes from %d", report.PixelDataBytes, len(data))
		}
		var buf bytes.Buffer
		if err := object.WriteFile(&buf, file); err != nil {
			t.Fatal(err)
		}
	})
}
