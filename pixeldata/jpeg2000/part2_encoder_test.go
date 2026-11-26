package jpeg2000

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ThalesMMS/dicom-go/internal/j2kpart2"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestPart2CollectionInspectorRefusesTruncation(t *testing.T) {
	frames, metadata := part2Input(8, 3, false)
	encoder, err := NewPart2Encoder(Options{TransferSyntaxUID: transfer.JPEG2000Part2Lossless.UID})
	if err != nil {
		t.Fatal(err)
	}
	data, err := encoder.EncodeFrameCollection(context.Background(), frames, metadata)
	if err != nil {
		t.Fatal(err)
	}
	for length := 0; length < len(data); length++ {
		if _, err := j2kpart2.Inspect(data[:length]); err == nil {
			t.Fatalf("accepted truncation at %d/%d", length, len(data))
		}
	}
	original := append([]byte(nil), data...)
	for i := 0; i < len(data)-2; i++ {
		if data[i] != 255 || data[i+1] != 0x74 {
			continue
		}
		for value := byte(1); value < 16; value++ {
			data[i+2] = value
			if _, err := j2kpart2.Inspect(data); err == nil {
				t.Fatalf("accepted damaged MCT length at %d", i)
			}
		}
		copy(data, original)
	}
}

type cancelCollectionContext struct {
	context.Context
	calls atomic.Int32
}

func (c *cancelCollectionContext) Err() error {
	if c.calls.Add(1) > 3 {
		return context.Canceled
	}
	return nil
}

func TestPart2CancellationDuringCollection(t *testing.T) {
	frames, metadata := part2Input(16, 5, true)
	encoder, err := NewPart2Encoder(Options{TransferSyntaxUID: transfer.JPEG2000Part2Lossless.UID})
	if err != nil {
		t.Fatal(err)
	}
	ctx := &cancelCollectionContext{Context: context.Background()}
	result, err := encoder.EncodeFrameCollection(ctx, frames, metadata)
	if !errors.Is(err, context.Canceled) || result != nil {
		t.Fatalf("result=%d bytes err=%v", len(result), err)
	}
}

func TestPart2CollectionConcurrentEncoding(t *testing.T) {
	frames, metadata := part2Input(16, 5, true)
	encoder, err := NewPart2Encoder(Options{TransferSyntaxUID: transfer.JPEG2000Part2Lossless.UID})
	if err != nil {
		t.Fatal(err)
	}
	expected, err := encoder.EncodeFrameCollection(context.Background(), frames, metadata)
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			result, err := encoder.EncodeFrameCollection(context.Background(), frames, metadata)
			if err != nil || !bytes.Equal(result, expected) {
				t.Errorf("concurrent encode changed: %v", err)
			}
		}()
	}
	group.Wait()
}

func part2Input(bits, frames int, signed bool) ([][]byte, pixeldata.Metadata) {
	m := pixeldata.Metadata{Rows: 9, Columns: 11, SamplesPerPixel: 1, PhotometricInterpretation: "MONOCHROME2", BitsAllocated: 8,
		BitsStored: uint16(bits), HighBit: uint16(bits - 1), NumberOfFrames: frames}
	if bits > 8 {
		m.BitsAllocated = 16
	}
	if signed {
		m.PixelRepresentation = 1
	}
	out := make([][]byte, frames)
	for i := range out {
		out[i] = make([]byte, m.FrameSize())
		for j := 0; j < int(m.Rows)*int(m.Columns); j++ {
			sample := (i*37 + j*19) & ((1 << bits) - 1)
			if signed && sample&(1<<(bits-1)) != 0 {
				sample -= 1 << bits
			}
			if bits <= 8 {
				out[i][j] = byte(sample)
			} else {
				binary.LittleEndian.PutUint16(out[i][2*j:], uint16(sample))
			}
		}
	}
	return out, m
}

func TestPart2CollectionSignalingAndGuards(t *testing.T) {
	frames, m := part2Input(12, 3, true)
	e, err := NewPart2Encoder(Options{TransferSyntaxUID: transfer.JPEG2000Part2Lossless.UID})
	if err != nil {
		t.Fatal(err)
	}
	data, err := e.EncodeFrameCollection(context.Background(), frames, m)
	if err != nil {
		t.Fatal(err)
	}
	layout, err := j2kpart2.Inspect(data)
	if err != nil {
		t.Fatal(err)
	}
	if layout.Frames != 3 || layout.Bits != 12 || !layout.Reversible || !layout.Signed {
		t.Fatalf("%+v", layout)
	}
	if _, err = e.EncodeFrame(context.Background(), frames[0], m); !errors.Is(err, pixeldata.ErrUnsupportedEncoderMetadata) {
		t.Fatalf("independent frame: %v", err)
	}
	for _, bad := range []func(*pixeldata.Metadata){
		func(m *pixeldata.Metadata) { m.SamplesPerPixel = 3 },
		func(m *pixeldata.Metadata) { m.NumberOfFrames = 1 },
		func(m *pixeldata.Metadata) { m.BitsStored = 17 },
		func(m *pixeldata.Metadata) { m.HighBit = 0 },
		func(m *pixeldata.Metadata) { m.PixelRepresentation = 2 },
		func(m *pixeldata.Metadata) { m.PlanarConfigurationPresent = true },
	} {
		metadata := m
		bad(&metadata)
		if _, err = e.EncodeFrameCollection(context.Background(), frames, metadata); err == nil {
			t.Fatalf("accepted %+v", metadata)
		}
	}
	big := m
	big.Rows, big.Columns = 65535, 65535
	if _, err = e.EncodeFrameCollection(context.Background(), frames, big); !errors.Is(err, pixeldata.ErrTranscodeResourceLimit) {
		t.Fatalf("budget: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = e.EncodeFrameCollection(ctx, frames, m); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	if _, err = NewPart2Encoder(Options{TransferSyntaxUID: transfer.JPEG2000Part2.UID, StepSize: 1}); !errors.Is(err, pixeldata.ErrTranscodeLossyDisallowed) {
		t.Fatalf("lossy authorization: %v", err)
	}
	corrupt := append([]byte(nil), data...)
	for i := 2; i < len(corrupt)-2; i++ {
		if corrupt[i] == 0xff && corrupt[i+1] == 0x75 {
			corrupt[i+11] = 1
			break
		}
	}
	if _, err = j2kpart2.Inspect(corrupt); err == nil {
		t.Fatal("accepted changed transform")
	}
	registry := pixeldata.NewMemoryEncoderRegistry()
	if err = RegisterPart2Encoder(registry, Options{TransferSyntaxUID: transfer.JPEG2000Part2Lossless.UID}); err != nil {
		t.Fatal(err)
	}
	if _, ok := registry.GetEncoder(transfer.JPEG2000Part2Lossless.UID); !ok {
		t.Fatal("missing explicit registry")
	}
}

func TestPart2CollectionOpenJPEGReconstruction(t *testing.T) {
	if os.Getenv("DICOMGO_J2K_PART2_ORACLE") != "1" {
		t.Skip("set DICOMGO_J2K_PART2_ORACLE=1 for independent OpenJPEG coefficient decode and Annex J reconstruction")
	}
	decoder := os.Getenv("DICOM_GO_OPENJPEG_DECOMPRESS")
	if decoder == "" {
		decoder = "opj_decompress"
	}
	if _, err := exec.LookPath(decoder); err != nil {
		t.Fatal(err)
	}
	python := os.Getenv("DICOM_GO_PYTHON")
	if python == "" {
		python = "python"
	}
	encoder := os.Getenv("DICOM_GO_OPENJPEG_COMPRESS")
	if encoder == "" {
		encoder = "opj_compress"
	}
	control := exec.Command(python, "testdata/part2_oracle_controls.py", "--encoder", encoder, "--decoder", decoder)
	if output, err := control.CombinedOutput(); err != nil {
		t.Fatalf("independent decoder controls: %v\n%s", err, output)
	} else {
		t.Log(string(output))
	}
	for _, tc := range []struct {
		bits, frames  int
		signed, lossy bool
	}{
		{1, 2, false, false}, {8, 3, false, false}, {8, 5, true, false}, {12, 7, false, false}, {16, 4, false, false}, {16, 16, true, false}, {8, 4, false, true}, {8, 5, true, true},
	} {
		t.Run(fmt.Sprint(tc), func(t *testing.T) {
			frames, m := part2Input(tc.bits, tc.frames, tc.signed)
			uid := transfer.JPEG2000Part2Lossless.UID
			step, limit := 0.0, 0
			if tc.lossy {
				uid, step, limit = transfer.JPEG2000Part2.UID, 2, 6
			}
			e, err := NewPart2Encoder(Options{TransferSyntaxUID: uid, StepSize: step, AllowLossy: tc.lossy})
			if err != nil {
				t.Fatal(err)
			}
			data, err := e.EncodeFrameCollection(context.Background(), frames, m)
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			input, expected := filepath.Join(dir, "encoded.j2k"), filepath.Join(dir, "expected.raw")
			if err = os.WriteFile(input, data, 0600); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(expected, bytes.Join(frames, nil), 0600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(python, "testdata/part2_oracle.py", "--decoder", decoder, "--input", input, "--expected", expected,
				"--allocated", fmt.Sprint(m.BitsAllocated), "--limit", fmt.Sprint(limit))
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%v: %s", err, output)
			}
			t.Log(string(output))
		})
	}
}
