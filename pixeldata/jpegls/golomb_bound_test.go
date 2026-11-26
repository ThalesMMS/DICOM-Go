package jpegls

import (
	"bytes"
	"context"
	"errors"
	"math"
	"math/big"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/pixeldata"
)

// decodeTimeout bounds each decode below, so a decoder that stops
// terminating fails the test instead of hanging the package.
const decodeTimeout = 30 * time.Second

// decodeWithin decodes stream in a goroutine and fails the test when it does
// not return within decodeTimeout (the goroutine is then left spinning).
func decodeWithin(t *testing.T, stream []byte, metadata pixeldata.Metadata) ([]byte, error) {
	t.Helper()
	type result struct {
		decoded []byte
		err     error
	}
	done := make(chan result, 1)
	go func() {
		decoded, err := decodeFrame(stream, metadata)
		done <- result{decoded, err}
	}()
	select {
	case r := <-done:
		return r.decoded, r.err
	case <-time.After(decodeTimeout):
		t.Fatalf("decode did not return within %v", decodeTimeout)
		return nil, nil
	}
}

const forgedGolombWidth = 64

// forgedGolombGrowthStream returns a one-row, 16-bit lossless codestream
// whose codes drive the Golomb parameter k of one regular context upward
// until N<<k can no longer reach A in an int64.
//
// The first sample has zero neighbors, so it starts run mode; a 0 bit
// interrupts the run at once and the interruption codes the error 1000.
// Every later sample then has Ra >= T3 and Rb = Rc = Rd = 0, which is
// regular context 4 with a negative sign. Each of its codes is the longest
// unary prefix that is not an escape, a one, and k low bits ending in 1: an
// odd mapped value, so a negative error of magnitude about 23*2^k that the
// sign turns into a large positive sample, keeping the next sample in the
// same context. A, N and k are tracked here as an unbounded decoder would
// track them; the codes stop once k would pass 62.
func forgedGolombGrowthStream(tb testing.TB) []byte {
	tb.Helper()
	st := newLocoState(65535)
	w := &bitWriter{}
	w.buf = append(w.buf,
		0xff, 0xd8,
		0xff, 0xf7, 0x00, 0x0b, 16, 0x00, 0x01, 0x00, forgedGolombWidth, 0x01, 0x01, 0x11, 0x00,
		0xff, 0xda, 0x00, 0x08, 0x01, 0x01, 0x00, 0x00, 0x00, 0x00,
	)
	ri := &st.ctx[366]
	w.writeBit(0)
	writeLimitedGolomb(w, 2*1000-1, st.golombK(ri.a+ri.n>>1, ri.n), st.limit-runJ[0]-1, st.qbpp)

	maxUnary := st.limit - st.qbpp - 2
	cx := st.ctx[4]
	a, n, k := big.NewInt(int64(cx.a)), cx.n, cx.k
	nShiftK := func(k int) *big.Int { return new(big.Int).Lsh(big.NewInt(int64(n)), uint(k)) }
	for x := 1; x < forgedGolombWidth && k <= 62; x++ {
		w.writeZeros(maxUnary)
		w.writeBit(1)
		w.writeZeros(k - 1)
		w.writeBit(1)
		magnitude := new(big.Int).Lsh(big.NewInt(int64(maxUnary)), uint(k))
		magnitude.Add(magnitude, big.NewInt(2)).Rsh(magnitude, 1)
		a.Add(a, magnitude)
		if n == st.reset {
			a.Rsh(a, 1)
			n >>= 1
		}
		n++
		for nShiftK(k).Cmp(a) < 0 {
			k++
		}
		for k > 0 && nShiftK(k-1).Cmp(a) >= 0 {
			k--
		}
	}
	w.padToByte()
	return append(w.buf, 0xff, 0xd9)
}

func TestDecoderRejectsForgedGolombParameterGrowth(t *testing.T) {
	stream := forgedGolombGrowthStream(t)
	_, err := decodeWithin(t, stream, encoderMetadata(1, forgedGolombWidth, 1, 16, "MONOCHROME2"))
	if !errors.Is(err, ErrInvalidCodestream) {
		t.Fatalf("decode error = %v, want %v", err, ErrInvalidCodestream)
	}
	if !errors.Is(err, errOutsideModularRange) {
		t.Fatalf("decode error = %v, want the modulo range rejection", err)
	}
}

func TestUpdateRegularCapsGolombParameter(t *testing.T) {
	st := newLocoState(65535)
	cx := &st.ctx[4]
	// An accumulator no N<<k reaches in an int: without the cap the loop
	// never ends.
	cx.a, cx.n = math.MaxInt, 2
	done := make(chan struct{})
	go func() {
		st.updateRegular(4, 0)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(decodeTimeout):
		t.Fatal("updateRegular did not return")
	}
	if cx.k != maxGolombK {
		t.Fatalf("k = %d, want the cap %d", cx.k, maxGolombK)
	}
}

func TestInModularRangeMatchesModRange(t *testing.T) {
	for _, preset := range []presetCodingParameters{
		{maxval: 1}, {maxval: 2}, {maxval: 3}, {maxval: 128}, {maxval: 255},
		{maxval: 255, near: 1}, {maxval: 255, near: 3}, {maxval: 4095, near: 7}, {maxval: 65535},
	} {
		st := &locoState{maxval: preset.maxval, near: preset.near}
		st.range_ = (preset.maxval+2*preset.near)/(2*preset.near+1) + 1
		low, high := -(st.range_ / 2), (st.range_+1)/2-1
		for err := -2 * st.range_; err <= 2*st.range_; err++ {
			if got, want := st.inModularRange(err), err >= low && err <= high; got != want {
				t.Fatalf("RANGE %d: inModularRange(%d) = %v, want %v", st.range_, err, got, want)
			}
		}
		// modRange reduces every prediction error, -(RANGE-1) through
		// RANGE-1, into the range and leaves values already in it alone.
		seen := map[int]bool{}
		for err := 1 - st.range_; err < st.range_; err++ {
			reduced := st.modRange(err)
			if !st.inModularRange(reduced) {
				t.Fatalf("RANGE %d: modRange(%d) = %d is outside the range", st.range_, err, reduced)
			}
			if st.inModularRange(err) && reduced != err {
				t.Fatalf("RANGE %d: modRange(%d) = %d", st.range_, err, reduced)
			}
			seen[reduced] = true
		}
		if len(seen) != st.range_ {
			t.Fatalf("RANGE %d: modRange produces %d values", st.range_, len(seen))
		}
	}
}

// TestDecoderAcceptsModularRangeBounds round-trips frames whose prediction
// errors reach both ends of the modulo range: differences of RANGE/2 and
// RANGE/2-1 between neighbors.
func TestDecoderAcceptsModularRangeBounds(t *testing.T) {
	for _, bits := range []uint16{8, 16} {
		maxval := 1<<bits - 1
		half := (maxval + 1) / 2
		levels := []int{0, half, half - 1, maxval, 0, half - 1, maxval, half, 1, half + 1}
		const rows, columns = 8, 16
		frame := make([]byte, 0, rows*columns*int(bits/8))
		for y := 0; y < rows; y++ {
			for x := 0; x < columns; x++ {
				v := levels[(x*(y+1)+y)%len(levels)]
				if bits == 8 {
					frame = append(frame, byte(v))
				} else {
					frame = append(frame, byte(v), byte(v>>8))
				}
			}
		}
		metadata := encoderMetadata(rows, columns, 1, bits, "MONOCHROME2")
		decoded, err := decodeWithin(t, encodedFrame(t, frame, metadata), metadata)
		if err != nil {
			t.Fatalf("%d-bit: %v", bits, err)
		}
		if !bytes.Equal(decoded, frame) {
			t.Fatalf("%d-bit: decoded frame differs from the source", bits)
		}
	}
}

// hostileSeed is a forged codestream from one of the six JPEG-LS decoder
// fixes DCMTK made between June and August 2026. want is the decoded frame,
// or nil when the decoder must reject the stream.
type hostileSeed struct {
	name   string
	stream []byte
	want   []byte
}

func hostileSeeds(tb testing.TB) []hostileSeed {
	tb.Helper()
	sos := bytes.Index(charLSLossless2x2, []byte{0xff, 0xda})
	if sos < 0 {
		tb.Fatal("SOS not found")
	}
	entropy := sos + 2 + int(charLSLossless2x2[sos+3])

	// A SOS segment length larger than the bytes that remain.
	longSOS := append([]byte(nil), charLSLossless2x2...)
	longSOS[sos+2], longSOS[sos+3] = 0x00, 0xff

	// RESET 256 with MAXVAL 255: above max(255, MAXVAL).
	largeReset := insertAt(charLSLossless2x2, sos, lseID1(255, 0, 0, 0, 256))

	// Entropy data cut short, so decoding needs bits beyond those before EOI.
	shortEntropy := append(append([]byte(nil), charLSLossless2x2[:entropy+2]...), 0xff, 0xd9)

	return []hostileSeed{
		{name: "SOS length beyond the remaining bytes", stream: longSOS},
		{name: "RESET above 255", stream: largeReset},
		{name: "Golomb k of 16 or more", stream: forgedGolombGrowthStream(tb)},
		gradientOfMaxvalSeed(tb),
		{name: "read beyond EOI", stream: shortEntropy},
		customResetSampleInterleavedSeed(tb),
	}
}

// gradientOfMaxvalSeed codes a checkerboard of 0 and a power-of-two MAXVAL
// (128, signalled by LSE), so local gradients have magnitude MAXVAL.
func gradientOfMaxvalSeed(tb testing.TB) hostileSeed {
	tb.Helper()
	const maxval = 128
	metadata := encoderMetadata(4, 4, 1, 8, "MONOCHROME2")
	frame := make([]byte, 16)
	for i := range frame {
		if (i/4+i%4)%2 == 1 {
			frame[i] = maxval
		}
	}
	t1, t2, t3 := defaultThresholds(maxval)
	preset := presetCodingParameters{maxval: maxval, t1: t1, t2: t2, t3: t3, reset: basicReset}
	stream := encodeWithPreset(tb, frame, metadata, preset, InterleaveNone)
	stream = insertAt(stream, bytes.Index(stream, []byte{0xff, 0xda}), lseID1(maxval, 0, 0, 0, 0))
	return hostileSeed{name: "gradient of MAXVAL with a power-of-two MAXVAL", stream: stream, want: frame}
}

// customResetSampleInterleavedSeed codes a sample-interleaved RGB frame
// with a non-default RESET signalled by LSE.
func customResetSampleInterleavedSeed(tb testing.TB) hostileSeed {
	tb.Helper()
	const reset = 32
	metadata := encoderMetadata(4, 4, 3, 8, "RGB")
	frame := make([]byte, 48)
	for i := range frame {
		frame[i] = byte(i * 37)
	}
	t1, t2, t3 := defaultThresholds(255)
	preset := presetCodingParameters{maxval: 255, t1: t1, t2: t2, t3: t3, reset: reset}
	stream := encodeWithPreset(tb, frame, metadata, preset, InterleaveSample)
	stream = insertAt(stream, bytes.Index(stream, []byte{0xff, 0xda}), lseID1(255, 0, 0, 0, reset))
	return hostileSeed{name: "custom RESET with three sample-interleaved components", stream: stream, want: frame}
}

func encodeWithPreset(tb testing.TB, frame []byte, metadata pixeldata.Metadata, preset presetCodingParameters, ilv InterleaveMode) []byte {
	tb.Helper()
	params, err := validateEncoderInput(metadata)
	if err != nil {
		tb.Fatal(err)
	}
	samples, err := nativeToSamples(frame, metadata, params)
	if err != nil {
		tb.Fatal(err)
	}
	stream, err := encodeFrameWithPresetInterleave(context.Background(), samples, metadata, params, preset, ilv)
	if err != nil {
		tb.Fatal(err)
	}
	return stream
}

func TestDecoderHandlesHostileSeeds(t *testing.T) {
	for _, seed := range hostileSeeds(t) {
		t.Run(seed.name, func(t *testing.T) {
			decoded, err := decodeWithin(t, seed.stream, hostileMetadata(seed.stream))
			if seed.want == nil {
				// Exhausted entropy data reports io.ErrUnexpectedEOF; the
				// other rejections ErrInvalidCodestream.
				if err == nil {
					t.Fatal("decode error = nil, want rejection")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(decoded, seed.want) {
				t.Fatalf("decoded = % x, want % x", decoded, seed.want)
			}
		})
	}
}
