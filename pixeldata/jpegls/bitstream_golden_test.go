package jpegls

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata"
)

// bitstreamGoldenCase is an input whose encoded bitstream is pinned by its
// SHA-256, so changes to the bit I/O or the coding loops (#1277) must
// reproduce every byte. The inputs cover 8, 12 and 16 bits, runs and their
// interruptions, the limited-Golomb escape, 0xFF bit stuffing, the three
// interleave modes and near-lossless coding.
type bitstreamGoldenCase struct {
	name     string
	frame    []byte
	metadata pixeldata.Metadata
	opts     EncoderOptions
	want     string
}

func goldenNoise(n int, seed uint32, mask uint32) []uint32 {
	out := make([]uint32, n)
	for i := range out {
		seed = seed*1664525 + 1013904223
		out[i] = (seed >> 8) & mask
	}
	return out
}

func golden16(rows, cols int, sample func(x, y int) uint16) []byte {
	frame := make([]byte, rows*cols*2)
	for y := 0; y < rows; y++ {
		for x := 0; x < cols; x++ {
			binary.LittleEndian.PutUint16(frame[2*(y*cols+x):], sample(x, y))
		}
	}
	return frame
}

func bitstreamGoldenCases() []bitstreamGoldenCase {
	ct := ctBenchFrame()
	ct12 := encoderMetadata(512, 512, 1, 16, "MONOCHROME2")
	ct12.BitsStored, ct12.HighBit = 12, 11

	noise16 := goldenNoise(97*61, 7, 0xffff)
	full16 := golden16(61, 97, func(x, y int) uint16 { return uint16(noise16[y*97+x]) })
	smooth16 := golden16(64, 80, func(x, y int) uint16 { return uint16(30000 + 40*x + 25*y + int(noise16[y*80+x]%7)) })
	m16 := encoderMetadata(61, 97, 1, 16, "MONOCHROME2")
	m16s := encoderMetadata(64, 80, 1, 16, "MONOCHROME2")

	noise8 := goldenNoise(120*90, 3, 0xff)
	mono8 := make([]byte, 120*90)
	for i := range mono8 {
		switch {
		case i%120 < 40: // flat runs
			mono8[i] = 200
		case i%120 < 80: // ramps
			mono8[i] = byte(i%120*3 + i/120)
		default: // noise, including 0xff samples
			mono8[i] = byte(noise8[i])
		}
	}
	m8 := encoderMetadata(90, 120, 1, 8, "MONOCHROME2")

	rgb := make([]byte, 50*40*3)
	for i := range rgb {
		pixel := i / 3
		rgb[i] = byte((pixel%50)*5 + (i%3)*60 + int(noise8[i%len(noise8)]%5))
		if pixel%50 < 12 {
			rgb[i] = byte(10 + i%3)
		}
	}
	mrgb := encoderMetadata(40, 50, 3, 8, "RGB")

	return []bitstreamGoldenCase{
		{name: "ct-12bit", frame: ct, metadata: ct12},
		{name: "noise-16bit", frame: full16, metadata: m16},
		{name: "smooth-16bit", frame: smooth16, metadata: m16s},
		{name: "mixed-8bit", frame: mono8, metadata: m8},
		{name: "mixed-8bit-near3", frame: mono8, metadata: m8, opts: EncoderOptions{Near: 3, AllowLossy: true}},
		{name: "ct-12bit-near2", frame: ct, metadata: ct12, opts: EncoderOptions{Near: 2, AllowLossy: true}},
		{name: "rgb-ilv0", frame: rgb, metadata: mrgb},
		{name: "rgb-ilv1", frame: rgb, metadata: mrgb, opts: EncoderOptions{Interleave: InterleaveLine}},
		{name: "rgb-ilv2", frame: rgb, metadata: mrgb, opts: EncoderOptions{Interleave: InterleaveSample}},
		{name: "rgb-ilv2-near1", frame: rgb, metadata: mrgb, opts: EncoderOptions{Interleave: InterleaveSample, Near: 1, AllowLossy: true}},
	}
}

var bitstreamGoldenSHA256 = map[string]string{
	"ct-12bit":         "ced1c42888f29f0fcddbc0cb620dfa68e55d845cfa9ce61cc6693356b81ac0c3",
	"noise-16bit":      "c2d216e2825a352e3f0ab555876a0756d7100786ad2e3355a12522a41b46f4f6",
	"smooth-16bit":     "7cd297f1ae70770fbaa0399a64bebd42d3e238728ae3a19a4155346675f3e10d",
	"mixed-8bit":       "fab0f734812f1c96dbc46df640edfd45a5fd69d6b1a875422244d4bd1a22aaf5",
	"mixed-8bit-near3": "45ff5141b3f1de0e9ad5233431e356fb6221ecd7b2aedaa6ed9aab9a4013c96f",
	"ct-12bit-near2":   "716a55858d9043576ab7f98a943e15208401c65d67f83e39feb75258873f098e",
	"rgb-ilv0":         "c04da9faebc7dac0188bf516ead4de991b58aada95bda9b435b7d02437c9b6b9",
	"rgb-ilv1":         "7c1102405afcad62bf6a268c976ea2de4bbbfcb1dfced6685d942e0f3427c095",
	"rgb-ilv2":         "2334f419cb189e0656685a6e98e37a714a932947f21ee34b89ff666dcb7e22c8",
	"rgb-ilv2-near1":   "4ec7aa60d609355018f17a0ef5732f12972cf4e9e1c6bf4d130f7a27f60850ed",
}

func TestEncoderBitstreamsMatchPinnedHashes(t *testing.T) {
	for _, test := range bitstreamGoldenCases() {
		encoder, err := NewEncoderWithOptions(test.opts)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := encoder.EncodeFrame(context.Background(), test.frame, test.metadata)
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		sum := sha256.Sum256(encoded.Data)
		got := hex.EncodeToString(sum[:])
		if want := bitstreamGoldenSHA256[test.name]; got != want {
			t.Errorf("%s: bitstream SHA-256 %s, want %s", test.name, got, want)
		}
		decoded, err := decodeFrameModeContext(context.Background(), encoded.Data, test.metadata, test.opts.Near > 0)
		if err != nil {
			t.Fatalf("%s: decode: %v", test.name, err)
		}
		if test.opts.Near == 0 && string(decoded) != string(test.frame) {
			t.Errorf("%s: lossless round trip differs", test.name)
		}
	}
}
