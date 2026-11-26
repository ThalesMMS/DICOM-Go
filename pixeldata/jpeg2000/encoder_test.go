package jpeg2000

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"math/rand"
	"reflect"
	"sync"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestNewEncoderValidatesOptions(t *testing.T) {
	tests := []struct {
		name string
		opts Options
		want error
	}{
		{"lossless part 1", Options{TransferSyntaxUID: transfer.JPEG2000LosslessOnly.UID}, nil},
		{"lossless htj2k", Options{TransferSyntaxUID: transfer.HTJ2KLossless.UID, ColorTransform: true, Levels: 3}, nil},
		{"lossless rpcl", Options{TransferSyntaxUID: transfer.HTJ2KLosslessRPCL.UID}, nil},
		{"lossy part 1", Options{TransferSyntaxUID: transfer.JPEG2000.UID, StepSize: 4, AllowLossy: true}, nil},
		{"lossy htj2k", Options{TransferSyntaxUID: transfer.HTJ2K.UID, StepSize: 0.25, AllowLossy: true}, nil},
		{"lossy needs authorization", Options{TransferSyntaxUID: transfer.HTJ2K.UID, StepSize: 4}, pixeldata.ErrTranscodeLossyDisallowed},
		{"lossy needs a step", Options{TransferSyntaxUID: transfer.JPEG2000.UID, AllowLossy: true}, pixeldata.ErrUnsupportedEncoderMetadata},
		{"step not a number", Options{TransferSyntaxUID: transfer.JPEG2000.UID, StepSize: math.NaN(), AllowLossy: true}, pixeldata.ErrUnsupportedEncoderMetadata},
		{"step too large", Options{TransferSyntaxUID: transfer.JPEG2000.UID, StepSize: 1 << 20, AllowLossy: true}, pixeldata.ErrUnsupportedEncoderMetadata},
		{"lossless rejects a step", Options{TransferSyntaxUID: transfer.JPEG2000LosslessOnly.UID, StepSize: 1}, pixeldata.ErrUnsupportedEncoderMetadata},
		{"part 2 is not encoded", Options{TransferSyntaxUID: transfer.JPEG2000Part2Lossless.UID}, pixeldata.ErrUnsupportedEncoderMetadata},
		{"negative levels", Options{TransferSyntaxUID: transfer.HTJ2KLossless.UID, Levels: -1}, pixeldata.ErrUnsupportedEncoderMetadata},
		{"too many levels", Options{TransferSyntaxUID: transfer.HTJ2KLossless.UID, Levels: 33}, pixeldata.ErrUnsupportedEncoderMetadata},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, err := NewEncoder(tt.opts)
			if tt.want == nil {
				if err != nil || e == nil {
					t.Fatalf("NewEncoder() = %v, %v", e, err)
				}
				return
			}
			if !errors.Is(err, tt.want) {
				t.Fatalf("NewEncoder() error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestEncoderCapabilitiesAndRegistration(t *testing.T) {
	registry := pixeldata.NewMemoryEncoderRegistry()
	if err := RegisterEncoders(registry); err != nil {
		t.Fatal(err)
	}
	if err := RegisterLossyEncoders(registry, LossyOptions{StepSize: 2, ColorTransform: true, AllowLossy: true}); err != nil {
		t.Fatal(err)
	}
	want := []string{transfer.HTJ2KLossless.UID, transfer.HTJ2KLosslessRPCL.UID, transfer.HTJ2K.UID, transfer.JPEG2000LosslessOnly.UID, transfer.JPEG2000.UID}
	if got := registry.RegisteredEncoderUIDs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("registered %v, want %v", got, want)
	}
	if err := RegisterEncoders(registry); !errors.Is(err, pixeldata.ErrEncoderAlreadyRegistered) {
		t.Fatalf("second registration error = %v", err)
	}
	if err := RegisterEncoders(nil); !errors.Is(err, pixeldata.ErrEncoderRegistryNil) {
		t.Fatalf("nil registry error = %v", err)
	}
	if err := RegisterLossyEncoders(pixeldata.NewMemoryEncoderRegistry(), LossyOptions{StepSize: 2}); !errors.Is(err, pixeldata.ErrTranscodeLossyDisallowed) {
		t.Fatalf("unauthorized lossy registration error = %v", err)
	}
	for _, tt := range []struct {
		uid      string
		lossless bool
		method   string
		outputs  []string
	}{
		{transfer.JPEG2000LosslessOnly.UID, true, "", nil},
		{transfer.HTJ2KLossless.UID, true, "", nil},
		{transfer.HTJ2KLosslessRPCL.UID, true, "", nil},
		{transfer.JPEG2000.UID, false, MethodPart1, []string{"YBR_ICT"}},
		{transfer.HTJ2K.UID, false, MethodHTJ2K, []string{"YBR_ICT"}},
	} {
		encoder, ok := registry.GetEncoder(tt.uid)
		if !ok {
			t.Fatalf("%s not registered", tt.uid)
		}
		caps := encoder.Capabilities()
		if caps.TransferSyntaxUID != tt.uid || caps.Lossless != tt.lossless || caps.LossyMethod != tt.method || !reflect.DeepEqual(caps.OutputPhotometricInterpretations, tt.outputs) || !caps.SupportsMultiFrame || caps.Backend != "pure-go" {
			t.Fatalf("%s capabilities = %+v", tt.uid, caps)
		}
		if !reflect.DeepEqual(caps.PixelRepresentations, []uint16{0, 1}) || !reflect.DeepEqual(caps.BitsAllocated, []uint16{8, 16}) {
			t.Fatalf("%s sample capabilities = %+v", tt.uid, caps)
		}
	}
	rct, _ := NewEncoder(Options{TransferSyntaxUID: transfer.HTJ2KLossless.UID, ColorTransform: true})
	if got := rct.Capabilities().OutputPhotometricInterpretations; !reflect.DeepEqual(got, []string{"YBR_RCT"}) {
		t.Fatalf("RCT output photometric = %v", got)
	}
}

func TestEncodeFrameRejectsUnsupportedMetadata(t *testing.T) {
	base := syntheticFrame(8, 6, 1, 12, 16, false, 1)
	rgb := syntheticFrame(8, 6, 3, 8, 8, false, 2)
	lossless, _ := NewEncoder(Options{TransferSyntaxUID: transfer.JPEG2000LosslessOnly.UID})
	lossy, _ := NewEncoder(Options{TransferSyntaxUID: transfer.HTJ2K.UID, StepSize: 1, AllowLossy: true})
	tests := []struct {
		name    string
		encoder *Encoder
		frame   []byte
		mutate  func(*pixeldata.Metadata)
		want    error
	}{
		{"rows", lossless, base.frame, func(m *pixeldata.Metadata) { m.Rows = 0 }, pixeldata.ErrUnsupportedEncoderMetadata},
		{"bits allocated", lossless, base.frame, func(m *pixeldata.Metadata) { m.BitsAllocated = 32 }, pixeldata.ErrUnsupportedEncoderMetadata},
		{"bits stored", lossless, base.frame, func(m *pixeldata.Metadata) { m.BitsStored = 17 }, pixeldata.ErrUnsupportedEncoderMetadata},
		{"high bit", lossless, base.frame, func(m *pixeldata.Metadata) { m.HighBit = 15 }, pixeldata.ErrUnsupportedEncoderMetadata},
		{"photometric", lossless, base.frame, func(m *pixeldata.Metadata) { m.PhotometricInterpretation = "YBR_FULL" }, pixeldata.ErrUnsupportedEncoderMetadata},
		{"lossy palette", lossy, base.frame, func(m *pixeldata.Metadata) { m.PhotometricInterpretation = "PALETTE COLOR" }, pixeldata.ErrUnsupportedEncoderMetadata},
		{"signed palette", lossless, base.frame, func(m *pixeldata.Metadata) {
			m.PhotometricInterpretation = "PALETTE COLOR"
			m.PixelRepresentation = 1
		}, pixeldata.ErrUnsupportedEncoderMetadata},
		{"frames", lossless, base.frame, func(m *pixeldata.Metadata) { m.NumberOfFrames = 0 }, pixeldata.ErrUnsupportedEncoderMetadata},
		{"length", lossless, base.frame[1:], func(*pixeldata.Metadata) {}, pixeldata.ErrPixelDataSizeMismatch},
		{"planar", lossless, rgb.frame, func(m *pixeldata.Metadata) { m.PlanarConfiguration = 1 }, pixeldata.ErrUnsupportedEncoderMetadata},
		{"rgb without planar", lossless, rgb.frame, func(m *pixeldata.Metadata) { m.PlanarConfigurationPresent = false }, pixeldata.ErrUnsupportedEncoderMetadata},
		{"signed rgb", lossless, rgb.frame, func(m *pixeldata.Metadata) { m.PixelRepresentation = 1 }, pixeldata.ErrUnsupportedEncoderMetadata},
		{"two samples", lossless, rgb.frame, func(m *pixeldata.Metadata) { m.SamplesPerPixel = 2 }, pixeldata.ErrUnsupportedEncoderMetadata},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := base.metadata
			if len(tt.frame) == len(rgb.frame) {
				m = rgb.metadata
			}
			tt.mutate(&m)
			if _, err := tt.encoder.EncodeFrame(context.Background(), tt.frame, m); !errors.Is(err, tt.want) {
				t.Fatalf("EncodeFrame() error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestEncodeFrameHonorsCancellation(t *testing.T) {
	tc := syntheticFrame(64, 64, 1, 12, 16, false, 3)
	encoder, _ := NewEncoder(Options{TransferSyntaxUID: transfer.HTJ2KLossless.UID})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := encoder.EncodeFrame(ctx, tc.frame, tc.metadata); !errors.Is(err, context.Canceled) {
		t.Fatalf("EncodeFrame() error = %v, want context.Canceled", err)
	}
}

func TestReversibleTransformsInvertExactly(t *testing.T) {
	rng := rand.New(rand.NewSource(5))
	for _, size := range [][2]int{{1, 1}, {2, 1}, {1, 7}, {5, 3}, {33, 17}, {64, 64}, {129, 2}} {
		w, h := size[0], size[1]
		for levels := 0; levels <= autoLevels(w, h)+1; levels++ {
			plane := make([]int32, w*h)
			for i := range plane {
				plane[i] = int32(rng.Intn(1<<17) - 1<<16)
			}
			original := append([]int32(nil), plane...)
			forwardDWT53(plane, w, h, levels)
			inverseDWT53(plane, w, h, levels)
			if !reflect.DeepEqual(plane, original) {
				t.Fatalf("5/3 %dx%d levels %d is not reversible", w, h, levels)
			}
			floats := make([]float64, w*h)
			for i, v := range original {
				floats[i] = float64(v)
			}
			forwardDWT97(floats, w, h, levels)
			inverseDWT97(floats, w, h, levels)
			for i, v := range floats {
				if math.Abs(v-float64(original[i])) > 1e-6 {
					t.Fatalf("9/7 %dx%d levels %d sample %d = %v, want %d", w, h, levels, i, v, original[i])
				}
			}
		}
	}
	r, g, b := []int32{0, 255, -7, 65535}, []int32{0, 0, 300, 1}, []int32{255, 128, -9, 65535}
	y, u, v := append([]int32(nil), r...), append([]int32(nil), g...), append([]int32(nil), b...)
	forwardRCT(y, u, v)
	for i := range r {
		gg := y[i] - (u[i]+v[i])>>2
		if gg != g[i] || v[i]+gg != r[i] || u[i]+gg != b[i] {
			t.Fatalf("RCT sample %d does not invert", i)
		}
	}
}

func TestSynthesisNormsMatchTheFilters(t *testing.T) {
	// The squared products are the 2D norms of the 9/7 subbands that
	// encoders weight with: LL 1.965, HL/LH 2.022 and HH 2.080 at the first
	// level; the exact LL norm at the second level is 4.122.
	low1, high1 := synthesisNorm(1, false), synthesisNorm(1, true)
	for _, tt := range []struct {
		name      string
		got, want float64
	}{
		{"LL1", low1 * low1, 1.965},
		{"HL1", low1 * high1, 2.022},
		{"HH1", high1 * high1, 2.080},
		{"LL2", synthesisNorm(2, false) * synthesisNorm(2, false), 4.122},
	} {
		if math.Abs(tt.got-tt.want) > 2e-3 {
			t.Fatalf("%s norm = %v, want %v", tt.name, tt.got, tt.want)
		}
	}
	if low2 := synthesisNorm(2, false); low2 <= synthesisNorm(1, false) {
		t.Fatalf("deeper low-pass norm %v does not grow", low2)
	}
}

func TestStepParametersEncodeTheStep(t *testing.T) {
	for _, tt := range []struct {
		delta float64
		depth int
	}{{1, 8}, {0.37, 12}, {3.9, 16}, {1e-3, 8}, {250, 8}} {
		e, mu := stepParameters(tt.delta, tt.depth)
		got := math.Ldexp(1+float64(mu)/2048, tt.depth-e)
		if math.Abs(got-tt.delta)/tt.delta > 1.0/2048 {
			t.Fatalf("step %v depth %d encoded as %v (epsilon %d, mu %d)", tt.delta, tt.depth, got, e, mu)
		}
	}
}

func TestPacketHeaderPrimitives(t *testing.T) {
	for _, tt := range []struct {
		n    int
		bits string
	}{
		{1, "0"}, {2, "10"}, {3, "1100"}, {5, "1110"}, {6, "111100000"}, {36, "111111110"},
		{37, "1111111110000000"}, {164, "1111111111111111"},
	} {
		w := newHeaderWriter()
		passCount(&w, tt.n)
		if got, want := bitString(w.finish()), stuffedBits(tt.bits); got[:len(want)] != want {
			t.Fatalf("%d passes = %s, want %s", tt.n, got, want)
		}
	}

	// B.10.2: a 2x2 tag tree holding 1, 3, 2, 0.
	tree := newTagTree(2, 2, []int{1, 3, 2, 0})
	w := newHeaderWriter()
	for i := 0; i < 4; i++ {
		tree.encode(&w, i%2, i/2, 1<<30)
	}
	// root 0: "1"; leaf 1: "01"; leaf 3: "0001"; leaf 2: "001"; leaf 0: "1".
	if got, want := bitString(w.finish())[:11], "1"+"01"+"0001"+"001"+"1"; got != want {
		t.Fatalf("tag tree bits = %s, want %s", got, want)
	}

	w = newHeaderWriter()
	w.bits(0xFF, 8)
	w.bits(0x7F, 7)
	if got := w.finish(); !bytes.Equal(got, []byte{0xFF, 0x7F}) {
		t.Fatalf("stuffed header = % x", got)
	}
	w = newHeaderWriter()
	w.bits(0xFF, 8)
	if got := w.finish(); !bytes.Equal(got, []byte{0xFF, 0x00}) {
		t.Fatalf("header ending in 0xFF = % x", got)
	}
}

// stuffedBits inserts the zero bit that follows every 0xFF byte (B.10.1).
func stuffedBits(bits string) string {
	out := []byte{}
	for i := 0; i < len(bits); i++ {
		out = append(out, bits[i])
		if len(out)%8 == 0 {
			if string(out[len(out)-8:]) == "11111111" {
				out = append(out, '0')
			}
		}
	}
	return string(out)
}

func bitString(b []byte) string {
	out := make([]byte, 0, 8*len(b))
	for _, v := range b {
		for i := 7; i >= 0; i-- {
			out = append(out, '0'+v>>uint(i)&1)
		}
	}
	return string(out)
}

// markerSegments walks the main header and the single tile-part.
func markerSegments(t *testing.T, cs []byte) map[uint16][]byte {
	t.Helper()
	if len(cs) < 4 || binary.BigEndian.Uint16(cs) != 0xFF4F || binary.BigEndian.Uint16(cs[len(cs)-2:]) != 0xFFD9 {
		t.Fatal("codestream is not SOC ... EOC")
	}
	segments := map[uint16][]byte{}
	pos := 2
	for pos+4 <= len(cs) {
		marker := binary.BigEndian.Uint16(cs[pos:])
		length := int(binary.BigEndian.Uint16(cs[pos+2:]))
		segments[marker] = cs[pos+4 : pos+2+length]
		if marker == 0xFF90 {
			psot := int(binary.BigEndian.Uint32(cs[pos+6:]))
			if pos+psot != len(cs)-2 {
				t.Fatalf("Psot %d does not end at EOC", psot)
			}
			segments[0] = cs[pos : pos+psot]
			if binary.BigEndian.Uint16(cs[pos+12:]) != 0xFF93 {
				t.Fatal("SOT is not followed by SOD")
			}
			return segments
		}
		pos += 2 + length
	}
	t.Fatal("no SOT")
	return nil
}

func TestCodestreamMarkersDescribeTheFrame(t *testing.T) {
	tc := syntheticFrame(70, 33, 3, 10, 16, false, 9)
	for _, tt := range []struct {
		opts        Options
		rsiz        uint16
		progression byte
		style       byte
		transform   byte
		mct         byte
		qcdStyle    byte
	}{
		{Options{TransferSyntaxUID: transfer.JPEG2000LosslessOnly.UID}, 0, 0, 0, 1, 0, 0},
		{Options{TransferSyntaxUID: transfer.HTJ2KLossless.UID, ColorTransform: true}, 0x4000, 0, 0x40, 1, 1, 0},
		{Options{TransferSyntaxUID: transfer.HTJ2KLosslessRPCL.UID}, 0x4000, 2, 0x40, 1, 0, 0},
		{Options{TransferSyntaxUID: transfer.JPEG2000.UID, StepSize: 2, AllowLossy: true, ColorTransform: true}, 0, 0, 0, 0, 1, 2},
		{Options{TransferSyntaxUID: transfer.HTJ2K.UID, StepSize: 2, AllowLossy: true, Levels: 2}, 0x4000, 0, 0x40, 0, 0, 2},
	} {
		encoder, err := NewEncoder(tt.opts)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := encoder.EncodeFrame(context.Background(), tc.frame, tc.metadata)
		if err != nil {
			t.Fatal(err)
		}
		seg := markerSegments(t, encoded.Data)
		siz := seg[0xFF51]
		if binary.BigEndian.Uint16(siz) != tt.rsiz || binary.BigEndian.Uint32(siz[2:]) != 70 || binary.BigEndian.Uint32(siz[6:]) != 33 || binary.BigEndian.Uint16(siz[34:]) != 3 || siz[36] != 9 {
			t.Fatalf("%s SIZ = % x", tt.opts.TransferSyntaxUID, siz)
		}
		cap, hasCAP := seg[0xFF50]
		if hasCAP != (tt.rsiz != 0) || hasCAP && binary.BigEndian.Uint32(cap) != 0x00020000 {
			t.Fatalf("%s CAP = % x", tt.opts.TransferSyntaxUID, cap)
		}
		if hasCAP && (binary.BigEndian.Uint16(cap[4:])&0x20 != 0) != (tt.transform == 0) {
			t.Fatalf("%s Ccap15 = % x", tt.opts.TransferSyntaxUID, cap[4:])
		}
		cod := seg[0xFF52]
		levels := byte(5)
		if tt.opts.Levels > 0 {
			levels = byte(tt.opts.Levels)
		}
		if cod[0] != 0 || cod[1] != tt.progression || binary.BigEndian.Uint16(cod[2:]) != 1 || cod[4] != tt.mct || cod[5] != levels || cod[6] != 4 || cod[7] != 4 || cod[8] != tt.style || cod[9] != tt.transform {
			t.Fatalf("%s COD = % x", tt.opts.TransferSyntaxUID, cod)
		}
		qcd := seg[0xFF5C]
		bands := 3*int(levels) + 1
		size := bands
		if tt.qcdStyle == 2 {
			size = 2 * bands
		}
		if qcd[0]&0x1F != tt.qcdStyle || qcd[0]>>5 < 2 || len(qcd) != 1+size {
			t.Fatalf("%s QCD = % x", tt.opts.TransferSyntaxUID, qcd)
		}
		tlm, hasTLM := seg[0xFF55]
		if hasTLM != (tt.progression == 2) || hasTLM && int(binary.BigEndian.Uint32(tlm[3:])) != len(seg[0]) {
			t.Fatalf("%s TLM = % x (tile-part %d bytes)", tt.opts.TransferSyntaxUID, tlm, len(seg[0]))
		}
		if tt.mct == 1 {
			want := "YBR_RCT"
			if tt.transform == 0 {
				want = "YBR_ICT"
			}
			if encoded.PhotometricInterpretation != want || encoded.PlanarConfiguration == nil || *encoded.PlanarConfiguration != 0 {
				t.Fatalf("%s photometric = %q", tt.opts.TransferSyntaxUID, encoded.PhotometricInterpretation)
			}
		} else if encoded.PhotometricInterpretation != "" || encoded.PlanarConfiguration != nil {
			t.Fatalf("%s changed metadata", tt.opts.TransferSyntaxUID)
		}
		for i := 0; i+1 < len(seg[0]); i++ {
			if i > 14 && seg[0][i] == 0xFF && seg[0][i+1] > 0x8F {
				t.Fatalf("%s tile-part holds marker code %02x%02x", tt.opts.TransferSyntaxUID, seg[0][i], seg[0][i+1])
			}
		}
	}
}

func TestEncodeIsDeterministicUnderConcurrency(t *testing.T) {
	tc := syntheticFrame(150, 90, 1, 16, 16, true, 11)
	for _, uid := range []string{transfer.JPEG2000LosslessOnly.UID, transfer.HTJ2KLossless.UID} {
		encoder, _ := NewEncoder(Options{TransferSyntaxUID: uid})
		want, err := encoder.EncodeFrame(context.Background(), tc.frame, tc.metadata)
		if err != nil {
			t.Fatal(err)
		}
		source := append([]byte(nil), tc.frame...)
		var wg sync.WaitGroup
		errs := make(chan error, 8)
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				got, err := encoder.EncodeFrame(context.Background(), tc.frame, tc.metadata)
				if err == nil && !bytes.Equal(got.Data, want.Data) {
					err = errors.New("different codestream")
				}
				errs <- err
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("%s: %v", uid, err)
			}
		}
		if !bytes.Equal(source, tc.frame) {
			t.Fatalf("%s modified the borrowed frame", uid)
		}
	}
}

func TestLossyQuantizationIsBoundedByTheStep(t *testing.T) {
	for _, tc := range []frameCase{smoothFrame(128, 96, 1, 12, false, 1), smoothFrame(80, 64, 3, 8, false, 2), smoothFrame(64, 64, 1, 16, true, 3)} {
		for _, step := range []float64{0.5, 2, 8} {
			for _, transform := range []bool{false, true} {
				c := coding{step: step, mct: transform}
				got := reconstructLossy(tc, c)
				psnr := peakSNR(tc.samples, got, int(tc.metadata.BitsStored))
				if psnr < minimumPSNR(step, int(tc.metadata.BitsStored)) {
					t.Fatalf("%s step %v ICT %t: PSNR %.1f dB", tc.name, step, transform, psnr)
				}
			}
		}
	}
}

func TestSignedSamplesIgnorePaddingBits(t *testing.T) {
	// A signed 12-bit sample stored without sign extension codes like the
	// sign-extended one.
	m := pixeldata.Metadata{Rows: 1, Columns: 2, SamplesPerPixel: 1, BitsAllocated: 16, BitsStored: 12, HighBit: 11, PixelRepresentation: 1, NumberOfFrames: 1, PhotometricInterpretation: "MONOCHROME2"}
	encoder, _ := NewEncoder(Options{TransferSyntaxUID: transfer.JPEG2000LosslessOnly.UID})
	a, err := encoder.EncodeFrame(context.Background(), []byte{0x00, 0xF8, 0xFF, 0x07}, m)
	if err != nil {
		t.Fatal(err)
	}
	b, err := encoder.EncodeFrame(context.Background(), []byte{0x00, 0x08, 0xFF, 0x07}, m)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Data, b.Data) {
		t.Fatal("padding bits changed the codestream")
	}
	if got := planesOf([]byte{0x00, 0x08, 0xFF, 0x07}, m); got[0][0] != -2048 || got[0][1] != 2047 {
		t.Fatalf("planes = %v", got)
	}
}

func benchmarkEncode(b *testing.B, opts Options, tc frameCase) {
	encoder, err := NewEncoder(opts)
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(tc.frame)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := encoder.EncodeFrame(context.Background(), tc.frame, tc.metadata); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEncode512Mono12(b *testing.B) {
	tc := smoothFrame(512, 512, 1, 12, false, 1)
	for _, bench := range []struct {
		name string
		opts Options
	}{
		{"Part1Lossless", Options{TransferSyntaxUID: transfer.JPEG2000LosslessOnly.UID}},
		{"HTJ2KLossless", Options{TransferSyntaxUID: transfer.HTJ2KLossless.UID}},
		{"Part1Lossy", Options{TransferSyntaxUID: transfer.JPEG2000.UID, StepSize: 4, AllowLossy: true}},
		{"HTJ2KLossy", Options{TransferSyntaxUID: transfer.HTJ2K.UID, StepSize: 4, AllowLossy: true}},
	} {
		b.Run(bench.name, func(b *testing.B) { benchmarkEncode(b, bench.opts, tc) })
	}
}
