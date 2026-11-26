package encapimage

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/builtin"
)

// TestFromJPEGMatchesDCMTK checks each encapsulated fixture with DCMTK
// 3.7.0: dcmdump must read it without a warning, dcmdjpeg must decode it
// (lossless ones to the samples the built-in decoder returns), and, for the
// processes img2dcm accepts, the Image Pixel attributes and the encapsulated
// bitstream must equal img2dcm's while every attribute img2dcm writes is
// present. Enable with DICOMGO_DCMTK_INTEGRATION=1.
func TestFromJPEGMatchesDCMTK(t *testing.T) {
	dcmdump := dcmtkoracle.Tool(t, "dcmdump")
	dcmdjpeg := dcmtkoracle.Tool(t, "dcmdjpeg")
	img2dcm := dcmtkoracle.Tool(t, "img2dcm")
	registry, err := builtin.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		file    string
		sop     string
		img2dcm []string // nil: img2dcm does not take this process
	}{
		{"baseline_gray.jpg", SecondaryCaptureImageStorage, []string{}},
		{"baseline_444.jpg", SecondaryCaptureImageStorage, []string{}},
		{"baseline_422.jpg", VLPhotographicImageStorage, []string{"-vlp"}},
		{"extended12_gray.jpg", SecondaryCaptureImageStorage, []string{}},
		{"progressive.jpg", VLPhotographicImageStorage, []string{"-vlp"}},
		{"lossless_sv1_gray16.jpg", SecondaryCaptureImageStorage, nil},
		{"lossless_p4_rgb.jpg", VLPhotographicImageStorage, nil},
		// img2dcm removes the Adobe APP14 this conversion keeps.
		{"baseline_rgb.jpg", SecondaryCaptureImageStorage, nil},
	}
	dir := t.TempDir()
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			jpegPath := filepath.Join(dir, tc.file)
			data := readFixture(t, tc.file)
			if err := os.WriteFile(jpegPath, data, 0o600); err != nil {
				t.Fatal(err)
			}
			file, _, err := FromJPEG(withMetadata(data), JPEGOptions{SOPClassUID: tc.sop, AllowRetiredProgressive: true})
			if err != nil {
				t.Fatal(err)
			}
			ours := filepath.Join(dir, tc.file+".dcm")
			writeDICOM(t, ours, file)
			if out := dcmtkoracle.Run(t, dcmtkoracle.Accepted, dcmdump, "-q", "-dc", ours); bytes.Contains(out, []byte("W: ")) || bytes.Contains(out, []byte("E: ")) {
				t.Fatalf("dcmdump:\n%s", out)
			}
			decoded := filepath.Join(dir, tc.file+".native.dcm")
			// Lossy processes are only checked for the decoded sample count.
			tolerance := "decoded sample count only (lossy samples not compared)"
			if strings.HasPrefix(tc.file, "lossless") {
				tolerance = dcmtkoracle.Exact
			}
			dcmtkoracle.Run(t, tolerance, dcmdjpeg, ours, decoded)
			native, err := object.OpenFile(decoded)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = native.Close() }()
			dcmtkSamples, _ := native.Dataset.GetRaw(tagPixelData)
			want := fixtureRows * fixtureColumns * getUS(t, file.Dataset, tagSamplesPerPixel) * getUS(t, file.Dataset, tagBitsAllocated) / 8
			if len(dcmtkSamples) < want {
				t.Fatalf("dcmdjpeg returned %d bytes, want %d", len(dcmtkSamples), want)
			}
			if strings.HasPrefix(tc.file, "lossless") {
				ownNative, err := pixeldata.DecompressFile(roundTrip(t, file), pixeldata.DecompressOptions{Registry: registry})
				if err != nil {
					t.Fatal(err)
				}
				own, _ := ownNative.Dataset.GetRaw(tagPixelData)
				if !bytes.Equal(own[:want], dcmtkSamples[:want]) {
					t.Fatal("lossless samples differ from dcmdjpeg")
				}
			}
			if tc.img2dcm == nil {
				return
			}
			reference := filepath.Join(dir, tc.file+".img2dcm.dcm")
			args := append(append([]string{}, tc.img2dcm...), "-rc", jpegPath, reference)
			dcmtkoracle.Run(t, "Image Pixel attributes and bitstream identical (JFIF APP0 and padding excepted)", img2dcm, args...)
			ref, err := object.OpenFile(reference)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = ref.Close() }()
			got := roundTrip(t, file)
			if ref.TransferSyntax.UID != got.TransferSyntax.UID {
				t.Fatalf("transfer syntax %s, img2dcm %s", got.TransferSyntax.UID, ref.TransferSyntax.UID)
			}
			for _, element := range ref.Dataset.Elements() {
				tag := element.Tag()
				if _, ok := got.Dataset.Get(tag); !ok {
					t.Errorf("img2dcm writes %s, this conversion does not", tag)
				}
				if tag.Group != 0x0028 || tag == tagLossyCompressionRatio {
					continue
				}
				theirs, _ := ref.Dataset.GetRaw(tag)
				mine, _ := got.Dataset.GetRaw(tag)
				if !bytes.Equal(bytes.TrimRight(theirs, " \x00"), bytes.TrimRight(mine, " \x00")) {
					t.Errorf("%s = %q, img2dcm %q", tag, mine, theirs)
				}
			}
			// img2dcm also removes the JFIF APP0 this conversion keeps, and
			// writes a one-entry zero offset table instead of an empty one.
			refPixel, err := pixeldata.ExtractView(ref.Dataset)
			if err != nil || len(refPixel.Sequence.Fragments) != 1 {
				t.Fatalf("img2dcm pixel data: %v", err)
			}
			if !bytes.Equal(withoutJFIF(fragment(t, got)), bytes.TrimRight(refPixel.Sequence.Fragments[0], "\x00")) {
				t.Fatal("encapsulated bitstream differs from img2dcm")
			}
		})
	}
}

func writeDICOM(t *testing.T, path string, file *object.File) {
	t.Helper()
	var buf bytes.Buffer
	if err := object.WriteFile(&buf, file); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// withoutJFIF drops a leading JFIF APP0 segment and the pad byte.
func withoutJFIF(stream []byte) []byte {
	stream = bytes.TrimRight(stream, "\x00")
	if len(stream) > 6 && stream[2] == 0xff && stream[3] == 0xe0 && bytes.HasPrefix(stream[6:], []byte("JFIF\x00")) {
		length := int(stream[4])<<8 | int(stream[5])
		return append(append([]byte{}, stream[:2]...), stream[4+length:]...)
	}
	return stream
}
