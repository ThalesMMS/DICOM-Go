package object

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// Run with:
// go test -run '^$' -bench '^BenchmarkReadFileLargePixelData$' ./object -benchmem
//
// Part 10 read cost when the dataset carries a CT-slice-sized (~512 KiB)
// native Pixel Data value, mirroring per-slice cost during bulk study loads.

func largePixelDataPart10Bytes(b *testing.B) []byte {
	b.Helper()
	pixels := make([]byte, 512*512*2)
	for i := range pixels {
		pixels[i] = byte(i)
	}
	elements := append([]core.Element{}, dicomtest.MinimalDataset()...)
	elements = append(elements,
		dicomtest.NewUShortElement(core.Tag{Group: 0x0028, Element: 0x0010}, 512),
		dicomtest.NewUShortElement(core.Tag{Group: 0x0028, Element: 0x0011}, 512),
		dicomtest.NewUShortElement(core.Tag{Group: 0x0028, Element: 0x0100}, 16),
		core.NewRawElement(core.TagPixelData, core.VROW, pixels),
	)
	data, err := dicomtest.Part10File(transfer.ExplicitVRLittleEndian, elements...)
	if err != nil {
		b.Fatal(err)
	}
	return data
}

func BenchmarkReadFileLargePixelData(b *testing.B) {
	data := largePixelDataPart10Bytes(b)

	b.Run("reader", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			if _, err := ReadFile(bytes.NewReader(data)); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("open_file", func(b *testing.B) {
		path := filepath.Join(b.TempDir(), "large.dcm")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		b.ResetTimer()

		for i := 0; i < b.N; i++ {
			file, err := OpenFile(path)
			if err != nil {
				b.Fatal(err)
			}
			if err := file.Close(); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// Run with:
// go test -run '^$' -bench '^BenchmarkReadFileLargeNestedValues$' ./object -benchmem
//
// Memory of opening a file with a 2 MiB encapsulated PDF and 1 MiB private
// blobs nested in sequence items, materialized or left on disk (#1141).
func BenchmarkReadFileLargeNestedValues(b *testing.B) {
	path := filepath.Join(b.TempDir(), "nested.dcm")
	if err := os.WriteFile(path, largeNestedValuePart10(b, 2<<20, 1<<20), 0o600); err != nil {
		b.Fatal(err)
	}
	for _, bench := range []struct {
		name string
		opts ReadFileOptions
	}{
		{"materialized", ReadFileOptions{}},
		{"top_level_deferred", ReadFileOptions{InlineValueBytesThreshold: 64 << 10}},
		{"nested_deferred", ReadFileOptions{InlineValueBytesThreshold: 64 << 10, DeferNestedValues: true}},
	} {
		b.Run(bench.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				file, err := OpenFileWithOptions(path, bench.opts)
				if err != nil {
					b.Fatal(err)
				}
				if err := file.Close(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
