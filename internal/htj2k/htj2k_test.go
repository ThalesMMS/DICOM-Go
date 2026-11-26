package htj2k

import (
	"errors"
	"fmt"
	"math/rand"
	"testing"
)

func TestCodebookCoversEveryQuadPattern(t *testing.T) {
	for row, records := range [2][]vlcRecord{vlcInitial[:], vlcNonInitial[:]} {
		for _, r := range records {
			if r.e1&^r.ek != 0 || r.ek&^r.rho != 0 || r.length == 0 || r.length > 7 {
				t.Fatalf("row %d record %+v is inconsistent", row, r)
			}
		}
		for context := 0; context < 8; context++ {
			for rho := 0; rho < 16; rho++ {
				for emb := 0; emb < 16; emb++ {
					if emb&^rho != 0 || context == 0 && rho == 0 {
						continue
					}
					code := encodeTables[row][context][rho][emb]
					if !code.ok {
						t.Fatalf("row %d context %d rho %#x emb %#x has no codeword", row, context, rho, emb)
					}
					symbol := decodeTables[row][context][code.codeword]
					if int(symbol.rho) != rho || symbol.length != code.length || symbol.ek != code.ek || symbol.e1 != code.e1 || (symbol.uOff != 0) != (emb != 0) {
						t.Fatalf("row %d context %d rho %#x emb %#x decodes as %+v", row, context, rho, emb, symbol)
					}
				}
			}
		}
	}
}

func TestMELRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for _, density := range []float64{0, 0.02, 0.3, 0.7, 1} {
		events := make([]bool, 3000)
		for i := range events {
			events[i] = rng.Float64() < density
		}
		w := newMELWriter()
		for _, e := range events {
			w.event(e)
		}
		data := w.finish()
		r := melReader{data: data}
		for i, want := range events {
			if got := r.event(); got != want {
				t.Fatalf("density %v event %d = %v, want %v", density, i, got, want)
			}
		}
	}
}

func TestCleanupRoundTrip(t *testing.T) {
	sizes := [][2]int{{1, 1}, {2, 1}, {1, 2}, {3, 3}, {5, 7}, {7, 5}, {64, 64}, {17, 64}, {64, 1}, {1, 64}, {1024, 4}, {4, 1024}, {32, 128}, {33, 9}}
	profiles := []struct {
		name    string
		density float64
		bits    int
	}{
		{"sparse-small", 0.05, 2},
		{"sparse-large", 0.1, 20},
		{"dense-8", 0.95, 8},
		{"dense-16", 1, 16},
		{"mixed-12", 0.6, 12},
		{"wide-30", 0.5, 30},
		{"ones", 1, 1},
	}
	rng := rand.New(rand.NewSource(1150))
	for _, size := range sizes {
		for _, p := range profiles {
			t.Run(fmt.Sprintf("%dx%d/%s", size[0], size[1], p.name), func(t *testing.T) {
				w, h := size[0], size[1]
				samples := make([]int32, w*h)
				for i := range samples {
					if rng.Float64() >= p.density {
						continue
					}
					limit := int64(1)<<p.bits - 1
					if limit > MaxMagnitude {
						limit = MaxMagnitude
					}
					v := int32(rng.Int63n(limit) + 1)
					if rng.Intn(2) == 0 {
						v = -v
					}
					samples[i] = v
				}
				samples[rng.Intn(len(samples))] = int32(1) << (p.bits - 1)
				segment, err := EncodeCleanup(samples, w, h)
				if err != nil {
					t.Fatal(err)
				}
				assertNoMarkerCode(t, segment)
				out := make([]int32, w*h)
				if err := DecodeCleanup(segment, w, h, out, 31); err != nil {
					t.Fatal(err)
				}
				for i := range samples {
					if out[i] != samples[i] {
						t.Fatalf("sample %d (%d,%d) = %d, want %d", i, i%w, i/w, out[i], samples[i])
					}
				}
			})
		}
	}
}

func TestCleanupGradientsExerciseKappa(t *testing.T) {
	// Smooth blocks make the exponent bound of each quad row track the row
	// above, so u values stay small and kappa does the work.
	for _, w := range []int{4, 9, 64} {
		h := 64
		samples := make([]int32, w*h)
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				samples[y*w+x] = int32((x*37+y*101)%4096) - 2048
			}
		}
		segment, err := EncodeCleanup(samples, w, h)
		if err != nil {
			t.Fatal(err)
		}
		out := make([]int32, len(samples))
		if err := DecodeCleanup(segment, w, h, out, 31); err != nil {
			t.Fatal(err)
		}
		for i := range samples {
			if out[i] != samples[i] {
				t.Fatalf("width %d sample %d = %d, want %d", w, i, out[i], samples[i])
			}
		}
	}
}

func TestCleanupRejects(t *testing.T) {
	if _, err := EncodeCleanup(make([]int32, 16), 4, 4); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("all-zero block error = %v", err)
	}
	if _, err := EncodeCleanup([]int32{1 << 30}, 1, 1); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("oversized magnitude error = %v", err)
	}
	if _, err := EncodeCleanup(make([]int32, 8192), 128, 64); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("oversized block error = %v", err)
	}
	out := make([]int32, 4)
	for _, segment := range [][]byte{nil, {0x00}, {0x00, 0x00}, {0x01, 0xFF}, {0x0F, 0x01}} {
		if err := DecodeCleanup(segment, 2, 2, out, 31); !errors.Is(err, ErrMalformed) {
			t.Fatalf("segment % x error = %v", segment, err)
		}
	}
	segment, err := EncodeCleanup([]int32{1 << 20, 0, 0, 0}, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := DecodeCleanup(segment, 2, 2, out, 8); !errors.Is(err, ErrMalformed) {
		t.Fatalf("bound error = %v", err)
	}
}

func assertNoMarkerCode(t *testing.T, segment []byte) {
	t.Helper()
	for i := 0; i+1 < len(segment); i++ {
		if segment[i] == 0xFF && segment[i+1] > 0x8F {
			t.Fatalf("segment holds a marker code %02X%02X at %d", segment[i], segment[i+1], i)
		}
	}
	if len(segment) > 0 && segment[len(segment)-1] == 0xFF {
		t.Fatal("segment ends with 0xFF")
	}
}

func BenchmarkEncodeCleanup64(b *testing.B) {
	rng := rand.New(rand.NewSource(1))
	samples := make([]int32, 64*64)
	for i := range samples {
		samples[i] = int32(rng.Intn(2048) - 1024)
	}
	b.SetBytes(int64(len(samples) * 2))
	for i := 0; i < b.N; i++ {
		if _, err := EncodeCleanup(samples, 64, 64); err != nil {
			b.Fatal(err)
		}
	}
}
