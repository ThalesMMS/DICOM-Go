package pixeldata_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/builtin"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpegls"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// admissionTestObject writes a 16-bit monochrome object of frames frames,
// rows x columns, whose sample values come from value, encoded in JPEG-LS
// Lossless, one fragment per frame.
func admissionTestObject(t *testing.T, rows, columns, frames int, value func(frame, i int) uint16) []byte {
	t.Helper()
	dataset := object.New(nil)
	us := func(tag core.Tag, v int) {
		dataset.Put(core.NewRawElement(tag, core.VRUS, binary.LittleEndian.AppendUint16(nil, uint16(v))))
	}
	text := func(tag core.Tag, vr core.VR, v string) {
		dataset.Put(core.Element{Header: core.ElementHeader{Tag: tag, VR: vr}, Value: core.StringValue{v}})
	}
	text(core.NewTag(0x0008, 0x0016), core.VRUI, "1.2.840.10008.5.1.4.1.1.7")
	text(core.NewTag(0x0008, 0x0018), core.VRUI, "1.2.3.4")
	us(core.NewTag(0x0028, 0x0010), rows)
	us(core.NewTag(0x0028, 0x0011), columns)
	us(core.NewTag(0x0028, 0x0002), 1)
	us(core.NewTag(0x0028, 0x0100), 16)
	us(core.NewTag(0x0028, 0x0101), 12)
	us(core.NewTag(0x0028, 0x0102), 11)
	us(core.NewTag(0x0028, 0x0103), 0)
	text(core.NewTag(0x0028, 0x0004), core.VRCS, "MONOCHROME2")
	text(core.NewTag(0x0028, 0x0008), core.VRIS, strconv.Itoa(frames))
	pixels := make([]byte, 0, rows*columns*frames*2)
	for frame := range frames {
		for i := range rows * columns {
			pixels = binary.LittleEndian.AppendUint16(pixels, value(frame, i)&0x0FFF)
		}
	}
	dataset.Put(core.NewRawElement(core.TagPixelData, core.VROW, pixels))
	encoders := pixeldata.NewMemoryEncoderRegistry()
	if err := jpegls.RegisterEncoder(encoders); err != nil {
		t.Fatal(err)
	}
	file := &object.File{Dataset: dataset, TransferSyntax: transfer.ExplicitVRLittleEndian}
	file, _, err := pixeldata.TranscodeFile(context.Background(), file, transfer.JPEGLSLossless, pixeldata.TranscodeOptions{EncoderRegistry: encoders})
	if err != nil {
		t.Fatal(err)
	}
	var buffer bytes.Buffer
	if err := object.WriteFile(&buffer, file); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// TestCheckTranscodeDecodeLimitsRefusesWhatTranscodePathRefuses converts
// JPEG-LS objects to Explicit VR Little Endian with TranscodePath and checks
// that CheckTranscodeDecodeLimits, on the object read with its Pixel Data
// deferred, refuses exactly those TranscodePath refuses on its limits:
// a constant frame whose decoding expands its stored bytes beyond
// MaxExpansionRatio, alone or beside a frame that keeps the whole object
// within the ratio, and objects above MaxFrames or MaxPixels.
func TestCheckTranscodeDecodeLimitsRefusesWhatTranscodePathRefuses(t *testing.T) {
	decoders, err := builtin.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	constant := func(int, int) uint16 { return 1000 }
	seed := uint32(1)
	noise := func(int, int) uint16 {
		seed = seed*1664525 + 1013904223
		return uint16(seed >> 20)
	}
	mixed := func(frame, i int) uint16 {
		if frame == 0 {
			return 1000
		}
		return noise(frame, i)
	}
	rowValued := func(frame, i int) uint16 { return uint16(frame*64 + i/96) }
	for _, tc := range []struct {
		name   string
		data   []byte
		limits pixeldata.TranscodeLimits
		refuse bool
	}{
		{"constant frame", admissionTestObject(t, 512, 512, 1, constant), pixeldata.TranscodeLimits{}, true},
		{"constant frame beside noise", admissionTestObject(t, 512, 512, 2, mixed), pixeldata.TranscodeLimits{}, true},
		{"row-valued frames", admissionTestObject(t, 64, 96, 3, rowValued), pixeldata.TranscodeLimits{}, false},
		{"constant frame under a larger ratio", admissionTestObject(t, 512, 512, 1, constant), pixeldata.TranscodeLimits{MaxExpansionRatio: 1 << 20}, false},
		{"frames", admissionTestObject(t, 64, 96, 3, rowValued), pixeldata.TranscodeLimits{MaxFrames: 2}, true},
		{"pixels", admissionTestObject(t, 64, 96, 3, rowValued), pixeldata.TranscodeLimits{MaxPixels: 64*96*3 - 1}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			options := pixeldata.TranscodeOptions{DecoderRegistry: decoders, Limits: tc.limits}
			directory := t.TempDir()
			input := filepath.Join(directory, "input.dcm")
			if err := os.WriteFile(input, tc.data, 0o600); err != nil {
				t.Fatal(err)
			}
			_, transcodeErr := pixeldata.TranscodePath(context.Background(), input, filepath.Join(directory, "output.dcm"), transfer.ExplicitVRLittleEndian, options)
			if transcodeErr != nil && !pixeldata.IsTranscodeLimitError(transcodeErr) {
				t.Fatalf("TranscodePath: %v", transcodeErr)
			}
			source, err := os.Open(input)
			if err != nil {
				t.Fatal(err)
			}
			defer source.Close()
			file, err := object.ReadFileWithOptions(source, object.ReadFileOptions{DeferPixelData: true})
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			checkErr := pixeldata.CheckTranscodeDecodeLimits(context.Background(), file, source, transfer.ExplicitVRLittleEndian, options)
			if checkErr != nil && !pixeldata.IsTranscodeLimitError(checkErr) {
				t.Fatalf("CheckTranscodeDecodeLimits: %v", checkErr)
			}
			t.Logf("%d bytes stored; TranscodePath: %v; CheckTranscodeDecodeLimits: %v", len(tc.data), transcodeErr, checkErr)
			if (transcodeErr != nil) != tc.refuse || (checkErr != nil) != tc.refuse {
				t.Errorf("TranscodePath refused %t and CheckTranscodeDecodeLimits refused %t, want both %t", transcodeErr != nil, checkErr != nil, tc.refuse)
			}
			var limit *pixeldata.DecompressResourceLimitError
			if errors.As(transcodeErr, &limit) {
				var checked *pixeldata.DecompressResourceLimitError
				if !errors.As(checkErr, &checked) || checked.Limit != limit.Limit {
					t.Errorf("TranscodePath refused on %s, CheckTranscodeDecodeLimits on %v", limit.Limit, checkErr)
				}
			}
		})
	}
}

// iconAdmissionTestObject writes an Implicit VR Little Endian object of two
// 8x8 frames with a 4x4 icon.
func iconAdmissionTestObject(t *testing.T) []byte {
	t.Helper()
	us := func(tag core.Tag, v int) core.Element {
		return core.NewRawElement(tag, core.VRUS, binary.LittleEndian.AppendUint16(nil, uint16(v)))
	}
	text := func(tag core.Tag, vr core.VR, v string) core.Element {
		return core.Element{Header: core.ElementHeader{Tag: tag, VR: vr}, Value: core.StringValue{v}}
	}
	image := func(rows, columns int, extra ...core.Element) []core.Element {
		return append([]core.Element{
			us(core.NewTag(0x0028, 0x0002), 1), text(core.NewTag(0x0028, 0x0004), core.VRCS, "MONOCHROME2"),
			us(core.NewTag(0x0028, 0x0010), rows), us(core.NewTag(0x0028, 0x0011), columns),
			us(core.NewTag(0x0028, 0x0100), 8), us(core.NewTag(0x0028, 0x0101), 8), us(core.NewTag(0x0028, 0x0102), 7), us(core.NewTag(0x0028, 0x0103), 0),
		}, extra...)
	}
	icon := object.FromElements(image(4, 4, core.NewRawElement(core.TagPixelData, core.VROB, make([]byte, 16))), nil)
	dataset := object.FromElements(image(8, 8,
		text(core.NewTag(0x0008, 0x0016), core.VRUI, "1.2.840.10008.5.1.4.1.1.7"),
		text(core.NewTag(0x0008, 0x0018), core.VRUI, "1.2.3.4"),
		text(core.NewTag(0x0028, 0x0008), core.VRIS, "2"),
		core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0088, 0x0200), VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{icon.ToDataSet()}}},
		core.NewRawElement(core.TagPixelData, core.VROB, bytes.Repeat([]byte{7}, 128)),
	), nil)
	var buffer bytes.Buffer
	if err := object.WriteFile(&buffer, &object.File{Dataset: dataset, TransferSyntax: transfer.ImplicitVRLittleEndian}); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// TestCheckTranscodeDecodeLimitsCountsIcons converts an object whose two
// frames carry a one-frame icon. TranscodePath bounds the frames and pixels
// of the image and its icons together, so MaxFrames 2, or MaxPixels one
// below the 144 pixels of both, refuses it; CheckTranscodeDecodeLimits, on
// the object read up to its Pixel Data as for a native source, refuses
// exactly the same.
func TestCheckTranscodeDecodeLimitsCountsIcons(t *testing.T) {
	data := iconAdmissionTestObject(t)
	directory := t.TempDir()
	input := filepath.Join(directory, "input.dcm")
	if err := os.WriteFile(input, data, 0o600); err != nil {
		t.Fatal(err)
	}
	stop := core.NewTag(0x7FE0, 0x0008)
	file, err := object.ReadFileWithOptions(bytes.NewReader(data), object.ReadFileOptions{StopBeforeTag: &stop})
	if err != nil && !errors.Is(err, object.ErrReadStopped) {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		limits pixeldata.TranscodeLimits
		refuse bool
	}{
		{"within the limits", pixeldata.TranscodeLimits{MaxFrames: 3, MaxPixels: 144}, false},
		{"frames", pixeldata.TranscodeLimits{MaxFrames: 2}, true},
		{"pixels", pixeldata.TranscodeLimits{MaxPixels: 143}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			options := pixeldata.TranscodeOptions{Limits: tc.limits}
			_, transcodeErr := pixeldata.TranscodePath(context.Background(), input, filepath.Join(directory, tc.name+".dcm"), transfer.ExplicitVRLittleEndian, options)
			checkErr := pixeldata.CheckTranscodeDecodeLimits(context.Background(), file, nil, transfer.ExplicitVRLittleEndian, options)
			t.Logf("TranscodePath: %v; CheckTranscodeDecodeLimits: %v", transcodeErr, checkErr)
			for name, err := range map[string]error{"TranscodePath": transcodeErr, "CheckTranscodeDecodeLimits": checkErr} {
				if (err != nil) != tc.refuse || (err != nil && !pixeldata.IsTranscodeLimitError(err)) {
					t.Errorf("%s: %v, want a limit refusal %t", name, err, tc.refuse)
				}
			}
		})
	}
}

// TestCheckTranscodeDecodeLimitsAdmitsCopies checks that a conversion that
// copies the encapsulated Pixel Data is not refused on decode limits.
func TestCheckTranscodeDecodeLimitsAdmitsCopies(t *testing.T) {
	data := admissionTestObject(t, 512, 512, 1, func(int, int) uint16 { return 1000 })
	file, err := object.ReadFileWithOptions(bytes.NewReader(data), object.ReadFileOptions{DeferPixelData: true})
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := pixeldata.CheckTranscodeDecodeLimits(context.Background(), file, bytes.NewReader(data), transfer.JPEGLSLossless, pixeldata.TranscodeOptions{}); err != nil {
		t.Fatalf("same syntax: %v", err)
	}
}
