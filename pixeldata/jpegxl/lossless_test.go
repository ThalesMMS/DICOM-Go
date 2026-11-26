package jpegxl

import (
	"math"
	"math/rand"
	"testing"
)

func syntheticImage(w, h, channels, bits int, seed int64) *Image {
	rng := rand.New(rand.NewSource(seed))
	img := &Image{Width: w, Height: h, Channels: channels, BitsPerSample: bits, Samples: make([]int32, w*h*channels)}
	maxv := float64(int(1)<<uint(bits) - 1)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			for c := 0; c < channels; c++ {
				v := maxv/2 + maxv/3*math.Sin(float64(x+5*c)/7)*math.Cos(float64(y)/9) + float64(rng.Intn(9)-4)
				if (x/13+y/11)%5 == 0 {
					v = maxv - v
				}
				img.Samples[(y*w+x)*channels+c] = int32(math.Max(0, math.Min(maxv, v)))
			}
		}
	}
	return img
}

func TestEncodeLosslessRoundTrip(t *testing.T) {
	cases := []struct{ w, h, c, bits int }{
		{1, 1, 1, 8}, {17, 5, 1, 1}, {64, 48, 1, 8}, {33, 21, 3, 8}, {40, 30, 1, 12},
		{50, 20, 1, 16}, {300, 70, 1, 12}, {1100, 9, 1, 10}, {9, 1030, 3, 8}, {20, 20, 1, 7},
	}
	for _, tc := range cases {
		img := syntheticImage(tc.w, tc.h, tc.c, tc.bits, int64(tc.w*tc.h))
		data, err := EncodeLossless(img, LosslessOptions{BitsPerSample: tc.bits})
		if err != nil {
			t.Fatalf("%+v: encode: %v", tc, err)
		}
		got, err := Decode(data)
		if err != nil {
			t.Fatalf("%+v: decode: %v", tc, err)
		}
		if got.Width != tc.w || got.Height != tc.h || got.Channels != tc.c || got.BitsPerSample != tc.bits {
			t.Fatalf("%+v: decoded %dx%dx%d %d bits", tc, got.Width, got.Height, got.Channels, got.BitsPerSample)
		}
		for i := range img.Samples {
			if img.Samples[i] != got.Samples[i] {
				t.Fatalf("%+v: sample %d = %d, want %d", tc, i, got.Samples[i], img.Samples[i])
			}
		}
		t.Logf("%+v: %d bytes for %d samples", tc, len(data), len(img.Samples))
	}
}
