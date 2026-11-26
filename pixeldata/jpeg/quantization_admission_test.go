package jpeg

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata"
)

func TestExtendedJPEGWideQuantizationTableAndMissingEntropy(t *testing.T) {
	base := process4Fixture(t)
	dqt := jpegMarkerBytes(t, base, 0xdb)
	at := bytes.Index(base, dqt)
	if len(dqt) != 69 || dqt[4] != 0 || at < 0 {
		t.Fatal("expected a single eight-bit table 0 in the reference fixture")
	}
	// Preserve the independently encoded table's values but express each
	// value in a two-byte field. The reference samples must stay identical.
	wide := []byte{0x10}
	for _, value := range dqt[5:] {
		wide = binary.BigEndian.AppendUint16(wide, uint16(value))
	}
	withTable := func(payload []byte) []byte {
		out := appendExtendedSegment(bytes.Clone(base[:at]), 0xdb, payload)
		return append(out, base[at+len(dqt):]...)
	}
	t.Run("equivalent-sixteen-bit-coefficients", func(t *testing.T) {
		frames, err := decodeProcess4Fixture(withTable(wide), 8, 8, 12)
		if err != nil {
			t.Fatal(err)
		}
		assertProcess4Samples(t, frames, process4DjpegReference, 8)
	})
	zero := func(coefficients []byte, at, size int) []byte {
		out := bytes.Clone(coefficients)
		clear(out[at : at+size])
		return out
	}
	for _, tc := range []struct {
		name  string
		table []byte
	}{
		{"last-eight-bit-coefficient-zero", zero(dqt[4:], 64, 1)},
		{"interior-sixteen-bit-coefficient-zero", zero(wide, 35, 2)},
		{"last-sixteen-bit-coefficient-zero", zero(wide, 127, 2)},
		{"last-sixteen-bit-coefficient-truncated", wide[:len(wide)-1]},
		{"short-second-table", append(bytes.Clone(wide), 0x01, 1, 2, 3)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frames, err := decodeProcess4Fixture(withTable(tc.table), 8, 8, 12)
			if !errors.Is(err, ErrInvalidFragment) || len(frames.Data) != 0 {
				t.Fatalf("decode produced %+v, %v, want invalid fragment", frames, err)
			}
		})
	}
	t.Run("headers-are-structurally-complete-without-entropy", func(t *testing.T) {
		sos := jpegMarkerBytes(t, base, 0xda)
		at := bytes.Index(base, sos)
		missing := append(bytes.Clone(base[:at+len(sos)]), 0xff, 0xd9)
		info, err := pixeldata.InspectJPEGFrame(context.Background(), missing, int64(len(missing)))
		if err != nil || info.Rows != 8 || info.Columns != 8 || info.Precision != 12 {
			t.Fatalf("header inspection = %+v, %v", info, err)
		}
		frames, err := decodeProcess4Fixture(missing, 8, 8, 12)
		if !errors.Is(err, ErrInvalidFragment) || len(frames.Data) != 0 {
			t.Fatalf("header-only decode produced %+v, %v", frames, err)
		}
	})
}
