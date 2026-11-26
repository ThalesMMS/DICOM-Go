package jpegxl

import (
	"encoding/binary"
	"errors"
	"math"
	"slices"
	"testing"
)

func TestJPEGXLContainerBoxBoundaries(t *testing.T) {
	want := &Image{Width: 3, Height: 2, Channels: 1, BitsPerSample: 8, Samples: []int32{1, 9, 42, 13, 80, 253}}
	bare, err := EncodeLossless(want, LosslessOptions{BitsPerSample: 8})
	if err != nil {
		t.Fatal(err)
	}
	box := func(kind string, payload []byte) []byte {
		out := binary.BigEndian.AppendUint32(nil, uint32(8+len(payload)))
		out = append(out, kind...)
		return append(out, payload...)
	}
	extendedHeader := func(kind string, length uint64) []byte {
		out := binary.BigEndian.AppendUint32(nil, 1)
		out = append(out, kind...)
		return binary.BigEndian.AppendUint64(out, length)
	}
	part := func(index uint32, payload []byte) []byte {
		return box("jxlp", append(binary.BigEndian.AppendUint32(nil, index), payload...))
	}
	container := func(boxes ...[]byte) []byte {
		out := append([]byte(nil), containerSignature...)
		out = append(out, box("ftyp", []byte{'j', 'x', 'l', ' ', 0, 0, 0, 0, 'j', 'x', 'l', ' '})...)
		for _, b := range boxes {
			out = append(out, b...)
		}
		return out
	}
	split := len(bare) / 2
	first := part(0, bare[:split])
	final := part(1<<31|1, bare[split:])
	parseToEnd := append([]byte{0, 0, 0, 0, 'j', 'x', 'l', 'c'}, bare...)
	full := box("jxlc", bare)

	valid := []struct {
		name string
		wire []byte
	}{
		{"extended-size", container(append(extendedHeader("jxlc", uint64(16+len(bare))), bare...))},
		{"size-zero-consumes-rest", container(parseToEnd)},
		{"ordered-parts-with-unrelated-box", container(first, box("free", []byte{0xff, 0x0a}), final)},
		{"single-final-part", container(part(1<<31, bare))},
	}
	for _, tc := range valid {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Decode(tc.wire)
			if err != nil || got == nil || got.Width != want.Width || got.Height != want.Height ||
				got.Channels != want.Channels || got.BitsPerSample != want.BitsPerSample || !slices.Equal(got.Samples, want.Samples) {
				t.Fatalf("Decode() = %+v, %v, want %+v", got, err, want)
			}
		})
	}

	invalid := []struct {
		name string
		wire []byte
	}{
		{"extended-size-max-signed", container(extendedHeader("jxlc", math.MaxInt64))},
		{"extended-size-max-unsigned", container(extendedHeader("jxlc", math.MaxUint64))},
		{"extended-size-smaller-than-header", container(extendedHeader("jxlc", 15))},
		{"extended-size-truncated-header", container(extendedHeader("jxlc", 16)[:15])},
		{"length-one-past-end", container(extendedHeader("jxlc", uint64(17+len(bare))), bare)},
		{"reordered-parts", container(final, first)},
		{"repeated-part-index", container(first, first, final)},
		{"missing-final-part", container(first, part(1, bare[split:]))},
		{"part-after-final", container(first, final, part(2, nil))},
		{"part-without-index", container(box("jxlp", []byte{1, 2, 3}))},
		{"whole-and-partial", container(full, first, final)},
		{"partial-and-whole", container(first, final, full)},
		{"duplicate-whole", container(full, full)},
		{"no-codestream", container(box("free", bare))},
		{"compressed-codestream-box", container(box("brob", append([]byte("jxlc"), bare...)))},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := Decode(tc.wire); !errors.Is(err, ErrInvalidCodestream) || got != nil {
				t.Fatalf("Decode() = %+v, %v, want typed error and no image", got, err)
			}
			if got, err := ReconstructJPEG(tc.wire); !errors.Is(err, ErrInvalidCodestream) || got != nil {
				t.Fatalf("ReconstructJPEG() returned %d bytes, %v, want typed error and no JPEG", len(got), err)
			}
		})
	}
}
