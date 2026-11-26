package jpegls

import (
	"context"
	"encoding/binary"
	"math"
	"testing"
)

// ctBenchFrame is a 512×512 CT-like slice, 12 bits stored in 16, like the
// synthetic CT of the comparative benchmarks (#1268): air, a soft-tissue
// ellipse with a bright structure, and deterministic noise.
func ctBenchFrame() []byte {
	const rows, cols = 512, 512
	frame := make([]byte, rows*cols*2)
	seed := uint32(12345)
	for y := 0; y < rows; y++ {
		for x := 0; x < cols; x++ {
			dx, dy := (float64(x)-256)/200, (float64(y)-256)/160
			value := 24.0 // air, -1000 HU at intercept -1024
			if dx*dx+dy*dy <= 1 {
				value = 1064 // soft tissue
				bx, by := (float64(x)-300)/40, (float64(y)-230)/30
				if bx*bx+by*by <= 1 {
					value = 2024 + 400*math.Cos(float64(x)/9)
				}
			}
			seed = seed*1664525 + 1013904223
			value += float64(int(seed>>24)%21 - 10)
			binary.LittleEndian.PutUint16(frame[2*(y*cols+x):], uint16(math.Max(0, math.Min(4095, value))))
		}
	}
	return frame
}

func BenchmarkCTEncode(b *testing.B) {
	frame := ctBenchFrame()
	metadata := encoderMetadata(512, 512, 1, 16, "MONOCHROME2")
	metadata.BitsStored, metadata.HighBit = 12, 11
	encoder := NewEncoder()
	b.SetBytes(int64(len(frame)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := encoder.EncodeFrame(context.Background(), frame, metadata); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCTDecode(b *testing.B) {
	frame := ctBenchFrame()
	metadata := encoderMetadata(512, 512, 1, 16, "MONOCHROME2")
	metadata.BitsStored, metadata.HighBit = 12, 11
	encoded, err := NewEncoder().EncodeFrame(context.Background(), frame, metadata)
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(frame)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := decodeFrame(encoded.Data, metadata); err != nil {
			b.Fatal(err)
		}
	}
}

// TestGolombKMatchesTheT87Loop checks the closed form of golombK against the
// loop of T.87 A.5.1 over the reachable range of A and N.
func TestGolombKMatchesTheT87Loop(t *testing.T) {
	var s locoState
	loop := func(a, n int) int {
		k := 0
		for n<<k < a {
			k++
		}
		return k
	}
	check := func(a, n int) {
		if got, want := s.golombK(a, n), loop(a, n); got != want {
			t.Fatalf("golombK(%d, %d) = %d, want %d", a, n, got, want)
		}
	}
	for n := 1; n <= 300; n++ {
		for a := 0; a <= 1<<14; a++ {
			check(a, n)
		}
	}
	for _, n := range []int{1, 2, 3, 64, 255, 256, 4095, 65535} {
		for shift := 0; shift < 32; shift++ {
			for _, delta := range []int{-1, 0, 1} {
				if a := n<<shift + delta; a >= 0 {
					check(a, n)
				}
			}
		}
	}
}

// The cached parameter must still be the smallest k with N<<k >= A after
// adaptation, including RESET halving and large early errors. Compare to the
// T.87 A.5.1 definition rather than to the optimized closed form.
func TestRegularGolombCacheMatchesT87Adaptation(t *testing.T) {
	for _, reset := range []int{3, 16, 64, 65535} {
		for _, near := range []int{0, 255} {
			preset := presetCodingParameters{maxval: 65535, near: near, reset: reset}
			preset.t1, preset.t2, preset.t3 = defaultNearThresholds(preset.maxval, near)
			st := newLocoStateForPreset(preset)
			seed := uint32(1296)
			for i := 0; i < 100000; i++ {
				seed = seed*1664525 + 1013904223
				err := int(seed%uint32(st.range_)) - st.range_/2
				st.updateRegular(13, err)
				cx := &st.ctx[13]
				want := 0
				for cx.n<<want < cx.a {
					want++
				}
				if cx.k != want {
					t.Fatalf("RESET=%d NEAR=%d sample=%d A=%d N=%d: k=%d want %d", reset, near, i, cx.a, cx.n, cx.k, want)
				}
			}
		}
	}
}
