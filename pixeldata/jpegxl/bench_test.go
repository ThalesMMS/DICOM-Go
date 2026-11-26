package jpegxl

import "testing"

// Benchmarks on a 512x512 synthetic frame, the size of the JPEGXL-Fixture
// corpus images.

func BenchmarkEncodeLossless16(b *testing.B) {
	img := syntheticImage(512, 512, 1, 16, 1)
	b.SetBytes(int64(len(img.Samples) * 2))
	for i := 0; i < b.N; i++ {
		if _, err := EncodeLossless(img, LosslessOptions{BitsPerSample: 16}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecodeLossless16(b *testing.B) {
	img := syntheticImage(512, 512, 1, 16, 1)
	data, err := EncodeLossless(img, LosslessOptions{BitsPerSample: 16})
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(img.Samples) * 2))
	for i := 0; i < b.N; i++ {
		if _, err := Decode(data); err != nil {
			b.Fatal(err)
		}
	}
}

func benchmarkJPEG(b *testing.B) []byte {
	return encodeStdJPEG(b, syntheticImage(512, 512, 3, 8, 2), 90)
}

func BenchmarkRecompressJPEG(b *testing.B) {
	jpg := benchmarkJPEG(b)
	b.SetBytes(int64(len(jpg)))
	for i := 0; i < b.N; i++ {
		if _, err := RecompressJPEG(jpg); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkReconstructJPEG(b *testing.B) {
	jxl, err := RecompressJPEG(benchmarkJPEG(b))
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(jxl)))
	for i := 0; i < b.N; i++ {
		if _, err := ReconstructJPEG(jxl); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecodeLossy(b *testing.B) {
	img := syntheticImage(512, 512, 3, 8, 3)
	data, err := EncodeLossy(img, LossyOptions{Quality: 90})
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(img.Samples)))
	for i := 0; i < b.N; i++ {
		if _, err := Decode(data); err != nil {
			b.Fatal(err)
		}
	}
}
