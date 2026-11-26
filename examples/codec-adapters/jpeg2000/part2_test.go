package jpeg2000

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/j2kpart2"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	j2kenc "github.com/ThalesMMS/dicom-go/pixeldata/jpeg2000"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func part2IndependentDirectory(t *testing.T) string {
	t.Helper()
	base := os.Getenv("DICOM_GO_PART2_EVIDENCE_DIR")
	if base == "" {
		return t.TempDir()
	}
	if err := os.MkdirAll(base, 0700); err != nil {
		t.Fatal(err)
	}
	name := strings.NewReplacer("/", "_", "{", "", "}", "", " ", "_").Replace(t.Name())
	dir, err := os.MkdirTemp(base, name+"-")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("independent synthetic evidence: %s", dir)
	return dir
}

func TestPart2IndependentDecoderLargeExtrema(t *testing.T) {
	if os.Getenv("DICOMGO_J2K_PART2_ORACLE") != "1" {
		t.Skip("set DICOMGO_J2K_PART2_ORACLE=1 for independent large-field decoding")
	}
	python := os.Getenv("DICOM_GO_PYTHON")
	if python == "" {
		python = "python"
	}
	decoder := os.Getenv("DICOM_GO_OPENJPEG_DECOMPRESS")
	if decoder == "" {
		decoder = "opj_decompress"
	}
	for _, signed := range []bool{false, true} {
		file, _ := part2Image(16, 16, 5, signed)
		const rows, columns = 129, 257
		file.Dataset.Put(us(0x0028, 0x0010, rows))
		file.Dataset.Put(us(0x0028, 0x0011, columns))
		pixels := make([]byte, 2*rows*columns*5)
		random := uint32(1841)
		for i := 0; i < len(pixels); i += 2 {
			random = random*1664525 + 1013904223
			value := uint16(random >> 16)
			if i%128 == 0 {
				value = 0
			}
			if i%128 == 2 {
				value = 65535
			}
			binary.LittleEndian.PutUint16(pixels[i:], value)
		}
		file.Dataset.Put(core.NewRawElement(core.TagPixelData, core.VROW, pixels))
		encoders := pixeldata.NewMemoryEncoderRegistry()
		if err := j2kenc.RegisterPart2Encoder(encoders, j2kenc.Options{TransferSyntaxUID: transfer.JPEG2000Part2Lossless.UID}); err != nil {
			t.Fatal(err)
		}
		output, _, err := pixeldata.TranscodeFile(context.Background(), file, transfer.JPEG2000Part2Lossless, pixeldata.TranscodeOptions{EncoderRegistry: encoders})
		if err != nil {
			t.Fatal(err)
		}
		dir := part2IndependentDirectory(t)
		inputPath, outputPath := filepath.Join(dir, "source.dcm"), filepath.Join(dir, "encoded.dcm")
		for path, fileToWrite := range map[string]*object.File{inputPath: file, outputPath: output} {
			f, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			err = object.WriteFile(f, fileToWrite)
			closeErr := f.Close()
			if err != nil || closeErr != nil {
				t.Fatalf("write: %v %v", err, closeErr)
			}
		}
		cmd := exec.Command(python, "../../../pixeldata/jpeg2000/testdata/part2_oracle.py",
			"--decoder", decoder, "--part10", "--input", outputPath, "--source-native", inputPath,
			"--output", filepath.Join(dir, "receipt.json"))
		result, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("signed=%v independent large field: %v\n%s", signed, err, result)
		}
		t.Log(string(result))
	}
}

func TestPart2IndependentPart10Decoder(t *testing.T) {
	if os.Getenv("DICOMGO_J2K_PART2_ORACLE") != "1" {
		t.Skip("set DICOMGO_J2K_PART2_ORACLE=1 with pydicom 3.0.2 and OpenJPEG for independent Part 10 decoding")
	}
	python := os.Getenv("DICOM_GO_PYTHON")
	if python == "" {
		python = "python"
	}
	decoder := os.Getenv("DICOM_GO_OPENJPEG_DECOMPRESS")
	if decoder == "" {
		decoder = "opj_decompress"
	}
	script := "../../../pixeldata/jpeg2000/testdata/part2_oracle.py"
	for _, tc := range []struct {
		bits, allocated, frames int
		signed, lossy           bool
	}{
		{1, 8, 2, false, false}, {8, 8, 3, true, false},
		{12, 16, 7, false, false}, {16, 16, 16, true, false},
		{16, 16, 4, false, false}, {8, 8, 4, false, true}, {8, 8, 5, true, true},
		{8, 8, 2, false, true}, {8, 8, 16, true, true},
	} {
		t.Run(fmt.Sprint(tc), func(t *testing.T) {
			file, _ := part2Image(tc.bits, tc.allocated, tc.frames, tc.signed)
			options := j2kenc.Options{TransferSyntaxUID: transfer.JPEG2000Part2Lossless.UID}
			syntax, limit := transfer.JPEG2000Part2Lossless, 0
			if tc.lossy {
				options.TransferSyntaxUID, options.AllowLossy, options.StepSize = transfer.JPEG2000Part2.UID, true, 2
				syntax, limit = transfer.JPEG2000Part2, 6
				if tc.frames == 16 {
					limit = 8
				}
			}
			encoders := pixeldata.NewMemoryEncoderRegistry()
			if err := j2kenc.RegisterPart2Encoder(encoders, options); err != nil {
				t.Fatal(err)
			}
			decoders := pixeldata.NewMemoryRegistry()
			if err := RegisterCorePart2(decoders); err != nil {
				t.Fatal(err)
			}
			dir := part2IndependentDirectory(t)
			source, destination, receipt := filepath.Join(dir, "source.dcm"), filepath.Join(dir, "encoded.dcm"), filepath.Join(dir, "receipt.json")
			var native bytes.Buffer
			if err := object.WriteFile(&native, file); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(source, native.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
			transcode := pixeldata.TranscodeOptions{EncoderRegistry: encoders, DecoderRegistry: decoders, AllowLossy: tc.lossy}
			if tc.lossy {
				bound := float64(limit)
				transcode.MaxPixelError = &bound
			}
			if _, err := pixeldata.TranscodePath(context.Background(), source, destination, syntax, transcode); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(python, script, "--decoder", decoder, "--part10", "--input", destination,
				"--source-native", source, "--limit", fmt.Sprint(limit), "--output", receipt)
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("independent Part 10 decoder: %v\n%s", err, output)
			}
			t.Log(string(output))
		})
	}
}

func TestPart2LossyPathVerifiesBeforePublication(t *testing.T) {
	file, _ := part2Image(8, 8, 5, true)
	encoders := pixeldata.NewMemoryEncoderRegistry()
	if err := j2kenc.RegisterPart2Encoder(encoders, j2kenc.Options{TransferSyntaxUID: transfer.JPEG2000Part2.UID, StepSize: 2, AllowLossy: true}); err != nil {
		t.Fatal(err)
	}
	decoders := pixeldata.NewMemoryRegistry()
	if err := RegisterCorePart2(decoders); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	input, output := filepath.Join(dir, "input.dcm"), filepath.Join(dir, "output.dcm")
	fp, err := os.Create(input)
	if err != nil {
		t.Fatal(err)
	}
	if err = object.WriteFile(fp, file); err != nil {
		t.Fatal(err)
	}
	if err = fp.Close(); err != nil {
		t.Fatal(err)
	}
	sentinel := []byte("previous destination")
	if err = os.WriteFile(output, sentinel, 0600); err != nil {
		t.Fatal(err)
	}
	limit := 0.0
	options := pixeldata.TranscodeOptions{EncoderRegistry: encoders, DecoderRegistry: decoders, AllowLossy: true, MaxPixelError: &limit}
	if _, err = pixeldata.TranscodePath(context.Background(), input, output, transfer.JPEG2000Part2, options); !errors.Is(err, pixeldata.ErrTranscodePixelError) {
		t.Fatalf("zero-error check: %v", err)
	}
	retained, err := os.ReadFile(output)
	if err != nil || !bytes.Equal(retained, sentinel) {
		t.Fatalf("failed verification changed destination: %v", err)
	}
	limit = 6
	report, err := pixeldata.TranscodePath(context.Background(), input, output, transfer.JPEG2000Part2, options)
	if err != nil || report.Verification != pixeldata.TranscodeVerificationPixelError || report.LossyVerification == nil || report.LossyVerification.Frames != 5 {
		t.Fatalf("successful lossy path: %+v %v", report, err)
	}
}

func TestPart2FrameReaderConcurrent(t *testing.T) {
	file, want := part2Image(16, 16, 5, true)
	encoders := pixeldata.NewMemoryEncoderRegistry()
	if err := j2kenc.RegisterPart2Encoder(encoders, j2kenc.Options{TransferSyntaxUID: transfer.JPEG2000Part2Lossless.UID}); err != nil {
		t.Fatal(err)
	}
	decoders := pixeldata.NewMemoryRegistry()
	if err := RegisterCorePart2(decoders); err != nil {
		t.Fatal(err)
	}
	out, _, err := pixeldata.TranscodeFile(context.Background(), file, transfer.JPEG2000Part2Lossless, pixeldata.TranscodeOptions{EncoderRegistry: encoders})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := pixeldata.NewFrameReader(context.Background(), out, nil, pixeldata.FrameReaderOptions{Registry: decoders})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var group sync.WaitGroup
	for i := 0; i < 10; i++ {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			frame := i % len(want)
			got, _, err := reader.Frame(context.Background(), frame)
			if err != nil || !bytes.Equal(got, want[frame]) {
				t.Errorf("concurrent Frame(%d): %v", frame, err)
			}
		}(i)
	}
	group.Wait()
}

func TestPart2SixteenBitExtremaAcrossBlocks(t *testing.T) {
	for _, signed := range []bool{false, true} {
		metadata := pixeldata.Metadata{
			Rows: 129, Columns: 257, SamplesPerPixel: 1, BitsAllocated: 16,
			BitsStored: 16, HighBit: 15, NumberOfFrames: 5,
			PhotometricInterpretation: "MONOCHROME2",
		}
		if signed {
			metadata.PixelRepresentation = 1
		}
		frames := make([][]byte, metadata.NumberOfFrames)
		seed := uint32(1841)
		for i := range frames {
			frames[i] = make([]byte, metadata.FrameSize())
			for j := 0; j < len(frames[i]); j += 2 {
				seed = seed*1664525 + 1013904223
				value := uint16(seed >> 16)
				if j%64 == 0 {
					value = 0
				}
				if j%64 == 2 {
					value = 65535
				}
				binary.LittleEndian.PutUint16(frames[i][j:], value)
			}
		}
		encoder, err := j2kenc.NewPart2Encoder(j2kenc.Options{TransferSyntaxUID: transfer.JPEG2000Part2Lossless.UID})
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := encoder.EncodeFrameCollection(context.Background(), frames, metadata)
		if err != nil {
			t.Fatal(err)
		}
		layout, err := j2kpart2.Inspect(encoded)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := DecodeCoreReduced(context.Background(), layout.CodedComponents(encoded), 0, CoreOptions{Limits: CoreLimits{MaxComponents: 16}}); !errors.Is(err, ErrUnsupportedCodestreamFeature) {
			t.Fatalf("17-bit Part 1 component accepted outside Annex J profile: %v", err)
		}
		file, _ := part2Image(16, 16, metadata.NumberOfFrames, signed)
		file.Dataset.Put(us(0x0028, 0x0010, metadata.Rows))
		file.Dataset.Put(us(0x0028, 0x0011, metadata.Columns))
		pixels := pixeldata.PixelData{Encapsulated: true, Sequence: core.FragmentSequence{Fragments: [][]byte{encoded}}}
		decoded, err := (part2Codec{lossless: true}).Decode(pixels, file.Dataset)
		if err != nil {
			t.Fatal(err)
		}
		for i := range frames {
			if !bytes.Equal(decoded.Data[i], frames[i]) {
				t.Fatalf("signed=%v frame=%d extreme samples changed", signed, i)
			}
		}
	}
}

func TestPart2RejectsLossyAuthorizationAndChangedLayout(t *testing.T) {
	file, _ := part2Image(8, 8, 3, false)
	encoders := pixeldata.NewMemoryEncoderRegistry()
	if err := j2kenc.RegisterPart2Encoder(encoders, j2kenc.Options{TransferSyntaxUID: transfer.JPEG2000Part2.UID, StepSize: 1, AllowLossy: true}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := pixeldata.TranscodeFile(context.Background(), file, transfer.JPEG2000Part2, pixeldata.TranscodeOptions{EncoderRegistry: encoders}); !errors.Is(err, pixeldata.ErrTranscodeLossyDisallowed) {
		t.Fatalf("lossy authorization: %v", err)
	}
	lossless := pixeldata.NewMemoryEncoderRegistry()
	if err := j2kenc.RegisterPart2Encoder(lossless, j2kenc.Options{TransferSyntaxUID: transfer.JPEG2000Part2Lossless.UID}); err != nil {
		t.Fatal(err)
	}
	output, _, err := pixeldata.TranscodeFile(context.Background(), file, transfer.JPEG2000Part2Lossless, pixeldata.TranscodeOptions{EncoderRegistry: lossless})
	if err != nil {
		t.Fatal(err)
	}
	registry := pixeldata.NewMemoryRegistry()
	if err := RegisterCorePart2(registry); err != nil {
		t.Fatal(err)
	}
	pixel, err := pixeldata.Extract(output.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*pixeldata.PixelData){
		func(p *pixeldata.PixelData) { p.Sequence.OffsetTable = []byte{0, 0, 0, 0} },
		func(p *pixeldata.PixelData) { p.Sequence.Fragments = append(p.Sequence.Fragments, []byte{0, 0}) },
		func(p *pixeldata.PixelData) { p.Sequence.Fragments[0] = p.Sequence.Fragments[0][:20] },
	} {
		bad := pixel
		bad.Sequence.Fragments = append([][]byte(nil), pixel.Sequence.Fragments...)
		mutate(&bad)
		if _, err := registry.DecodeFrames(transfer.JPEG2000Part2Lossless.UID, bad, output.Dataset); err == nil {
			t.Fatal("malformed collection decoded")
		}
	}
}

func part2Image(bits, allocated, frames int, signed bool) (*object.File, [][]byte) {
	const rows, columns = 9, 11
	sign := uint16(0)
	if signed {
		sign = 1
	}
	var elements []core.Element
	for tag, value := range map[core.Tag]uint16{
		core.NewTag(0x0028, 0x0010): rows, core.NewTag(0x0028, 0x0011): columns,
		core.NewTag(0x0028, 0x0002): 1, core.NewTag(0x0028, 0x0100): uint16(allocated),
		core.NewTag(0x0028, 0x0101): uint16(bits), core.NewTag(0x0028, 0x0102): uint16(bits - 1),
		core.NewTag(0x0028, 0x0103): sign,
	} {
		elements = append(elements, core.NewRawElement(tag, core.VRUS, binary.LittleEndian.AppendUint16(nil, value)))
	}
	elements = append(elements,
		core.NewRawElement(core.NewTag(0x0028, 0x0004), core.VRCS, []byte("MONOCHROME2")),
		core.NewRawElement(core.NewTag(0x0028, 0x0008), core.VRIS, []byte(fmt.Sprint(frames))),
		core.NewRawElement(core.NewTag(0x0008, 0x0016), core.VRUI, []byte("1.2.840.10008.5.1.4.1.1.7.2")),
		core.NewRawElement(core.NewTag(0x0008, 0x0018), core.VRUI, []byte("2.25.1841")),
	)
	mask := 1<<bits - 1
	pixels := make([][]byte, frames)
	for frame := range pixels {
		pixels[frame] = make([]byte, rows*columns*allocated/8)
		for i := 0; i < rows*columns; i++ {
			value := (i*19 + frame*37) & mask
			if allocated == 8 {
				pixels[frame][i] = byte(value)
			} else {
				binary.LittleEndian.PutUint16(pixels[frame][2*i:], uint16(value))
			}
		}
	}
	elements = append(elements, core.NewRawElement(core.TagPixelData, core.VROW, bytes.Join(pixels, nil)))
	return &object.File{Dataset: object.FromElements(elements, nil), TransferSyntax: transfer.ExplicitVRLittleEndian}, pixels
}

func TestPart2TranscodeCollectionRoundTrip(t *testing.T) {
	for _, config := range []struct {
		bits, allocated, frames int
		signed                  bool
	}{
		{1, 8, 2, false}, {8, 8, 3, false}, {8, 8, 5, true}, {12, 16, 7, false}, {15, 16, 16, true}, {16, 16, 4, false}, {16, 16, 7, true},
	} {
		t.Run(fmt.Sprint(config), func(t *testing.T) {
			file, expected := part2Image(config.bits, config.allocated, config.frames, config.signed)
			encoder, err := j2kenc.NewPart2Encoder(j2kenc.Options{TransferSyntaxUID: transfer.JPEG2000Part2Lossless.UID})
			if err != nil {
				t.Fatal(err)
			}
			encoders := pixeldata.NewMemoryEncoderRegistry()
			if err = encoders.RegisterEncoder(transfer.JPEG2000Part2Lossless.UID, encoder); err != nil {
				t.Fatal(err)
			}
			decoders := pixeldata.NewMemoryRegistry()
			if err = RegisterCorePart2(decoders); err != nil {
				t.Fatal(err)
			}
			output, report, err := pixeldata.TranscodeFile(context.Background(), file, transfer.JPEG2000Part2Lossless, pixeldata.TranscodeOptions{
				EncoderRegistry: encoders, DecoderRegistry: decoders,
			})
			if err != nil {
				t.Fatal(err)
			}
			if report.Frames != config.frames || report.Lossy {
				t.Fatalf("report %#v", report)
			}
			pixels, err := pixeldata.Extract(output.Dataset)
			if err != nil {
				t.Fatal(err)
			}
			if len(pixels.Sequence.Fragments) != 1 || len(pixels.Sequence.OffsetTable) != 0 {
				t.Fatal("not an empty-BOT collection")
			}
			got, err := decoders.DecodeFrames(transfer.JPEG2000Part2Lossless.UID, pixels, output.Dataset)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Data) != len(expected) {
				t.Fatal("frame count")
			}
			reader, err := pixeldata.NewFrameReader(context.Background(), output, nil, pixeldata.FrameReaderOptions{Registry: decoders})
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			for i := range expected {
				frame, meta, err := reader.Frame(context.Background(), i)
				if err != nil || !bytes.Equal(frame, expected[i]) || meta.NumberOfFrames != 1 {
					t.Fatalf("Frame(%d): metadata=%+v err=%v", i, meta, err)
				}
				into := make([]byte, len(frame))
				result, err := reader.FrameInto(context.Background(), i, into)
				if err != nil || result.Written != len(into) || !bytes.Equal(into, frame) {
					t.Fatalf("FrameInto(%d): %+v %v", i, result, err)
				}
			}
			region, meta, err := reader.Region(context.Background(), 0, image.Rect(0, 0, 2, 1))
			if err != nil || meta.Rows != 1 || meta.Columns != 2 || !bytes.Equal(region, expected[0][:2*config.allocated/8]) {
				t.Fatalf("Region: metadata=%+v err=%v", meta, err)
			}
			if _, err = reader.RawFrame(context.Background(), 0); !errors.Is(err, pixeldata.ErrFrameLayout) {
				t.Fatalf("RawFrame should refuse an independent frame bitstream: %v", err)
			}
			// Canonical signed samples are sign-extended from BitsStored.
			for i := range expected {
				for n := 0; n < len(expected[i]); n += config.allocated / 8 {
					if config.allocated == 16 && config.signed {
						raw := binary.LittleEndian.Uint16(expected[i][n:])
						if raw&(1<<(config.bits-1)) != 0 {
							raw |= ^uint16((1 << config.bits) - 1)
						}
						binary.LittleEndian.PutUint16(expected[i][n:], raw)
					}
				}
				if !bytes.Equal(got.Data[i], expected[i]) {
					t.Fatalf("frame %d sample mismatch: got %v want %v", i, got.Data[i][:12], expected[i][:12])
				}
			}
			canceled, cancel := context.WithCancel(context.Background())
			cancel()
			if _, _, err := pixeldata.TranscodeFile(canceled, file, transfer.JPEG2000Part2Lossless, pixeldata.TranscodeOptions{EncoderRegistry: encoders}); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancel: %v", err)
			}
		})
	}
}

func TestPart2TranscodePathUsesCollectionAndAtomicLimits(t *testing.T) {
	file, expected := part2Image(12, 16, 4, false)
	encoders := pixeldata.NewMemoryEncoderRegistry()
	if err := j2kenc.RegisterPart2Encoder(encoders, j2kenc.Options{TransferSyntaxUID: transfer.JPEG2000Part2Lossless.UID}); err != nil {
		t.Fatal(err)
	}
	decoders := pixeldata.NewMemoryRegistry()
	if err := RegisterCorePart2(decoders); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	source, destination := filepath.Join(dir, "source.dcm"), filepath.Join(dir, "out.dcm")
	stream, err := os.Create(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := object.WriteFile(stream, file); err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	options := pixeldata.TranscodeOptions{EncoderRegistry: encoders, DecoderRegistry: decoders}
	report, err := pixeldata.TranscodePath(context.Background(), source, destination, transfer.JPEG2000Part2Lossless, options)
	if err != nil || report.Verification != pixeldata.TranscodeVerificationPixelExact || report.Streaming {
		t.Fatalf("path report=%+v error=%v", report, err)
	}
	reader, err := pixeldata.OpenFrameReader(context.Background(), destination, pixeldata.FrameReaderOptions{Registry: decoders})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	for i, want := range expected {
		got, _, err := reader.Frame(context.Background(), i)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("deferred Frame(%d): %v", i, err)
		}
	}
	before, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	options.Limits.MaxOutputBytes = 64
	_, err = pixeldata.TranscodePath(context.Background(), source, destination, transfer.JPEG2000Part2Lossless, options)
	if !errors.Is(err, pixeldata.ErrTranscodeResourceLimit) && !errors.Is(err, pixeldata.ErrDecompressResourceLimit) {
		t.Fatalf("output limit: %v", err)
	}
	after, err := os.ReadFile(destination)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("failed publication changed destination: %v", err)
	}
}

func TestPart2LossyCollectionTranscode(t *testing.T) {
	file, _ := part2Image(8, 8, 3, false)
	encoder, err := j2kenc.NewPart2Encoder(j2kenc.Options{TransferSyntaxUID: transfer.JPEG2000Part2.UID, StepSize: 2, AllowLossy: true})
	if err != nil {
		t.Fatal(err)
	}
	encoders := pixeldata.NewMemoryEncoderRegistry()
	if err := encoders.RegisterEncoder(transfer.JPEG2000Part2.UID, encoder); err != nil {
		t.Fatal(err)
	}
	decoders := pixeldata.NewMemoryRegistry()
	if err := RegisterCorePart2(decoders); err != nil {
		t.Fatal(err)
	}
	limit := float64(5)
	options := pixeldata.TranscodeOptions{EncoderRegistry: encoders, DecoderRegistry: decoders, AllowLossy: true, MaxPixelError: &limit}
	output, report, err := pixeldata.TranscodeFile(context.Background(), file, transfer.JPEG2000Part2, options)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Lossy || report.LossyVerification == nil || report.LossyVerification.MaximumError <= 0 || report.LossyVerification.MaximumError > 5 {
		t.Fatalf("report %#v", report)
	}
	method, _ := output.Dataset.GetString(core.NewTag(0x0028, 0x2114))
	if method != "ISO_15444_1" {
		t.Fatalf("lossy method %q", method)
	}
	oldUID, _ := file.Dataset.GetString(core.NewTag(0x0008, 0x0018))
	newUID, _ := output.Dataset.GetString(core.NewTag(0x0008, 0x0018))
	if oldUID == newUID {
		t.Fatal("new lossy generation retained source identity")
	}
	tooStrict := 0.0
	options.MaxPixelError = &tooStrict
	if _, _, err := pixeldata.TranscodeFile(context.Background(), file, transfer.JPEG2000Part2, options); !errors.Is(err, pixeldata.ErrTranscodePixelError) {
		t.Fatalf("zero allowed error accepted lossy samples: %v", err)
	}
}
