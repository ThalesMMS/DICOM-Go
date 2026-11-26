package vendors

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// gdcmRLE2ImgMD5 is the MD5 of the Pixel Data GDCM's rle2img example
// (GDCM 2cd05d132, built out of tree) writes for the compressed stream of
// ELSCINT1_PMSCT_RLE1.dcm of gdcmData 9b38ac7a.
const gdcmRLE2ImgMD5 = "bf7371ab8dab0951e2bfa35da89acb3b"

// TestElscintPixelsMatchGDCM decodes the PMSCT_RLE1 file of gdcmData and
// compares the frame with GDCM's rle2img output: the recorded MD5, and the
// tool's output itself when DICOMGO_GDCM_RLE2IMG names the example binary.
// Enable with DICOMGO_GDCMDATA_ROOT.
func TestElscintPixelsMatchGDCM(t *testing.T) {
	root := os.Getenv("DICOMGO_GDCMDATA_ROOT")
	if root == "" {
		t.Skip("opt-in: DICOMGO_GDCMDATA_ROOT (and DICOMGO_GDCM_RLE2IMG for the live oracle)")
	}
	path := filepath.Join(root, "ELSCINT1_PMSCT_RLE1.dcm")
	file, err := object.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	pixels, err := ElscintPixelDataOf(file.Dataset, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if pixels.Compression != ElscintPMSCTRLE1 || pixels.Source.Group != 0x07A1 || len(pixels.Frame) != 256*256*2 {
		t.Fatalf("compression %s from %s, %d bytes", pixels.Compression, pixels.Source, len(pixels.Frame))
	}
	if file.Dataset.Has(core.TagPixelData) {
		t.Fatal("the object gained Pixel Data")
	}
	sum := md5.Sum(pixels.Frame)
	got := hex.EncodeToString(sum[:])
	t.Logf("PMSCT_RLE1 frame MD5 %s", got)
	if got != gdcmRLE2ImgMD5 {
		t.Errorf("frame MD5 %s, GDCM rle2img %s", got, gdcmRLE2ImgMD5)
	}
	tool := os.Getenv("DICOMGO_GDCM_RLE2IMG")
	if tool == "" {
		return
	}
	// rle2img does not find the private element as this file writes it and
	// exits; it also reads the stream from Pixel Data, so it gets the same
	// bytes there.
	stream, ok := file.Dataset.GetRaw(pixels.Source)
	if !ok {
		t.Fatal("no compressed stream")
	}
	stream = bytes.Clone(stream)
	variant := file.Dataset
	variant.Remove(pixels.Source)
	variant.Put(core.NewRawElement(core.TagPixelData, core.VROB, stream))
	input := filepath.Join(t.TempDir(), "standard.dcm")
	var encoded bytes.Buffer
	if err := object.WriteFile(&encoded, &object.File{Dataset: variant, TransferSyntax: file.TransferSyntax}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(input, encoded.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "rle2img.dcm")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if out, err := exec.CommandContext(ctx, tool, input, output).CombinedOutput(); err != nil {
		t.Fatalf("rle2img: %v: %s", err, out)
	}
	reference, err := object.OpenFile(output)
	if err != nil {
		t.Fatal(err)
	}
	defer reference.Close()
	want, ok := reference.Dataset.GetRaw(core.TagPixelData)
	if !ok || !bytes.Equal(pixels.Frame, want) {
		t.Fatalf("frame differs from rle2img's Pixel Data (%d and %d bytes)", len(pixels.Frame), len(want))
	}
}

// runGDCMExample writes dataset, runs a GDCM example on it and returns the
// Pixel Data of its output.
func runGDCMExample(t *testing.T, tool string, dataset *object.Object) []byte {
	t.Helper()
	dir := t.TempDir()
	input, output := filepath.Join(dir, "in.dcm"), filepath.Join(dir, "out.dcm")
	dataset.Put(core.NewRawElement(core.NewTag(0x0008, 0x0016), core.VRUI, []byte("1.2.840.10008.5.1.4.1.1.7\x00")))
	dataset.Put(core.NewRawElement(core.NewTag(0x0008, 0x0018), core.VRUI, []byte("1.2.826.0.1.3680043.10.1.1144")))
	var encoded bytes.Buffer
	if err := object.WriteFile(&encoded, &object.File{Dataset: dataset, TransferSyntax: transfer.ExplicitVRLittleEndian}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(input, encoded.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if out, err := exec.CommandContext(ctx, tool, input, output).CombinedOutput(); err != nil {
		t.Fatalf("%s: %v: %s", filepath.Base(tool), err, out)
	}
	reference, err := object.OpenFile(output)
	if err != nil {
		t.Fatal(err)
	}
	defer reference.Close()
	want, _ := reference.Dataset.GetRaw(core.TagPixelData)
	return bytes.Clone(want)
}

// TestElscintSyntheticStreamsMatchGDCMExamples decodes synthetic PMSCT_RLE1
// and PMSCT_RGB1 streams with ElscintPixelDataOf and with GDCM's rle2img and
// pmsct_rgb1 examples (paths in DICOMGO_GDCM_RLE2IMG and
// DICOMGO_GDCM_PMSCT_RGB1) and requires the same samples. rle2img reads the
// stream from Pixel Data; pmsct_rgb1 from (07A1,xx0A).
func TestElscintSyntheticStreamsMatchGDCMExamples(t *testing.T) {
	rle2img, rgb1 := os.Getenv("DICOMGO_GDCM_RLE2IMG"), os.Getenv("DICOMGO_GDCM_PMSCT_RGB1")
	if rle2img == "" && rgb1 == "" {
		t.Skip("opt-in: DICOMGO_GDCM_RLE2IMG and/or DICOMGO_GDCM_PMSCT_RGB1 name the GDCM example binaries")
	}
	if rle2img != "" {
		const rows, columns = 37, 53
		samples := make([]uint16, rows*columns)
		for i := range samples {
			samples[i] = uint16(i*i%4093 + i%11)
			if i/columns%6 == 1 {
				samples[i] = 0xA5A5
			}
			if i%101 == 50 {
				samples[i] = 0x5A00 + uint16(i%3)
			}
		}
		dataset := elscintObject(ElscintPMSCTRLE1, encodePMSCTRLE1(samples), rows, columns, 1, 16, true)
		pixels, err := ElscintPixelDataOf(dataset, Limits{})
		if err != nil {
			t.Fatal(err)
		}
		// The odd-length stream ends with a pad byte: rle2img keeps the
		// sample it makes after the frame, ElscintPixelDataOf drops it.
		want := runGDCMExample(t, rle2img, dataset)
		if len(want) != len(pixels.Frame)+2 || !bytes.Equal(pixels.Frame, want[:len(pixels.Frame)]) || !bytes.Equal(pixels.Frame, le16(samples...)) {
			t.Fatalf("PMSCT_RLE1: frame differs from rle2img (%d and %d bytes)", len(pixels.Frame), len(want))
		}
	}
	if rgb1 != "" {
		const rows, columns = 9, 14
		var stream []byte
		gray := true
		width := func() int {
			if gray {
				return 1
			}
			return 3
		}
		for p, step := 0, 0; p < rows*columns; step++ {
			switch step % 7 {
			case 0:
				stream = append(stream, 0x81) // toggle and zero
				gray = !gray
			case 1:
				stream = append(stream, []byte{0x05, 0xFB, 0x10}[:width()]...) // difference
				p++
			case 2:
				stream = append(stream, 0x82) // escaped codes as differences
				stream = append(stream, []byte{0x81, 0x83, 0x82}[:width()]...)
				p++
			case 3:
				n := min(5, rows*columns-p)
				stream = append(stream, 0x83, byte(n)) // repeat
				p += n
			default:
				stream = append(stream, []byte{byte(step), byte(7 * step), 0x40}[:width()]...)
				if stream[len(stream)-width()] >= 0x81 && stream[len(stream)-width()] <= 0x83 {
					stream[len(stream)-width()] = 0x11
				}
				p++
			}
		}
		stream = append(stream, 0x00) // value padding after the last pixel
		for _, planar := range []uint16{0, 1} {
			dataset := elscintObject(ElscintPMSCTRGB1, stream, rows, columns, 3, 8, false)
			dataset.Put(core.NewRawElement(core.NewTag(0x0028, 0x0006), core.VRUS, le16(planar)))
			pixels, err := ElscintPixelDataOf(dataset, Limits{})
			if err != nil {
				t.Fatal(err)
			}
			if want := runGDCMExample(t, rgb1, dataset); !bytes.Equal(pixels.Frame, want) {
				t.Fatalf("PMSCT_RGB1 planar=%d: frame differs from pmsct_rgb1 (%d and %d bytes)", planar, len(pixels.Frame), len(want))
			}
		}
	}
}
