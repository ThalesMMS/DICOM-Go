package jpegls

import (
	"bytes"
	"context"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata"
)

func FuzzEncodeFrameMono8(f *testing.F) {
	f.Add([]byte{0, 64, 128, 255})
	f.Add(bytes.Repeat([]byte{7}, 15))
	f.Fuzz(func(t *testing.T, frame []byte) {
		if len(frame) == 0 {
			return
		}
		columns := len(frame)
		if columns > 32 {
			columns = 32
			frame = frame[:32]
		}
		metadata := encoderMetadata(1, uint16(columns), 1, 8, "MONOCHROME2")
		encoded, err := NewEncoder().EncodeFrame(context.Background(), frame, metadata)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := decodeFrame(encoded.Data, metadata)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(decoded, frame) {
			t.Fatalf("round trip = % x, want % x", decoded, frame)
		}
	})
}

func FuzzEncodeFrameMono16(f *testing.F) {
	f.Add([]byte{0, 0, 1, 0, 0, 128, 255, 255})
	f.Add(bytes.Repeat([]byte{0x55, 0xaa}, 8))
	f.Fuzz(func(t *testing.T, frame []byte) {
		if len(frame) < 2 {
			return
		}
		if len(frame) > 64 {
			frame = frame[:64]
		}
		frame = frame[:len(frame)&^1]
		columns := len(frame) / 2
		metadata := encoderMetadata(1, uint16(columns), 1, 16, "MONOCHROME2")
		encoded, err := NewEncoder().EncodeFrame(context.Background(), frame, metadata)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := decodeFrame(encoded.Data, metadata)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(decoded, frame) {
			t.Fatalf("round trip = % x, want % x", decoded, frame)
		}
	})
}

func FuzzDecodeHostileCodestream(f *testing.F) {
	valid, err := NewEncoder().EncodeFrame(context.Background(), []byte{0, 64, 128, 255}, encoderMetadata(2, 2, 1, 8, "MONOCHROME2"))
	if err != nil {
		f.Fatal(err)
	}
	f.Add(valid.Data)
	f.Add(charLSLossless2x2)
	f.Add([]byte{0xff, 0xd8, 0xff, 0xd9})
	f.Add([]byte{0xff, 0xd8})
	for _, seed := range hostileSeeds(f) {
		f.Add(seed.stream)
	}
	f.Fuzz(func(t *testing.T, stream []byte) {
		_, _ = decodeFrame(stream, hostileMetadata(stream))
	})
}

// hostileMetadata describes the frame a codestream's headers declare, so the
// decoder reaches its entropy data, when the headers parse and the frame has
// at most 4096 samples; otherwise it describes a 2x2 8-bit monochrome frame.
func hostileMetadata(stream []byte) pixeldata.Metadata {
	header, err := parseFrameHeader(stream)
	if err != nil || header.rows*header.columns*header.components > 1<<12 {
		return encoderMetadata(2, 2, 1, 8, "MONOCHROME2")
	}
	photometric := "MONOCHROME2"
	if header.components == 3 {
		photometric = "RGB"
	}
	bitsAllocated := uint16(8)
	if header.precision > 8 {
		bitsAllocated = 16
	}
	metadata := encoderMetadata(uint16(header.rows), uint16(header.columns), uint16(header.components), bitsAllocated, photometric)
	metadata.BitsStored = uint16(header.precision)
	metadata.HighBit = metadata.BitsStored - 1
	return metadata
}
