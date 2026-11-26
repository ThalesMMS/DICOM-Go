package encapimage

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpeg2000"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpegls"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func sampleMetadata() pixeldata.Metadata {
	return pixeldata.Metadata{Rows: 2, Columns: 2, SamplesPerPixel: 1, BitsAllocated: 8, BitsStored: 8, HighBit: 7, NumberOfFrames: 1, PhotometricInterpretation: "MONOCHROME2"}
}
func TestFromImagesJPEGPayloadAndJPEGFamilyDetection(t *testing.T) {
	data := readFixture(t, "baseline_gray.jpg")
	f, r := imported(t, [][]byte{data}, ImageOptions{})
	payload := fragment(t, part10Roundtrip(t, f))
	if !bytes.Equal(payload[:len(data)], data) || !r.Encapsulated || r.Decoded || r.Format != JPEG || r.TransferSyntaxUID != transfer.JPEGBaseline.UID {
		t.Fatalf("JPEG import: %+v", r)
	}
	spoof := append([]byte{255, 216, 255, 0xe1, 0, 4, 255, 0xf7}, data[2:]...)
	if detectImage(spoof) != JPEG {
		t.Fatal("APP content spoofed JPEG-LS detection")
	}
}
func TestFromImagesJLSActualLosslessAndNearPayloadReadback(t *testing.T) {
	samples := []byte{0, 23, 128, 255}
	for _, near := range []int{0, 2} {
		enc, e := jpegls.NewEncoderWithOptions(jpegls.EncoderOptions{Near: near, AllowLossy: near > 0})
		if e != nil {
			t.Fatal(e)
		}
		encoded, e := enc.EncodeFrame(context.Background(), samples, sampleMetadata())
		if e != nil {
			t.Fatal(e)
		}
		f, r := imported(t, [][]byte{encoded.Data}, ImageOptions{})
		out := part10Roundtrip(t, f)
		payload := fragment(t, out)
		if !bytes.Equal(payload[:len(encoded.Data)], encoded.Data) || r.Decoded || r.Format != JLS {
			t.Fatal("JLS payload changed")
		}
		codec := jpegls.New()
		wantTS := transfer.JPEGLSLossless.UID
		wantLossy := "00"
		if near > 0 {
			codec = jpegls.NewNearLossless()
			wantTS = transfer.JPEGLSNearLossless.UID
			wantLossy = "01"
		}
		if f.TransferSyntax.UID != wantTS || getString(f.Dataset, tagLossyImageCompression) != wantLossy {
			t.Fatal("NEAR metadata mismatch")
		}
		p, e := pixeldata.ExtractView(out.Dataset)
		if e != nil {
			t.Fatal(e)
		}
		frames, e := codec.DecodeContext(context.Background(), p, out.Dataset)
		if e != nil || len(frames.Data) != 1 || len(frames.Data[0]) != len(samples) {
			t.Fatalf("decode: %v", e)
		}
		for i, want := range samples {
			d := int(frames.Data[0][i]) - int(want)
			if d < -near || d > near {
				t.Fatalf("JLS sample %d error %d", i, d)
			}
		}
	}
}
func TestFromImagesJ2KActualHeadersPayloadAndDeclaration(t *testing.T) {
	samples := []byte{0, 23, 128, 255}
	for _, lossless := range []bool{true, false} {
		uid := transfer.JPEG2000LosslessOnly.UID
		opt := jpeg2000.Options{TransferSyntaxUID: uid}
		if !lossless {
			uid = transfer.JPEG2000.UID
			opt = jpeg2000.Options{TransferSyntaxUID: uid, StepSize: 1, AllowLossy: true}
		}
		enc, e := jpeg2000.NewEncoder(opt)
		if e != nil {
			t.Fatal(e)
		}
		coded, e := enc.EncodeFrame(context.Background(), samples, sampleMetadata())
		if e != nil {
			t.Fatal(e)
		}
		f, r := imported(t, [][]byte{coded.Data}, ImageOptions{J2KLossless: lossless})
		got := fragment(t, part10Roundtrip(t, f))
		if !bytes.Equal(got[:len(coded.Data)], coded.Data) || f.TransferSyntax.UID != uid || r.Decoded || r.LosslessDeclared != lossless {
			t.Fatalf("J2K: %+v", r)
		}
		if lossless {
			f, _ = imported(t, [][]byte{coded.Data}, ImageOptions{})
			if f.TransferSyntax.UID != transfer.JPEG2000.UID || getString(f.Dataset, tagLossyImageCompression) != "" {
				t.Fatal("undeclared reversible source falsely described as lossless/lossy")
			}
		} else {
			if f, _, e := FromImages(context.Background(), [][]byte{coded.Data}, ImageOptions{J2KLossless: true}); e == nil || f != nil {
				t.Fatal("irreversible .90 accepted")
			}
		}
	}
}

// Only coding headers are inspected here; these synthetic MPEG packets are
// deliberately not claimed to be decoder-qualified pictures.
func mpegHeaderFixture() []byte {
	es := []byte{0, 0, 1, 0xb3, 1, 0, 16, 0x13, 0, 0, 0, 0, 0, 0, 1, 0xb5, 0x14, 0x8a, 0, 0, 0, 0, 0, 0, 1, 0, 0, 8, 0, 0}
	pack := []byte{0, 0, 1, 0xba, 0x44, 0, 4, 0, 4, 1, 0, 0, 3, 0xf8}
	pes := []byte{0, 0, 1, 0xe0, 0, 0, 0x80, 0, 0}
	binary.BigEndian.PutUint16(pes[4:], uint16(len(es)+3))
	return append(append(append(pack, pes...), es...), 0, 0, 1, 0xb9)
}
func TestFromImagesVideoPreservesContainerAndNeverClaimsDecode(t *testing.T) {
	data := mpegHeaderFixture()
	f, r := imported(t, [][]byte{data}, ImageOptions{AnatomicRegion: &AnatomicRegion{CodeValue: "SYNTHETIC", CodingSchemeDesignator: "99TEST", CodeMeaning: "Synthetic test region"}})
	if r.Decoded || !r.Encapsulated || r.Format != Video || r.Frames != 1 || f.TransferSyntax.UID != transfer.MPEG2MPML.UID || getString(f.Dataset, tagSOPClassUID) != VideoPhotographicImageStorage {
		t.Fatalf("video metadata: %+v", r)
	}
	got := fragment(t, part10Roundtrip(t, f))
	if !bytes.Equal(got[:len(data)], data) {
		t.Fatal("video container changed")
	}
	p, _ := pixeldata.ExtractView(f.Dataset)
	if _, e := pixeldata.DecodeFramesContext(context.Background(), f.TransferSyntax.UID, p, f.Dataset); e == nil {
		t.Fatal("video accepted as still codec decode")
	}
}

func TestFromImagesCompressedMultiFrameSCUsesActualFrameBoundaries(t *testing.T) {
	samples := []byte{0, 23, 128, 255}
	coded, e := jpegls.NewEncoder().EncodeFrame(context.Background(), samples, sampleMetadata())
	if e != nil {
		t.Fatal(e)
	}
	f, r := imported(t, [][]byte{coded.Data, coded.Data}, ImageOptions{})
	if r.Frames != 2 || getString(f.Dataset, tagSOPClassUID) != MultiFrameGrayscaleByteSC {
		t.Fatal("wrong MF class")
	}
	out := part10Roundtrip(t, f)
	p, e := pixeldata.ExtractView(out.Dataset)
	if e != nil {
		t.Fatal(e)
	}
	if len(p.Sequence.Fragments) != 2 || len(p.Sequence.OffsetTable) != 0 {
		t.Fatal("frame fragment/BOT contract")
	}
	frames, e := jpegls.New().DecodeContext(context.Background(), p, out.Dataset)
	if e != nil || len(frames.Data) != 2 || !bytes.Equal(frames.Data[0], samples) || !bytes.Equal(frames.Data[1], samples) {
		t.Fatalf("multi-frame decode: %v", e)
	}
}

func TestFromImagesJ2KColorTransformsAndMultiFrameIOD(t *testing.T) {
	samples := []byte{0, 1, 255, 255, 128, 0, 30, 40, 50, 80, 90, 100}
	m := sampleMetadata()
	m.SamplesPerPixel = 3
	m.PhotometricInterpretation = "RGB"
	m.PlanarConfigurationPresent = true
	for _, lossless := range []bool{true, false} {
		uid := transfer.JPEG2000LosslessOnly.UID
		opt := jpeg2000.Options{TransferSyntaxUID: uid, ColorTransform: true}
		wantPI := "YBR_RCT"
		if !lossless {
			uid = transfer.JPEG2000.UID
			opt = jpeg2000.Options{TransferSyntaxUID: uid, ColorTransform: true, StepSize: 1, AllowLossy: true}
			wantPI = "YBR_ICT"
		}
		encoder, e := jpeg2000.NewEncoder(opt)
		if e != nil {
			t.Fatal(e)
		}
		coded, e := encoder.EncodeFrame(context.Background(), samples, m)
		if e != nil {
			t.Fatal(e)
		}
		f, r := imported(t, [][]byte{coded.Data, coded.Data}, ImageOptions{J2KLossless: lossless})
		out := part10Roundtrip(t, f)
		p, e := pixeldata.ExtractView(out.Dataset)
		if e != nil || len(p.Sequence.Fragments) != 2 || r.Frames != 2 || getString(out.Dataset, tagPhotometric) != wantPI || getString(out.Dataset, tagSOPClassUID) != MultiFrameTrueColorSC {
			t.Fatalf("color image import: %+v err=%v", r, e)
		}
		for _, fragment := range p.Sequence.Fragments {
			if !bytes.Equal(fragment[:len(coded.Data)], coded.Data) {
				t.Fatal("color J2K payload changed")
			}
		}
	}
}
