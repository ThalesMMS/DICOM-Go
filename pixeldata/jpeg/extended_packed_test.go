package jpeg

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata"
)

func packedMonoFixture(t *testing.T, metadata pixeldata.Metadata) []byte {
	t.Helper()
	encoder, err := NewExtendedEncoder(DefaultQuality)
	if err != nil {
		t.Fatal(err)
	}
	raw := extendedPhantomFrame(int(metadata.Rows), int(metadata.Columns), 12, 1740)
	frame, err := encoder.EncodeFrame(context.Background(), raw, metadata)
	if err != nil {
		t.Fatal(err)
	}
	return frame.Data
}

func TestJPEGExtendedPackedMonoMatchesPaddedPath(t *testing.T) {
	const width, height = 31, 19
	metadata := extendedEncoderMetadata(height, width, 12)
	stream := packedMonoFixture(t, metadata)
	// Trigger the padded compatibility path without changing the scan's
	// entropy, restart state or effective sequential interpretation.
	quirked := bytes.Clone(stream)
	quirked[jpegSegment(t, quirked, 0xda)+4] = 0 // Se=0 instead of 63.
	ctx := pixeldata.DecodeContextWithRecovery(context.Background(), pixeldata.DecodeRecovery{JPEGSequentialScanQuirks: true})

	decoded, err := decodeExtendedProcess4(stream, metadata)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name         string
		bits, signed uint16
	}{
		{"unsigned12", 12, 0},
		{"signed12", 12, 1},
		{"unsigned10", 10, 0},
		{"signed10", 10, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj, pixel := process4Object(t, stream, height, width, 16, tc.bits, tc.bits-1, tc.signed)
			got, err := New().Decode(pixel, obj)
			if err != nil {
				t.Fatal(err)
			}
			if got.Rows != height || got.Columns != width || len(got.Data) != 1 || len(got.Data[0]) != height*width*2 {
				t.Fatalf("packed output geometry: %dx%d, %d frames", got.Columns, got.Rows, len(got.Data))
			}
			obj, pixel = process4Object(t, quirked, height, width, 16, tc.bits, tc.bits-1, tc.signed)
			padded, err := New().DecodeContext(ctx, pixel, obj)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(padded.Diagnostics, pixeldata.DiagnosticJPEGScanParametersIgnored) || !bytes.Equal(got.Data[0], padded.Data[0]) {
				t.Fatalf("packed frame differs from padded recovery, diagnostics=%v", padded.Diagnostics)
			}
			limit := uint16(1<<tc.bits - 1)
			for offset := 0; offset < len(decoded); offset += 2 {
				sample := binary.LittleEndian.Uint16(decoded[offset:])
				want := min(sample, limit)
				if tc.signed != 0 {
					want = sample & limit
				}
				if sample := binary.LittleEndian.Uint16(got.Data[0][offset:]); sample != want {
					t.Fatalf("sample %d = %d, want %d", offset/2, sample, want)
				}
			}
		})
	}
}

type cancelAfterPackedOutput struct {
	context.Context
	cancel  context.CancelFunc
	decoder *extendedProcess4Decoder
}

func (c cancelAfterPackedOutput) Err() error {
	if len(c.decoder.packedMono) >= 2 && binary.LittleEndian.Uint16(c.decoder.packedMono) != 0 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestJPEGExtendedPackedMonoDiscardsPartialOutput(t *testing.T) {
	const width, height = 211, 153
	metadata := extendedEncoderMetadata(height, width, 12)
	stream := packedMonoFixture(t, metadata)

	t.Run("canceled codec entry", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		obj, pixel := process4Object(t, stream, height, width, 16, 12, 11, 0)
		frames, err := New().DecodeContext(ctx, pixel, obj)
		if err != context.Canceled || len(frames.Data) != 0 {
			t.Fatalf("canceled codec = %d exported frames, err=%v", len(frames.Data), err)
		}
	})

	t.Run("canceled during MCU decode", func(t *testing.T) {
		base, cancel := context.WithCancel(context.Background())
		defer cancel()
		d := extendedProcess4Decoder{data: stream, pos: 2, adobeTransform: -1}
		d.ctx = cancelAfterPackedOutput{Context: base, cancel: cancel, decoder: &d}
		out, _, err := d.decode(metadata)
		if !errors.Is(err, context.Canceled) || out != nil || len(d.packedMono) != width*height*2 {
			t.Fatalf("canceled decode = %d exported bytes, %d private bytes, err=%v", len(out), len(d.packedMono), err)
		}
	})

	t.Run("truncated final MCUs", func(t *testing.T) {
		eoi := bytes.LastIndex(stream, []byte{0xff, 0xd9})
		// Keep framing valid while dropping compressed sample data.
		broken := append(bytes.Clone(stream[:eoi-16]), 0xff, 0xd9)
		obj, pixel := process4Object(t, broken, height, width, 16, 12, 11, 0)
		frames, err := New().Decode(pixel, obj)
		if !errors.Is(err, ErrInvalidFragment) || len(frames.Data) != 0 {
			t.Fatalf("truncated decode = %d exported frames, err=%v", len(frames.Data), err)
		}
	})
}
